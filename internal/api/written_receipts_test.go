package api

import (
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/auth"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func TestCollectorTokenCannotReadOrOperate(t *testing.T) {
	h := newHarness(t)
	token := strings.Repeat("s", 32)
	h.api.cfg.WrittenTokens, _ = auth.ParseWrittenTokens("app-shop:" + token)
	body := `{"scope":"application_commits","coverage":"reservation_inserts","generation":"db-1","records":[{"table":"app.reservations","pk":"71","eventId":"reservation-71","at":"2026-11-07T01:00:00Z"}]}`
	for range 2 {
		if code, b := h.do("POST", "/v1/written-records/app-shop", token, body); code != 202 {
			t.Fatalf("append: %d %s", code, b)
		}
	}
	_, b := h.do("GET", "/v1/written-records/app-shop", viewerToken, nil)
	stats := decode[apiv1.WrittenRecordStats](t, b)
	if stats.Count != 1 || stats.Generation == nil || *stats.Generation != "db-1" {
		t.Fatalf("duplicate inflated stats: %s", b)
	}
	for _, req := range []struct{ method, path string }{{"GET", "/v1/written-records/app-shop"}, {"POST", "/v1/written-records/app-other"}, {"POST", "/v1/plans"}, {"POST", "/v1/migrations"}} {
		if code, _ := h.do(req.method, req.path, token, body); code != 401 {
			t.Fatalf("collector token accessed %s %s: %d", req.method, req.path, code)
		}
	}
	for _, bad := range []string{strings.Replace(body, `01:00:00Z`, `01:00:01Z`, 1), strings.Replace(body, `"db-1"`, `"db-2"`, 1)} {
		if code, b := h.do("POST", "/v1/written-records/app-shop", token, bad); code != 409 {
			t.Fatalf("conflict: %d %s", code, b)
		}
	}
	for _, bad := range []string{strings.Replace(body, "application_commits", "client_acknowledged", 1), strings.Replace(body, "app.reservations", "app.other", 1), strings.Replace(body, `"pk":"71"`, `"pk":"72"`, 1)} {
		if code, b := h.do("POST", "/v1/written-records/app-shop", token, bad); code != 403 {
			t.Fatalf("collector escaped evidence scope: %d %s", code, b)
		}
	}
}
