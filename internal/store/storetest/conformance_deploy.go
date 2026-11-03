package storetest

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// ConformanceDeploy runs the store.DeployStore contract against fresh stores
// from newStore, with the same rules as Conformance.
func ConformanceDeploy(t *testing.T, newStore func(t *testing.T) store.Store) {
	cases := []struct {
		name string
		run  func(t *testing.T, s store.Store)
	}{
		{"AppCreateGet", testAppCreateGet},
		{"AppList", testAppList},
		{"AppUpdate", testAppUpdate},
		{"AppDelete", testAppDelete},
		{"DeploymentCreateGet", testDeploymentCreateGet},
		{"DeploymentList", testDeploymentList},
		{"DeploymentUpdate", testDeploymentUpdate},
		{"DeploymentSupersedeRace", testDeploymentSupersedeRace},
		{"DeploymentUnfinished", testDeploymentUnfinished},
		{"DeploymentEvents", testDeploymentEvents},
		{"DeploymentEventsConcurrent", testDeploymentEventsConcurrent},
		{"DeployCopies", testDeployCopies},
		{"DeployText", testDeployText},
		{"WebhookDeliveries", testWebhookDeliveries},
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

func newApp(name string, createdAt int) store.App {
	return store.App{
		Name:         name,
		Repo:         "2026-Softbank-Hackathon-Charlie/" + name,
		Branch:       "main",
		Environments: []string{"gke", "eks"},
		AutoDeploy:   true,
		Spec:         []byte(`{"name": "` + name + `", "services": [{"name": "web", "path": ".", "dockerfile": "Dockerfile", "port": 8080, "public": true, "replicas": 1}]}`),
		SpecCommit:   "9fceb02d0ae598e95dc970b74767f19372d61af8",
		CreatedBy:    "seohyun",
		CreatedAt:    at(createdAt),
	}
}

func newDeployment(id, app string, createdAt int) store.Deployment {
	return store.Deployment{
		ID:          id,
		App:         app,
		Ref:         "main",
		Commit:      "9fceb02d0ae598e95dc970b74767f19372d61af8",
		Trigger:     "push",
		TriggeredBy: "github:seohyun",
		Status:      store.DeployQueued,
		Images:      []store.DeploymentImage{},
		Environments: []store.DeploymentEnvironment{
			{Env: "gke", Status: "pending"},
			{Env: "eks", Status: "pending"},
		},
		CreatedAt: at(createdAt),
	}
}

func eqApp(t *testing.T, got, want store.App) {
	t.Helper()
	got.Spec, want.Spec = canonJSON(t, got.Spec), canonJSON(t, want.Spec)
	want.CreatedAt = pg(want.CreatedAt)
	if want.Environments == nil {
		want.Environments = []string{}
	}
	if got.Environments == nil {
		t.Errorf("app %s: Environments read back nil, want a non-nil slice", want.Name)
	}
	eqFields(t, "app "+want.Name, got, want)
}

func eqDeployment(t *testing.T, got, want store.Deployment) {
	t.Helper()
	want.CreatedAt, want.StartedAt, want.FinishedAt = pg(want.CreatedAt), pgPtr(want.StartedAt), pgPtr(want.FinishedAt)
	if want.Images == nil {
		want.Images = []store.DeploymentImage{}
	}
	if want.Environments == nil {
		want.Environments = []store.DeploymentEnvironment{}
	}
	if got.Images == nil || got.Environments == nil {
		t.Errorf("deployment %s: Images or Environments read back nil, want non-nil slices", want.ID)
	}
	eqFields(t, "deployment "+want.ID, got, want)
}

func eqDeploymentEvent(t *testing.T, got, want store.DeploymentEvent) {
	t.Helper()
	want.Time = pg(want.Time)
	eqFields(t, fmt.Sprintf("deployment event %d", want.Seq), got, want)
}

func appNames(as []store.App) []string {
	out := []string{}
	for _, a := range as {
		out = append(out, a.Name)
	}
	return out
}

func deploymentIDs(ds []store.Deployment) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.ID)
	}
	return out
}

