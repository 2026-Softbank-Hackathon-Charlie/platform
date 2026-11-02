package client

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// Message is one message of a run's event stream.
type Message struct {
	// Name is the SSE event name: "snapshot", or the RunEvent kind.
	Name string
	// Seq is the event seq. For a snapshot it is the stream's last event id
	// at that point, usually 0 for the first snapshot.
	Seq int64
	// Snapshot is the run state; set when Name is "snapshot".
	Snapshot *apiv1.Run
	// Event is set for every other message.
	Event *apiv1.RunEvent
}

// protocolError marks a response that breaks the contract; reconnecting
// would get the same answer.
type protocolError struct{ err error }

func (e *protocolError) Error() string { return e.err.Error() }
func (e *protocolError) Unwrap() error { return e.err }

// StreamEvents reads a run's event stream once and calls fn for each message.
// after > 0 is sent as Last-Event-ID so the server replays only events with a
// greater seq. It returns nil when the server ends the stream, fn's error as
// is when fn fails, and ctx.Err() when ctx is done.
func (c *Client) StreamEvents(ctx context.Context, runID string, after int64, fn func(Message) error) error {
	h := http.Header{"Accept": {"text/event-stream"}, "Cache-Control": {"no-cache"}}
	if after > 0 {
		h.Set("Last-Event-ID", strconv.FormatInt(after, 10))
	}
	resp, err := c.send(ctx, request{method: http.MethodGet, path: "/v1/migrations/%s/events", params: []string{runID}, header: h})
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return err
	}
	defer resp.Body.Close()
	if mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type")); mt != "text/event-stream" {
		return &protocolError{fmt.Errorf("client: event stream answered with Content-Type %q", resp.Header.Get("Content-Type"))}
	}

	var fnErr error
	err = readSSE(resp.Body, func(ev sseEvent) error {
		m, err := toMessage(ev)
		if err != nil {
			return err
		}
		if err := fn(m); err != nil {
			fnErr = err
			return err
		}
		return nil
	})
	switch {
	case fnErr != nil:
		return fnErr
	case ctx.Err() != nil:
		return ctx.Err()
	case errors.Is(err, bufio.ErrTooLong):
		return &protocolError{fmt.Errorf("client: event stream line exceeds %d bytes", maxSSELine)}
	case err != nil:
		var pe *protocolError
		if errors.As(err, &pe) {
			return err
		}
		return fmt.Errorf("client: read event stream: %w", err)
	}
	return nil
}

func toMessage(ev sseEvent) (Message, error) {
	m := Message{Name: ev.name}
	if n, err := strconv.ParseInt(ev.id, 10, 64); err == nil {
		m.Seq = n
	}
	if ev.name == "snapshot" {
		var run apiv1.Run
		if err := json.Unmarshal([]byte(ev.data), &run); err != nil {
			return m, &protocolError{fmt.Errorf("client: decode snapshot: %w", err)}
		}
		m.Snapshot = &run
		return m, nil
	}
	var e apiv1.RunEvent
	if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
		return m, &protocolError{fmt.Errorf("client: decode %q event: %w", ev.name, err)}
	}
	// The SSE id carries over to messages without one, so the event's own
	// seq is the reliable number.
	if e.Seq > 0 {
		m.Seq = e.Seq
	}
	m.Event = &e
	return m, nil
}

