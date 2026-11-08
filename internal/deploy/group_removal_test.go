package deploy

import (
	"context"
	"slices"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// TestRemoveGroupPeerMovesThePeer: removing the peer of a three-environment
// database group leaves the primary and the third as the pair the files
// render with the two as explicit members (so the Clusters keep the label
// under which Argo CD owns externalClusters), keeps the third's backup path, and gives the
// removed environment a new path when it is added back as the third
// member (ADR 0064).
func TestRemoveGroupPeerMovesThePeer(t *testing.T) {
	se := newStorageEnv(t)
	se.ctrl["onprem"], se.drvs["onprem"] = newController(t), newFakeDriver()
	se.github.push("main", sha("p1"), map[string]string{"platform.yaml": pgSpec, "Dockerfile": "FROM scratch\n"})
	se.registerAlone("shop", "gke", "eks")
	ex := se.start()
	ctx := context.Background()
	app, err := se.store.GetApp(ctx, "shop")
	if err != nil {
		t.Fatal(err)
	}
	base := BackupPath(app.CreatedAt)
	first := se.deploy(sha("p1"), "gke")
	if first.Status != store.DeployHealthy {
		t.Fatalf("first: %s %q", first.Status, first.Error)
	}
	to := func(envs ...string) {
		t.Helper()
		a, _ := se.store.GetApp(ctx, "shop")
		if !slices.Equal(a.Environments, envs) {
			if err := se.store.SetAppEnvironments(ctx, "shop", a.Environments, envs); err != nil {
				t.Fatal(err)
			}
		}
		stored, _ := se.store.GetDeployment(ctx, first.ID)
		d := se.environmentDeployment(stored, envs...)
		se.enqueue(d.ID)
		if got := se.wait(d.ID); got.Status != store.DeployHealthy {
			t.Fatalf("deploy to %v: %s %q\n%s", envs, got.Status, got.Error, se.dump(got.ID))
		}
	}
	check := func(env, peer string, members []string, path string) {
		t.Helper()
		f, ok := se.topology("shop", env)
		if tp := f.Values.DBTopology; !ok || tp == nil || tp.Primary != "gke" || tp.Peer != peer || !slices.Equal(tp.Members, members) {
			t.Fatalf("%s dbTopology %+v, want peer %s members %v", env, f.Values.DBTopology, peer, members)
		}
		if b := se.file("shop", env).Values.Backup; b == nil || b.Path != path {
			t.Fatalf("%s backup %+v, want path %s", env, b, path)
		}
	}
	to("gke", "eks")
	to("gke", "eks", "onprem")
	for _, env := range []string{"gke", "eks", "onprem"} {
		check(env, "eks", []string{"gke", "eks", "onprem"}, base)
	}

	if err := ex.RemoveEnvironment(ctx, "shop", "eks", nil); err != nil {
		t.Fatal(err)
	}
	row, _ := se.store.GetAppDatabase(ctx, "shop")
	if row.PrimaryEnv != "gke" || row.PeerEnv != "onprem" || row.Generation("eks") != 1 || row.Generation("onprem") != 0 || row.PeerGeneration != 0 {
		t.Fatalf("group after removing eks: %s/%s %v %d", row.PrimaryEnv, row.PeerEnv, row.MemberGenerations, row.PeerGeneration)
	}
	if a, _ := se.store.GetApp(ctx, "shop"); !slices.Equal(a.Environments, []string{"gke", "onprem"}) {
		t.Fatalf("environments %v", a.Environments)
	}
	if _, ok := se.topology("shop", "eks"); ok {
		t.Fatal("the removed environment's file is still there")
	}
	to("gke", "onprem")
	for _, env := range []string{"gke", "onprem"} {
		check(env, "onprem", []string{"gke", "onprem"}, base)
	}

	// eks comes back as the third member, under a new path.
	to("gke", "onprem", "eks")
	check("gke", "onprem", []string{"gke", "onprem", "eks"}, base)
	check("onprem", "onprem", []string{"gke", "onprem", "eks"}, base)
	check("eks", "onprem", []string{"gke", "onprem", "eks"}, base+"-1")

	// The third goes: the peer stays, and its own path is new next time.
	if err := ex.RemoveEnvironment(ctx, "shop", "eks", nil); err != nil {
		t.Fatal(err)
	}
	row, _ = se.store.GetAppDatabase(ctx, "shop")
	if row.PeerEnv != "onprem" || row.Generation("eks") != 2 || row.Generation("onprem") != 0 {
		t.Fatalf("group after removing the third: %s %v", row.PeerEnv, row.MemberGenerations)
	}
	to("gke", "onprem", "eks")
	check("eks", "onprem", []string{"gke", "onprem", "eks"}, base+"-2")

	// Back to two, then the peer goes too: the primary's file keeps the
	// two members with peerPresent false.
	if err := ex.RemoveEnvironment(ctx, "shop", "eks", nil); err != nil {
		t.Fatal(err)
	}
	to("gke", "onprem")
	check("onprem", "onprem", []string{"gke", "onprem"}, base)
	if err := ex.RemoveEnvironment(ctx, "shop", "onprem", nil); err != nil {
		t.Fatal(err)
	}
	to("gke")
	f, _ := se.topology("shop", "gke")
	if tp := f.Values.DBTopology; tp == nil || !slices.Equal(tp.Members, []string{"gke", "onprem"}) || tp.PeerPresent == nil || *tp.PeerPresent {
		t.Fatalf("gke alone: %+v", f.Values.DBTopology)
	}
}
