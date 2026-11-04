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
		{"AppDeleteBranches", testAppDeleteBranches},
		{"AppBranchCreateList", testAppBranchCreateList},
		{"AppBranchUpdate", testAppBranchUpdate},
		{"AppBranchDelete", testAppBranchDelete},
		{"AppBranchDeleteRace", testAppBranchDeleteRace},
		{"AppBranchLimit", testAppBranchLimit},
		{"AppBranchCreateRace", testAppBranchCreateRace},
		{"DeploymentCreateGet", testDeploymentCreateGet},
		{"DeploymentInstance", testDeploymentInstance},
		{"DeploymentList", testDeploymentList},
		{"DeploymentListInstance", testDeploymentListInstance},
		{"DeploymentUpdate", testDeploymentUpdate},
		{"DeploymentSpec", testDeploymentSpec},
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

// manyBranches is a branch limit that the tests not about the limit never
// reach.
const manyBranches = 100

func newAppBranch(app, branch, instance string, createdAt int) store.AppBranch {
	return store.AppBranch{
		App:        app,
		Branch:     branch,
		Instance:   instance,
		AutoDeploy: true,
		Spec:       []byte(`{"name": "` + app + `", "services": [{"name": "web", "path": ".", "dockerfile": "Dockerfile", "port": 8080, "public": true, "replicas": 1}]}`),
		SpecCommit: "1e4d0f8c2b7a9e3f5d6c8b1a0f2e4d6c8b0a1f3e",
		CreatedBy:  "seohyun",
		CreatedAt:  at(createdAt),
	}
}

