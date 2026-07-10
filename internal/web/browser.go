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
	"log"
	"net"
	"net/http"
	"net/url"

	"github.com/gin-gonic/gin"
	"github.com/gobwas/ws"

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

// GET /v1/beds/:bedId/browser/info — per-bed CDP endpoint (execd-compatible
// envelope). The control plane fetches this and injects the cdp_url into the
// bed as PLAYWRIGHT_MCP_CDP_ENDPOINT; the bed's playwright then connectOverCDP
// to a proxied socket that shows only this bed's slice of the shared browser.
// The url is bed-local (127.0.0.1): the bed shares the pod net ns with hostel.
func (s *Server) browserInfo(c *gin.Context) {
	b, br := s.browserOf(c)
	if br == nil {
		return
	}
	token, err := br.CDPToken(b.ID)
	if err != nil {
		runtimeError(c, err.Error())
		return
	}
	// Reuse the port the request came in on; host is always loopback for the bed.
	_, port := splitHostPort(c.Request.Host)
	cdpURL := (&url.URL{
		Scheme:   "ws",
		Host:     "127.0.0.1:" + port,
		Path:     "/v1/cdp",
		RawQuery: url.Values{"bed": {b.ID}, "t": {token}}.Encode(),
	}).String()
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    gin.H{"cdp_url": cdpURL},
	})
}

// GET /v1/cdp?bed=&t= — websocket upgrade → per-bed CDP proxy. Not bed-scoped
// by path: playwright connectOverCDP passes the full url (incl. query) and
// can't set headers, so the bed id + token ride the query. ServeCDP
// authenticates the token against the bed-level secret, so a guessed bed/token
// is refused there.
func (s *Server) browserCDP(c *gin.Context) {
	a := s.mgr.Amenities().Find("chromium")
	br, ok := a.(amenity.Browser)
	if a == nil || !ok {
		respondError(c, http.StatusServiceUnavailable, ErrServiceUnavailable, "browser amenity unavailable")
		return
	}
	bedID := c.Query("bed")
	token := c.Query("t")
	if bedID == "" || token == "" {
		badRequest(c, "missing bed or token")
		return
	}
	// Resolve before upgrading: ServeCDP ensures the tenant at dial time (the
	// lazy browser boot point) and needs the bed's workspace for that create.
	b, err := s.mgr.Resolve(bedID)
	if err != nil {
		respondBedError(c, err)
		return
	}
	conn, _, _, err := ws.UpgradeHTTP(c.Request, c.Writer)
	if err != nil {
		// Response already partially written by the upgrader on failure.
		log.Printf("hostel: cdp ws upgrade for bed=%s failed: %v", bedID, err)
		return
	}
	if err := br.ServeCDP(conn, b.ID, b.Workspace, token); err != nil {
		log.Printf("hostel: cdp proxy for bed=%s ended: %v", bedID, err)
	}
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

// splitHostPort returns host and port from a "host:port" (port "" if absent).
// net.SplitHostPort errors on a bare host; we tolerate that.
func splitHostPort(hostport string) (host, port string) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return h, p
	}
	return hostport, ""
}
