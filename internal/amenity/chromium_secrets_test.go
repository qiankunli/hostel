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

import "testing"

// The bed-level CDP secret must survive tenant recycling (browser/close lands
// in ReleaseTenant while the bed lives on, and long-running shells keep the
// env-injected endpoint) and die only with the bed (RevokeBedSecrets, wired
// into Registry.ReleaseAll).
func TestCDPSecretLifecycle(t *testing.T) {
	c := &chromium{cfg: ChromiumConfig{DebugPort: 9222}, state: StateIdle,
		tenants: map[string]*chromiumTenant{}, cdpSecrets: map[string]string{}}

	tok, err := c.CDPToken("b1")
	if err != nil || tok == "" {
		t.Fatalf("mint: tok=%q err=%v", tok, err)
	}
	if again, _ := c.CDPToken("b1"); again != tok {
		t.Fatalf("re-mint changed the secret: %q != %q", again, tok)
	}

	// browser/close path: tenant released, secret must survive.
	if err := c.ReleaseTenant("b1"); err != nil {
		t.Fatalf("ReleaseTenant: %v", err)
	}
	if after, _ := c.CDPToken("b1"); after != tok {
		t.Fatalf("ReleaseTenant revoked the bed secret: %q != %q", after, tok)
	}

	// Bed teardown path: Registry.ReleaseAll revokes via BedScopedSecrets.
	reg := NewRegistry()
	reg.Register(c)
	reg.ReleaseAll("b1")
	if fresh, _ := c.CDPToken("b1"); fresh == tok {
		t.Fatalf("bed teardown did not revoke the secret")
	}
}
