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
	"context"
	"encoding/json"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/qiankunli/go-stdx/shellx"

	"github.com/qiankunli/hostel/internal/bed"
	"github.com/qiankunli/hostel/internal/fsops"
)

func jsonUnmarshal(s string, v any) error { return json.Unmarshal([]byte(s), v) }

// RunCommandRequest mirrors execd's shape.
type RunCommandRequest struct {
	Command    string            `json:"command"`
	Cwd        string            `json:"cwd,omitempty"`
	Background bool              `json:"background,omitempty"`
	TimeoutMs  int64             `json:"timeout,omitempty"`
	Envs       map[string]string `json:"envs,omitempty"`
}

// resolveCwd maps a client cwd (virtual /workspace path) to the directory the
// bed's SHELL should cd into, or "" when unset. Under direct that's the host
// dir; under bwrap the workspace is mounted at the canonical mount point, so
// the shell needs the in-sandbox path (the host dir doesn't exist in its mount
// namespace). Validation (escape rejection) is fsops.Resolve either way.
// Returns false (after writing an error) on an invalid path.
func (s *Server) resolveCwd(c *gin.Context, b *bed.Bed, ops *fsops.Ops, cwd string) (string, bool) {
	if cwd == "" {
		return "", true
	}
	host, err := ops.Resolve(cwd)
	if err != nil {
		badRequest(c, err.Error())
		return "", false
	}
	mp := s.mgr.Isolator().MountPoint()
	if mp == "" {
		return host, true
	}
	rel, err := filepath.Rel(b.Workspace, host)
	if err != nil || strings.HasPrefix(rel, "..") {
		// Unreachable after ops.Resolve confinement; refuse rather than guess.
		badRequest(c, "cwd outside the bed workspace")
		return "", false
	}
	return path.Join(mp, filepath.ToSlash(rel)), true
}

// POST /command — SSE stream. Foreground runs in the bed's shared stateful
// shell; background detaches and returns an init event with the command id.
func (s *Server) runCommand(c *gin.Context) {
	b, ops := s.opsOf(c)
	if ops == nil {
		return
	}
	var req RunCommandRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if req.Command == "" {
		badRequest(c, "missing 'command'")
		return
	}
	hostCwd, ok := s.resolveCwd(c, b, ops, req.Cwd)
	if !ok {
		return
	}
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond

	if req.Background {
		cmd, err := s.mgr.StartCommand(b, req.Command, hostCwd, req.Envs, timeout, nil)
		if err != nil {
			runtimeError(c, err.Error())
			return
		}
		sse := newSSE(c)
		sse.send(StreamEvent{Type: EventInit, Text: cmd.ID})
		sse.send(StreamEvent{Type: EventComplete})
		return
	}

	// Foreground: stateful shell run, streamed live.
	sh, err := s.mgr.ForegroundShell(b)
	if err != nil {
		runtimeError(c, err.Error())
		return
	}
	sse := newSSE(c)
	start := time.Now()
	res, err := sh.Run(c.Request.Context(), wrapWithCwd(req.Command, hostCwd, req.Envs), func(line string) {
		sse.send(StreamEvent{Type: EventStdout, Text: line})
	})
	b.RecordCommand(time.Since(start))
	if err != nil {
		if sse.hasStarted() {
			sse.send(StreamEvent{Type: EventError, Error: err.Error()})
		} else {
			runtimeError(c, err.Error())
		}
		return
	}
	sse.send(StreamEvent{
		Type:          EventComplete,
		ExecutionTime: time.Since(start).Milliseconds(),
		ExitCode:      &res.ExitCode,
	})
}

// wrapWithCwd prefixes a subshell cd + env exports so a foreground command runs
// with the requested cwd/env without permanently mutating the shared shell.
func wrapWithCwd(command, hostCwd string, envs map[string]string) string {
	prefix := ""
	for k, v := range envs {
		prefix += "export " + k + "=" + shellx.Quote(v) + "; "
	}
	if hostCwd != "" {
		prefix += "cd -- " + shellx.Quote(hostCwd) + " && "
	}
	if prefix == "" {
		return command
	}
	// Group so the prefix applies only to this command line.
	return prefix + "{ " + command + " ; }"
}

