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

// Package bed is hostel's isolation unit. A bed is what the control plane calls
// a sandbox: one workspace dir, its own mount namespace (under bwrap), stateful
// shell sessions and one-shot commands running inside it. A pod with one bed ≈
// dedicated; with many beds ≈ shared — each bed still carrying its private
// slice (ns, workspace, shell state, service tenants).
package bed

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/qiankunli/hostel/internal/amenity"
	"github.com/qiankunli/hostel/internal/isolation"
	"github.com/qiankunli/hostel/internal/store"
)

// Bed is one isolation unit.
type Bed struct {
	ID string
	// Dir is the bed's root: meta.json + data/ (docs/persistence.md §4).
	// Snapshots pack this dir; bed code never sees it.
	Dir string
	// Workspace is Dir/data — the only part bound into the sandbox and the
	// only part bed code can touch.
	Workspace string
	CreatedAt time.Time // survives evict/resume via snapshot meta

	mu          sync.Mutex
	lastUsed    time.Time
	persistedAt time.Time         // last successful snapshot (zero = never)
	evicting    bool              // an evict's persist is in flight
	shells      map[string]*Shell // stateful bash sessions (spec /session)
}

// State reports the lifecycle state for observability: "active" or "evicting".
// DORMANT beds aren't in memory at all (their state is the snapshot itself).
func (b *Bed) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.evicting {
		return "evicting"
	}
	return "active"
}

func (b *Bed) touch() {
	b.mu.Lock()
	b.lastUsed = time.Now()
	b.mu.Unlock()
}

// LastUsed reports the last activity time (for idle GC).
func (b *Bed) LastUsed() time.Time {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.lastUsed
}

// Manager owns the set of beds and their lifecycle. Safe for concurrent use.
type Manager struct {
	root       string
	defaultBed string
	iso        isolation.Isolator
	shellPath  string
	amenities  *amenity.Registry // nil-safe; ReleaseAll on bed teardown
	commands   *CommandRegistry  // one-shot commands, daemon-global ids
	maxBeds    int               // cap on concurrent beds; 0 = unlimited
	store      store.Store       // workspace persistence (Noop when disabled)

	mu   sync.Mutex
	beds map[string]*Bed
}

// ErrBedLimit is returned when creating a new bed would exceed the configured
// cap. Callers should surface it as backpressure (HTTP 429): the upstream
// scheduler is expected to place the sandbox on another instance.
var ErrBedLimit = errors.New("bed: max bed count reached")

// NewManager creates the bed manager and ensures the workspace root exists.
// amenities and st may be nil; maxBeds 0 = unlimited.
func NewManager(root, defaultBed, shellPath string, iso isolation.Isolator, amenities *amenity.Registry, maxBeds int, st store.Store) (*Manager, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("bed: create workspace root %s: %w", root, err)
	}
	if st == nil {
		st = store.Noop{}
	}
	return &Manager{
		root:       root,
		defaultBed: defaultBed,
		iso:        iso,
		shellPath:  shellPath,
		amenities:  amenities,
		commands:   newCommandRegistry(),
		maxBeds:    maxBeds,
		store:      st,
		beds:       make(map[string]*Bed),
	}, nil
}

// Isolator exposes the configured isolator (for /healthz + capabilities).
func (m *Manager) Isolator() isolation.Isolator { return m.iso }

// Amenities exposes the amenity manager (for capabilities + web adapters).
func (m *Manager) Amenities() *amenity.Registry { return m.amenities }

// Commands exposes the one-shot command registry (spec /command endpoints are
// bed-agnostic on status/logs lookups — command ids are daemon-global).
func (m *Manager) Commands() *CommandRegistry { return m.commands }

// MaxBeds reports the configured cap (0 = unlimited) for capacity reporting.
func (m *Manager) MaxBeds() int { return m.maxBeds }

// StoreName reports the persistence backend for capabilities reporting.
func (m *Manager) StoreName() string { return m.store.Name() }

// DefaultBedID reports the id used when a request omits a bed.
func (m *Manager) DefaultBedID() string { return m.defaultBed }

