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

// Package amenity manages hostel's shared facilities (docs/amenity.md):
// heavyweight, natively multi-tenant processes (Chromium via BrowserContext,
// Jupyter via kernels, ...) run ONCE per hostel, outside any bed; each bed
// gets a native tenant slice whose artifacts land in that bed's workspace.
//
// Like beds, amenities have a lifecycle of their own — started on demand,
// stopped when idle — hostel is the composition of web server + bed manager +
// amenity manager + store.
package amenity

import "sync"

// Lifecycle states reported by Amenity.State.
const (
	// StateUnavailable: not usable on this deployment (binary/endpoint
	// missing or boot probe failed). Reported honestly, never guessed.
	StateUnavailable = "unavailable"
	// StateIdle: usable but not running — starts on first tenant demand.
	StateIdle = "idle"
	// StateRunning: the shared process is up and serving tenants.
	StateRunning = "running"
)

// Tenant is one bed's slice of an amenity (a BrowserContext, a kernel).
type Tenant interface {
	Close() error
}

// Amenity is one shared facility managed by hostel.
type Amenity interface {
	Name() string
	// State reports the lifecycle state (unavailable | idle | running).
	State() string
	// AcquireTenant returns (creating, and starting the facility, if needed)
	// the tenant slice for a bed. Artifacts must be routed into the bed's
	// workspace dir.
	AcquireTenant(bedID, workspace string) (Tenant, error)
	// ReleaseTenant tears down the bed's slice. Called on bed evict/purge.
	// Releasing the last tenant may stop the facility after an idle grace.
	ReleaseTenant(bedID string) error
}

// Registry is the amenity manager: the set of facilities wired at boot.
// Nil-safe: a nil registry behaves as empty.
type Registry struct {
	mu        sync.RWMutex
	amenities []Amenity
}

func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) Register(a Amenity) {
	if r == nil || a == nil {
		return
	}
	r.mu.Lock()
	r.amenities = append(r.amenities, a)
	r.mu.Unlock()
}

// List snapshots the registered amenities.
func (r *Registry) List() []Amenity {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Amenity, len(r.amenities))
	copy(out, r.amenities)
	return out
}

// Find returns the named amenity, or nil.
func (r *Registry) Find(name string) Amenity {
	for _, a := range r.List() {
		if a.Name() == name {
			return a
		}
	}
	return nil
}

// BedScopedSecrets is an optional amenity capability: credentials minted per
// BED (not per tenant), e.g. the chromium CDP proxy token handed to every bed
// process via spawn env. They must survive tenant recycling — browser/close
// releases the tenant while the bed lives on, and long-running shells keep the
// minted endpoint in env — so revocation is tied to bed teardown, not
// ReleaseTenant.
type BedScopedSecrets interface {
	RevokeBedSecrets(bedID string)
}

// ReleaseAll tears down every amenity's tenant for a bed AND revokes its
// bed-scoped secrets — this is the bed-teardown path (evict/purge), the one
// place both lifecycles end together. Best-effort — bed teardown must not be
// blocked by one bad facility.
func (r *Registry) ReleaseAll(bedID string) {
	for _, a := range r.List() {
		_ = a.ReleaseTenant(bedID)
		if s, ok := a.(BedScopedSecrets); ok {
			s.RevokeBedSecrets(bedID)
		}
	}
}
