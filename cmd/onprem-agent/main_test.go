package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fixtureIssuer struct{ calls int }

func (f *fixtureIssuer) Issue(context.Context) (connectionRequest, error) {
	f.calls++
	return connectionRequest{ClusterUID: "cluster-uid", CAData: "Y2E=", PlatformToken: "platform-short-token", ArgoToken: "argo-short-token", TokenExpiresAt: time.Now().Add(time.Hour)}, nil
}

func TestHeartbeatSendsShortTokensOnlyToBoundPath(t *testing.T) {
	var got connectionRequest
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/v1/onprem/enrollments/enr_0123456789abcdef01234567/connection" || r.Header.Get("Authorization") != "Bearer connection-secret" {
			t.Error("wrong enrollment or authentication")
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer s.Close()
	f := &fixtureIssuer{}
	a := agent{origin: s.URL, id: "enr_0123456789abcdef01234567", client: s.Client(), issuer: f, token: func() (string, error) { return "connection-secret", nil }}
	if err := a.heartbeat(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.calls != 1 || got.ClusterUID != "cluster-uid" || got.PlatformToken != "platform-short-token" || got.ArgoToken != "argo-short-token" {
		t.Fatal("wrong credential scope")
	}
}

func TestHeartbeatDoesNotExposeServerBodyAndStopsOnRevocation(t *testing.T) {
	s := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte("connection-secret platform-short-token"))
	}))
	defer s.Close()
	a := agent{origin: s.URL, id: "enr_0123456789abcdef01234567", client: s.Client(), issuer: &fixtureIssuer{}, token: func() (string, error) { return "connection-secret", nil }}
	err := a.heartbeat(context.Background())
	if err == nil || !permanent(err) || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unsafe revocation handling: %v", err)
	}
}

func TestConfigRejectsCredentialRedirectOrigins(t *testing.T) {
	for _, origin := range []string{"http://api.test", "https://user:password@api.test", "https://api.test/path", "https://api.test?redirect=x"} {
		if validOrigin(origin) {
			t.Fatalf("accepted %q", origin)
		}
	}
}
