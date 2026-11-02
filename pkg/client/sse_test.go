package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func TestReadSSE(t *testing.T) {
	bom := string(rune(0xFEFF))
	cases := []struct {
		name string
		in   string
		want []sseEvent
	}{
		{"named event", "event: snapshot\ndata: {}\n\n", []sseEvent{{"snapshot", "", "{}"}}},
		{"multi-line data", "data: a\ndata: b\ndata:\n\n", []sseEvent{{"message", "", "a\nb\n"}}},
		{"comments and keep-alives", ": hello\n\n: keep-alive\n\nevent: log\n: inside\ndata: x\n\n", []sseEvent{{"log", "", "x"}}},
		{"CRLF", "event: metric\r\nid: 7\r\ndata: {\"seq\":7}\r\n\r\n", []sseEvent{{"metric", "7", `{"seq":7}`}}},
		{"lone CR", "event: log\rdata: x\r\r", []sseEvent{{"log", "", "x"}}},
		{"mixed line endings", "data: a\r\n\rdata: b\n\r\n", []sseEvent{{"message", "", "a"}, {"message", "", "b"}}},
		{"colon spacing", "data:x\ndata:  y\n\n", []sseEvent{{"message", "", "x\n y"}}},
		{"field without colon", "data\n\n", []sseEvent{{"message", "", ""}}},
		{"event without data is not dispatched", "event: snapshot\n\ndata: x\n\n", []sseEvent{{"message", "", "x"}}},
		{"retry and unknown fields ignored", "retry: 1000\nfoo: bar\ndata: x\n\n", []sseEvent{{"message", "", "x"}}},
		{"id carries over until reset", "id: 3\ndata: a\n\ndata: b\n\nid\ndata: c\n\n", []sseEvent{{"message", "3", "a"}, {"message", "3", "b"}, {"message", "", "c"}}},
		{"id with NUL ignored", "id: 1\ndata: a\n\nid: 2\x00\ndata: b\n\n", []sseEvent{{"message", "1", "a"}, {"message", "1", "b"}}},
		{"leading BOM", bom + "data: x\n\n", []sseEvent{{"message", "", "x"}}},
		{"empty event name", "event:\ndata: x\n\n", []sseEvent{{"message", "", "x"}}},
		{"unterminated event dropped", "data: a\n\ndata: b\n", []sseEvent{{"message", "", "a"}}},
		{"unterminated line dropped", "data: a\n\ndata: b", []sseEvent{{"message", "", "a"}}},
		{"empty stream", "", nil},
	}
	readers := map[string]func(string) io.Reader{
		"whole":    func(s string) io.Reader { return strings.NewReader(s) },
		"one byte": func(s string) io.Reader { return iotest.OneByteReader(strings.NewReader(s)) },
		"half":     func(s string) io.Reader { return iotest.HalfReader(strings.NewReader(s)) },
	}
	for _, tc := range cases {
		for rn, mk := range readers {
			t.Run(tc.name+"/"+rn, func(t *testing.T) {
				var got []sseEvent
				err := readSSE(mk(tc.in), func(ev sseEvent) error { got = append(got, ev); return nil })
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, tc.want) {
					t.Errorf("got %q, want %q", got, tc.want)
				}
			})
		}
	}
}

