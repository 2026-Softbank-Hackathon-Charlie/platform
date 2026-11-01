// Command gateway is the HopCharlie traffic gateway. NOT IMPLEMENTED YET.
//
// It will sit outside every cluster (behind Cloudflare Tunnel) and proxy user
// traffic to one backend environment. The migration engine drives it through
// this admin API (pkg/gateway in migration-engine, still a proposal):
//
//	GET  /healthz                     200 when ready
//	POST /v1/writes/hold              hold new writes; return once in-flight writes drained
//	POST /v1/writes/release           flush held writes to the current backend
//	POST /v1/route {"backend":"eks"}  send 100% of traffic to that backend
//
// Until it exists, every endpoint answers 501 and /healthz answers 503, so an
// engine run in gateway "http" mode fails at precheck instead of mid-cutover.
package main

import (
	"encoding/json"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"time"
)

func main() {
	addr := flag.String("admin-addr", ":9901", "admin API listen address")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	notImplemented := func(w http.ResponseWriter, status int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]map[string]string{
			"error": {"code": "not_implemented", "message": "the traffic gateway is not implemented yet"},
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { notImplemented(w, http.StatusServiceUnavailable) })
	for _, p := range []string{"POST /v1/writes/hold", "POST /v1/writes/release", "POST /v1/route"} {
		mux.HandleFunc(p, func(w http.ResponseWriter, _ *http.Request) { notImplemented(w, http.StatusNotImplemented) })
	}

	log.Warn("gateway is a stub: all admin endpoints return 501", "adminAddr", *addr)
	srv := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	if err := srv.ListenAndServe(); err != nil {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}
