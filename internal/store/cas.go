// Copyright 2026 Li Qiankun
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package store

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/folbricht/desync"
)

// casStore is the content-addressed backend (docs/persistence.md §3.3): the
// bed dir is serialized to a catar stream (desync, the casync model), CDC-
// chunked, and only chunks absent from the bed's previous snapshot are
// uploaded. The index object is the commit point and carries the generation,
// exactly like the tarball backend's single object.
//
// The blob space is PER BED (<prefix>/cas/<bedID>/...): no cross-bed dedup,
// but GC stays a local diff ("chunks not referenced by the committed index")
// whose correctness needs only the upstream scheduler's single-writer-per-bed
// guarantee — never a cross-manifest, cross-instance sweep.
type casStore struct {
	obj    objAPI
	prefix string
}

// Chunk size bounds (min/avg/max) for the CDC chunker. Larger than casync's
// 16/64/256 KiB defaults: chunk count is S3 request count, and request
// round-trips — not rolling-hash granularity — dominate persist latency here.
const (
	casChunkMin = 64 << 10
	casChunkAvg = 256 << 10
	casChunkMax = 1 << 20
)

// casConcurrency bounds parallel chunk transfers (and hashing/compression
// workers during chunking).
const casConcurrency = 8

// casConverters is the at-rest chunk encoding (zstd). Conversion happens in
// the desync layer; reads verify the chunk ID against the decompressed data,
// so a corrupted or tampered object fails restore instead of poisoning a bed.
var casConverters = desync.Converters{desync.Compressor{}}

// casMetaBytes is the S3 user-metadata key on the index object carrying the
// logical (uncompressed catar) size, since the index object's own
// ContentLength says nothing about the workspace.
const casMetaBytes = "bytes"

func newCAS(ctx context.Context, cfg Config) (Store, error) {
	client, err := newS3Client(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &casStore{obj: &s3obj{client: client, bucket: cfg.Bucket}, prefix: cfg.Prefix}, nil
}

func (s *casStore) Name() string { return "cas" }

func (s *casStore) indexKey(bedID string) string {
	return path.Join(s.prefix, "cas", bedID+".caibx")
}

// chunkPrefix ends with "/" so bedID "a" can never match bedID "ab"'s chunks.
func (s *casStore) chunkPrefix(bedID string) string {
	return path.Join(s.prefix, "cas", bedID) + "/"
}

func (s *casStore) Stat(ctx context.Context, bedID string) (*SnapshotInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, s3OpTimeout)
	defer cancel()
	meta, _, exists, err := s.obj.head(ctx, s.indexKey(bedID))
	if err != nil || !exists {
		return nil, err
	}
	info := &SnapshotInfo{}
	// Same tolerance as the tarball backend: unparsable metadata reads as 0.
	if g, err := strconv.ParseInt(meta[generationMetaKey], 10, 64); err == nil {
		info.Generation = g
	}
	if b, err := strconv.ParseInt(meta[casMetaBytes], 10, 64); err == nil {
		info.Bytes = b
	}
	return info, nil
}

