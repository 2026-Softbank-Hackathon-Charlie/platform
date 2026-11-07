package plans

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// OnboardingRole is store.Plan.SubmittedRole of a cutover plan an
// onboarding reserved itself (ADR 0042, ReserveOnboarding). No caller of
// the API has it, so only those plans are parsed with an onboarding's
// policy, also when their halted run is resumed (Load).
const OnboardingRole = "onboarding"

// LegacyPasswordEnv is the fence user's password variable of an
// onboarding's plan (internal/onboard.LegacyPasswordEnv).
const LegacyPasswordEnv = "LEGACY_PGPASSWORD"

// OnboardingScope is what an onboarding's policy adds to the server's for
// its own cutover plan (ADR 0042 addendum, "계약을 정하면서 정한 것").
type OnboardingScope struct {
	// App is the onboarded app.
	App string
	// SourceHost is the management cluster's route to the source,
	// db-legacy-<app>.<route namespace>.svc.cluster.local:5432: the only
	// host LegacyPasswordEnv is sent to, and one of the app's database
	// hosts for the gateway scope.
	SourceHost string
	// SourceBucket is "legacy:<bucket>", a registered bucket for this plan;
	// empty without files.
	SourceBucket string
	// SmokeURL is the target environment's backend URL of the app, the
	// only target.app.smokeUrl the plan may have (the engine needs one for
	// a cold app); its smoke requests must be GET or HEAD.
	SmokeURL string
	// Password is the fence user's password: LegacyPasswordEnv is set to
	// it only while the plan is parsed, under a lock, and unset right after.
	Password string
}

// legacyEnvMu serializes setting LegacyPasswordEnv for a parse.
var legacyEnvMu sync.Mutex

// parseOnboarding parses raw with the onboarding's policy, with
// LegacyPasswordEnv in the process environment for plan.Parse only.
func (s *Service) parseOnboarding(ctx context.Context, policy Policy, raw []byte, scope OnboardingScope) (*plan.Plan, apiv1.PlanSummary, error) {
	legacyEnvMu.Lock()
	defer legacyEnvMu.Unlock()
	_ = os.Setenv(LegacyPasswordEnv, scope.Password)
	defer os.Unsetenv(LegacyPasswordEnv)
	return s.parseWith(ctx, policy.withOnboarding(scope), raw)
}

// OnboardingScopes finds the scope of an onboarding's plan by its id, for
// Load. ok is false when no onboarding has that plan.
type OnboardingScopes func(ctx context.Context, planID string) (scope OnboardingScope, ok bool, err error)

// SetOnboardingScopes lets Load parse a reserved onboarding plan again.
func (s *Service) SetOnboardingScopes(f OnboardingScopes) { s.onboardings = f }

// withOnboarding returns p with o's additions and target.app and an
// external source allowed.
func (p Policy) withOnboarding(o OnboardingScope) Policy {
	p.HostPasswordEnvs = cloneHosts(p.HostPasswordEnvs)
	p.HostPasswordEnvs[LegacyPasswordEnv] = []string{o.SourceHost}
	p.AppDBHosts = cloneHosts(p.AppDBHosts)
	p.AppDBHosts[o.App] = append(slices.Clone(p.AppDBHosts[o.App]), o.SourceHost)
	p.Buckets = slices.Clone(p.Buckets)
	if o.SourceBucket != "" && CheckBucket(o.SourceBucket) == nil && !slices.Contains(p.Buckets, o.SourceBucket) {
		p.Buckets = append(p.Buckets, o.SourceBucket)
	}
	p.onboarding = &o
	return p
}