// Watch follows a run until it ends and calls fn for each message.
//
// After a dropped stream it reconnects with backoff, sending the last
// delivered seq as Last-Event-ID so missed events are replayed; an event seq
// is never delivered twice. Every connection starts with a fresh snapshot,
// which is delivered. Watch returns nil once a snapshot shows a status other
// than running or awaiting_switch and that stream has closed normally, fn's
// error as is, and ctx.Err() when ctx is done. An error before the first
// message (unknown run, bad token, server down) is returned at once; after
// that only 4xx answers, contract violations and maxEmptyStreams streams in a
// row that close without a message stop it. A first stream that closes
// without a message is an error too.
func (c *Client) Watch(ctx context.Context, runID string, fn func(Message) error) error {
	var (
		lastSeq   int64
		connected bool // a message arrived on some connection
		empty     int  // streams that closed without a message since the last message
		fnErr     error
	)
	delay := c.retryMin
	for {
		progressed := false
		ended := false // this stream's latest snapshot shows a final status
		err := c.StreamEvents(ctx, runID, lastSeq, func(m Message) error {
			progressed, connected = true, true
			if m.Event != nil && m.Seq > 0 {
				if m.Seq <= lastSeq {
					return nil // replayed after a reconnect
				}
				lastSeq = m.Seq
			}
			if m.Snapshot != nil {
				ended = finalStatus(m.Snapshot.Status)
			}
			if err := fn(m); err != nil {
				fnErr = err
				return err
			}
			return nil
		})
		switch {
		case fnErr != nil:
			return fnErr
		case ctx.Err() != nil:
			return ctx.Err()
		case err == nil && ended:
			return nil
		case err != nil && (!connected || !retryable(err)):
			return err
		case err == nil && !progressed:
			// A 200 that closes at once is a broken server or proxy, not a
			// dropped connection; reconnecting may never help.
			if !connected {
				return fmt.Errorf("client: event stream of run %s closed before its first message", runID)
			}
			if empty++; empty >= maxEmptyStreams {
				return fmt.Errorf("client: event stream of run %s closed without a message %d times in a row", runID, empty)
			}
		}

		// A stream that delivered something earns a quick reconnect; one that
		// did not backs off further.
		if progressed {
			delay, empty = c.retryMin, 0
		}
		t := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
		if !progressed {
			delay = min(2*delay, c.retryMax)
		}
	}
}

// maxEmptyStreams is how many streams in a row may close without a message
// before Watch gives up.
const maxEmptyStreams = 5

func finalStatus(s apiv1.RunStatus) bool {
	return s != apiv1.RunStatusRunning && s != apiv1.RunStatusAwaitingSwitch
}

func retryable(err error) bool {
	var pe *protocolError
	if errors.As(err, &pe) {
		return false
	}
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status >= 500 || ae.Status == http.StatusRequestTimeout || ae.Status == http.StatusTooManyRequests
	}
	return true // network errors
}

// sseEvent is one dispatched SSE message.
type sseEvent struct {
	name string // "message" when the stream gave no event name
	id   string // the last event id at dispatch time
	data string
}

// maxSSELine bounds one line; a snapshot arrives as a single data line.
const maxSSELine = 16 << 20

// readSSE parses an event stream as the WHATWG "Server-sent events" spec
// describes and calls fn for each dispatched message. A message cut off by
// the end of the stream is dropped, as the spec requires.
func readSSE(r io.Reader, fn func(sseEvent) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxSSELine)
	sc.Split(splitLines())
	var (
		name, id string
		data     strings.Builder
		hasData  bool
		first    = true
	)
	for sc.Scan() {
		line := sc.Text()
		if first {
			line = strings.TrimPrefix(line, "\ufeff")
			first = false
		}
		if line == "" {
			if hasData {
				ev := sseEvent{name: name, id: id, data: strings.TrimSuffix(data.String(), "\n")}
				if ev.name == "" {
					ev.name = "message"
				}
				if err := fn(ev); err != nil {
					return err
				}
			}
			name, hasData = "", false
			data.Reset()
			continue
		}
		if line[0] == ':' {
			continue // comment, e.g. a keep-alive
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			name = value
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
			hasData = true
		case "id":
			if !strings.ContainsRune(value, 0) {
				id = value
			}
		}
		// "retry" is ignored: Watch has its own backoff.
	}
	return sc.Err()
}

// splitLines splits on CRLF, LF or a lone CR, which SSE all allows. A CR is
// a line end right away, so a live stream never waits for the next byte; a
// following LF is then skipped.
func splitLines() bufio.SplitFunc {
	afterCR := false
	return func(data []byte, atEOF bool) (int, []byte, error) {
		skip := 0
		if afterCR && len(data) > 0 {
			afterCR = false
			if data[0] == '\n' {
				skip = 1
			}
		}
		rest := data[skip:]
		if i := bytes.IndexAny(rest, "\r\n"); i >= 0 {
			if rest[i] == '\r' {
				if i+1 == len(rest) {
					afterCR = true
				} else if rest[i+1] == '\n' {
					return skip + i + 2, rest[:i], nil
				}
			}
			return skip + i + 1, rest[:i], nil
		}
		if atEOF && len(rest) > 0 {
			return len(data), rest, nil
		}
		return skip, nil, nil
	}
}