func deploymentSeqs(es []store.DeploymentEvent) []int64 {
	out := []int64{}
	for _, e := range es {
		out = append(out, e.Seq)
	}
	return out
}

func testAppCreateGet(t *testing.T, s store.Store) {
	ctx := t.Context()
	a := newApp("moimroom", 0)
	must(t, s.CreateApp(ctx, a))
	got, err := s.GetApp(ctx, "moimroom")
	must(t, err)
	eqApp(t, got, a)

	wantErr(t, s.CreateApp(ctx, newApp("moimroom", 1)), store.ErrConflict)
	_, err = s.GetApp(ctx, "missing")
	wantErr(t, err, store.ErrNotFound)

	// No environments and no spec: an empty slice and nil come back.
	bare := store.App{Name: "bare", Repo: "o/bare", Branch: "main", CreatedAt: at(2)}
	must(t, s.CreateApp(ctx, bare))
	got, err = s.GetApp(ctx, "bare")
	must(t, err)
	eqApp(t, got, bare)

	if err := s.CreateApp(ctx, store.App{Name: "badspec", Spec: []byte(`{"name":`), CreatedAt: at(3)}); err == nil {
		t.Fatal("an app whose spec is not JSON was stored")
	}
}

func testAppList(t *testing.T, s store.Store) {
	ctx := t.Context()
	list, err := s.ListApps(ctx)
	must(t, err)
	if list == nil || len(list) != 0 {
		t.Fatalf("empty store lists %v, want an empty non-nil slice", list)
	}
	// By name in byte order, not by creation and not by a locale collation
	// that ignores hyphens.
	for i, name := range []string{"shop", "a-z", "az", "a1", "moimroom"} {
		must(t, s.CreateApp(ctx, newApp(name, -i)))
	}
	list, err = s.ListApps(ctx)
	must(t, err)
	eqList(t, "ListApps", appNames(list), []string{"a-z", "a1", "az", "moimroom", "shop"})
	eqApp(t, list[3], newApp("moimroom", -4))
}

func testAppUpdate(t *testing.T, s store.Store) {
	ctx := t.Context()
	a := newApp("moimroom", 0)
	must(t, s.CreateApp(ctx, a))

	u := a
	u.Branch, u.Environments, u.AutoDeploy = "release", []string{"eks"}, false
	u.Spec, u.SpecCommit = []byte(`{"name": "moimroom", "services": []}`), "1111111111111111111111111111111111111111"
	// Repo, CreatedBy and CreatedAt are not updated.
	u.Repo, u.CreatedBy, u.CreatedAt = "someone/else", "mallory", at(9)
	must(t, s.UpdateApp(ctx, u))

	want := a
	want.Branch, want.Environments, want.AutoDeploy, want.Spec, want.SpecCommit = u.Branch, u.Environments, u.AutoDeploy, u.Spec, u.SpecCommit
	got, err := s.GetApp(ctx, "moimroom")
	must(t, err)
	eqApp(t, got, want)

	wantErr(t, s.UpdateApp(ctx, newApp("missing", 0)), store.ErrNotFound)
}

