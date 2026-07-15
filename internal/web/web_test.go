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

package web

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/qiankunli/hostel/internal/bed"
	"github.com/qiankunli/hostel/internal/isolation"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	mgr, err := bed.NewManager(root, "default", "/bin/bash", isolation.New("dorm", root), nil, 0, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return NewServer(mgr)
}

func do(t *testing.T, s *Server, method, path string, body io.Reader, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, body)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestPingAndHealthz(t *testing.T) {
	s := newTestServer(t)
	if rec := do(t, s, "GET", "/ping", nil, nil); rec.Code != 200 || rec.Body.String() != "pong" {
		t.Fatalf("/ping = %d %q", rec.Code, rec.Body.String())
	}
	rec := do(t, s, "GET", "/healthz", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("/healthz = %d", rec.Code)
	}
	var h map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &h)
	iso, _ := h["isolation"].(map[string]any)
	if h["ok"] != true || iso == nil || iso["level"] != "dorm" || iso["mechanism"] != "direct" {
		t.Fatalf("/healthz body = %v", h)
	}
}

func TestUploadInfoDownloadRoundTrip(t *testing.T) {
	s := newTestServer(t)

	// multipart upload with a metadata JSON part carrying the path.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("metadata", `{"path":"/workspace/hi.txt"}`)
	fw, _ := mw.CreateFormFile("file", "hi.txt")
	_, _ = fw.Write([]byte("hello hostel"))
	_ = mw.Close()

	rec := do(t, s, "POST", "/files/upload", &buf, map[string]string{"Content-Type": mw.FormDataContentType()})
	if rec.Code != 200 {
		t.Fatalf("upload = %d %s", rec.Code, rec.Body.String())
	}

	// info returns a map keyed by the requested path.
	rec = do(t, s, "GET", "/files/info?path=/workspace/hi.txt", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("info = %d %s", rec.Code, rec.Body.String())
	}
	var info map[string]map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &info)
	if info["/workspace/hi.txt"]["type"] != "file" {
		t.Fatalf("info body = %v", info)
	}

	// download returns the bytes, with an explicit Content-Length so relays
	// (e.g. to S3 presigned PUT) never see a chunked body of unknown size.
	rec = do(t, s, "GET", "/files/download?path=/workspace/hi.txt", nil, nil)
	if rec.Code != 200 || rec.Body.String() != "hello hostel" {
		t.Fatalf("download = %d %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len("hello hostel")) {
		t.Fatalf("download Content-Length = %q", got)
	}
}

func TestAbsolutePathAndCommandCwdShareBedRoot(t *testing.T) {
	s := newTestServer(t)
	const clientPath = "/tmp/workspace/job/input.txt"

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("metadata", `{"path":"`+clientPath+`"}`)
	fw, _ := mw.CreateFormFile("file", "input.txt")
	_, _ = fw.Write([]byte("bed-local"))
	_ = mw.Close()

	rec := do(t, s, "POST", "/files/upload", &buf, map[string]string{"Content-Type": mw.FormDataContentType()})
	if rec.Code != http.StatusOK {
		t.Fatalf("upload absolute path = %d %s", rec.Code, rec.Body.String())
	}

	rec = do(t, s, "POST", "/command",
		strings.NewReader(`{"command":"cat input.txt","cwd":"/tmp/workspace/job"}`),
		map[string]string{"Content-Type": "application/json"})
	if rec.Code != http.StatusOK {
		t.Fatalf("command absolute cwd = %d %s", rec.Code, rec.Body.String())
	}
	var output string
	for _, ev := range parseSSE(t, rec.Body.String()) {
		if ev.Type == EventStdout {
			output += ev.Text
		}
	}
	if !strings.Contains(output, "bed-local") {
		t.Fatalf("command cwd and file API resolved different locations: %q", output)
	}
}

// parseSSE extracts the JSON event frames from an SSE body.
func parseSSE(t *testing.T, body string) []StreamEvent {
	t.Helper()
	var evs []StreamEvent
	for _, frame := range strings.Split(body, "\n\n") {
		frame = strings.TrimSpace(frame)
		if frame == "" {
			continue
		}
		var ev StreamEvent
		if err := json.Unmarshal([]byte(frame), &ev); err != nil {
			t.Fatalf("bad SSE frame %q: %v", frame, err)
		}
		evs = append(evs, ev)
	}
	return evs
}

func TestCommandForegroundSSE(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, "POST", "/command", strings.NewReader(`{"command":"echo hostel-ok"}`),
		map[string]string{"Content-Type": "application/json"})
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
	evs := parseSSE(t, rec.Body.String())
	var sawStdout, sawComplete bool
	for _, ev := range evs {
		if ev.Type == EventStdout && strings.Contains(ev.Text, "hostel-ok") {
			sawStdout = true
		}
		if ev.Type == EventComplete {
			sawComplete = true
			if ev.ExitCode == nil || *ev.ExitCode != 0 {
				t.Fatalf("complete exit code = %v", ev.ExitCode)
			}
		}
	}
	if !sawStdout || !sawComplete {
		t.Fatalf("SSE missing events: stdout=%v complete=%v (%+v)", sawStdout, sawComplete, evs)
	}
}

