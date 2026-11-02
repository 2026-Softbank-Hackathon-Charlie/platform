// Command gateway is the HopCharlie traffic gateway.
//
// It sits outside every cluster (behind Cloudflare Tunnel) and proxies user
// traffic to the backend environments. The migration engine drives it
// through the admin API during cutover (pkg/gateway in migration-engine):
//
//	GET  /healthz                     200 once the config is loaded
//	POST /v1/writes/hold              hold new writes; 200 once in-flight writes drained
//	POST /v1/route {"backend":"eks"}  writes and 100% of reads to that backend
//	POST /v1/writes/release           dispatch held writes to the current write backend
//
// See internal/gateway for the full admin API, routing rules and metrics.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
)

func main() {
	listen := flag.String("listen", envOr("GATEWAY_LISTEN", ":8000"), "public proxy listen address")
	adminAddr := flag.String("admin-addr", envOr("GATEWAY_ADMIN_ADDR", ":9901"), "admin API listen address")
	backends := flag.String("backends", os.Getenv("GATEWAY_BACKENDS"), "backends as name=url,... e.g. gke=http://shop.gke.ts.net,eks=http://shop.eks.ts.net")
	initial := flag.String("initial-backend", os.Getenv("GATEWAY_INITIAL_BACKEND"), "write backend when there is no state file (default: first backend)")
	stateFile := flag.String("state-file", envOr("GATEWAY_STATE_FILE", filepath.Join(".hopcharlie", "gateway-state.json")), "routing and hold state, kept across restarts")
	drain := flag.Duration("drain-timeout", gateway.DefaultDrainTimeout, "how long a hold waits for in-flight writes")
	maxHold := flag.Duration("max-hold", gateway.DefaultMaxHold, "held writes are released to the current write backend after this")
	maxHeld := flag.Int("max-held", gateway.DefaultMaxHeld, "waiting writes beyond this get 503")
	healthInterval := flag.Duration("health-interval", gateway.DefaultHealthInterval, "backend health check interval")
	healthPath := flag.String("health-path", gateway.DefaultHealthPath, "backend health check path")
	insecure := flag.Bool("insecure-no-auth", false, "run the admin API without GATEWAY_ADMIN_TOKEN (local development only)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	token := os.Getenv("GATEWAY_ADMIN_TOKEN")
	if token == "" && !*insecure {
		log.Error("GATEWAY_ADMIN_TOKEN is not set (use -insecure-no-auth for local development)")
		os.Exit(2)
	}
	bs, err := gateway.ParseBackends(*backends)
	if err != nil {
		log.Error("invalid -backends / GATEWAY_BACKENDS", "err", err)
		os.Exit(2)
	}
	gw, err := gateway.New(gateway.Config{
		Backends:       bs,
		InitialBackend: *initial,
		StateFile:      *stateFile,
		AdminToken:     token,
		DrainTimeout:   *drain,
		MaxHold:        *maxHold,
		MaxHeld:        *maxHeld,
		HealthInterval: *healthInterval,
		HealthPath:     *healthPath,
		Log:            log,
	})
	if err != nil {
		log.Error("start gateway", "err", err)
		os.Exit(1)
	}

	// No ReadTimeout or WriteTimeout: held writes wait up to max-hold and
	// streamed responses can run long.
	public := &http.Server{Addr: *listen, Handler: gw.Proxy(), ReadHeaderTimeout: 10 * time.Second}
	admin := &http.Server{Addr: *adminAddr, Handler: gw.Admin(), ReadHeaderTimeout: 10 * time.Second}

	sig, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	healthCtx, stopHealth := context.WithCancel(context.Background())
	go gw.Run(healthCtx)
	for _, s := range []*http.Server{public, admin} {
		go func() {
			if err := s.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("listen", "addr", s.Addr, "err", err)
				os.Exit(1)
			}
		}()
	}
	names := make([]string, len(bs))
	for i, b := range bs {
		names[i] = b.Name + "=" + b.URL.Redacted()
	}
	log.Info("gateway listening", "listen", *listen, "adminAddr", *adminAddr, "auth", token != "",
		"backends", names, "stateFile", *stateFile)
	<-sig.Done()
	stop() // a second signal now kills the process

	log.Info("shutting down")
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, s := range []*http.Server{public, admin} {
		wg.Go(func() {
			if err := s.Shutdown(shCtx); err != nil {
				log.Warn("requests still in flight at shutdown", "addr", s.Addr, "err", err)
			}
		})
	}
	wg.Wait()
	stopHealth()
	gw.Close()
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
