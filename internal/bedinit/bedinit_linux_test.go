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

package bedinit

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain doubles as the bedinit helper process (stdlib helper idiom): the
// subreaper semantics under test require a REAL separate process — in-process
// Serve would make children of the test binary and its wait4(-1) loop would
// race os/exec used elsewhere.
func TestMain(m *testing.M) {
	if os.Getenv("BEDINIT_HELPER") == "1" {
		os.Exit(Run([]string{"--socket", os.Getenv("BEDINIT_SOCKET"), "--bed", "test"}))
	}
	os.Exit(m.Run())
}

// startInit launches the helper bedinit and waits for its socket.
func startInit(t *testing.T) (socket string, proc *os.Process) {
	t.Helper()
	socket = filepath.Join(t.TempDir(), "init.sock")
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "BEDINIT_HELPER=1", "BEDINIT_SOCKET="+socket)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(socket); err == nil {
			return socket, cmd.Process
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("bedinit socket never appeared")
	return "", nil
}

// spawnSh spawns `sh -c script` via the init, returning the handle and the
// read end of its combined output.
func spawnSh(t *testing.T, socket, script string) (*Handle, *os.File) {
	t.Helper()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	h, err := Spawn(socket, []string{"/bin/sh", "-c", script}, "", os.Environ(), devnull, pw, pw)
	pw.Close()
	if err != nil {
		pr.Close()
		t.Fatalf("Spawn: %v", err)
	}
	return h, pr
}

func TestSpawnExitCodeAndOutput(t *testing.T) {
	socket, _ := startInit(t)

	h, out := spawnSh(t, socket, "echo hi from bedinit; exit 7")
	data, _ := io.ReadAll(out)
	out.Close()
	code, err := h.WaitExit()
	if err != nil {
		t.Fatalf("WaitExit: %v", err)
	}
	if code != 7 {
		t.Fatalf("exit = %d, want 7", code)
	}
	if !strings.Contains(string(data), "hi from bedinit") {
		t.Fatalf("output = %q", data)
	}
}

// TestParentageAndSigtermKillsTree is the point of bedinit: the spawned child
// is a child of the INIT (not of this process), and SIGTERM to the init takes
// the whole tree down — including a setsid daemon that escaped its pgid.
func TestParentageAndSigtermKillsTree(t *testing.T) {
	socket, initProc := startInit(t)

	// Long runner + a setsid-style escapee writing its pid.
	pidfile := filepath.Join(t.TempDir(), "escapee.pid")
	h, out := spawnSh(t, socket,
		"setsid sh -c 'echo $$ > "+pidfile+"; sleep 60' & echo started; sleep 60")
	go io.Copy(io.Discard, out) //nolint:errcheck // drain so the child never blocks
	defer out.Close()

	// Parentage: the child's ppid must be the init, not us.
	waitFor(t, "child parented to init", func() bool {
		stat, err := os.ReadFile("/proc/" + itoa(h.Pid()) + "/stat")
		return err == nil && strings.Contains(string(stat), ") S "+itoa(initProc.Pid)+" ")
	})
	var escapee int
	waitFor(t, "escapee pidfile", func() bool {
		b, err := os.ReadFile(pidfile)
		if err != nil {
			return false
		}
		escapee = atoi(strings.TrimSpace(string(b)))
		return escapee > 0
	})

	if err := initProc.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM init: %v", err)
	}
	waitFor(t, "direct child killed", func() bool { return !alive(h.Pid()) })
	waitFor(t, "setsid escapee killed", func() bool { return !alive(escapee) })
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

func alive(pid int) bool {
	// Signal 0 probes existence; a zombie still "exists" but its stat state is
	// Z and it is gone for our purposes once the parent (bedinit) reaped it —
	// after bedinit exits, reparenting to real init reaps promptly.
	err := syscall.Kill(pid, 0)
	if err != nil {
		return false
	}
	stat, rerr := os.ReadFile("/proc/" + itoa(pid) + "/stat")
	if rerr != nil {
		return false
	}
	return !strings.Contains(string(stat), ") Z ")
}

func itoa(i int) string { return strconv.Itoa(i) }

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
