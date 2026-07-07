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
	"encoding/json"
	"fmt"
	"net"
	"sync"

	"github.com/gobwas/ws"
	"github.com/gobwas/ws/wsutil"
)

// proxyCDP bridges an already-upgraded client websocket (conn) to the shared
// browser's CDP websocket (upstreamWS), presenting only bedID's own
// BrowserContext. This is the mechanism behind "pod-level shared Chromium,
// per-bed slice": the browser PROCESS is shared, but a bed's playwright — via
// this proxy — sees and drives only targets in its own context.
//
// Enforcement (grey-list, not an exhaustive white-list, so the CDP surface can
// evolve without breaking playwright/chrome-devtools-mcp):
//
//	client → browser (commands)
//	  Target.createTarget           → force the bed's browserContextId
//	  Browser.close/crash*          → drop (would kill the shared browser)
//	browser → client (events/responses)
//	  Target.targetCreated/Info*    → drop when the context isn't the bed's
//	  Target.getTargets response    → filter targetInfos to the bed's context
//	  Target.createBrowserContext resp → remember the id the bed just created
//
// Other beds' target ids are thus never revealed, so the bed cannot even name
// them to attach. Everything else is copied verbatim.
func proxyCDP(conn net.Conn, upstreamWS, bedID, contextID string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	up, _, _, err := ws.Dial(ctx, upstreamWS)
	if err != nil {
		return fmt.Errorf("amenity: chromium CDP dial upstream %q: %w", upstreamWS, err)
	}
	defer up.Close()

	f := &cdpFilter{contextID: contextID, ownedCtx: map[string]bool{contextID: true}, pending: map[int]string{}}
	done := make(chan struct{}, 2)

	// client → browser: rewrite/deny commands.
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msg, err := wsutil.ReadClientText(conn)
			if err != nil {
				return
			}
			out := f.onClientMsg(msg)
			if out == nil {
				continue
			}
			if err := wsutil.WriteClientText(up, out); err != nil {
				return
			}
		}
	}()
	// browser → client: filter events/responses.
	go func() {
		defer func() { done <- struct{}{} }()
		for {
			msg, err := wsutil.ReadServerText(up)
			if err != nil {
				return
			}
			out := f.onBrowserMsg(msg)
			if out == nil {
				continue
			}
			if err := wsutil.WriteServerText(conn, out); err != nil {
				return
			}
		}
	}()

	<-done // first side to end tears both down (defer cancel + Close)
	return nil
}

// cdpFilter holds the per-bed pinning state for one proxied connection.
type cdpFilter struct {
	contextID string // the bed's own BrowserContextId

	mu       sync.Mutex
	ownedCtx map[string]bool // contexts the bed owns (its own + any it created)
	pending  map[int]string  // in-flight command id → method (to read its response)
}

func (f *cdpFilter) owns(ctxID string) bool {
	if ctxID == "" {
		return false
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ownedCtx[ctxID]
}

// onClientMsg transforms a command from the bed's playwright ((nil) = drop).
func (f *cdpFilter) onClientMsg(raw []byte) []byte {
	var m struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Method == "" {
		return raw
	}
	switch m.Method {
	case "Browser.close", "Browser.crash", "Browser.crashGpuProcess":
		return nil // would kill the browser for every bed
	case "Target.createTarget":
		if out, err := setParamField(raw, "browserContextId", f.contextID); err == nil {
			raw = out
		}
	}
	if m.ID != 0 {
		f.mu.Lock()
		f.pending[m.ID] = m.Method
		f.mu.Unlock()
	}
	return raw
}

// onBrowserMsg filters an event/response heading to the bed ((nil) = drop).
func (f *cdpFilter) onBrowserMsg(raw []byte) []byte {
	var m struct {
		ID     int             `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	if m.Method != "" { // event
		switch m.Method {
		case "Target.targetCreated", "Target.targetInfoChanged":
			if ctxID := targetInfoContextID(m.Params); ctxID != "" && !f.owns(ctxID) {
				return nil // a sibling bed's target — hide it
			}
		}
		return raw
	}
	// response: interpret by the command it answers.
	f.mu.Lock()
	method := f.pending[m.ID]
	delete(f.pending, m.ID)
	f.mu.Unlock()
	switch method {
	case "Target.createBrowserContext":
		if id := resultContextID(m.Result); id != "" {
			f.mu.Lock()
			f.ownedCtx[id] = true
			f.mu.Unlock()
		}
	case "Target.getTargets":
		if out, ok := filterGetTargets(raw, f); ok {
			return out
		}
	}
	return raw
}

// --- JSON helpers ----------------------------------------------------------

func targetInfoContextID(params json.RawMessage) string {
	var p struct {
		TargetInfo struct {
			BrowserContextID string `json:"browserContextId"`
		} `json:"targetInfo"`
	}
	_ = json.Unmarshal(params, &p)
	return p.TargetInfo.BrowserContextID
}

func resultContextID(result json.RawMessage) string {
	var r struct {
		BrowserContextID string `json:"browserContextId"`
	}
	_ = json.Unmarshal(result, &r)
	return r.BrowserContextID
}

// setParamField sets params.<field>=val on a CDP command envelope.
func setParamField(raw []byte, field, val string) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw, err
	}
	params, _ := m["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	params[field] = val
	m["params"] = params
	return json.Marshal(m)
}

// filterGetTargets rewrites a Target.getTargets response, keeping only
// targetInfos in a context the bed owns.
func filterGetTargets(raw []byte, f *cdpFilter) ([]byte, bool) {
	var m struct {
		ID     int `json:"id"`
		Result struct {
			TargetInfos []map[string]any `json:"targetInfos"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw, false
	}
	kept := m.Result.TargetInfos[:0]
	for _, ti := range m.Result.TargetInfos {
		ctxID, _ := ti["browserContextId"].(string)
		if f.owns(ctxID) {
			kept = append(kept, ti)
		}
	}
	out, err := json.Marshal(map[string]any{
		"id":     m.ID,
		"result": map[string]any{"targetInfos": kept},
	})
	if err != nil {
		return raw, false
	}
	return out, true
}
