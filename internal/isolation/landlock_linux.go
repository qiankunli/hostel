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
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"

	ll "github.com/landlock-lsm/go-landlock/landlock"
	lls "github.com/landlock-lsm/go-landlock/landlock/syscall"
)

// ConfineArg is the hidden subcommand hostel re-execs into to apply Landlock
// before exec-ing the real command (main handles it). Landlock can only
// restrict the CALLING process, never hostel itself (which manages every bed
// and needs full FS), so — like bwrap's external argv prefix — the mechanism is
// a self-re-exec: `hostel __confine <bedDataDir> -- <cmd>...`.
const ConfineArg = "__confine"

// landlock is the room mechanism: kernel-enforced filesystem access control
// (Landlock LSM, Linux 5.13+), no capability required. A bed can't ACCESS
// other beds' data (EACCES), but siblings stay visible and host paths (/tmp,
// /usr) are shared — the "private room, shared toilet" tier.
type landlock struct {
	self string // hostel binary path, re-execed as the confiner
}

func newLandlock(workspaceRoot string) Isolator {
	// Probe: Landlock ABI ≥ 1 means the kernel exposes filesystem restrictions
	// (a custom kernel without CONFIG_SECURITY_LANDLOCK fails right here).
	if v, err := lls.LandlockGetABIVersion(); err != nil || v < 1 {
		return unavailable{name: "landlock", lvl: Room}
	}
	self, err := os.Executable()
	if err != nil {
		return unavailable{name: "landlock", lvl: Room}
	}
	// The workspace root may not exist yet at probe time (the bed manager
	// creates it later); the smoke confines a temp dir under it.
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		log.Printf("isolation: cannot create workspace root %s: %v", workspaceRoot, err)
	}
	// ABI presence alone doesn't prove ENFORCEMENT — run the full form once.
	if err := landlockSmoke(self, workspaceRoot); err != nil {
		log.Printf("isolation: landlock ABI present but unusable (%v)", err)
		return unavailable{name: "landlock", lvl: Room}
	}
	return &landlock{self: self}
}

// landlockSmoke proves the restriction actually bites, using the exact
// production form: `hostel __confine <own> -- /bin/sh -c <check>` with a fake
// sibling bed next to the confined dir. The check must find its own dir
// writable and the sibling's file unreadable. This catches what the ABI probe
// can't: ApplyConfine is BestEffort (can silently no-op into zero isolation),
// and a workspace root placed inside a shared-RW allowance (e.g. under /tmp)
// leaves sibling beds reachable — in both cases room's guarantee would be a
// lie, so we honestly report it unavailable.
// The check execs /bin/sh, not hostel itself: production only ever execs
// system binaries post-confine, and hostel's own dir isn't in the allowlist.
func landlockSmoke(self, workspaceRoot string) error {
	base, err := os.MkdirTemp(workspaceRoot, ".probe-*")
	if err != nil {
		return fmt.Errorf("smoke test: temp dir: %w", err)
	}
	defer os.RemoveAll(base)
	own := filepath.Join(base, "own")
	secret := filepath.Join(base, "sibling", "secret")
	if err := os.MkdirAll(own, 0o755); err != nil {
		return fmt.Errorf("smoke test: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(secret), 0o755); err != nil {
		return fmt.Errorf("smoke test: %w", err)
	}
	if err := os.WriteFile(secret, []byte("s"), 0o644); err != nil {
		return fmt.Errorf("smoke test: %w", err)
	}

	script := fmt.Sprintf("echo ok > probe.txt || exit 10; cat %q >/dev/null 2>&1 && exit 11; exit 0", secret)
	cmd := exec.Command(self, ConfineArg, own, "--", "/bin/sh", "-c", script)
	cmd.Dir = own
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case 10:
			return errors.New("smoke test: own data dir not writable under confinement")
		case 11:
			return errors.New("smoke test: sibling data still readable — restriction not enforced (BestEffort no-op, or workspace root inside a shared-RW path like /tmp)")
		}
	}
	return fmt.Errorf("smoke test: %w (%s)", err, out)
}

func (l *landlock) Name() string       { return "landlock" }
func (l *landlock) Level() Level       { return Room }
func (l *landlock) Available() bool    { return true } // only constructed when ABI≥1
func (l *landlock) MountPoint() string { return "" }   // no remount; real host paths

func (l *landlock) Wrap(cmd *exec.Cmd, ws Workspace) error {
	// Prefix `hostel __confine <bedDataDir> --` before the user command, so the
	// confiner child applies Landlock then execs it. cmd.Dir gives the shell
	// its starting cwd (real host path, since there's no /workspace remount).
	prefix := []string{l.self, ConfineArg, ws.Path, "--"}
	userArgs := cmd.Args
	cmd.Args = make([]string, 0, len(prefix)+len(userArgs))
	cmd.Args = append(cmd.Args, prefix...)
	cmd.Args = append(cmd.Args, userArgs...)
	cmd.Path = l.self
	cmd.Dir = ws.Path
	return nil
}

// ApplyConfine locks the CURRENT process to dataDir (rw) + the system paths a
// shell needs (ro), then it's safe to exec the user command. Best-effort: on
// an old kernel / missing landlock it degrades to a no-op (honest — the boot
// probe already decided room was achievable; if it wasn't, we wouldn't be
// here). Called by main's __confine subcommand.
func ApplyConfine(dataDir string) error {
	return applyLandlock(dataDir)
}

// landlockRODirs are the system paths a shell + interpreters need to read/exec;
// missing ones are skipped by BestEffort. Borrowed from greywall's set.
var landlockRODirs = []string{
	"/usr", "/lib", "/lib64", "/lib32", "/bin", "/sbin",
	"/etc", "/proc", "/sys", "/run", "/opt",
}

// landlockRWDirs: the bed's own data (private), plus shared host scratch —
// /tmp and /dev are the "shared toilet": writable but common to all beds.
func landlockRWDirs(dataDir string) []string {
	return []string{dataDir, "/tmp", "/dev", "/var/tmp"}
}

// applyLandlock restricts the current process to the bed data dir (rw) + system
// paths (ro). BestEffort degrades on older ABIs; missing paths are dropped so a
// distro without e.g. /lib32 doesn't fail the whole restriction.
func applyLandlock(dataDir string) error {
	existing := func(paths []string) []string {
		out := paths[:0:0]
		for _, p := range paths {
			if _, err := os.Stat(p); err == nil {
				out = append(out, p)
			}
		}
		return out
	}
	return ll.V9.BestEffort().RestrictPaths(
		ll.RODirs(existing(landlockRODirs)...),
		ll.RWDirs(existing(landlockRWDirs(dataDir))...),
	)
}
