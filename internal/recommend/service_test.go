package recommend

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func fixture() []Candidate {
	src, dst := "gke", "onprem"
	return []Candidate{{Item: Item{ID: "shop-gke-onprem", App: "shop", Source: &src, Target: &dst, Title: "shop 이전 조건 확인", Reasons: []string{"대상 환경은 응답하지만 총비용은 미확인입니다.", "데이터 반출 정책과 용량 검사가 필요합니다."}, Missing: []string{"onprem_tco"}, Sources: []Source{}}, AllowedActions: []string{"stay", "insufficient"}}}
}

type fakeRunner struct {
	calls   atomic.Int64
	request chan Launch
}

func (r *fakeRunner) StartRecommendation(_ context.Context, q Launch) error {
	r.calls.Add(1)
	r.request <- q
	return nil
}

func TestEntrySingleflightAndStatusNeverCreatesJob(t *testing.T) {
	now := time.Now().UTC()
	r := &fakeRunner{request: make(chan Launch, 1)}
	var reads atomic.Int64
	s := New(func(context.Context) ([]Candidate, error) { reads.Add(1); return fixture(), nil }, r, func() time.Time { return now })
	if got := s.Status(); got.Status != "not_configured" || r.calls.Load() != 0 || reads.Load() != 0 {
		t.Fatal("status performed work")
	}
	var pending sync.WaitGroup
	for range 20 {
		pending.Go(func() { s.Trigger(context.Background(), "viewer") })
	}
	pending.Wait()
	var launch Launch
	select {
	case launch = <-r.request:
	case <-time.After(time.Second):
		t.Fatal("no AI job")
	}
	for range 20 {
		s.Status()
	}
	if r.calls.Load() != 1 || reads.Load() != 1 {
		t.Fatal("duplicate provider/AI work")
	}
	var input Input
	if err := json.Unmarshal(launch.Input, &input); err != nil {
		t.Fatal(err)
	}
	res := Result{Nonce: launch.Nonce, SnapshotID: input.SnapshotID, Status: "ready", Model: "claude-sonnet-5-5", Recommendations: []Selection{{ID: input.Candidates[0].ID, Action: "insufficient", Reasons: []string{input.Candidates[0].Reasons[0]}}}}
	if err := s.Complete(launch.ID, res); err != nil {
		t.Fatal(err)
	}
	got := s.Trigger(context.Background(), "other-viewer")
	if got.Status != "ready" || len(got.Recommendations) != 1 || got.Recommendations[0].EstimatedMonthlySavingsUSD != nil {
		t.Fatalf("%+v", got)
	}
	if err := s.Complete(launch.ID, res); err != nil {
		t.Fatal("identical retry must be idempotent")
	}
	res.Recommendations[0].Reasons = []string{input.Candidates[0].Reasons[1]}
	if !errors.Is(s.Complete(launch.ID, res), ErrConflict) {
		t.Fatal("replayed callback rewrote output")
	}
}

func TestAIImpossibleClaimsNeverBecomeRecommendations(t *testing.T) {
	for _, fault := range []string{"nonce", "snapshot", "unknown id", "move without evidence", "invented reason", "duplicate"} {
		t.Run(fault, func(t *testing.T) {
			r := &fakeRunner{request: make(chan Launch, 1)}
			s := New(func(context.Context) ([]Candidate, error) { return fixture(), nil }, r, time.Now)
			s.Trigger(context.Background(), "viewer")
			q := <-r.request
			var in Input
			_ = json.Unmarshal(q.Input, &in)
			res := Result{Nonce: q.Nonce, SnapshotID: in.SnapshotID, Status: "ready", Model: "claude-sonnet-5-5", Recommendations: []Selection{{ID: in.Candidates[0].ID, Action: "insufficient", Reasons: []string{in.Candidates[0].Reasons[0]}}}}
			switch fault {
			case "nonce":
				res.Nonce = "wrong"
			case "snapshot":
				res.SnapshotID = "old"
			case "unknown id":
				res.Recommendations[0].ID = "other"
			case "move without evidence":
				res.Recommendations[0].Action = "move"
			case "invented reason":
				res.Recommendations[0].Reasons = []string{"월 100만원 절감"}
			case "duplicate":
				res.Recommendations = append(res.Recommendations, res.Recommendations[0])
			}
			if s.Complete(q.ID, res) == nil {
				t.Fatal("untrusted recommendation accepted")
			}
			if len(s.Status().Recommendations) != 0 {
				t.Fatal("invalid AI result leaked")
			}
		})
	}
}

