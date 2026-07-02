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
	"fmt"
	"log"
	"os"
	"os/exec"
)

// bwrap confines each command under bubblewrap. Mount view per
// docs/data-isolation.md: RO host root, sibling beds masked out of existence,
// own workspace rw at the canonical /workspace, host user data and mounted
// secrets masked, secret-looking env vars stripped.
type bwrap struct {
	path      string   // bwrap binary (probed at boot)
	root      string   // parent dir of all bed workspaces (masked in-sandbox)
	maskPaths []string // existing sensitive host paths to mask (computed once)
}

// newBwrap probes bubblewrap at boot: binary present AND a minimal namespace
// actually starts (binary-present-but-broken — e.g. unprivileged userns
// disabled — must not count as isolated). On failure it falls back to direct
// so the daemon still boots and /healthz reports the truth.
// Probe pattern borrowed from OpenSandbox execd.
func newBwrap(workspaceRoot string) Isolator {
	path, err := exec.LookPath("bwrap")
	if err != nil {
		log.Printf("isolation: bwrap not found, falling back to direct (no isolation)")
		return direct{}
	}
	if err := bwrapSmoke(path); err != nil {
		log.Printf("isolation: bwrap found but unusable (%v), falling back to direct", err)
		return direct{}
	}
	var masks []string
	for _, p := range defaultMaskCandidates {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			masks = append(masks, p)
		}
	}
	return &bwrap{path: path, root: workspaceRoot, maskPaths: masks}
}

// bwrapSmoke verifies bwrap can create the namespaces we ask for.
func bwrapSmoke(path string) error {
	cmd := exec.Command(path,
		"--unshare-pid", "--unshare-uts", "--unshare-ipc",
		"--ro-bind", "/", "/",
		"--proc", "/proc",
		"--", "true",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("smoke test: %w (%s)", err, out)
	}
	return nil
}

func (b *bwrap) Name() string       { return "bwrap" }
func (b *bwrap) Available() bool    { return true } // probed at construction
func (b *bwrap) MountPoint() string { return BwrapMountPoint }

func (b *bwrap) Wrap(cmd *exec.Cmd, ws Workspace) error {
	// No silent degradation past this point: this isolator passed the boot
	// probe, so any failure to build the sandbox is a hard error.
	argv := buildBwrapArgs(b.root, ws.Path, b.maskPaths, os.Environ())
	userArgs := cmd.Args
	cmd.Args = make([]string, 0, len(argv)+len(userArgs)+1)
	cmd.Args = append(cmd.Args, b.path)
	cmd.Args = append(cmd.Args, argv...)
	cmd.Args = append(cmd.Args, userArgs...)
	cmd.Path = b.path
	return nil
}
