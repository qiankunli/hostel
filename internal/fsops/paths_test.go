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

package fsops

import (
	"path/filepath"
	"testing"
)

// TestPathsFromClientAllLevels pins path rebasing as a bed-level contract:
// room type changes the access barrier, never where a client path is stored.
func TestPathsFromClientAllLevels(t *testing.T) {
	root := t.TempDir()
	levels := []struct {
		name       string
		mountPoint string
	}{
		{name: "dorm"},
		{name: "room"},
		{name: "suite", mountPoint: "/workspace"},
	}
	cases := []struct {
		client string
		rel    string
	}{
		{client: "/workspace", rel: "workspace"},
		{client: "/workspace/a.txt", rel: "workspace/a.txt"},
		{client: "/tmp/workspace/job", rel: "tmp/workspace/job"},
		{client: "tmp/workspace/job", rel: "workspace/tmp/workspace/job"}, // relative = workspace-relative
		{client: "/"},
	}

	for _, level := range levels {
		t.Run(level.name, func(t *testing.T) {
			paths := NewPaths(root, level.mountPoint)
			for _, tc := range cases {
				got, err := paths.FromClient(tc.client)
				if err != nil {
					t.Fatalf("FromClient(%q): %v", tc.client, err)
				}
				want := filepath.Join(root, filepath.FromSlash(tc.rel))
				if got != want {
					t.Errorf("FromClient(%q) = %q, want %q", tc.client, got, want)
				}
			}
		})
	}
}

// TestPathsInBed covers the third path space: what the bed's own processes see.
// With a mount view (suite) ONLY the workspace subdir has an in-bed name (the
// mount point); other private-root paths are refused, never guessed. Without a
// mount view (direct/room) the host path is used as-is.
func TestPathsInBed(t *testing.T) {
	root := t.TempDir()

	t.Run("mounted", func(t *testing.T) {
		p := NewPaths(root, "/workspace")
		ws := filepath.Join(root, "workspace")
		cases := []struct{ host, want string }{
			{ws, "/workspace"},
			{filepath.Join(ws, "sub"), "/workspace/sub"},
			{filepath.Join(ws, "a", "b"), "/workspace/a/b"},
		}
		for _, tc := range cases {
			got, err := p.InBed(tc.host)
			if err != nil || got != tc.want {
				t.Errorf("InBed(%q) = %q,%v want %q", tc.host, got, err, tc.want)
			}
		}
		// bed_home outside the workspace has no in-bed name under suite.
		for _, host := range []string{root, filepath.Join(root, "tmp", "x"), filepath.Dir(root)} {
			if _, err := p.InBed(host); err == nil {
				t.Errorf("InBed(%q) must refuse (not mounted), got nil error", host)
			}
		}
	})

	t.Run("no mount view", func(t *testing.T) {
		p := NewPaths(root, "")
		host := filepath.Join(root, "tmp", "x") // whole bed_home reachable
		got, err := p.InBed(host)
		if err != nil || got != host {
			t.Errorf("InBed(%q) = %q,%v want the host path back", host, got, err)
		}
	})
}

// TestPathsRoundTrip pins FromClient/ToClient as inverses on absolute client
// paths — the "own pod" contract: a path is echoed back exactly as sent, for
// workspace and non-workspace locations alike.
func TestPathsRoundTrip(t *testing.T) {
	root := t.TempDir()
	p := NewPaths(root, "/workspace")
	for _, cp := range []string{"/", "/workspace", "/workspace/a.txt", "/workspace/x/y", "/tmp/workspace/job", "/etc/hosts"} {
		host, err := p.FromClient(cp)
		if err != nil {
			t.Fatalf("FromClient(%q): %v", cp, err)
		}
		if back := p.ToClient(host); back != cp {
			t.Errorf("ToClient(FromClient(%q)) = %q", cp, back)
		}
	}
}
