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
	"bufio"
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/qiankunli/go-stdx/randx"
	"github.com/qiankunli/go-stdx/shellx"

	"github.com/qiankunli/hostel/internal/amenity"
	"github.com/qiankunli/hostel/internal/fsops"
	"github.com/qiankunli/hostel/internal/isolation"
	"github.com/qiankunli/hostel/internal/store"
)

// ShortID derives a display-only short form of a bed id for log lines. Caller
// ids look like "sandbox-<uuidv7>": the shared prefix carries no information
// (uuidv7 leads with a timestamp) while the entropy sits at the tail, so keep
// the tail. Display only — the full id stays the identity everywhere; the
// "bed active" line logged at activation anchors the full↔short mapping, and
// grepping a tail also hits lines that print the full id.
func ShortID(id string) string {
	const tail = 8
	if len(id) <= tail+2 { // "default" and other short ids read best untouched
		return id
	}
	return "…" + id[len(id)-tail:]
}

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

	// paths converts between this bed's three path spaces (client / host /
	// in-bed). THE place for any path stitching — callers must not rebuild
	// MountPoint()+Rel+Join by hand (that's how the exec-cwd ENOENT happened).
	paths fsops.Paths

	mu          sync.Mutex
	lastUsed    time.Time
	persistedAt time.Time         // last successful snapshot (zero = never)
	evicting    bool              // an evict's persist is in flight
	shells      map[string]*Shell // stateful bash sessions (spec /session)
	profile     Profile           // cumulative; seeded from meta, flushed at persist
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

// Short is ShortID(b.ID) — the log-friendly form of this bed's id.
func (b *Bed) Short() string { return ShortID(b.ID) }

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

// Paths converts between this bed's path spaces (client / host / in-bed).
// Immutable value set at creation; safe without the lock.
func (b *Bed) Paths() fsops.Paths { return b.paths }

// RecordCommand adds one finished run (foreground, session or background) to
// the bed's usage profile. Failed runs count too — they are load all the same.
func (b *Bed) RecordCommand(d time.Duration) {
	b.mu.Lock()
	b.profile.CmdCount++
	b.profile.CmdTotalMs += d.Milliseconds()
	b.mu.Unlock()
}

// Profile returns a copy of the bed's current usage profile.
func (b *Bed) Profile() Profile {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.profile
}

