// This disposable integration-test server exposes the actual pure registrar
// validator over TLS. It has no Kubernetes or cloud client and never mutates state.
package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onprem"
	admissionv1 "k8s.io/api/admission/v1"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:18443", "TLS listen address")
	cert := flag.String("cert", "", "disposable TLS certificate file")
	key := flag.String("key", "", "disposable TLS private key file")
	flag.Parse()
	if *cert == "" || *key == "" {
		os.Exit(2)
	}
	capacity := make(chan struct{}, 64)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.RequestURI() != "/v1/onprem/admission/cluster-secret" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		select {
		case capacity <- struct{}{}:
			defer func() { <-capacity }()
		default:
			http.Error(w, "admission capacity reached", http.StatusServiceUnavailable)
			return
		}
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(3 * time.Second))
		defer r.Body.Close()
		// UPDATE contains both old and new Secrets, each bounded to 64 KiB by
		// the validator, plus the surrounding AdmissionReview metadata.
		r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var review admissionv1.AdmissionReview
		if err := decoder.Decode(&review); err != nil {
			http.Error(w, "invalid admission review", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			http.Error(w, "invalid admission review", http.StatusBadRequest)
			return
		}
		out := onprem.ValidateClusterSecretAdmission(review, time.Now().UTC())
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(out)
	})
	server := &http.Server{Addr: *addr, Handler: handler, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 8 << 10}
	if server.ListenAndServeTLS(*cert, *key) != nil {
		os.Exit(1)
	}
}
