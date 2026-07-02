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
	"io"
	"net/http"
	"os"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/qiankunli/hostel/internal/fsops"
)

// GET /files/info?path=...(&path=...) → map[path]FileInfo
func (s *Server) filesInfo(c *gin.Context) {
	_, ops := s.opsOf(c)
	if ops == nil {
		return
	}
	paths := c.QueryArray("path")
	if len(paths) == 0 {
		respondError(c, http.StatusBadRequest, ErrMissingQuery, "missing query parameter 'path'")
		return
	}
	out := make(map[string]fsops.FileInfo, len(paths))
	for _, p := range paths {
		fi, err := ops.Stat(p)
		if err != nil {
			if os.IsNotExist(err) {
				respondError(c, http.StatusNotFound, ErrFileNotFound, err.Error())
				return
			}
			runtimeError(c, err.Error())
			return
		}
		out[p] = fi
	}
	c.JSON(http.StatusOK, out)
}

// DELETE /files?path=...(&path=...)
func (s *Server) filesDelete(c *gin.Context) {
	_, ops := s.opsOf(c)
	if ops == nil {
		return
	}
	paths := c.QueryArray("path")
	if len(paths) == 0 {
		respondError(c, http.StatusBadRequest, ErrMissingQuery, "missing query parameter 'path'")
		return
	}
	if err := ops.Remove(paths); err != nil {
		runtimeError(c, err.Error())
		return
	}
	c.Status(http.StatusOK)
}

// POST /files/mv  body: [{src,dest}, ...]
func (s *Server) filesRename(c *gin.Context) {
	_, ops := s.opsOf(c)
	if ops == nil {
		return
	}
	var items []fsops.RenameItem
	if err := c.ShouldBindJSON(&items); err != nil {
		badRequest(c, err.Error())
		return
	}
	for _, it := range items {
		if err := ops.Rename(it.Src, it.Dest); err != nil {
			runtimeError(c, err.Error())
			return
		}
	}
	c.Status(http.StatusOK)
}

// POST /files/permissions  body: {path: {owner,group,mode}, ...}
func (s *Server) filesChmod(c *gin.Context) {
	_, ops := s.opsOf(c)
	if ops == nil {
		return
	}
	var m map[string]fsops.Permission
	if err := c.ShouldBindJSON(&m); err != nil {
		badRequest(c, err.Error())
		return
	}
	for p, perm := range m {
		if err := ops.Chmod(p, perm); err != nil {
			runtimeError(c, err.Error())
			return
		}
	}
	c.Status(http.StatusOK)
}

// GET /files/search?path=...&pattern=... → []FileInfo
func (s *Server) filesSearch(c *gin.Context) {
	_, ops := s.opsOf(c)
	if ops == nil {
		return
	}
	p := c.Query("path")
	if p == "" {
		respondError(c, http.StatusBadRequest, ErrMissingQuery, "missing query parameter 'path'")
		return
	}
	res, err := ops.Search(p, c.Query("pattern"))
	if err != nil {
		runtimeError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, res)
}

// POST /files/replace  body: {path: {old,new}, ...} → map[path]ReplaceResult
func (s *Server) filesReplace(c *gin.Context) {
	_, ops := s.opsOf(c)
	if ops == nil {
		return
	}
	var m map[string]fsops.ReplaceItem
	if err := c.ShouldBindJSON(&m); err != nil {
		badRequest(c, err.Error())
		return
	}
	out := make(map[string]fsops.ReplaceResult, len(m))
	for p, item := range m {
		res, err := ops.Replace(p, item)
		if err != nil {
			runtimeError(c, err.Error())
			return
		}
		out[p] = res
	}
	c.JSON(http.StatusOK, out)
}

