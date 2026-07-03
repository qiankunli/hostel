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

package bed

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiankunli/hostel/internal/isolation"
	"github.com/qiankunli/hostel/internal/store"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	m, err := NewManager(root, "default", "/bin/bash", isolation.New("dorm", root), nil, 0, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return m
}

func TestResolveDefaultBedAndValidation(t *testing.T) {
	m := newTestManager(t)

	b, err := m.Resolve("") // empty → default
	if err != nil || b.ID != "default" {
		t.Fatalf("Resolve(\"\") = %v, %v", b, err)
	}
	if _, err := m.Resolve("bad id!"); err == nil {
		t.Fatal("Resolve invalid id: want error")
	}
	b2, _ := m.Resolve("conv-123")
	if b2.ID != "conv-123" || b2.Workspace == b.Workspace {
		t.Fatalf("distinct bed expected, got %+v", b2)
	}
}

func TestForegroundShellPersistsState(t *testing.T) {
	m := newTestManager(t)
	b, _ := m.Resolve("default")

	sh, err := m.ForegroundShell(b)
	if err != nil {
		t.Fatalf("ForegroundShell: %v", err)
	}
	// Same shell returned on second call (state persistence).
	if sh2, _ := m.ForegroundShell(b); sh2 != sh {
		t.Fatal("ForegroundShell should reuse the same session")
	}

	ctx := context.Background()
	if _, err := sh.Run(ctx, "export HOSTEL_TEST=42", nil); err != nil {
		t.Fatalf("Run export: %v", err)
	}
	var out strings.Builder
	res, err := sh.Run(ctx, "echo v=$HOSTEL_TEST", func(l string) { out.WriteString(l) })
	if err != nil {
		t.Fatalf("Run echo: %v", err)
	}
	if res.ExitCode != 0 || !strings.Contains(out.String(), "v=42") {
		t.Fatalf("state not preserved: exit=%d out=%q", res.ExitCode, out.String())
	}
}

func TestShellExitCode(t *testing.T) {
	m := newTestManager(t)
	b, _ := m.Resolve("default")
	sh, _ := m.ForegroundShell(b)
	// Use a subshell so a non-zero exit doesn't kill the persistent session.
	res, err := sh.Run(context.Background(), `sh -c "exit 7"`, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 7 {
		t.Fatalf("exit code = %d, want 7", res.ExitCode)
	}
	// Session survives and still works afterward.
	if r2, err := sh.Run(context.Background(), "true", nil); err != nil || r2.ExitCode != 0 {
		t.Fatalf("session dead after non-zero exit: %v %+v", err, r2)
	}
}

func TestBackgroundCommandAndLogs(t *testing.T) {
	m := newTestManager(t)
	b, _ := m.Resolve("default")

	cmd, err := m.StartCommand(b, "printf 'a\\nb\\nc\\n'", "", nil, 0, nil)
	if err != nil {
		t.Fatalf("StartCommand: %v", err)
	}
	cmd.Wait()
	st := cmd.Status()
	if st.Running || st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("status after wait: %+v", st)
	}
	content, next, running, err := m.Commands().Logs(cmd.ID, -1)
	if err != nil || running {
		t.Fatalf("Logs: running=%v err=%v", running, err)
	}
	if content != "a\nb\nc\n" || next != 2 {
		t.Fatalf("Logs content=%q next=%d", content, next)
	}
	// Incremental read from cursor 0 → lines after line 0.
	inc, _, _, _ := m.Commands().Logs(cmd.ID, 0)
	if inc != "b\nc\n" {
		t.Fatalf("incremental Logs = %q", inc)
	}
}

func TestDeleteBedReleasesAndRemoves(t *testing.T) {
	m := newTestManager(t)
	b, _ := m.Resolve("conv-x")
	_, _ = m.ForegroundShell(b)
	if ok, err := m.Evict("conv-x"); err != nil || !ok {
		t.Fatalf("Evict: ok=%v err=%v", ok, err)
	}
	if _, ok := m.Get("conv-x"); ok {
		t.Fatal("bed should be gone after Delete")
	}
}

