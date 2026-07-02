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

// Package fsops implements bed-scoped filesystem operations. Every path is
// resolved and confined under the bed's workspace root, so one bed's file API
// can never touch another bed's data even though the daemon runs unconfined.
//
// Path contract (OpenSandbox SDK compatible): clients address files under the
// virtual prefix "/workspace" (e.g. "/workspace/a.txt"); hostel rebases that
// onto the bed's host workspace dir. Relative paths are workspace-relative.
// Absolute paths outside the prefix are rejected — a bed never sees the host.
package fsops

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// VirtualPrefix is where a bed's workspace appears to clients. Kept as the
// OpenSandbox convention so SDK calls (`/workspace/...`) work unchanged.
const VirtualPrefix = "/workspace"

// FileInfo mirrors the OpenSandbox execd file metadata shape so existing SDKs
// deserialize hostel responses unchanged. Paths are reported back under the
// virtual prefix.
type FileInfo struct {
	Path       string    `json:"path,omitempty"`
	Type       string    `json:"type,omitempty"` // "file" | "directory" | "symlink"
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at,omitzero"`
	Owner      string    `json:"owner"`
	Group      string    `json:"group"`
	Mode       int       `json:"mode"`
}

// ReplaceItem / ReplaceResult mirror execd's /files/replace shapes.
type ReplaceItem struct {
	Old string `json:"old,omitempty"`
	New string `json:"new,omitempty"`
}
type ReplaceResult struct {
	ReplacedCount int `json:"replacedCount"`
}

// RenameItem mirrors execd's /files/mv item shape.
type RenameItem struct {
	Src  string `json:"src,omitempty"`
	Dest string `json:"dest,omitempty"`
}

// Permission mirrors execd's permission shape (owner/group accepted but not
// applied in v1 — hostel runs beds under one uid; only mode is applied).
type Permission struct {
	Owner string `json:"owner"`
	Group string `json:"group"`
	Mode  int    `json:"mode"`
}

// Ops is rooted at one bed's workspace.
type Ops struct{ root string }

// New returns file ops confined to root (the bed workspace host dir).
func New(root string) *Ops { return &Ops{root: root} }

// Resolve maps a client path to a host path under the workspace, rejecting
// escapes. Exported for exec cwd resolution.
func (o *Ops) Resolve(p string) (string, error) {
	if p == "" {
		return "", fmt.Errorf("fsops: empty path")
	}
	if strings.HasPrefix(p, "~") {
		return "", fmt.Errorf("fsops: %q: home-relative paths are not supported", p)
	}
	rel := p
	if path.IsAbs(p) {
		switch {
		case p == VirtualPrefix:
			rel = "."
		case strings.HasPrefix(p, VirtualPrefix+"/"):
			rel = strings.TrimPrefix(p, VirtualPrefix+"/")
		default:
			return "", fmt.Errorf("fsops: %q is outside the bed workspace (%s)", p, VirtualPrefix)
		}
	}
	// Normalize under a fake root to neutralize any ".." segments.
	clean := path.Clean("/" + rel)
	full := filepath.Join(o.root, filepath.FromSlash(clean))
	if r, err := filepath.Rel(o.root, full); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("fsops: path %q escapes workspace", p)
	}
	return full, nil
}

func (o *Ops) virtual(full string) string {
	rel, err := filepath.Rel(o.root, full)
	if err != nil || rel == "." {
		return VirtualPrefix
	}
	return path.Join(VirtualPrefix, filepath.ToSlash(rel))
}

func (o *Ops) info(full string, li os.FileInfo) FileInfo {
	typ := "file"
	switch {
	case li.IsDir():
		typ = "directory"
	case li.Mode()&os.ModeSymlink != 0:
		typ = "symlink"
	}
	return FileInfo{
		Path:       o.virtual(full),
		Type:       typ,
		Size:       li.Size(),
		ModifiedAt: li.ModTime(),
		Mode:       int(li.Mode().Perm()),
	}
}

// Stat returns metadata for one path.
func (o *Ops) Stat(p string) (FileInfo, error) {
	full, err := o.Resolve(p)
	if err != nil {
		return FileInfo{}, err
	}
	li, err := os.Lstat(full)
	if err != nil {
		return FileInfo{}, err
	}
	return o.info(full, li), nil
}

// Read returns full file contents.
func (o *Ops) Read(p string) ([]byte, error) {
	full, err := o.Resolve(p)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(full)
}

// ReadLines returns up to limit lines starting at 0-based line offset.
func (o *Ops) ReadLines(p string, offset, limit int) (string, error) {
	data, err := o.Read(p)
	if err != nil {
		return "", err
	}
	lines := strings.SplitAfter(string(data), "\n")
	// SplitAfter leaves a trailing "" when the file ends with \n.
	if n := len(lines); n > 0 && lines[n-1] == "" {
		lines = lines[:n-1]
	}
	if offset >= len(lines) {
		return "", nil
	}
	end := len(lines)
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return strings.Join(lines[offset:end], ""), nil
}

