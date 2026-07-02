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

// Package isolation confines a bed's processes. The interface is deliberately
// small ("wrap this exec.Cmd for this workspace") so the weak-tier isolators
// (direct, bwrap, and later a real setuid/seccomp/cgroup stack ported from
// OpenSandbox execd) are interchangeable behind it.
package isolation

import "os/exec"

// Workspace is the writable root a bed's commands are rooted at.
type Workspace struct {
	// Path is the bed's workspace directory on the host (also the cwd).
	Path string
}

// Isolator rewrites/prepares an exec.Cmd so it runs confined to a workspace.
type Isolator interface {
	// Name reports the isolator for /healthz + logs.
	Name() string
	// Available reports whether this isolator can run on the current host
	// (e.g. bwrap binary present, right caps). direct is always available.
	Available() bool
	// Wrap prepares cmd to run confined to ws. It may rewrite cmd.Path/Args
	// (bwrap) or just set cmd.Dir (direct). Called once per exec.
	Wrap(cmd *exec.Cmd, ws Workspace) error
}

// New returns the isolator for the given mode. Unknown modes fall back to
// direct so a misconfig degrades to "no isolation" rather than failing to boot.
func New(mode string) Isolator {
	switch mode {
	case "bwrap":
		return newBwrap()
	default:
		return direct{}
	}
}

// direct runs the command straight on the host, only pinning its cwd to the
// bed workspace. No isolation — for local dev and fully-trusted single-tenant
// use. It is always available (all platforms).
type direct struct{}

func (direct) Name() string      { return "direct" }
func (direct) Available() bool   { return true }
func (direct) Wrap(cmd *exec.Cmd, ws Workspace) error {
	cmd.Dir = ws.Path
	return nil
}