// DELETE /command?id=... — interrupt a (background) command.
func (s *Server) interruptCommand(c *gin.Context) {
	id := c.Query("id")
	if id == "" {
		respondError(c, http.StatusBadRequest, ErrMissingQuery, "missing query parameter 'id'")
		return
	}
	cmd, ok := s.mgr.Commands().Get(id)
	if !ok {
		respondError(c, http.StatusNotFound, ErrCommandNotFound, "command not found: "+id)
		return
	}
	cmd.Interrupt()
	c.Status(http.StatusOK)
}

// GET /command/status/:id
func (s *Server) commandStatus(c *gin.Context) {
	cmd, ok := s.mgr.Commands().Get(c.Param("id"))
	if !ok {
		respondError(c, http.StatusNotFound, ErrCommandNotFound, "command not found")
		return
	}
	st := cmd.Status()
	c.JSON(http.StatusOK, gin.H{
		"id":          st.ID,
		"content":     st.Content,
		"running":     st.Running,
		"exit_code":   st.ExitCode,
		"error":       st.Err,
		"started_at":  st.StartedAt,
		"finished_at": st.FinishedAt,
	})
}

// GET /command/:id/logs?cursor=N — plain text; next cursor in header.
func (s *Server) commandLogs(c *gin.Context) {
	id := c.Param("id")
	cursor := int64(-1)
	if q := c.Query("cursor"); q != "" {
		if n, err := strconv.ParseInt(q, 10, 64); err == nil {
			cursor = n
		}
	}
	content, next, running, err := s.mgr.Commands().Logs(id, cursor)
	if err != nil {
		respondError(c, http.StatusNotFound, ErrCommandNotFound, err.Error())
		return
	}
	c.Header("EXECD-COMMANDS-TAIL-CURSOR", strconv.FormatInt(next, 10))
	c.Header("EXECD-COMMANDS-RUNNING", strconv.FormatBool(running))
	c.String(http.StatusOK, content)
}

// --- /session: explicit stateful bash sessions ---

type createSessionRequest struct {
	Cwd string `json:"cwd,omitempty"`
}
type runInSessionRequest struct {
	Command string `json:"command"`
	Cwd     string `json:"cwd,omitempty"`
	Timeout int64  `json:"timeout,omitempty"`
}

// POST /session
func (s *Server) sessionCreate(c *gin.Context) {
	b, ops := s.opsOf(c)
	if ops == nil {
		return
	}
	var req createSessionRequest
	_ = c.ShouldBindJSON(&req)
	hostCwd, ok := s.resolveCwd(c, b, ops, req.Cwd)
	if !ok {
		return
	}
	id, err := s.mgr.CreateShell(b, hostCwd)
	if err != nil {
		runtimeError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"session_id": id})
}

// POST /session/:sessionId/run — SSE stream.
func (s *Server) sessionRun(c *gin.Context) {
	b, ops := s.opsOf(c)
	if ops == nil {
		return
	}
	sh, ok := b.GetShell(c.Param("sessionId"))
	if !ok {
		respondError(c, http.StatusNotFound, ErrSessionNotFound, "session not found")
		return
	}
	var req runInSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		badRequest(c, err.Error())
		return
	}
	if req.Command == "" {
		badRequest(c, "missing 'command'")
		return
	}
	hostCwd, ok := s.resolveCwd(c, b, ops, req.Cwd)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.Timeout)*time.Millisecond)
		defer cancel()
	}
	sse := newSSE(c)
	start := time.Now()
	res, err := sh.Run(ctx, wrapWithCwd(req.Command, hostCwd, nil), func(line string) {
		sse.send(StreamEvent{Type: EventStdout, Text: line})
	})
	b.RecordCommand(time.Since(start))
	if err != nil {
		if sse.hasStarted() {
			sse.send(StreamEvent{Type: EventError, Error: err.Error()})
		} else {
			runtimeError(c, err.Error())
		}
		return
	}
	sse.send(StreamEvent{Type: EventComplete, ExecutionTime: time.Since(start).Milliseconds(), ExitCode: &res.ExitCode})
}

// DELETE /session/:sessionId
func (s *Server) sessionDelete(c *gin.Context) {
	b := s.bedOf(c)
	if b == nil {
		return
	}
	if !b.DeleteShell(c.Param("sessionId")) {
		respondError(c, http.StatusNotFound, ErrSessionNotFound, "session not found")
		return
	}
	c.Status(http.StatusOK)
}