// A CR at the end of a read must end the line at once: a live stream may
// not send its next byte for a long time.
func TestReadSSEDispatchesWithoutWaiting(t *testing.T) {
	for _, chunks := range [][]string{
		{"data: a\r", "\n\r\n"},
		{"data: a\r\n\r"},
		{"data: a\n", "\n"},
	} {
		pr, pw := io.Pipe()
		got := make(chan sseEvent, 1)
		done := make(chan error, 1)
		go func() { done <- readSSE(pr, func(ev sseEvent) error { got <- ev; return nil }) }()
		for _, c := range chunks {
			if _, err := pw.Write([]byte(c)); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case ev := <-got:
			if ev.data != "a" {
				t.Errorf("%q: data = %q", chunks, ev.data)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%q: event not dispatched until more data arrived", chunks)
		}
		pw.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func runDoc(status string) string {
	return strings.Replace(runJSON, `"status":"running"`, `"status":"`+status+`"`, 1)
}

// writeSnapshot and writeEvent write messages the way the contract says the
// server does: snapshot without id, events named by kind with seq as id.
func writeSnapshot(w io.Writer, status string) {
	fmt.Fprintf(w, "event: snapshot\ndata: %s\n\n", runDoc(status))
	w.(http.Flusher).Flush()
}

func writeEvent(w io.Writer, seq int64, kind string) {
	fmt.Fprintf(w, "event: %s\nid: %d\ndata: {\"seq\":%d,\"time\":\"2026-11-02T09:00:00Z\",\"runId\":\"run_1\",\"kind\":%q,\"step\":\"presync\",\"message\":\"m%d\",\"data\":{\"lagBytes\":%d}}\n\n",
		kind, seq, seq, kind, seq, seq*100)
	w.(http.Flusher).Flush()
}

// sseServer serves the event stream with handle, numbering connections from
// 0, and records each connection's request headers and path.
type sseServer struct {
	mu      sync.Mutex
	headers []http.Header
	paths   []string
}

func newSSEServer(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, conn int)) (*Client, *sseServer) {
	t.Helper()
	s := &sseServer{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		n := len(s.headers)
		s.headers = append(s.headers, r.Header.Clone())
		s.paths = append(s.paths, r.URL.EscapedPath())
		s.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		handle(w, r, n)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL, "tok")
	c.retryMin, c.retryMax = time.Millisecond, 10*time.Millisecond
	return c, s
}

func (s *sseServer) lastEventIDs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []string
	for _, h := range s.headers {
		ids = append(ids, h.Get("Last-Event-ID"))
	}
	return ids
}

func (s *sseServer) conns() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.headers)
}

func TestStreamEvents(t *testing.T) {
	c, s := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		writeSnapshot(w, "running")
		io.WriteString(w, ": keep-alive\n\n")
		writeEvent(w, 6, "step_started")
		writeEvent(w, 7, "metric")
		writeSnapshot(w, "succeeded")
	})
	var got []Message
	err := c.StreamEvents(context.Background(), "run 1", 5, func(m Message) error { got = append(got, m); return nil })
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var seqs []int64
	for _, m := range got {
		names = append(names, m.Name)
		seqs = append(seqs, m.Seq)
	}
	if want := []string{"snapshot", "step_started", "metric", "snapshot"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
	if want := []int64{0, 6, 7, 7}; !reflect.DeepEqual(seqs, want) {
		t.Errorf("seqs = %v, want %v", seqs, want)
	}
	if got[0].Snapshot == nil || got[0].Snapshot.Status != apiv1.RunStatusRunning || got[3].Snapshot.Status != apiv1.RunStatusSucceeded {
		t.Errorf("snapshots not decoded: %+v %+v", got[0].Snapshot, got[3].Snapshot)
	}
	ev := got[2].Event
	if ev == nil || ev.Kind != "metric" || *ev.Step != "presync" || *ev.Message != "m7" || (*ev.Data)["lagBytes"] != float64(700) {
		t.Errorf("event not decoded: %+v", ev)
	}
	h := s.headers[0]
	if h.Get("Last-Event-ID") != "5" || h.Get("Accept") != "text/event-stream" || h.Get("Authorization") != "Bearer tok" {
		t.Errorf("headers = %v", h)
	}
	if s.paths[0] != "/v1/migrations/run%201/events" {
		t.Errorf("path = %s", s.paths[0])
	}

	// after = 0 asks for everything, so no Last-Event-ID.
	if err := c.StreamEvents(context.Background(), "run_1", 0, func(Message) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.headers[1]["Last-Event-Id"]; ok {
		t.Errorf("Last-Event-ID sent for after=0")
	}
}

func TestStreamEventsErrors(t *testing.T) {
	ctx := context.Background()
	noop := func(Message) error { return nil }
	errStop := errors.New("stop")

	t.Run("callback error is returned as is", func(t *testing.T) {
		c, _ := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
			writeSnapshot(w, "running")
			writeEvent(w, 1, "log")
		})
		calls := 0
		err := c.StreamEvents(ctx, "run_1", 0, func(Message) error { calls++; return errStop })
		if err != errStop || calls != 1 {
			t.Fatalf("err = %v after %d calls", err, calls)
		}
	})
	t.Run("API error", func(t *testing.T) {
		c, _ := fakeAPI(t, 404, `{"error":{"code":"run_not_found","message":"run not found"}}`)
		if err := c.StreamEvents(ctx, "run_x", 0, noop); ErrorCode(err) != "run_not_found" {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("not an event stream", func(t *testing.T) {
		c, _ := fakeAPI(t, 200, runJSON)
		err := c.StreamEvents(ctx, "run_1", 0, noop)
		var pe *protocolError
		if !errors.As(err, &pe) {
			t.Fatalf("err = %v, want a protocol error", err)
		}
	})
	t.Run("bad event data", func(t *testing.T) {
		c, _ := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
			io.WriteString(w, "event: snapshot\ndata: {not json\n\n")
		})
		err := c.StreamEvents(ctx, "run_1", 0, noop)
		var pe *protocolError
		if !errors.As(err, &pe) {
			t.Fatalf("err = %v, want a protocol error", err)
		}
	})
	t.Run("context cancelled mid-stream", func(t *testing.T) {
		c, _ := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, _ int) {
			writeSnapshot(w, "running")
			<-r.Context().Done()
		})
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		err := c.StreamEvents(ctx, "run_1", 0, func(Message) error { cancel(); return nil })
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	})
}