func (s *casStore) Persist(ctx context.Context, bedID, dir string, generation int64) error {
	// Fencing parity with the tarball backend (docs/persistence.md §3.5):
	// remote generation >= ours means another instance persisted this bed
	// after our activation — refuse rather than silently overwrite.
	prevMeta, _, prevExists, err := s.obj.head(ctx, s.indexKey(bedID))
	if err != nil {
		return fmt.Errorf("store: persist %s: pre-write stat: %w", bedID, err)
	}
	if prevExists {
		if g, err := strconv.ParseInt(prevMeta[generationMetaKey], 10, 64); err == nil && g >= generation {
			return fmt.Errorf("store: persist %s: remote generation %d >= local %d: %w",
				bedID, g, generation, ErrConflict)
		}
	}

	// The previous index doubles as the transfer accelerator: every chunk it
	// references is already in the store, so the write adapter skips those
	// without as much as a HEAD. Unchanged data costs zero requests.
	var prev *desync.Index
	if prevExists {
		if idx, err := s.loadIndex(ctx, bedID); err != nil {
			return err
		} else {
			prev = &idx
		}
	}
	ws := newCASObjStore(ctx, s.obj, s.chunkPrefix(bedID))
	if prev != nil {
		for _, c := range prev.Chunks {
			ws.known[c.ID] = struct{}{}
		}
	}

	// One streaming pass: Tar serializes the bed dir into a pipe while
	// ChunkStream chunks/compresses/uploads from the other end. No temp file,
	// memory bounded by chunk max × workers.
	pr, pw := io.Pipe()
	go func() {
		src := &filteredFS{inner: desync.NewLocalFS(dir, desync.LocalFSOptions{}), root: dir}
		pw.CloseWithError(desync.Tar(ctx, pw, src))
	}()
	chunker, err := desync.NewChunker(pr, casChunkMin, casChunkAvg, casChunkMax)
	if err != nil {
		pr.CloseWithError(err)
		return fmt.Errorf("store: persist %s: chunker: %w", bedID, err)
	}
	idx, err := desync.ChunkStream(ctx, chunker, ws, casConcurrency)
	if err != nil {
		pr.CloseWithError(err)
		return fmt.Errorf("store: persist %s: chunk: %w", bedID, err)
	}

	// No-op short-circuit: identical content produces an identical catar
	// (stable walk order) and therefore an identical chunk sequence. Skip the
	// index write and GC entirely; the remote snapshot stays valid as-is.
	if prev != nil && sameChunks(idx, *prev) {
		return nil
	}

	var buf bytes.Buffer
	if _, err := idx.WriteTo(&buf); err != nil {
		return fmt.Errorf("store: persist %s: encode index: %w", bedID, err)
	}
	putCtx, cancel := context.WithTimeout(ctx, s3OpTimeout)
	defer cancel()
	err = s.obj.put(putCtx, s.indexKey(bedID), &buf, int64(buf.Len()), map[string]string{
		generationMetaKey: strconv.FormatInt(generation, 10),
		casMetaBytes:      strconv.FormatInt(idx.Length(), 10),
	})
	if err != nil {
		return fmt.Errorf("store: persist %s: put index: %w", bedID, err)
	}

	// GC after the commit point: anything under the bed's chunk prefix that
	// the committed index doesn't reference is garbage — dropped by this
	// persist, or orphaned by an earlier crashed one (uploaded chunks whose
	// index never landed). Correct under the single-writer guarantee. A GC
	// failure is deliberately not a persist failure: the snapshot is already
	// durable, and stragglers are swept by the next persist.
	keep := make(map[string]struct{}, len(idx.Chunks))
	for _, c := range idx.Chunks {
		keep[c.ID.String()] = struct{}{}
	}
	listed, err := s.obj.list(ctx, s.chunkPrefix(bedID))
	if err != nil {
		return nil //nolint:nilerr // snapshot committed; GC is best-effort
	}
	var garbage []string
	for _, key := range listed {
		if _, ok := keep[path.Base(key)]; !ok {
			garbage = append(garbage, key)
		}
	}
	if len(garbage) > 0 {
		_ = s.obj.del(ctx, garbage)
	}
	return nil
}

func (s *casStore) Restore(ctx context.Context, bedID, dir string) error {
	idx, err := s.loadIndex(ctx, bedID)
	if err != nil {
		return err
	}
	// NoSameOwner/NoSamePermissions-off: modes are restored, ownership is
	// not — the daemon may not be root, and the uid isolator's Prepare
	// re-owns the tree after every restore anyway.
	dst := desync.NewLocalFS(dir, desync.LocalFSOptions{NoSameOwner: true})
	rs := newCASObjStore(ctx, s.obj, s.chunkPrefix(bedID))
	// NullProgressBar, not nil: UnTarIndex calls pb methods unconditionally.
	if err := desync.UnTarIndex(ctx, dst, idx, rs, casConcurrency, desync.NullProgressBar{}); err != nil {
		return fmt.Errorf("store: restore %s: %w", bedID, err)
	}
	return nil
}

func (s *casStore) Delete(ctx context.Context, bedID string) error {
	keys, err := s.obj.list(ctx, s.chunkPrefix(bedID))
	if err != nil {
		return fmt.Errorf("store: delete %s: list chunks: %w", bedID, err)
	}
	keys = append(keys, s.indexKey(bedID))
	if err := s.obj.del(ctx, keys); err != nil {
		return fmt.Errorf("store: delete %s: %w", bedID, err)
	}
	return nil
}

