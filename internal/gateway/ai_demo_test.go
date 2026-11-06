package gateway

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const aiTestBody = `{"model":"qwen2.5:1.5b-instruct-q4_K_M","messages":[{"role":"system","content":"public data only"},{"role":"user","content":"[SPACE-1]"}],"stream":false,"keep_alive":"60s","options":{"num_ctx":2048,"num_predict":128,"temperature":0,"seed":42}}`

type aiRT func(*http.Request) (*http.Response, error)

func (f aiRT) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type aiReply struct {
	code    int
	backend string
	body    []byte
	err     error
}
type aiSnapshot struct {
	Route    string         `json:"route"`
	Active   map[string]int `json:"active"`
	Requests []struct {
		RequestID  string     `json:"requestId"`
		Backend    string     `json:"backend"`
		Outcome    string     `json:"outcome"`
		Status     int        `json:"status"`
		FinishedAt *time.Time `json:"finishedAt"`
	} `json:"requests"`
}

func aiTestGateway(t *testing.T, gcp, laptop string, transport http.RoundTripper) *Gateway {
	t.Helper()
	gu, _ := url.Parse(gcp)
	lu, _ := url.Parse(laptop)
	cfg := Config{Backends: []BackendConfig{{Name: "gcp", URL: gu}, {Name: "laptop", URL: lu}}, AdminToken: "admin-secret", WriteTimeout: 12 * time.Second, Transport: transport}
	cfg.AIDemo = &AIDemoConfig{App: "app", Token: "ai-secret"}
	g, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestAIDemoRejectsInvalidRequestsBeforeForwarding(t *testing.T) {
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); io.WriteString(w, `{"done":true}`) }))
	defer upstream.Close()
	g := aiTestGateway(t, upstream.URL, upstream.URL, nil)
	public, admin := httptest.NewServer(g.Proxy()), httptest.NewServer(g.Admin())
	defer public.Close()
	defer admin.Close()
	cases := []struct {
		name, method, path, token, id, body string
		status                              int
	}{
		{"wrong-token", "POST", "/api/chat", "wrong", "id", aiTestBody, 401},
		{"wrong-path", "POST", "/api/generate", "ai-secret", "id", aiTestBody, 404},
		{"wrong-method", "GET", "/api/chat", "ai-secret", "id", aiTestBody, 404},
		{"query", "POST", "/api/chat?model=other", "ai-secret", "id", aiTestBody, 404},
		{"empty-query", "POST", "/api/chat?", "ai-secret", "id", aiTestBody, 404},
		{"missing-id", "POST", "/api/chat", "ai-secret", "", aiTestBody, 400},
		{"malformed", "POST", "/api/chat", "ai-secret", "id", `{`, 400},
		{"trailing-json", "POST", "/api/chat", "ai-secret", "id", aiTestBody + `{}`, 400},
		{"oversize", "POST", "/api/chat", "ai-secret", "id", aiTestBody + strings.Repeat(" ", 32<<10), 413},
		{"model", "POST", "/api/chat", "ai-secret", "id", strings.Replace(aiTestBody, "qwen2.5:1.5b-instruct-q4_K_M", "other", 1), 400},
		{"stream", "POST", "/api/chat", "ai-secret", "id", strings.Replace(aiTestBody, `"stream":false`, `"stream":true`, 1), 400},
		{"keep-alive", "POST", "/api/chat", "ai-secret", "id", strings.Replace(aiTestBody, `"60s"`, `"24h"`, 1), 400},
		{"context", "POST", "/api/chat", "ai-secret", "id", strings.Replace(aiTestBody, `2048`, `9999`, 1), 400},
		{"output", "POST", "/api/chat", "ai-secret", "id", strings.Replace(aiTestBody, `128`, `9999`, 1), 400},
		{"temperature", "POST", "/api/chat", "ai-secret", "id", strings.Replace(aiTestBody, `"temperature":0`, `"temperature":1`, 1), 400},
		{"missing-temperature", "POST", "/api/chat", "ai-secret", "id", strings.Replace(aiTestBody, `"temperature":0,`, "", 1), 400},
		{"seed", "POST", "/api/chat", "ai-secret", "id", strings.Replace(aiTestBody, `42`, `43`, 1), 400},
		{"extra-option", "POST", "/api/chat", "ai-secret", "id", strings.Replace(aiTestBody, `"seed":42`, `"seed":42,"num_gpu":999`, 1), 400},
		{"extra-field", "POST", "/api/chat", "ai-secret", "id", strings.Replace(aiTestBody, `"model":`, `"tools":[],"model":`, 1), 400},
		{"role", "POST", "/api/chat", "ai-secret", "id", strings.Replace(aiTestBody, `"system"`, `"assistant"`, 1), 400},
		{"over-scalar-limit", "POST", "/api/chat", "ai-secret", "id", strings.Replace(aiTestBody, "public data only", strings.Repeat("😀", 1992), 1), 422},
		{"invalid-utf8", "POST", "/api/chat", "ai-secret", "id", strings.Replace(aiTestBody, "public data only", string([]byte{0xff}), 1), 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := aiCall(public.Client(), tc.method, public.URL+tc.path, tc.token, tc.id, tc.body)
			requireAIStatus(t, got, tc.status)
			if got.backend != "" {
				t.Fatalf("pre-selection failure fabricated provenance %s", got.backend)
			}
		})
	}
	requireAIStatus(t, aiCall(admin.Client(), "GET", admin.URL+"/v1/ai/requests", "wrong", "", ""), 401)
	requireAIStatus(t, aiCall(admin.Client(), "POST", admin.URL+"/v1/route", "wrong", "", `{"backend":"laptop"}`), 401)
	if hits.Load() != 0 || len(aiObserve(t, admin.URL).Requests) != 0 {
		t.Fatal("rejected request reached inference or ledger")
	}
	// 1991 supplementary scalars + 9 user scalars = exactly 2000.
	boundary := strings.Replace(aiTestBody, "public data only", strings.Repeat("😀", 1991), 1)
	requireAIStatus(t, aiCall(public.Client(), "POST", public.URL+"/api/chat", "ai-secret", "boundary", boundary), 200)
	requireAIStatus(t, aiCall(public.Client(), "POST", public.URL+"/api/chat", "ai-secret", "boundary", boundary), 409)
	if hits.Load() != 1 {
		t.Fatalf("duplicate forwarded, hits=%d", hits.Load())
	}
}

