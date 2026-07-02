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

	"github.com/qiankunli/hostel/internal/isolation"
	"github.com/qiankunli/hostel/internal/service"
	"github.com/qiankunli/hostel/internal/store"
)

// Bed is one isolation unit.
type Bed struct {
	ID        string
	Workspace string // host dir backing the bed's virtual /workspace
	CreatedAt time.Time

	mu          sync.Mutex
	lastUsed    time.Time
	persistedAt time.Time         // last successful snapshot (zero = never)
	shells      map[string]*Shell // stateful bash sessions (spec /session)
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
	services   *service.Registry // nil-safe; ReleaseAll on bed teardown
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
// services and st may be nil; maxBeds 0 = unlimited.
func NewManager(root, defaultBed, shellPath string, iso isolation.Isolator, services *service.Registry, maxBeds int, st store.Store) (*Manager, error) {
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
		services:   services,
		commands:   newCommandRegistry(),
		maxBeds:    maxBeds,
		store:      st,
		beds:       make(map[string]*Bed),
	}, nil
}

// Isolator exposes the configured isolator (for /healthz + capabilities).
func (m *Manager) Isolator() isolation.Isolator { return m.iso }

// Services exposes the managed-service registry (for capabilities reporting).
func (m *Manager) Services() *service.Registry { return m.services }

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
	ws := filepath.Join(m.root, id)
	if err := os.MkdirAll(ws, 0o755); err != nil {
		return nil, fmt.Errorf("bed: create workspace %s: %w", ws, err)
	}
	// Resume-from-snapshot: if the bed has a durable copy, hydrate the fresh
	// workspace BEFORE serving. A restore failure fails the resolve — silently
	// starting empty when a snapshot exists would look like data loss.
	if ok, err := m.store.Exists(context.Background(), id); err != nil {
		return nil, fmt.Errorf("bed: check snapshot %s: %w", id, err)
	} else if ok {
		if err := m.store.Restore(context.Background(), id, ws); err != nil {
			return nil, fmt.Errorf("bed: restore %s: %w", id, err)
		}
	}
	now := time.Now()
	b := &Bed{ID: id, Workspace: ws, CreatedAt: now, lastUsed: now, persistedAt: now, shells: make(map[string]*Shell)}
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

// Delete tears a bed down: shells killed, service tenants released, workspace
// persisted (release compute, keep identity — docs/persistence.md) and then
// removed. A persist failure ABORTS the delete: destroying the only copy is
// worse than leaving the bed alive for a retry. The default bed keeps its
// workspace (only runtime state resets) so single-tenant callers can never
// lose their data to a stray DELETE.
func (m *Manager) Delete(id string) error {
	if id == "" {
		id = m.defaultBed
	}
	m.mu.Lock()
	b, ok := m.beds[id]
	m.mu.Unlock()
	if !ok {
		return nil
	}
	// Snapshot before any destructive step (shells are about to die anyway,
	// but the workspace must survive a persist failure).
	if id != m.defaultBed {
		if err := m.store.Persist(context.Background(), id, b.Workspace); err != nil {
			return fmt.Errorf("bed: persist before delete %s: %w", id, err)
		}
	}
	m.mu.Lock()
	delete(m.beds, id)
	m.mu.Unlock()
	b.mu.Lock()
	for sid, sh := range b.shells {
		sh.Close()
		delete(b.shells, sid)
	}
	b.mu.Unlock()
	m.commands.killBed(id)
	m.services.ReleaseAll(id)
	if id == m.defaultBed {
		return nil
	}
	return os.RemoveAll(b.Workspace)
}

// Checkpoint snapshots a bed's workspace now, without tearing it down.
// Best-effort consistency: hostel does not quiesce running commands yet —
// callers should checkpoint at their own idle points (docs/persistence.md §4).
func (m *Manager) Checkpoint(ctx context.Context, id string) error {
	b, ok := m.Get(id)
	if !ok {
		return fmt.Errorf("bed: unknown bed %q", id)
	}
	if err := m.store.Persist(ctx, b.ID, b.Workspace); err != nil {
		return err
	}
	b.mu.Lock()
	b.persistedAt = time.Now()
	b.mu.Unlock()
	return nil
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
		if err := m.store.Persist(ctx, b.ID, b.Workspace); err != nil {
			continue
		}
		b.mu.Lock()
		b.persistedAt = time.Now()
		b.mu.Unlock()
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
	for _, id := range stale {
		_ = m.Delete(id)
	}
	return stale
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
