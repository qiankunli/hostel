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
	"slices"
	"testing"
)

// TestResolveMaskPathsDedupsSymlink reproduces the k8s /var/run -> /run layout:
// /run/secrets and /var/run/secrets are the same dir, and masking both breaks
// bwrap. resolveMaskPaths must keep only the first (canonical) one.
func TestResolveMaskPathsDedupsSymlink(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, "run")
	if err := os.MkdirAll(filepath.Join(run, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	// /var/run -> /run
	varDir := filepath.Join(root, "var")
	if err := os.MkdirAll(varDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(run, filepath.Join(varDir, "run")); err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}

	runSecrets := filepath.Join(run, "secrets")
	varRunSecrets := filepath.Join(varDir, "run", "secrets") // symlink path to the same dir
	missing := filepath.Join(root, "nope")

	got := resolveMaskPaths([]string{home, runSecrets, varRunSecrets, missing})

	// home + one of the secrets paths; the symlink duplicate and the missing
	// path are dropped. Order preserved, canonical (first-seen) secrets kept.
	want := []string{home, runSecrets}
	if !slices.Equal(got, want) {
		t.Fatalf("resolveMaskPaths = %v, want %v", got, want)
	}
}
