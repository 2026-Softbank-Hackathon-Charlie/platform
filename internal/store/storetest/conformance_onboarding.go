package storetest

import (
	"encoding/json"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// ConformanceOnboarding runs the store.OnboardingStore contract (ADR 0042)
// against fresh stores from newStore, like Conformance.
func ConformanceOnboarding(t *testing.T, newStore func(t *testing.T) store.Store) {
	cases := []struct {
		name string
		run  func(t *testing.T, s store.Store)
	}{
		{"OnboardingRoundTrip", testOnboardingRoundTrip},
		{"OnboardingOneActivePerApp", testOnboardingOneActivePerApp},
		{"OnboardingEvents", testOnboardingEvents},
		{"OnboardingEventsKept", testOnboardingEventsKept},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			t.Cleanup(s.Close)
			c.run(t, s)
		})
	}
}

func newOnboarding(id, app string, createdAt int) store.Onboarding {
	return store.Onboarding{
		ID: id, App: app, Repo: "2026-Softbank-Hackathon-Charlie/" + app, State: store.OnboardingDiscovering,
		Doc:       []byte(`{"id":"` + id + `","state":"discovering"}`),
		CreatedAt: at(createdAt), UpdatedAt: at(createdAt),
	}
}

// eqOnboarding compares got with want after canonicalizing their JSON and
// truncating want's times like a timestamptz. want's doc is first given
// its columns, as every write does.
func eqOnboarding(t *testing.T, got, want store.Onboarding) {
	t.Helper()
	want, err := want.Clean()
	must(t, err)
	got.Doc, want.Doc = canonJSON(t, got.Doc), canonJSON(t, want.Doc)
	got.Facts, want.Facts = canonJSON(t, got.Facts), canonJSON(t, want.Facts)
	want.CreatedAt, want.UpdatedAt = pg(want.CreatedAt), pg(want.UpdatedAt)
	eqFields(t, "onboarding "+want.ID, got, want)
}

