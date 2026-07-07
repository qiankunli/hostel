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
//	client:  what callers say — the VirtualPrefix form (/workspace/x) or a
//	         workspace-relative path. OpenSandbox SDK contract.
//	host:    where it really lives — {workspace-root}/{bed id}/data/x on the
//	         carrier host. The daemon's own file ops (fsops) work here.
//	in-bed:  what the bed's processes see — under suite the workspace is
//	         mounted at the canonical mount point, so host paths don't exist
//	         in the sandbox's mount namespace; without a mount view
//	         (direct/room) it is just the host path.
//
// Immutable value; safe to copy.
type Paths struct {
	root       string // bed workspace host dir ({bed dir}/data)
	mountPoint string // in-sandbox canonical mount; "" = no mount view (direct/room)
}

// NewPaths builds the converter for one bed. mountPoint comes from the
// isolator (Isolator.MountPoint(), "" when the tier gives no mount view).
func NewPaths(root, mountPoint string) Paths {
	return Paths{root: root, mountPoint: mountPoint}
}

// Root is the bed workspace host dir this converter is anchored at.
func (p Paths) Root() string { return p.root }

// FromClient maps a client path to the host path, rejecting escapes: absolute
// paths must live under VirtualPrefix, relative paths are workspace-relative,
// ".." is neutralized. A bed can never name a host path outside its workspace.
func (p Paths) FromClient(cp string) (string, error) {
	if cp == "" {
		return "", fmt.Errorf("fsops: empty path")
	}
	if strings.HasPrefix(cp, "~") {
		return "", fmt.Errorf("fsops: %q: home-relative paths are not supported", cp)
	}
	rel := cp
	if path.IsAbs(cp) {
		switch {
		case cp == VirtualPrefix:
			rel = "."
		case strings.HasPrefix(cp, VirtualPrefix+"/"):
			rel = strings.TrimPrefix(cp, VirtualPrefix+"/")
		default:
			return "", fmt.Errorf("fsops: %q is outside the bed workspace (%s)", cp, VirtualPrefix)
		}
	}
	// Normalize under a fake root to neutralize any ".." segments.
	clean := path.Clean("/" + rel)
	full := filepath.Join(p.root, filepath.FromSlash(clean))
	if r, err := filepath.Rel(p.root, full); err != nil || r == ".." || strings.HasPrefix(r, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("fsops: path %q escapes workspace", cp)
	}
	return full, nil
}

// ToClient maps a host path under the workspace back to its VirtualPrefix form
// (how it is reported to callers).
func (p Paths) ToClient(host string) string {
	rel, err := filepath.Rel(p.root, host)
	if err != nil || rel == "." {
		return VirtualPrefix
	}
	return path.Join(VirtualPrefix, filepath.ToSlash(rel))
}

// InBed maps a host path under the workspace to the path the bed's own
// processes must use (e.g. an exec cwd): the mount-point form under suite, the
// host path itself when there is no mount view. Rejects paths outside the
// workspace rather than guessing.
func (p Paths) InBed(host string) (string, error) {
	if p.mountPoint == "" {
		return host, nil
	}
	rel, err := filepath.Rel(p.root, host)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("fsops: host path %q is outside the bed workspace", host)
	}
	return path.Join(p.mountPoint, filepath.ToSlash(rel)), nil
}
