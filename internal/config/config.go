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

// Package config loads hostel configuration from flags + environment.
package config

import (
	"flag"
	"time"

	"github.com/qiankunli/go-stdx/osx"
)

// Config is the hostel runtime configuration. hostel is a generic sandbox
// data-plane manager: it can run standalone on a laptop/VM, but is primarily
// meant to run inside a pod, serving one or many beds (isolation units).
// DefaultAddr is the default HTTP listen address.
const DefaultAddr = ":8872"

type Config struct {
	ShowVersion bool
	HealthCheck bool
	// Addr is the HTTP listen address.
	Addr string
	// WorkspaceRoot is the parent dir under which each bed gets its workspace
	// (<root>/<bedID>). In a pod this is typically a bind of shared network FS.
	WorkspaceRoot string
	// IsolationMode is the requested data-isolation level (room type):
	//   "dorm" | "room" | "suite" | "auto" (= environment ceiling, default).
	// Levels resolve to mechanisms (direct/landlock/bwrap) in internal/isolation;
	// effective = min(requested, ceiling), over-asks degrade honestly.
	IsolationMode string
	// DefaultBed is the bed id used when a request omits one — lets simple
	// single-tenant callers ignore the bed concept entirely.
	DefaultBed string
	// BedIdleTimeout reaps a bed whose shell has been idle this long (0 = never).
	BedIdleTimeout time.Duration
	// MaxBeds caps how many beds may exist at once (0 = unlimited). Applies to
	// NEW bed creation only, never to the default bed; the 429 it produces is
	// the backpressure/placement signal for an upstream scheduler.
	MaxBeds int
	// BedInit selects the process spawner: "auto" (default) probes the per-bed
	// init (docs/design.md 〈进程树〉) at boot and falls back to in-process
	// forking where it can't serve; "off" forces in-process.
	BedInit string

	// Workspace persistence (docs/persistence.md). Backend "auto" (default)
	// resolves to "s3" when a bucket is configured and "noop" otherwise.
	// "s3" stores content-addressed chunks under <prefix>/cas/ and transfers
	// incrementally, at lifecycle boundaries (evict/checkpoint/interval);
	// credentials resolve via the standard AWS SDK chain.
	StoreBackend string
	S3Bucket     string
	S3Prefix     string
	S3Endpoint   string // S3-compatible endpoint (MinIO/TOS/Ceph); "" = AWS
	// PersistInterval is the periodic snapshot safety net (0 = only at
	// lifecycle boundaries). Bounds how much work a crash can lose.
	PersistInterval time.Duration
	// LuggageHighBytes / LuggageLowBytes are the disk watermarks for luggage
	// (evicted beds' local dirs kept as warm cache): past high, luggage GC
	// deletes cold copies until under low. High 0 disables GC (luggage
	// accumulates — fine when workspace-root is on disposable/ample disk).
	LuggageHighBytes int64
	LuggageLowBytes  int64

	// Chromium amenity (docs/amenity.md): launch (path) or attach (CDP URL).
	ChromiumPath     string
	ChromiumCDPURL   string
	ChromiumIdleStop time.Duration
	// ShellPath is the shell binary a bed's long-running session runs.
	ShellPath string
}

