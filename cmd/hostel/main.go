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
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/qiankunli/hostel/internal/amenity"
	"github.com/qiankunli/hostel/internal/bed"
	"github.com/qiankunli/hostel/internal/config"
	"github.com/qiankunli/hostel/internal/isolation"
	"github.com/qiankunli/hostel/internal/store"
	"github.com/qiankunli/hostel/internal/web"
)

func main() {
	cfg := config.Load(os.Args[1:])

	iso := isolation.New(cfg.IsolationMode, cfg.WorkspaceRoot)
	if !iso.Available() {
		log.Printf("hostel: isolator %q unavailable on this host — commands will run with reduced/no isolation", iso.Name())
	}

	// Amenity manager: shared facilities light up per deployment. Chromium is
	// registered when launch (binary) or attach (--chromium-cdp-url) is
	// possible; otherwise the facility is honestly absent.
	amenities := amenity.NewRegistry()
	if br, ok := amenity.NewChromium(amenity.ChromiumConfig{
		ExecPath: cfg.ChromiumPath,
		CDPURL:   cfg.ChromiumCDPURL,
		IdleStop: cfg.ChromiumIdleStop,
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
