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
	"flag"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Run is the bedinit process entry (`hostel __bedinit --socket S --bed B`).
// It never returns to the caller's main path — the exit code is the process's.
func Run(args []string) int {
	fs := flag.NewFlagSet(InitArg, flag.ContinueOnError)
	socket := fs.String("socket", "", "unix socket to serve spawn requests on")
	bed := fs.String("bed", "", "bed id (ps visibility only)")
	if err := fs.Parse(args); err != nil || *socket == "" {
		log.Printf("bedinit: bad args (need --socket): %v", err)
		return 2
	}

	// Subreaper: every descendant orphaned anywhere below us reparents HERE,
	// not to pid 1 — that is what makes the /proc ppid scan in killAll able to
	// enumerate double-forked daemons, and what lets the reaper collect them.
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		log.Printf("bedinit[%s]: set subreaper: %v", *bed, err)
		return 1
	}

	_ = os.Remove(*socket)
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: *socket, Net: "unix"})
	if err != nil {
		log.Printf("bedinit[%s]: listen %s: %v", *bed, *socket, err)
		return 1
	}
	defer os.Remove(*socket)

	s := &server{bed: *bed, watchers: make(map[int]chan int), unclaimed: make(map[int]int)}
	go s.reap()

	// SIGTERM = bed teardown: stop serving, kill the whole tree, exit.
	term := make(chan os.Signal, 1)
	signal.Notify(term, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-term
		ln.Close()
		s.killAll()
		os.Exit(0)
	}()

	for {
		conn, err := ln.AcceptUnix()
		if err != nil {
			// Listener closed by the SIGTERM path (or a fatal accept error
			// with the daemon gone either way): let the signal goroutine win.
			time.Sleep(time.Second)
			return 0
		}
		go s.serveSpawn(conn)
	}
}

type server struct {
	bed string

	mu        sync.Mutex
	watchers  map[int]chan int // pid → exit-code delivery
	unclaimed map[int]int      // exited before the watcher registered
}

// reap is the single wait loop: dispatches exit codes for spawned children and
// silently collects adopted orphans. Nobody else may wait4 — os/exec is
// deliberately unused in this process.
func (s *server) reap() {
	sigc := make(chan os.Signal, 64)
	signal.Notify(sigc, syscall.SIGCHLD)
	for range sigc {
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if pid <= 0 || err != nil {
				break
			}
			if !ws.Exited() && !ws.Signaled() {
				continue // stopped/continued: not terminal
			}
			code := ws.ExitStatus()
			if ws.Signaled() {
				code = 128 + int(ws.Signal())
			}
			s.mu.Lock()
			if ch, ok := s.watchers[pid]; ok {
				delete(s.watchers, pid)
				ch <- code
			} else {
				// Either an adopted orphan (fine, reaped) or a spawn racing
				// its watcher registration (claimed under the same lock).
				s.unclaimed[pid] = code
			}
			s.mu.Unlock()
		}
	}
}

// serveSpawn handles one connection = one spawn: fork the requested command
// with the fds that rode along, reply {pid}, then {exit} once reaped.
func (s *server) serveSpawn(conn *net.UnixConn) {
	defer conn.Close()
	var req spawnRequest
	fds, err := readMsg(conn, &req)
	// The child dups the fds at fork; our copies must go regardless of outcome.
	defer func() {
		for _, fd := range fds {
			syscall.Close(fd)
		}
	}()
	if err != nil {
		_ = writeMsg(conn, reply{Error: "read spawn request: " + err.Error()}, nil)
		return
	}
	if len(req.Argv) == 0 || len(fds) != 3 {
		_ = writeMsg(conn, reply{Error: "spawn request needs argv and exactly 3 fds"}, nil)
		return
	}

	exitc := make(chan int, 1)
	s.mu.Lock()
	pid, err := syscall.ForkExec(req.Argv[0], req.Argv, &syscall.ProcAttr{
		Dir:   req.Dir,
		Env:   req.Env,
		Files: []uintptr{uintptr(fds[0]), uintptr(fds[1]), uintptr(fds[2])},
		Sys: &syscall.SysProcAttr{
			Setpgid: true, // one pgid per command: the group kill takes its tree
			// If bedinit itself dies unexpectedly, direct children must not
			// leak into the pod as unattributable strays.
			Pdeathsig: syscall.SIGKILL,
		},
	})
	if err == nil {
		if code, done := s.unclaimed[pid]; done { // exited before we got here
			delete(s.unclaimed, pid)
			exitc <- code
		} else {
			s.watchers[pid] = exitc
		}
	}
	s.mu.Unlock()
	if err != nil {
		_ = writeMsg(conn, reply{Error: "fork: " + err.Error()}, nil)
		return
	}
	if err := writeMsg(conn, reply{Pid: pid}, nil); err != nil {
		return // daemon gone; the reaper still collects the child
	}
	code := <-exitc
	_ = writeMsg(conn, reply{Exit: &code}, nil)
}

// killAll force-kills every descendant. The /proc ppid scan enumerates both
// direct children and reparented (setsid/double-fork) orphans — as subreaper
// we are their parent now; killing by pgid AND pid per round, re-scanning
// until the tree is empty, converges even when kills race fresh forks.
func (s *server) killAll() {
	self := os.Getpid()
	for range 50 {
		pids := childrenOf(self)
		if len(pids) == 0 {
			return
		}
		for _, pid := range pids {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
		time.Sleep(20 * time.Millisecond) // let the reaper drain
	}
	log.Printf("bedinit[%s]: descendants survived kill loop", s.bed)
}

// childrenOf lists live pids whose ppid is p (via /proc/*/stat; comm may
// contain anything, so fields are taken after the LAST ')').
func childrenOf(p int) []int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		rest := string(stat)
		if i := strings.LastIndexByte(rest, ')'); i >= 0 {
			rest = rest[i+1:]
		}
		f := strings.Fields(rest)
		if len(f) >= 2 && f[1] == strconv.Itoa(p) {
			out = append(out, pid)
		}
	}
	return out
}
