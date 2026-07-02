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
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// packDir writes dir as a tar.gz stream to w. Entries are relative to dir;
// regular files, directories and symlinks are kept (sockets/devices skipped —
// they're runtime state, not workspace data).
func packDir(dir string, w io.Writer) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		var link string
		if info.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		} else if !info.Mode().IsRegular() && !info.IsDir() {
			return nil // skip sockets, fifos, devices
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// unpackDir extracts a tar.gz stream into dir, refusing entries that would
// escape it (zip-slip) — snapshots cross a trust boundary once they've been
// at rest in a bucket.
func unpackDir(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return fmt.Errorf("store: gzip: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("store: tar: %w", err)
		}
		target, err := confine(dir, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, fs.FileMode(hdr.Mode)&fs.ModePerm); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// The link target may be relative and escape-y; confine what it
			// resolves to, not just its text.
			resolved := hdr.Linkname
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(target), resolved)
			}
			if !strings.HasPrefix(filepath.Clean(resolved)+string(os.PathSeparator), filepath.Clean(dir)+string(os.PathSeparator)) {
				return fmt.Errorf("store: symlink %q escapes workspace", hdr.Name)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, fs.FileMode(hdr.Mode)&fs.ModePerm)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, tr) //nolint:gosec // size bounded by snapshot we produced
			f.Close()
			if err != nil {
				return err
			}
		default:
			// skip other types
		}
	}
}

// confine joins name under dir and rejects path escapes.
func confine(dir, name string) (string, error) {
	target := filepath.Join(dir, filepath.FromSlash(name))
	if !strings.HasPrefix(target+string(os.PathSeparator), filepath.Clean(dir)+string(os.PathSeparator)) &&
		target != filepath.Clean(dir) {
		return "", fmt.Errorf("store: entry %q escapes workspace", name)
	}
	return target, nil
}
