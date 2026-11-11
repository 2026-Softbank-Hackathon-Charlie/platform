package store

import "testing"

// TestGenerationOfStalePeer: a binary from before ADR 0064 counts up only
// peer_generation, so the peer's generation is never below it.
func TestGenerationOfStalePeer(t *testing.T) {
	d := AppDatabase{App: "shop", PrimaryEnv: "gke", PeerEnv: "eks", PeerGeneration: 3, MemberGenerations: map[string]int{"eks": 1, "onprem": 2}}
	if d.Generation("eks") != 3 || d.Generation("onprem") != 2 || d.Generation("gke") != 0 {
		t.Fatalf("generations %d %d %d", d.Generation("eks"), d.Generation("onprem"), d.Generation("gke"))
	}
	r, err := d.RemoveMember("eks", "", false)
	if err != nil || r.Generation("eks") != 4 || r.PeerGeneration != 4 || r.ExplicitMembers {
		t.Fatalf("after removing the stale peer: %d %d %v %v", r.Generation("eks"), r.PeerGeneration, r.ExplicitMembers, err)
	}
	r, err = d.RemoveMember("eks", "onprem", true)
	if err != nil || r.PeerEnv != "onprem" || r.Generation("eks") != 4 || r.Generation("onprem") != 2 || r.PeerGeneration != 2 || !r.ExplicitMembers {
		t.Fatalf("after moving the peer: %+v %v", r, err)
	}
}