func testAppDelete(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.CreateApp(ctx, newApp("shop", 0)))
	must(t, s.CreateDeployment(ctx, newDeployment("dep_1", "moimroom", 1)))
	must(t, s.CreateDeployment(ctx, newDeployment("dep_2", "moimroom", 2)))
	must(t, s.CreateDeployment(ctx, newDeployment("dep_3", "shop", 3)))
	for _, id := range []string{"dep_1", "dep_2", "dep_3"} {
		_, err := s.AppendDeploymentEvent(ctx, store.DeploymentEvent{DeploymentID: id, Time: at(4), Type: "phase", Phase: store.DeployBuilding})
		must(t, err)
	}

	must(t, s.DeleteApp(ctx, "moimroom"))
	_, err := s.GetApp(ctx, "moimroom")
	wantErr(t, err, store.ErrNotFound)
	for _, id := range []string{"dep_1", "dep_2"} {
		_, err := s.GetDeployment(ctx, id)
		wantErr(t, err, store.ErrNotFound)
		events, err := s.ListDeploymentEvents(ctx, id, 0, 0)
		must(t, err)
		if len(events) != 0 {
			t.Fatalf("%s keeps %d events after its app was deleted", id, len(events))
		}
	}
	unfinished, err := s.ListUnfinishedDeployments(ctx)
	must(t, err)
	eqList(t, "unfinished after delete", deploymentIDs(unfinished), []string{"dep_3"})
	events, err := s.ListDeploymentEvents(ctx, "dep_3", 0, 0)
	must(t, err)
	if len(events) != 1 {
		t.Fatalf("the other app's deployment has %d events, want 1", len(events))
	}

	wantErr(t, s.DeleteApp(ctx, "moimroom"), store.ErrNotFound)
	_, err = s.AppendDeploymentEvent(ctx, store.DeploymentEvent{DeploymentID: "dep_1", Time: at(5), Type: "log"})
	wantErr(t, err, store.ErrNotFound)
	_, err = s.UpdateDeployment(ctx, newDeployment("dep_1", "moimroom", 1))
	wantErr(t, err, store.ErrNotFound)
	wantErr(t, s.CreateDeployment(ctx, newDeployment("dep_4", "moimroom", 6)), store.ErrNotFound)

	// Registering the name again starts with no history.
	must(t, s.CreateApp(ctx, newApp("moimroom", 7)))
	list, err := s.ListDeployments(ctx, "moimroom", 0)
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("a re-registered app lists %v", deploymentIDs(list))
	}
}

func testDeploymentCreateGet(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	d := newDeployment("dep_1", "moimroom", 1)
	d.Trigger, d.TriggeredBy, d.Ref = "manual", "seohyun", "9fceb02"
	must(t, s.CreateDeployment(ctx, d))
	got, err := s.GetDeployment(ctx, "dep_1")
	must(t, err)
	eqDeployment(t, got, d)

	// Everything set, nothing nil.
	full := newDeployment("dep_2", "moimroom", 2)
	full.Status = store.DeployHealthy
	full.Images = []store.DeploymentImage{{Service: "web", Image: "registry.example/hopcharlie/apps/moimroom/web@sha256:4c2e"}}
	full.Environments = []store.DeploymentEnvironment{{Env: "eks", Status: "healthy", SyncStatus: "Synced", HealthStatus: "Healthy",
		URL: "https://moimroom.eks.example", Message: "synced <app> & \"done\""}}
	full.Error, full.BuildID = "", "hopcharlie-apps:4f1c"
	full.StartedAt, full.FinishedAt = ptr(at(3)), ptr(at(4))
	must(t, s.CreateDeployment(ctx, full))
	got, err = s.GetDeployment(ctx, "dep_2")
	must(t, err)
	eqDeployment(t, got, full)

	// nil slices are stored as empty ones.
	empty := newDeployment("dep_3", "moimroom", 3)
	empty.Images, empty.Environments = nil, nil
	must(t, s.CreateDeployment(ctx, empty))
	got, err = s.GetDeployment(ctx, "dep_3")
	must(t, err)
	eqDeployment(t, got, empty)

	wantErr(t, s.CreateDeployment(ctx, newDeployment("dep_1", "moimroom", 5)), store.ErrConflict)
	wantErr(t, s.CreateDeployment(ctx, newDeployment("dep_9", "nobody", 5)), store.ErrNotFound)
	_, err = s.GetDeployment(ctx, "dep_missing")
	wantErr(t, err, store.ErrNotFound)
}