func testOnboardingRoundTrip(t *testing.T, s store.Store) {
	ctx := t.Context()
	_, err := s.GetOnboarding(ctx, "onb-000000000001")
	wantErr(t, err, store.ErrNotFound)
	list, err := s.ListOnboardings(ctx)
	must(t, err)
	if list == nil || len(list) != 0 {
		t.Fatalf("ListOnboardings of nothing = %#v, want empty and not nil", list)
	}

	o := newOnboarding("onb-000000000001", "moimroom-legacy", 1)
	o.Revision = 7 // ignored: a new onboarding is at revision 1
	must(t, s.CreateOnboarding(ctx, o))
	wantErr(t, s.CreateOnboarding(ctx, o), store.ErrConflict)
	got, err := s.GetOnboarding(ctx, o.ID)
	must(t, err)
	o.Revision = 1
	eqOnboarding(t, got, o)

	// Discovery writes the facts and seals what it read from the VM. The
	// doc's id, app, repo and state follow the columns.
	next := o
	next.State, next.Doc = store.OnboardingPatching, []byte(`{"id":"other","state":"discovering","targetEnv":"eks"}`)
	next.Facts = []byte(`{"version":1,"app":"moimroom-legacy","report":{"blockers":[],"warnings":[]}}`)
	next.KeyVersion, next.KeyCheck = 1, "0a1b2c3d4e5f"
	next.SecretsNonce, next.SecretsCiphertext = []byte("nonce-env"), []byte("sealed:env")
	next.SuperuserPasswordNonce, next.SuperuserPasswordCiphertext = []byte("nonce-su"), []byte("sealed:su")
	next.ReplicationPasswordNonce, next.ReplicationPasswordCiphertext = []byte("nonce-repl"), []byte("sealed:repl")
	next.UpdatedAt = at(2)
	got, err = s.UpdateOnboarding(ctx, next, []string{store.OnboardingDiscovering})
	must(t, err)
	next.Revision = 2
	eqOnboarding(t, got, next)
	var doc struct{ ID, State, TargetEnv string }
	must(t, json.Unmarshal(got.Doc, &doc))
	if doc.ID != "onb-000000000001" || doc.State != "patching" || doc.TargetEnv != "eks" {
		t.Fatalf("doc after update = %s, want the columns' id and state", got.Doc)
	}
	got.SecretsCiphertext[0] = 'X'
	got, err = s.GetOnboarding(ctx, o.ID)
	must(t, err)
	eqOnboarding(t, got, next)

	// The state and the revision are conditions of every update: a writer
	// that read revision 1 does not overwrite revision 2.
	stale := next
	stale.Revision = 1
	_, err = s.UpdateOnboarding(ctx, stale, []string{store.OnboardingPatching})
	wantErr(t, err, store.ErrConflict)
	_, err = s.UpdateOnboarding(ctx, next, []string{store.OnboardingDiscovering})
	wantErr(t, err, store.ErrConflict)
	_, err = s.UpdateOnboarding(ctx, newOnboarding("onb-00000000000f", "other", 1), []string{store.OnboardingDiscovering})
	wantErr(t, err, store.ErrNotFound)
	got, err = s.GetOnboarding(ctx, o.ID)
	must(t, err)
	eqOnboarding(t, got, next)

	must(t, s.CreateOnboarding(ctx, newOnboarding("onb-000000000002", "shop-legacy", 3)))
	list, err = s.ListOnboardings(ctx)
	must(t, err)
	if len(list) != 2 || list[0].ID != "onb-000000000002" || list[1].ID != o.ID {
		t.Fatalf("ListOnboardings = %d rows, want onb-000000000002 then %s", len(list), o.ID)
	}
	// The list holds summaries: no facts and no sealed values.
	sum := next
	sum.Facts, sum.SecretsNonce, sum.SecretsCiphertext = nil, nil, nil
	sum.SuperuserPasswordNonce, sum.SuperuserPasswordCiphertext = nil, nil
	sum.ReplicationPasswordNonce, sum.ReplicationPasswordCiphertext = nil, nil
	eqOnboarding(t, list[1], sum)

	must(t, s.DeleteOnboarding(ctx, o.ID))
	_, err = s.GetOnboarding(ctx, o.ID)
	wantErr(t, err, store.ErrNotFound)
	wantErr(t, s.DeleteOnboarding(ctx, o.ID), store.ErrNotFound)
	_, err = s.GetOnboarding(ctx, "onb\x00")
	wantErr(t, err, store.ErrNotFound)
}

func testOnboardingOneActivePerApp(t *testing.T, s store.Store) {
	ctx := t.Context()
	first := newOnboarding("onb-000000000001", "moimroom-legacy", 1)
	must(t, s.CreateOnboarding(ctx, first))
	wantErr(t, s.CreateOnboarding(ctx, newOnboarding("onb-000000000002", "moimroom-legacy", 2)), store.ErrConflict)

	// A failed onboarding still holds the app; a done one does not.
	first.State, first.Doc, first.Revision = store.OnboardingFailed, []byte(`{"state":"failed"}`), 1
	_, err := s.UpdateOnboarding(ctx, first, []string{store.OnboardingDiscovering})
	must(t, err)
	wantErr(t, s.CreateOnboarding(ctx, newOnboarding("onb-000000000002", "moimroom-legacy", 2)), store.ErrConflict)
	first.State, first.Doc, first.Revision = store.OnboardingDone, []byte(`{"state":"done"}`), 2
	_, err = s.UpdateOnboarding(ctx, first, []string{store.OnboardingFailed})
	must(t, err)
	must(t, s.CreateOnboarding(ctx, newOnboarding("onb-000000000002", "moimroom-legacy", 2)))
}

