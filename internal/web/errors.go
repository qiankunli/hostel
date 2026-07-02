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
)

// ErrorCode mirrors the OpenSandbox execd error vocabulary so SDK error
// handling works against hostel unchanged.
type ErrorCode string

const (
	ErrInvalidRequest     ErrorCode = "INVALID_REQUEST_BODY"
	ErrMissingQuery       ErrorCode = "MISSING_QUERY"
	ErrRuntimeError       ErrorCode = "RUNTIME_ERROR"
	ErrFileNotFound       ErrorCode = "FILE_NOT_FOUND"
	ErrNotSupported       ErrorCode = "NOT_SUPPORTED"
	ErrSessionNotFound    ErrorCode = "SESSION_NOT_FOUND"
	ErrCommandNotFound    ErrorCode = "COMMAND_NOT_FOUND"
	ErrBedInvalid         ErrorCode = "BED_INVALID"
	ErrBedLimitExceeded   ErrorCode = "BED_LIMIT_EXCEEDED"
	ErrBedBusy            ErrorCode = "BED_BUSY"
	ErrServiceUnavailable ErrorCode = "SERVICE_UNAVAILABLE"
)

// ErrorResponse is the JSON error envelope (matches execd).
type ErrorResponse struct {
	Code    ErrorCode `json:"code,omitempty"`
	Message string    `json:"message,omitempty"`
}

func respondError(c *gin.Context, status int, code ErrorCode, msg string) {
	c.JSON(status, ErrorResponse{Code: code, Message: msg})
}

func badRequest(c *gin.Context, msg string) {
	respondError(c, http.StatusBadRequest, ErrInvalidRequest, msg)
}

func runtimeError(c *gin.Context, msg string) {
	respondError(c, http.StatusInternalServerError, ErrRuntimeError, msg)
}