func testDeploymentList(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.CreateApp(ctx, newApp("shop", 0)))
	for i, id := range []string{"dep_a", "dep_c", "dep_b"} {
		must(t, s.CreateDeployment(ctx, newDeployment(id, "moimroom", i)))
	}
	// Same created_at as dep_b: the id breaks the tie, descending.
	must(t, s.CreateDeployment(ctx, newDeployment("dep_d", "moimroom", 2)))
	must(t, s.CreateDeployment(ctx, newDeployment("dep_x", "shop", 9)))

	for _, c := range []struct {
		app   string
		limit int
		want  []string
	}{
		{"moimroom", 0, []string{"dep_d", "dep_b", "dep_c", "dep_a"}},
		{"moimroom", 2, []string{"dep_d", "dep_b"}},
		{"moimroom", -1, []string{"dep_d", "dep_b", "dep_c", "dep_a"}},
		{"shop", 0, []string{"dep_x"}},
		{"nobody", 0, []string{}},
	} {
		list, err := s.ListDeployments(ctx, c.app, c.limit)
		must(t, err)
		eqList(t, fmt.Sprintf("ListDeployments(%s, %d)", c.app, c.limit), deploymentIDs(list), c.want)
	}
}

func testDeploymentUpdate(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	d := newDeployment("dep_1", "moimroom", 0)
	must(t, s.CreateDeployment(ctx, d))

	u := d
	u.Status, u.BuildID, u.StartedAt = store.DeployBuilding, "build-1", ptr(at(1))
	u.Images = []store.DeploymentImage{{Service: "web", Image: "r/hopcharlie/apps/moimroom/web@sha256:aa"}}
	u.Environments = []store.DeploymentEnvironment{{Env: "gke", Status: "syncing", SyncStatus: "OutOfSync"}, {Env: "eks", Status: "pending"}}
	// App, Ref, Commit, Trigger, TriggeredBy and CreatedAt are not updated.
	u.App, u.Ref, u.Commit, u.Trigger, u.TriggeredBy, u.CreatedAt = "shop", "dev", "0000", "manual", "mallory", at(5)
	got, err := s.UpdateDeployment(ctx, u)
	must(t, err)
	want := d
	want.Status, want.BuildID, want.StartedAt, want.Images, want.Environments = u.Status, u.BuildID, u.StartedAt, u.Images, u.Environments
	eqDeployment(t, got, want)
	stored, err := s.GetDeployment(ctx, "dep_1")
	must(t, err)
	eqDeployment(t, stored, want)

	// Every field that can change can also go back to empty.
	clear := want
	clear.Status, clear.BuildID, clear.StartedAt, clear.Images, clear.Environments = store.DeployQueued, "", nil, nil, nil
	got, err = s.UpdateDeployment(ctx, clear)
	must(t, err)
	eqDeployment(t, got, clear)

	final := want
	final.Status, final.Error, final.FinishedAt = store.DeployFailed, "build failed: exit 1", ptr(at(2))
	got, err = s.UpdateDeployment(ctx, final)
	must(t, err)
	eqDeployment(t, got, final)

	// A finished deployment is never updated again.
	for _, status := range []string{store.DeployBuilding, store.DeployHealthy, store.DeploySuperseded} {
		again := final
		again.Status, again.Error = status, "late"
		_, err = s.UpdateDeployment(ctx, again)
		wantErr(t, err, store.ErrConflict)
		if !strings.Contains(err.Error(), store.DeployFailed) {
			t.Errorf("conflict %q does not name the current status", err)
		}
	}
	stored, err = s.GetDeployment(ctx, "dep_1")
	must(t, err)
	eqDeployment(t, stored, final)

	_, err = s.UpdateDeployment(ctx, newDeployment("dep_missing", "moimroom", 0))
	wantErr(t, err, store.ErrNotFound)

	for _, status := range []string{store.DeployQueued, store.DeployBuilding, store.DeployCommitting, store.DeploySyncing} {
		if store.DeploymentFinished(status) {
			t.Errorf("DeploymentFinished(%s) = true", status)
		}
	}
	for _, status := range []string{store.DeployHealthy, store.DeployFailed, store.DeploySuperseded} {
		if !store.DeploymentFinished(status) {
			t.Errorf("DeploymentFinished(%s) = false", status)
		}
	}
}