func TestAIDemoCanonicalNativeBodyAndRequestID(t *testing.T) {
	receivedBody := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		received, _ := io.ReadAll(r.Body)
		receivedBody <- received
		if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || r.Header.Get("X-Hop-Backend") != "" || r.Header.Get("X-Request-Id") != "canonical" {
			t.Errorf("untrusted headers: %v", r.Header)
		}
		w.Header().Set("X-Hop-Backend", "forged")
		w.Header().Set("X-Request-Id", "upstream-forgery")
		io.WriteString(w, `{"done":true}`)
	}))
	defer upstream.Close()
	g := aiTestGateway(t, upstream.URL, upstream.URL, nil)
	r := httptest.NewRequest("POST", "http://gateway/api/chat", strings.NewReader(aiTestBody))
	r.Header.Set("Authorization", "Bearer ai-secret")
	r.Header.Set("X-Request-Id", "canonical")
	r.Header.Set("Connection", "X-Request-Id")
	r.Header.Set("Cookie", "session=private")
	r.Header.Set("X-Hop-Backend", "forged")
	w := httptest.NewRecorder()
	g.Proxy().ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("X-Hop-Backend") != "gcp" || len(w.Header().Values("X-Hop-Backend")) != 1 || len(w.Header().Values("X-Request-Id")) != 1 {
		t.Fatalf("response %d %v", w.Code, w.Header())
	}
	received := <-receivedBody
	var want, got any
	json.Unmarshal([]byte(aiTestBody), &want)
	json.Unmarshal(received, &got)
	wb, _ := json.Marshal(want)
	gb, _ := json.Marshal(got)
	if !bytes.Equal(wb, gb) {
		t.Fatalf("native body changed: %s", received)
	}
}

func TestAIDemoSlowBodyDeadlineBeforeForwarding(t *testing.T) {
	var hits atomic.Int64
	transport := aiRT(func(r *http.Request) (*http.Response, error) {
		hits.Add(1)
		return nil, errors.New("must not forward incomplete body")
	})
	g := aiTestGateway(t, "http://gcp", "http://laptop", transport)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Millisecond)
		defer cancel()
		g.Proxy().ServeHTTP(w, r.WithContext(ctx))
	}))
	defer server.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(time.Second))
	fmt.Fprintf(conn, "POST /api/chat HTTP/1.1\r\nHost: gateway\r\nAuthorization: Bearer ai-secret\r\nX-Request-Id: slow-body\r\nContent-Length: %d\r\n\r\n%s", len(aiTestBody), aiTestBody[:5])
	response, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("body deadline did not produce bounded rejection: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != 504 || response.Header.Get("X-Hop-Backend") != "" || hits.Load() != 0 {
		t.Fatalf("slow-body status=%d headers=%v hits=%d", response.StatusCode, response.Header, hits.Load())
	}
}

