package job

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
)

// doc is the onboarding row's doc: the API's apiv1.Onboarding and, under
// "internal", where the job is inside its state, so it resumes there after
// a restart. The API decodes the doc as apiv1.Onboarding and never shows
// internal.
type doc struct {
	apiv1.Onboarding
	Internal internal `json:"internal"`
}

type internal struct {
	// Waiting: a person must act first (humanActions); only resume goes on.
	Waiting bool `json:"waiting,omitempty"`
	// DeleteRequested: DELETE asked for the undo; cleared when it failed,
	// so only another DELETE retries it.
	DeleteRequested bool `json:"deleteRequested,omitempty"`
	// TargetPeer is the other environment of the app's database pair.
	TargetPeer string `json:"targetPeer,omitempty"`

	// patching and checking
	HopaiJob    string `json:"hopaiJob,omitempty"`
	CheckLog    string `json:"checkLog,omitempty"` // the failed test deployment's log, for hopai
	Branch      string `json:"branch,omitempty"`   // what the test app deploys: the PR branch, or the default branch
	TestApp     bool   `json:"testApp,omitempty"`  // the test app may exist
	SkipPatch   bool   `json:"skipPatch,omitempty"`
	HopaiResult string `json:"hopaiResult,omitempty"`

	// connecting
	WorkflowsSince *time.Time `json:"workflowsSince,omitempty"`
	Connected      bool       `json:"connected,omitempty"`
	SourceHost     string     `json:"sourceHost,omitempty"` // db-legacy-<app>.<ns>.svc.cluster.local:5432
	SourceChecked  bool       `json:"sourceChecked,omitempty"`

	// registering
	Registered   bool   `json:"registered,omitempty"`
	Deployment   string `json:"deployment,omitempty"`
	GatewayReady bool   `json:"gatewayReady,omitempty"`

	// syncing
	DNSAt *time.Time `json:"dnsAt,omitempty"`

	// cutting_over
	Attempt int    `json:"attempt,omitempty"`
	PlanRef string `json:"planRef,omitempty"`
	// PostRun are the steps after a successful run that are done.
	PostRun []string `json:"postRun,omitempty"`

	// decommissioning
	LegacyRemoved bool `json:"legacyRemoved,omitempty"`
	DeviceRemoved bool `json:"deviceRemoved,omitempty"`
}

// rec is a row with its doc and facts decoded.
type rec struct {
	row   store.Onboarding
	doc   doc
	facts *facts.Facts
}

func decode(row store.Onboarding) (*rec, error) {
	x := &rec{row: row}
	if err := json.Unmarshal(row.Doc, &x.doc); err != nil {
		return nil, fmt.Errorf("onboarding %s: doc: %w", row.ID, err)
	}
	if len(row.Facts) > 0 && string(row.Facts) != "null" {
		var f facts.Facts
		if err := json.Unmarshal(row.Facts, &f); err != nil {
			return nil, fmt.Errorf("onboarding %s: facts: %w", row.ID, err)
		}
		x.facts = &f
	}
	return x, nil
}

func (x *rec) encode() error {
	b, err := json.Marshal(x.doc)
	if err != nil {
		return err
	}
	x.row.Doc = b
	if x.facts != nil {
		if x.row.Facts, err = json.Marshal(x.facts); err != nil {
			return err
		}
	}
	return nil
}

// View is the API's view of a row: its doc without internal, with the
// row's columns.
func View(row store.Onboarding) (apiv1.Onboarding, error) {
	x, err := decode(row)
	if err != nil {
		return apiv1.Onboarding{}, err
	}
	o := x.doc.Onboarding
	o.Id, o.App, o.Repo, o.State = row.ID, row.App, row.Repo, apiv1.OnboardingState(row.State)
	o.CreatedAt, o.UpdatedAt = row.CreatedAt, row.UpdatedAt
	if o.HumanActions == nil {
		o.HumanActions = []apiv1.OnboardingHumanAction{}
	}
	if o.LegacyChanges == nil {
		o.LegacyChanges = []apiv1.OnboardingLegacyChange{}
	}
	if o.Report.Blockers == nil {
		o.Report.Blockers = []apiv1.OnboardingFinding{}
	}
	if o.Report.Warnings == nil {
		o.Report.Warnings = []apiv1.OnboardingFinding{}
	}
	return o, nil
}

// stateOrder is how far each state is, for DELETE and resume.
var stateOrder = map[string]int{
	store.OnboardingDiscovering: 0, store.OnboardingPatching: 1, store.OnboardingChecking: 2,
	store.OnboardingAwaitingApproval: 3, store.OnboardingConnecting: 4, store.OnboardingRegistering: 5,
	store.OnboardingSyncing: 6, store.OnboardingReady: 7, store.OnboardingCuttingOver: 8,
	store.OnboardingDecommissioning: 9, store.OnboardingDone: 10,
}

// effectiveState is the state a failed onboarding failed in, else its state.
func (x *rec) effectiveState() string {
	if x.row.State == store.OnboardingFailed && x.doc.FailedState != nil {
		return string(*x.doc.FailedState)
	}
	return x.row.State
}

// Deletable says DELETE may undo it: before cutting_over.
func (x *rec) deletable() bool {
	return stateOrder[x.effectiveState()] < stateOrder[store.OnboardingCuttingOver]
}

func (x *rec) addChange(c apiv1.OnboardingLegacyChange) {
	for _, have := range x.doc.LegacyChanges {
		if have.Kind == c.Kind && have.Target == c.Target && have.UndoneAt == nil {
			return
		}
	}
	x.doc.LegacyChanges = append(x.doc.LegacyChanges, c)
}

func (x *rec) hasChange(kind apiv1.OnboardingLegacyChangeKind, target string) bool {
	for _, c := range x.doc.LegacyChanges {
		if c.Kind == kind && c.Target == target && c.UndoneAt == nil {
			return true
		}
	}
	return false
}

func (x *rec) postRunDone(step string) bool {
	for _, s := range x.doc.Internal.PostRun {
		if s == step {
			return true
		}
	}
	return false
}

func ptr[T any](v T) *T { return &v }
