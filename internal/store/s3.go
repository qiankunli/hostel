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
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// s3Store keeps one gzipped tarball per bed: <prefix>/<bedID>.tar.gz.
// One object per bed makes Persist atomic (S3 PUT is all-or-nothing) and
// versionable; incremental sync is a later optimization (docs/persistence.md §3).
type s3Store struct {
	client *s3.Client
	bucket string
	prefix string
}

// s3OpTimeout bounds a single snapshot transfer. Generous: snapshots can be
// hundreds of MB on slow links; lifecycle callers pass Background contexts.
const s3OpTimeout = 5 * time.Minute

func newS3(ctx context.Context, cfg Config) (Store, error) {
	client, err := newS3Client(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &s3Store{client: client, bucket: cfg.Bucket, prefix: cfg.Prefix}, nil
}

// newS3Client is shared by the s3 (tarball) and cas backends.
func newS3Client(ctx context.Context, cfg Config) (*s3.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("store: aws config: %w", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.Endpoint != "" {
			o.BaseEndpoint = &cfg.Endpoint
			// S3-compatible stores (MinIO/TOS/Ceph) generally want
			// path-style addressing rather than virtual-hosted buckets.
			o.UsePathStyle = true
		}
	}), nil
}

// Name says "tarball" (the layout), not "s3" (the transport): cas rides the
// same S3-compatible storage, so the transport doesn't distinguish backends.
func (s *s3Store) Name() string { return "tarball" }

func (s *s3Store) key(bedID string) string {
	return path.Join(s.prefix, bedID+".tar.gz")
}

// generationMetaKey is the S3 user-metadata key carrying the bed generation
// (served back by HEAD, so Stat never downloads the snapshot). The SDK strips
// the x-amz-meta- prefix and lowercases keys on read.
const generationMetaKey = "generation"

func (s *s3Store) Stat(ctx context.Context, bedID string) (*SnapshotInfo, error) {
	ctx, cancel := context.WithTimeout(ctx, s3OpTimeout)
	defer cancel()
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: &s.bucket, Key: strPtr(s.key(bedID)),
	})
	if err != nil {
		var nf *s3types.NotFound
		if errors.As(err, &nf) {
			return nil, nil
		}
		return nil, fmt.Errorf("store: head %s: %w", s.key(bedID), err)
	}
	info := &SnapshotInfo{}
	if out.ContentLength != nil {
		info.Bytes = *out.ContentLength
	}
	// Missing/garbled metadata (snapshot from a pre-generation hostel) reads
	// as generation 0 — any local copy counts as fresh, matching the old
	// "trust what's on disk after restart" behavior.
	if g, err := strconv.ParseInt(out.Metadata[generationMetaKey], 10, 64); err == nil {
		info.Generation = g
	}
	return info, nil
}

func (s *s3Store) Persist(ctx context.Context, bedID, dir string, generation int64) error {
	// Fencing guard: HEAD before writing. Our generation was derived from the
	// snapshot we activated from, so the remote generation must still be below
	// it; remote >= ours means another instance persisted this bed after our
	// activation (dual-activation) and last-writer-wins would silently drop
	// its data. This is a detector, not an atomic CAS — a writer landing in
	// the HEAD→PUT window still slips through — but real dual-activation
	// lasts seconds-to-minutes, so the check catches it in practice. True
	// atomicity needs conditional PUT (If-Match), pending support across the
	// S3-compatible targets (MinIO/TOS).
	if cur, err := s.Stat(ctx, bedID); err != nil {
		return fmt.Errorf("store: persist %s: pre-write stat: %w", bedID, err)
	} else if cur != nil && cur.Generation >= generation {
		return fmt.Errorf("store: persist %s: remote generation %d >= local %d: %w",
			bedID, cur.Generation, generation, ErrConflict)
	}

	ctx, cancel := context.WithTimeout(ctx, s3OpTimeout)
	defer cancel()

	// Stage the tarball in a temp file first: S3 PUT wants a known length,
	// and it keeps the pack step's disk/CPU cost off the network timeout.
	tmp, err := os.CreateTemp("", "hostel-snap-*.tar.gz")
	if err != nil {
		return fmt.Errorf("store: temp snapshot: %w", err)
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()

	// Top-level *.local files are host-private by convention and stay out of
	// the portable snapshot (docs/persistence.md §4).
	skipLocal := func(rel string) bool {
		return !strings.Contains(rel, "/") && strings.HasSuffix(rel, ".local")
	}
	if err := packDir(dir, tmp, skipLocal); err != nil {
		return fmt.Errorf("store: pack %s: %w", bedID, err)
	}
	if _, err := tmp.Seek(0, 0); err != nil {
		return err
	}
	info, err := tmp.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        &s.bucket,
		Key:           strPtr(s.key(bedID)),
		Body:          tmp,
		ContentLength: &size,
		Metadata:      map[string]string{generationMetaKey: strconv.FormatInt(generation, 10)},
	})
	if err != nil {
		return fmt.Errorf("store: put %s: %w", s.key(bedID), err)
	}
	return nil
}

func (s *s3Store) Restore(ctx context.Context, bedID, dir string) error {
	ctx, cancel := context.WithTimeout(ctx, s3OpTimeout)
	defer cancel()
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket, Key: strPtr(s.key(bedID)),
	})
	if err != nil {
		return fmt.Errorf("store: get %s: %w", s.key(bedID), err)
	}
	defer out.Body.Close()
	if err := unpackDir(out.Body, dir); err != nil {
		return fmt.Errorf("store: unpack %s: %w", bedID, err)
	}
	return nil
}

func (s *s3Store) Delete(ctx context.Context, bedID string) error {
	ctx, cancel := context.WithTimeout(ctx, s3OpTimeout)
	defer cancel()
	// S3 DeleteObject on a missing key succeeds — matching the interface.
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: &s.bucket, Key: strPtr(s.key(bedID)),
	})
	if err != nil {
		return fmt.Errorf("store: delete %s: %w", s.key(bedID), err)
	}
	return nil
}

func strPtr(s string) *string { return &s }