// Load builds Config from flags, with env fallbacks (HOSTEL_*).
func Load(args []string) *Config {
	fs := flag.NewFlagSet("hostel", flag.ContinueOnError)
	c := &Config{}
	fs.StringVar(&c.Addr, "addr", osx.EnvStr("HOSTEL_ADDR", DefaultAddr), "HTTP listen address")
	// Preflight flags handled by main (used by the image HEALTHCHECK); real
	// flags so addr resolution stays identical to the running server.
	fs.BoolVar(&c.ShowVersion, "version", false, "print version and exit")
	fs.BoolVar(&c.HealthCheck, "health", false, "GET local /healthz and exit (0=ok)")
	fs.StringVar(&c.WorkspaceRoot, "workspace-root", osx.EnvStr("HOSTEL_WORKSPACE_ROOT", "/workspace"), "parent dir for per-bed workspaces")
	fs.StringVar(&c.IsolationMode, "isolation", osx.EnvStr("HOSTEL_ISOLATION", "auto"), "data-isolation level: dorm | room | suite | auto (auto=env ceiling)")
	fs.StringVar(&c.DefaultBed, "default-bed", osx.EnvStr("HOSTEL_DEFAULT_BED", "default"), "bed id used when a request omits one")
	fs.StringVar(&c.ShellPath, "shell", osx.EnvStr("HOSTEL_SHELL", "/bin/bash"), "shell for bed sessions")
	idle := fs.Duration("bed-idle-timeout", osx.EnvDuration("HOSTEL_BED_IDLE_TIMEOUT", 30*time.Minute), "reap a bed after this idle duration (0=never)")
	fs.IntVar(&c.MaxBeds, "max-beds", osx.EnvInt("HOSTEL_MAX_BEDS", 0), "max concurrent beds, 0=unlimited (default bed exempt)")
	fs.StringVar(&c.BedInit, "bed-init", osx.EnvStr("HOSTEL_BED_INIT", "auto"), "per-bed init spawner: auto (probe at boot, fall back in-process) | off")
	fs.StringVar(&c.StoreBackend, "store", osx.EnvStr("HOSTEL_STORE", "auto"), "workspace persistence backend: auto (s3 when --s3-bucket is set, else noop) | noop | s3")
	fs.StringVar(&c.S3Bucket, "s3-bucket", osx.EnvStr("HOSTEL_S3_BUCKET", ""), "S3 bucket for bed snapshots")
	fs.StringVar(&c.S3Prefix, "s3-prefix", osx.EnvStr("HOSTEL_S3_PREFIX", "hostel"), "key prefix for bed snapshots")
	fs.StringVar(&c.S3Endpoint, "s3-endpoint", osx.EnvStr("HOSTEL_S3_ENDPOINT", ""), "S3-compatible endpoint (empty = AWS)")
	persist := fs.Duration("persist-interval", osx.EnvDuration("HOSTEL_PERSIST_INTERVAL", 0), "periodic snapshot interval, 0=lifecycle boundaries only")
	fs.Int64Var(&c.LuggageHighBytes, "luggage-high-bytes", osx.EnvInt64("HOSTEL_LUGGAGE_HIGH_BYTES", 0), "luggage disk high watermark in bytes, 0=no luggage GC")
	fs.Int64Var(&c.LuggageLowBytes, "luggage-low-bytes", osx.EnvInt64("HOSTEL_LUGGAGE_LOW_BYTES", 0), "luggage GC target in bytes (default 80% of high)")
	fs.StringVar(&c.ChromiumPath, "chromium-path", osx.EnvStr("HOSTEL_CHROMIUM_PATH", ""), "chromium binary for the browser amenity (empty = probe PATH)")
	fs.StringVar(&c.ChromiumCDPURL, "chromium-cdp-url", osx.EnvStr("HOSTEL_CHROMIUM_CDP_URL", ""), "attach to an existing Chromium CDP endpoint instead of launching")
	idleStop := fs.Duration("chromium-idle-stop", osx.EnvDuration("HOSTEL_CHROMIUM_IDLE_STOP", 5*time.Minute), "stop a launched Chromium this long after its last tenant, 0=never")
	// Ignore parse errors for unknown flags in tests; flag prints usage itself.
	_ = fs.Parse(args)
	c.BedIdleTimeout = *idle
	c.PersistInterval = *persist
	c.ChromiumIdleStop = *idleStop
	// Low defaults to 80% of high so a bare --luggage-high-bytes works; a low
	// above high would make GC loop uselessly, so clamp it.
	if c.LuggageHighBytes > 0 && (c.LuggageLowBytes <= 0 || c.LuggageLowBytes > c.LuggageHighBytes) {
		c.LuggageLowBytes = c.LuggageHighBytes * 8 / 10
	}
	return c
}
