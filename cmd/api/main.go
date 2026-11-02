// Command api is the HopCharlie platform API: it accepts migration plans,
// records human review, runs the migration engine, streams its events, and
// reports environments, traffic and the audit log.
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

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/api"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/apps"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/auth"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/security"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func main() {
	addr := flag.String("addr", envOr("PLATFORM_ADDR", ":8080"), "listen address")
	stateDir := flag.String("state-dir", envOr("PLATFORM_STATE_DIR", filepath.Join(".hopcharlie", "runs")), "engine run state directory")
	insecure := flag.Bool("insecure-no-auth", false, "run without tokens; every caller is the operator \"dev\" (local development only)")
	drain := flag.Duration("shutdown-wait", 15*time.Minute, "how long to wait for active migrations on shutdown")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	fatal := func(msg string, args ...any) {
		log.Error(msg, args...)
		os.Exit(2)
	}

	tokens, err := auth.ParseTokens(os.Getenv("PLATFORM_TOKENS"))
	if err != nil {
		fatal("PLATFORM_TOKENS", "err", err)
	}
	if t := os.Getenv("PLATFORM_API_TOKEN"); t != "" { // the first version's single operator token
		tokens.Add("operator", apiv1.RoleOperator, t)
	}
	if tokens.Len() == 0 && !*insecure {
		fatal("no tokens: set PLATFORM_TOKENS (name:role:token,...) or use -insecure-no-auth for local development")
	}

	dbURL := os.Getenv("PLATFORM_DATABASE_URL")
	if dbURL == "" {
		fatal("PLATFORM_DATABASE_URL is not set (postgres://user:pass@host:5432/db)")
	}
	ctx := context.Background()
	openCtx, cancelOpen := context.WithTimeout(ctx, 30*time.Second)
	st, err := store.Open(openCtx, dbURL)
	cancelOpen()
	if err != nil {
		fatal("open platform database", "err", err)
	}
	defer st.Close()

	envPath := os.Getenv("PLATFORM_ENVS_FILE")
	if envPath == "" {
		fatal("PLATFORM_ENVS_FILE is not set (see examples/envs.yaml)")
	}
	envCfg, err := envs.LoadConfig(envPath)
	if err != nil {
		fatal("PLATFORM_ENVS_FILE", "err", err)
	}
	envSvc, err := envs.New(envCfg, envs.Options{})
	if err != nil {
		fatal("environments", "err", err)
	}

	policy := plans.DefaultPolicy()
	if v := os.Getenv("PLATFORM_PLAN_ENV_ALLOW"); v != "" {
		// Password variables either DSN of any plan may use.
		policy.PasswordEnvs = splitList(v)
		for _, name := range policy.PasswordEnvs {
			if err := plans.CheckPasswordEnv(name); err != nil {
				fatal("PLATFORM_PLAN_ENV_ALLOW", "err", err)
			}
		}
	}
	policy.GatewayURL = os.Getenv("PLATFORM_GATEWAY_URL")
	policy.DSNHostSuffixes = splitList(os.Getenv("PLATFORM_DSN_HOST_SUFFIXES"))
	if len(policy.DSNHostSuffixes) == 0 {
		log.Warn("PLATFORM_DSN_HOST_SUFFIXES is not set: a plan may point its DSN at any host; approvers must check dbHost in the plan summary")
	}

	// Optional: apps to draft plans for. A plan may use an app's password
	// variable only in a DSN to that app's database host.
	var appCat *apps.Catalog
	if path := os.Getenv("PLATFORM_APPS_FILE"); path != "" {
		appCfg, err := apps.LoadConfig(path)
		if err == nil {
			appCat, err = apps.New(appCfg, envCfg.Environments, policy.GatewayURL)
		}
		if err != nil {
			fatal("PLATFORM_APPS_FILE", "err", err)
		}
		policy.HostPasswordEnvs = appCat.PasswordHosts()
	}

	// One gateway client for the runner (canary, request counts) and the API.
	// The engine builds its own from the plan; both use GATEWAY_TOKEN.
	var gw *gateway.Client
	if policy.GatewayURL != "" {
		gw = &gateway.Client{BaseURL: policy.GatewayURL, Token: os.Getenv("GATEWAY_TOKEN"), HTTP: &http.Client{Timeout: 10 * time.Second}}
	}

	// Runs are not tied to request contexts: an operator closing the browser
	// must not abort a migration. runCtx is only cancelled on a forced shutdown.
	runCtx, cancelRuns := context.WithCancel(ctx)
	defer cancelRuns()
	rcfg := runs.Config{StateDir: *stateDir, Store: st, Log: log}
	if gw != nil {
		rcfg.Traffic = gw
	}
	runner := runs.New(runCtx, rcfg)
	planSvc := plans.New(policy, st)
	planSvc.SetStateDir(*stateDir)

	if n, err := planSvc.RecoverStarting(ctx); err != nil {
		fatal("recover plans", "err", err)
	} else if n > 0 {
		log.Warn("released plans left in starting by a previous crash", "count", n)
	}
	if n, err := runner.Recover(ctx); err != nil {
		fatal("recover runs", "err", err)
	} else if n > 0 {
		log.Warn("marked runs interrupted by the previous shutdown; resume them to continue", "count", n)
	}

	deps := api.Deps{Plans: planSvc, Runs: runner, Store: st, Envs: envSvc, EnvNotFound: envs.ErrNotFound}
	deps.Apps = appCat
	if gw != nil {
		deps.Traffic = gw
	}
	// The security view reads every cluster, the AWS account and our own audit
	// log; GCP and Cloudflare are reported as not connected.
	deps.Security = security.New(security.Collectors(security.Config{
		Environments:      envCfg.Environments,
		AWSRegion:         os.Getenv("PLATFORM_AWS_REGION"),
		Audit:             st,
		DSNHostSuffixes:   policy.DSNHostSuffixes,
		GatewayConfigured: gw != nil,
	}), security.Options{Log: log})
	deps.Agents = agentlog.New(st)
	srv := &http.Server{
		Addr: *addr,
		Handler: api.New(api.Config{
			Tokens:      tokens,
			Insecure:    *insecure,
			CORSOrigins: splitList(os.Getenv("PLATFORM_CORS_ORIGINS")),
			Log:         log,
		}, deps),
		ReadHeaderTimeout: 10 * time.Second,
	}

	sig, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Info("platform api listening", "addr", *addr, "tokens", tokens.Len(), "insecure", *insecure,
			"gatewayURL", policy.GatewayURL, "environments", envSvc.Names())
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen", "err", err)
			os.Exit(1)
		}
	}()
	<-sig.Done()
	stop() // a second signal now kills the process

	log.Info("shutting down", "activeRuns", runner.Active(), "interruptedWaiting", runner.InterruptWaiting())
	shCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	_ = srv.Shutdown(shCtx)
	cancel()

	// Let active migrations finish instead of interrupting a switchover.
	waitCtx, cancelWait := context.WithTimeout(ctx, *drain)
	defer cancelWait()
	if err := runner.Wait(waitCtx); err != nil {
		log.Warn("migrations still active; cancelling (engine rolls back before promotion, halts after)", "activeRuns", runner.Active())
		cancelRuns()
		final, cancelFinal := context.WithTimeout(ctx, 3*time.Minute)
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