// testDeploymentSupersedeRace has workers advance a deployment while another
// writer supersedes it: the supersede wins exactly once, and after it every
// update is refused, so the deployment stays superseded.
func testDeploymentSupersedeRace(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	d := newDeployment("dep_1", "moimroom", 0)
	must(t, s.CreateDeployment(ctx, d))

	const n, each = 8, 5
	errs := make([]error, n+1)
	var mu sync.Mutex
	superseded := 0
	parallel(n+1, func(i int) {
		u := d
		if i == n {
			u.Status, u.FinishedAt = store.DeploySuperseded, ptr(at(1))
			_, errs[i] = s.UpdateDeployment(ctx, u)
			if errs[i] == nil {
				mu.Lock()
				superseded++
				mu.Unlock()
			}
			return
		}
		for j := range each {
			u.Status, u.BuildID = []string{store.DeployBuilding, store.DeployCommitting, store.DeploySyncing}[j%3], fmt.Sprintf("w%d-%d", i, j)
			_, err := s.UpdateDeployment(ctx, u)
			switch {
			case err == nil:
			case errors.Is(err, store.ErrConflict):
				return // superseded
			default:
				errs[i] = err
				return
			}
		}
	})
	for _, err := range errs {
		must(t, err)
	}
	if superseded != 1 {
		t.Fatalf("supersede applied %d times", superseded)
	}
	got, err := s.GetDeployment(ctx, "dep_1")
	must(t, err)
	if got.Status != store.DeploySuperseded {
		t.Fatalf("status %s after the race, want superseded", got.Status)
	}
}

func testDeploymentUnfinished(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.CreateApp(ctx, newApp("shop", 0)))
	list, err := s.ListUnfinishedDeployments(ctx)
	must(t, err)
	if list == nil || len(list) != 0 {
		t.Fatalf("empty store lists %v, want an empty non-nil slice", list)
	}
	for i, c := range []struct{ id, app, status string }{
		{"dep_e", "moimroom", store.DeploySyncing},
		{"dep_a", "shop", store.DeployQueued},
		{"dep_h", "moimroom", store.DeployHealthy},
		{"dep_f", "shop", store.DeployFailed},
		{"dep_s", "moimroom", store.DeploySuperseded},
		{"dep_b", "moimroom", store.DeployBuilding},
		{"dep_c", "shop", store.DeployCommitting},
	} {
		d := newDeployment(c.id, c.app, i)
		d.Status = c.status
		must(t, s.CreateDeployment(ctx, d))
	}
	// Same created_at as dep_b: the id breaks the tie, ascending.
	must(t, s.CreateDeployment(ctx, newDeployment("dep_0", "shop", 5)))

	list, err = s.ListUnfinishedDeployments(ctx)
	must(t, err)
	eqList(t, "ListUnfinishedDeployments", deploymentIDs(list), []string{"dep_e", "dep_a", "dep_0", "dep_b", "dep_c"})
}

