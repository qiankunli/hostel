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
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Paths is the single place that converts between the three path spaces of one
// bed. Everything else (web handlers, exec plumbing, file ops) should go
// through it instead of stitching MountPoint()/Rel/Join by hand — the exec-cwd
// ENOENT bug came exactly from one call site doing its own stitching and
// picking the wrong space.
//
//	client:  what callers say. The client's "/" IS the bed's private root —
//	         the bed behaves as if it owned the whole pod filesystem. So
//	         /workspace/x, /tmp/x and a relative path (workspace-relative,
//	         OpenSandbox SDK contract) all name places inside one bed, and
//	         the mapping is injective: echoes reproduce the path as sent.
//	host:    where it really lives — {workspace-root}/{bed id}/data/x on the
//	         carrier host. The daemon's own file ops (fsops) work here.
//	in-bed:  what the bed's processes see — under suite ONLY the workspace
//	         subdir is mounted (at the canonical mount point); the rest of
//	         the private root has no in-bed name there. Without a mount view
//	         (direct/room) it is just the host path.
//
// Immutable value; safe to copy.
type Paths struct {
	root       string // bed private root host dir ({bed dir}/data) — the bed's "/"
	mountPoint string // in-sandbox canonical mount; "" = no mount view (direct/room)
}

// NewPaths builds the converter for one bed, anchored at the bed's private
// root ({bed dir}/data). mountPoint comes from the isolator
// (Isolator.MountPoint(), "" when the tier gives no mount view).
func NewPaths(root, mountPoint string) Paths {
	return Paths{root: root, mountPoint: mountPoint}
}

// Root is the bed private root host dir this converter is anchored at.
func (p Paths) Root() string { return p.root }

// WorkspaceHost is the host dir of the bed's workspace: the private-root
// subdir the client names VirtualPrefix. Derived, not stored — the client
// namespace IS the private root, so /workspace resolves by the general rule.
func (p Paths) WorkspaceHost() string {
	return filepath.Join(p.root, filepath.FromSlash(strings.TrimPrefix(VirtualPrefix, "/")))
}

// FromClient maps a client path to the host path. The client's "/" is the
// bed's private root, so every absolute path lands inside the bed by the same
// rule (/workspace/x included — no aliasing, echoes stay symmetric); relative
// paths are workspace-relative per the OpenSandbox SDK contract. Bed selection
// has already happened before this conversion, so isolation level must not
// change the mapping result.
func (p Paths) FromClient(cp string) (string, error) {
	if cp == "" {
		return "", fmt.Errorf("fsops: empty path")
	}
	if strings.HasPrefix(cp, "~") {
		return "", fmt.Errorf("fsops: %q: home-relative paths are not supported", cp)
	}
	rel := cp
	if !path.IsAbs(cp) {
		rel = path.Join(VirtualPrefix, cp) // workspace-relative
	}
	// Normalize under a fake root to neutralize any ".." segments.
	clean := path.Clean("/" + strings.TrimPrefix(rel, "/"))
	full := filepath.Join(p.root, filepath.FromSlash(clean))
	if r, err := filepath.Rel(p.root, full); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("fsops: path %q escapes the bed", cp)
	}
	return full, nil
}

// ToClient maps a host path under the private root back to its client form.
// Inverse of FromClient on absolute paths: a file uploaded as /tmp/x is
// reported as /tmp/x, one uploaded as /workspace/x as /workspace/x.
func (p Paths) ToClient(host string) string {
	rel, err := filepath.Rel(p.root, host)
	if err != nil || rel == "." {
		return "/"
	}
	return "/" + filepath.ToSlash(rel)
}

// InBed maps a host path to the path the bed's own processes must use (e.g. an
// exec cwd). Without a mount view (direct/room) that is the host path itself —
// the whole private root is reachable. Under suite only the workspace subdir
// is mounted (at mountPoint), so host paths elsewhere in the private root have
// NO in-bed name: refuse rather than guess.
func (p Paths) InBed(host string) (string, error) {
	if p.mountPoint == "" {
		return host, nil
	}
	rel, err := filepath.Rel(p.WorkspaceHost(), host)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("fsops: host path %q is not visible inside the sandbox (only the workspace is mounted at %s)", host, p.mountPoint)
	}
	return path.Join(p.mountPoint, filepath.ToSlash(rel)), nil
}
