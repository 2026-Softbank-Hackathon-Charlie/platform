// Command api is the HopCharlie platform API: it accepts migration plans,
// records human review, runs the migration engine, streams its events, and
// reports environments, traffic and the audit log.
package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/cnpg"
	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/agentlog"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/aiobs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/airun"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/api"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/apps"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/auth"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/dbroute"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/deploy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/fileguard"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/github"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/job"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/capacity"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/cloudpolicy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/gate"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/paritygate"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/pgversion"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/probes"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/reach"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/residency"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/retention"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/security"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"golang.org/x/term"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	authorizationclient "k8s.io/client-go/kubernetes/typed/authorization/v1"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
)

func main() {
	addr := flag.String("addr", envOr("PLATFORM_ADDR", ":8080"), "listen address")
	stateDir := flag.String("state-dir", envOr("PLATFORM_STATE_DIR", filepath.Join(".hopcharlie", "runs")), "engine run state directory")
	insecure := flag.Bool("insecure-no-auth", false, "run without tokens or administrators; every caller is the operator \"dev\" (local development only)")
	drain := flag.Duration("shutdown-wait", 15*time.Minute, "how long to wait for active migrations on shutdown")
	hashPw := flag.Bool("hash-password", false, "read a password from stdin, print its bcrypt hash for PLATFORM_ADMINS and exit")
	flag.Parse()

	if *hashPw {
		if err := hashPassword(os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, "hash-password:", err)
			os.Exit(2)
		}
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	fatal := func(msg string, args ...any) {
		log.Error(msg, args...)
		os.Exit(2)
	}

	tokens, err := auth.ParseTokens(os.Getenv("PLATFORM_TOKENS"))
	if err != nil {
		fatal("PLATFORM_TOKENS", "err", err)
	}
	writtenTokens, err := auth.ParseWrittenTokens(os.Getenv("PLATFORM_WRITTEN_RECORD_TOKENS"))
	if err != nil {
		fatal("PLATFORM_WRITTEN_RECORD_TOKENS", "err", err)
	}
	if t := os.Getenv("PLATFORM_API_TOKEN"); t != "" { // the first version's single operator token
		tokens.Add("operator", apiv1.RoleOperator, t)
	}
	admins, err := auth.ParseAdmins(os.Getenv("PLATFORM_ADMINS"), auth.MinHashCost)
	if err != nil {
		fatal("PLATFORM_ADMINS", "err", err)
	}
	if tokens.Len() == 0 && admins.Len() == 0 && !*insecure {
		fatal("no tokens or administrators: set PLATFORM_TOKENS (name:role:token,...) or PLATFORM_ADMINS (name:bcrypt-hash,...), or use -insecure-no-auth for local development")
	}
	// Optional: the key app secrets are stored under (ADR 0019). A value that
	// is not base64 of 32 bytes stops the server.
	secretsKey, err := secrets.FromEnv(os.Getenv)
	if err != nil {
		fatal("PLATFORM_SECRETS_KEY", "err", err)
	}
	if secretsKey == nil {
		log.Warn("PLATFORM_SECRETS_KEY is not set: the app secrets API answers 503 secrets_not_configured")
	} else {
		log.Info("app secrets enabled", "keyVersion", secrets.KeyVersion, "keyCheck", secretsKey.KeyCheck())
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
	if secretsKey != nil {
		// Secrets stored under another key cannot be read or deployed; say
		// so now rather than at the first deployment.
		checkCtx, cancelCheck := context.WithTimeout(ctx, 30*time.Second)
		switch n, err := secretsKey.OtherKeyRows(checkCtx, st); {
		case err != nil:
			log.Warn("could not check the stored app secrets against PLATFORM_SECRETS_KEY", "err", err)
		case n > 0:
			log.Error("stored app secrets were written with another PLATFORM_SECRETS_KEY: they cannot be read or deployed until the key that wrote them is back", "rows", n, "keyCheck", secretsKey.KeyCheck())
		}
		cancelCheck()
	}

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
	// What each environment can take: sealing needs the cluster's
	// sealed-secrets certificate, buckets need a storage driver (ADR 0016).
	// A certificate that is missing now is read again when it is needed.
	for _, c := range envCfg.Environments {
		storageDriver, bucketPrefix, sealing := "none", "", "none"
		if c.Storage != nil {
			storageDriver, bucketPrefix = c.Storage.Driver, c.Storage.BucketPrefix
			if _, err := envSvc.Storage(c.Name); err != nil {
				storageDriver = "unavailable"
				log.Warn("storage driver could not be built: buckets and app keys in this environment fail until the API restarts with it fixed", "env", c.Name, "driver", c.Storage.Driver, "err", err)
			}
		}
		switch sl, err := envSvc.Sealer(c.Name); {
		case errors.Is(err, envs.ErrSealingNotConfigured):
		case err != nil:
			sealing = "unavailable"
			log.Warn("sealed-secrets certificate is not usable yet: deployments that need sealing fail until it is", "env", c.Name, "path", c.SealedSecretsCert, "err", err)
		default:
			sealing = sl.Fingerprint()
		}
		log.Info("environment sealing and storage", "env", c.Name, "storage", storageDriver, "bucketPrefix", bucketPrefix, "sealing", sealing)
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
		policy.AppDBHosts = appCat.AppHosts()
	}

	// Buckets files may move between: the apps' buckets and any listed here.
	// rclone reads each remote's backend and credentials from the server's
	// environment (RCLONE_CONFIG_<REMOTE>_*), never from the plan.
	for _, b := range splitList(os.Getenv("PLATFORM_PLAN_BUCKETS")) {
		if err := plans.CheckBucket(b); err != nil {
			fatal("PLATFORM_PLAN_BUCKETS", "err", err)
		}
		policy.Buckets = append(policy.Buckets, b)
	}
	if appCat != nil {
		for _, b := range appCat.Buckets() {
			if !slices.Contains(policy.Buckets, b) {
				policy.Buckets = append(policy.Buckets, b)
			}
		}
	}
	for _, b := range policy.Buckets {
		if env := plans.RcloneTypeEnv(b); os.Getenv(env) == "" {
			log.Warn("rclone remote is not configured: plans that move files to or from this bucket will fail", "bucket", b, "missing", env)
		}
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
	rcfg := runs.Config{StateDir: *stateDir, Store: st, Log: log, ClusterLease: st.TryClusterLease}
	if gw != nil {
		rcfg.Traffic = gw
		// A plan's gateway.url is the root or an app's scope (plans.Policy):
		// canary, request counts and observe use the same scope as the engine.
		rcfg.TrafficAt = func(u string) runs.Traffic {
			return &gateway.Client{BaseURL: u, Token: gw.Token, HTTP: gw.HTTP}
		}
		// With the public proxy known, the canary sends synthetic reads through
		// it, so that an app without users still has its target judged.
		if proxy := os.Getenv("PLATFORM_GATEWAY_PROXY_URL"); proxy != "" {
			p := &gateway.Prober{ProxyURL: proxy, AppDomain: os.Getenv("PLATFORM_APP_DOMAIN"), HTTP: gateway.ProbeClient()}
			rcfg.Probe = p.Probe
		}
	}
	planSvc := plans.New(policy, st)
	planSvc.SetStateDir(*stateDir)

	// Optional (ADR 0030): an app registered in exactly two environments gets
	// a database pair, born primary in its first environment and a replica in
	// the other, so that it can be migrated between them.
	appDBTopology := false
	switch v := os.Getenv("PLATFORM_APP_DB_TOPOLOGY"); v {
	case "", "0":
	case "1":
		appDBTopology = true
		if secretsKey == nil {
			log.Warn("PLATFORM_APP_DB_TOPOLOGY is on, but PLATFORM_SECRETS_KEY is not set: apps are registered without a database pair")
		}
	default:
		fatal("PLATFORM_APP_DB_TOPOLOGY must be 1 or 0", "value", v)
	}
	// Optional (ADR 0042): the onboarding API, which answers 501 until it
	// is built. Off, its routes are not there.
	onboarding := false
	switch v := os.Getenv(onboard.FeatureEnv); v {
	case "", "0":
	case "1":
		onboarding = true
	default:
		fatal(onboard.FeatureEnv+" must be 1 or 0", "value", v)
	}
	// Optional (ADR 0032): registered apps' files move between their
	// buckets through rclone remotes named after the environments, which
	// use the platform's own identities.
	appFiles, err := apps.EnableFiles(os.Getenv("PLATFORM_APP_FILES"), envCfg.Environments, os.LookupEnv, os.Setenv)
	if err != nil {
		fatal("PLATFORM_APP_FILES", "err", err)
	}
	// Plans for those apps: drafted from the store, their database hosts and
	// password variables allowed and their passwords put into the
	// environment on every parse, so a new app needs no restart. Apps
	// without a database (ADR 0049) need no host templates, so the registry
	// is always there; without templates no app has a database endpoint.
	var appDBEnvs []string
	var tmpls map[string]string
	if v := os.Getenv("PLATFORM_APP_DB_HOST_TEMPLATES"); v != "" {
		if secretsKey == nil {
			fatal("PLATFORM_APP_DB_HOST_TEMPLATES needs PLATFORM_SECRETS_KEY to read the database passwords")
		}
		tmpls, err = apps.ParseHostTemplates(v, envSvc.Names())
		if err != nil {
			fatal("PLATFORM_APP_DB_HOST_TEMPLATES", "err", err)
		}
		for env := range tmpls {
			appDBEnvs = append(appDBEnvs, env)
		}
		log.Info("plans for registered apps with a database pair enabled", "hostTemplates", tmpls)
	}
	if appCat == nil {
		appCat = apps.Empty(envCfg.Environments, policy.GatewayURL)
	}
	reg := &apps.Registry{Store: st, Secrets: secretsKey, HostTemplates: tmpls}
	if appFiles != nil {
		reg.BucketPrefixes = appFiles.BucketPrefixes
		log.Info("registered apps' files enabled", "remotesSet", appFiles.Set, "remotesKept", appFiles.Kept)
	}
	appCat = appCat.WithRegistry(reg)
	planSvc.SetAppDatabases(appCat)

	// Optional: the residency policy. With it every new run (not a rollback)
	// starts only if the source's live inventory may be copied to the target.
	var bucketInspector api.BucketInspector
	if path := os.Getenv("PLATFORM_RESIDENCY_POLICY_FILE"); path != "" {
		g, err := residencyGate(ctx, path, envSvc.Names())
		if err != nil {
			fatal("PLATFORM_RESIDENCY_POLICY_FILE", "err", err)
		}
		planSvc.SetResidencyGate(g)
		rcfg.FileSyncer = g.FileSyncer
		// An onboarding's plan reaches its source bucket through its own
		// legacy remote (ADR 0042), set once the onboarding runner exists.
		g.SetRemote(func(p *plan.Plan) fileguard.Remote {
			if f := onboardRemote.Load(); f != nil {
				return (*f)(p)
			}
			return fileguard.Remote{}
		})
		bucketInspector = g
		log.Info("residency gate enabled", "policyDigest", g.PolicyDigest())
	} else {
		log.Warn("PLATFORM_RESIDENCY_POLICY_FILE is not set: residency gate disabled; migrations start without a data residency check")
	}

	// Optional: measure the source and target before every new run and refuse
	// one that would clearly fail (too few target connections, a replica that
	// cannot catch up). Adds a few seconds to each start.
	switch v := os.Getenv("PLATFORM_CAPACITY_GATE"); v {
	case "on":
		planSvc.SetCapacityGate(capacity.Gate{})
		log.Info("capacity gate enabled", "window", capacity.DefaultWindow)
	case "", "off":
		log.Info("PLATFORM_CAPACITY_GATE is not on: migrations start without a capacity check")
	default:
		fatal("PLATFORM_CAPACITY_GATE must be on or off", "value", v)
	}

	// Optional: record a parity report (target RBAC for the app's service
	// account, Prometheus scrape health) on every new run's start. Audit mode
	// only: the report goes to the audit log and never stops a run.
	if path := os.Getenv("PLATFORM_PARITY_FILE"); path != "" {
		g, err := parityGate(path, envSvc)
		if err != nil {
			fatal("PLATFORM_PARITY_FILE", "err", err)
		}
		planSvc.SetParityGate(g)
		log.Info("parity recording enabled (audit only, never blocks)")
	} else {
		log.Info("PLATFORM_PARITY_FILE is not set: no parity report is recorded")
	}

	// On unless turned off: refuse a run whose source and target clearly run
	// different PostgreSQL major versions, which physical replication cannot
	// cross. An unknown version never blocks.
	switch v := os.Getenv("PLATFORM_VERSION_GATE"); v {
	case "", "on":
		planSvc.SetVersionGate(pgversion.Checker{})
		log.Info("PostgreSQL major version gate enabled")
	case "off":
		log.Info("PLATFORM_VERSION_GATE is off: migrations start without a PostgreSQL major version check")
	default:
		fatal("PLATFORM_VERSION_GATE must be on or off", "value", v)
	}

	// On unless turned off: refuse to approve a plan whose source database name
	// does not resolve, or whose port does not accept connections, from here.
	// The engine would stop at its precheck right after the start. Anything the
	// check cannot establish never blocks.
	switch v := os.Getenv("PLATFORM_REACH_GATE"); v {
	case "", "on":
		planSvc.SetReachGate(reach.Checker{})
		log.Info("source database reachability gate enabled")
	case "off":
		log.Info("PLATFORM_REACH_GATE is off: plans are approved without checking that the source database is reachable")
	default:
		fatal("PLATFORM_REACH_GATE must be on or off", "value", v)
	}

	rcfg.RetentionPlan = planSvc.Load
	// Explicit server opt-in. Retention is never read from an AI/client plan.
	if value := os.Getenv("PLATFORM_SOURCE_RETENTION"); value != "" && value != "off" {
		period, err := time.ParseDuration(value)
		if err != nil || period < time.Minute || period > 30*24*time.Hour {
			fatal("PLATFORM_SOURCE_RETENTION must be off or a duration between 1m and 720h")
		}
		if gw == nil {
			fatal("source retention requires the platform gateway")
		}
		backend, err := retention.NewKube(envCfg.Environments, func(u string) retention.Traffic { return &gateway.Client{BaseURL: u, Token: gw.Token, HTTP: gw.HTTP} })
		if err != nil {
			fatal("initialize source retention", "err", err)
		}
		rcfg.Retention, rcfg.RetentionPeriod, rcfg.RetentionPlan = backend, period, planSvc.Load
		log.Warn("source retention enabled; source database deletion ends immediate rollback, PVCs retained", "period", period)
	}
	runner := runs.New(runCtx, rcfg)
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
	if rcfg.Retention != nil {
		retentionCtx, stopRetention := context.WithCancel(runCtx)
		defer stopRetention()
		go runner.RunRetention(retentionCtx)
	}
	deps.Apps = appCat
	deps.Buckets = bucketInspector
	// Draft plans can carry the source's numbers (POST /v1/plans/draft with
	// sourceStats), measured like the capacity gate does, on request only.
	deps.SourceStats = capacity.Prober{}
	if gw != nil {
		deps.Traffic = gw
		deps.TrafficApp = func(app string) api.Traffic { return gw.App(app) }
	}
	// The security view reads every cluster, the AWS account and our own audit
	// log; GCP and Cloudflare are reported as not connected.
	deps.Security = security.New(security.Collectors(security.Config{
		Environments:      envCfg.Environments,
		AWSRegion:         os.Getenv("PLATFORM_AWS_REGION"),
		Audit:             st,
		DSNHostSuffixes:   policy.DSNHostSuffixes,
		GatewayConfigured: gw != nil,
		StorageRows:       st,
		Storage:           envSvc.Storage,
		Log:               log,
	}), security.Options{Log: log})
	// Scans the image scan workflow posted to POST /v1/security/scans (SEC-04).
	deps.ImageScans = security.StoredScans{Store: st}
	// KEV and EPSS for the vulnerability report (SEC-07): public feeds, no credentials.
	deps.Exploits = security.NewExploitIntel()
	// Read the KEV catalog now, off the request path, so the 7-day exception
	// limit for KEV-listed CVEs (SEC-08) holds right after a restart.
	go deps.Exploits.Warm(ctx)
	// A running agent session with no event for PLATFORM_AGENT_IDLE is closed
	// as cancelled, so an agent that crashed does not leave it open forever.
	agentIdle := agentlog.DefaultIdle
	if v := os.Getenv("PLATFORM_AGENT_IDLE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			fatal("PLATFORM_AGENT_IDLE must be a positive duration such as 30m", "value", v)
		}
		agentIdle = d
	}
	agents := agentlog.New(st)
	deps.Agents = agents
	// The AI runner as a Job (ai-sandbox hopai), started from the console.
	// Without PLATFORM_AIRUN_JOB_TEMPLATE it is off and /v1/ai-runs answers 503.
	// A bad template turns only /v1/ai-runs off (503), not the whole API.
	aiRuns, err := airun.FromEnv(os.Getenv)
	if err != nil {
		log.Error("AI runner is off: its Job template cannot be used; /v1/ai-runs answers 503", "err", err)
		aiRuns = nil
	}
	if aiRuns != nil {
		deps.AIRuns = aiRuns
	} else {
		log.Info("PLATFORM_AIRUN_JOB_TEMPLATE is not set: the console cannot start the AI runner; run hopai by hand")
	}
	// Optional: read what the AI runner did from the observability stack's
	// ClickHouse (the HyperDX store) so the console's migration map can show the
	// real calls. A read-only ClickHouse user is expected; the password comes
	// from a mounted file, never an environment value. A bad setting turns only
	// /v1/ai-activity off (503).
	if u := os.Getenv("PLATFORM_AIOBS_CLICKHOUSE_URL"); u != "" {
		pw := ""
		if f := os.Getenv("PLATFORM_AIOBS_CLICKHOUSE_PASSWORD_FILE"); f != "" {
			raw, err := os.ReadFile(f)
			if err != nil {
				log.Error("AI activity is off: the ClickHouse password file cannot be read; /v1/ai-activity answers 503", "err", err)
				u = ""
			}
			pw = strings.TrimSpace(string(raw))
		}
		if u != "" {
			c, err := aiobs.New(u, os.Getenv("PLATFORM_AIOBS_CLICKHOUSE_USER"), pw, os.Getenv("PLATFORM_AIOBS_CLICKHOUSE_TABLE"))
			if err != nil {
				log.Error("AI activity is off; /v1/ai-activity answers 503", "err", err)
			} else {
				deps.AIActivity = c
				deps.AppObs = c
				log.Info("AI activity enabled", "table", c.Table)
			}
		}
	} else {
		log.Info("PLATFORM_AIOBS_CLICKHOUSE_URL is not set: /v1/ai-activity answers 503")
	}
	deps.Secrets = secretsKey
	deps.StorageConfigured = func(env string) bool { _, err := envSvc.Storage(env); return err == nil }
	deps.SealingConfigured = func(env string) bool { _, err := envSvc.Sealer(env); return err == nil }
	// The readiness of a database pair (ADR 0043) reads the app's CNPG
	// Clusters with the platform's own workload credentials.
	workloads := workloadClients(envSvc)
	deps.DBCluster = dbClusterReader(workloads)
	// Adding an environment back waits until its earlier workloads are gone.
	deps.WorkloadResidue = workloadResidue(workloads)
	if gw != nil {
		deps.Gateway = gw
	}
	go agents.RunSweeper(runCtx, agentIdle, log)
	// Optional (ADR 0030): the management cluster's routes to registered apps' databases.
	dbRoutes, err := dbroute.FromEnv(os.Getenv, envSvc.Names(), st, log)
	if err != nil {
		fatal("PLATFORM_APP_DB_ROUTE_*", "err", err)
	} else if dbRoutes != nil {
		deps.DBRoutes = dbRoutes
		go dbRoutes.Run(runCtx)
	}
	// Optional: the GitHub App that apps are registered and deployed from.
	gh, err := github.FromEnv(os.Getenv)
	if err != nil {
		fatal("GitHub App", "err", err)
	}
	if gh != nil {
		deps.GitHub = gh
	} else {
		log.Warn("PLATFORM_GITHUB_APP_ID is not set: registering apps from GitHub answers 503 github_not_configured")
	}
	webhookSecret := os.Getenv("PLATFORM_GITHUB_WEBHOOK_SECRET")
	if webhookSecret == "" {
		log.Warn("PLATFORM_GITHUB_WEBHOOK_SECRET is not set: the GitHub webhook answers 503 and pushes do not deploy")
	}

	// Deployments of apps registered from GitHub. Without
	// PLATFORM_DEPLOY_BUILDER there is no deployer and they stay queued.
	// Deployments are not tied to requests either; on shutdown they stop and
	// resume after the restart.
	deployCtx, cancelDeploy := context.WithCancel(ctx)
	defer cancelDeploy()
	deployOpts := deploy.EnvOptions{
		Store:     st,
		AppDomain: envOr("PLATFORM_APP_DOMAIN", api.DefaultAppDomain),
		Log:       log,
		Kube: func(env string) (kubernetes.Interface, error) {
			c, ok := envSvc.Config(env)
			if !ok {
				return nil, errors.New("environment " + env + " is not in PLATFORM_ENVS_FILE")
			}
			return envs.NewKubeClient(c, 10*time.Second)
		},
		Secrets: secretsKey,
		Sealer:  envSvc.Sealer,
		// A deployment waits while its app's onboarding cuts over (ADR 0042).
		CuttingOver: job.CuttingOver(st),
		Storage:     envSvc.Storage,
		Envs:        envSvc.Names(),
		StorageDriver: func(env string) string {
			if c, ok := envSvc.Config(env); ok && c.Storage != nil {
				return c.Storage.Driver
			}
			return ""
		},
	}
	if gh != nil {
		deployOpts.GitHub = gh
	}
	if gw != nil {
		deployOpts.Gateway = gw
	}
	deployer, err := deploy.FromEnv(ctx, os.Getenv, deployOpts)
	if err != nil {
		fatal("deployments", "err", err)
	}
	if deployer != nil {
		if err := deployer.Start(deployCtx); err != nil {
			fatal("start deployments", "err", err)
		}
		deps.Deployer, deps.Remover = deployer, deployer
		deps.Backends = deployer.BackendURLs()
	}
	if onboarding {
		deps.Onboarding = onboardingConfig(ctx, log, envSvc, secretsKey, gh, aiRuns, gw, dbRoutes, planSvc, runner, workloads)
	}
	apiServer := api.New(api.Config{
		WrittenTokens:   writtenTokens,
		Tokens:          tokens,
		Insecure:        *insecure,
		Admins:          admins,
		ConsoleURL:      os.Getenv("PLATFORM_CONSOLE_URL"),
		OnpremPublicURL: os.Getenv("PLATFORM_ONPREM_PUBLIC_URL"),
		CORSOrigins:     splitList(os.Getenv("PLATFORM_CORS_ORIGINS")),
		Log:             log,

		GitHubWebhookSecret: webhookSecret,
		AppDomain:           os.Getenv("PLATFORM_APP_DOMAIN"),
		GitOpsRepo:          os.Getenv("PLATFORM_GITOPS_REPO"),
		AppDBTopology:       appDBTopology,
		AppDBEnvs:           appDBEnvs,
		Onboarding:          onboarding,
	}, deps)
	if r := apiServer.Onboardings(); r != nil {
		f := r.Remote
		onboardRemote.Store(&f)
		// From the rows, also after a restart; steps outlive requests.
		go r.Run(runCtx)
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           apiServer,
		ReadHeaderTimeout: 10 * time.Second,
	}

	sig, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Info("platform api listening", "addr", *addr, "tokens", tokens.Len(), "admins", admins.Len(), "insecure", *insecure,
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
	if deployer != nil {
		cancelDeploy() // unfinished deployments resume after the restart
		waitDeploy, cancelWaitDeploy := context.WithTimeout(ctx, 30*time.Second)
		_ = deployer.Wait(waitDeploy)
		cancelWaitDeploy()
	}

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

// onboardRemote is the onboarding runner's Remote once it exists.
var onboardRemote atomic.Pointer[func(*plan.Plan) fileguard.Remote]

// residencyGate loads the reviewed residency policy. Every environment it
// names must be registered, or a typo would silently deny every migration.
func residencyGate(ctx context.Context, path string, registered []string) (*gate.Gate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read the residency policy file")
	}
	cfg, err := residency.ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	for _, id := range cfg.EnvironmentIDs() {
		if !slices.Contains(registered, id) {
			return nil, errors.New("residency policy names environment " + id + ", which is not in PLATFORM_ENVS_FILE")
		}
	}
	return gate.New(ctx, cfg, nil)
}

// hashPassword reads one password from in, without echo when in is a
// terminal, and writes its bcrypt hash for PLATFORM_ADMINS to out. The prompt
// goes to prompt so out holds only the hash. When in is not a terminal (a
// pipe, or Git Bash's mintty without ConPTY, where typing still echoes) it
// says so on prompt and reads the first line.
func hashPassword(in *os.File, out, prompt io.Writer) error {
	var pw []byte
	var err error
	if fd := int(in.Fd()); term.IsTerminal(fd) {
		fmt.Fprint(prompt, "Password: ")
		pw, err = term.ReadPassword(fd)
		fmt.Fprintln(prompt)
	} else {
		fmt.Fprintln(prompt, "stdin is not a terminal; reading the password from stdin (it may be visible)")
		pw, err = readLine(in)
	}
	if err != nil {
		return err
	}
	h, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, h)
	return err
}

// readLine returns the first line of r without its line ending.
func readLine(r io.Reader) ([]byte, error) {
	line, err := bufio.NewReader(r).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	line = bytes.TrimSuffix(line, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r")), nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// workloadClients returns each environment's dynamic client for its
// workload cluster, built on first use from its kubeconfig context with the
// platform's own credentials and kept; a failed build is tried again next
// time.
func workloadClients(envSvc *envs.Service) func(env string) (dynamic.Interface, error) {
	var mu sync.Mutex
	clients := map[string]dynamic.Interface{}
	return func(env string) (dynamic.Interface, error) {
		mu.Lock()
		defer mu.Unlock()
		if dyn, ok := clients[env]; ok {
			return dyn, nil
		}
		c, known := envSvc.Config(env)
		if !known {
			return nil, errors.New("environment " + env + " is not in PLATFORM_ENVS_FILE")
		}
		_, d, err := envs.NewReadClients(c, api.DBClusterTimeout)
		if err != nil {
			return nil, err
		}
		clients[env] = d
		return d, nil
	}
}

// dbClusterReader reads a CNPG Cluster in an environment's workload cluster.
func dbClusterReader(clients func(string) (dynamic.Interface, error)) api.DBClusterReader {
	return func(ctx context.Context, env, namespace, name string) (*cnpg.Status, error) {
		dyn, err := clients(env)
		if err != nil {
			return nil, err
		}
		return cnpg.New(dyn, namespace, name).Get(ctx)
	}
}

// workloadResidue lists one Service and one Pod of a namespace in an
// environment's workload cluster (the platform's observer role lists both,
// infra ADR 0063), for whether an environment removed from an app is gone
// (ADR 0043).
func workloadResidue(clients func(string) (dynamic.Interface, error)) api.WorkloadResidue {
	kinds := []struct {
		kind string
		gvr  schema.GroupVersionResource
	}{
		{"Service", schema.GroupVersionResource{Version: "v1", Resource: "services"}},
		{"Pod", schema.GroupVersionResource{Version: "v1", Resource: "pods"}},
	}
	return func(ctx context.Context, env, namespace string) (string, error) {
		dyn, err := clients(env)
		if err != nil {
			return "", err
		}
		for _, k := range kinds {
			list, err := dyn.Resource(k.gvr).Namespace(namespace).List(ctx, metav1.ListOptions{Limit: 1})
			if err != nil {
				return "", err
			}
			if len(list.Items) > 0 {
				return k.kind + " " + list.Items[0].GetName(), nil
			}
		}
		return "", nil
	}
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

// parityGate loads the parity file. Each environment's SubjectAccessReview
// client is built on first use from the environment's kubeconfig context,
// with the platform's own credentials for that cluster.
func parityGate(path string, envSvc *envs.Service) (*paritygate.Gate, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("cannot read the parity file")
	}
	cfg, err := paritygate.ParseConfig(raw, envSvc.Names())
	if err != nil {
		return nil, err
	}
	sar := func(env string) (authorizationclient.SubjectAccessReviewInterface, error) {
		c, ok := envSvc.Config(env)
		if !ok {
			return nil, errors.New("unknown environment")
		}
		k, err := envs.NewKubeClient(c, 10*time.Second)
		if err != nil {
			return nil, err
		}
		return k.AuthorizationV1().SubjectAccessReviews(), nil
	}
	resources := func(env string) (metadata.Interface, dynamic.Interface, error) {
		c, ok := envSvc.Config(env)
		if !ok {
			return nil, nil, errors.New("unknown environment")
		}
		return envs.NewReadClients(c, 10*time.Second)
	}
	g := paritygate.NewWithResources(cfg, sar, nil, resources)
	var cloudEnvironments []string
	for _, name := range envSvc.Names() {
		c, _ := envSvc.Config(name)
		if c.Provider == "aws" || c.Provider == "gcp" {
			cloudEnvironments = append(cloudEnvironments, name)
		}
	}
	g.SetCloudPolicyFetcher(cloudpolicy.New(func(environment string) (cloudpolicy.Environment, error) {
		c, ok := envSvc.Config(environment)
		if !ok {
			return cloudpolicy.Environment{}, errors.New("unknown environment")
		}
		return cloudpolicy.Environment{Provider: c.Provider, Region: c.Region}, nil
	}, nil, nil).Check, cloudEnvironments...)
	g.SetUsageFetcher(probes.ServiceUsageFetcher(func(environment string) (rest.Interface, error) {
		c, ok := envSvc.Config(environment)
		if !ok {
			return nil, errors.New("unknown environment")
		}
		k, err := envs.NewKubeClient(c, 5*time.Second)
		if err != nil {
			return nil, err
		}
		return k.CoreV1().RESTClient(), nil
	}))
	return g, nil
}
