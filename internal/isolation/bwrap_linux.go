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

//go:build linux

package isolation

import (
	"os/exec"
)

// bwrap confines each command under bubblewrap: new mount/pid/uts/ipc
// namespaces, read-only root, the bed workspace bind-mounted rw. This is the
// minimal v1 argv; the fuller OSEP-0013 stack (per-profile /tmp, seccomp memfd,
// real setuid via setpriv, overlay upper) is ported incrementally from
// OpenSandbox execd — see docs/weak-tier.md "hostel" roadmap.
type bwrap struct {
	path string
}

func newBwrap() Isolator {
	path, _ := exec.LookPath("bwrap")
	return &bwrap{path: path}
}

func (b *bwrap) Name() string    { return "bwrap" }
func (b *bwrap) Available() bool  { return b.path != "" }

func (b *bwrap) Wrap(cmd *exec.Cmd, ws Workspace) error {
	if b.path == "" {
		// No bwrap on host — degrade to cwd pinning rather than fail the exec.
		cmd.Dir = ws.Path
		return nil
	}
	argv := []string{
		"--unshare-pid", "--unshare-uts", "--unshare-ipc",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
		"--bind", ws.Path, ws.Path,
		"--chdir", ws.Path,
		"--",
	}
	userArgs := cmd.Args
	cmd.Args = make([]string, 0, len(argv)+len(userArgs)+1)
	cmd.Args = append(cmd.Args, b.path)
	cmd.Args = append(cmd.Args, argv...)
	cmd.Args = append(cmd.Args, userArgs...)
	cmd.Path = b.path
	return nil
}
