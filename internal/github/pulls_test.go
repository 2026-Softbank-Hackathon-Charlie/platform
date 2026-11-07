package github

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestOnboardingCalls: the PR, compare and workflow calls of an onboarding
// go to the right endpoints with the installation token.
func TestPullAndWorkflowCalls(t *testing.T) {
	clk := &clock{t: time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)}
	f, _ := newFake(t, clk.now)
	var mu sync.Mutex
	var seen []string
	prs := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/repos/") {
			f.ServeHTTP(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" "+string(body))
		mu.Unlock()
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ghs_test_") {
			t.Errorf("%s: not the installation token", r.URL.Path)
		}
		pr := map[string]any{"number": 12, "html_url": "https://github.com/acme/shop/pull/12", "state": "open",
			"head": map[string]any{"sha": sha}, "base": map[string]any{"ref": "main"}}
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/acme/shop/pulls":
			if prs == 0 {
				writeJSON(w, []any{})
			} else {
				writeJSON(w, []any{pr})
			}
		case r.Method == "POST" && r.URL.Path == "/repos/acme/shop/pulls":
			prs++
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(pr)
		case r.Method == "GET" && r.URL.Path == "/repos/acme/shop/compare/main...hopcharlie/onboard":
			writeJSON(w, map[string]any{"behind_by": 2})
		case r.URL.Path == "/repos/acme/shop/actions/workflows/5/runs":
			if r.URL.Query().Get("status") == "in_progress" {
				writeJSON(w, map[string]any{"workflow_runs": []any{map[string]any{"id": 77, "status": "in_progress"}}})
			} else {
				writeJSON(w, map[string]any{"workflow_runs": []any{}})
			}
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	c, err := New(Config{AppID: 1234, PrivateKey: keyPEM(t), InstallationID: 42, Org: "acme", APIURL: srv.URL + "/", Now: clk.now})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	pr, err := c.OpenPR(ctx, "acme/shop", "hopcharlie/onboard", "main", "t", "b")
	if err != nil || pr.Number != 12 || pr.HeadSHA != sha || pr.BaseRef != "main" {
		t.Fatalf("open: %+v %v", pr, err)
	}
	if again, err := c.OpenPR(ctx, "acme/shop", "hopcharlie/onboard", "main", "t", "b"); err != nil || again.Number != 12 || prs != 1 {
		t.Fatalf("open again made another: %d %v", prs, err)
	}
	if n, err := c.BehindBy(ctx, "acme/shop", "main", "hopcharlie/onboard"); err != nil || n != 2 {
		t.Fatalf("behind %d %v", n, err)
	}
	runs, err := c.ActiveRuns(ctx, "acme/shop", 5)
	if err != nil || len(runs) != 1 || runs[0].ID != 77 {
		t.Fatalf("runs %+v %v", runs, err)
	}
	for _, err := range []error{c.MergePR(ctx, "acme/shop", 12, sha), c.ClosePR(ctx, "acme/shop", 12),
		c.DisableWorkflow(ctx, "acme/shop", 5), c.EnableWorkflow(ctx, "acme/shop", 5), c.CancelRun(ctx, "acme/shop", 77)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := c.MergePR(ctx, "acme/shop", 12, "main"); err == nil {
		t.Fatal("merged at a ref, not a sha")
	}
	all := strings.Join(seen, "\n")
	for _, want := range []string{
		`PUT /repos/acme/shop/pulls/12/merge {"merge_method":"squash","sha":"` + sha + `"}`,
		`PATCH /repos/acme/shop/pulls/12 {"state":"closed"}`,
		`PUT /repos/acme/shop/actions/workflows/5/disable `,
		`PUT /repos/acme/shop/actions/workflows/5/enable `,
		`POST /repos/acme/shop/actions/runs/77/cancel `,
		`GET /repos/acme/shop/pulls?state=open&head=acme%3Ahopcharlie%2Fonboard `,
	} {
		if !strings.Contains(all, want) {
			t.Errorf("no %q in\n%s", want, all)
		}
	}
}
