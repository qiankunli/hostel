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
	"os"
	"os/exec"

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

func newLandlock(string) Isolator {
	// Probe: Landlock ABI ≥ 1 means filesystem restrictions are available.
	if v, err := lls.LandlockGetABIVersion(); err != nil || v < 1 {
		return unavailable{name: "landlock", lvl: Room}
	}
	self, err := os.Executable()
	if err != nil {
		return unavailable{name: "landlock", lvl: Room}
	}
	return &landlock{self: self}
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
