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

// Package isolation confines a bed's processes. Data isolation is graded into
// "hostel room types" (docs/data-isolation.md): the LEVEL is the north-facing
// guarantee, the MECHANISM (direct/landlock/bwrap) is how it's realized on the
// current host. A request expresses a wish; the effective level is capped by
// what the environment can actually deliver:
//
//	effective = highest achievable level ≤ requested
//
// so "auto" yields the environment's ceiling, and an explicit lower request is
// an honest, deliberate downgrade.
package isolation

import (
	"log"
	"os/exec"
)

// Level is a data-isolation guarantee, ordered weakest→strongest.
type Level int

const (
	// Dorm — bunk room: no barrier between beds (organizational split only).
	Dorm Level = iota
	// Room — private room, shared toilet: a bed can't ACCESS others' data
	// (EACCES) but siblings stay visible and host paths (/tmp, /usr) are shared.
	Room
	// Suite — fully private: siblings invisible, private mount view, canonical
	// /workspace, env scrubbed.
	Suite
)

func (l Level) String() string {
	switch l {
	case Room:
		return "room"
	case Suite:
		return "suite"
	default:
		return "dorm"
	}
}

// parseRequest maps a config value to a requested level. "auto" (and "") means
// "as high as the environment allows" → Suite.
func parseRequest(s string) Level {
	switch s {
	case "room":
		return Room
	case "suite":
		return Suite
	default: // "auto", "dorm", unknown
		if s == "dorm" {
			return Dorm
		}
		return Suite
	}
}

// Workspace is the writable root a bed's commands are rooted at.
type Workspace struct {
	// Path is the bed's data directory on the host.
	Path string
}

// Isolator confines an exec.Cmd. Each mechanism reports the Level it provides.
type Isolator interface {
	// Name is the mechanism: direct | landlock | bwrap.
	Name() string
	// Level is the guarantee this mechanism delivers.
	Level() Level
	// Available reports whether the mechanism actually works on this host
	// (probed at construction). direct is always available.
	Available() bool
	// MountPoint is where the workspace appears inside the sandbox
	// ("/workspace" for suite/bwrap; "" otherwise — real host paths).
	MountPoint() string
	// Wrap prepares cmd to run confined to ws. A mechanism reported Available
	// must NOT silently degrade here — failing to build the sandbox is an error.
	Wrap(cmd *exec.Cmd, ws Workspace) error
}

// Report is the boot-time resolution, exposed for capabilities/healthz.
type Report interface {
	Requested() Level
	Effective() Level
	Ceiling() Level
	Mechanism() string
}

// resolved wraps the chosen mechanism with the resolution facts.
type resolved struct {
	Isolator
	req, eff, ceil Level
}

func (r *resolved) Requested() Level  { return r.req }
func (r *resolved) Effective() Level  { return r.eff }
func (r *resolved) Ceiling() Level    { return r.ceil }
func (r *resolved) Mechanism() string { return r.Isolator.Name() }

// New resolves the requested isolation level against what the environment can
// deliver and returns the chosen mechanism (also implementing Report). The
// returned value is always usable — worst case it degrades to dorm/direct,
// which is logged honestly.
func New(requested, workspaceRoot string) Isolator {
	req := parseRequest(requested)

	// Candidate mechanisms, strongest first. Each probes availability at
	// construction. direct (dorm) is the always-available floor.
	candidates := []Isolator{
		newBwrap(workspaceRoot),    // suite
		newLandlock(workspaceRoot), // room
		direct{},                   // dorm
	}

	ceiling := Dorm
	var chosen Isolator = direct{}
	eff := Dorm
	for _, m := range candidates {
		if !m.Available() {
			continue
		}
		if m.Level() > ceiling {
			ceiling = m.Level()
		}
		// Highest available level that does not exceed the request.
		if m.Level() <= req && m.Level() >= eff {
			chosen = m
			eff = m.Level()
		}
	}

	if eff < req {
		log.Printf("isolation: requested %s but environment ceiling is %s — using %s (mechanism=%s)",
			req, ceiling, eff, chosen.Name())
	} else {
		log.Printf("isolation: level=%s mechanism=%s (requested=%s, ceiling=%s)",
			eff, chosen.Name(), req, ceiling)
	}
	return &resolved{Isolator: chosen, req: req, eff: eff, ceil: ceiling}
}

// unavailable is a mechanism that probed as not usable on this host. It keeps
// its Level so the resolver can still compute the ceiling correctly, but is
// never chosen (Available()=false) and refuses to Wrap.
type unavailable struct {
	name string
	lvl  Level
}

func (u unavailable) Name() string       { return u.name }
func (u unavailable) Level() Level       { return u.lvl }
func (u unavailable) Available() bool    { return false }
func (u unavailable) MountPoint() string { return "" }
func (u unavailable) Wrap(*exec.Cmd, Workspace) error {
	return errUnavailable
}

var errUnavailable = &isoError{"isolation: mechanism unavailable"}

type isoError struct{ msg string }

func (e *isoError) Error() string { return e.msg }

// direct runs the command straight on the host, only pinning its cwd to the
// bed workspace. The dorm level: no enforced isolation. Always available.
type direct struct{}

func (direct) Name() string       { return "direct" }
func (direct) Level() Level       { return Dorm }
func (direct) Available() bool    { return true }
func (direct) MountPoint() string { return "" }
func (direct) Wrap(cmd *exec.Cmd, ws Workspace) error {
	cmd.Dir = ws.Path
	return nil
}
