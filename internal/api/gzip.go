package api

import (
	"compress/gzip"
	"net/http"
	"strconv"
	"strings"
)

// gzipped compresses the answer of h when the client accepts gzip. The
// security views are megabytes of JSON (GET /v1/security/vulnerabilities
// was 5.2 MB on 2026-11-07) that the console reads again every minute.
func gzipped(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			h(w, r)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		zw := gzip.NewWriter(w)
		defer zw.Close()
		h(gzipWriter{ResponseWriter: w, zw: zw}, r)
	}
}

type gzipWriter struct {
	http.ResponseWriter
	zw *gzip.Writer
}

func (g gzipWriter) Write(b []byte) (int, error) { return g.zw.Write(b) }

// acceptsGzip reports whether an Accept-Encoding header allows gzip
// ("gzip;q=0" refuses it).
func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		coding, params, _ := strings.Cut(part, ";")
		if !strings.EqualFold(strings.TrimSpace(coding), "gzip") {
			continue
		}
		for _, p := range strings.Split(params, ";") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(p), "q="); ok {
				q, err := strconv.ParseFloat(v, 64)
				return err == nil && q > 0
			}
		}
		return true
	}
	return false
}
