package gateway

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Expect: 100-continue is acknowledged only when the handler reads the body.
// It proves that the slow PUT has reached body decoding before control calls.
func stalledAdminBody(t *testing.T, address, path string) net.Conn {
	t.Helper()
	u, err := url.Parse(address)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	_, err = fmt.Fprintf(conn, "PUT %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Length: 1000\r\nExpect: 100-continue\r\n\r\n", path, u.Host, testToken)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPut})
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusContinue {
		t.Fatalf("slow body did not reach decoder: %d", response.StatusCode)
	}
	return conn
}

func TestSlowAdminPutDoesNotLockHoldRelease(t *testing.T) {
	tg := newTestGW(t, Config{}, "src", "dst")
	conn := stalledAdminBody(t, tg.admin.URL, "/apps/slow")
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := tg.client.HoldWrites(ctx); err != nil {
		t.Fatalf("slow app body blocked Hold: %v", err)
	}
	if err := tg.client.Route(ctx, "dst"); err != nil {
		t.Fatalf("slow app body blocked Route: %v", err)
	}
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatalf("slow app body blocked Release: %v", err)
	}
	if tg.g.lookup("slow") != nil {
		t.Fatal("an incomplete registration changed the app registry")
	}
}

func TestAdminJSONRequiresExactlyOneValue(t *testing.T) {
	for _, test := range []struct {
		name, method, path, body string
	}{
		{"app", http.MethodPut, "/apps/shop", `{"backends":{"src":"http://localhost:8080"},"writeBackend":"src"} {}`},
		{"route", http.MethodPost, "/v1/route", `{"backend":"dst"} {}`},
		{"weights", http.MethodPost, "/v1/weights", `{"readWeights":{"dst":100}} {}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			tg := newTestGW(t, Config{}, "src", "dst")
			status, code := tg.adminDo(t, test.method, test.path, "Bearer "+testToken, test.body)
			if status != http.StatusBadRequest || code != "invalid_request" {
				t.Fatalf("trailing JSON: got %d %s", status, code)
			}
			if tg.g.lookup("shop") != nil || tg.g.state(defaultWindow).WriteBackend != "src" {
				t.Fatal("an invalid body changed routing")
			}
		})
	}
}

func TestAdminJSONBoundsTheWholeBody(t *testing.T) {
	tg := newTestGW(t, Config{}, "src", "dst")
	body := `{"backend":"dst"}` + strings.Repeat(" ", maxAdminBody)
	status, code := tg.adminDo(t, http.MethodPost, "/v1/route", "Bearer "+testToken, body)
	if status != http.StatusRequestEntityTooLarge || code != "body_too_large" {
		t.Fatalf("whole body limit: got %d %s", status, code)
	}
	if tg.g.state(defaultWindow).WriteBackend != "src" {
		t.Fatal("an oversized body changed routing")
	}
}
