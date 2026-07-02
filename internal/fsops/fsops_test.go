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

func TestResolveConfinement(t *testing.T) {
	root := t.TempDir()
	o := New(root)

	cases := []struct {
		in      string
		wantErr bool
		wantAbs string // expected host path (when no error)
	}{
		{"a.txt", false, filepath.Join(root, "a.txt")},
		{"/workspace/a.txt", false, filepath.Join(root, "a.txt")},
		{"/workspace", false, root},
		{"/workspace/sub/b", false, filepath.Join(root, "sub", "b")},
		{"/workspace/../../etc/passwd", false, filepath.Join(root, "etc", "passwd")}, // .. neutralized under /workspace
		{"../escape", false, filepath.Join(root, "escape")},                          // relative .. clamped to root, cannot escape
		{"/etc/passwd", true, ""},                                                    // absolute outside the virtual prefix
		{"~/secrets", true, ""},
		{"", true, ""},
	}
	for _, tc := range cases {
		got, err := o.Resolve(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("Resolve(%q): want error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("Resolve(%q): unexpected error %v", tc.in, err)
			continue
		}
		if got != tc.wantAbs {
			t.Errorf("Resolve(%q) = %q, want %q", tc.in, got, tc.wantAbs)
		}
	}
}

func TestWriteReadListReplace(t *testing.T) {
	o := New(t.TempDir())

	if err := o.Write("/workspace/dir/hello.txt", []byte("localhost:8080\nkeep\n"), 0); err != nil {
		t.Fatalf("Write: %v", err)
	}
	data, err := o.Read("/workspace/dir/hello.txt")
	if err != nil || string(data) != "localhost:8080\nkeep\n" {
		t.Fatalf("Read: %q err=%v", data, err)
	}

	// Stat reports the virtual path back.
	fi, err := o.Stat("dir/hello.txt")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Path != "/workspace/dir/hello.txt" || fi.Type != "file" {
		t.Fatalf("Stat path=%q type=%q", fi.Path, fi.Type)
	}

	// List depth 1 shows the dir; depth 2 shows the file.
	shallow, err := o.List("/workspace", 1)
	if err != nil || len(shallow) != 1 || shallow[0].Type != "directory" {
		t.Fatalf("List depth1: %+v err=%v", shallow, err)
	}
	deep, err := o.List("/workspace", 2)
	if err != nil || len(deep) != 2 {
		t.Fatalf("List depth2: want 2 entries, got %+v err=%v", deep, err)
	}

	// Replace counts + rewrites.
	res, err := o.Replace("dir/hello.txt", ReplaceItem{Old: "localhost:8080", New: "0.0.0.0:9090"})
	if err != nil || res.ReplacedCount != 1 {
		t.Fatalf("Replace: %+v err=%v", res, err)
	}
	data, _ = o.Read("dir/hello.txt")
	if string(data) != "0.0.0.0:9090\nkeep\n" {
		t.Fatalf("after Replace: %q", data)
	}
}

func TestReadLines(t *testing.T) {
	o := New(t.TempDir())
	_ = o.Write("f", []byte("l0\nl1\nl2\nl3\n"), 0)
	got, err := o.ReadLines("f", 1, 2)
	if err != nil || got != "l1\nl2\n" {
		t.Fatalf("ReadLines(1,2) = %q err=%v", got, err)
	}
	got, _ = o.ReadLines("f", 3, 0) // limit 0 = to end
	if got != "l3\n" {
		t.Fatalf("ReadLines(3,0) = %q", got)
	}
}

func TestSearch(t *testing.T) {
	o := New(t.TempDir())
	_ = o.Write("a/x.go", nil, 0)
	_ = o.Write("a/y.txt", nil, 0)
	_ = o.Write("b/z.go", nil, 0)
	hits, err := o.Search("/workspace", "*.go")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("Search *.go: want 2, got %d (%+v)", len(hits), hits)
	}
}
