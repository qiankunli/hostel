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
	"strings"
	"testing"

	"github.com/qiankunli/hostel/internal/bed"
	"github.com/qiankunli/hostel/internal/isolation"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	mgr, err := bed.NewManager(root, "default", "/bin/bash", isolation.New("direct", root), nil)
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
	if h["isolator"] != "direct" || h["ok"] != true {
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

	// download returns the bytes.
	rec = do(t, s, "GET", "/files/download?path=/workspace/hi.txt", nil, nil)
	if rec.Code != 200 || rec.Body.String() != "hello hostel" {
		t.Fatalf("download = %d %q", rec.Code, rec.Body.String())
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
	up("alice", "/workspace/secret.txt", "alice-data")

	// bob's bed must NOT see alice's file.
	rec := do(t, s, "GET", "/files/download?path=/workspace/secret.txt", nil, map[string]string{BedHeader: "bob"})
	if rec.Code != 404 {
		t.Fatalf("bob reading alice's file = %d (want 404)", rec.Code)
	}
	// alice still sees her own.
	rec = do(t, s, "GET", "/files/download?path=/workspace/secret.txt", nil, map[string]string{BedHeader: "alice"})
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
