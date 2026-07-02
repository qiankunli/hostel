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
	"errors"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/qiankunli/hostel/internal/bed"
)

func randomHex() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// bedView is the JSON shape for a bed in the management API.
type bedView struct {
	ID        string    `json:"id"`
	State     string    `json:"state"` // active | evicting (dormant beds aren't listed)
	Workspace string    `json:"workspace"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used"`
}

// GET /v1/beds
func (s *Server) bedList(c *gin.Context) {
	beds := s.mgr.List()
	out := make([]bedView, 0, len(beds))
	for _, b := range beds {
		out = append(out, bedView{ID: b.ID, State: b.State(), Workspace: b.Workspace, CreatedAt: b.CreatedAt, LastUsed: b.LastUsed()})
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
	c.JSON(http.StatusOK, bedView{ID: b.ID, State: b.State(), Workspace: b.Workspace, CreatedAt: b.CreatedAt, LastUsed: b.LastUsed()})
}

// GET /v1/beds/:bedId
func (s *Server) bedGet(c *gin.Context) {
	b, ok := s.mgr.Get(c.Param("bedId"))
	if !ok {
		respondError(c, http.StatusNotFound, ErrBedInvalid, "bed not found")
		return
	}
	c.JSON(http.StatusOK, bedView{ID: b.ID, State: b.State(), Workspace: b.Workspace, CreatedAt: b.CreatedAt, LastUsed: b.LastUsed()})
}

// DELETE /v1/beds/:bedId — evict by default (persist, release compute, keep
// the snapshot identity); ?purge=true ends the identity (snapshot deleted
// too). An evict canceled by concurrent bed activity returns 409 BED_BUSY —
// stop sending traffic, then retry.
func (s *Server) bedDelete(c *gin.Context) {
	id := c.Param("bedId")
	if c.Query("purge") == "true" {
		if err := s.mgr.Purge(id); err != nil {
			if errors.Is(err, bed.ErrPurgeDefault) {
				badRequest(c, err.Error())
				return
			}
			runtimeError(c, err.Error())
			return
		}
		c.Status(http.StatusOK)
		return
	}
	evicted, err := s.mgr.Evict(id)
	if err != nil {
		runtimeError(c, err.Error())
		return
	}
	if !evicted {
		if _, ok := s.mgr.Get(id); ok {
			respondError(c, http.StatusConflict, ErrBedBusy, "bed saw activity during eviction; retry after traffic stops")
			return
		}
		// Not ACTIVE at all — idempotent delete.
	}
	c.Status(http.StatusOK)
}

// GET /v1/beds/capabilities — what this hostel can do (SDK feature detection).
func (s *Server) capabilities(c *gin.Context) {
	iso := s.mgr.Isolator()
	amenities := map[string]string{} // name → lifecycle state
	for _, a := range s.mgr.Amenities().List() {
		amenities[a.Name()] = a.State()
	}
	c.JSON(http.StatusOK, gin.H{
		"isolator":    iso.Name(),
		"isolator_ok": iso.Available(),
		// True when the bed workspace is mounted at the canonical /workspace
		// inside the sandbox (bwrap): shell paths == file-API paths. False
		// under direct, where /workspace is only the file-API virtual prefix.
		"workspace_mount": iso.MountPoint() != "",
		"max_beds":        s.mgr.MaxBeds(),
		"persistence":     s.mgr.StoreName(),
		"files":           true,
		"directories":     true,
		"command":         true,
		"session":         true,
		"beds":            true,
		"amenities":       amenities, // name → unavailable|idle|running
		// Explicitly-not-yet capabilities, so SDKs don't probe blindly.
		"pty":            false,
		"code":           false,
		"overlay_commit": false,
	})
}

// POST /v1/beds/:bedId/checkpoint — snapshot the bed's workspace now, without
// tearing it down. 200 with the persistence backend on success.
func (s *Server) bedCheckpoint(c *gin.Context) {
	id := c.Param("bedId")
	if err := s.mgr.Checkpoint(c.Request.Context(), id); err != nil {
		runtimeError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"persistence": s.mgr.StoreName()})
}
