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
	"strings"
	"testing"
	"time"

	"github.com/qiankunli/hostel/internal/isolation"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := NewManager(t.TempDir(), "default", "/bin/bash", isolation.New("direct"), nil)
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
