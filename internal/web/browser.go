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
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/qiankunli/hostel/internal/amenity"
	"github.com/qiankunli/hostel/internal/bed"
)

// browserOf resolves the bed from the :bedId path param and the Browser
// amenity, writing the error response when either is missing. The raw CDP
// socket is never exposed — only these bed-scoped verbs (docs/amenity.md §2).
func (s *Server) browserOf(c *gin.Context) (*bed.Bed, amenity.Browser) {
	a := s.mgr.Amenities().Find("chromium")
	br, ok := a.(amenity.Browser)
	if a == nil || !ok {
		respondError(c, http.StatusServiceUnavailable, ErrServiceUnavailable,
			"browser amenity is not available on this hostel (no chromium binary or CDP endpoint)")
		return nil, nil
	}
	b, err := s.mgr.Resolve(c.Param("bedId"))
	if err != nil {
		respondBedError(c, err)
		return nil, nil
	}
	return b, br
}

// POST /v1/beds/:bedId/browser/goto {url}
func (s *Server) browserGoto(c *gin.Context) {
	b, br := s.browserOf(c)
	if br == nil {
		return
	}
	var req struct {
		URL string `json:"url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.URL == "" {
		badRequest(c, "missing 'url'")
		return
	}
	title, loc, err := br.Goto(c.Request.Context(), b.ID, b.Workspace, req.URL)
	if err != nil {
		runtimeError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"title": title, "url": loc})
}

// POST /v1/beds/:bedId/browser/screenshot {path?}
func (s *Server) browserScreenshot(c *gin.Context) {
	b, br := s.browserOf(c)
	if br == nil {
		return
	}
	var req struct {
		Path string `json:"path,omitempty"`
	}
	_ = c.ShouldBindJSON(&req)
	saved, err := br.Screenshot(c.Request.Context(), b.ID, b.Workspace, req.Path)
	if err != nil {
		runtimeError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"path": saved})
}

// POST /v1/beds/:bedId/browser/text
func (s *Server) browserText(c *gin.Context) {
	b, br := s.browserOf(c)
	if br == nil {
		return
	}
	text, err := br.Text(c.Request.Context(), b.ID, b.Workspace)
	if err != nil {
		runtimeError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"text": text})
}

// POST /v1/beds/:bedId/browser/close — release this bed's browser context.
func (s *Server) browserClose(c *gin.Context) {
	b, br := s.browserOf(c)
	if br == nil {
		return
	}
	if err := br.ReleaseTenant(b.ID); err != nil {
		runtimeError(c, err.Error())
		return
	}
	c.Status(http.StatusOK)
}

// POST /v1/beds/:bedId/browser/click {selector}
func (s *Server) browserClick(c *gin.Context) {
	b, br := s.browserOf(c)
	if br == nil {
		return
	}
	var req struct {
		Selector string `json:"selector"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Selector == "" {
		badRequest(c, "missing 'selector'")
		return
	}
	if err := br.Click(c.Request.Context(), b.ID, b.Workspace, req.Selector); err != nil {
		runtimeError(c, err.Error())
		return
	}
	c.Status(http.StatusOK)
}

// POST /v1/beds/:bedId/browser/type {selector, text, clear?}
func (s *Server) browserType(c *gin.Context) {
	b, br := s.browserOf(c)
	if br == nil {
		return
	}
	var req struct {
		Selector string `json:"selector"`
		Text     string `json:"text"`
		Clear    bool   `json:"clear,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Selector == "" {
		badRequest(c, "missing 'selector'")
		return
	}
	if err := br.Type(c.Request.Context(), b.ID, b.Workspace, req.Selector, req.Text, req.Clear); err != nil {
		runtimeError(c, err.Error())
		return
	}
	c.Status(http.StatusOK)
}

// POST /v1/beds/:bedId/browser/press {key}
func (s *Server) browserPress(c *gin.Context) {
	b, br := s.browserOf(c)
	if br == nil {
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Key == "" {
		badRequest(c, "missing 'key'")
		return
	}
	if err := br.Press(c.Request.Context(), b.ID, b.Workspace, req.Key); err != nil {
		runtimeError(c, err.Error())
		return
	}
	c.Status(http.StatusOK)
}

// POST /v1/beds/:bedId/browser/scroll {dx?, dy?}
func (s *Server) browserScroll(c *gin.Context) {
	b, br := s.browserOf(c)
	if br == nil {
		return
	}
	var req struct {
		DX int `json:"dx,omitempty"`
		DY int `json:"dy,omitempty"`
	}
	_ = c.ShouldBindJSON(&req)
	if err := br.Scroll(c.Request.Context(), b.ID, b.Workspace, req.DX, req.DY); err != nil {
		runtimeError(c, err.Error())
		return
	}
	c.Status(http.StatusOK)
}

// POST /v1/beds/:bedId/browser/wait {selector}
func (s *Server) browserWait(c *gin.Context) {
	b, br := s.browserOf(c)
	if br == nil {
		return
	}
	var req struct {
		Selector string `json:"selector"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.Selector == "" {
		badRequest(c, "missing 'selector'")
		return
	}
	if err := br.Wait(c.Request.Context(), b.ID, b.Workspace, req.Selector); err != nil {
		runtimeError(c, err.Error())
		return
	}
	c.Status(http.StatusOK)
}
