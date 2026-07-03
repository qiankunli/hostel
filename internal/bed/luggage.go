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

package bed

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Luggage is a DORMANT bed's local dir left behind by Evict: a warm cache of
// the snapshot, so a same-instance resume skips the download. It is never the
// authoritative copy (the snapshot is — docs/persistence.md); deleting
// luggage costs at most one extra Restore. The exception is the noop store,
// where nothing else exists: there luggage GC is destruction, the price of
// the "nothing persists" world.

// gcTmpPrefix marks a luggage dir claimed by GC: renamed under the manager
// lock (atomic — a concurrent Resolve either sees the bed dir or doesn't,
// never a half-deleted one), then removed outside it. Leftovers from a crash
// are swept on the next tick. It can never collide with a bed id (validBedID
// rejects a leading dot).
const gcTmpPrefix = ".gc-"

// LuggageEntry describes one cold local copy for GC and inventory reporting.
type LuggageEntry struct {
	BedID string
	// Bytes is the dir's file size total — the disk this entry occupies.
	Bytes int64
	// Generation is the local copy's persist counter (0 when meta is missing).
	Generation int64
	// LastUsedAt orders LRU eviction: the evict-time stamp, falling back to
	// LastPersistedAt and then dir mtime for copies predating the stamp.
	LastUsedAt time.Time
	// Profile is the usage picture the bed left behind (from its meta).
	Profile Profile
}

// ListLuggage scans the workspace root for bed dirs that are not ACTIVE —
// the local copies of DORMANT beds. The default bed is never luggage (its
// dir is permanent by contract).
func (m *Manager) ListLuggage() []LuggageEntry {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil
	}
	m.mu.Lock()
	active := make(map[string]bool, len(m.beds))
	for id := range m.beds {
		active[id] = true
	}
	m.mu.Unlock()

	var out []LuggageEntry
	for _, e := range entries {
		id := e.Name()
		if !e.IsDir() || active[id] || id == m.defaultBed || validBedID(id) != nil {
			continue
		}
		dir := filepath.Join(m.root, id)
		l := LuggageEntry{BedID: id, Bytes: dirBytes(dir)}
		if meta, ok := loadMeta(dir); ok {
			l.Generation = meta.Generation
			l.LastUsedAt = meta.LastUsedAt
			l.Profile = meta.Profile
			if l.LastUsedAt.IsZero() {
				l.LastUsedAt = meta.LastPersistedAt
			}
		}
		if l.LastUsedAt.IsZero() {
			if fi, err := e.Info(); err == nil {
				l.LastUsedAt = fi.ModTime()
			}
		}
		out = append(out, l)
	}
	return out
}

// SetLuggageLimits configures the disk watermarks (bytes; high 0 = GC off).
// Call once at startup, before serving — the fields are not synchronized.
func (m *Manager) SetLuggageLimits(high, low int64) {
	m.luggageHigh, m.luggageLow = high, low
}

// LuggageLimits reports the configured watermarks for healthz/inventory.
func (m *Manager) LuggageLimits() (high, low int64) {
	return m.luggageHigh, m.luggageLow
}

// CollectLuggage enforces the luggage disk watermarks: when the total exceeds
// the high watermark, delete cold copies until under the low one. Returns
// reaped ids. Deletion order is the cost-aware eviction seam (v1: score =
// recency): stale-generation copies first — the snapshot is newer, so they
// are pure garbage — then least recently used.
func (m *Manager) CollectLuggage(ctx context.Context) []string {
	if m.luggageHigh <= 0 {
		return nil
	}
	m.sweepGCLeftovers()
	luggage := m.ListLuggage()
	var total int64
	for _, l := range luggage {
		total += l.Bytes
	}
	if total <= m.luggageHigh {
		return nil
	}
	// One Stat (HEAD) per entry, paid only on the over-watermark path.
	stale := map[string]bool{}
	for _, l := range luggage {
		if info, err := m.store.Stat(ctx, l.BedID); err == nil && info != nil && l.Generation < info.Generation {
			stale[l.BedID] = true
		}
	}
	sort.Slice(luggage, func(i, j int) bool {
		if stale[luggage[i].BedID] != stale[luggage[j].BedID] {
			return stale[luggage[i].BedID]
		}
		return luggage[i].LastUsedAt.Before(luggage[j].LastUsedAt)
	})
	var reaped []string
	for _, l := range luggage {
		if total <= m.luggageLow {
			break
		}
		if m.removeLuggage(l.BedID) {
			total -= l.Bytes
			reaped = append(reaped, l.BedID)
		}
	}
	return reaped
}

// removeLuggage deletes one cold copy. The rename happens under the manager
// lock so it is atomic against Resolve: a bed resurrected since the scan is
// skipped, and a Resolve arriving after the rename sees no dir and takes the
// cold-restore path — never a half-deleted copy.
func (m *Manager) removeLuggage(id string) bool {
	dir := filepath.Join(m.root, id)
	tmp := filepath.Join(m.root, gcTmpPrefix+id)
	m.mu.Lock()
	if _, ok := m.beds[id]; ok {
		m.mu.Unlock()
		return false
	}
	if err := os.Rename(dir, tmp); err != nil {
		m.mu.Unlock()
		return false
	}
	m.mu.Unlock()
	_ = os.RemoveAll(tmp)
	return true
}

// sweepGCLeftovers removes rename-then-crash debris from earlier GC runs.
func (m *Manager) sweepGCLeftovers() {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), gcTmpPrefix) {
			_ = os.RemoveAll(filepath.Join(m.root, e.Name()))
		}
	}
}

// dirBytes sums regular-file sizes under dir (best-effort; errors skip).
func dirBytes(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			n += info.Size()
		}
		return nil
	})
	return n
}

// InventoryBed is one row of the scheduler-facing inventory: every bed this
// instance holds, in memory (active/evicting) or as luggage. Generation is
// the last PERSISTED counter — an active bed's workspace may be ahead of it,
// which is exactly what "the authoritative copy is here" means.
type InventoryBed struct {
	ID         string    `json:"id"`
	State      string    `json:"state"` // active | evicting | luggage
	Generation int64     `json:"generation"`
	Bytes      int64     `json:"bytes,omitempty"` // luggage only (active dirs aren't sized)
	LastUsedAt time.Time `json:"last_used_at"`
	// Profile lets the scheduler weigh placement and migration: command
	// rate/duration derive from deltas between polls; Last{Persist,Restore}Ms
	// approximate this bed's migration cost (node-specific — see Profile).
	Profile Profile `json:"profile,omitzero"`
}

// Inventory reports all local beds for the upstream scheduler: placement
// wants "who has a fresh copy" (generation) and "who is loaded" (active
// count). The result is a stale-tolerant hint — freshness is re-checked at
// activation (Resolve), so a scheduler routing on outdated data is slow,
// never wrong.
func (m *Manager) Inventory() []InventoryBed {
	beds := m.List()
	out := make([]InventoryBed, 0, len(beds))
	for _, b := range beds {
		var gen int64
		if meta, ok := loadMeta(b.Dir); ok {
			gen = meta.Generation
		}
		// The in-memory profile, not meta's: an active bed's counters run
		// ahead of the last flush, and fresher is better for a hint.
		out = append(out, InventoryBed{ID: b.ID, State: b.State(), Generation: gen, LastUsedAt: b.LastUsed(), Profile: b.Profile()})
	}
	for _, l := range m.ListLuggage() {
		out = append(out, InventoryBed{ID: l.BedID, State: "luggage", Generation: l.Generation, Bytes: l.Bytes, LastUsedAt: l.LastUsedAt, Profile: l.Profile})
	}
	return out
}
