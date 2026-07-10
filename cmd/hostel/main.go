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

// Command hostel is a generic sandbox data-plane manager: it runs one or many
// isolated "beds" and serves an OpenSandbox-compatible HTTP API over them.
// Standalone-capable, but primarily meant to run inside a pod. See docs/design.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/qiankunli/hostel/extensions"
	"github.com/qiankunli/hostel/internal/amenity"
	"github.com/qiankunli/hostel/internal/bed"
	"github.com/qiankunli/hostel/internal/bedinit"
	"github.com/qiankunli/hostel/internal/config"
	"github.com/qiankunli/hostel/internal/isolation"
	"github.com/qiankunli/hostel/internal/store"
	"github.com/qiankunli/hostel/internal/web"
)

// version is stamped at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// Multi-call dispatch (extensions/): the image symlinks in-bed tool names
	// (e.g. playwright, playwright-cli) to this binary and argv[0] selects the
	// tool — one binary, tools ride along for the cost of a symlink. Must be
	// first: an extension invocation is never a daemon invocation.
	if name := filepath.Base(os.Args[0]); name != "hostel" {
		if run, ok := extensions.Lookup(name); ok {
			os.Exit(run(os.Args[1:]))
		}
	}

	// Isolation re-exec confiners (room mechanisms): before anything else, since
	// the argv is `hostel <subcmd> ... -- <cmd>...`, not flags. The daemon must
	// keep its privileges, so both mechanisms confine a self-re-exec, not hostel.
	if len(os.Args) >= 2 {
		switch os.Args[1] {
		case isolation.ConfineArg: // landlock: __confine <dataDir> -- <cmd>...
			os.Exit(runConfine(os.Args[2:]))
		case isolation.AsUserArg: // uid: __asuser <uid> <dataDir> -- <cmd>...
			os.Exit(runAsUser(os.Args[2:]))
		case bedinit.InitArg: // per-bed init/spawner: __bedinit --socket S --bed B
			os.Exit(bedinit.Run(os.Args[2:]))
		}
	}

	cfg := config.Load(os.Args[1:])

	// Preflight subcommands used by the image (no curl needed). Handled after
	// config.Load so --health probes the SAME addr the server would listen on
	// (flag > env > default), not a separately-guessed one.
	if cfg.ShowVersion {
		fmt.Println(version)
		return
	}
	if cfg.HealthCheck {
		os.Exit(healthCheck(cfg.Addr))
	}

	log.Printf("hostel %s starting", version)

	// New resolves the requested level against the environment ceiling and
	// logs the outcome; the returned isolator is always usable.
	iso := isolation.New(cfg.IsolationMode, cfg.WorkspaceRoot)

	// Amenity manager: shared facilities light up per deployment. Chromium is
	// registered when launch (binary) or attach (--chromium-cdp-url) is
	// possible; otherwise the facility is honestly absent.
	amenities := amenity.NewRegistry()
	if br, ok := amenity.NewChromium(amenity.ChromiumConfig{
		ExecPath:  cfg.ChromiumPath,
		CDPURL:    cfg.ChromiumCDPURL,
		IdleStop:  cfg.ChromiumIdleStop,
		DebugPort: cfg.ChromiumDebugPort,
	}); ok {
		amenities.Register(br.(amenity.Amenity))
		log.Printf("hostel: amenity chromium registered (attach=%v)", cfg.ChromiumCDPURL != "")
	}

	// Fail fast on a misconfigured store: booting with silent noop while the
	// operator believes snapshots are on would be quiet data loss.
	st, err := store.New(context.Background(), store.Config{
		Backend:  cfg.StoreBackend,
		Bucket:   cfg.S3Bucket,
		Prefix:   cfg.S3Prefix,
		Endpoint: cfg.S3Endpoint,
	})
	if err != nil {
		log.Fatalf("hostel: init store: %v", err)
	}

	mgr, err := bed.NewManager(cfg.WorkspaceRoot, cfg.DefaultBed, cfg.ShellPath, iso, amenities, cfg.MaxBeds, st)
	if err != nil {
		log.Fatalf("hostel: init bed manager: %v", err)
	}
	mgr.SetLuggageLimits(cfg.LuggageHighBytes, cfg.LuggageLowBytes)
	// Per-bed browser endpoint injection (PLAYWRIGHT_MCP_CDP_ENDPOINT): beds
	// reach hostel over loopback (shared pod net ns). Minting is lazy-safe, so
	// this is on whenever the browser amenity can proxy.
	if addr := loopbackAddr(cfg.Addr); addr != "" {
		mgr.SetCDPAdvertise(addr)
	}

	// Per-bed init spawner (docs/design.md 〈进程树〉 S1): auto probes once at
	// boot; a failed probe (non-linux, odd deployment) is an honest downgrade
	// to in-process forking, logged, never a startup failure.
	if cfg.BedInit != "off" {
		if exe, err := os.Executable(); err != nil {
			log.Printf("hostel: bed-init disabled (executable path: %v)", err)
		} else if err := mgr.EnableBedInit(exe); err != nil {
			log.Printf("hostel: bed-init unavailable, using in-process spawner: %v", err)
		} else {
			log.Printf("hostel: bed-init spawner enabled")
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Idle bed reaper.
	if cfg.BedIdleTimeout > 0 {
		go func() {
			t := time.NewTicker(cfg.BedIdleTimeout / 2)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if reaped := mgr.CollectIdle(cfg.BedIdleTimeout); len(reaped) > 0 {
						log.Printf("hostel: reaped idle beds: %v", reaped)
					}
				}
			}
		}()
	}

	// Luggage GC: keep evicted beds' local dirs (warm cache) under the disk
	// watermarks. Fixed cadence — the watermarks, not the tick rate, decide
	// how much disk luggage may hold.
	if cfg.LuggageHighBytes > 0 {
		go func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if reaped := mgr.CollectLuggage(ctx); len(reaped) > 0 {
						log.Printf("hostel: reaped luggage: %v", reaped)
					}
				}
			}
		}()
	}

	// Periodic snapshot safety net.
	if cfg.PersistInterval > 0 {
		go func() {
			t := time.NewTicker(cfg.PersistInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if done := mgr.PersistDirty(ctx); len(done) > 0 {
						log.Printf("hostel: persisted beds: %v", done)
					}
				}
			}
		}()
	}

	srv := &http.Server{Addr: cfg.Addr, Handler: web.NewServer(mgr).Handler()}
	go func() {
		log.Printf("hostel: listening on %s (isolation=%s, workspace-root=%s, default-bed=%s)",
			cfg.Addr, iso.Name(), cfg.WorkspaceRoot, cfg.DefaultBed)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("hostel: server error: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("hostel: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

// runConfine implements `hostel __confine <dataDir> -- <cmd> <args>...`: apply
// the room (Landlock) restrictions to THIS process, then exec the real command
// so it inherits them. Returns a process exit code (it only returns on error;
// success replaces the process image).
func runConfine(args []string) int {
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	if sep < 1 || sep+1 >= len(args) {
		fmt.Fprintln(os.Stderr, "hostel __confine: usage: __confine <dataDir> -- <cmd>...")
		return 2
	}
	dataDir := args[0]
	cmd := args[sep+1:]

	if err := isolation.ApplyConfine(dataDir); err != nil {
		fmt.Fprintf(os.Stderr, "hostel __confine: apply landlock: %v\n", err)
		return 1
	}
	path, err := exec.LookPath(cmd[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostel __confine: %s: %v\n", cmd[0], err)
		return 127
	}
	if err := syscall.Exec(path, cmd, os.Environ()); err != nil {
		fmt.Fprintf(os.Stderr, "hostel __confine: exec %s: %v\n", path, err)
		return 126
	}
	return 0 // unreachable
}

// runAsUser implements `hostel __asuser <uid> <dataDir> -- <cmd> <args>...`:
// drop THIS process to the bed uid (and no_new_privs), enter the data dir, then
// exec the real command so it inherits the reduced identity. Returns a process
// exit code (only on error; success replaces the process image).
func runAsUser(args []string) int {
	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
			break
		}
	}
	// Need at least <uid> <dataDir> before the "--", and a command after it.
	if sep < 2 || sep+1 >= len(args) {
		fmt.Fprintln(os.Stderr, "hostel __asuser: usage: __asuser <uid> <dataDir> -- <cmd>...")
		return 2
	}
	uid, err := strconv.Atoi(args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostel __asuser: bad uid %q: %v\n", args[0], err)
		return 2
	}
	dataDir := args[1]
	cmd := args[sep+1:]

	if err := isolation.ApplyAsUser(uid, dataDir); err != nil {
		fmt.Fprintf(os.Stderr, "hostel __asuser: %v\n", err)
		return 1
	}
	path, err := exec.LookPath(cmd[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostel __asuser: %s: %v\n", cmd[0], err)
		return 127
	}
	// The bed uid has no /etc/passwd entry; give tools a sane HOME (its own
	// workspace) and USER so bash and friends don't choke on the unknown uid.
	// Filter any inherited HOME/USER/LOGNAME first: appending would leave two
	// copies, and a libc getenv() takes the FIRST (the daemon's) — bash happens
	// to be last-wins, but don't rely on the exec target being a shell.
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "HOME=") || strings.HasPrefix(kv, "USER=") || strings.HasPrefix(kv, "LOGNAME=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "HOME="+dataDir, "USER=hostel-bed", "LOGNAME=hostel-bed")
	if err := syscall.Exec(path, cmd, env); err != nil {
		fmt.Fprintf(os.Stderr, "hostel __asuser: exec %s: %v\n", path, err)
		return 126
	}
	return 0 // unreachable
}

// loopbackAddr rewrites the listen address into the loopback host:port a bed
// can dial: wildcard or empty hosts become 127.0.0.1, concrete hosts stay.
// Empty on unparseable input — callers treat that as "don't advertise".
func loopbackAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return ""
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// healthCheck GETs the local /healthz for the image HEALTHCHECK — no external
// tool required. addr is the server's resolved listen address, so the probe
// can never target the wrong port.
func healthCheck(addr string) int {
	if addr == "" {
		addr = config.DefaultAddr
	}
	if addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