func TestCollectIdleSkipsDefault(t *testing.T) {
	m := newTestManager(t)
	_, _ = m.Resolve("default")
	_, _ = m.Resolve("conv-idle")
	time.Sleep(10 * time.Millisecond)
	reaped := m.CollectIdle(time.Millisecond)
	if len(reaped) != 1 || reaped[0] != "conv-idle" {
		t.Fatalf("CollectIdle reaped %v, want [conv-idle]", reaped)
	}
	if _, ok := m.Get("default"); !ok {
		t.Fatal("default bed must never be reaped")
	}
}

func TestMaxBedsCap(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root, "default", "/bin/bash", isolation.New("dorm", root), nil, 2, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := m.Resolve("a"); err != nil {
		t.Fatalf("bed a: %v", err)
	}
	if _, err := m.Resolve("b"); err != nil {
		t.Fatalf("bed b: %v", err)
	}
	// Cap hit: a third bed is refused with the sentinel.
	if _, err := m.Resolve("c"); !errors.Is(err, ErrBedLimit) {
		t.Fatalf("bed c: want ErrBedLimit, got %v", err)
	}
	// Existing beds still resolve.
	if _, err := m.Resolve("a"); err != nil {
		t.Fatalf("existing bed a after cap: %v", err)
	}
	// The default bed is exempt — the single-tenant path never breaks.
	if _, err := m.Resolve(""); err != nil {
		t.Fatalf("default bed exempt: %v", err)
	}
	// Evicting frees a slot.
	if ok, err := m.Evict("a"); err != nil || !ok {
		t.Fatalf("evict a: ok=%v err=%v", ok, err)
	}
	if _, err := m.Resolve("c"); err != nil {
		t.Fatalf("bed c after free slot: %v", err)
	}
}

// fakeStore is an in-memory Store for lifecycle tests.
type fakeStore struct {
	mu    sync.Mutex
	snaps map[string][]byte // bedID → marker file content
	gens  map[string]int64  // bedID → generation of the stored snapshot
	fail  bool              // force Persist to fail
}

func newFakeStore() *fakeStore {
	return &fakeStore{snaps: map[string][]byte{}, gens: map[string]int64{}}
}

func (f *fakeStore) Name() string { return "fake" }
func (f *fakeStore) Stat(_ context.Context, id string) (*store.SnapshotInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.snaps[id]
	if !ok {
		return nil, nil
	}
	return &store.SnapshotInfo{Generation: f.gens[id], Bytes: int64(len(data))}, nil
}
func (f *fakeStore) Restore(_ context.Context, id, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(filepath.Join(dir, "data"), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "data", "restored.txt"), f.snaps[id], 0o644)
}
func (f *fakeStore) Persist(_ context.Context, id, dir string, generation int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("fake persist failure")
	}
	// dir is the bed dir: meta.json + data/. Mimic that shape.
	data, _ := os.ReadFile(filepath.Join(dir, "data", "data.txt"))
	f.snaps[id] = data
	f.gens[id] = generation
	return nil
}

func (f *fakeStore) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.snaps, id)
	delete(f.gens, id)
	return nil
}

// generation returns the stored snapshot generation (0 = no snapshot).
func (f *fakeStore) generation(id string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gens[id]
}

