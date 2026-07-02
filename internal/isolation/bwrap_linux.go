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

// newBwrap probes bubblewrap at boot: binary present AND the FULL mount shape
// we will actually use starts (binary-present-but-broken — unprivileged userns
// disabled, or no /workspace mount point on the RO host root — must not count
// as isolated; a partial probe once let healthz report workspace_mount:true
// while every exec failed). On failure it falls back to direct so the daemon
// still boots and /healthz reports the truth.
// Probe pattern borrowed from OpenSandbox execd, extended to the real argv.
func newBwrap(workspaceRoot string) Isolator {
	path, err := exec.LookPath("bwrap")
	if err != nil {
		return unavailable{name: "bwrap", lvl: Suite}
	}

	// bwrap cannot mkdir the mount point inside the read-only root bind, so
	// the canonical /workspace must exist on the HOST. Create it if we can
	// (in a pod hostel usually runs as root); if we can't, the full-shape
	// smoke below fails and we honestly degrade.
	if err := os.MkdirAll(BwrapMountPoint, 0o755); err != nil {
		log.Printf("isolation: cannot ensure mount point %s on host: %v", BwrapMountPoint, err)
	}
	// The workspace root may not exist yet at probe time (the bed manager
	// creates it later); the smoke test masks it, so it must exist now.
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		log.Printf("isolation: cannot create workspace root %s: %v", workspaceRoot, err)
	}

	var masks []string
	for _, p := range defaultMaskCandidates {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			masks = append(masks, p)
		}
	}
	if err := bwrapSmoke(path, workspaceRoot, masks); err != nil {
		log.Printf("isolation: bwrap found but unusable (%v)", err)
		return unavailable{name: "bwrap", lvl: Suite}
	}
	return &bwrap{path: path, root: workspaceRoot, maskPaths: masks}
}

// bwrapSmoke runs `true` under the exact argv shape used for real commands —
// namespaces, masking, and the /workspace bind all get exercised, so whatever
// passes here works for beds too.
func bwrapSmoke(path, workspaceRoot string, masks []string) error {
	probeWs, err := os.MkdirTemp(workspaceRoot, ".probe-*")
	if err != nil {
		return fmt.Errorf("smoke test: temp workspace: %w", err)
	}
	defer os.RemoveAll(probeWs)

	argv := buildBwrapArgs(workspaceRoot, probeWs, masks, nil)
	cmd := exec.Command(path, append(argv, "true")...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("smoke test: %w (%s)", err, out)
	}
	return nil
}

func (b *bwrap) Name() string       { return "bwrap" }
func (b *bwrap) Level() Level       { return Suite }
func (b *bwrap) Available() bool    { return true } // only constructed when probe passed
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