func TestExpiredAnalysisAndMissingRunnerNeverInventAI(t *testing.T) {
	now := time.Now().UTC()
	missing := New(func(context.Context) ([]Candidate, error) { t.Fatal("unconfigured AI fetched inputs"); return nil, nil }, nil, func() time.Time { return now })
	if got := missing.Trigger(context.Background(), "viewer"); got.Status != "not_configured" || len(got.Recommendations) != 0 {
		t.Fatalf("%+v", got)
	}
	r := &fakeRunner{request: make(chan Launch, 1)}
	s := New(func(context.Context) ([]Candidate, error) { return fixture(), nil }, r, func() time.Time { return now })
	s.Trigger(context.Background(), "viewer")
	q := <-r.request
	var in Input
	_ = json.Unmarshal(q.Input, &in)
	now = now.Add(AnalysisTimeout + time.Second)
	if s.Status().Status != "unavailable" || r.calls.Load() != 1 {
		t.Fatal("expired analysis restarted from status")
	}
	if s.Complete(q.ID, Result{Nonce: q.Nonce, SnapshotID: in.SnapshotID, Status: "unavailable", ErrorCode: "ai_unavailable"}) == nil {
		t.Fatal("expired callback accepted")
	}
}

func TestIncompleteBuildFactsCannotBecomeAnAIStayOrMove(t *testing.T) {
	now := time.Now().UTC()
	candidates := Build([]Application{{Name: "docker-doom", Current: str("gke"), Environments: []string{"gke", "eks"}}}, []Environment{{Name: "gke", Ready: true, ObservedAt: now}, {Name: "eks", Ready: true, ObservedAt: now}}, nil, now)
	r := &fakeRunner{request: make(chan Launch, 1)}
	s := New(func(context.Context) ([]Candidate, error) { return candidates, nil }, r, func() time.Time { return now })
	s.Trigger(context.Background(), "viewer")
	launch := <-r.request
	var input Input
	if err := json.Unmarshal(launch.Input, &input); err != nil {
		t.Fatal(err)
	}
	c := input.Candidates[0]
	result := Result{Nonce: launch.Nonce, SnapshotID: input.SnapshotID, Status: "ready", Model: "sandbox-model", Recommendations: []Selection{{ID: c.ID, Action: "insufficient", Reasons: c.Reasons}}}
	for _, action := range []string{"stay", "move"} {
		result.Recommendations[0].Action = action
		if !errors.Is(s.Complete(launch.ID, result), ErrConflict) {
			t.Fatalf("accepted %s without comparison evidence", action)
		}
	}
	result.Recommendations[0].Action = "insufficient"
	result.Recommendations[0].Reasons = []string{"AWS m7i.large 참고 $0.1239"}
	if !errors.Is(s.Complete(launch.ID, result), ErrConflict) {
		t.Fatal("accepted reference price as a decision")
	}
	result.Recommendations[0].Reasons = c.Reasons
	if err := s.Complete(launch.ID, result); err != nil {
		t.Fatal(err)
	}
	got := s.Status().Recommendations[0]
	if got.Action != "insufficient" || len(got.Reasons) != 1 || got.Reasons[0] != c.Reasons[0] || got.EstimatedMonthlySavingsUSD != nil || r.calls.Load() != 1 {
		t.Fatalf("lost primary decision evidence: %+v", got)
	}
}