// Resolve returns the bed for id, creating it on first use. An empty id maps to
// the default bed — so callers that don't know about beds still get one.
func (m *Manager) Resolve(id string) (*Bed, error) {
	if id == "" {
		id = m.defaultBed
	}
	if err := validBedID(id); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.beds[id]; ok {
		b.touch()
		return b, nil
	}
	// Cap NEW beds only; the default bed is the single-tenant fallback and
	// must never be refused (a full instance still serves its primary tenant)
	// nor counted — max-beds means "N tenant beds", not "N-1 once the default
	// bed happens to exist".
	if m.maxBeds > 0 && id != m.defaultBed {
		n := len(m.beds)
		if _, ok := m.beds[m.defaultBed]; ok {
			n--
		}
		if n >= m.maxBeds {
			return nil, ErrBedLimit
		}
	}
	bedDir := filepath.Join(m.root, id)
	dataDir := filepath.Join(bedDir, "data")
	if err := os.MkdirAll(bedDir, 0o755); err != nil {
		return nil, fmt.Errorf("bed: create bed dir %s: %w", bedDir, err)
	}
	// Resume-from-snapshot: if the bed has a durable copy, hydrate BEFORE
	// serving (snapshot = portable meta + data). A restore failure fails the
	// resolve — silently starting empty when a snapshot exists would look
	// like data loss.
	restored := false
	if info, err := m.store.Stat(context.Background(), id); err != nil {
		return nil, fmt.Errorf("bed: check snapshot %s: %w", id, err)
	} else if info != nil {
		if err := m.store.Restore(context.Background(), id, bedDir); err != nil {
			return nil, fmt.Errorf("bed: restore %s: %w", id, err)
		}
		restored = true
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("bed: create workspace %s: %w", dataDir, err)
	}

	now := time.Now()
	meta, ok := loadMeta(bedDir)
	if !ok {
		meta = bedMeta{Version: 1, BedID: id, CreatedAt: now}
		if err := saveMeta(bedDir, meta); err != nil {
			return nil, fmt.Errorf("bed: write meta %s: %w", id, err)
		}
	}
	// Dirty-tracking baseline: a just-restored bed is in sync NOW; a dir that
	// survived a process restart (default bed) trusts its on-disk timestamp,
	// so the periodic safety net stays correct across restarts.
	persistedAt := meta.LastPersistedAt
	if restored || persistedAt.IsZero() {
		persistedAt = now
	}
	b := &Bed{ID: id, Dir: bedDir, Workspace: dataDir, CreatedAt: meta.CreatedAt, lastUsed: now, persistedAt: persistedAt, shells: make(map[string]*Shell)}
	m.beds[id] = b
	return b, nil
}

// Get returns an existing bed without creating it.
func (m *Manager) Get(id string) (*Bed, bool) {
	if id == "" {
		id = m.defaultBed
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.beds[id]
	return b, ok
}

// List returns a snapshot of all beds.
func (m *Manager) List() []*Bed {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Bed, 0, len(m.beds))
	for _, b := range m.beds {
		out = append(out, b)
	}
	return out
}

// Evict releases a bed's compute while keeping its identity (ACTIVE →
// EVICTING → DORMANT, docs/persistence.md §4): persist, then tear down and
// free the max-beds slot. Returns evicted=false without error when the
// eviction was CANCELED because the bed saw new activity during the persist
// window — serving beats reclaiming, and removing the workspace after a
// mid-persist write would silently drop that write. A persist failure aborts
// the evict (never destroy the only copy). The default bed keeps its
// workspace (runtime state resets only).
func (m *Manager) Evict(id string) (bool, error) {
	if id == "" {
		id = m.defaultBed
	}
	m.mu.Lock()
	b, ok := m.beds[id]
	m.mu.Unlock()
	if !ok {
		return false, nil // not ACTIVE; nothing to evict
	}

	// Enter EVICTING: remember the activity watermark we snapshot against.
	b.mu.Lock()
	if b.evicting {
		b.mu.Unlock()
		return false, nil // another evict is already in flight
	}
	b.evicting = true
	watermark := b.lastUsed
	b.mu.Unlock()

	if err := m.persistBed(context.Background(), b); err != nil {
		b.mu.Lock()
		b.evicting = false
		b.mu.Unlock()
		return false, fmt.Errorf("bed: persist before evict %s: %w", id, err)
	}

	// Atomic re-check: activity during the persist window cancels the evict.
	// The snapshot we just took is still valid (it's simply not the final
	// word), so nothing is wasted.
	b.mu.Lock()
	if b.lastUsed.After(watermark) {
		b.evicting = false
		b.mu.Unlock()
		return false, nil
	}
	b.mu.Unlock()

	m.mu.Lock()
	delete(m.beds, id)
	m.mu.Unlock()
	m.teardown(b)
	if id == m.defaultBed {
		b.mu.Lock()
		b.evicting = false
		b.mu.Unlock()
		return true, nil
	}
	return true, os.RemoveAll(b.Dir)
}

