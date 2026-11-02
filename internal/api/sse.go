package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// SSEHeartbeat keeps idle connections open through proxies (Cloudflare drops
// idle streams after about 100 seconds).
var SSEHeartbeat = 15 * time.Second

// GET /v1/migrations/{runId}/events
//
// Stream contract:
//   - first message: "snapshot" with the current run (the source of truth)
//   - then stored events after Last-Event-ID (or ?after=), then live events;
//     the SSE event name is the kind and the SSE id is the event seq
//   - when the run ends: a final "snapshot", then the stream closes
//
// Events are stored before they are delivered, so a client that reconnects
// with the last id it saw misses nothing.
func (s *Server) migrationEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("runId")
	ctx := r.Context()
	fl, ok := w.(http.Flusher)
	if !ok {
		s.internal(w, r, errors.New("response writer cannot stream"))
		return
	}
	after, err := lastEventID(r)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	live, cancel, err := s.d.Runs.Subscribe(ctx, id) // before reading history, so nothing falls in between
	if err != nil {
		s.runError(w, r, err)
		return
	}
	defer cancel()
	snap, err := s.d.Runs.Get(ctx, id)
	if err != nil {
		s.runError(w, r, err)
		return
	}
	history, err := s.d.Runs.Events(ctx, id, after)
	if err != nil {
		s.internal(w, r, err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if writeSSE(w, "snapshot", 0, snap) != nil {
		return
	}
	last := after
	for _, ev := range history {
		if writeSSE(w, ev.Kind, ev.Seq, ev) != nil {
			return
		}
		last = max(last, ev.Seq)
	}
	fl.Flush()

	tick := time.NewTicker(SSEHeartbeat)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			fl.Flush()
		case ev, open := <-live:
			if !open {
				// The run ended (or this client fell behind): send the
				// authoritative state and end; a lagging client reconnects.
				if snap, err := s.d.Runs.Get(ctx, id); err == nil {
					_ = writeSSE(w, "snapshot", 0, snap)
					fl.Flush()
				}
				return
			}
			if ev.Seq > 0 && ev.Seq <= last {
				continue // already sent from history
			}
			if writeSSE(w, ev.Kind, ev.Seq, ev) != nil {
				return
			}
			last = max(last, ev.Seq)
			fl.Flush()
		}
	}
}

func lastEventID(r *http.Request) (int64, error) {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("after")
	}
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < 0 {
		return 0, errors.New("Last-Event-ID / after must be a non-negative event seq")
	}
	return n, nil
}

func writeSSE(w io.Writer, event string, seq int64, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if seq > 0 {
		_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", seq, event, b)
	} else {
		_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	}
	return err
}
