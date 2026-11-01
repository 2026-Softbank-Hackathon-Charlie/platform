// Command api is the HopCharlie platform API: it accepts migration plans,
// records human approval, runs the migration engine and streams its events.
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
	"strings"
	"syscall"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/api"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
)

func main() {
	addr := flag.String("addr", envOr("PLATFORM_ADDR", ":8080"), "listen address")
	stateDir := flag.String("state-dir", envOr("PLATFORM_STATE_DIR", filepath.Join(".hopcharlie", "runs")), "engine run state directory")
	insecure := flag.Bool("insecure-no-auth", false, "run without PLATFORM_API_TOKEN (local development only)")
	drain := flag.Duration("shutdown-wait", 15*time.Minute, "how long to wait for active migrations on shutdown")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	token := os.Getenv("PLATFORM_API_TOKEN")
	if token == "" && !*insecure {
		log.Error("PLATFORM_API_TOKEN is not set (use -insecure-no-auth for local development)")
		os.Exit(2)
	}

	policy := plans.DefaultPolicy()
	if v := os.Getenv("PLATFORM_PLAN_ENV_ALLOW"); v != "" {
		policy.EnvAllow = splitList(v)
	}
	policy.GatewayURL = os.Getenv("PLATFORM_GATEWAY_URL")
	policy.DSNHostSuffixes = splitList(os.Getenv("PLATFORM_DSN_HOST_SUFFIXES"))

	// Runs are not tied to request contexts: an operator closing the browser
	// must not abort a migration. runCtx is only cancelled on a forced shutdown.
	runCtx, cancelRuns := context.WithCancel(context.Background())
	defer cancelRuns()
	runner := runs.New(runCtx, *stateDir, runs.EngineFactory)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.New(api.Config{Token: token, Log: log}, plans.New(policy), runner),
		ReadHeaderTimeout: 10 * time.Second,
	}

	sig, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Info("platform api listening", "addr", *addr, "auth", token != "", "gatewayURL", policy.GatewayURL)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen", "err", err)
			os.Exit(1)
		}
	}()
	<-sig.Done()
	stop() // a second signal now kills the process

	log.Info("shutting down", "activeRuns", runner.Active())
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = srv.Shutdown(shCtx)
	cancel()

	// Let active migrations finish instead of interrupting a switchover.
	waitCtx, cancelWait := context.WithTimeout(context.Background(), *drain)
	defer cancelWait()
	if err := runner.Wait(waitCtx); err != nil {
		log.Warn("migrations still active; cancelling (engine rolls back before promotion, halts after)", "activeRuns", runner.Active())
		cancelRuns()
		final, cancelFinal := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancelFinal()
		_ = runner.Wait(final)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