// ErrPurgeDefault marks a client mistake (4xx), not a server failure: the
// default bed is the single-tenant fallback and cannot be purged.
var ErrPurgeDefault = errors.New("bed: refusing to purge the default bed")

// Purge ends a bed's identity: tear down (no persist), remove the local dir,
// and delete the snapshot. Explicitly destructive — the caller asked for the
// data to be gone, so concurrent activity does not cancel it.
func (m *Manager) Purge(id string) error {
	if id == "" || id == m.defaultBed {
		return ErrPurgeDefault
	}
	m.mu.Lock()
	b, ok := m.beds[id]
	if ok {
		delete(m.beds, id)
	}
	m.mu.Unlock()
	if ok {
		m.teardown(b)
		if err := os.RemoveAll(b.Dir); err != nil {
			return err
		}
	}
	// DORMANT (or never-existed) beds still have a snapshot to remove.
	return m.store.Delete(context.Background(), id)
}

// teardown kills a bed's runtime state: shells, one-shot commands, service
// tenants. The workspace is untouched — callers decide its fate.
func (m *Manager) teardown(b *Bed) {
	b.mu.Lock()
	for sid, sh := range b.shells {
		sh.Close()
		delete(b.shells, sid)
	}
	b.mu.Unlock()
	m.commands.killBed(b.ID)
	m.amenities.ReleaseAll(b.ID)
}

// persistBed snapshots the bed dir (portable meta + data) and, on success,
// advances both the in-memory and on-disk persistence watermarks.
//
// Ordering constraint: the generation bump is saved BEFORE packing (the
// snapshot must carry its own generation), but LastPersistedAt only AFTER a
// successful upload — a failed upload leaving the local generation ahead is
// accurate ("locally dirty"), while a falsely-advanced LastPersistedAt would
// make restart-time dirty tracking skip data that never reached the store.
func (m *Manager) persistBed(ctx context.Context, b *Bed) error {
	meta, ok := loadMeta(b.Dir)
	if !ok {
		meta = bedMeta{Version: 1, BedID: b.ID, CreatedAt: b.CreatedAt}
	}
	meta.Generation++
	if err := saveMeta(b.Dir, meta); err != nil {
		return fmt.Errorf("bed: bump generation %s: %w", b.ID, err)
	}
	if err := m.store.Persist(ctx, b.ID, b.Dir, meta.Generation); err != nil {
		return err
	}
	now := time.Now()
	b.mu.Lock()
	b.persistedAt = now
	b.mu.Unlock()
	meta.LastPersistedAt = now
	_ = saveMeta(b.Dir, meta) // best-effort; in-memory watermark is set
	return nil
}

// Checkpoint snapshots a bed's workspace now, without tearing it down.
// Best-effort consistency: hostel does not quiesce running commands yet —
// callers should checkpoint at their own idle points (docs/persistence.md §4).
func (m *Manager) Checkpoint(ctx context.Context, id string) error {
	b, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("bed: unknown bed %q", id)
	}
	return m.persistBed(ctx, b)
}

// PersistDirty is the periodic safety net: snapshot every bed touched since
// its last snapshot. Best-effort — a failed bed is retried next tick. Returns
// ids persisted. The default bed is included (its data matters most).
func (m *Manager) PersistDirty(ctx context.Context) []string {
	var done []string
	for _, b := range m.List() {
		b.mu.Lock()
		dirty := b.lastUsed.After(b.persistedAt)
		b.mu.Unlock()
		if !dirty {
			continue
		}
		if err := m.persistBed(ctx, b); err != nil {
			continue
		}
		done = append(done, b.ID)
	}
	return done
}

