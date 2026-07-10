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

// Package playwright is the in-bed verb dispatcher behind the `playwright` and
// `playwright-cli` command names (multi-call symlinks to the hostel binary).
//
// Agents type playwright commands from public-internet muscle memory; the
// image ships the real npm CLIs. This dispatcher routes verbs so those habits
// land on the bed's own slice of the shared browser instead of exploding or
// launching stray browser processes (docs/amenity.md §6):
//
//   - install / install-deps → no-op success. Browsers and system deps are
//     baked into the image; in-bed non-root cannot apt anyway (--with-deps
//     escalates via su and dies).
//   - page verbs → the real @playwright/cli. Its per-bed daemon connects to
//     the bed's proxied CDP endpoint via PLAYWRIGHT_MCP_CDP_ENDPOINT (injected
//     by hostel at spawn); when no session is open yet we bootstrap one with
//     `open` and retry — lazily, matching the amenity's demand-started browser.
//   - open <url> → goto. A session-scoped `open <url>` verifiably launches a
//     NEW browser instead of reusing the connected session, so it must be
//     rewritten.
//   - anything else → the real playwright package CLI (codegen etc.), true
//     upstream semantics.
//
// The dispatcher deliberately parses no flags — routing is by verb only, so
// upstream CLI surface changes cannot silently break it.
package playwright

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Real CLI entrypoints (node scripts). Env-overridable because they encode
// image layout, not hostel contract.
const (
	defaultCLIReal = "/usr/local/lib/node_modules/@playwright/cli/playwright-cli.js"
	defaultPWReal  = "/usr/local/lib/node_modules/playwright/cli.js"
)

// pageVerbs are @playwright/cli commands that act on the bed's browser session
// (from `playwright-cli --help`). A new upstream verb missing here degrades to
// the classic CLI's "unknown command" error — visible, not silent.
var pageVerbs = map[string]bool{
	"goto": true, "screenshot": true, "snapshot": true, "find": true,
	"click": true, "dblclick": true, "fill": true, "type": true,
	"drag": true, "drop": true, "hover": true, "select": true,
	"upload": true, "check": true, "uncheck": true, "eval": true,
	"dialog-accept": true, "dialog-dismiss": true, "resize": true,
	"delete-data": true, "go-back": true, "go-forward": true, "reload": true,
	"press": true, "keydown": true, "keyup": true, "mousemove": true,
	"mousedown": true, "mouseup": true, "mousewheel": true, "pdf": true,
	"tab-list": true, "tab-new": true, "tab-close": true, "tab-select": true,
	"state-load": true, "state-save": true,
}

type actionKind int

const (
	kindNoop      actionKind = iota // install/install-deps: succeed without work
	kindCLI                         // @playwright/cli page verb, with session bootstrap
	kindCLIDirect                   // @playwright/cli verbatim (attach/detach/close/help)
	kindPW                          // classic playwright CLI verbatim
)

type action struct {
	kind actionKind
	argv []string
}

// route is the pure routing decision — kept side-effect free for testing.
// cdpEndpoint is the bed's injected PLAYWRIGHT_MCP_CDP_ENDPOINT ("" outside a
// bed or when the browser amenity is absent).
func route(args []string, cdpEndpoint string) action {
	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "", "-h", "--help", "help":
		return action{kind: kindCLIDirect, argv: []string{"--help"}}
	case "install", "install-deps":
		return action{kind: kindNoop, argv: args}
	case "open", "navigate":
		if len(args) >= 2 && isURL(args[1]) {
			return action{kind: kindCLI, argv: append([]string{"goto"}, args[1:]...)}
		}
		// Bare open = "make the browser usable": with the endpoint injected it
		// connects to the bed slice; passthrough keeps upstream semantics.
		return action{kind: kindCLIDirect, argv: args}
	case "attach":
		// A bare attach inside a bed gets the bed's endpoint, so it can never
		// wander off to something else.
		if cdpEndpoint != "" && !hasConnectFlag(args[1:]) {
			return action{kind: kindCLIDirect, argv: append([]string{"attach", "--cdp=" + cdpEndpoint}, args[1:]...)}
		}
		return action{kind: kindCLIDirect, argv: args}
	case "detach", "close":
		return action{kind: kindCLIDirect, argv: args}
	}
	if pageVerbs[cmd] {
		return action{kind: kindCLI, argv: args}
	}
	return action{kind: kindPW, argv: args}
}

func isURL(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") ||
		strings.HasPrefix(s, "file://")
}

func hasConnectFlag(args []string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, "--cdp") || strings.HasPrefix(a, "--endpoint") || strings.HasPrefix(a, "--extension") {
			return true
		}
	}
	return false
}

// Run dispatches one invocation; the returned int is the exit code (page-verb
// and passthrough paths normally exec-replace the process instead).
func Run(args []string) int {
	a := route(args, os.Getenv("PLAYWRIGHT_MCP_CDP_ENDPOINT"))
	switch a.kind {
	case kindNoop:
		fmt.Printf("playwright browsers and system dependencies are preinstalled in this image; %q skipped (browsers live in %s).\n",
			a.argv[0], envOr("PLAYWRIGHT_BROWSERS_PATH", "the image browsers dir"))
		return 0
	case kindPW:
		return execNode(envOr("PLAYWRIGHT_REAL_CLI", defaultPWReal), a.argv)
	case kindCLIDirect:
		return execNode(cliReal(), a.argv)
	default:
		return ensureSessionRun(a.argv)
	}
}

// ensureSessionRun runs a page verb, bootstrapping the daemon session once if
// needed: @playwright/cli requires an explicit open/attach before any page
// command, and with PLAYWRIGHT_MCP_CDP_ENDPOINT set that `open` CONNECTS to
// the bed's proxied slice rather than launching a browser. Bootstrapping here,
// on first use, keeps hostel's browser demand-started.
func ensureSessionRun(argv []string) int {
	out, code := runNode(cliReal(), argv)
	if code != 0 && strings.Contains(out, "is not open") && os.Getenv("PLAYWRIGHT_MCP_CDP_ENDPOINT") != "" {
		if _, bootCode := runNode(cliReal(), []string{"open"}); bootCode == 0 {
			return execNode(cliReal(), argv)
		}
	}
	os.Stdout.WriteString(out)
	return code
}

func cliReal() string { return envOr("PLAYWRIGHT_CLI_REAL", defaultCLIReal) }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// execNode replaces this process with `node <script> <args...>` — argv, stdio
// and exit code pass through with full fidelity.
func execNode(script string, args []string) int {
	node, err := exec.LookPath("node")
	if err != nil {
		fmt.Fprintf(os.Stderr, "playwright dispatcher: node not found in PATH: %v\n", err)
		return 127
	}
	argv := append([]string{"node", script}, args...)
	if err := syscall.Exec(node, argv, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "playwright dispatcher: exec %s: %v\n", script, err)
		return 126
	}
	return 0 // unreachable
}

// runNode runs `node <script> <args...>` capturing combined output (needed to
// sniff the bootstrap condition without disturbing what the caller sees).
func runNode(script string, args []string) (string, int) {
	cmd := exec.Command("node", append([]string{script}, args...)...)
	cmd.Env = os.Environ()
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	return string(out), code
}
