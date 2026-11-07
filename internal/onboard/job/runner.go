package job

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// Errors of the operations; the API maps them to its codes.
var (
	ErrNotFound      = errors.New("onboarding not found")                          // 404 onboarding_not_found
	ErrExists        = errors.New("the app has an onboarding")                     // 409 onboarding_exists
	ErrAppExists     = errors.New("an app of that name is registered")             // 409 app_exists
	ErrState         = errors.New("not in a state that allows it")                 // 409 onboarding_state_conflict
	ErrDeleting      = errors.New("the onboarding is being deleted")               // 409 onboarding_deleting
	ErrHeadMismatch  = errors.New("the PR head is not the commit approved")        // 409 pr_head_mismatch
	ErrInvalid       = errors.New("invalid onboarding request")                    // 400 invalid_request
	ErrNotConfigured = errors.New("this server cannot do that step of onboarding") // 503
)

// Config is the runner's dependencies; nil ports fail the steps that need
// them with ErrNotConfigured (and the onboarding with it).
type Config struct {
	Store   Store
	Secrets *secrets.Cipher

	Discover Discoverer
	Connect  Connector
	GitHub   GitHub
	AI       AI
	Apps     Apps
	Kube     Kube
	Files    Files
	DNS      DNS
	Gateway  Gateway
	Routes   Routes
	Cutover  Cutover
	SourceDB SourceDB

	// AppDomain is <label>.<AppDomain> of the service URLs (hopcharlie.dev).
	AppDomain string
	// Envs are the platform's environments; the target must be one.
	Envs []string
	// Poll is how often waiting states are looked at again (default 5s).
	Poll time.Duration
	Now  func() time.Time
	Log  *slog.Logger
}

// Runner drives onboardings. One goroutine at a time works on one.
type Runner struct {
	cfg Config

	mu   sync.Mutex
	busy map[string]bool
	wg   sync.WaitGroup
	ctx  context.Context // set by Run; steps outlive requests

	// cutoverMu serializes putting LEGACY_PGPASSWORD into the process
	// environment and the reservation that expands it.
	cutoverMu sync.Mutex
}

// New returns a runner; Run starts it.
func New(cfg Config) *Runner {
	if cfg.Poll <= 0 {
		cfg.Poll = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	return &Runner{cfg: cfg, busy: map[string]bool{}, ctx: context.Background()}
}

// Run works on every onboarding that is not done from where its row says
// it is, now (after a restart) and every Poll, until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	r.mu.Lock()
	r.ctx = ctx
	r.mu.Unlock()
	t := time.NewTicker(r.cfg.Poll)
	defer t.Stop()
	for {
		r.Tick(ctx)
		select {
		case <-ctx.Done():
			r.wg.Wait()
			return
		case <-t.C:
		}
	}
}

// Tick starts work on every onboarding that has some.
func (r *Runner) Tick(ctx context.Context) {
	list, err := r.cfg.Store.ListOnboardings(ctx)
	if err != nil {
		r.cfg.Log.Warn("list onboardings", "err", err)
		return
	}
	for _, o := range list {
		if o.State != store.OnboardingDone {
			r.kick(o.ID)
		}
	}
}

// Wait waits for the work started.
func (r *Runner) Wait() { r.wg.Wait() }

// kick works on id in the background unless that is going on.
func (r *Runner) kick(id string) {
	r.mu.Lock()
	if r.busy[id] {
		r.mu.Unlock()
		return
	}
	r.busy[id] = true
	ctx := r.ctx
	r.wg.Add(1)
	r.mu.Unlock()
	go func() {
		defer r.wg.Done()
		defer func() {
			r.mu.Lock()
			delete(r.busy, id)
			r.mu.Unlock()
		}()
		r.Advance(ctx, id)
	}()
}

// Advance steps id until it waits; tests call it directly.
func (r *Runner) Advance(ctx context.Context, id string) {
	for i := 0; i < 50 && ctx.Err() == nil; i++ {
		moved, err := r.step(ctx, id)
		if err != nil {
			if !errors.Is(err, ErrNotFound) && !errors.Is(err, store.ErrConflict) {
				r.cfg.Log.Warn("onboarding step", "onboarding", id, "err", err)
			}
			return
		}
		if !moved {
			return
		}
	}
}

// --- rows ---