// Write creates/overwrites a file (0644 when mode==0), making parent dirs.
func (o *Ops) Write(p string, data []byte, mode int) error {
	full, err := o.Resolve(p)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	fm := os.FileMode(0o644)
	if mode != 0 {
		fm = os.FileMode(mode) & os.ModePerm
	}
	return os.WriteFile(full, data, fm)
}

// Remove deletes files (not directories); missing files are ignored.
func (o *Ops) Remove(paths []string) error {
	for _, p := range paths {
		full, err := o.Resolve(p)
		if err != nil {
			return err
		}
		li, err := os.Lstat(full)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return err
		}
		if li.IsDir() {
			return fmt.Errorf("fsops: %q is a directory (use the directories API)", p)
		}
		if err := os.Remove(full); err != nil {
			return err
		}
	}
	return nil
}

// Rename moves src to dest (creating dest parents).
func (o *Ops) Rename(src, dest string) error {
	s, err := o.Resolve(src)
	if err != nil {
		return err
	}
	d, err := o.Resolve(dest)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(d), 0o755); err != nil {
		return err
	}
	return os.Rename(s, d)
}

// Chmod applies mode bits. Owner/group are accepted for spec compatibility but
// not applied in v1 (single-uid beds); real setuid lands with the OSEP-0013
// isolation port.
func (o *Ops) Chmod(p string, perm Permission) error {
	full, err := o.Resolve(p)
	if err != nil {
		return err
	}
	return os.Chmod(full, os.FileMode(perm.Mode)&os.ModePerm)
}

// Replace substitutes all occurrences of old with new in one file.
func (o *Ops) Replace(p string, item ReplaceItem) (ReplaceResult, error) {
	full, err := o.Resolve(p)
	if err != nil {
		return ReplaceResult{}, err
	}
	data, err := os.ReadFile(full)
	if err != nil {
		return ReplaceResult{}, err
	}
	if item.Old == "" {
		return ReplaceResult{}, fmt.Errorf("fsops: empty 'old' for %q", p)
	}
	count := strings.Count(string(data), item.Old)
	if count == 0 {
		return ReplaceResult{ReplacedCount: 0}, nil
	}
	li, _ := os.Lstat(full)
	mode := os.FileMode(0o644)
	if li != nil {
		mode = li.Mode().Perm()
	}
	out := strings.ReplaceAll(string(data), item.Old, item.New)
	if err := os.WriteFile(full, []byte(out), mode); err != nil {
		return ReplaceResult{}, err
	}
	return ReplaceResult{ReplacedCount: count}, nil
}

// MakeDir creates a directory (and parents).
func (o *Ops) MakeDir(p string) error {
	full, err := o.Resolve(p)
	if err != nil {
		return err
	}
	return os.MkdirAll(full, 0o755)
}

// RemoveDir removes a directory tree.
func (o *Ops) RemoveDir(p string) error {
	full, err := o.Resolve(p)
	if err != nil {
		return err
	}
	li, err := os.Lstat(full)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !li.IsDir() {
		return fmt.Errorf("fsops: %q is not a directory", p)
	}
	return os.RemoveAll(full)
}

// List returns entries of a directory down to depth levels (1 = immediate).
func (o *Ops) List(p string, depth int) ([]FileInfo, error) {
	full, err := o.Resolve(p)
	if err != nil {
		return nil, err
	}
	li, err := os.Lstat(full)
	if err != nil {
		return nil, err
	}
	if li.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("fsops: %q is a symlink, refusing to traverse", p)
	}
	if !li.IsDir() {
		return nil, fmt.Errorf("fsops: %q is not a directory", p)
	}
	if depth < 1 {
		depth = 1
	}
	var out []FileInfo
	var walk func(dir string, d int) error
	walk = func(dir string, d int) error {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range entries {
			fi, err := e.Info()
			if err != nil {
				continue
			}
			out = append(out, o.info(filepath.Join(dir, e.Name()), fi))
			if e.IsDir() && d > 1 {
				if err := walk(filepath.Join(dir, e.Name()), d-1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(full, depth); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// searchLimit bounds Search results so a huge workspace can't OOM the daemon.
const searchLimit = 1000

// Search walks under p and returns files whose base name matches pattern
// (glob when pattern contains meta characters, substring otherwise).
func (o *Ops) Search(p, pattern string) ([]FileInfo, error) {
	full, err := o.Resolve(p)
	if err != nil {
		return nil, err
	}
	match := func(name string) bool { return true }
	if pattern != "" {
		if strings.ContainsAny(pattern, "*?[") {
			match = func(name string) bool {
				ok, _ := path.Match(pattern, name)
				return ok
			}
		} else {
			match = func(name string) bool { return strings.Contains(name, pattern) }
		}
	}
	var out []FileInfo
	err = filepath.WalkDir(full, func(fp string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if len(out) >= searchLimit {
			return filepath.SkipAll
		}
		if d.IsDir() || !match(d.Name()) {
			return nil
		}
		fi, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, o.info(fp, fi))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
