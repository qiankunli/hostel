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

package store

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func TestPackUnpackRoundTrip(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "sub/deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub/deep/b.bin"), []byte{0, 1, 2}, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := packDir(src, &buf, nil); err != nil {
		t.Fatalf("pack: %v", err)
	}

	dst := t.TempDir()
	if err := unpackDir(&buf, dst); err != nil {
		t.Fatalf("unpack: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dst, "a.txt"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("a.txt: %q err=%v", data, err)
	}
	fi, err := os.Stat(filepath.Join(dst, "sub/deep/b.bin"))
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Fatalf("b.bin mode: %v err=%v", fi.Mode(), err)
	}
	// Symlink survives and resolves inside the workspace.
	got, err := os.Readlink(filepath.Join(dst, "link"))
	if err != nil || got != "a.txt" {
		t.Fatalf("link: %q err=%v", got, err)
	}
}

// mkTar builds a raw tar.gz with fully controlled entries (attack payloads).
func mkTar(t *testing.T, entries []tar.Header) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i := range entries {
		if err := tw.WriteHeader(&entries[i]); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	gz.Close()
	return &buf
}

func TestUnpackRejectsEscapes(t *testing.T) {
	dst := t.TempDir()

	// Path traversal in the entry name.
	slip := mkTar(t, []tar.Header{{Name: "../evil.txt", Typeflag: tar.TypeReg, Mode: 0o644}})
	if err := unpackDir(slip, dst); err == nil {
		t.Fatal("path traversal not rejected")
	}

	// Symlink whose target escapes the workspace.
	link := mkTar(t, []tar.Header{{Name: "l", Typeflag: tar.TypeSymlink, Linkname: "../../etc/passwd"}})
	if err := unpackDir(link, dst); err == nil {
		t.Fatal("escaping symlink not rejected")
	}
}

func TestNoopStore(t *testing.T) {
	st, err := New(t.Context(), Config{Backend: "noop"})
	if err != nil || st.Name() != "noop" {
		t.Fatalf("New noop: %v %v", st, err)
	}
	info, err := st.Stat(t.Context(), "x")
	if err != nil || info != nil {
		t.Fatalf("noop Stat = %v %v", info, err)
	}
	if _, err := New(t.Context(), Config{Backend: "s3"}); err == nil {
		t.Fatal("s3 without bucket should fail")
	}
	if _, err := New(t.Context(), Config{Backend: "bogus"}); err == nil {
		t.Fatal("unknown backend should fail")
	}
}