func TestEvictLeavesLuggageAndWarmResume(t *testing.T) {
	root := t.TempDir()
	fs := newFakeStore()
	m, err := NewManager(root, "default", "/bin/bash", isolation.New("dorm", root), nil, 0, fs)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Write data into a bed, evict it → snapshot taken, local dir stays
	// behind as luggage.
	b, _ := m.Resolve("conv-1")
	if err := os.WriteFile(filepath.Join(b.Workspace, "data.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, err := m.Evict("conv-1"); err != nil || !ok {
		t.Fatalf("Evict: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "meta.json")); err != nil {
		t.Fatalf("luggage should remain after evict: %v", err)
	}
	if string(fs.snaps["conv-1"]) != "payload" {
		t.Fatalf("snapshot content = %q", fs.snaps["conv-1"])
	}
	meta, ok := loadMeta(b.Dir)
	if !ok || meta.LastUsedAt.IsZero() {
		t.Fatalf("luggage meta should carry LastUsedAt, got %+v (ok=%v)", meta, ok)
	}

	// Re-resolve the same bed id → warm start from luggage: the real file is
	// still there and Restore was never called (no marker).
	b2, err := m.Resolve("conv-1")
	if err != nil {
		t.Fatalf("re-Resolve: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b2.Workspace, "data.txt")); err != nil {
		t.Fatalf("warm resume lost workspace data: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b2.Workspace, "restored.txt")); err == nil {
		t.Fatal("fresh luggage must not be re-restored from the store")
	}
}

// A luggage copy whose generation is behind the snapshot (the bed ran on
// another instance meanwhile) must be discarded and re-restored, never served.
func TestStaleLuggageDiscardedOnResume(t *testing.T) {
	root := t.TempDir()
	fs := newFakeStore()
	m, _ := NewManager(root, "default", "/bin/bash", isolation.New("dorm", root), nil, 0, fs)

	b, _ := m.Resolve("conv-s")
	_ = os.WriteFile(filepath.Join(b.Workspace, "data.txt"), []byte("old"), 0o644)
	if ok, err := m.Evict("conv-s"); err != nil || !ok {
		t.Fatalf("Evict: ok=%v err=%v", ok, err)
	}

	// Another hostel persisted a newer snapshot.
	fs.mu.Lock()
	fs.gens["conv-s"] = fs.gens["conv-s"] + 1
	fs.mu.Unlock()

	b2, err := m.Resolve("conv-s")
	if err != nil {
		t.Fatalf("re-Resolve: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b2.Workspace, "restored.txt")); err != nil {
		t.Fatalf("stale luggage should be replaced by a restore: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b2.Workspace, "data.txt")); err == nil {
		t.Fatal("stale luggage content must not survive")
	}
}

// Without luggage (cold resume on a different/cleaned instance), the snapshot
// is restored before serving.
func TestColdResumeRestoresFromSnapshot(t *testing.T) {
	root := t.TempDir()
	fs := newFakeStore()
	m, _ := NewManager(root, "default", "/bin/bash", isolation.New("dorm", root), nil, 0, fs)

	b, _ := m.Resolve("conv-c")
	_ = os.WriteFile(filepath.Join(b.Workspace, "data.txt"), []byte("payload"), 0o644)
	if ok, err := m.Evict("conv-c"); err != nil || !ok {
		t.Fatalf("Evict: ok=%v err=%v", ok, err)
	}
	// Simulate luggage GC / another instance: no local copy.
	if err := os.RemoveAll(b.Dir); err != nil {
		t.Fatal(err)
	}

	b2, err := m.Resolve("conv-c")
	if err != nil {
		t.Fatalf("re-Resolve: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b2.Workspace, "restored.txt")); err != nil {
		t.Fatalf("cold resume should restore from snapshot: %v", err)
	}
}

func TestPersistFailureAbortsDelete(t *testing.T) {
	root := t.TempDir()
	fs := newFakeStore()
	fs.fail = true
	m, _ := NewManager(root, "default", "/bin/bash", isolation.New("dorm", root), nil, 0, fs)

	b, _ := m.Resolve("conv-2")
	if _, err := m.Evict("conv-2"); err == nil {
		t.Fatal("Evict should fail when persist fails")
	}
	// Bed must survive: not deleted from the map, workspace intact.
	if _, ok := m.Get("conv-2"); !ok {
		t.Fatal("bed should still exist after aborted delete")
	}
	if _, err := os.Stat(b.Workspace); err != nil {
		t.Fatalf("workspace should be intact: %v", err)
	}
}

func TestPersistDirty(t *testing.T) {
	root := t.TempDir()
	fs := newFakeStore()
	m, _ := NewManager(root, "default", "/bin/bash", isolation.New("dorm", root), nil, 0, fs)

	b, _ := m.Resolve("conv-3")
	_ = os.WriteFile(filepath.Join(b.Workspace, "data.txt"), []byte("v1"), 0o644)

	// Freshly created bed: persistedAt == created time; touch to mark dirty.
	time.Sleep(5 * time.Millisecond)
	b.touch()
	done := m.PersistDirty(context.Background())
	if len(done) != 1 || done[0] != "conv-3" {
		t.Fatalf("PersistDirty = %v, want [conv-3]", done)
	}
	// Untouched since → not persisted again.
	if done := m.PersistDirty(context.Background()); len(done) != 0 {
		t.Fatalf("second PersistDirty = %v, want []", done)
	}
}

// Regression for the devbox-found deadlock: a shell whose process dies while a
// Run is waiting for output must error out (reader closes the lines channel),
// and the manager/bed locks must stay usable from other goroutines throughout.
// Before the runMu/mu split, this hung the entire daemon including /healthz.
func TestDyingShellDoesNotDeadlock(t *testing.T) {
	m := newTestManager(t)
	b, _ := m.Resolve("default")
	sh, err := m.ForegroundShell(b)
	if err != nil {
		t.Fatalf("ForegroundShell: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		// The shell kills itself: no marker line will ever arrive.
		_, err := sh.Run(context.Background(), "kill -9 $$", nil)
		done <- err
	}()

	// While that Run is in flight/dying, the full lock chain must stay live:
	// Manager.Resolve (m.mu) → touch (b.mu) → ForegroundShell (b.mu + Dead()).
	probe := make(chan struct{})
	go func() {
		_, _ = m.Resolve("default")
		_, _ = m.ForegroundShell(b) // may restart the shell; must not block forever
		close(probe)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Run on self-killed shell should return an error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("DEADLOCK: Run never returned after shell death")
	}
	select {
	case <-probe:
	case <-time.After(5 * time.Second):
		t.Fatal("DEADLOCK: manager locks wedged by dying shell")
	}

	// And the bed recovers: a fresh foreground shell works.
	sh2, err := m.ForegroundShell(b)
	if err != nil {
		t.Fatalf("ForegroundShell after death: %v", err)
	}
	if res, err := sh2.Run(context.Background(), "echo back", nil); err != nil || res.ExitCode != 0 {
		t.Fatalf("recovered shell run: %v %+v", err, res)
	}
}

// slowStore wraps fakeStore with a controllable persist delay, to widen the
// eviction window for the cancel-race test.
type slowStore struct {
	*fakeStore
	gate chan struct{} // Persist blocks until this closes
}

func (s *slowStore) Persist(ctx context.Context, id, dir string, generation int64) error {
	<-s.gate
	return s.fakeStore.Persist(ctx, id, dir, generation)
}

// Activity during an evict's persist window must CANCEL the eviction —
// otherwise writes landing after the snapshot are silently destroyed with the
// workspace (docs/persistence.md §4).
func TestEvictCanceledByActivity(t *testing.T) {
	root := t.TempDir()
	ss := &slowStore{fakeStore: newFakeStore(), gate: make(chan struct{})}
	m, _ := NewManager(root, "default", "/bin/bash", isolation.New("dorm", root), nil, 0, ss)

	b, _ := m.Resolve("conv-race")
	res := make(chan struct {
		ok  bool
		err error
	}, 1)
	go func() {
		ok, err := m.Evict("conv-race")
		res <- struct {
			ok  bool
			err error
		}{ok, err}
	}()

	// While persist is blocked on the gate, the bed sees new activity.
	time.Sleep(10 * time.Millisecond) // let Evict reach Persist
	if b.State() != "evicting" {
		t.Fatalf("state during persist = %q, want evicting", b.State())
	}
	b.touch()
	close(ss.gate)

	r := <-res
	if r.err != nil || r.ok {
		t.Fatalf("Evict = (%v, %v), want canceled (false, nil)", r.ok, r.err)
	}
	// Bed survived, back to active, still resolvable.
	if b.State() != "active" {
		t.Fatalf("state after canceled evict = %q", b.State())
	}
	if _, ok := m.Get("conv-race"); !ok {
		t.Fatal("bed should still be ACTIVE after canceled evict")
	}
}

func TestPurgeEndsIdentity(t *testing.T) {
	root := t.TempDir()
	fs := newFakeStore()
	m, _ := NewManager(root, "default", "/bin/bash", isolation.New("dorm", root), nil, 0, fs)

	b, _ := m.Resolve("conv-p")
	_ = os.WriteFile(filepath.Join(b.Workspace, "data.txt"), []byte("x"), 0o644)
	if ok, _ := m.Evict("conv-p"); !ok {
		t.Fatal("evict failed")
	}
	if info, _ := fs.Stat(context.Background(), "conv-p"); info == nil {
		t.Fatal("snapshot should exist after evict (DORMANT)")
	}
	// Purge the dormant bed: snapshot AND luggage gone, resolve starts fresh.
	if err := m.Purge("conv-p"); err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if info, _ := fs.Stat(context.Background(), "conv-p"); info != nil {
		t.Fatal("snapshot should be deleted after purge")
	}
	if _, err := os.Stat(b.Dir); !os.IsNotExist(err) {
		t.Fatal("luggage should be removed after purge")
	}
	b2, _ := m.Resolve("conv-p")
	if _, err := os.Stat(filepath.Join(b2.Workspace, "restored.txt")); err == nil {
		t.Fatal("purged bed must start empty, not restored")
	}
	if _, err := os.Stat(filepath.Join(b2.Workspace, "data.txt")); err == nil {
		t.Fatal("purged bed must not resurrect old luggage data")
	}
	// Default bed is not purgeable.
	if err := m.Purge("default"); err == nil {
		t.Fatal("purging the default bed must be refused")
	}
}

// Every successful persist bumps the generation by one, and the store's
// metadata mirrors the bed meta's counter — this is the freshness token the
// luggage warm-start (and any future fencing) compares against.
func TestGenerationMonotonicAcrossPersists(t *testing.T) {
	root := t.TempDir()
	fs := newFakeStore()
	m, _ := NewManager(root, "default", "/bin/bash", isolation.New("dorm", root), nil, 0, fs)

	b, _ := m.Resolve("conv-g")
	if err := m.Checkpoint(context.Background(), "conv-g"); err != nil {
		t.Fatalf("checkpoint 1: %v", err)
	}
	if g := fs.generation("conv-g"); g != 1 {
		t.Fatalf("generation after first persist = %d, want 1", g)
	}
	if err := m.Checkpoint(context.Background(), "conv-g"); err != nil {
		t.Fatalf("checkpoint 2: %v", err)
	}
	if g := fs.generation("conv-g"); g != 2 {
		t.Fatalf("generation after second persist = %d, want 2", g)
	}
	meta, ok := loadMeta(b.Dir)
	if !ok || meta.Generation != 2 {
		t.Fatalf("local meta generation = %+v (ok=%v), want 2", meta, ok)
	}

	// A failed upload still bumps the local counter (locally dirty, ahead of
	// the store) but never advances LastPersistedAt.
	before := meta.LastPersistedAt
	fs.fail = true
	if err := m.Checkpoint(context.Background(), "conv-g"); err == nil {
		t.Fatal("checkpoint with failing store should error")
	}
	meta, _ = loadMeta(b.Dir)
	if meta.Generation != 3 || !meta.LastPersistedAt.Equal(before) {
		t.Fatalf("after failed persist: gen=%d lastPersisted=%v, want gen=3 lastPersisted=%v",
			meta.Generation, meta.LastPersistedAt, before)
	}
	if g := fs.generation("conv-g"); g != 2 {
		t.Fatalf("store generation after failed persist = %d, want 2", g)
	}
}

func TestBedDirLayoutAndMetaAcrossRestart(t *testing.T) {
	root := t.TempDir()
	m, _ := NewManager(root, "default", "/bin/bash", isolation.New("dorm", root), nil, 0, nil)

	b, _ := m.Resolve("default")
	// Layout: {root}/default/{meta.json,data}; Workspace points at data.
	if b.Workspace != filepath.Join(root, "default", "data") {
		t.Fatalf("Workspace = %s", b.Workspace)
	}
	if _, err := os.Stat(filepath.Join(b.Dir, "meta.json")); err != nil {
		t.Fatalf("meta.json missing: %v", err)
	}
	created := b.CreatedAt

	// "Restart": a new Manager over the same root sees the same identity.
	time.Sleep(5 * time.Millisecond)
	m2, _ := NewManager(root, "default", "/bin/bash", isolation.New("dorm", root), nil, 0, nil)
	b2, _ := m2.Resolve("default")
	if !b2.CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt not preserved across restart: %v vs %v", b2.CreatedAt, created)
	}
}
