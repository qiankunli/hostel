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
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

func randomHex() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// bedView is the JSON shape for a bed in the management API.
type bedView struct {
	ID        string    `json:"id"`
	Workspace string    `json:"workspace"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used"`
}

// GET /v1/beds
func (s *Server) bedList(c *gin.Context) {
	beds := s.mgr.List()
	out := make([]bedView, 0, len(beds))
	for _, b := range beds {
		out = append(out, bedView{ID: b.ID, Workspace: b.Workspace, CreatedAt: b.CreatedAt, LastUsed: b.LastUsed()})
	}
	c.JSON(http.StatusOK, gin.H{"beds": out})
}

type createBedRequest struct {
	ID string `json:"id,omitempty"`
}

// POST /v1/beds — create (or return existing) a bed. Empty id → server-assigned.
func (s *Server) bedCreate(c *gin.Context) {
	var req createBedRequest
	_ = c.ShouldBindJSON(&req)
	id := req.ID
	if id == "" {
		id = "bed-" + randomHex()
	}
	b, err := s.mgr.Resolve(id)
	if err != nil {
		respondBedError(c, err)
		return
	}
	c.JSON(http.StatusOK, bedView{ID: b.ID, Workspace: b.Workspace, CreatedAt: b.CreatedAt, LastUsed: b.LastUsed()})
}

// GET /v1/beds/:bedId
func (s *Server) bedGet(c *gin.Context) {
	b, ok := s.mgr.Get(c.Param("bedId"))
	if !ok {
		respondError(c, http.StatusNotFound, ErrBedInvalid, "bed not found")
		return
	}
	c.JSON(http.StatusOK, bedView{ID: b.ID, Workspace: b.Workspace, CreatedAt: b.CreatedAt, LastUsed: b.LastUsed()})
}

// DELETE /v1/beds/:bedId
func (s *Server) bedDelete(c *gin.Context) {
	if err := s.mgr.Delete(c.Param("bedId")); err != nil {
		runtimeError(c, err.Error())
		return
	}
	c.Status(http.StatusOK)
}

// GET /v1/beds/capabilities — what this hostel can do (SDK feature detection).
func (s *Server) capabilities(c *gin.Context) {
	iso := s.mgr.Isolator()
	svcNames := []string{}
	for _, sv := range s.mgr.Services().List() {
		svcNames = append(svcNames, sv.Name())
	}
	c.JSON(http.StatusOK, gin.H{
		"isolator":    iso.Name(),
		"isolator_ok": iso.Available(),
		// True when the bed workspace is mounted at the canonical /workspace
		// inside the sandbox (bwrap): shell paths == file-API paths. False
		// under direct, where /workspace is only the file-API virtual prefix.
		"workspace_mount":  iso.MountPoint() != "",
		"max_beds":         s.mgr.MaxBeds(),
		"files":            true,
		"directories":      true,
		"command":          true,
		"session":          true,
		"beds":             true,
		"managed_services": svcNames, // empty in v1
		// Explicitly-not-yet capabilities, so SDKs don't probe blindly.
		"pty":            false,
		"code":           false,
		"overlay_commit": false,
	})
}
