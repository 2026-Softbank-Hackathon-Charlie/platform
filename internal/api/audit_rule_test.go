package api

import "testing"

func TestShouldAudit(t *testing.T) {
	cases := []struct {
		pattern, method string
		status          int
		want            bool
	}{
		{"POST /v1/plans", "POST", 201, true},
		{"GET /v1/plans", "GET", 200, false},
		{"GET /v1/plans", "GET", 401, true},
		{"POST /v1/written-records/{stream}", "POST", 202, false},
		{"POST /v1/written-records/{stream}", "POST", 403, true},
		{"PUT /v1/plans/{ref}/attachments/{name}", "PUT", 200, true},
		{"GET /v1/apps/{name}/secrets", "GET", 200, false},
		{"GET /v1/apps/{name}/secrets/{secret}", "GET", 200, true},
		{"GET /v1/apps/{name}/secrets/{secret}", "GET", 404, true},
	}
	for _, c := range cases {
		if got := shouldAudit(c.pattern, c.method, c.status); got != c.want {
			t.Errorf("shouldAudit(%q, %s, %d) = %v", c.pattern, c.method, c.status, got)
		}
	}
}
