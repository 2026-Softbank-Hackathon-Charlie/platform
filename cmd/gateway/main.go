// Command gateway is the HopCharlie traffic gateway.
//
// It sits in front of every environment, behind one public proxy (the shared
// ALB of the management cluster), and proxies user traffic to the backend
// environments. The migration engine drives it
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
	appDomain := flag.String("app-domain", os.Getenv("GATEWAY_APP_DOMAIN"), "route Host <app>.<domain> to the app registered as <app> (PUT /apps/<app>), 404 for an unknown <app>; other hosts go to the default app. Empty sends every request to the default app")
	defaultApp := flag.String("default-app", envOr("GATEWAY_DEFAULT_APP", gateway.DefaultAppName), "name of the app that -backends serve: <name>.<app-domain> and /apps/<name> reach it")
	drain := flag.Duration("drain-timeout", gateway.DefaultDrainTimeout, "how long a hold waits for in-flight writes")
	maxHold := flag.Duration("max-hold", gateway.DefaultMaxHold, "a held write that waited this long gets 503 hold_timeout and is not forwarded; the hold itself lasts until release")
	maxHeld := flag.Int("max-held", gateway.DefaultMaxHeld, "waiting writes beyond this get 503")
	writeTimeout := flag.Duration("write-timeout", gateway.DefaultWriteTimeout, "how long a forwarded write may take; writes are not cancelled when their client leaves")
	bodyReadTimeout := flag.Duration("held-body-timeout", gateway.DefaultBodyReadTimeout, "maximum initial body read while a write is held")
	adminBodyReadTimeout := flag.Duration("admin-body-timeout", gateway.DefaultAdminBodyReadTimeout, "maximum admin JSON body receive time; excludes drain and dispatch waits")
	maxConcurrentWrites := flag.Int("max-concurrent-writes", gateway.DefaultMaxConcurrentWrites, "maximum writes dispatched per app at once")
	maxBufferedBytes := flag.Int64("max-buffered-bytes", gateway.DefaultMaxBufferedBytes, "global held-body memory reservation limit")
	healthInterval := flag.Duration("health-interval", gateway.DefaultHealthInterval, "backend health check interval")
	healthPath := flag.String("health-path", gateway.DefaultHealthPath, "backend health check path")
	connRecycle := flag.Duration("conn-recycle", gateway.DefaultConnRecycle, "how often idle backend connections are closed, so the next request resolves the backend's name again")
	insecure := flag.Bool("insecure-no-auth", false, "run the admin API without GATEWAY_ADMIN_TOKEN (local development only)")
	aiApp := flag.String("ai-app", "", "enable bounded AI demo on this dedicated default app (requires AI_GATEWAY_TOKEN)")
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
	var aiCfg *gateway.AIDemoConfig
	if *aiApp != "" {
		aiCfg = &gateway.AIDemoConfig{App: *aiApp, Token: os.Getenv("AI_GATEWAY_TOKEN")}
	}
	gw, err := gateway.New(gateway.Config{
		AIDemo:               aiCfg,
		Backends:             bs,
		InitialBackend:       *initial,
		StateFile:            *stateFile,
		AdminToken:           token,
		AdminBodyReadTimeout: *adminBodyReadTimeout,
		AppDomain:            *appDomain,
		DefaultApp:           *defaultApp,
		DrainTimeout:         *drain,
		MaxHold:              *maxHold,
		MaxHeld:              *maxHeld,
		WriteTimeout:         *writeTimeout,
		BodyReadTimeout:      *bodyReadTimeout,
		MaxConcurrentWrites:  *maxConcurrentWrites,
		MaxBufferedBytes:     *maxBufferedBytes,
		HealthInterval:       *healthInterval,
		HealthPath:           *healthPath,
		ConnRecycle:          *connRecycle,
		Log:                  log,
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
		"backends", names, "stateFile", *stateFile, "appDomain", *appDomain, "defaultApp", *defaultApp)
	<-sig.Done()
	stop() // a second signal now kills the process

	log.Info("shutting down")
	shutdown(log, gw, public, admin, 10*time.Second)
	stopHealth()
	gw.Close()
}

// shutdown stops the servers without dropping a request silently. Held writes
// are answered 503 shutting_down first (the hold stays recorded, so a restart
// keeps holding), then the proxy finishes its in-flight requests, and only
// then does the admin API stop: the engine can reach it until the proxy is
// done, and admin calls in progress get their own time to answer.
func shutdown(log *slog.Logger, gw *gateway.Gateway, public, admin *http.Server, timeout time.Duration) {
	gw.BeginShutdown()
	for _, s := range []*http.Server{public, admin} {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		if err := s.Shutdown(ctx); err != nil {
			log.Warn("requests still in flight at shutdown", "addr", s.Addr, "err", err)
		}
		cancel()
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
