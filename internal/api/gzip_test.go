package api

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGzipped(t *testing.T) {
	h := gzipped(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusTeapot, map[string]string{"a": "b"})
	})
	for _, tc := range []struct {
		accept string
		gzip   bool
	}{
		{"", false},
		{"gzip", true},
		{"br, GZIP;q=0.5", true},
		{"deflate", false},
		{"gzip;q=0", false},
		{"identity, gzip ; q=0.0", false},
	} {
		r := httptest.NewRequest(http.MethodGet, "/v1/security", nil)
		if tc.accept != "" {
			r.Header.Set("Accept-Encoding", tc.accept)
		}
		w := httptest.NewRecorder()
		h(w, r)
		if w.Code != http.StatusTeapot || w.Header().Get("Vary") != "Accept-Encoding" {
			t.Fatalf("%q: status %d vary %q", tc.accept, w.Code, w.Header().Get("Vary"))
		}
		body := io.Reader(w.Body)
		if got := w.Header().Get("Content-Encoding") == "gzip"; got != tc.gzip {
			t.Fatalf("%q: gzip=%v, want %v", tc.accept, got, tc.gzip)
		}
		if tc.gzip {
			zr, err := gzip.NewReader(w.Body)
			if err != nil {
				t.Fatalf("%q: %v", tc.accept, err)
			}
			body = zr
		}
		if b, _ := io.ReadAll(body); string(b) != "{\"a\":\"b\"}\n" {
			t.Fatalf("%q: body %q", tc.accept, b)
		}
	}
}
