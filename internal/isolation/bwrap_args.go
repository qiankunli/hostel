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

package isolation

// This file has no build tag: the argv builder is pure string assembly so its
// tests run on every platform (the exec-ing side lives in bwrap_linux.go).

import "strings"

// BwrapMountPoint is where a bed's workspace is bind-mounted inside the
// sandbox. Fixed and canonical: it makes shell paths and file-API paths the
// same string, matching OpenSandbox SDK expectations.
const BwrapMountPoint = "/workspace"

// buildBwrapArgs assembles the bwrap argv (between the binary and the user
// command). Segment order is a contract — bwrap applies mounts in argv order
// and later mounts cover earlier ones (the masking below depends on it):
//
//  1. Namespace flags (mount ns is implicit; net stays shared in v1)
//  2. --ro-bind / /            — RO host root: toolchains stay usable
//  3. --dev /dev, --proc /proc, --tmpfs /tmp — fresh device/proc/tmp
//  4. Masking: --tmpfs over workspaceRoot (sibling beds cease to exist),
//     and over each maskPath (host user data / mounted secrets)
//  5. --bind <bed workspace> /workspace — own data only, canonical name
//     (must come AFTER the workspaceRoot mask so it re-opens only our dir)
//  6. --unsetenv for secret-looking env vars (host credentials must not
//     leak into bed code; list borrowed from execd's strict profile)
//  7. --chdir /workspace, --die-with-parent, --
//
// environ is os.Environ()-shaped; maskPaths are host paths that exist.
func buildBwrapArgs(workspaceRoot, wsPath string, maskPaths, environ []string) []string {
	argv := []string{
		// 1.
		"--unshare-pid", "--unshare-uts", "--unshare-ipc",
		// 2.
		"--ro-bind", "/", "/",
		// 3.
		"--dev", "/dev",
		"--proc", "/proc",
		"--tmpfs", "/tmp",
	}
	// 4. Mask BEFORE binding our workspace: if workspaceRoot were masked after,
	// the tmpfs would swallow the bed's own mount too.
	argv = append(argv, "--tmpfs", workspaceRoot)
	for _, p := range maskPaths {
		argv = append(argv, "--tmpfs", p)
	}
	// 5.
	argv = append(argv, "--bind", wsPath, BwrapMountPoint)
	// 6.
	argv = append(argv, unsetSecretEnvArgs(environ)...)
	// 7.
	argv = append(argv,
		"--chdir", BwrapMountPoint,
		"--die-with-parent",
		"--",
	)
	return argv
}

// secretEnvPatterns are env-name globs whose values must not reach bed code.
// Borrowed from OpenSandbox execd's strict-profile blacklist.
var secretEnvPatterns = []string{
	"*_API_KEY", "*_TOKEN", "*_SECRET", "*_PASSWORD",
	"AWS_*", "K8S_*", "KUBE_*",
}

// unsetSecretEnvArgs returns --unsetenv args for every env var in environ
// matching secretEnvPatterns.
func unsetSecretEnvArgs(environ []string) []string {
	var argv []string
	for _, env := range environ {
		name, _, _ := strings.Cut(env, "=")
		for _, pattern := range secretEnvPatterns {
			if matchEnvPattern(name, pattern) {
				argv = append(argv, "--unsetenv", name)
				break
			}
		}
	}
	return argv
}

// matchEnvPattern is a case-insensitive glob supporting a single leading or
// trailing "*" (the only shapes secretEnvPatterns uses).
func matchEnvPattern(name, pattern string) bool {
	name = strings.ToUpper(name)
	pattern = strings.ToUpper(pattern)
	if suffix, ok := strings.CutPrefix(pattern, "*"); ok {
		return strings.HasSuffix(name, suffix)
	}
	if prefix, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(name, prefix)
	}
	return name == pattern
}

// defaultMaskCandidates are host paths masked when they exist: host user data
// and platform-mounted credentials (e.g. K8s serviceaccount tokens). Secrets
// belong to hostel/managed services, never to arbitrary bed code.
var defaultMaskCandidates = []string{
	"/root",
	"/home",
	"/run/secrets",
	"/var/run/secrets",
}