func TestSessionStatePersistsAcrossRuns(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, "POST", "/session", strings.NewReader(`{}`), map[string]string{"Content-Type": "application/json"})
	if rec.Code != 200 {
		t.Fatalf("create session = %d %s", rec.Code, rec.Body.String())
	}
	var cr struct {
		SessionID string `json:"session_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &cr)
	if cr.SessionID == "" {
		t.Fatal("no session_id")
	}

	run := func(cmd string) []StreamEvent {
		rec := do(t, s, "POST", "/session/"+cr.SessionID+"/run",
			strings.NewReader(`{"command":`+jsonStr(cmd)+`}`),
			map[string]string{"Content-Type": "application/json"})
		if rec.Code != 200 {
			t.Fatalf("run %q = %d %s", cmd, rec.Code, rec.Body.String())
		}
		return parseSSE(t, rec.Body.String())
	}
	// Shell cwd starts at the bed workspace (host dir); use relative paths — the
	// /workspace virtual prefix is a file-API convenience, not a shell mount (v1).
	run("mkdir -p subdir && cd subdir")
	evs := run("pwd")
	joined := ""
	for _, ev := range evs {
		if ev.Type == EventStdout {
			joined += ev.Text
		}
	}
	if !strings.Contains(joined, "subdir") {
		t.Fatalf("cwd not preserved across runs: %q", joined)
	}
}

func TestBedIsolationAcrossHeader(t *testing.T) {
	s := newTestServer(t)
	// Write a file into bed "alice".
	up := func(bedID, path, content string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		_ = mw.WriteField("metadata", `{"path":"`+path+`"}`)
		fw, _ := mw.CreateFormFile("file", "f")
		_, _ = fw.Write([]byte(content))
		_ = mw.Close()
		rec := do(t, s, "POST", "/files/upload", &buf, map[string]string{
			"Content-Type": mw.FormDataContentType(),
			BedHeader:      bedID,
		})
		if rec.Code != 200 {
			t.Fatalf("upload bed=%s = %d %s", bedID, rec.Code, rec.Body.String())
		}
	}
	up("alice", "/tmp/workspace/secret.txt", "alice-data")

	// bob's bed must NOT see alice's file.
	rec := do(t, s, "GET", "/files/download?path=/tmp/workspace/secret.txt", nil, map[string]string{BedHeader: "bob"})
	if rec.Code != 404 {
		t.Fatalf("bob reading alice's file = %d (want 404)", rec.Code)
	}
	// alice still sees her own.
	rec = do(t, s, "GET", "/files/download?path=/tmp/workspace/secret.txt", nil, map[string]string{BedHeader: "alice"})
	if rec.Code != 200 || rec.Body.String() != "alice-data" {
		t.Fatalf("alice reading own file = %d %q", rec.Code, rec.Body.String())
	}
}

func TestBedsCRUD(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, "POST", "/v1/beds", strings.NewReader(`{"id":"conv-1"}`), map[string]string{"Content-Type": "application/json"})
	if rec.Code != 200 {
		t.Fatalf("create bed = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, s, "GET", "/v1/beds", nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "conv-1") {
		t.Fatalf("list beds = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, s, "DELETE", "/v1/beds/conv-1", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("delete bed = %d", rec.Code)
	}
	rec = do(t, s, "GET", "/v1/beds/conv-1", nil, nil)
	if rec.Code != 404 {
		t.Fatalf("get deleted bed = %d (want 404)", rec.Code)
	}
}

func TestCapabilities(t *testing.T) {
	s := newTestServer(t)
	rec := do(t, s, "GET", "/v1/beds/capabilities", nil, nil)
	var caps map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &caps)
	if caps["command"] != true || caps["pty"] != false {
		t.Fatalf("capabilities = %v", caps)
	}
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

var _ = http.StatusOK

func TestMaxBedsBackpressure(t *testing.T) {
	root := t.TempDir()
	mgr, err := bed.NewManager(root, "default", "/bin/bash", isolation.New("dorm", root), nil, 1, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	s := NewServer(mgr)

	// First bed fills the only slot.
	rec := do(t, s, "POST", "/v1/beds", strings.NewReader(`{"id":"one"}`), map[string]string{"Content-Type": "application/json"})
	if rec.Code != 200 {
		t.Fatalf("create one = %d %s", rec.Code, rec.Body.String())
	}
	// Second bed → 429 BED_LIMIT_EXCEEDED, whether via management API...
	rec = do(t, s, "POST", "/v1/beds", strings.NewReader(`{"id":"two"}`), map[string]string{"Content-Type": "application/json"})
	if rec.Code != 429 || !strings.Contains(rec.Body.String(), "BED_LIMIT_EXCEEDED") {
		t.Fatalf("create two = %d %s (want 429 BED_LIMIT_EXCEEDED)", rec.Code, rec.Body.String())
	}
	// ...or via implicit creation on any endpoint.
	rec = do(t, s, "GET", "/files/info?path=/workspace", nil, map[string]string{BedHeader: "three"})
	if rec.Code != 429 {
		t.Fatalf("implicit create three = %d (want 429)", rec.Code)
	}
	// Default bed still works on a full instance.
	rec = do(t, s, "GET", "/files/info?path=/workspace", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("default bed on full instance = %d %s", rec.Code, rec.Body.String())
	}
	// Capacity is reported for scheduler placement.
	rec = do(t, s, "GET", "/healthz", nil, nil)
	var h map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &h)
	if h["max_beds"] != float64(1) {
		t.Fatalf("healthz max_beds = %v, want 1", h["max_beds"])
	}
}

func TestCheckpointEndpointAndPersistenceReporting(t *testing.T) {
	root := t.TempDir()
	mgr, err := bed.NewManager(root, "default", "/bin/bash", isolation.New("dorm", root), nil, 0, nil)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	s := NewServer(mgr)

	// Checkpoint an existing bed (noop backend → trivially succeeds).
	rec := do(t, s, "POST", "/v1/beds", strings.NewReader(`{"id":"cp"}`), map[string]string{"Content-Type": "application/json"})
	if rec.Code != 200 {
		t.Fatalf("create = %d", rec.Code)
	}
	rec = do(t, s, "POST", "/v1/beds/cp/checkpoint", nil, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"persistence":"noop"`) {
		t.Fatalf("checkpoint = %d %s", rec.Code, rec.Body.String())
	}
	// Unknown bed → runtime error, not a crash.
	rec = do(t, s, "POST", "/v1/beds/ghost/checkpoint", nil, nil)
	if rec.Code != 500 {
		t.Fatalf("checkpoint unknown bed = %d", rec.Code)
	}

	// healthz reports the backend.
	rec = do(t, s, "GET", "/healthz", nil, nil)
	if !strings.Contains(rec.Body.String(), `"persistence":"noop"`) {
		t.Fatalf("healthz missing persistence: %s", rec.Body.String())
	}
}

