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
	"os"
	"path/filepath"
	"time"
)

// metaFile sits next to (not inside) the bed's data dir, so bed code can never
// see or tamper with it; it travels inside the snapshot (portable by default —
// host-local state would use the *.local convention instead).
const metaFile = "meta.json"

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
}

func metaPath(bedDir string) string { return filepath.Join(bedDir, metaFile) }

func loadMeta(bedDir string) (bedMeta, bool) {
	var m bedMeta
	data, err := os.ReadFile(metaPath(bedDir))
	if err != nil {
		return m, false
	}
	if json.Unmarshal(data, &m) != nil {
		return bedMeta{}, false
	}
	return m, true
}

func saveMeta(bedDir string, m bedMeta) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath(bedDir), data, 0o644)
}