// POST /files/upload  multipart: metadata (JSON {path,...}) + file (binary)
func (s *Server) filesUpload(c *gin.Context) {
	_, ops := s.opsOf(c)
	if ops == nil {
		return
	}
	// Accept either the execd shape (metadata JSON part with a "path") or a
	// simpler ?path= query, whichever is present.
	path := c.Query("path")
	if md := c.PostForm("metadata"); md != "" {
		var meta struct {
			Path string `json:"path"`
			Mode int    `json:"mode"`
		}
		if err := jsonUnmarshal(md, &meta); err == nil && meta.Path != "" {
			path = meta.Path
		}
	}
	if path == "" {
		respondError(c, http.StatusBadRequest, ErrMissingQuery, "missing file path (metadata.path or ?path=)")
		return
	}
	fh, err := c.FormFile("file")
	if err != nil {
		badRequest(c, "missing multipart 'file' part: "+err.Error())
		return
	}
	f, err := fh.Open()
	if err != nil {
		runtimeError(c, err.Error())
		return
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		runtimeError(c, err.Error())
		return
	}
	if err := ops.Write(path, data, 0); err != nil {
		runtimeError(c, err.Error())
		return
	}
	c.Status(http.StatusOK)
}

// GET /files/download?path=...&offset=&limit= → file bytes (or line slice)
func (s *Server) filesDownload(c *gin.Context) {
	_, ops := s.opsOf(c)
	if ops == nil {
		return
	}
	p := c.Query("path")
	if p == "" {
		respondError(c, http.StatusBadRequest, ErrMissingQuery, "missing query parameter 'path'")
		return
	}
	offset := c.Query("offset")
	limit := c.Query("limit")
	if offset != "" || limit != "" {
		off, _ := strconv.Atoi(offset)
		lim, _ := strconv.Atoi(limit)
		content, err := ops.ReadLines(p, off, lim)
		if err != nil {
			downloadErr(c, err)
			return
		}
		c.String(http.StatusOK, content)
		return
	}
	data, err := ops.Read(p)
	if err != nil {
		downloadErr(c, err)
		return
	}
	c.Data(http.StatusOK, "application/octet-stream", data)
}

func downloadErr(c *gin.Context, err error) {
	if os.IsNotExist(err) {
		respondError(c, http.StatusNotFound, ErrFileNotFound, err.Error())
		return
	}
	runtimeError(c, err.Error())
}

// --- directories ---

// GET /directories/list?path=...&depth= → []FileInfo
func (s *Server) dirList(c *gin.Context) {
	_, ops := s.opsOf(c)
	if ops == nil {
		return
	}
	p := c.Query("path")
	if p == "" {
		respondError(c, http.StatusBadRequest, ErrMissingQuery, "missing query parameter 'path'")
		return
	}
	depth := 1
	if d := c.Query("depth"); d != "" {
		if n, err := strconv.Atoi(d); err == nil {
			depth = n
		}
	}
	res, err := ops.List(p, depth)
	if err != nil {
		if os.IsNotExist(err) {
			respondError(c, http.StatusNotFound, ErrFileNotFound, err.Error())
			return
		}
		runtimeError(c, err.Error())
		return
	}
	c.JSON(http.StatusOK, res)
}

// POST /directories?path=...  (path also accepted in a JSON body {path})
func (s *Server) dirCreate(c *gin.Context) {
	_, ops := s.opsOf(c)
	if ops == nil {
		return
	}
	p := dirPathParam(c)
	if p == "" {
		respondError(c, http.StatusBadRequest, ErrMissingQuery, "missing 'path'")
		return
	}
	if err := ops.MakeDir(p); err != nil {
		runtimeError(c, err.Error())
		return
	}
	c.Status(http.StatusOK)
}

// DELETE /directories?path=...
func (s *Server) dirDelete(c *gin.Context) {
	_, ops := s.opsOf(c)
	if ops == nil {
		return
	}
	p := dirPathParam(c)
	if p == "" {
		respondError(c, http.StatusBadRequest, ErrMissingQuery, "missing 'path'")
		return
	}
	if err := ops.RemoveDir(p); err != nil {
		runtimeError(c, err.Error())
		return
	}
	c.Status(http.StatusOK)
}

func dirPathParam(c *gin.Context) string {
	if p := c.Query("path"); p != "" {
		return p
	}
	var body struct {
		Path string `json:"path"`
	}
	_ = c.ShouldBindJSON(&body)
	return body.Path
}