func cloneHosts(m map[string][]string) map[string][]string {
	out := make(map[string][]string, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	return out
}

// check checks what only an onboarding's plan may have: an
// external source reached at the scope's host, and a cold target app of
// the onboarded app's namespace.
func (o *OnboardingScope) check(pl *plan.Plan, hosts dbHosts) error {
	if !pl.Source.External() || pl.Source.Name != "legacy" {
		return fmt.Errorf("an onboarding's plan has the external source legacy")
	}
	if !strings.EqualFold(hosts.source, o.SourceHost) {
		return fmt.Errorf("an onboarding's source is %s", o.SourceHost)
	}
	a := pl.Target.App
	if a == nil || !a.Cold() || a.Kind != "Deployment" || !strings.HasPrefix(a.Name, o.App+"-") || a.SmokeURL != o.SmokeURL {
		return fmt.Errorf("an onboarding's target.app is the cold Deployment of app %s, smoked at %s", o.App, o.SmokeURL)
	}
	for _, r := range a.Smoke {
		if r.Method != "GET" && r.Method != "HEAD" {
			return fmt.Errorf("an onboarding's smoke requests only read")
		}
	}
	if pl.Target.Namespace != o.App {
		return fmt.Errorf("an onboarding's target namespace is %s", o.App)
	}
	return nil
}

// planIDOf reads the id of a plan as submitted.
func planIDOf(raw []byte) string {
	var v struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(bytes.NewReader(raw)).Decode(&v)
	return v.ID
}

// ReserveOnboarding stores the cutover plan an onboarding wrote (raw) and
// reserves it, as Reserve does an approved plan: the operator's approval
// is the onboarding's cutover request, by actor. It parses raw with the
// server's policy and scope's additions, then runs the residency gate. The
// version and capacity gates of an external source are the onboarding's
// (it compares the discovered facts with the chosen image and PVC); parity
// and retention do not run, as there is no source environment. A plan a
// gate refuses is stored rejected. The caller must call Started or Release.
func (s *Service) ReserveOnboarding(ctx context.Context, raw []byte, actor string, scope OnboardingScope) (Reserved, string, error) {
	policy, err := s.registeredPolicy(ctx)
	if err != nil {
		return Reserved{}, "", err
	}
	p, sum, err := s.parseOnboarding(ctx, policy, raw, scope)
	if err != nil {
		return Reserved{}, "", err
	}
	b, err := json.Marshal(sum)
	if err != nil {
		return Reserved{}, "", err
	}
	now := s.now()
	row := store.Plan{
		Ref: newRef("pln_"), PlanID: p.ID, Status: store.PlanStarting, Summary: b, Raw: append([]byte(nil), raw...),
		Revision: 1, SubmittedBy: actor, SubmittedRole: OnboardingRole, SubmittedAt: now,
		ReviewedBy: actor, ReviewedAt: &now, ReviewComment: "onboarding cutover",
	}
	if err := s.store.CreatePlan(ctx, row); err != nil {
		return Reserved{}, "", err
	}
	check, err := s.checkResidency(ctx, "", p)
	if err == nil {
		err = s.materialize(ctx, row.Ref, p)
	}
	if err != nil {
		_, _ = s.transition(ctx, row.Ref, []string{store.PlanStarting}, store.PlanRejected,
			store.PlanUpdate{ReviewedBy: actor, ReviewedAt: &now, ReviewComment: "refused at reservation: " + err.Error()})
		return Reserved{}, row.Ref, err
	}
	return Reserved{Plan: p, Summary: sum, Residency: check,
		Capacity: CapacityCheck{Outcome: OutcomeExternalSource}, Version: VersionCheck{Outcome: OutcomeExternalSource},
		Parity: ParityCheck{Outcome: OutcomeExternalSource}}, row.Ref, nil
}

// OutcomeExternalSource is the outcome of a gate an onboarding's plan does
// not run here ("외부 원본이라 생략").
const OutcomeExternalSource = "external_source"

// loadOnboarding parses a reserved onboarding plan again with its
// onboarding's scope, for resuming its halted run.
func (s *Service) loadOnboarding(ctx context.Context, row store.Plan) (*plan.Plan, error) {
	if s.onboardings == nil {
		return nil, fmt.Errorf("%w: plan %s is an onboarding's, and this server does not run onboardings", ErrInvalid, row.Ref)
	}
	scope, ok, err := s.onboardings(ctx, planIDOf(row.Raw))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("%w: plan %s belongs to no onboarding any more", ErrInvalid, row.Ref)
	}
	policy, err := s.registeredPolicy(ctx)
	if err != nil {
		return nil, err
	}
	p, _, err := s.parseOnboarding(ctx, policy, row.Raw, scope)
	return p, err
}
