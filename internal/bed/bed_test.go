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
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	root := t.TempDir()
	m, err := NewManager(root, "default", "/bin/bash", isolation.New("direct", root), nil, 0, nil)
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
	if err := m.Delete("conv-x"); err != nil {
		t.Fatalf("Delete: %v", err)
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
	m, err := NewManager(root, "default", "/bin/bash", isolation.New("direct", root), nil, 2, nil)
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
	// Deleting frees a slot.
	if err := m.Delete("a"); err != nil {
		t.Fatalf("delete a: %v", err)
	}
	if _, err := m.Resolve("c"); err != nil {
		t.Fatalf("bed c after free slot: %v", err)
	}
}

// fakeStore is an in-memory Store for lifecycle tests.
type fakeStore struct {
	mu    sync.Mutex
	snaps map[string][]byte // bedID → marker file content
	fail  bool              // force Persist to fail
}

func newFakeStore() *fakeStore { return &fakeStore{snaps: map[string][]byte{}} }

func (f *fakeStore) Name() string { return "fake" }
func (f *fakeStore) Exists(_ context.Context, id string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.snaps[id]
	return ok, nil
}
func (f *fakeStore) Restore(_ context.Context, id, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return os.WriteFile(filepath.Join(dir, "restored.txt"), f.snaps[id], 0o644)
}
func (f *fakeStore) Persist(_ context.Context, id, dir string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return errors.New("fake persist failure")
	}
	data, _ := os.ReadFile(filepath.Join(dir, "data.txt"))
	f.snaps[id] = data
	return nil
}

func TestPersistOnDeleteAndRestoreOnCreate(t *testing.T) {
	root := t.TempDir()
	fs := newFakeStore()
	m, err := NewManager(root, "default", "/bin/bash", isolation.New("direct", root), nil, 0, fs)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// Write data into a bed, delete it → snapshot taken, workspace gone.
	b, _ := m.Resolve("conv-1")
	if err := os.WriteFile(filepath.Join(b.Workspace, "data.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete("conv-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(b.Workspace); !os.IsNotExist(err) {
		t.Fatal("workspace should be removed after delete")
	}
	if string(fs.snaps["conv-1"]) != "payload" {
		t.Fatalf("snapshot content = %q", fs.snaps["conv-1"])
	}

	// Re-resolve the same bed id → restored before serving.
	b2, err := m.Resolve("conv-1")
	if err != nil {
		t.Fatalf("re-Resolve: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b2.Workspace, "restored.txt")); err != nil {
		t.Fatalf("restore marker missing: %v", err)
	}
}

func TestPersistFailureAbortsDelete(t *testing.T) {
	root := t.TempDir()
	fs := newFakeStore()
	fs.fail = true
	m, _ := NewManager(root, "default", "/bin/bash", isolation.New("direct", root), nil, 0, fs)

	b, _ := m.Resolve("conv-2")
	if err := m.Delete("conv-2"); err == nil {
		t.Fatal("Delete should fail when persist fails")
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
	m, _ := NewManager(root, "default", "/bin/bash", isolation.New("direct", root), nil, 0, fs)

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
