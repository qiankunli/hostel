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
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/qiankunli/hostel/internal/bedinit"
)

// initSpawner forks bed processes through the bed's own init (`hostel
// __bedinit`, docs/design.md 〈进程树〉 S1): every command becomes a child of
// that init, so the bed owns a real process tree — KillBed terminates the init,
// which kills every descendant including reparented setsid daemons. The daemon
// side stays protocol-thin: build the command as usual, hand argv+env+fds over
// the bed's socket.
type initSpawner struct {
	exe     string // the hostel binary to re-exec as __bedinit
	sockDir string // LOCAL dir for per-bed sockets (workspace may be network FS)

	mu    sync.Mutex
	inits map[string]*initHandle // bedID → live bedinit
}

type initHandle struct {
	proc   *os.Process
	socket string
	done   chan struct{} // closed once the bedinit process exited
}

func newInitSpawner(exe string) (*initSpawner, error) {
	// Unix sockets don't belong on the (possibly network) workspace FS.
	dir, err := os.MkdirTemp("", "hostel-bedinit-*")
	if err != nil {
		return nil, err
	}
	return &initSpawner{exe: exe, sockDir: dir, inits: make(map[string]*initHandle)}, nil
}

// ensure returns the bed's live init, starting one on first use (or after a
// crash: a dead init self-removes from the map, so the next spawn heals it).
func (s *initSpawner) ensure(bedID string) (*initHandle, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if h, ok := s.inits[bedID]; ok {
		select {
		case <-h.done: // died; fall through and restart
		default:
			return h, nil
		}
	}
	socket := filepath.Join(s.sockDir, bedID+".sock")
	_ = os.Remove(socket)
	cmd := exec.Command(s.exe, bedinit.InitArg, "--socket", socket, "--bed", bedID)
	cmd.Stdout = os.Stderr // bedinit logs join the daemon's stream
	cmd.Stderr = os.Stderr
	setPdeathsig(cmd) // daemon death → SIGTERM → the init takes its tree along
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("bed: start bedinit for %s: %w", bedID, err)
	}
	h := &initHandle{proc: cmd.Process, socket: socket, done: make(chan struct{})}
	go func() {
		_, _ = cmd.Process.Wait()
		close(h.done)
		s.mu.Lock()
		if s.inits[bedID] == h {
			delete(s.inits, bedID)
		}
		s.mu.Unlock()
	}()
	// The socket appearing is the ready signal.
	for range 100 {
		if _, err := os.Stat(socket); err == nil {
			s.inits[bedID] = h
			return h, nil
		}
		select {
		case <-h.done:
			return nil, fmt.Errorf("bed: bedinit for %s exited before serving", bedID)
		case <-time.After(20 * time.Millisecond):
		}
	}
	_ = cmd.Process.Kill()
	return nil, fmt.Errorf("bed: bedinit for %s never came up", bedID)
}

func (s *initSpawner) Start(bedID string, cmd *exec.Cmd) (Proc, error) {
	h, err := s.ensure(bedID)
	if err != nil {
		return nil, err
	}
	stdout, err := fileOf(cmd.Stdout, "stdout")
	if err != nil {
		return nil, err
	}
	stderr, err := fileOf(cmd.Stderr, "stderr")
	if err != nil {
		return nil, err
	}
	stdin, err := fileOf(cmd.Stdin, "stdin")
	if err != nil {
		return nil, err
	}
	if stdin == nil { // one-shots leave stdin unset; the child still needs fd 0
		devnull, err := os.Open(os.DevNull)
		if err != nil {
			return nil, err
		}
		defer devnull.Close()
		stdin = devnull
	}
	env := cmd.Env
	if env == nil {
		env = os.Environ()
	}
	// bedinit execs Argv[0] as the path — exec.Command resolved it into
	// cmd.Path, while cmd.Args[0] may still be the bare name.
	argv := append([]string{cmd.Path}, cmd.Args[1:]...)
	handle, err := bedinit.Spawn(h.socket, argv, cmd.Dir, env, stdin, stdout, stderr)
	if err != nil {
		return nil, err
	}
	return &initProc{h: handle}, nil
}

// fileOf enforces the Spawner seam contract: stdio must be concrete *os.File
// (or nil) so the raw fd can cross to the init via SCM_RIGHTS.
func fileOf(v any, name string) (*os.File, error) {
	if v == nil {
		return nil, nil
	}
	f, ok := v.(*os.File)
	if !ok {
		return nil, fmt.Errorf("bed: %s must be *os.File for the bedinit spawner, got %T", name, v)
	}
	return f, nil
}

// KillBed tears the bed's whole process tree down by terminating its init:
// SIGTERM triggers bedinit's kill loop (children by pgid + reparented orphans
// by /proc scan), with a SIGKILL fallback if it wedges.
func (s *initSpawner) KillBed(bedID string) {
	s.mu.Lock()
	h, ok := s.inits[bedID]
	if ok {
		delete(s.inits, bedID)
	}
	s.mu.Unlock()
	if !ok {
		return
	}
	_ = h.proc.Signal(syscall.SIGTERM)
	select {
	case <-h.done:
	case <-time.After(3 * time.Second):
		_ = h.proc.Kill()
	}
	_ = os.Remove(h.socket)
}

type initProc struct {
	h *bedinit.Handle
}

func (p *initProc) Pid() int { return p.h.Pid() }

// Kill takes the command's process group directly — at S1 the daemon shares
// the pid namespace with the child, so no round-trip through the init.
func (p *initProc) Kill() { _ = syscall.Kill(-p.h.Pid(), syscall.SIGKILL) }

func (p *initProc) Wait() (int, error) { return p.h.WaitExit() }

// EnableBedInit switches the manager to the bedinit spawner after a smoke test
// (start a probe init, run /bin/true through it, tear it down). On platforms or
// deployments where bedinit can't serve, the error lets the caller log and stay
// on the in-process spawner — "auto" semantics, decided once at boot.
func (m *Manager) EnableBedInit(exe string) error {
	sp, err := newInitSpawner(exe)
	if err != nil {
		return err
	}
	const probe = "bedinit-probe"
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	sink, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer sink.Close()
	cmd := exec.Command("/bin/true")
	cmd.Stdin = devnull
	cmd.Stdout = sink
	cmd.Stderr = sink
	proc, err := sp.Start(probe, cmd)
	if err != nil {
		sp.KillBed(probe)
		return fmt.Errorf("bedinit probe: %w", err)
	}
	if code, err := proc.Wait(); err != nil || code != 0 {
		sp.KillBed(probe)
		return fmt.Errorf("bedinit probe: exit=%d err=%v", code, err)
	}
	sp.KillBed(probe)
	m.spawner = sp
	return nil
}