func (s *casStore) loadIndex(ctx context.Context, bedID string) (desync.Index, error) {
	ctx, cancel := context.WithTimeout(ctx, s3OpTimeout)
	defer cancel()
	rc, err := s.obj.get(ctx, s.indexKey(bedID))
	if err != nil {
		return desync.Index{}, fmt.Errorf("store: index %s: %w", bedID, err)
	}
	defer rc.Close()
	idx, err := desync.IndexFromReader(rc)
	if err != nil {
		return desync.Index{}, fmt.Errorf("store: decode index %s: %w", bedID, err)
	}
	return idx, nil
}

func sameChunks(a, b desync.Index) bool {
	if len(a.Chunks) != len(b.Chunks) {
		return false
	}
	for i := range a.Chunks {
		if a.Chunks[i].ID != b.Chunks[i].ID {
			return false
		}
	}
	return true
}

// casObjStore adapts objAPI to desync's WriteStore for one bed's chunk space.
// known is primed with the previous index's chunk IDs so unchanged chunks are
// skipped without any request; it also absorbs duplicate chunks within one
// stream. StoreChunk is called from ChunkStream's worker goroutines.
type casObjStore struct {
	ctx    context.Context
	obj    objAPI
	prefix string
	mu     sync.Mutex
	known  map[desync.ChunkID]struct{}
}

func newCASObjStore(ctx context.Context, obj objAPI, prefix string) *casObjStore {
	return &casObjStore{ctx: ctx, obj: obj, prefix: prefix, known: make(map[desync.ChunkID]struct{})}
}

func (s *casObjStore) key(id desync.ChunkID) string { return s.prefix + id.String() }

func (s *casObjStore) StoreChunk(c *desync.Chunk) error {
	s.mu.Lock()
	if _, ok := s.known[c.ID()]; ok {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	data, err := c.Storage(casConverters)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.ctx, s3OpTimeout)
	defer cancel()
	if err := s.obj.put(ctx, s.key(c.ID()), bytes.NewReader(data), int64(len(data)), nil); err != nil {
		return err
	}
	s.mu.Lock()
	s.known[c.ID()] = struct{}{}
	s.mu.Unlock()
	return nil
}

func (s *casObjStore) GetChunk(id desync.ChunkID) (*desync.Chunk, error) {
	ctx, cancel := context.WithTimeout(s.ctx, s3OpTimeout)
	defer cancel()
	rc, err := s.obj.get(ctx, s.key(id))
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("store: read chunk %s: %w", id, err)
	}
	// skipVerify=false: the ID is recomputed from the decompressed data, so
	// bucket-side corruption fails here instead of landing in the workspace.
	return desync.NewChunkFromStorage(id, data, casConverters, false)
}

func (s *casObjStore) HasChunk(id desync.ChunkID) (bool, error) {
	s.mu.Lock()
	if _, ok := s.known[id]; ok {
		s.mu.Unlock()
		return true, nil
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(s.ctx, s3OpTimeout)
	defer cancel()
	_, _, exists, err := s.obj.head(ctx, s.key(id))
	return exists, err
}

func (s *casObjStore) Close() error   { return nil }
func (s *casObjStore) String() string { return "cas:" + s.prefix }

// filteredFS wraps a FilesystemReader and drops top-level *.local entries —
// the host-private convention shared with the tarball backend
// (docs/persistence.md §4).
type filteredFS struct {
	inner desync.FilesystemReader
	root  string
}

func (f *filteredFS) Next() (*desync.File, error) {
	for {
		file, err := f.inner.Next()
		if err != nil || file == nil {
			return file, err
		}
		if f.topLocal(file) {
			_ = file.Close()
			continue
		}
		return file, nil
	}
}

func (f *filteredFS) topLocal(file *desync.File) bool {
	if !strings.HasSuffix(file.Name, ".local") {
		return false
	}
	rel := file.Path
	if r, err := filepath.Rel(f.root, file.Path); err == nil {
		rel = r
	}
	rel = strings.Trim(filepath.ToSlash(rel), "/")
	return !strings.Contains(rel, "/")
}