func testOnboardingEvents(t *testing.T, s store.Store) {
	ctx := t.Context()
	_, err := s.AppendOnboardingEvent(ctx, store.OnboardingEvent{OnboardingID: "onb-000000000001", Time: at(1), State: "discovering", Kind: "state"})
	wantErr(t, err, store.ErrNotFound)
	must(t, s.CreateOnboarding(ctx, newOnboarding("onb-000000000001", "moimroom-legacy", 1)))
	must(t, s.CreateOnboarding(ctx, newOnboarding("onb-000000000002", "shop-legacy", 1)))

	var seqs []int64
	for i, id := range []string{"onb-000000000001", "onb-000000000002", "onb-000000000001"} {
		e, err := s.AppendOnboardingEvent(ctx, store.OnboardingEvent{OnboardingID: id, Time: at(i), State: "discovering",
			Kind: "step", Message: "read the instance", Data: []byte(`{"instance":"i-0abc"}`)})
		must(t, err)
		if e.Seq == 0 || (len(seqs) > 0 && e.Seq <= seqs[len(seqs)-1]) {
			t.Fatalf("event %d seq %d after %v", i, e.Seq, seqs)
		}
		if !same(e.Time, pg(at(i))) {
			t.Fatalf("event %d time %v", i, e.Time)
		}
		seqs = append(seqs, e.Seq)
	}
	got, err := s.ListOnboardingEvents(ctx, "onb-000000000001", 0, 0)
	must(t, err)
	if len(got) != 2 || got[0].Seq != seqs[0] || got[1].Seq != seqs[2] || got[0].Message != "read the instance" {
		t.Fatalf("events of onb-000000000001 = %+v", got)
	}
	if string(canonJSON(t, got[0].Data)) != string(canonJSON(t, []byte(`{"instance":"i-0abc"}`))) {
		t.Fatalf("event data = %s", got[0].Data)
	}
	got, err = s.ListOnboardingEvents(ctx, "onb-000000000001", seqs[0], 1)
	must(t, err)
	if len(got) != 1 || got[0].Seq != seqs[2] {
		t.Fatalf("events after %d = %+v", seqs[0], got)
	}

	// Deleting an onboarding deletes its events.
	must(t, s.DeleteOnboarding(ctx, "onb-000000000001"))
	got, err = s.ListOnboardingEvents(ctx, "onb-000000000001", 0, 0)
	must(t, err)
	if got == nil || len(got) != 0 {
		t.Fatalf("events after delete = %#v, want empty and not nil", got)
	}
	got, err = s.ListOnboardingEvents(ctx, "onb-000000000002", 0, 0)
	must(t, err)
	if len(got) != 1 {
		t.Fatalf("the other onboarding's events = %d, want 1", len(got))
	}
}

func testOnboardingEventsKept(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateOnboarding(ctx, newOnboarding("onb-000000000001", "moimroom-legacy", 1)))
	must(t, s.CreateOnboarding(ctx, newOnboarding("onb-000000000002", "shop-legacy", 1)))
	other, err := s.AppendOnboardingEvent(ctx, store.OnboardingEvent{OnboardingID: "onb-000000000002", Time: at(0), State: "discovering", Kind: "log"})
	must(t, err)
	var seqs []int64
	for i := range store.OnboardingEventsKept + 3 {
		e, err := s.AppendOnboardingEvent(ctx, store.OnboardingEvent{OnboardingID: "onb-000000000001", Time: at(i), State: "discovering", Kind: "log"})
		must(t, err)
		seqs = append(seqs, e.Seq)
	}
	got, err := s.ListOnboardingEvents(ctx, "onb-000000000001", 0, 0)
	must(t, err)
	if len(got) != store.OnboardingEventsKept || got[0].Seq != seqs[3] || got[len(got)-1].Seq != seqs[len(seqs)-1] {
		t.Fatalf("kept %d events from seq %d, want the newest %d from %d", len(got), got[0].Seq, store.OnboardingEventsKept, seqs[3])
	}
	// Another onboarding's events stay.
	if got, err := s.ListOnboardingEvents(ctx, "onb-000000000002", 0, 0); err != nil || len(got) != 1 || got[0].Seq != other.Seq {
		t.Fatalf("the other onboarding's events = %+v, %v", got, err)
	}
}
