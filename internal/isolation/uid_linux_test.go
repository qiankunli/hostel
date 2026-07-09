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
	"path/filepath"
	"syscall"
	"testing"
)

// bedUID must be deterministic (Prepare and Wrap derive it independently and
// must agree) and land in the reserved band.
func TestBedUIDDeterministicAndRanged(t *testing.T) {
	const p = "/ws/alice/data"
	if bedUID(p) != bedUID(p) {
		t.Fatal("bedUID not deterministic for the same path")
	}
	u := bedUID(p)
	if u < uidBase || u >= uidBase+uidRange {
		t.Fatalf("bedUID %d outside band [%d,%d)", u, uidBase, uidBase+uidRange)
	}
	if bedUID("/ws/alice/data") == bedUID("/ws/bob/data") {
		// Not a failure — collisions are allowed and documented — but flag it so
		// a hash change that suddenly collides common names is visible.
		t.Log("note: two sample bed paths collided to the same uid")
	}
}

// missingUIDCaps reads the shared HostFacts and names the absent caps — the
// honest-degrade signal the resolver relies on. Cross-check it against the live
// facts so the cap-bit wiring can't silently drift.
func TestMissingUIDCaps(t *testing.T) {
	facts := collectHostFacts()
	miss := missingUIDCaps(facts)
	t.Logf("effective caps %#x, missing uid caps: %q", facts.EffectiveCaps, miss)
	wantAllPresent := facts.HasCap(capCHOWN) && facts.HasCap(capSETGID) && facts.HasCap(capSETUID)
	if wantAllPresent != (miss == "") {
		t.Fatalf("missingUIDCaps=%q but HasCap(all)=%v — cap-bit wiring drifted", miss, wantAllPresent)
	}
}

// collectHostFacts must probe without panicking and yield self-consistent facts
// on any Linux host (root or not).
func TestCollectHostFactsSane(t *testing.T) {
	f := collectHostFacts()
	t.Logf("host facts: %+v", f)
	if f.KernelRelease == "" {
		t.Error("KernelRelease empty on Linux (uname should populate it)")
	}
	if f.EUID != os.Geteuid() {
		t.Errorf("EUID = %d, want %d", f.EUID, os.Geteuid())
	}
	if f.EGID != os.Getegid() {
		t.Errorf("EGID = %d, want %d", f.EGID, os.Getegid())
	}
	if f.LandlockABI < 0 {
		t.Errorf("LandlockABI = %d, must be >= 0", f.LandlockABI)
	}
}

// chownTree must NOT rehome a multiply-linked regular file: a hardlink into the
// bed dir pointing at a host file would otherwise let Prepare hand the bed
// ownership of that inode. Chowning to a foreign uid needs root, so gate on it.
func TestChownTreeSkipsHardlinks(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to chown to a foreign uid and observe the skip")
	}
	root := t.TempDir()
	// An "outside" file the bed shouldn't be able to capture, plus a hardlink to
	// it inside the tree, plus a normal file that SHOULD be chowned.
	outside := filepath.Join(t.TempDir(), "host-owned")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "captured")
	if err := os.Link(outside, link); err != nil {
		t.Fatalf("hardlink (same fs needed): %v", err)
	}
	normal := filepath.Join(root, "own")
	if err := os.WriteFile(normal, []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	const uid = uidBase + 7
	if err := chownTree(root, uid); err != nil {
		t.Fatalf("chownTree: %v", err)
	}
	ownerOf := func(p string) int {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return int(fi.Sys().(*syscall.Stat_t).Uid)
	}
	if got := ownerOf(outside); got == uid {
		t.Fatalf("hardlinked host file was rehomed to bed uid %d — escalation not prevented", uid)
	}
	if got := ownerOf(normal); got != uid {
		t.Fatalf("normal file owner = %d, want bed uid %d", got, uid)
	}
}

// prepareUIDDir sets 0700 and chowns the tree. chown needs CAP_CHOWN, so gate
// the ownership assertion on being able to actually chown; the mode change is
// checkable unprivileged.
func TestPrepareUIDDir(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(sub, "f")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	const uid = uidBase + 42
	err := prepareUIDDir(dir, uid)
	if os.Geteuid() != 0 {
		// Non-root: Lchown to a foreign uid is refused; prepareUIDDir surfaces
		// it. We can still confirm the dir mode was tightened first.
		fi, serr := os.Stat(dir)
		if serr != nil {
			t.Fatal(serr)
		}
		if fi.Mode().Perm() != 0o700 {
			t.Fatalf("dir mode = %o, want 0700", fi.Mode().Perm())
		}
		t.Skipf("unprivileged: chown refused as expected (%v)", err)
	}
	if err != nil {
		t.Fatalf("prepareUIDDir: %v", err)
	}
	for _, p := range []string{dir, sub, f} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		st := fi.Sys().(*syscall.Stat_t)
		if int(st.Uid) != uid || int(st.Gid) != uid {
			t.Fatalf("%s owned %d:%d, want %d:%d", p, st.Uid, st.Gid, uid, uid)
		}
	}
}