func (r *Runner) load(ctx context.Context, id string) (*rec, error) {
	row, err := r.cfg.Store.GetOnboarding(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return decode(row)
}

// update applies change to the newest row of id while its state is one of
// from (any when empty), and writes it with the state change returns (""
// keeps it). A row another writer changed meanwhile is read again and
// change applied again, up to a few times; so a step's result is never
// lost to a concurrent DELETE that only set deleting.
func (r *Runner) update(ctx context.Context, id string, from []string, change func(x *rec) (string, error)) (*rec, error) {
	for attempt := 0; ; attempt++ {
		x, err := r.load(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(from) > 0 && !contains(from, x.row.State) {
			return nil, fmt.Errorf("%w: onboarding %s is %s", ErrState, id, x.row.State)
		}
		prev := x.row.State
		to, err := change(x)
		if err != nil {
			return nil, err
		}
		if to == "" {
			to = prev
		}
		x.row.State = to
		x.row.UpdatedAt = r.cfg.Now()
		if err := x.encode(); err != nil {
			return nil, err
		}
		row, err := r.cfg.Store.UpdateOnboarding(ctx, x.row, []string{prev})
		if errors.Is(err, store.ErrConflict) && attempt < 4 {
			continue
		}
		if err != nil {
			return nil, err
		}
		x.row = row
		if to != prev {
			r.event(ctx, x, apiv1.OnboardingEventKindState, "", nil)
		}
		return x, nil
	}
}

func (r *Runner) event(ctx context.Context, x *rec, kind apiv1.OnboardingEventKind, msg string, data map[string]any) {
	e := store.OnboardingEvent{OnboardingID: x.row.ID, Time: r.cfg.Now(), State: x.row.State, Kind: string(kind), Message: msg}
	if data != nil {
		e.Data, _ = json.Marshal(data)
	}
	if _, err := r.cfg.Store.AppendOnboardingEvent(ctx, e); err != nil && !errors.Is(err, store.ErrNotFound) {
		r.cfg.Log.Warn("onboarding event", "onboarding", x.row.ID, "err", err)
	}
}

// fail ends the state with failed and why.
func (r *Runner) fail(ctx context.Context, id, state string, cause error) error {
	x, err := r.update(ctx, id, []string{state}, func(x *rec) (string, error) {
		st := apiv1.OnboardingState(state)
		x.doc.FailedState, x.doc.Failure = &st, ptr(cause.Error())
		return store.OnboardingFailed, nil
	})
	if err == nil {
		r.event(ctx, x, apiv1.OnboardingEventKindError, cause.Error(), nil)
	}
	return err
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// --- operations of the API ---

// Start creates an onboarding of repo answering at serviceURL and starts
// discovering it.
func (r *Runner) Start(ctx context.Context, req apiv1.OnboardingRequest, actor string) (apiv1.Onboarding, error) {
	app, err := r.appOf(req.ServiceUrl)
	if err != nil {
		return apiv1.Onboarding{}, err
	}
	if strings.Count(req.Repo, "/") != 1 || strings.HasSuffix(req.Repo, ".git") {
		return apiv1.Onboarding{}, fmt.Errorf("%w: repo must be owner/name", ErrInvalid)
	}
	target := ""
	if req.TargetEnv != nil {
		target = *req.TargetEnv
		if len(r.cfg.Envs) > 0 && !contains(r.cfg.Envs, target) {
			return apiv1.Onboarding{}, fmt.Errorf("%w: unknown environment %q", ErrInvalid, target)
		}
	}
	if _, err := r.cfg.Store.GetApp(ctx, app); err == nil {
		return apiv1.Onboarding{}, fmt.Errorf("%w: %s", ErrAppExists, app)
	} else if !errors.Is(err, store.ErrNotFound) {
		return apiv1.Onboarding{}, err
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return apiv1.Onboarding{}, err
	}
	id := onboard.IDPrefix + hex.EncodeToString(b[:])
	now := r.cfg.Now()
	x := &rec{doc: doc{Onboarding: apiv1.Onboarding{Id: id, App: app, Repo: req.Repo, ServiceUrl: strings.TrimRight(req.ServiceUrl, "/"),
		State: apiv1.OnboardingStateDiscovering, TargetEnv: target, CreatedBy: actor, CreatedAt: now, UpdatedAt: now,
		HumanActions: []apiv1.OnboardingHumanAction{}, LegacyChanges: []apiv1.OnboardingLegacyChange{},
		Report: apiv1.OnboardingReport{Blockers: []apiv1.OnboardingFinding{}, Warnings: []apiv1.OnboardingFinding{}}}}}
	x.row = store.Onboarding{ID: id, App: app, Repo: req.Repo, State: store.OnboardingDiscovering, CreatedAt: now, UpdatedAt: now}
	if err := x.encode(); err != nil {
		return apiv1.Onboarding{}, err
	}
	if err := r.cfg.Store.CreateOnboarding(ctx, x.row); err != nil {
		if errors.Is(err, store.ErrConflict) {
			return apiv1.Onboarding{}, fmt.Errorf("%w: %s", ErrExists, app)
		}
		return apiv1.Onboarding{}, err
	}
	r.event(ctx, x, apiv1.OnboardingEventKindState, "started by "+actor, nil)
	r.kick(id)
	return r.Get(ctx, id)
}

// appOf is the app name of https://<label>.<AppDomain>.
func (r *Runner) appOf(serviceURL string) (string, error) {
	u, err := url.Parse(serviceURL)
	if err != nil || u.Scheme != "https" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.User != nil || u.Port() != "" {
		return "", fmt.Errorf("%w: serviceUrl must be https://<label>.%s", ErrInvalid, r.cfg.AppDomain)
	}
	label, ok := strings.CutSuffix(strings.ToLower(u.Hostname()), "."+r.cfg.AppDomain)
	if !ok || strings.Contains(label, ".") || !onboard.AppName.MatchString(label) || strings.HasPrefix(label, onboard.IDPrefix) {
		return "", fmt.Errorf("%w: serviceUrl must be https://<label>.%s with an app name as the label", ErrInvalid, r.cfg.AppDomain)
	}
	return label, nil
}

// Get is one onboarding as the API shows it.
func (r *Runner) Get(ctx context.Context, id string) (apiv1.Onboarding, error) {
	row, err := r.cfg.Store.GetOnboarding(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return apiv1.Onboarding{}, ErrNotFound
	}
	if err != nil {
		return apiv1.Onboarding{}, err
	}
	return View(row)
}

// Approve approves the PR at headSha (awaiting_approval). A PR whose head
// moved is ErrHeadMismatch; one behind its base goes back to patching to
// be made again and tested (ErrState says so).
func (r *Runner) Approve(ctx context.Context, id, headSha, actor string) (apiv1.Onboarding, error) {
	x, err := r.load(ctx, id)
	if err != nil {
		return apiv1.Onboarding{}, err
	}
	if x.doc.Deleting {
		return apiv1.Onboarding{}, ErrDeleting
	}
	if x.row.State != store.OnboardingAwaitingApproval {
		return apiv1.Onboarding{}, fmt.Errorf("%w: onboarding %s is %s", ErrState, id, x.row.State)
	}
	if x.doc.Pr != nil {
		if r.cfg.GitHub == nil {
			return apiv1.Onboarding{}, ErrNotConfigured
		}
		pr, err := r.cfg.GitHub.GetPR(ctx, x.row.Repo, x.doc.Pr.Number)
		if err != nil {
			return apiv1.Onboarding{}, err
		}
		if pr.HeadSHA != headSha || pr.State != "open" {
			return apiv1.Onboarding{}, fmt.Errorf("%w: the PR head is %s", ErrHeadMismatch, pr.HeadSHA)
		}
		behind, err := r.cfg.GitHub.BehindBy(ctx, x.row.Repo, pr.BaseRef, onboard.Branch)
		if err != nil {
			return apiv1.Onboarding{}, err
		}
		if behind > 0 {
			_, err := r.update(ctx, id, []string{store.OnboardingAwaitingApproval}, func(x *rec) (string, error) {
				x.doc.Internal.HopaiJob, x.doc.Internal.CheckLog = "", ""
				x.doc.Check = nil
				return store.OnboardingPatching, nil
			})
			if err == nil {
				r.kick(id)
			}
			return apiv1.Onboarding{}, fmt.Errorf("%w: the PR is %d commits behind %s, so the branch is made again and tested", ErrState, behind, pr.BaseRef)
		}
	} else if x.facts != nil && x.facts.GitHub != nil && x.facts.GitHub.Head != headSha {
		return apiv1.Onboarding{}, fmt.Errorf("%w: the default branch is at %s", ErrHeadMismatch, x.facts.GitHub.Head)
	}
	x, err = r.update(ctx, id, []string{store.OnboardingAwaitingApproval}, func(x *rec) (string, error) {
		now := r.cfg.Now()
		x.doc.ApprovedAt, x.doc.ApprovedBy, x.doc.ApprovedSha = &now, &actor, &headSha
		return store.OnboardingConnecting, nil
	})
	if err != nil {
		return apiv1.Onboarding{}, err
	}
	r.kick(id)
	return View(x.row)
}

// Decommission confirms stopping the source instance (decommissioning,
// once the legacy backend and route are gone).
func (r *Runner) Decommission(ctx context.Context, id, actor string) (apiv1.Onboarding, error) {
	x, err := r.load(ctx, id)
	if err != nil {
		return apiv1.Onboarding{}, err
	}
	if x.row.State != store.OnboardingDecommissioning || !x.doc.Internal.LegacyRemoved || !x.doc.Internal.DeviceRemoved {
		return apiv1.Onboarding{}, fmt.Errorf("%w: onboarding %s is %s and not ready to stop the source", ErrState, id, x.row.State)
	}
	if r.cfg.Connect == nil {
		return apiv1.Onboarding{}, ErrNotConfigured
	}
	if err := r.cfg.Connect.StopInstance(ctx, r.connectReq(x)); err != nil {
		return apiv1.Onboarding{}, err
	}
	x, err = r.update(ctx, id, []string{store.OnboardingDecommissioning}, func(x *rec) (string, error) {
		return store.OnboardingDone, nil
	})
	if err != nil {
		return apiv1.Onboarding{}, err
	}
	r.event(ctx, x, apiv1.OnboardingEventKindStep, "source instance stopped, confirmed by "+actor, nil)
	return View(x.row)
}

// Resume goes on from failed (back to the state it failed in; ready when
// the cutover's plan was refused) or from waiting for a person.
func (r *Runner) Resume(ctx context.Context, id, actor string) (apiv1.Onboarding, error) {
	x, err := r.update(ctx, id, nil, func(x *rec) (string, error) {
		if x.doc.Deleting {
			return "", ErrDeleting
		}
		switch {
		case x.row.State == store.OnboardingFailed:
			to := x.effectiveState()
			if to == store.OnboardingCuttingOver && x.doc.RunId == nil {
				to = store.OnboardingReady
			}
			x.doc.FailedState, x.doc.Failure = nil, nil
			x.doc.Internal.Waiting = false
			return to, nil
		case x.doc.Internal.Waiting:
			x.doc.Internal.Waiting = false
			return "", nil
		}
		return "", fmt.Errorf("%w: onboarding %s is %s and waits for nobody", ErrState, x.row.ID, x.row.State)
	})
	if err != nil {
		return apiv1.Onboarding{}, err
	}
	r.event(ctx, x, apiv1.OnboardingEventKindStep, "resumed by "+actor, nil)
	r.kick(id)
	return View(x.row)
}

// Delete starts undoing an onboarding (202): from cutting_over on it
// cannot be.
func (r *Runner) Delete(ctx context.Context, id, actor string) (apiv1.Onboarding, error) {
	x, err := r.update(ctx, id, nil, func(x *rec) (string, error) {
		if !x.deletable() {
			return "", fmt.Errorf("%w: onboarding %s is past its cutover (%s) and cannot be deleted", ErrState, x.row.ID, x.effectiveState())
		}
		x.doc.Deleting, x.doc.Internal.DeleteRequested = true, true
		x.doc.Failure = nil
		return "", nil
	})
	if err != nil {
		return apiv1.Onboarding{}, err
	}
	r.event(ctx, x, apiv1.OnboardingEventKindStep, "deletion asked for by "+actor, nil)
	r.kick(id)
	return View(x.row)
}

// Active reports whether app has an onboarding that is not done, and
// whether it is being deleted.
func (r *Runner) Active(ctx context.Context, app string) (active, deleting bool, err error) {
	return ActiveFor(ctx, r.cfg.Store, app)
}

// ActiveFor is Active on a store.
func ActiveFor(ctx context.Context, st store.OnboardingStore, app string) (active, deleting bool, err error) {
	list, err := st.ListOnboardings(ctx)
	if err != nil {
		return false, false, err
	}
	for _, o := range list {
		if o.App == app && o.State != store.OnboardingDone {
			var d struct {
				Deleting bool `json:"deleting"`
			}
			_ = json.Unmarshal(o.Doc, &d)
			return true, d.Deleting, nil
		}
	}
	return false, false, nil
}

// CuttingOver reports whether app's onboarding is cutting over
// (deploy.Config.CuttingOver).
func CuttingOver(st store.OnboardingStore) func(ctx context.Context, app string) (bool, error) {
	return func(ctx context.Context, app string) (bool, error) {
		list, err := st.ListOnboardings(ctx)
		if err != nil {
			return false, err
		}
		for _, o := range list {
			if o.App == app && o.State == store.OnboardingCuttingOver {
				return true, nil
			}
		}
		return false, nil
	}
}

// PushAllowed reports whether repo has an onboarding in patching or
// checking that is not being deleted, which may get a push token (ADR
// 0042 "2.").
func PushAllowed(ctx context.Context, st store.OnboardingStore, repo string) (bool, error) {
	list, err := st.ListOnboardings(ctx)
	if err != nil {
		return false, err
	}
	for _, o := range list {
		if !strings.EqualFold(o.Repo, repo) || (o.State != store.OnboardingPatching && o.State != store.OnboardingChecking) {
			continue
		}
		var d struct {
			Deleting bool `json:"deleting"`
		}
		_ = json.Unmarshal(o.Doc, &d)
		if !d.Deleting {
			return true, nil
		}
	}
	return false, nil
}
