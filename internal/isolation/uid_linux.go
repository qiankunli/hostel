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
	"hash/fnv"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// AsUserArg is the hidden subcommand hostel re-execs into to drop to the bed's
// dedicated uid before exec-ing the real command (main handles it). Like
// landlock's __confine, the drop must happen in the CALLING process — the
// daemon manages every bed and must keep its privileges — so the mechanism is a
// self-re-exec: `hostel __asuser <uid> <bedDataDir> -- <cmd>...`.
const AsUserArg = "__asuser"

// Bed uids live in a fixed high band, ASSUMED unused by the host — not
// guaranteed: this range can overlap /etc/subuid userns mappings (the 2nd
// default user gets 231072..) or LDAP/service accounts. Under our threat model
// (a bed straying into another bed, not adversarial uid-squatting) that's
// acceptable; a bed colliding with a real host identity is a deployment
// concern, documented in docs/data-isolation.md. The uid is derived from the
// data dir path (no registry), so Prepare (chown) and Wrap (setuid) agree with
// no shared state and it stays stable across restarts. Two beds hashing to the
// same uid is possible but rare; a colliding pair degrades to mutual access
// (dorm between just those two) — never a crash.
const (
	uidBase  = 200000
	uidRange = 100000 // uids 200000..299999
)

// bedUID derives a bed's dedicated uid from its data dir. gid == uid: each bed
// gets a private primary group of the same number.
func bedUID(dataDir string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(dataDir))
	return uidBase + int(h.Sum32()%uidRange)
}

// uidIso is the room mechanism realized with classic Unix DAC: each bed's
// processes run as a dedicated uid, its data dir owned 0700 by that uid. A bed
// can't ACCESS another's data (EACCES) and — a bonus over landlock — can't
// signal, ptrace, or read /proc/<pid> of another bed's processes either.
// Siblings stay visible (dir names listable, /tmp and system paths shared): the
// "private room, shared toilet" tier. Needs the daemon to hold
// CAP_SETUID/SETGID/CHOWN (root, or a setcap'd binary) but no special kernel —
// so it fills the room slot where Landlock is absent (old/custom kernels).
type uidIso struct {
	self string // hostel binary, re-execed as the uid-dropper
}

func newUID(facts HostFacts, workspaceRoot string) Isolator {
	self, err := os.Executable()
	if err != nil {
		return unavailable{name: "uid", lvl: Room}
	}
	// Missing caps isn't an error — many environments simply don't grant them;
	// the resolver falls through to the next mechanism and logs honestly.
	if miss := missingUIDCaps(facts); miss != "" {
		return unavailable{name: "uid", lvl: Room}
	}
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		log.Printf("isolation: cannot create workspace root %s: %v", workspaceRoot, err)
	}
	// Caps present ≠ enforcement works. Prove the whole chain once — chown →
	// setuid → no_new_privs → EACCES on a sibling — exactly as production runs.
	if err := uidSmoke(self, workspaceRoot); err != nil {
		log.Printf("isolation: uid isolation caps present but unusable (%v)", err)
		return unavailable{name: "uid", lvl: Room}
	}
	return &uidIso{self: self}
}

// capsForUID: the effective capabilities the daemon needs to run beds under
// dedicated uids — SETUID/SETGID to drop, CHOWN to hand the data dir over.
var capsForUID = []struct {
	bit  uint
	name string
}{
	{capCHOWN, "CAP_CHOWN"}, {capSETGID, "CAP_SETGID"}, {capSETUID, "CAP_SETUID"},
}

// missingUIDCaps returns a comma-joined list of the required caps absent from
// the host's effective set ("" = all present), read from the shared HostFacts.
func missingUIDCaps(facts HostFacts) string {
	var miss []string
	for _, c := range capsForUID {
		if !facts.HasCap(c.bit) {
			miss = append(miss, c.name)
		}
	}
	return strings.Join(miss, ",")
}