// abort drops the connection without ending the chunked body, as a crashing
// server or a broken network would.
func abort() { panic(http.ErrAbortHandler) }

func collect(t *testing.T, c *Client) (seqs []int64, snapshots int, err error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = c.Watch(ctx, "run_1", func(m Message) error {
		if m.Snapshot != nil {
			snapshots++
		} else {
			seqs = append(seqs, m.Seq)
		}
		return nil
	})
	return seqs, snapshots, err
}

func TestWatchReconnectsWithoutDuplicates(t *testing.T) {
	c, s := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, conn int) {
		switch conn {
		case 0: // ends cleanly before the run is over, e.g. a slow subscriber was dropped
			writeSnapshot(w, "running")
			for seq := int64(1); seq <= 3; seq++ {
				writeEvent(w, seq, "log")
			}
		case 1: // replays more than asked, then breaks
			writeSnapshot(w, "running")
			for seq := int64(2); seq <= 5; seq++ {
				writeEvent(w, seq, "log")
			}
			abort()
		default:
			writeSnapshot(w, "running")
			writeEvent(w, 6, "done")
			writeSnapshot(w, "succeeded")
		}
	})
	seqs, snapshots, err := collect(t, c)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{1, 2, 3, 4, 5, 6}; !reflect.DeepEqual(seqs, want) {
		t.Errorf("seqs = %v, want %v", seqs, want)
	}
	if snapshots != 4 {
		t.Errorf("snapshots = %d, want 4", snapshots)
	}
	if want := []string{"", "3", "5"}; !reflect.DeepEqual(s.lastEventIDs(), want) {
		t.Errorf("Last-Event-ID per connection = %q, want %q", s.lastEventIDs(), want)
	}
}

