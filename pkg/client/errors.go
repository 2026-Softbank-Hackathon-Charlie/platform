package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// APIError is a non-2xx response. Code and Message come from the Error body;
// Code is empty when the body was not one (e.g. a proxy's HTML page).
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("HTTP %d: %s", e.Status, e.Message)
	}
	return fmt.Sprintf("%s: %s (HTTP %d)", e.Code, e.Message, e.Status)
}

// ErrorCode returns the API error code in err's chain, or "" when err is not
// an *APIError or the server sent no code.
func ErrorCode(err error) string {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Code
	}
	return ""
}

const (
	errBodyLimit   = 64 << 10 // bytes of an error body read
	errExcerptRune = 200      // runes of a non-Error body kept in Message
)

func decodeError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, errBodyLimit))
	e := &APIError{Status: resp.StatusCode}
	var body apiv1.Error
	if json.Unmarshal(b, &body) == nil && body.Error.Code != "" {
		e.Code, e.Message = body.Error.Code, body.Error.Message
		return e
	}
	// Not an Error body: keep a one-line excerpt so proxy pages and crashes
	// stay diagnosable.
	msg := strings.Join(strings.Fields(string(b)), " ")
	if r := []rune(msg); len(r) > errExcerptRune {
		msg = string(r[:errExcerptRune]) + "..."
	}
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	e.Message = msg
	return e
}
