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
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// StreamEventType matches execd's ServerStreamEvent.type enum.
type StreamEventType string

const (
	EventInit     StreamEventType = "init"
	EventStatus   StreamEventType = "status"
	EventError    StreamEventType = "error"
	EventStdout   StreamEventType = "stdout"
	EventStderr   StreamEventType = "stderr"
	EventComplete StreamEventType = "execution_complete"
)

// StreamEvent is one SSE frame payload (JSON), shaped like execd's
// ServerStreamEvent so SDK stream parsers work unchanged.
type StreamEvent struct {
	Type          StreamEventType `json:"type,omitempty"`
	Text          string          `json:"text,omitempty"`
	ExecutionTime int64           `json:"execution_time,omitempty"`
	Timestamp     int64           `json:"timestamp,omitempty"`
	Error         string          `json:"error,omitempty"`
	ExitCode      *int            `json:"exit_code,omitempty"`
}

// sseStream owns an SSE response: sets headers once, writes framed events.
type sseStream struct {
	c       *gin.Context
	started bool
}

func newSSE(c *gin.Context) *sseStream { return &sseStream{c: c} }

var sseHeaders = map[string]string{
	"Content-Type":      "text/event-stream",
	"Cache-Control":     "no-cache",
	"Connection":        "keep-alive",
	"X-Accel-Buffering": "no",
}

func (s *sseStream) setup() {
	if s.started {
		return
	}
	for k, v := range sseHeaders {
		s.c.Writer.Header().Set(k, v)
	}
	s.c.Writer.WriteHeader(http.StatusOK)
	s.flush()
	s.started = true
}

// send writes one event as `<json>\n\n` and flushes.
func (s *sseStream) send(ev StreamEvent) {
	s.setup()
	if ev.Timestamp == 0 {
		ev.Timestamp = time.Now().UnixMilli()
	}
	b, _ := json.Marshal(ev)
	b = append(b, '\n', '\n')
	_, _ = s.c.Writer.Write(b)
	s.flush()
}

func (s *sseStream) flush() {
	if f, ok := s.c.Writer.(http.Flusher); ok {
		f.Flush()
	}
}

// started reports whether any SSE header/frame has been committed — callers use
// it to decide between a JSON error (nothing sent yet) and an error event.
func (s *sseStream) hasStarted() bool { return s.started }
