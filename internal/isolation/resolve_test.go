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

import (
	"os/exec"
	"testing"
)

// fakeMech is an availability-controllable mechanism for resolver tests.
type fakeMech struct {
	name  string
	lvl   Level
	avail bool
}

func (m fakeMech) Name() string                    { return m.name }
func (m fakeMech) Level() Level                    { return m.lvl }
func (m fakeMech) Available() bool                 { return m.avail }
func (m fakeMech) MountPoint() string              { return "" }
func (m fakeMech) Wrap(*exec.Cmd, Workspace) error { return nil }

// resolveMechs mirrors New's selection over an injected candidate set, so the
// "effective = highest achievable ≤ requested" rule is tested without a real
// kernel. Kept in lockstep with New.
func resolveMechs(req Level, candidates []Isolator) (chosen Isolator, eff, ceiling Level) {
	chosen = direct{}
	eff, ceiling = Dorm, Dorm
	for _, m := range candidates {
		if !m.Available() {
			continue
		}
		if m.Level() > ceiling {
			ceiling = m.Level()
		}
		if m.Level() <= req && m.Level() >= eff {
			chosen = m
			eff = m.Level()
		}
	}
	return
}

func TestResolveEffectiveLevel(t *testing.T) {
	suite := fakeMech{"bwrap", Suite, false}
	room := fakeMech{"landlock", Room, false}
	all := func(s, r bool) []Isolator {
		return []Isolator{fakeMech{"bwrap", Suite, s}, fakeMech{"landlock", Room, r}, direct{}}
	}

	cases := []struct {
		name        string
		req         Level
		suite, room bool
		wantEff     Level
		wantCeil    Level
		wantMech    string
	}{
		// auto (=suite request) takes the ceiling.
		{"auto full env", Suite, true, true, Suite, Suite, "bwrap"},
		{"auto no userns", Suite, false, true, Room, Room, "landlock"},
		{"auto nothing", Suite, false, false, Dorm, Dorm, "direct"},
		// deliberate downgrade is honored even when more is available.
		{"request room, suite available", Room, true, true, Room, Suite, "landlock"},
		{"request dorm always dorm", Dorm, true, true, Dorm, Suite, "direct"},
		// request exceeds ceiling → honest degrade to best achievable ≤ req.
		{"request suite, only room", Suite, false, true, Room, Room, "landlock"},
		// request room but room's mechanism missing (suite present) → drop to
		// best ≤ room that IS available = dorm; never silently give more.
		{"request room, only suite", Room, true, false, Dorm, Suite, "direct"},
	}
	_ = suite
	_ = room
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chosen, eff, ceil := resolveMechs(tc.req, all(tc.suite, tc.room))
			if eff != tc.wantEff || ceil != tc.wantCeil || chosen.Name() != tc.wantMech {
				t.Fatalf("eff=%s ceil=%s mech=%s; want eff=%s ceil=%s mech=%s",
					eff, ceil, chosen.Name(), tc.wantEff, tc.wantCeil, tc.wantMech)
			}
		})
	}
}

func TestParseRequestAndNewReports(t *testing.T) {
	for in, want := range map[string]Level{
		"dorm": Dorm, "room": Room, "suite": Suite, "auto": Suite, "": Suite, "bogus": Suite,
	} {
		if got := parseRequest(in); got != want {
			t.Errorf("parseRequest(%q) = %s, want %s", in, got, want)
		}
	}
	// On this host (mac/CI, no bwrap/landlock) New must resolve to dorm/direct
	// and expose the Report interface honestly.
	iso := New("auto", t.TempDir())
	if iso.Name() != "direct" || iso.Level() != Dorm {
		t.Fatalf("New(auto) on unprivileged host = %s/%s", iso.Name(), iso.Level())
	}
	r, ok := iso.(Report)
	if !ok || r.Requested() != Suite || r.Effective() != Dorm {
		t.Fatalf("report = %+v ok=%v", r, ok)
	}
}
