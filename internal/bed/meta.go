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
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/qiankunli/go-stdx/osx"
)

// metaFile sits next to (not inside) the bed's data dir, so bed code can never
// see or tamper with it; it travels inside the snapshot (portable by default —
// host-local state would use the *.local convention instead).
const metaFile = "meta.json"

// Profile is the bed's usage picture for the upstream scheduler (placement,
// evict and migration decisions). Counters are cumulative over the bed's
// lifetime and travel with the snapshot; rates derive from deltas between
// inventory polls, so hostel keeps no windows. Like the rest of the
// inventory, every value is a stale-tolerant hint, never load-bearing.
type Profile struct {
	// Command volume: foreground, session and background runs alike.
	CmdCount   int64 `json:"cmd_count,omitempty"`
	CmdTotalMs int64 `json:"cmd_total_ms,omitempty"` // wall-clock sum
	// Node-specific: measured on the host that last ran the bed (its
	// store/network/disk speed). After a cross-host migration they describe
	// the previous host until re-measured here.
	LastPersistMs int64 `json:"last_persist_ms,omitempty"`
	LastRestoreMs int64 `json:"last_restore_ms,omitempty"`
}

// bedMeta is hostel's durable per-bed bookkeeping (docs/persistence.md §4).
type bedMeta struct {
	Version int    `json:"version"`
	BedID   string `json:"bed_id"`
	// CreatedAt is when the bed identity was first created — it survives
	// evict/resume cycles via the snapshot.
	CreatedAt time.Time `json:"created_at"`
	// LastPersistedAt is set only after a SUCCESSFUL persist, so restart-time
	// dirty tracking never mistakes a failed upload for a fresh snapshot.
	LastPersistedAt time.Time `json:"last_persisted_at,omitzero"`
	// Generation counts persist attempts, monotonically; it is the freshness
	// token for local copies (docs/persistence.md): luggage is current iff its
	// generation >= the snapshot's. Bumped BEFORE packing so the snapshot
	// carries its own generation; a failed upload leaves the local copy ahead,
	// which reads as "locally dirty" — accurate, and the next persist re-bumps.
	// It orders snapshots where wall clocks cannot (beds migrate across hosts).
	Generation int64 `json:"generation,omitempty"`
	// LastUsedAt is stamped at evict time so luggage GC can order cold local
	// copies by recency without any in-memory state.
	LastUsedAt time.Time `json:"last_used_at,omitzero"`
	// Profile accumulates in memory while the bed is ACTIVE and is flushed
	// here at persist time — the snapshot carries the counters, so they
	// survive evict/resume and migration.
	Profile Profile `json:"profile,omitzero"`
}

func metaPath(bedDir string) string { return filepath.Join(bedDir, metaFile) }

func loadMeta(bedDir string) (bedMeta, bool) {
	var m bedMeta
	data, err := os.ReadFile(metaPath(bedDir))
	if err != nil {
		return m, false // missing = fresh bed; other errors surface on save
	}
	if err := json.Unmarshal(data, &m); err != nil {
		// A corrupt meta gets rebuilt by the caller — losing CreatedAt is
		// survivable, but never silently: this is the bed's identity record.
		log.Printf("bed: corrupt %s in %s (%v); rebuilding meta", metaFile, bedDir, err)
		return bedMeta{}, false
	}
	return m, true
}

// saveMeta writes atomically: meta.json is the bed's sole identity record —
// a crash mid-write must not truncate it.
func saveMeta(bedDir string, m bedMeta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return osx.WriteFileAtomic(metaPath(bedDir), data, 0o600)
}