// Manager owns the set of beds and their lifecycle. Safe for concurrent use.
type Manager struct {
	root       string
	defaultBed string
	iso        isolation.Isolator
	shellPath  string
	amenities  *amenity.Registry // nil-safe; ReleaseAll on bed teardown
	commands   *CommandRegistry  // one-shot commands, daemon-global ids
	spawner    Spawner           // forks bed processes; owns the teardown sweep
	maxBeds    int               // cap on concurrent beds; 0 = unlimited
	store      store.Store       // workspace persistence (Noop when disabled)
	// luggage disk watermarks (bytes; high 0 = GC off). Set once at startup
	// via SetLuggageLimits — not synchronized.
	luggageHigh int64
	luggageLow  int64
	// cdpAdvertise (loopback host:port) enables per-bed browser endpoint
	// injection into bed env. Set once at startup via SetCDPAdvertise — not
	// synchronized.
	cdpAdvertise string

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
		spawner:    newInProcSpawner(),
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
	// Resume: prefer the local copy (luggage) when its generation says it is
	// at least as new as the snapshot — evict→resume on the same instance
	// then costs no download. A stale local copy (the bed ran elsewhere
	// meanwhile) is discarded, never merged. A restore failure fails the
	// resolve — silently starting empty when a snapshot exists would look
	// like data loss.
	restored := false
	var restoreMs int64
	if info, err := m.store.Stat(context.Background(), id); err != nil {
		return nil, fmt.Errorf("bed: check snapshot %s: %w", id, err)
	} else if info != nil {
		local, ok := loadMeta(bedDir)
		if !ok || local.Generation < info.Generation {
			if err := os.RemoveAll(bedDir); err != nil {
				return nil, fmt.Errorf("bed: drop stale luggage %s: %w", id, err)
			}
			if err := os.MkdirAll(bedDir, 0o755); err != nil {
				return nil, fmt.Errorf("bed: recreate bed dir %s: %w", bedDir, err)
			}
			t0 := time.Now()
			if err := m.store.Restore(context.Background(), id, bedDir); err != nil {
				return nil, fmt.Errorf("bed: restore %s: %w", id, err)
			}
			restoreMs = time.Since(t0).Milliseconds()
			restored = true
		}
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("bed: create workspace %s: %w", dataDir, err)
	}
	// Let the isolator prepare the freshly (re)created data dir — uid isolation
	// chowns it to the bed's dedicated uid; other mechanisms no-op. Must run
	// after any restore repopulated the tree, before the bed serves.
	if p, ok := m.iso.(isolation.Preparer); ok {
		if err := p.Prepare(isolation.Workspace{Path: dataDir}); err != nil {
			return nil, fmt.Errorf("bed: prepare workspace %s: %w", id, err)
		}
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
	// The snapshot's profile keeps accumulating in memory; a fresh restore
	// measurement replaces whatever host measured the previous one. In-memory
	// only until the next persist flushes it — losing it to a crash is fine,
	// it's a hint.
	profile := meta.Profile
	if restored {
		profile.LastRestoreMs = restoreMs
	}
	b := &Bed{ID: id, Dir: bedDir, Workspace: dataDir, CreatedAt: meta.CreatedAt, lastUsed: now, persistedAt: persistedAt, profile: profile, shells: make(map[string]*Shell),
		paths: fsops.NewPaths(dataDir, m.iso.MountPoint())}
	m.beds[id] = b
	// The one place the full id is logged: everything downstream logs Short(),
	// so this line is the grep anchor from a control-plane sandbox id.
	log.Printf("hostel bed active: bed=%s short=%s restored=%v", id, b.Short(), restored)
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
// free the max-beds slot. The local dir stays behind as luggage — a warm
// cache of the DORMANT bed, so a same-instance resume skips the snapshot
// download; luggage GC reclaims disk separately. Returns evicted=false
// without error when the eviction was CANCELED because the bed saw new
// activity during the persist window — serving beats reclaiming, and
// removing runtime state after a mid-persist write would silently drop that
// write. A persist failure aborts the evict (never destroy the only copy).
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
	// Stamp the luggage with its last activity so luggage GC can order cold
	// copies by recency with no in-memory state. Best-effort — a missing
	// stamp only weakens GC ordering, never correctness.
	if meta, ok := loadMeta(b.Dir); ok {
		meta.LastUsedAt = watermark
		_ = saveMeta(b.Dir, meta)
	}
	b.mu.Lock()
	b.evicting = false
	b.mu.Unlock()
	return true, nil
}

// ErrPurgeDefault marks a client mistake (4xx), not a server failure: the
// default bed is the single-tenant fallback and cannot be purged.
var ErrPurgeDefault = errors.New("bed: refusing to purge the default bed")

// Purge ends a bed's identity: tear down (no persist), remove the local dir
// (active workspace or leftover luggage), and delete the snapshot. Explicitly
// destructive — the caller asked for the data to be gone, so concurrent
// activity does not cancel it.
func (m *Manager) Purge(id string) error {
	if id == "" || id == m.defaultBed {
		return ErrPurgeDefault
	}
	// Purge touches the filesystem even for beds not in memory (luggage), so
	// the id must be validated here too — never path-join an unchecked id.
	if err := validBedID(id); err != nil {
		return err
	}
	m.mu.Lock()
	b, ok := m.beds[id]
	if ok {
		delete(m.beds, id)
	}
	m.mu.Unlock()
	if ok {
		m.teardown(b)
	}
	// DORMANT beds may leave luggage (same path as an active bed's dir).
	if err := os.RemoveAll(filepath.Join(m.root, id)); err != nil {
		return err
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
	// The spawner sweep is the authoritative kill: it also catches processes
	// the registry never saw (foreground RunForeground runs are unregistered).
	m.spawner.KillBed(b.ID)
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
	// Flush the in-memory counters pre-pack so the snapshot carries them.
	// LastPersistMs necessarily lags one persist behind in the snapshot (this
	// persist's duration is only known after the upload) — fine for a hint.
	meta.Profile = b.Profile()
	if err := saveMeta(b.Dir, meta); err != nil {
		return fmt.Errorf("bed: bump generation %s: %w", b.ID, err)
	}
	t0 := time.Now()
	if err := m.store.Persist(ctx, b.ID, b.Dir, meta.Generation); err != nil {
		return err
	}
	now := time.Now()
	b.mu.Lock()
	b.persistedAt = now
	b.profile.LastPersistMs = now.Sub(t0).Milliseconds()
	meta.Profile = b.profile
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
// cwdInBed, when non-empty, is the starting directory (already resolved+confined
// by the caller via fsops).
func (m *Manager) CreateShell(b *Bed, cwdInBed string) (string, error) {
	b.touch()
	sh, err := startShell(m.spawner, b.ID, m.shellPath, m.bedEnv(b.ID), m.iso, isolation.Workspace{Path: b.Workspace}, cwdInBed)
	if err != nil {
		return "", err
	}
	id := "session-" + randx.Hex(8)
	b.mu.Lock()
	b.shells[id] = sh
	b.mu.Unlock()
	return id, nil
}

// foregroundShellID is the well-known session backing the explicit /session
// foreground shell (cwd/env persist across its runs). NOTE: the one-shot
// /command path no longer uses it — foreground /command now runs a fresh
// isolated process (see Manager.RunForeground). Retained for /session and its
// tests; a candidate for removal once /session is reworked.
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

	sh, err := startShell(m.spawner, b.ID, m.shellPath, m.bedEnv(b.ID), m.iso, isolation.Workspace{Path: b.Workspace}, "")
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
func (m *Manager) buildCommand(b *Bed, command, cwdInBed string, envs map[string]string) (*exec.Cmd, error) {
	b.touch()
	// Apply cwd with a `cd` INSIDE the command (same mechanism the session shell
	// uses), NOT via cmd.Dir. Under suite cwdInBed is a sandbox-internal path
	// (/workspace/…) that doesn't exist on the carrier host, so setting it as
	// the outer (bwrap) process's Dir makes ForkExec's chdir fail with ENOENT
	// ("bedinit: spawn: fork: no such file or directory"). The cd runs in the
	// command's own view — inside bwrap under suite, directly under direct —
	// where cwdInBed is valid (web.resolveCwd materialized the dir via EnsureDir).
	if cwdInBed != "" {
		command = "cd -- " + shellx.Quote(cwdInBed) + " && { " + command + " ; }"
	}
	cmd := exec.Command(m.shellPath, "-c", command)
	if err := m.iso.Wrap(cmd, isolation.Workspace{Path: b.Workspace}); err != nil {
		return nil, err
	}
	// The OUTER process cwd must exist on the host; the bed's own workspace
	// always does (the in-sandbox cwd is handled by the cd above / bwrap --chdir).
	cmd.Dir = b.Workspace
	env := m.bedEnv(b.ID)
	for k, v := range envs {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	return cmd, nil
}

// SetCDPAdvertise enables per-bed browser endpoint injection: every process
// spawned into a bed gets PLAYWRIGHT_MCP_CDP_ENDPOINT pointing at its own
// proxied CDP slice (docs/amenity.md §6). addr is the host:port beds can reach
// hostel on — loopback, since beds share the pod net ns.
func (m *Manager) SetCDPAdvertise(addr string) { m.cdpAdvertise = addr }

// bedEnv is the environment every process spawned into a bed receives.
// HOSTEL_BED_ID lets in-bed tooling address its OWN bed on the bed-scoped APIs
// — always present, since a bed can't otherwise learn its id from inside.
// PLAYWRIGHT_MCP_CDP_ENDPOINT hands playwright-family tooling (playwright-cli,
// playwright MCP, the extensions/playwright dispatcher) the bed's proxied
// browser slice with zero in-bed config. Minting its secret is cheap by design
// (no browser boot — see amenity Browser.CDPToken), so every bed gets one
// eagerly while the browser stays demand-started (first proxy dial).
func (m *Manager) bedEnv(bedID string) []string {
	env := append(os.Environ(), "HOSTEL_BED_ID="+bedID)
	if m.cdpAdvertise == "" {
		return env
	}
	a := m.amenities.Find("chromium")
	br, ok := a.(amenity.Browser)
	if a == nil || !ok {
		return env
	}
	token, err := br.CDPToken(bedID)
	if err != nil {
		// Proxy unavailable (e.g. launch mode without a fixed debug port):
		// honest absence — tools fall back to their own browsers.
		return env
	}
	u := url.URL{Scheme: "ws", Host: m.cdpAdvertise, Path: "/v1/cdp",
		RawQuery: url.Values{"bed": {bedID}, "t": {token}}.Encode()}
	return append(env, "PLAYWRIGHT_MCP_CDP_ENDPOINT="+u.String())
}

// startOneShot builds and launches an isolated one-shot command via the
// spawner, returning the proc and the read end of its combined stdout+stderr.
// The pipe is explicit (not StdoutPipe) so the raw child-side fd can cross a
// process boundary when the spawner is the bed's init.
func (m *Manager) startOneShot(b *Bed, command, cwdInBed string, envs map[string]string) (Proc, *os.File, error) {
	cmd, err := m.buildCommand(b, command, cwdInBed, envs)
	if err != nil {
		return nil, nil, err
	}
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	cmd.Stdout = pw
	cmd.Stderr = pw // interleave, like the /command spec
	proc, err := m.spawner.Start(b.ID, cmd)
	// Child holds its own copy now (or never will, on error): drop ours, or
	// the reader never sees EOF.
	pw.Close()
	if err != nil {
		pr.Close()
		return nil, nil, err
	}
	return proc, pr, nil
}

// StartCommand launches a one-shot command in the bed and registers it.
func (m *Manager) StartCommand(b *Bed, command, cwdInBed string, envs map[string]string, timeout time.Duration, onLine func(string)) (*Command, error) {
	proc, out, err := m.startOneShot(b, command, cwdInBed, envs)
	if err != nil {
		return nil, err
	}
	c := m.commands.track(b.ID, proc, out, timeout, onLine)
	go func() { // profile the run once it is reaped (background = async)
		c.Wait()
		st := c.Status()
		if st.FinishedAt != nil {
			b.RecordCommand(st.FinishedAt.Sub(st.StartedAt))
		}
	}()
	return c, nil
}

// RunForeground executes a one-shot command as a fresh, isolated `bash -c`
// process (execd parity: /command is stateless), streams combined stdout+stderr
// via onLine, and blocks until the process exits or ctx is cancelled. Returns
// the process exit code (-1 on a non-exit failure).
//
// Unlike StartCommand it is NOT registered in the daemon-global command registry
// (which never GCs — a per-exec entry there would leak) and reuses nothing: the
// command runs in its OWN process, so a caller script's `set -e` / `exit` /
// `trap` dies with it. The foreground /command path used to run in the bed's
// shared stateful shell, where exactly those constructs tore the whole session
// down ("shell: session exited during run"); the persistent shell now serves
// only the explicit /session endpoint.
func (m *Manager) RunForeground(ctx context.Context, b *Bed, command, cwdInBed string, envs map[string]string, timeout time.Duration, onLine func(string)) (int, error) {
	proc, out, err := m.startOneShot(b, command, cwdInBed, envs)
	if err != nil {
		return -1, err
	}
	start := time.Now()
	if timeout > 0 {
		t := time.AfterFunc(timeout, proc.Kill)
		defer t.Stop()
	}
	// Client disconnect / shutdown cancels the run: kill the tree so a runaway
	// command can't outlive its caller.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			proc.Kill()
		case <-stop:
		}
	}()
	reader := bufio.NewReader(out)
	for {
		line, rerr := reader.ReadString('\n')
		if line != "" && onLine != nil {
			onLine(line)
		}
		if rerr != nil {
			break
		}
	}
	out.Close()
	code, werr := proc.Wait()
	b.RecordCommand(time.Since(start))
	if werr != nil {
		return -1, werr
	}
	return code, nil
}
