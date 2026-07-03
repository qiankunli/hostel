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

// GET /v1/inventory — the scheduler-facing snapshot: capacity plus every bed
// this instance holds (active/evicting in memory, luggage on disk) with its
// last persisted generation. Everything here is a stale-tolerant hint —
// freshness is re-enforced at activation, so routing on outdated inventory
// is slow, never wrong. Callers must treat store "noop" as "beds are pinned
// here": no snapshot exists elsewhere to migrate from.
func (s *Server) inventory(c *gin.Context) {
	beds := s.mgr.Inventory()
	active := 0
	var luggageBytes int64
	for _, b := range beds {
		if b.State == "luggage" {
			luggageBytes += b.Bytes
		} else {
			active++
		}
	}
	high, low := s.mgr.LuggageLimits()
	c.JSON(http.StatusOK, gin.H{
		"instance": gin.H{
			"store":              s.mgr.StoreName(),
			"isolation":          s.mgr.Isolator().Level().String(),
			"max_beds":           s.mgr.MaxBeds(),
			"active_beds":        active,
			"luggage_bytes":      luggageBytes,
			"luggage_high_bytes": high,
			"luggage_low_bytes":  low,
		},
		"beds": beds,
	})
}