func testDeploymentEvents(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.CreateDeployment(ctx, newDeployment("dep_a", "moimroom", 0)))
	must(t, s.CreateDeployment(ctx, newDeployment("dep_b", "moimroom", 1)))

	var a, all []store.DeploymentEvent
	var last int64
	for i := range 6 {
		e := store.DeploymentEvent{
			DeploymentID: []string{"dep_a", "dep_b"}[i%2],
			Time:         at(i),
			Type:         []string{"phase", "log", "environment"}[i%3],
		}
		switch i % 3 {
		case 0:
			e.Phase, e.Message = store.DeployBuilding, "building 2 services"
		case 1:
			e.Phase, e.Message = store.DeployBuilding, "#5 [web 2/4] RUN go build ./..."
		case 2:
			e.Env, e.Message = "eks", "Synced, Healthy"
		}
		got, err := s.AppendDeploymentEvent(ctx, e)
		must(t, err)
		if got.Seq <= last {
			t.Fatalf("Seq %d after %d: not strictly increasing", got.Seq, last)
		}
		last = got.Seq
		e.Seq = got.Seq
		eqDeploymentEvent(t, got, e)
		all = append(all, e)
		if e.DeploymentID == "dep_a" {
			a = append(a, e)
		}
	}

	list, err := s.ListDeploymentEvents(ctx, "dep_a", 0, 0)
	must(t, err)
	eqList(t, "ListDeploymentEvents(dep_a)", deploymentSeqs(list), deploymentSeqs(a))
	for i := range list {
		eqDeploymentEvent(t, list[i], a[i])
	}
	for _, c := range []struct {
		after int64
		limit int
		want  []store.DeploymentEvent
	}{
		{a[0].Seq, 0, a[1:]},
		{all[1].Seq, 0, a[1:]},
		{0, 2, a[:2]},
		{0, -1, a},
		{a[2].Seq, 0, nil},
	} {
		list, err := s.ListDeploymentEvents(ctx, "dep_a", c.after, c.limit)
		must(t, err)
		eqList(t, fmt.Sprintf("ListDeploymentEvents(dep_a, %d, %d)", c.after, c.limit), deploymentSeqs(list), deploymentSeqs(c.want))
	}

	// A finished deployment still takes events (a done event, trailing logs).
	d, err := s.GetDeployment(ctx, "dep_a")
	must(t, err)
	d.Status = store.DeployHealthy
	_, err = s.UpdateDeployment(ctx, d)
	must(t, err)
	_, err = s.AppendDeploymentEvent(ctx, store.DeploymentEvent{DeploymentID: "dep_a", Time: at(9), Type: "done", Phase: store.DeployHealthy})
	must(t, err)

	_, err = s.AppendDeploymentEvent(ctx, store.DeploymentEvent{DeploymentID: "dep_missing", Time: at(0), Type: "log"})
	wantErr(t, err, store.ErrNotFound)
	list, err = s.ListDeploymentEvents(ctx, "dep_missing", 0, 0)
	must(t, err)
	if list == nil || len(list) != 0 {
		t.Fatalf("unknown deployment lists %v, want an empty non-nil slice", list)
	}
}

// testDeploymentEventsConcurrent appends to one deployment from several
// goroutines: every event is stored, listed in Seq order without gaps
// between what was accepted.
func testDeploymentEventsConcurrent(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.CreateDeployment(ctx, newDeployment("dep_1", "moimroom", 0)))
	const n, each = 8, 5
	errs := make([]error, n)
	parallel(n, func(i int) {
		for j := range each {
			if _, err := s.AppendDeploymentEvent(ctx, store.DeploymentEvent{DeploymentID: "dep_1", Time: at(0), Type: "log", Message: fmt.Sprintf("%d-%d", i, j)}); err != nil {
				errs[i] = err
				return
			}
		}
	})
	for _, err := range errs {
		must(t, err)
	}
	list, err := s.ListDeploymentEvents(ctx, "dep_1", 0, 0)
	must(t, err)
	if len(list) != n*each {
		t.Fatalf("%d events stored, want %d", len(list), n*each)
	}
	for i := 1; i < len(list); i++ {
		if list[i].Seq <= list[i-1].Seq {
			t.Fatalf("listed Seq %d after %d", list[i].Seq, list[i-1].Seq)
		}
	}
}