// /v1/inventory is the scheduler's one-poll picture: instance capacity plus
// every local bed — in-memory ones and luggage (evicted, dir kept).
func TestInventoryEndpoint(t *testing.T) {
	s := newTestServer(t)
	s.mgr.SetLuggageLimits(1000, 800)

	rec := do(t, s, "POST", "/v1/beds", strings.NewReader(`{"id":"inv-live"}`), map[string]string{"Content-Type": "application/json"})
	if rec.Code != 200 {
		t.Fatalf("create live = %d", rec.Code)
	}
	rec = do(t, s, "POST", "/v1/beds", strings.NewReader(`{"id":"inv-cold"}`), map[string]string{"Content-Type": "application/json"})
	if rec.Code != 200 {
		t.Fatalf("create cold = %d", rec.Code)
	}
	if rec = do(t, s, "DELETE", "/v1/beds/inv-cold", nil, nil); rec.Code != 200 {
		t.Fatalf("evict cold = %d", rec.Code)
	}

	rec = do(t, s, "GET", "/v1/inventory", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("inventory = %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Instance struct {
			Store            string `json:"store"`
			MaxBeds          int    `json:"max_beds"`
			ActiveBeds       int    `json:"active_beds"`
			LuggageHighBytes int64  `json:"luggage_high_bytes"`
		} `json:"instance"`
		Beds []struct {
			ID    string `json:"id"`
			State string `json:"state"`
		} `json:"beds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Instance.Store != "noop" || body.Instance.ActiveBeds != 1 || body.Instance.LuggageHighBytes != 1000 {
		t.Fatalf("instance = %+v", body.Instance)
	}
	states := map[string]string{}
	for _, b := range body.Beds {
		states[b.ID] = b.State
	}
	if states["inv-live"] != "active" || states["inv-cold"] != "luggage" {
		t.Fatalf("bed states = %v, want inv-live active / inv-cold luggage", states)
	}
}

func TestDeleteEvictVsPurge(t *testing.T) {
	s := newTestServer(t)
	// Create, then default DELETE = evict (noop store: no snapshot, but 200).
	rec := do(t, s, "POST", "/v1/beds", strings.NewReader(`{"id":"lifecycle"}`), map[string]string{"Content-Type": "application/json"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"state":"active"`) {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	rec = do(t, s, "DELETE", "/v1/beds/lifecycle", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("evict = %d %s", rec.Code, rec.Body.String())
	}
	// Purge an absent bed is idempotent (snapshot delete of missing key is OK).
	rec = do(t, s, "DELETE", "/v1/beds/lifecycle?purge=true", nil, nil)
	if rec.Code != 200 {
		t.Fatalf("purge = %d %s", rec.Code, rec.Body.String())
	}
	// Default bed refuses purge.
	rec = do(t, s, "DELETE", "/v1/beds/default?purge=true", nil, nil)
	if rec.Code != 400 {
		t.Fatalf("purge default = %d (want 400 client error)", rec.Code)
	}
}
