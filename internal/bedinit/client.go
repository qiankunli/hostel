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
	"fmt"
	"net"
	"os"
)

// Spawn is the daemon side of one spawn: dial the bed's init, hand over the
// command spec plus the three stdio fds, and return once the child is running.
// The returned handle's WaitExit blocks for the exit reply. The caller keeps
// ownership of the *os.File ends it passed (close your copies as usual — the
// fds are dup'ed across the socket).
func Spawn(socket string, argv []string, dir string, env []string, stdin, stdout, stderr *os.File) (*Handle, error) {
	raddr := &net.UnixAddr{Name: socket, Net: "unix"}
	conn, err := net.DialUnix("unix", nil, raddr)
	if err != nil {
		return nil, fmt.Errorf("bedinit: dial %s: %w", socket, err)
	}
	req := spawnRequest{Argv: argv, Dir: dir, Env: env}
	fds := []int{int(stdin.Fd()), int(stdout.Fd()), int(stderr.Fd())}
	if err := writeMsg(conn, req, fds); err != nil {
		conn.Close()
		return nil, fmt.Errorf("bedinit: send spawn: %w", err)
	}
	var rep reply
	if _, err := readMsg(conn, &rep); err != nil {
		conn.Close()
		return nil, fmt.Errorf("bedinit: read pid: %w", err)
	}
	if rep.Error != "" {
		conn.Close()
		return nil, fmt.Errorf("bedinit: spawn: %s", rep.Error)
	}
	return &Handle{conn: conn, pid: rep.Pid}, nil
}

// Handle is one spawned child, connection-scoped: the conn lives until the
// exit reply.
type Handle struct {
	conn *net.UnixConn
	pid  int
}

// Pid returns the child's pid (valid in the daemon's pid namespace at S1).
func (h *Handle) Pid() int { return h.pid }

// WaitExit blocks until bedinit reports the child's exit code. Call at most
// once; the connection is closed on return.
func (h *Handle) WaitExit() (int, error) {
	defer h.conn.Close()
	var rep reply
	if _, err := readMsg(h.conn, &rep); err != nil {
		return -1, fmt.Errorf("bedinit: read exit: %w", err)
	}
	if rep.Error != "" {
		return -1, fmt.Errorf("bedinit: %s", rep.Error)
	}
	if rep.Exit == nil {
		return -1, fmt.Errorf("bedinit: exit reply without code")
	}
	return *rep.Exit, nil
}