func testDeployCopies(t *testing.T, s store.Store) {
	ctx := t.Context()
	a := newApp("moimroom", 0)
	must(t, s.CreateApp(ctx, a))
	a.Environments[0], a.Spec[2] = "changed", 'X'
	got, err := s.GetApp(ctx, "moimroom")
	must(t, err)
	got.Environments[1] = "changed"
	got, err = s.GetApp(ctx, "moimroom")
	must(t, err)
	eqApp(t, got, newApp("moimroom", 0))

	created := func() store.Deployment {
		d := newDeployment("dep_1", "moimroom", 0)
		d.Images = []store.DeploymentImage{{Service: "web", Image: "img"}}
		return d
	}
	d := created()
	must(t, s.CreateDeployment(ctx, d))
	d.Images[0].Image, d.Environments[0].Status = "changed", "changed" // the caller's slices
	out, err := s.GetDeployment(ctx, "dep_1")
	must(t, err)
	out.Images[0].Image, out.Environments[1].Status = "changed", "changed" // a reader's slices
	out, err = s.GetDeployment(ctx, "dep_1")
	must(t, err)
	eqDeployment(t, out, created())

	upd := created()
	upd.Status = store.DeployBuilding
	ret, err := s.UpdateDeployment(ctx, upd)
	must(t, err)
	upd.Images[0].Image, upd.Environments[0].Status = "changed", "changed" // the updater's slices
	ret.Images[0].Image, ret.Environments[1].Status = "changed", "changed" // the returned slices
	want := created()
	want.Status = store.DeployBuilding
	out, err = s.GetDeployment(ctx, "dep_1")
	must(t, err)
	eqDeployment(t, out, want)
}