type aiWriteObserver struct {
	http.ResponseWriter
	writing atomic.Bool
}

func (w *aiWriteObserver) Write(p []byte) (int, error) {
	w.writing.Store(true)
	defer w.writing.Store(false)
	return w.ResponseWriter.Write(p)
}

func (w *aiWriteObserver) FlushError() error {
	w.writing.Store(true)
	defer w.writing.Store(false)
	return http.NewResponseController(w.ResponseWriter).Flush()
}

func (w *aiWriteObserver) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func TestAIDemoDownstreamWriteBounded(t *testing.T) {
	for _, cancelEarly := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelEarly), func(t *testing.T) {
			var hits atomic.Int64
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if hits.Add(1) != 1 {
					io.WriteString(w, `{"done":true}`)
					return
				}
				chunk := bytes.Repeat([]byte("x"), 64<<10)
				for range 512 {
					if _, err := w.Write(chunk); err != nil {
						return
					}
				}
			}))
			defer upstream.Close()
			g := aiTestGateway(t, upstream.URL, upstream.URL, nil)
			done, expired := make(chan struct{}), make(chan struct{})
			cancelRequest := make(chan context.CancelFunc, 1)
			observer := make(chan *aiWriteObserver, 1)
			public := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("X-Request-Id") != "blocked-write" {
					g.Proxy().ServeHTTP(w, r)
					return
				}
				defer close(done)
				timeout := 500 * time.Millisecond
				if cancelEarly {
					timeout = 5 * time.Second
				}
				ctx, cancel := context.WithTimeout(r.Context(), timeout)
				defer cancel()
				cancelRequest <- cancel
				context.AfterFunc(ctx, func() { close(expired) })
				observed := &aiWriteObserver{ResponseWriter: w}
				observer <- observed
				g.Proxy().ServeHTTP(observed, r.WithContext(ctx))
			}))
			public.Config.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
				if err := conn.(*net.TCPConn).SetWriteBuffer(1024); err != nil {
					t.Error(err)
				}
				return ctx
			}
			public.Start()
			defer public.Close()
			conn, err := net.Dial("tcp", strings.TrimPrefix(public.URL, "http://"))
			if err != nil {
				t.Fatal(err)
			}
			// Keep the stalled connection open through the capacity/shutdown checks.
			defer conn.Close()
			if err := conn.(*net.TCPConn).SetReadBuffer(1024); err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprintf(conn, "POST /api/chat HTTP/1.1\r\nHost: gateway\r\nAuthorization: Bearer ai-secret\r\nX-Request-Id: blocked-write\r\nContent-Length: %d\r\n\r\n%s", len(aiTestBody), aiTestBody); err != nil {
				t.Fatal(err)
			}
			observed := <-observer
			// A sustained real response write/flush with a nonreading TCP client
			// distinguishes downstream blockage from waiting on upstream/context.
			until, blockedSince := time.Now().Add(time.Second), time.Time{}
			for time.Now().Before(until) {
				if observed.writing.Load() {
					if blockedSince.IsZero() {
						blockedSince = time.Now()
					}
					if time.Since(blockedSince) >= 30*time.Millisecond {
						break
					}
				} else {
					blockedSince = time.Time{}
				}
				time.Sleep(time.Millisecond)
			}
			if blockedSince.IsZero() || time.Since(blockedSince) < 30*time.Millisecond {
				t.Fatal("did not observe a blocked downstream write")
			}
			g.mu.Lock()
			active := g.ai.active["gcp"]
			g.mu.Unlock()
			if active != 1 {
				t.Fatalf("blocked request active=%d, want 1", active)
			}
			if cancelEarly {
				(<-cancelRequest)()
			}
			aiWait(t, expired)
			select {
			case <-done:
			case <-time.After(time.Second):
				g.mu.Lock()
				active, outcome := g.ai.active["gcp"], g.ai.records[0].Outcome
				g.mu.Unlock()
				t.Fatalf("blocked downstream write outlived request context: active=%d outcome=%s", active, outcome)
			}
			g.mu.Lock()
			rec := *g.ai.records[0]
			active, inflight := g.ai.active["gcp"], g.inflightWrites
			g.mu.Unlock()
			if active != 0 || inflight != 0 || rec.FinishedAt == nil || (rec.Outcome != "aborted" && rec.Outcome != "failed") {
				t.Fatalf("blocked response capacity/ledger: active=%d inflight=%d rec=%+v", active, inflight, rec)
			}
			reply := aiCall(public.Client(), "POST", public.URL+"/api/chat", "ai-secret", "after-blocked-write", aiTestBody)
			requireAIStatus(t, reply, 200)
			if reply.backend != "gcp" || hits.Load() != 2 {
				t.Fatalf("capacity was not reusable without retry: backend=%s hits=%d", reply.backend, hits.Load())
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := g.waitWritesDrained(ctx); err != nil {
				t.Fatalf("gateway drain blocked: %v", err)
			}
			if err := public.Config.Shutdown(ctx); err != nil {
				t.Fatalf("server shutdown blocked: %v", err)
			}
		})
	}
}

