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

// Package store persists bed workspaces beyond the life of the process/pod
// (docs/persistence.md): the durable identity of a bed is a snapshot in object
// storage; the local workspace is just its working copy, synced at lifecycle
// boundaries (create/resume ← restore, idle/delete/checkpoint → persist).
// hostel does not solve multi-writer coordination — "one bedID live in one
// hostel at a time" is the upstream scheduler's guarantee.
package store

import (
	"context"
	"fmt"
)

// SnapshotInfo describes a bed's durable snapshot without downloading it.
type SnapshotInfo struct {
	// Generation is the snapshot's persist counter (mirrors the bed meta's
	// generation, carried in backend metadata so Stat stays cheap). A local
	// copy with generation >= this is current and can skip Restore.
	Generation int64
	// Bytes is the packed snapshot size (0 when the backend can't tell).
	Bytes int64
}

// Store is the persistence backend for bed workspaces. Implementations must
// treat Persist as atomic per bed (a reader never sees a half-written
// snapshot) — the tarball-per-bed layout gives this for free on S3.
type Store interface {
	// Name reports the backend for capabilities/healthz ("noop", "s3").
	Name() string
	// Stat describes the bed's snapshot, or nil when none exists. Must be
	// cheap (S3: HEAD + user metadata, no download) — luggage freshness
	// checks call it on every resume.
	Stat(ctx context.Context, bedID string) (*SnapshotInfo, error)
	// Restore unpacks the bed's snapshot into dir (an existing, usually empty
	// workspace dir). Called on bed create/resume, before serving requests.
	Restore(ctx context.Context, bedID, dir string) error
	// Persist snapshots dir as the bed's durable copy, replacing any previous
	// snapshot. Called on evict, explicit checkpoint, and the periodic safety
	// net. dir is the bed dir (portable meta + data/); top-level *.local
	// files are host-private and excluded. generation is the meta's persist
	// counter, surfaced back through Stat.
	Persist(ctx context.Context, bedID, dir string, generation int64) error
	// Delete removes the bed's snapshot — the purge path: after this the bed
	// identity no longer exists anywhere. Deleting a missing snapshot is not
	// an error.
	Delete(ctx context.Context, bedID string) error
}

// Config selects and parameterizes the backend (flags/env in config package).
type Config struct {
	Backend  string // "noop" (default) | "s3"
	Bucket   string
	Prefix   string // key prefix inside the bucket, e.g. "hostel/prod"
	Endpoint string // non-AWS S3-compatible endpoint (MinIO/TOS/Ceph); "" = AWS
}

// New builds the configured backend. noop needs nothing; s3 resolves
// credentials via the standard AWS SDK chain (env, shared config, IRSA...).
func New(ctx context.Context, cfg Config) (Store, error) {
	switch cfg.Backend {
	case "", "noop":
		return Noop{}, nil
	case "s3":
		if cfg.Bucket == "" {
			return nil, fmt.Errorf("store: s3 backend requires a bucket")
		}
		return newS3(ctx, cfg)
	default:
		return nil, fmt.Errorf("store: unknown backend %q", cfg.Backend)
	}
}

// Noop is the zero-dependency default: nothing persists, nothing restores.
// Beds behave exactly like pre-persistence hostel (laptop/dev, or when the
// upstream platform persists workspaces some other way).
type Noop struct{}

func (Noop) Name() string                                         { return "noop" }
func (Noop) Stat(context.Context, string) (*SnapshotInfo, error)  { return nil, nil }
func (Noop) Restore(context.Context, string, string) error        { return nil }
func (Noop) Persist(context.Context, string, string, int64) error { return nil }
func (Noop) Delete(context.Context, string) error                 { return nil }