// testDeployText stores strings with NUL and invalid UTF-8, which PostgreSQL
// rejects: every store keeps them cleaned the same way, and a lookup by a key
// that is not clean finds nothing.
func testDeployText(t *testing.T, s store.Store) {
	ctx := t.Context()
	a := newApp("moim\x00room", 0)
	a.Repo, a.Branch, a.Environments, a.SpecCommit, a.CreatedBy = "o/r\xff", "ma\x00in", []string{"g\x00ke"}, "ab\xfe", "seo\x00hyun"
	a.Spec = []byte(`{"name": "moim\u0000room", "x": "\ud800"}`)
	must(t, s.CreateApp(ctx, a))
	want := newApp("moimroom", 0)
	want.Repo, want.Branch, want.Environments, want.SpecCommit, want.CreatedBy = "o/r�", "main", []string{"gke"}, "ab�", "seohyun"
	want.Spec = []byte(`{"name": "moimroom", "x": "�"}`)
	got, err := s.GetApp(ctx, "moimroom")
	must(t, err)
	eqApp(t, got, want)

	d := newDeployment("dep\x00_1", "moimroom", 1)
	d.Error, d.BuildID = "exit 1:\x00 \xff", "b\x00"
	d.Images = []store.DeploymentImage{{Service: "w\x00eb", Image: "i\xffmg"}}
	d.Environments = []store.DeploymentEnvironment{{Env: "e\x00ks", Status: "failed", Message: "boom\x00\xfe"}}
	must(t, s.CreateDeployment(ctx, d))
	wantD := newDeployment("dep_1", "moimroom", 1)
	wantD.Error, wantD.BuildID = "exit 1: �", "b"
	wantD.Images = []store.DeploymentImage{{Service: "web", Image: "i�mg"}}
	wantD.Environments = []store.DeploymentEnvironment{{Env: "eks", Status: "failed", Message: "boom�"}}
	gotD, err := s.GetDeployment(ctx, "dep_1")
	must(t, err)
	eqDeployment(t, gotD, wantD)

	u := wantD
	u.Status, u.Error = "build\x00ing", "still\xff"
	gotD, err = s.UpdateDeployment(ctx, u)
	must(t, err)
	if gotD.Status != store.DeployBuilding || gotD.Error != "still�" {
		t.Fatalf("updated status %q error %q", gotD.Status, gotD.Error)
	}

	e, err := s.AppendDeploymentEvent(ctx, store.DeploymentEvent{DeploymentID: "dep_1", Time: at(2), Type: "l\x00og", Phase: "build\xffing", Env: "e\x00ks",
		Message: "#5 \x1b[31mred\x1b[0m \x00\xff"})
	must(t, err)
	wantE := store.DeploymentEvent{Seq: e.Seq, DeploymentID: "dep_1", Time: at(2), Type: "log", Phase: "build�ing", Env: "eks",
		Message: "#5 \x1b[31mred\x1b[0m �"}
	eqDeploymentEvent(t, e, wantE)
	list, err := s.ListDeploymentEvents(ctx, "dep_1", 0, 0)
	must(t, err)
	if len(list) != 1 {
		t.Fatalf("%d events, want 1", len(list))
	}
	eqDeploymentEvent(t, list[0], wantE)

	_, err = s.GetApp(ctx, "moim\x00room")
	wantErr(t, err, store.ErrNotFound)
	upd := want
	upd.Name = "moimroom\xff"
	wantErr(t, s.UpdateApp(ctx, upd), store.ErrNotFound)
	wantErr(t, s.DeleteApp(ctx, "moimroom\x00"), store.ErrNotFound)
	wantErr(t, s.CreateDeployment(ctx, newDeployment("dep_2", "moim\x00room", 3)), store.ErrNotFound)
	_, err = s.GetDeployment(ctx, "dep\x00_1")
	wantErr(t, err, store.ErrNotFound)
	_, err = s.UpdateDeployment(ctx, newDeployment("dep_1\xff", "moimroom", 1))
	wantErr(t, err, store.ErrNotFound)
	_, err = s.AppendDeploymentEvent(ctx, store.DeploymentEvent{DeploymentID: "dep_1\x00", Time: at(3), Type: "log"})
	wantErr(t, err, store.ErrNotFound)
	deps, err := s.ListDeployments(ctx, "moimroom\x00", 0)
	must(t, err)
	evs, err := s.ListDeploymentEvents(ctx, "dep_1\xff", 0, 0)
	must(t, err)
	if deps == nil || len(deps) != 0 || evs == nil || len(evs) != 0 {
		t.Fatalf("lookups by unclean keys: deployments %v, events %v", deps, evs)
	}
	if _, err := s.GetApp(ctx, "moimroom"); err != nil {
		t.Fatalf("refused writes by unclean keys touched the app: %v", err)
	}
}

func testWebhookDeliveries(t *testing.T, s store.Store) {
	ctx := t.Context()
	for _, c := range []struct {
		id   string
		want bool
	}{
		{"72d3162e-cc78-11e3-81ab-4c9367dc0958", true},
		{"72d3162e-cc78-11e3-81ab-4c9367dc0958", false},
		{"0b989ba4-242f-11e5-81e1-c7b6966d2516", true},
		{"bad\x00id", true},
		{"badid", false}, // stored cleaned
	} {
		isNew, err := s.RecordWebhookDelivery(ctx, c.id, at(0))
		must(t, err)
		if isNew != c.want {
			t.Errorf("RecordWebhookDelivery(%q) = %v, want %v", c.id, isNew, c.want)
		}
	}

	// GitHub can deliver the same id twice at once: one of them is new.
	const n = 8
	var mu sync.Mutex
	fresh := 0
	errs := make([]error, n)
	parallel(n, func(i int) {
		isNew, err := s.RecordWebhookDelivery(ctx, "same", at(1))
		errs[i] = err
		if isNew {
			mu.Lock()
			fresh++
			mu.Unlock()
		}
	})
	for _, err := range errs {
		must(t, err)
	}
	if fresh != 1 {
		t.Fatalf("%d concurrent records of one delivery were new, want 1", fresh)
	}
}