// newDeployment is a deployment into app's default instance, as newApp
// registers it.
func newDeployment(id, app string, createdAt int) store.Deployment {
	return store.Deployment{
		ID:          id,
		App:         app,
		Branch:      "main",
		Instance:    app,
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
	got.Spec, want.Spec = canonJSON(t, got.Spec), canonJSON(t, want.Spec)
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

// branchDeployment is a deployment into an added branch's instance.
func branchDeployment(id string, b store.AppBranch, createdAt int) store.Deployment {
	d := newDeployment(id, b.App, createdAt)
	d.Branch, d.Instance, d.Ref = b.Branch, b.Instance, b.Branch
	return d
}

func eqAppBranch(t *testing.T, got, want store.AppBranch) {
	t.Helper()
	got.Spec, want.Spec = canonJSON(t, got.Spec), canonJSON(t, want.Spec)
	want.CreatedAt = pg(want.CreatedAt)
	eqFields(t, "app "+want.App+" branch "+want.Branch, got, want)
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

func branchNames(bs []store.AppBranch) []string {
	out := []string{}
	for _, b := range bs {
		out = append(out, b.Branch)
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

	// The app's branch cannot move onto one of its added branches; another
	// app's added branch is no obstacle.
	must(t, s.CreateApp(ctx, newApp("shop", 0)))
	must(t, s.CreateAppBranch(ctx, newAppBranch("moimroom", "dev", "moimroom--dev", 1), manyBranches))
	must(t, s.CreateAppBranch(ctx, newAppBranch("shop", "qa", "shop--qa", 1), manyBranches))
	onto := u
	onto.Branch = "dev"
	err = s.UpdateApp(ctx, onto)
	wantErr(t, err, store.ErrBranchExists)
	wantErr(t, err, store.ErrConflict)
	got, err = s.GetApp(ctx, "moimroom")
	must(t, err)
	eqApp(t, got, want)
	onto.Branch = "qa"
	must(t, s.UpdateApp(ctx, onto))

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

// testAppDeleteBranches deletes an app with added branches: they go with
// it, with their deployments, and their instance names are free again.
func testAppDeleteBranches(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.CreateApp(ctx, newApp("shop", 0)))
	dev := newAppBranch("moimroom", "dev", "moimroom--dev", 0)
	must(t, s.CreateAppBranch(ctx, dev, manyBranches))
	must(t, s.CreateAppBranch(ctx, newAppBranch("shop", "dev", "shop--dev", 0), manyBranches))
	must(t, s.CreateDeployment(ctx, branchDeployment("dep_dev", dev, 1)))
	_, err := s.AppendDeploymentEvent(ctx, store.DeploymentEvent{DeploymentID: "dep_dev", Time: at(2), Type: "log"})
	must(t, err)

	must(t, s.DeleteApp(ctx, "moimroom"))
	list, err := s.ListAppBranches(ctx, "moimroom")
	must(t, err)
	if list == nil || len(list) != 0 {
		t.Fatalf("a deleted app lists branches %v, want an empty non-nil slice", branchNames(list))
	}
	_, err = s.GetDeployment(ctx, "dep_dev")
	wantErr(t, err, store.ErrNotFound)
	events, err := s.ListDeploymentEvents(ctx, "dep_dev", 0, 0)
	must(t, err)
	if len(events) != 0 {
		t.Fatalf("dep_dev keeps %d events after its app was deleted", len(events))
	}
	wantErr(t, s.CreateAppBranch(ctx, dev, manyBranches), store.ErrNotFound)
	list, err = s.ListAppBranches(ctx, "shop")
	must(t, err)
	eqList(t, "the other app's branches", branchNames(list), []string{"dev"})

	// Registering the name again starts with no branches, and the instance
	// name is free.
	must(t, s.CreateApp(ctx, newApp("moimroom", 3)))
	list, err = s.ListAppBranches(ctx, "moimroom")
	must(t, err)
	if len(list) != 0 {
		t.Fatalf("a re-registered app lists branches %v", branchNames(list))
	}
	must(t, s.CreateAppBranch(ctx, dev, manyBranches))
}

func testAppBranchCreateList(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.CreateApp(ctx, newApp("shop", 0)))
	for _, app := range []string{"moimroom", "shop", "nobody"} {
		list, err := s.ListAppBranches(ctx, app)
		must(t, err)
		if list == nil || len(list) != 0 {
			t.Fatalf("ListAppBranches(%s) = %v before any branch, want an empty non-nil slice", app, list)
		}
	}

	// By branch in byte order, not by creation and not by a locale collation
	// that ignores case or hyphens.
	for i, branch := range []string{"feature/x", "dev", "Dev", "a-z", "az"} {
		must(t, s.CreateAppBranch(ctx, newAppBranch("moimroom", branch, fmt.Sprintf("moimroom--b%d", i), -i), manyBranches))
	}
	all := []string{"Dev", "a-z", "az", "dev", "feature/x"}
	list, err := s.ListAppBranches(ctx, "moimroom")
	must(t, err)
	eqList(t, "ListAppBranches(moimroom)", branchNames(list), all)
	eqAppBranch(t, list[3], newAppBranch("moimroom", "dev", "moimroom--b1", -1))

	// The same branch in another app is another instance. No spec: nil comes
	// back.
	must(t, s.CreateAppBranch(ctx, newAppBranch("shop", "dev", "shop--dev", 1), manyBranches))
	bare := store.AppBranch{App: "shop", Branch: "bare", Instance: "shop--bare", CreatedAt: at(2)}
	must(t, s.CreateAppBranch(ctx, bare, manyBranches))
	list, err = s.ListAppBranches(ctx, "shop")
	must(t, err)
	eqList(t, "ListAppBranches(shop)", branchNames(list), []string{"bare", "dev"})
	eqAppBranch(t, list[0], bare)

	for _, c := range []struct {
		name          string
		b             store.AppBranch
		want, general error
	}{
		{"the app's own branch", newAppBranch("moimroom", "main", "moimroom--main", 3), store.ErrBranchExists, store.ErrConflict},
		{"a branch the app has", newAppBranch("moimroom", "dev", "moimroom--dev", 3), store.ErrBranchExists, store.ErrConflict},
		{"an instance another app's branch has", newAppBranch("shop", "feature/x", "moimroom--b0", 3), store.ErrInstanceExists, store.ErrConflict},
		{"an instance another branch of the app has", newAppBranch("moimroom", "release", "moimroom--b0", 3), store.ErrInstanceExists, store.ErrConflict},
		{"an instance that is another app's name", newAppBranch("moimroom", "release", "shop", 3), store.ErrInstanceExists, store.ErrConflict},
		{"an instance that is the app's name", newAppBranch("moimroom", "release", "moimroom", 3), store.ErrInstanceExists, store.ErrConflict},
		{"an app that does not exist", newAppBranch("nobody", "dev", "nobody--dev", 3), store.ErrNotFound, store.ErrNotFound},
	} {
		if err := s.CreateAppBranch(ctx, c.b, manyBranches); !errors.Is(err, c.want) || !errors.Is(err, c.general) {
			t.Errorf("%s: err = %v, want %v (%v)", c.name, err, c.want, c.general)
		}
	}
	if err := s.CreateAppBranch(ctx, store.AppBranch{App: "moimroom", Branch: "badspec", Instance: "moimroom--badspec", Spec: []byte(`{"name":`), CreatedAt: at(4)}, manyBranches); err == nil {
		t.Fatal("a branch whose spec is not JSON was stored")
	}
	// The refused creates changed nothing.
	list, err = s.ListAppBranches(ctx, "moimroom")
	must(t, err)
	eqList(t, "ListAppBranches(moimroom) after refused creates", branchNames(list), all)
	eqAppBranch(t, list[3], newAppBranch("moimroom", "dev", "moimroom--b1", -1))
}

func testAppBranchUpdate(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	b := newAppBranch("moimroom", "dev", "moimroom--dev", 1)
	must(t, s.CreateAppBranch(ctx, b, manyBranches))
	get := func() store.AppBranch {
		t.Helper()
		list, err := s.ListAppBranches(ctx, "moimroom")
		must(t, err)
		if len(list) != 1 {
			t.Fatalf("branches %v, want dev", branchNames(list))
		}
		return list[0]
	}

	u := b
	u.AutoDeploy, u.Spec, u.SpecCommit = false, []byte(`{"name": "moimroom", "services": []}`), "2222222222222222222222222222222222222222"
	// Instance, CreatedBy and CreatedAt are not updated.
	u.Instance, u.CreatedBy, u.CreatedAt = "moimroom--other", "mallory", at(9)
	must(t, s.UpdateAppBranch(ctx, u))
	want := b
	want.AutoDeploy, want.Spec, want.SpecCommit = u.AutoDeploy, u.Spec, u.SpecCommit
	eqAppBranch(t, get(), want)

	// The spec can go back to none.
	u.Spec = nil
	must(t, s.UpdateAppBranch(ctx, u))
	want.Spec = nil
	eqAppBranch(t, get(), want)

	wantErr(t, s.UpdateAppBranch(ctx, newAppBranch("moimroom", "missing", "moimroom--missing", 0)), store.ErrNotFound)
	// The default instance is the app's; UpdateApp changes it.
	wantErr(t, s.UpdateAppBranch(ctx, newAppBranch("moimroom", "main", "moimroom", 0)), store.ErrNotFound)
	wantErr(t, s.UpdateAppBranch(ctx, newAppBranch("nobody", "dev", "nobody--dev", 0)), store.ErrNotFound)
	bad := want
	bad.Spec = []byte(`{"name":`)
	if err := s.UpdateAppBranch(ctx, bad); err == nil {
		t.Fatal("a branch spec that is not JSON was stored")
	}
	eqAppBranch(t, get(), want)
}

// testAppBranchDelete deletes an added branch: the deployments into its
// instance go with it, and nothing of the app's other instances or of the
// other app's branch of the same name.
func testAppBranchDelete(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.CreateApp(ctx, newApp("shop", 0)))
	dev := newAppBranch("moimroom", "dev", "moimroom--dev", 0)
	feature := newAppBranch("moimroom", "feature/x", "moimroom--feature-x", 0)
	shopDev := newAppBranch("shop", "dev", "shop--dev", 0)
	for _, b := range []store.AppBranch{dev, feature, shopDev} {
		must(t, s.CreateAppBranch(ctx, b, manyBranches))
	}
	for i, d := range []store.Deployment{
		newDeployment("dep_main", "moimroom", 1),
		branchDeployment("dep_dev1", dev, 2),
		branchDeployment("dep_dev2", dev, 3),
		branchDeployment("dep_feature", feature, 4),
		newDeployment("dep_shop", "shop", 5),
		branchDeployment("dep_shopdev", shopDev, 6),
	} {
		must(t, s.CreateDeployment(ctx, d))
		_, err := s.AppendDeploymentEvent(ctx, store.DeploymentEvent{DeploymentID: d.ID, Time: at(7 + i), Type: "phase", Phase: store.DeployBuilding})
		must(t, err)
	}

	must(t, s.DeleteAppBranch(ctx, "moimroom", "dev"))
	list, err := s.ListAppBranches(ctx, "moimroom")
	must(t, err)
	eqList(t, "moimroom branches after delete", branchNames(list), []string{"feature/x"})
	for _, id := range []string{"dep_dev1", "dep_dev2"} {
		_, err := s.GetDeployment(ctx, id)
		wantErr(t, err, store.ErrNotFound)
		events, err := s.ListDeploymentEvents(ctx, id, 0, 0)
		must(t, err)
		if len(events) != 0 {
			t.Fatalf("%s keeps %d events after its branch was deleted", id, len(events))
		}
	}
	deps, err := s.ListDeployments(ctx, "moimroom", 0)
	must(t, err)
	eqList(t, "moimroom deployments after delete", deploymentIDs(deps), []string{"dep_feature", "dep_main"})
	for _, id := range []string{"dep_main", "dep_feature", "dep_shop", "dep_shopdev"} {
		events, err := s.ListDeploymentEvents(ctx, id, 0, 0)
		must(t, err)
		if len(events) != 1 {
			t.Fatalf("%s has %d events after another branch was deleted, want 1", id, len(events))
		}
	}
	list, err = s.ListAppBranches(ctx, "shop")
	must(t, err)
	eqList(t, "shop branches", branchNames(list), []string{"dev"})

	wantErr(t, s.DeleteAppBranch(ctx, "moimroom", "dev"), store.ErrNotFound)
	// The default instance is not an added branch; it goes with the app.
	wantErr(t, s.DeleteAppBranch(ctx, "moimroom", "main"), store.ErrNotFound)
	wantErr(t, s.DeleteAppBranch(ctx, "nobody", "dev"), store.ErrNotFound)
	wantErr(t, s.CreateDeployment(ctx, branchDeployment("dep_dev3", dev, 9)), store.ErrInstanceNotFound)
	if _, err := s.GetApp(ctx, "moimroom"); err != nil {
		t.Fatalf("deleting a branch touched the app: %v", err)
	}

	// Adding the branch again starts with no history.
	must(t, s.CreateAppBranch(ctx, dev, manyBranches))
	deps, err = s.ListDeployments(ctx, "moimroom", 0)
	must(t, err)
	eqList(t, "moimroom deployments after adding dev again", deploymentIDs(deps), []string{"dep_feature", "dep_main"})
}

// testAppBranchDeleteRace deletes a branch while deployments into its
// instance are created, and an app while branches are added to it: each
// create either fails as not found or is deleted with the rest, so nothing
// of the branch or the app is left.
func testAppBranchDeleteRace(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	dev := newAppBranch("moimroom", "dev", "moimroom--dev", 0)
	must(t, s.CreateAppBranch(ctx, dev, manyBranches))

	const n = 8
	errs := make([]error, n+1)
	parallel(n+1, func(i int) {
		if i == n {
			errs[i] = s.DeleteAppBranch(ctx, "moimroom", "dev")
			return
		}
		if err := s.CreateDeployment(ctx, branchDeployment(fmt.Sprintf("dep_%d", i), dev, i)); !errors.Is(err, store.ErrNotFound) {
			errs[i] = err
		}
	})
	for _, err := range errs {
		must(t, err)
	}
	deps, err := s.ListDeployments(ctx, "moimroom", 0)
	must(t, err)
	if len(deps) != 0 {
		t.Fatalf("deployments %v outlived their branch", deploymentIDs(deps))
	}

	errs = make([]error, n+1)
	parallel(n+1, func(i int) {
		if i == n {
			errs[i] = s.DeleteApp(ctx, "moimroom")
			return
		}
		b := newAppBranch("moimroom", fmt.Sprintf("b%d", i), fmt.Sprintf("moimroom--b%d", i), i)
		if err := s.CreateAppBranch(ctx, b, manyBranches); !errors.Is(err, store.ErrNotFound) {
			errs[i] = err
		}
	})
	for _, err := range errs {
		must(t, err)
	}
	branches, err := s.ListAppBranches(ctx, "moimroom")
	must(t, err)
	if len(branches) != 0 {
		t.Fatalf("branches %v outlived their app", branchNames(branches))
	}
}

func testAppBranchLimit(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.CreateApp(ctx, newApp("shop", 0)))
	must(t, s.CreateAppBranch(ctx, newAppBranch("moimroom", "dev", "moimroom--dev", 0), 2))
	must(t, s.CreateAppBranch(ctx, newAppBranch("moimroom", "qa", "moimroom--qa", 0), 2))
	err := s.CreateAppBranch(ctx, newAppBranch("moimroom", "next", "moimroom--next", 0), 2)
	wantErr(t, err, store.ErrInstanceLimit)
	wantErr(t, err, store.ErrConflict)
	// At the limit, a branch the app has and a taken instance name are still
	// named as such.
	wantErr(t, s.CreateAppBranch(ctx, newAppBranch("moimroom", "dev", "moimroom--dev-2", 0), 2), store.ErrBranchExists)
	wantErr(t, s.CreateAppBranch(ctx, newAppBranch("moimroom", "next", "moimroom--qa", 0), 2), store.ErrInstanceExists)

	// The limit counts each app's own branches.
	must(t, s.CreateAppBranch(ctx, newAppBranch("shop", "dev", "shop--dev", 0), 1))
	wantErr(t, s.CreateAppBranch(ctx, newAppBranch("shop", "qa", "shop--qa", 0), 1), store.ErrInstanceLimit)
	wantErr(t, s.CreateAppBranch(ctx, newAppBranch("shop", "qa", "shop--qa", 0), 0), store.ErrInstanceLimit)

	// Deleting a branch makes room.
	must(t, s.DeleteAppBranch(ctx, "moimroom", "dev"))
	must(t, s.CreateAppBranch(ctx, newAppBranch("moimroom", "next", "moimroom--next", 0), 2))
	list, err := s.ListAppBranches(ctx, "moimroom")
	must(t, err)
	eqList(t, "moimroom branches", branchNames(list), []string{"next", "qa"})
}

// testAppBranchCreateRace adds branches at the same moment: the limit holds,
// one create of the same branch wins, and one taker of an instance name
// wins, also across two apps. Branch creates, deployments and app updates of
// one app interleave without a deadlock.
func testAppBranchCreateRace(t *testing.T, s store.Store) {
	ctx := t.Context()
	for _, name := range []string{"moimroom", "shop", "alpha", "beta"} {
		must(t, s.CreateApp(ctx, newApp(name, 0)))
	}
	const n, limit = 8, 2
	race := func(what string, wantWins int, refused error, create func(i int) error) {
		t.Helper()
		errs := make([]error, n)
		parallel(n, func(i int) { errs[i] = create(i) })
		wins := 0
		for i, err := range errs {
			switch {
			case err == nil:
				wins++
			case !errors.Is(err, refused):
				t.Fatalf("%s: create %d: %v, want nil or %v", what, i, err, refused)
			}
		}
		if wins != wantWins {
			t.Fatalf("%s: %d creates succeeded, want %d", what, wins, wantWins)
		}
	}
	race("limit", limit, store.ErrInstanceLimit, func(i int) error {
		return s.CreateAppBranch(ctx, newAppBranch("moimroom", fmt.Sprintf("b%d", i), fmt.Sprintf("moimroom--b%d", i), i), limit)
	})
	race("same branch", 1, store.ErrBranchExists, func(i int) error {
		return s.CreateAppBranch(ctx, newAppBranch("shop", "dev", fmt.Sprintf("shop--dev-%d", i), i), manyBranches)
	})
	race("same instance in two apps", 1, store.ErrInstanceExists, func(i int) error {
		app := []string{"alpha", "beta"}[i%2]
		return s.CreateAppBranch(ctx, newAppBranch(app, fmt.Sprintf("b%d", i), "shared--x", i), manyBranches)
	})
	for app, want := range map[string]int{"moimroom": limit, "shop": 1} {
		list, err := s.ListAppBranches(ctx, app)
		must(t, err)
		if len(list) != want {
			t.Fatalf("%s has branches %v after the race, want %d", app, branchNames(list), want)
		}
	}

	errs := make([]error, n)
	parallel(n, func(i int) {
		switch i % 3 {
		case 0:
			errs[i] = s.CreateAppBranch(ctx, newAppBranch("alpha", fmt.Sprintf("c%d", i), fmt.Sprintf("alpha--c%d", i), i), manyBranches)
		case 1:
			errs[i] = s.CreateDeployment(ctx, newDeployment(fmt.Sprintf("dep_%d", i), "alpha", i))
		case 2:
			errs[i] = s.UpdateApp(ctx, newApp("alpha", 0))
		}
	})
	for _, err := range errs {
		must(t, err)
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

// testDeploymentInstance creates deployments into an app's instances: the
// default one (the app's branch and name) and an added branch's. Any other
// branch and instance is not found.
func testDeploymentInstance(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.CreateApp(ctx, newApp("shop", 0)))
	dev := newAppBranch("moimroom", "dev", "moimroom--dev", 0)
	must(t, s.CreateAppBranch(ctx, dev, manyBranches))
	must(t, s.CreateAppBranch(ctx, newAppBranch("shop", "release", "shop--release", 0), manyBranches))

	for _, d := range []store.Deployment{newDeployment("dep_main", "moimroom", 1), branchDeployment("dep_dev", dev, 2)} {
		must(t, s.CreateDeployment(ctx, d))
		got, err := s.GetDeployment(ctx, d.ID)
		must(t, err)
		eqDeployment(t, got, d)
	}
	for i, c := range []struct{ name, branch, instance string }{
		{"the app's branch with the added branch's instance", "main", "moimroom--dev"},
		{"the added branch with the app's instance", "dev", "moimroom"},
		{"a branch the app does not deploy", "feature/x", "moimroom--feature-x"},
		{"another app's default instance", "main", "shop"},
		{"another app's added branch", "release", "shop--release"},
		{"no branch and no instance", "", ""},
	} {
		d := newDeployment(fmt.Sprintf("dep_x%d", i), "moimroom", 3)
		d.Branch, d.Instance = c.branch, c.instance
		if err := s.CreateDeployment(ctx, d); !errors.Is(err, store.ErrInstanceNotFound) || !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s: err = %v, want %v", c.name, err, store.ErrInstanceNotFound)
		}
	}
	// An app that does not exist is not an instance that does not exist.
	if err := s.CreateDeployment(ctx, newDeployment("dep_y", "nobody", 3)); !errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrInstanceNotFound) {
		t.Errorf("unknown app: err = %v, want %v and not %v", err, store.ErrNotFound, store.ErrInstanceNotFound)
	}
	// An app's deployments are those of all its instances.
	list, err := s.ListDeployments(ctx, "moimroom", 0)
	must(t, err)
	eqList(t, "ListDeployments(moimroom)", deploymentIDs(list), []string{"dep_dev", "dep_main"})
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

// testDeploymentListInstance lists one instance's deployments: newest first,
// with the limit applied to that instance's deployments alone.
func testDeploymentListInstance(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	must(t, s.CreateApp(ctx, newApp("shop", 0)))
	dev := newAppBranch("moimroom", "dev", "moimroom--dev", 0)
	must(t, s.CreateAppBranch(ctx, dev, manyBranches))
	// The added branch's deployments are older than the default instance's.
	for i, id := range []string{"dep_d1", "dep_d3", "dep_d2"} {
		must(t, s.CreateDeployment(ctx, branchDeployment(id, dev, i)))
	}
	// Same created_at as dep_d2: the id breaks the tie, descending.
	must(t, s.CreateDeployment(ctx, branchDeployment("dep_d4", dev, 2)))
	for i, id := range []string{"dep_m1", "dep_m2"} {
		must(t, s.CreateDeployment(ctx, newDeployment(id, "moimroom", 5+i)))
	}
	must(t, s.CreateDeployment(ctx, newDeployment("dep_s1", "shop", 9)))

	for _, c := range []struct {
		instance string
		limit    int
		want     []string
	}{
		{"moimroom--dev", 0, []string{"dep_d4", "dep_d2", "dep_d3", "dep_d1"}},
		{"moimroom--dev", 2, []string{"dep_d4", "dep_d2"}},
		{"moimroom", 0, []string{"dep_m2", "dep_m1"}},
		{"moimroom", 1, []string{"dep_m2"}},
		{"shop", -1, []string{"dep_s1"}},
		{"nobody", 0, []string{}},
	} {
		list, err := s.ListInstanceDeployments(ctx, c.instance, c.limit)
		must(t, err)
		eqList(t, fmt.Sprintf("ListInstanceDeployments(%s, %d)", c.instance, c.limit), deploymentIDs(list), c.want)
	}
	list, err := s.ListInstanceDeployments(ctx, "moimroom--dev", 1)
	must(t, err)
	eqDeployment(t, list[0], branchDeployment("dep_d4", dev, 2))
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
	// App, Branch, Instance, Ref, Commit, Trigger, TriggeredBy and CreatedAt
	// are not updated.
	u.App, u.Branch, u.Instance, u.Ref, u.Commit, u.Trigger, u.TriggeredBy, u.CreatedAt = "shop", "dev", "shop--dev", "dev", "0000", "manual", "mallory", at(5)
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

	branch := func() store.AppBranch { return newAppBranch("moimroom", "dev", "moimroom--dev", 0) }
	b := branch()
	must(t, s.CreateAppBranch(ctx, b, manyBranches))
	b.Spec[2] = 'X' // the caller's spec
	list, err := s.ListAppBranches(ctx, "moimroom")
	must(t, err)
	list[0].Spec[2] = 'X' // a reader's spec
	list, err = s.ListAppBranches(ctx, "moimroom")
	must(t, err)
	eqAppBranch(t, list[0], branch())
	ub := branch()
	ub.SpecCommit = "3333333333333333333333333333333333333333"
	must(t, s.UpdateAppBranch(ctx, ub))
	ub.Spec[2] = 'X' // the updater's spec
	wantB := branch()
	wantB.SpecCommit = ub.SpecCommit
	list, err = s.ListAppBranches(ctx, "moimroom")
	must(t, err)
	eqAppBranch(t, list[0], wantB)
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

	b := newAppBranch("moimroom", "de\x00v", "moimroom--d\x00ev", 2)
	b.SpecCommit, b.CreatedBy = "cd\xfe", "seo\x00hyun"
	b.Spec = []byte(`{"name": "moim\u0000room", "x": "\ud800"}`)
	must(t, s.CreateAppBranch(ctx, b, manyBranches))
	wantB := newAppBranch("moimroom", "dev", "moimroom--dev", 2)
	wantB.SpecCommit, wantB.CreatedBy = "cd�", "seohyun"
	wantB.Spec = []byte(`{"name": "moimroom", "x": "�"}`)
	branches, err := s.ListAppBranches(ctx, "moimroom")
	must(t, err)
	if len(branches) != 1 {
		t.Fatalf("branches %v, want dev", branchNames(branches))
	}
	eqAppBranch(t, branches[0], wantB)
	ub := wantB
	ub.SpecCommit, ub.Spec = "ef\x00", []byte(`{"name": "\u0000"}`)
	must(t, s.UpdateAppBranch(ctx, ub))
	wantB.SpecCommit, wantB.Spec = "ef", []byte(`{"name": ""}`)
	branches, err = s.ListAppBranches(ctx, "moimroom")
	must(t, err)
	eqAppBranch(t, branches[0], wantB)
	bd := branchDeployment("dep_b", wantB, 2)
	must(t, s.CreateDeployment(ctx, bd))
	gotD, err = s.GetDeployment(ctx, "dep_b")
	must(t, err)
	eqDeployment(t, gotD, bd)

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
	for _, d := range []store.Deployment{branchDeployment("dep_3", b, 3), newDeployment("dep_4", "moimroom", 3)} {
		d.Instance += "\xff"
		wantErr(t, s.CreateDeployment(ctx, d), store.ErrInstanceNotFound)
	}
	wantErr(t, s.CreateAppBranch(ctx, newAppBranch("moim\x00room", "rel", "moimroom--rel", 3), manyBranches), store.ErrNotFound)
	for _, u := range []store.AppBranch{{App: "moimroom\xff", Branch: "dev"}, {App: "moimroom", Branch: "d\x00ev"}} {
		wantErr(t, s.UpdateAppBranch(ctx, u), store.ErrNotFound)
		wantErr(t, s.DeleteAppBranch(ctx, u.App, u.Branch), store.ErrNotFound)
	}
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
	brs, err := s.ListAppBranches(ctx, "moimroom\x00")
	must(t, err)
	ideps, err := s.ListInstanceDeployments(ctx, "moimroom--dev\xff", 0)
	must(t, err)
	if deps == nil || len(deps) != 0 || evs == nil || len(evs) != 0 || brs == nil || len(brs) != 0 || ideps == nil || len(ideps) != 0 {
		t.Fatalf("lookups by unclean keys: deployments %v, events %v, branches %v, instance deployments %v", deps, evs, brs, ideps)
	}
	if _, err := s.GetApp(ctx, "moimroom"); err != nil {
		t.Fatalf("refused writes by unclean keys touched the app: %v", err)
	}
	branches, err = s.ListAppBranches(ctx, "moimroom")
	must(t, err)
	if len(branches) != 1 {
		t.Fatalf("refused writes by unclean keys left branches %v, want dev", branchNames(branches))
	}
	eqAppBranch(t, branches[0], wantB)
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

// testDeploymentSpec records the spec a deployment read, and a redeploy
// that copies it.
func testDeploymentSpec(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateApp(ctx, newApp("moimroom", 0)))
	d := newDeployment("dep_1", "moimroom", 1)
	must(t, s.CreateDeployment(ctx, d))
	got, err := s.GetDeployment(ctx, "dep_1")
	must(t, err)
	if got.Spec != nil {
		t.Fatalf("a new deployment has spec %s", got.Spec)
	}
	d.Status, d.Spec = store.DeployBuilding, []byte(`{"name": "moimroom", "x": "\u0000a"}`)
	got, err = s.UpdateDeployment(ctx, d)
	must(t, err)
	want := d
	want.Spec = []byte(`{"name": "moimroom", "x": "a"}`)
	eqDeployment(t, got, want)

	r := newDeployment("dep_2", "moimroom", 2)
	r.Trigger, r.Spec = "redeploy", got.Spec
	must(t, s.CreateDeployment(ctx, r))
	again, err := s.GetDeployment(ctx, "dep_2")
	must(t, err)
	eqDeployment(t, again, r)
	bad := newDeployment("dep_3", "moimroom", 3)
	bad.Spec = []byte(`{`)
	if err := s.CreateDeployment(ctx, bad); err == nil {
		t.Fatal("a deployment with a spec that is not JSON was stored")
	}
}
