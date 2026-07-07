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
	"encoding/json"
	"testing"
)

func newFilter(ctx string) *cdpFilter {
	return &cdpFilter{contextID: ctx, ownedCtx: map[string]bool{ctx: true}, pending: map[int]string{}}
}

// TestCDPFilterClientCommands: createTarget is pinned to the bed's context and
// the browser-killing commands are dropped.
func TestCDPFilterClientCommands(t *testing.T) {
	f := newFilter("CTX-BED")

	// createTarget with NO context → forced to the bed's.
	out := f.onClientMsg([]byte(`{"id":1,"method":"Target.createTarget","params":{"url":"about:blank"}}`))
	if got := paramField(t, out, "browserContextId"); got != "CTX-BED" {
		t.Errorf("createTarget browserContextId = %q, want CTX-BED", got)
	}
	// createTarget trying to target ANOTHER context → overwritten to the bed's.
	out = f.onClientMsg([]byte(`{"id":2,"method":"Target.createTarget","params":{"browserContextId":"CTX-OTHER","url":"x"}}`))
	if got := paramField(t, out, "browserContextId"); got != "CTX-BED" {
		t.Errorf("createTarget hijack not pinned: %q", got)
	}
	// Browser.close would kill the shared browser → dropped.
	if out := f.onClientMsg([]byte(`{"id":3,"method":"Browser.close"}`)); out != nil {
		t.Errorf("Browser.close must be dropped, got %s", out)
	}
	// An unrelated command passes through untouched.
	in := []byte(`{"id":4,"method":"Page.navigate","params":{"url":"y"}}`)
	if out := f.onClientMsg(in); string(out) != string(in) {
		t.Errorf("passthrough altered: %s", out)
	}
}

// TestCDPFilterEvents: target lifecycle events for a sibling bed's context are
// hidden; the bed's own are visible.
func TestCDPFilterEvents(t *testing.T) {
	f := newFilter("CTX-BED")
	own := `{"method":"Target.targetCreated","params":{"targetInfo":{"targetId":"T1","browserContextId":"CTX-BED"}}}`
	other := `{"method":"Target.targetCreated","params":{"targetInfo":{"targetId":"T2","browserContextId":"CTX-OTHER"}}}`
	if out := f.onBrowserMsg([]byte(own)); out == nil {
		t.Errorf("own target event must pass")
	}
	if out := f.onBrowserMsg([]byte(other)); out != nil {
		t.Errorf("sibling target event must be dropped, got %s", out)
	}
}

// TestCDPFilterGetTargets: the getTargets response is filtered to the bed's
// contexts (correlated by the pending command id).
func TestCDPFilterGetTargets(t *testing.T) {
	f := newFilter("CTX-BED")
	// The command establishes the pending id → method mapping.
	f.onClientMsg([]byte(`{"id":7,"method":"Target.getTargets"}`))
	resp := `{"id":7,"result":{"targetInfos":[
		{"targetId":"T1","browserContextId":"CTX-BED"},
		{"targetId":"T2","browserContextId":"CTX-OTHER"}]}}`
	out := f.onBrowserMsg([]byte(resp))
	var m struct {
		Result struct {
			TargetInfos []map[string]any `json:"targetInfos"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if len(m.Result.TargetInfos) != 1 || m.Result.TargetInfos[0]["targetId"] != "T1" {
		t.Errorf("getTargets not filtered to the bed: %s", out)
	}
}

// TestCDPFilterCreatedContextBecomesVisible: a context the bed creates itself is
// tracked, so its later target events are no longer dropped.
func TestCDPFilterCreatedContextBecomesVisible(t *testing.T) {
	f := newFilter("CTX-BED")
	f.onClientMsg([]byte(`{"id":9,"method":"Target.createBrowserContext"}`))
	f.onBrowserMsg([]byte(`{"id":9,"result":{"browserContextId":"CTX-NEW"}}`))
	evt := `{"method":"Target.targetCreated","params":{"targetInfo":{"targetId":"T3","browserContextId":"CTX-NEW"}}}`
	if out := f.onBrowserMsg([]byte(evt)); out == nil {
		t.Errorf("event for the bed's own created context must pass")
	}
}

func paramField(t *testing.T, raw []byte, field string) string {
	t.Helper()
	var m struct {
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	s, _ := m.Params[field].(string)
	return s
}