// uidSmoke proves the mechanism bites, using the exact production form: prepare
// two sibling dirs owned by DIFFERENT bed uids, then run `hostel __asuser` as
// one and check it can write its own dir but gets EACCES on the sibling's
// secret. Catches a silently-broken setuid (e.g. no CAP_SETUID) that the cap
// bits alone wouldn't — same honesty contract as landlockSmoke.
func uidSmoke(self, workspaceRoot string) error {
	base, err := os.MkdirTemp(workspaceRoot, ".uidprobe-*")
	if err != nil {
		return fmt.Errorf("smoke test: temp dir: %w", err)
	}
	defer os.RemoveAll(base)
	// The probe process (a bed uid) must be able to TRAVERSE base to reach the
	// two dirs under it — MkdirTemp makes it 0700, which would block a non-root
	// uid at the door.
	if err := os.Chmod(base, 0o755); err != nil {
		return fmt.Errorf("smoke test: %w", err)
	}
	own := filepath.Join(base, "own")
	sibling := filepath.Join(base, "sibling")
	secret := filepath.Join(sibling, "secret")
	if err := os.MkdirAll(own, 0o755); err != nil {
		return fmt.Errorf("smoke test: %w", err)
	}
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		return fmt.Errorf("smoke test: %w", err)
	}
	if err := os.WriteFile(secret, []byte("s"), 0o600); err != nil {
		return fmt.Errorf("smoke test: %w", err)
	}
	if err := prepareUIDDir(own, bedUID(own)); err != nil {
		return fmt.Errorf("smoke test: prepare own: %w", err)
	}
	if err := prepareUIDDir(sibling, bedUID(sibling)); err != nil {
		return fmt.Errorf("smoke test: prepare sibling: %w", err)
	}

	script := fmt.Sprintf("echo ok > probe.txt || exit 10; cat %q >/dev/null 2>&1 && exit 11; exit 0", secret)
	cmd := exec.Command(self, AsUserArg, strconv.Itoa(bedUID(own)), own, "--", "/bin/sh", "-c", script)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch ee.ExitCode() {
		case 10:
			return errors.New("smoke test: own data dir not writable under the bed uid")
		case 11:
			return errors.New("smoke test: sibling data still readable — uid isolation not enforced")
		}
	}
	return fmt.Errorf("smoke test: %w (%s)", err, out)
}

func (u *uidIso) Name() string       { return "uid" }
func (u *uidIso) Level() Level       { return Room }
func (u *uidIso) Available() bool    { return true } // only constructed when the smoke passed
func (u *uidIso) MountPoint() string { return "" }   // no remount; real host paths

func (u *uidIso) Wrap(cmd *exec.Cmd, ws Workspace) error {
	// Prefix `hostel __asuser <uid> <bedDataDir> --` so the child drops to the
	// bed uid, then execs the user command. cmd.Dir gives the parent its start
	// dir; the child re-chdirs there anyway after dropping.
	uid := bedUID(ws.Path)
	prefix := []string{u.self, AsUserArg, strconv.Itoa(uid), ws.Path, "--"}
	userArgs := cmd.Args
	cmd.Args = make([]string, 0, len(prefix)+len(userArgs))
	cmd.Args = append(cmd.Args, prefix...)
	cmd.Args = append(cmd.Args, userArgs...)
	cmd.Path = u.self
	cmd.Dir = ws.Path
	return nil
}

// Prepare hands the bed's data dir to its dedicated uid: 0700 on the dir so
// siblings can't enter, owned recursively by the uid so the bed can read and
// write its own files. Implements Preparer; the bed manager calls it after the
// data dir is (re)created — including after a restore repopulated the tree.
func (u *uidIso) Prepare(ws Workspace) error {
	return prepareUIDDir(ws.Path, bedUID(ws.Path))
}

func prepareUIDDir(dir string, uid int) error {
	if err := os.Chmod(dir, 0o700); err != nil {
		return err
	}
	return chownTree(dir, uid)
}

// chownTree recursively chowns root to uid:uid (uid == gid). Lchown so symlinks
// are retargeted, not their referents; WalkDir doesn't descend symlinked dirs,
// so a symlink can't lead the walk out of the tree.
//
// Hardlinks are the subtle case: a hardlink is a second name for the SAME
// inode, so chowning it by path also rehomes whatever else points at that inode
// — a bed could `ln /etc/x data/x` and, on the next Prepare, be handed
// ownership of a host file (privilege escalation when fs.protected_hardlinks is
// off — precisely the old/custom-kernel hosts uid isolation targets). So we
// skip any multiply-linked regular file: it keeps its original owner (root), so
// the bed still can't write it. Deployments should also keep
// fs.protected_hardlinks=1 (docs/data-isolation.md). Directories legitimately
// have nlink>1 (subdirs, "."), so the guard is regular-files-only.
func chownTree(root string, uid int) error {
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, ierr := d.Info()
			if ierr != nil {
				return ierr
			}
			if st, ok := info.Sys().(*syscall.Stat_t); ok && st.Nlink > 1 {
				return nil // multiply-linked: may point outside the tree, don't rehome it
			}
		}
		return os.Lchown(p, uid, uid)
	})
}

// ApplyAsUser drops the CURRENT process to uid (uid == gid, private group),
// sets no_new_privs so a setuid-root binary in the image can't re-escalate,
// then enters the bed data dir — after which it's safe to exec the bed command.
// Called by main's __asuser subcommand. Order matters: each privileged step
// must run before we drop the capability that permits it (groups and gid before
// uid), and no_new_privs before the exec it must outlive.
func ApplyAsUser(uid int, dataDir string) error {
	if err := syscall.Setgroups([]int{}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(uid); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, uintptr(1), 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	if err := syscall.Setuid(uid); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	if err := os.Chdir(dataDir); err != nil {
		return fmt.Errorf("chdir: %w", err)
	}
	return nil
}
