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
	"os/exec"
	"sync"
	"syscall"
)

// Spawner is the seam between "what to run" and "who forks it"
// (docs/design.md 〈进程树〉). The Manager builds the fully-specified command —
// argv, dir, env, isolation already applied, stdio as concrete *os.File pipe
// ends (never StdinPipe/StdoutPipe conveniences: raw fds must be extractable to
// hand to an out-of-process spawner) — and the Spawner owns process lifetime:
// starting, killing, and the per-bed sweep at teardown.
//
// Implementations: inProcSpawner (all platforms, commands are the daemon's
// children, liveness tracked in a map) and, on linux, the bed-init spawner
// (S1: commands are children of the bed's own init, teardown = kill its tree).
type Spawner interface {
	// Start launches cmd on behalf of the bed and returns its handle.
	Start(bedID string, cmd *exec.Cmd) (Proc, error)
	// KillBed force-kills every live process started for the bed. The safety
	// net behind per-process kills: it must not rely on callers having waited.
	KillBed(bedID string)
}

// Proc is one started bed process.
type Proc interface {
	Pid() int
	// Kill force-kills the process group.
	Kill()
	// Wait blocks until the process exits and returns its exit code. A non-exit
	// failure (wait error, spawner channel broken) returns -1 and the error.
	// Call at most once.
	Wait() (int, error)
}

// inProcSpawner forks commands as direct children of the daemon and tracks
// live process groups per bed so teardown can sweep them. Every command is
// started with Setpgid, so pid == pgid and Kill(-pid) takes the whole tree
// (modulo setsid escapees — the bed-init spawner closes that gap on linux).
type inProcSpawner struct {
	mu   sync.Mutex
	live map[string]map[int]struct{} // bedID → live pids (== pgids)
}

func newInProcSpawner() *inProcSpawner {
	return &inProcSpawner{live: make(map[string]map[int]struct{})}
}

func (s *inProcSpawner) Start(bedID string, cmd *exec.Cmd) (Proc, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true // one pgid per command: kill takes the tree
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	pid := cmd.Process.Pid
	s.mu.Lock()
	if s.live[bedID] == nil {
		s.live[bedID] = make(map[int]struct{})
	}
	s.live[bedID][pid] = struct{}{}
	s.mu.Unlock()
	return &inProcProc{cmd: cmd, pid: pid, untrack: func() { s.untrack(bedID, pid) }}, nil
}

func (s *inProcSpawner) untrack(bedID string, pid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pids, ok := s.live[bedID]; ok {
		delete(pids, pid)
		if len(pids) == 0 {
			delete(s.live, bedID)
		}
	}
}

func (s *inProcSpawner) KillBed(bedID string) {
	s.mu.Lock()
	pids := make([]int, 0, len(s.live[bedID]))
	for pid := range s.live[bedID] {
		pids = append(pids, pid)
	}
	s.mu.Unlock()
	for _, pid := range pids {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}
}

type inProcProc struct {
	cmd     *exec.Cmd
	pid     int
	untrack func()
	once    sync.Once
}

func (p *inProcProc) Pid() int { return p.pid }

func (p *inProcProc) Kill() { _ = syscall.Kill(-p.pid, syscall.SIGKILL) }

func (p *inProcProc) Wait() (int, error) {
	err := p.cmd.Wait()
	p.once.Do(p.untrack)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode(), nil
		}
		return -1, err
	}
	return 0, nil
}
