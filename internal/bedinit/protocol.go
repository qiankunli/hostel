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

// Package bedinit implements the per-bed init process (docs/design.md
// 〈进程树〉, S1): a tiny spawner-reaper the daemon re-execs once per bed. Bed
// commands are forked BY bedinit (parentage is decided by who forks), so the
// bed owns a real process tree: teardown = SIGTERM bedinit → it kills every
// descendant (a /proc ppid scan also catches reparented setsid orphans — it is
// the subreaper) and exits. The daemon talks to it over a unix socket, one
// connection per spawn, stdio crossing as SCM_RIGHTS fds.
//
// Shape follows containerd's shim (small per-unit process owning a tree,
// IPC to the daemon); tini/dumb-init don't fit (pure reapers, no spawn IPC)
// and a shell doesn't either (in-band stdin protocol — the exact fragility
// that killed the shared foreground shell).
package bedinit

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"syscall"
)

// InitArg is the hidden subcommand hostel re-execs into to become a bed's
// init: `hostel __bedinit --socket <path> --bed <id>`.
const InitArg = "__bedinit"

// spawnRequest asks bedinit to fork one fully-specified command. Argv[0] is an
// absolute path (the daemon resolves via exec.LookPath); Env is the COMPLETE
// child environment. Three SCM_RIGHTS fds ride along: stdin, stdout, stderr.
type spawnRequest struct {
	Argv []string `json:"argv"`
	Dir  string   `json:"dir,omitempty"`
	Env  []string `json:"env"`
}

// reply is bedinit → daemon. Exactly two replies per connection: {pid} once
// the child is running (or {error}), then {exit} when it is reaped.
type reply struct {
	Pid   int    `json:"pid,omitempty"`
	Exit  *int   `json:"exit,omitempty"`
	Error string `json:"error,omitempty"`
}

// writeMsg sends one length-prefixed JSON message, with fds attached as
// SCM_RIGHTS on the first write when given.
func writeMsg(conn *net.UnixConn, v any, fds []int) error {
	payload, err := json.Marshal(v)
	if err != nil {
		return err
	}
	buf := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(buf, uint32(len(payload)))
	copy(buf[4:], payload)
	var oob []byte
	if len(fds) > 0 {
		oob = syscall.UnixRights(fds...)
	}
	n, _, err := conn.WriteMsgUnix(buf, oob, nil)
	if err != nil {
		return err
	}
	// The rights went with the first fragment; push any remainder plainly.
	for n < len(buf) {
		m, err := conn.Write(buf[n:])
		if err != nil {
			return err
		}
		n += m
	}
	return nil
}

// readMsg reads one length-prefixed JSON message, collecting any SCM_RIGHTS
// fds that arrive with it (a stream read may fragment; rights can ride any
// fragment, in practice the first).
func readMsg(conn *net.UnixConn, v any) ([]int, error) {
	var fds []int
	buf := make([]byte, 0, 4096)
	need := -1 // unknown until the 4-byte header is complete
	chunk := make([]byte, 64<<10)
	oob := make([]byte, syscall.CmsgSpace(16*4))
	for {
		if need >= 0 && len(buf) >= 4+need {
			break
		}
		n, oobn, _, _, err := conn.ReadMsgUnix(chunk, oob)
		if err != nil {
			return fds, err
		}
		if oobn > 0 {
			got, err := parseRights(oob[:oobn])
			if err != nil {
				return fds, err
			}
			fds = append(fds, got...)
		}
		buf = append(buf, chunk[:n]...)
		if need < 0 && len(buf) >= 4 {
			need = int(binary.BigEndian.Uint32(buf))
		}
	}
	if err := json.Unmarshal(buf[4:4+need], v); err != nil {
		return fds, fmt.Errorf("bedinit: decode message: %w", err)
	}
	return fds, nil
}

func parseRights(oob []byte) ([]int, error) {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, fmt.Errorf("bedinit: parse control message: %w", err)
	}
	var fds []int
	for _, m := range msgs {
		got, err := syscall.ParseUnixRights(&m)
		if err != nil {
			return nil, fmt.Errorf("bedinit: parse rights: %w", err)
		}
		fds = append(fds, got...)
	}
	return fds, nil
}
