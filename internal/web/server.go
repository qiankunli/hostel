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

// Package web is hostel's HTTP layer (gin): a thin adapter that maps
// OpenSandbox-compatible routes onto the framework-agnostic bed/fsops/shell
// core. Bed selection is by the X-Hostel-Bed header (or ?bed=), defaulting to
// the configured default bed so callers can ignore beds entirely.
package web

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/qiankunli/hostel/internal/bed"
	"github.com/qiankunli/hostel/internal/fsops"
)

// respondBedError maps bed-resolution failures: a full instance is 429
// backpressure (scheduler should place elsewhere), anything else is a bad id.
func respondBedError(c *gin.Context, err error) {
	if errors.Is(err, bed.ErrBedLimit) {
		respondError(c, http.StatusTooManyRequests, ErrBedLimitExceeded, err.Error())
		return
	}
	respondError(c, http.StatusBadRequest, ErrBedInvalid, err.Error())
}

// BedHeader carries the target bed id; empty → default bed.
const BedHeader = "X-Hostel-Bed"

// Server wires the bed manager into gin routes.
type Server struct {
	mgr    *bed.Manager
	engine *gin.Engine
}

// NewServer builds the gin engine with all routes registered.
func NewServer(mgr *bed.Manager) *Server {
	gin.SetMode(gin.ReleaseMode)
	e := gin.New()
	e.Use(gin.Recovery())
	s := &Server{mgr: mgr, engine: e}
	s.routes()
	return s
}

// Handler exposes the engine for http.Server / tests.
func (s *Server) Handler() http.Handler { return s.engine }

func (s *Server) routes() {
	e := s.engine
	e.GET("/ping", func(c *gin.Context) { c.String(http.StatusOK, "pong") })
	e.GET("/healthz", s.healthz)

	files := e.Group("/files")
	{
		files.GET("/info", s.filesInfo)
		files.DELETE("", s.filesDelete)
		files.POST("/mv", s.filesRename)
		files.POST("/permissions", s.filesChmod)
		files.GET("/search", s.filesSearch)
		files.POST("/replace", s.filesReplace)
		files.POST("/upload", s.filesUpload)
		files.GET("/download", s.filesDownload)
	}

	dirs := e.Group("/directories")
	{
		dirs.GET("/list", s.dirList)
		dirs.POST("", s.dirCreate)
		dirs.DELETE("", s.dirDelete)
	}

	e.POST("/command", s.runCommand)
	e.DELETE("/command", s.interruptCommand)
	e.GET("/command/status/:id", s.commandStatus)
	e.GET("/command/:id/logs", s.commandLogs)

	sess := e.Group("/session")
	{
		sess.POST("", s.sessionCreate)
		sess.POST("/:sessionId/run", s.sessionRun)
		sess.DELETE("/:sessionId", s.sessionDelete)
	}

	v1 := e.Group("/v1/beds")
	{
		v1.GET("", s.bedList)
		v1.POST("", s.bedCreate)
		v1.GET("/capabilities", s.capabilities)
		v1.GET("/:bedId", s.bedGet)
		v1.DELETE("/:bedId", s.bedDelete)
		v1.POST("/:bedId/checkpoint", s.bedCheckpoint)
		// Browser amenity verbs (docs/amenity.md §2) — bed-scoped actions,
		// never a raw CDP passthrough.
		v1.POST("/:bedId/browser/goto", s.browserGoto)
		v1.POST("/:bedId/browser/screenshot", s.browserScreenshot)
		v1.POST("/:bedId/browser/text", s.browserText)
		v1.POST("/:bedId/browser/close", s.browserClose)
	}
}

// bedOf resolves the target bed from the request (header/query → default),
// creating it on first use. On invalid id it writes an error and returns nil.
func (s *Server) bedOf(c *gin.Context) *bed.Bed {
	id := c.GetHeader(BedHeader)
	if id == "" {
		id = c.Query("bed")
	}
	b, err := s.mgr.Resolve(id)
	if err != nil {
		respondBedError(c, err)
		return nil
	}
	return b
}

// opsOf returns filesystem ops rooted at the request's bed (or nil after error).
func (s *Server) opsOf(c *gin.Context) (*bed.Bed, *fsops.Ops) {
	b := s.bedOf(c)
	if b == nil {
		return nil, nil
	}
	return b, fsops.New(b.Workspace)
}

func (s *Server) healthz(c *gin.Context) {
	iso := s.mgr.Isolator()
	c.JSON(http.StatusOK, gin.H{
		"ok":              true,
		"isolator":        iso.Name(),
		"isolator_ok":     iso.Available(),
		"workspace_mount": iso.MountPoint() != "",
		"beds":            len(s.mgr.List()),
		"max_beds":        s.mgr.MaxBeds(),
		"persistence":     s.mgr.StoreName(),
		"default_bed":     s.mgr.DefaultBedID(),
	})
}