// CollectIdle reaps beds idle longer than timeout (0 disables). Returns reaped
// ids. The default bed is never reaped.
func (m *Manager) CollectIdle(timeout time.Duration) []string {
	if timeout <= 0 {
		return nil
	}
	now := time.Now()
	var stale []string
	m.mu.Lock()
	for id, b := range m.beds {
		if id == m.defaultBed {
			continue
		}
		if now.Sub(b.LastUsed()) > timeout {
			stale = append(stale, id)
		}
	}
	m.mu.Unlock()
	var reaped []string
	for _, id := range stale {
		if ok, _ := m.Evict(id); ok {
			reaped = append(reaped, id)
		}
	}
	return reaped
}

// --- shell sessions (spec /session: stateful bash inside the bed) ---

// CreateShell starts a new stateful shell session in the bed and returns its id.
// hostCwd, when non-empty, is the starting directory (already resolved+confined
// by the caller via fsops).
func (m *Manager) CreateShell(b *Bed, hostCwd string) (string, error) {
	b.touch()
	sh, err := startShell(m.shellPath, m.iso, isolation.Workspace{Path: b.Workspace}, hostCwd)
	if err != nil {
		return "", err
	}
	id := "session-" + randomID()
	b.mu.Lock()
	b.shells[id] = sh
	b.mu.Unlock()
	return id, nil
}

// foregroundShellID is the well-known session backing bed-agnostic /command
// foreground runs, so cwd/env persist across a bed's commands without the
// caller managing a session id.
const foregroundShellID = "session-foreground"

// ForegroundShell returns the bed's implicit foreground shell, starting it
// once and reusing it (restarting if it died).
func (m *Manager) ForegroundShell(b *Bed) (*Shell, error) {
	b.touch()
	b.mu.Lock()
	if sh, ok := b.shells[foregroundShellID]; ok && !sh.Dead() {
		b.mu.Unlock()
		return sh, nil
	}
	b.mu.Unlock()

	sh, err := startShell(m.shellPath, m.iso, isolation.Workspace{Path: b.Workspace}, "")
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if ex, ok := b.shells[foregroundShellID]; ok && !ex.Dead() { // lost a race
		sh.Close()
		return ex, nil
	}
	b.shells[foregroundShellID] = sh
	return sh, nil
}

// GetShell returns a live shell session by id.
func (b *Bed) GetShell(id string) (*Shell, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	sh, ok := b.shells[id]
	if !ok || sh.Dead() {
		return nil, false
	}
	return sh, true
}

// DeleteShell kills and removes a shell session. Returns false when unknown.
func (b *Bed) DeleteShell(id string) bool {
	b.mu.Lock()
	sh, ok := b.shells[id]
	if ok {
		delete(b.shells, id)
	}
	b.mu.Unlock()
	if ok {
		sh.Close()
	}
	return ok
}

// --- one-shot commands (spec /command) ---

// buildCommand constructs an isolated `bash -c <command>` for the bed. envs are
// appended to the daemon environment; cwd (host path) overrides the workspace.
func (m *Manager) buildCommand(b *Bed, command, hostCwd string, envs map[string]string) (*exec.Cmd, error) {
	b.touch()
	cmd := exec.Command(m.shellPath, "-c", command)
	if err := m.iso.Wrap(cmd, isolation.Workspace{Path: b.Workspace}); err != nil {
		return nil, err
	}
	if hostCwd != "" {
		cmd.Dir = hostCwd
	}
	if len(envs) > 0 {
		env := os.Environ()
		for k, v := range envs {
			env = append(env, k+"="+v)
		}
		cmd.Env = env
	}
	return cmd, nil
}

// StartCommand launches a one-shot command in the bed and registers it.
func (m *Manager) StartCommand(b *Bed, command, hostCwd string, envs map[string]string, timeout time.Duration, onLine func(string)) (*Command, error) {
	cmd, err := m.buildCommand(b, command, hostCwd, envs)
	if err != nil {
		return nil, err
	}
	return m.commands.start(b.ID, cmd, timeout, onLine)
}
