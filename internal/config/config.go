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
	"os"
	"strconv"
	"time"
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
	// IsolationMode selects how a bed's commands are confined:
	//   "direct" — no isolation, just chdir into the workspace (dev / trusted).
	//   "bwrap"  — Linux only: run under bubblewrap (mount/pid/uts/ipc ns).
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

	// Workspace persistence (docs/persistence.md). Backend "noop" disables;
	// "s3" snapshots each bed to <bucket>/<prefix>/<bedID>.tar.gz at lifecycle
	// boundaries. Credentials resolve via the standard AWS SDK chain.
	StoreBackend string
	S3Bucket     string
	S3Prefix     string
	S3Endpoint   string // S3-compatible endpoint (MinIO/TOS/Ceph); "" = AWS
	// PersistInterval is the periodic snapshot safety net (0 = only at
	// lifecycle boundaries). Bounds how much work a crash can lose.
	PersistInterval time.Duration

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
	fs.StringVar(&c.Addr, "addr", envStr("HOSTEL_ADDR", DefaultAddr), "HTTP listen address")
	// Preflight flags handled by main (used by the image HEALTHCHECK); real
	// flags so addr resolution stays identical to the running server.
	fs.BoolVar(&c.ShowVersion, "version", false, "print version and exit")
	fs.BoolVar(&c.HealthCheck, "health", false, "GET local /healthz and exit (0=ok)")
	fs.StringVar(&c.WorkspaceRoot, "workspace-root", envStr("HOSTEL_WORKSPACE_ROOT", "/workspace"), "parent dir for per-bed workspaces")
	fs.StringVar(&c.IsolationMode, "isolation", envStr("HOSTEL_ISOLATION", "direct"), "isolation mode: direct | bwrap")
	fs.StringVar(&c.DefaultBed, "default-bed", envStr("HOSTEL_DEFAULT_BED", "default"), "bed id used when a request omits one")
	fs.StringVar(&c.ShellPath, "shell", envStr("HOSTEL_SHELL", "/bin/bash"), "shell for bed sessions")
	idle := fs.Duration("bed-idle-timeout", envDur("HOSTEL_BED_IDLE_TIMEOUT", 30*time.Minute), "reap a bed after this idle duration (0=never)")
	fs.IntVar(&c.MaxBeds, "max-beds", envInt("HOSTEL_MAX_BEDS", 0), "max concurrent beds, 0=unlimited (default bed exempt)")
	fs.StringVar(&c.StoreBackend, "store", envStr("HOSTEL_STORE", "noop"), "workspace persistence backend: noop | s3")
	fs.StringVar(&c.S3Bucket, "s3-bucket", envStr("HOSTEL_S3_BUCKET", ""), "S3 bucket for bed snapshots (store=s3)")
	fs.StringVar(&c.S3Prefix, "s3-prefix", envStr("HOSTEL_S3_PREFIX", "hostel"), "key prefix for bed snapshots")
	fs.StringVar(&c.S3Endpoint, "s3-endpoint", envStr("HOSTEL_S3_ENDPOINT", ""), "S3-compatible endpoint (empty = AWS)")
	persist := fs.Duration("persist-interval", envDur("HOSTEL_PERSIST_INTERVAL", 0), "periodic snapshot interval, 0=lifecycle boundaries only")
	fs.StringVar(&c.ChromiumPath, "chromium-path", envStr("HOSTEL_CHROMIUM_PATH", ""), "chromium binary for the browser amenity (empty = probe PATH)")
	fs.StringVar(&c.ChromiumCDPURL, "chromium-cdp-url", envStr("HOSTEL_CHROMIUM_CDP_URL", ""), "attach to an existing Chromium CDP endpoint instead of launching")
	idleStop := fs.Duration("chromium-idle-stop", envDur("HOSTEL_CHROMIUM_IDLE_STOP", 5*time.Minute), "stop a launched Chromium this long after its last tenant, 0=never")
	// Ignore parse errors for unknown flags in tests; flag prints usage itself.
	_ = fs.Parse(args)
	c.BedIdleTimeout = *idle
	c.PersistInterval = *persist
	c.ChromiumIdleStop = *idleStop
	return c
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDur(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
		if n, err := strconv.Atoi(v); err == nil {
			return time.Duration(n) * time.Second
		}
	}
	return def
}
