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

// missingUIDCaps must read /proc/self/status without error and, when caps are
// absent, name them — the honest-degrade signal the resolver relies on.
func TestMissingUIDCapsParses(t *testing.T) {
	miss := missingUIDCaps()
	t.Logf("missing uid caps here: %q", miss)
	// Under an unprivileged test runner at least one cap is absent; under root
	// none are. Both are valid — we only assert it didn't error out (an error
	// would surface as the error string, which contains no CAP_ token).
	if _, err := effectiveCaps(); err != nil {
		t.Fatalf("effectiveCaps: %v", err)
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
