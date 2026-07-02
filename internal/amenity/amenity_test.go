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

package amenity

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeAmenity struct {
	name     string
	released []string
}

func (f *fakeAmenity) Name() string  { return f.name }
func (f *fakeAmenity) State() string { return StateIdle }
func (f *fakeAmenity) AcquireTenant(bedID, ws string) (Tenant, error) {
	return nil, nil
}
func (f *fakeAmenity) ReleaseTenant(bedID string) error {
	f.released = append(f.released, bedID)
	return nil
}

func TestRegistryFindAndReleaseAll(t *testing.T) {
	r := NewRegistry()
	a := &fakeAmenity{name: "x"}
	r.Register(a)
	if r.Find("x") == nil || r.Find("nope") != nil {
		t.Fatal("Find broken")
	}
	r.ReleaseAll("bed-1")
	if len(a.released) != 1 || a.released[0] != "bed-1" {
		t.Fatalf("ReleaseAll: %v", a.released)
	}
	// nil registry is inert.
	var nilReg *Registry
	nilReg.Register(a)
	nilReg.ReleaseAll("z")
	if nilReg.List() != nil {
		t.Fatal("nil registry should list nothing")
	}
}

// TestChromiumEndToEnd runs against a real browser when one is present
// (macOS dev machines usually have Chrome; CI without one skips).
func TestChromiumEndToEnd(t *testing.T) {
	br, ok := NewChromium(ChromiumConfig{IdleStop: 200 * time.Millisecond, ActionTimeout: 20 * time.Second})
	if !ok {
		t.Skip("no chromium/chrome available on this host")
	}
	c := br.(*chromium)
	ctx := context.Background()
	wsA, wsB := t.TempDir(), t.TempDir()

	// Lifecycle: idle until first demand.
	if c.State() != StateIdle {
		t.Fatalf("initial state = %s", c.State())
	}

	title, _, err := br.Goto(ctx, "bedA", wsA, `data:text/html,<title>hello-a</title><body>alpha content</body>`)
	if err != nil {
		t.Fatalf("Goto A: %v", err)
	}
	if title != "hello-a" {
		t.Fatalf("title = %q", title)
	}
	if c.State() != StateRunning {
		t.Fatalf("state after demand = %s", c.State())
	}

	text, err := br.Text(ctx, "bedA", wsA)
	if err != nil || !strings.Contains(text, "alpha content") {
		t.Fatalf("Text A: %q err=%v", text, err)
	}

	// Second bed gets its own context — its page is independent.
	if _, _, err := br.Goto(ctx, "bedB", wsB, `data:text/html,<body>beta content</body>`); err != nil {
		t.Fatalf("Goto B: %v", err)
	}
	textB, _ := br.Text(ctx, "bedB", wsB)
	if !strings.Contains(textB, "beta") || strings.Contains(textB, "alpha") {
		t.Fatalf("bed contexts not independent: %q", textB)
	}
	if len(c.tenants) != 2 {
		t.Fatalf("tenants = %d, want 2", len(c.tenants))
	}

	// Screenshot lands in the right bed's workspace, virtual path returned.
	saved, err := br.Screenshot(ctx, "bedA", wsA, "")
	if err != nil {
		t.Fatalf("Screenshot: %v", err)
	}
	if !strings.HasPrefix(saved, "/workspace/screenshots/") {
		t.Fatalf("virtual path = %q", saved)
	}
	onDisk := filepath.Join(wsA, strings.TrimPrefix(saved, "/workspace/"))
	if fi, err := os.Stat(onDisk); err != nil || fi.Size() == 0 {
		t.Fatalf("screenshot file: %v", err)
	}
	// Escaping paths refused.
	if _, err := br.Screenshot(ctx, "bedA", wsA, "../evil.png"); err == nil {
		t.Fatal("escaping screenshot path not rejected")
	}

	// Release both tenants → idle-stop kicks in.
	_ = br.ReleaseTenant("bedA")
	_ = br.ReleaseTenant("bedB")
	deadline := time.Now().Add(5 * time.Second)
	for c.State() != StateIdle && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if c.State() != StateIdle {
		t.Fatalf("idle-stop did not fire: state=%s", c.State())
	}

	// And the facility restarts on new demand.
	if _, _, err := br.Goto(ctx, "bedC", t.TempDir(), `data:text/html,<title>again</title>`); err != nil {
		t.Fatalf("Goto after idle-stop: %v", err)
	}
	_ = br.ReleaseTenant("bedC")
	c.mu.Lock()
	c.stopLocked()
	c.mu.Unlock()
}