func TestAIDemoWriteDeadlineDoesNotLeakToKeepAlive(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"done":true}`)
	}))
	defer upstream.Close()
	g := aiTestGateway(t, upstream.URL, upstream.URL, nil)
	deadlinePassed := make(chan struct{})
	ordinaryBody := strings.Repeat("ordinary", 1024)
	public := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/ordinary" {
			io.WriteString(w, ordinaryBody)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 100*time.Millisecond)
		defer cancel()
		deadline, _ := ctx.Deadline()
		time.AfterFunc(time.Until(deadline)+20*time.Millisecond, func() { close(deadlinePassed) })
		g.Proxy().ServeHTTP(w, r.WithContext(ctx))
	}))
	defer public.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(public.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	if _, err := fmt.Fprintf(conn, "POST /api/chat HTTP/1.1\r\nHost: gateway\r\nAuthorization: Bearer ai-secret\r\nX-Request-Id: keepalive\r\nContent-Length: %d\r\n\r\n%s", len(aiTestBody), aiTestBody); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || readErr != nil || string(body) != `{"done":true}` {
		t.Fatalf("AI final response flush: status=%d body=%s err=%v", response.StatusCode, body, readErr)
	}
	aiWait(t, deadlinePassed)
	if _, err := io.WriteString(conn, "GET /ordinary HTTP/1.1\r\nHost: gateway\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	response, err = http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("AI deadline/cancellation leaked to next keepalive response: %v", err)
	}
	body, readErr = io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || readErr != nil || string(body) != ordinaryBody {
		t.Fatalf("ordinary response after AI deadline: status=%d bytes=%d err=%v", response.StatusCode, len(body), readErr)
	}
}

func TestAIDemoBackendCapacityIndependent(t *testing.T) {
	gcpStarted, laptopStarted, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	upstream := func(ch chan struct{}) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(ch); <-release; io.WriteString(w, `{"done":true}`) }))
	}
	gcp, laptop := upstream(gcpStarted), upstream(laptopStarted)
	defer gcp.Close()
	defer laptop.Close()
	g := aiTestGateway(t, gcp.URL, laptop.URL, nil)
	public, admin := httptest.NewServer(g.Proxy()), httptest.NewServer(g.Admin())
	defer public.Close()
	defer admin.Close()
	defer once.Do(func() { close(release) })
	old, newResult := make(chan aiReply, 1), make(chan aiReply, 1)
	go func() {
		old <- aiCall(public.Client(), "POST", public.URL+"/api/chat", "ai-secret", "gcp-active", aiTestBody)
	}()
	aiWait(t, gcpStarted)
	requireAIStatus(t, aiCall(admin.Client(), "POST", admin.URL+"/v1/route", "admin-secret", "", `{"backend":"laptop"}`), 200)
	go func() {
		newResult <- aiCall(public.Client(), "POST", public.URL+"/api/chat", "ai-secret", "laptop-active", aiTestBody)
	}()
	aiWait(t, laptopStarted)
	got := aiCall(public.Client(), "POST", public.URL+"/api/chat", "ai-secret", "laptop-busy", aiTestBody)
	requireAIStatus(t, got, 429)
	if got.backend != "laptop" {
		t.Fatalf("busy provenance %s", got.backend)
	}
	s := aiObserve(t, admin.URL)
	if s.Active["gcp"] != 1 || s.Active["laptop"] != 1 {
		t.Fatalf("independent capacity %+v", s)
	}
	once.Do(func() { close(release) })
	requireAIStatus(t, aiReceive(t, old), 200)
	requireAIStatus(t, aiReceive(t, newResult), 200)
}

func TestAIDemoFailureAndNoRetry(t *testing.T) {
	for _, tc := range []struct {
		name           string
		status         int
		transportError bool
	}{{"HTTP-503", 503, false}, {"transport-error", 502, true}} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int64
			transport := aiRT(func(r *http.Request) (*http.Response, error) {
				calls.Add(1)
				if tc.transportError {
					return nil, errors.New("test transport failed")
				}
				return &http.Response{StatusCode: 503, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`failed`)), Request: r}, nil
			})
			g := aiTestGateway(t, "http://gcp", "http://laptop", transport)
			r := httptest.NewRequest("POST", "http://gateway/api/chat", strings.NewReader(aiTestBody))
			r.Header.Set("Authorization", "Bearer ai-secret")
			r.Header.Set("X-Request-Id", "failure")
			w := httptest.NewRecorder()
			g.Proxy().ServeHTTP(w, r)
			if w.Code != tc.status || w.Header().Get("X-Hop-Backend") != "gcp" || calls.Load() != 1 {
				t.Fatalf("status=%d headers=%v calls=%d", w.Code, w.Header(), calls.Load())
			}
			g.mu.Lock()
			defer g.mu.Unlock()
			if g.ai.active["gcp"] != 0 || g.ai.records[0].Outcome != "failed" {
				t.Fatalf("ledger %+v", g.ai)
			}
		})
	}
}

func TestAIDemoDeadlineAndCancellation(t *testing.T) {
	for _, deadline := range []bool{true, false} {
		t.Run(fmt.Sprint("deadline=", deadline), func(t *testing.T) {
			entered := make(chan struct{})
			calls := 0
			transport := aiRT(func(r *http.Request) (*http.Response, error) {
				calls++
				close(entered)
				<-r.Context().Done()
				return nil, r.Context().Err()
			})
			g := aiTestGateway(t, "http://gcp", "http://laptop", transport)
			var ctx context.Context
			var cancel context.CancelFunc
			if deadline {
				ctx, cancel = context.WithTimeout(context.Background(), 40*time.Millisecond)
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			r := httptest.NewRequest("POST", "http://gateway/api/chat", strings.NewReader(aiTestBody)).WithContext(ctx)
			r.Header.Set("Authorization", "Bearer ai-secret")
			r.Header.Set("X-Request-Id", "cancel")
			w := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { g.Proxy().ServeHTTP(w, r); close(done) }()
			aiWait(t, entered)
			if !deadline {
				cancel()
			}
			aiWait(t, done)
			g.mu.Lock()
			defer g.mu.Unlock()
			rec := g.ai.records[0]
			if calls != 1 || g.ai.active["gcp"] != 0 || rec.Outcome != "aborted" || rec.FinishedAt == nil {
				t.Fatalf("cancellation lost: calls=%d active=%v rec=%+v", calls, g.ai.active, rec)
			}
			if deadline && w.Code != 504 {
				t.Fatalf("deadline status=%d", w.Code)
			}
		})
	}
}

type aiBrokenBody struct{ read bool }

func (b *aiBrokenBody) Read(p []byte) (int, error) {
	if !b.read {
		b.read = true
		return copy(p, "partial"), nil
	}
	return 0, io.ErrUnexpectedEOF
}
func (*aiBrokenBody) Close() error { return nil }

func TestAIDemoBodyAbortReleasesCapacity(t *testing.T) {
	transport := aiRT(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: &aiBrokenBody{}, Request: r}, nil
	})
	g := aiTestGateway(t, "http://gcp", "http://laptop", transport)
	r := httptest.NewRequest("POST", "http://gateway/api/chat", strings.NewReader(aiTestBody))
	r.Header.Set("Authorization", "Bearer ai-secret")
	r.Header.Set("X-Request-Id", "body-abort")
	// Real HTTP server context makes ReverseProxy propagate ErrAbortHandler.
	r = r.WithContext(context.WithValue(r.Context(), http.ServerContextKey, &http.Server{}))
	func() {
		defer func() {
			if got := recover(); got != http.ErrAbortHandler {
				t.Errorf("abort=%v", got)
			}
		}()
		g.Proxy().ServeHTTP(httptest.NewRecorder(), r)
	}()
	g.mu.Lock()
	defer g.mu.Unlock()
	rec := g.ai.records[0]
	if g.ai.active["gcp"] != 0 || rec.Outcome != "aborted" || rec.FinishedAt == nil {
		t.Fatalf("body abort ledger: %+v", rec)
	}
}

func TestAIDemoStartupValidationAndOptOut(t *testing.T) {
	gu, _ := url.Parse("http://gcp")
	lu, _ := url.Parse("http://laptop")
	valid := func() Config {
		return Config{Backends: []BackendConfig{{Name: "gcp", URL: gu}, {Name: "laptop", URL: lu}}, AdminToken: "admin", AIDemo: &AIDemoConfig{App: "app", Token: "ai"}, WriteTimeout: 12 * time.Second}
	}
	for _, tc := range []struct {
		name   string
		change func(*Config)
	}{
		{"missing-ai-token", func(c *Config) { c.AIDemo.Token = "" }},
		{"missing-admin-token", func(c *Config) { c.AdminToken = "" }},
		{"wrong-app", func(c *Config) { c.AIDemo.App = "registered" }},
		{"wrong-timeout", func(c *Config) { c.WriteTimeout = time.Second }},
		{"wrong-backend-set", func(c *Config) { c.Backends[1].Name = "other" }},
		{"one-backend", func(c *Config) { c.Backends = c.Backends[:1] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.change(&c)
			if _, err := New(c); err == nil {
				t.Fatal("invalid AI config accepted")
			}
		})
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer existing" {
			t.Error("opt-out auth changed")
		}
		io.WriteString(w, "ordinary")
	}))
	defer upstream.Close()
	u, _ := url.Parse(upstream.URL)
	g, err := New(Config{Backends: []BackendConfig{{Name: "ordinary", URL: u}}})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("POST", "http://gateway/arbitrary", strings.NewReader("not chat JSON"))
	r.Header.Set("Authorization", "Bearer existing")
	w := httptest.NewRecorder()
	g.Proxy().ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("X-Hop-Backend") != "" || w.Body.String() != "ordinary" {
		t.Fatalf("opt-out behavior changed: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	w = httptest.NewRecorder()
	g.Admin().ServeHTTP(w, httptest.NewRequest("GET", "http://gateway/v1/ai/requests", nil))
	if w.Code != 404 {
		t.Fatalf("AI ledger exposed on generic app: %d", w.Code)
	}
}

func aiCall(client *http.Client, method, endpoint, token, id, body string) aiReply {
	r, err := http.NewRequest(method, endpoint, bytes.NewBufferString(body))
	if err != nil {
		return aiReply{err: err}
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("X-Request-Id", id)
	r.Header.Set("X-Hop-Backend", "caller-forgery")
	r.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(r)
	if err != nil {
		return aiReply{err: err}
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return aiReply{resp.StatusCode, resp.Header.Get("X-Hop-Backend"), b, err}
}
func requireAIStatus(t *testing.T, got aiReply, want int) {
	t.Helper()
	if got.err != nil || got.code != want {
		t.Fatalf("status=%d error=%v body=%s; want %d", got.code, got.err, got.body, want)
	}
}
func aiObserve(t *testing.T, admin string) aiSnapshot {
	t.Helper()
	got := aiCall(&http.Client{Timeout: 3 * time.Second}, "GET", admin+"/v1/ai/requests", "admin-secret", "", "")
	requireAIStatus(t, got, 200)
	var s aiSnapshot
	if err := json.Unmarshal(got.body, &s); err != nil {
		t.Fatal(err)
	}
	return s
}
func aiWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("channel deadline")
	}
}
func aiReceive(t *testing.T, ch <-chan aiReply) aiReply {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(3 * time.Second):
		t.Fatal("reply deadline")
		return aiReply{}
	}
}

func TestAIDemoSelectedBackendSurvivesCutover(t *testing.T) {
	entered, releaseTransport, bodyStarted, releaseBody := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var transportOnce, bodyOnce sync.Once
	var mu sync.Mutex
	hits := map[string]int{}
	upstream := func(name string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits[name]++
			mu.Unlock()
			if r.Header.Get("Authorization") != "" || r.Header.Get("X-Hop-Backend") != "" {
				t.Error("forged provenance or bearer reached upstream")
			}
			w.Header().Set("X-Hop-Backend", "upstream-forgery")
			w.Header().Set("Content-Type", "application/json")
			if r.Header.Get("X-Request-Id") == "old-001" {
				io.WriteString(w, `{"message":{"content":"`)
				w.(http.Flusher).Flush()
				close(bodyStarted)
				<-releaseBody
				io.WriteString(w, `gcp"},"done":true}`)
			} else {
				io.WriteString(w, `{"message":{"content":"ok"},"done":true}`)
			}
		}
	}
	gcp := httptest.NewServer(upstream("gcp"))
	defer gcp.Close()
	laptop := httptest.NewServer(upstream("laptop"))
	defer laptop.Close()
	base := http.DefaultTransport.(*http.Transport).Clone()
	defer base.CloseIdleConnections()
	transport := aiRT(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Request-Id") == "old-001" {
			close(entered)
			<-releaseTransport
		}
		return base.RoundTrip(r)
	})
	g := aiTestGateway(t, gcp.URL, laptop.URL, transport)
	public, admin := httptest.NewServer(g.Proxy()), httptest.NewServer(g.Admin())
	defer public.Close()
	defer admin.Close()
	defer func() {
		transportOnce.Do(func() { close(releaseTransport) })
		bodyOnce.Do(func() { close(releaseBody) })
	}()
	client := &http.Client{Timeout: 3 * time.Second}
	old := make(chan aiReply, 1)
	go func() { old <- aiCall(client, "POST", public.URL+"/api/chat", "ai-secret", "old-001", aiTestBody) }()
	aiWait(t, entered)
	if s := aiObserve(t, admin.URL); s.Active["gcp"] != 1 {
		t.Fatalf("selected request not active before transport: %+v", s)
	}
	busy := aiCall(client, "POST", public.URL+"/api/chat", "ai-secret", "busy-gcp", aiTestBody)
	requireAIStatus(t, busy, 429)
	if busy.backend != "gcp" {
		t.Fatalf("429 lost captured provenance: %+v", busy)
	}
	requireAIStatus(t, aiCall(client, "POST", admin.URL+"/v1/route", "admin-secret", "", `{"backend":"laptop"}`), 200)
	newReply := aiCall(client, "POST", public.URL+"/api/chat", "ai-secret", "new-001", aiTestBody)
	requireAIStatus(t, newReply, 200)
	if newReply.backend != "laptop" {
		t.Fatalf("new provenance=%s", newReply.backend)
	}
	if s := aiObserve(t, admin.URL); s.Active["gcp"] != 1 || s.Active["laptop"] != 0 {
		t.Fatalf("route switch lost source active: %+v", s)
	}
	transportOnce.Do(func() { close(releaseTransport) })
	aiWait(t, bodyStarted)
	if s := aiObserve(t, admin.URL); s.Active["gcp"] != 1 {
		t.Fatalf("body not yet drained: %+v", s)
	}
	requireAIStatus(t, aiCall(client, "POST", admin.URL+"/v1/route", "admin-secret", "", `{"backend":"gcp"}`), 200)
	requireAIStatus(t, aiCall(client, "POST", public.URL+"/api/chat", "ai-secret", "rollback-busy", aiTestBody), 429)
	bodyOnce.Do(func() { close(releaseBody) })
	got := aiReceive(t, old)
	requireAIStatus(t, got, 200)
	if got.backend != "gcp" {
		t.Fatalf("old provenance=%s", got.backend)
	}
	requireAIStatus(t, aiCall(client, "POST", public.URL+"/api/chat", "ai-secret", "rollback-ready", aiTestBody), 200)
	s := aiObserve(t, admin.URL)
	if s.Active["gcp"] != 0 || s.Active["laptop"] != 0 || len(s.Requests) != 3 {
		t.Fatalf("final ledger: %+v", s)
	}
	for _, rec := range s.Requests {
		if rec.Outcome != "complete" || rec.Status != 200 || rec.FinishedAt == nil {
			t.Fatalf("unfinished: %+v", rec)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["gcp"] != 2 || hits["laptop"] != 1 {
		t.Fatalf("unexpected hits: %v", hits)
	}
}
