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
type Config struct {
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
	// ShellPath is the shell binary a bed's long-running session runs.
	ShellPath string
}

// Load builds Config from flags, with env fallbacks (HOSTEL_*).
func Load(args []string) *Config {
	fs := flag.NewFlagSet("hostel", flag.ContinueOnError)
	c := &Config{}
	fs.StringVar(&c.Addr, "addr", envStr("HOSTEL_ADDR", ":44772"), "HTTP listen address")
	fs.StringVar(&c.WorkspaceRoot, "workspace-root", envStr("HOSTEL_WORKSPACE_ROOT", "/workspace"), "parent dir for per-bed workspaces")
	fs.StringVar(&c.IsolationMode, "isolation", envStr("HOSTEL_ISOLATION", "direct"), "isolation mode: direct | bwrap")
	fs.StringVar(&c.DefaultBed, "default-bed", envStr("HOSTEL_DEFAULT_BED", "default"), "bed id used when a request omits one")
	fs.StringVar(&c.ShellPath, "shell", envStr("HOSTEL_SHELL", "/bin/bash"), "shell for bed sessions")
	idle := fs.Duration("bed-idle-timeout", envDur("HOSTEL_BED_IDLE_TIMEOUT", 30*time.Minute), "reap a bed after this idle duration (0=never)")
	fs.IntVar(&c.MaxBeds, "max-beds", envInt("HOSTEL_MAX_BEDS", 0), "max concurrent beds, 0=unlimited (default bed exempt)")
	// Ignore parse errors for unknown flags in tests; flag prints usage itself.
	_ = fs.Parse(args)
	c.BedIdleTimeout = *idle
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
