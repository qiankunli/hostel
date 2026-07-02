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

// Package service defines hostel's managed-service plugin point: heavyweight
// processes that natively multiplex tenants (Chromium via BrowserContext,
// Jupyter via kernels, ...) run ONCE per hostel, outside any bed; each bed gets
// a native tenant slice whose artifacts land in that bed's workspace.
//
// v1 ships only the interface and registry — no instances — so the bed
// lifecycle already carries the ReleaseTenant hook and Chromium/Jupyter drop in
// later without touching bed code. See docs/design.md §3.
package service

import "sync"

// Tenant is one bed's slice of a managed service (a BrowserContext, a kernel).
type Tenant interface {
	Close() error
}

// ManagedService is one shared heavyweight process managed by hostel.
type ManagedService interface {
	Name() string
	// AcquireTenant returns (creating if needed) the tenant slice for a bed.
	// Artifacts must be routed into the bed's workspace dir.
	AcquireTenant(bedID, workspace string) (Tenant, error)
	// ReleaseTenant tears down the bed's slice. Called on bed delete/idle GC.
	ReleaseTenant(bedID string) error
	Healthy() bool
}

// Registry is the set of managed services wired at boot. Nil-safe: a nil
// registry behaves as empty so tests and minimal setups need no wiring.
type Registry struct {
	mu       sync.RWMutex
	services []ManagedService
}

func NewRegistry() *Registry { return &Registry{} }

func (r *Registry) Register(s ManagedService) {
	if r == nil || s == nil {
		return
	}
	r.mu.Lock()
	r.services = append(r.services, s)
	r.mu.Unlock()
}

// List snapshots the registered services.
func (r *Registry) List() []ManagedService {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]ManagedService, len(r.services))
	copy(out, r.services)
	return out
}

// ReleaseAll tears down every service's tenant for a bed. Errors are collected
// best-effort — bed teardown must not be blocked by one bad service.
func (r *Registry) ReleaseAll(bedID string) {
	for _, s := range r.List() {
		_ = s.ReleaseTenant(bedID)
	}
}