func TestWatchKeepsGoingWhileAwaitingSwitch(t *testing.T) {
	c, s := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, conn int) {
		writeSnapshot(w, "awaiting_switch")
		if conn == 0 {
			writeEvent(w, 1, "status_changed")
			return
		}
		writeEvent(w, 2, "status_changed")
		writeSnapshot(w, "succeeded")
	})
	seqs, _, err := collect(t, c)
	if err != nil || !reflect.DeepEqual(seqs, []int64{1, 2}) || s.conns() != 2 {
		t.Fatalf("err %v, seqs %v, connections %d", err, seqs, s.conns())
	}
}

// A finished run still replays its events; a stream that breaks during the
// replay must be resumed even though the snapshot already shows the end.
func TestWatchFinishesReplayOfEndedRun(t *testing.T) {
	c, s := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, conn int) {
		writeSnapshot(w, "rolled_back")
		if conn == 0 {
			writeEvent(w, 1, "step_started")
			writeEvent(w, 2, "rollback")
			abort()
		}
		writeEvent(w, 3, "done")
		writeSnapshot(w, "rolled_back")
	})
	seqs, _, err := collect(t, c)
	if err != nil || !reflect.DeepEqual(seqs, []int64{1, 2, 3}) {
		t.Fatalf("err %v, seqs %v", err, seqs)
	}
	if want := []string{"", "2"}; !reflect.DeepEqual(s.lastEventIDs(), want) {
		t.Errorf("Last-Event-ID = %q, want %q", s.lastEventIDs(), want)
	}
}

func TestWatchErrors(t *testing.T) {
	apiErr := func(w http.ResponseWriter, status int, code string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"error":{"code":%q,"message":"x"}}`, code)
	}
	t.Run("unknown run fails at once", func(t *testing.T) {
		c, s := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, _ int) { apiErr(w, 404, "run_not_found") })
		if _, _, err := collect(t, c); ErrorCode(err) != "run_not_found" || s.conns() != 1 {
			t.Fatalf("err %v after %d connections", err, s.conns())
		}
	})
	t.Run("4xx after a reconnect stops", func(t *testing.T) {
		c, s := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, conn int) {
			if conn == 0 {
				writeSnapshot(w, "running")
				return
			}
			apiErr(w, 401, "unauthorized")
		})
		if _, _, err := collect(t, c); ErrorCode(err) != "unauthorized" || s.conns() != 2 {
			t.Fatalf("err %v after %d connections", err, s.conns())
		}
	})
	t.Run("5xx after a reconnect is retried", func(t *testing.T) {
		c, s := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, conn int) {
			switch conn {
			case 0:
				writeSnapshot(w, "running")
			case 1, 2:
				apiErr(w, 503, "unavailable")
			default:
				writeSnapshot(w, "succeeded")
			}
		})
		if _, snaps, err := collect(t, c); err != nil || snaps != 2 || s.conns() != 4 {
			t.Fatalf("err %v, snapshots %d, connections %d", err, snaps, s.conns())
		}
	})
	t.Run("server down at the start", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		srv.Close()
		c := New(srv.URL, "tok")
		if _, _, err := collect(t, c); err == nil || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want the connection error", err)
		}
	})
	t.Run("callback error", func(t *testing.T) {
		errStop := errors.New("stop")
		c, s := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, _ int) { writeSnapshot(w, "running") })
		err := c.Watch(context.Background(), "run_1", func(Message) error { return errStop })
		if err != errStop || s.conns() != 1 {
			t.Fatalf("err %v after %d connections", err, s.conns())
		}
	})
	t.Run("context cancelled during backoff", func(t *testing.T) {
		c, _ := newSSEServer(t, func(w http.ResponseWriter, r *http.Request, _ int) { writeSnapshot(w, "running") })
		c.retryMin = time.Hour
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		start := time.Now()
		err := c.Watch(ctx, "run_1", func(Message) error {
			time.AfterFunc(50*time.Millisecond, cancel)
			return nil
		})
		if !errors.Is(err, context.Canceled) || time.Since(start) > 5*time.Second {
			t.Fatalf("err %v after %s", err, time.Since(start))
		}
	})
}
