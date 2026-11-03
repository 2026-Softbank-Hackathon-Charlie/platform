// Command proxy is a forward HTTP proxy into the kind cluster it runs in. It
// lets programs on the host, the gateway (HTTP_PROXY) and run.sh's curl (-x),
// reach http://<service>.<namespace>.svc.cluster.local:<port> by the name a
// program in the cluster would use. Only plain HTTP to *.svc.cluster.local
// is forwarded; GET /healthz (not a proxy request) answers 200.
//
//	proxy -listen :3128
package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"
)

const suffix = ".svc.cluster.local"

func main() {
	listen := flag.String("listen", ":3128", "address to listen on")
	flag.Parse()

	// A proxy request carries the absolute URL, which is where it goes.
	forward := &httputil.ReverseProxy{
		Director:      func(*http.Request) {},
		FlushInterval: -1,
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case !r.URL.IsAbs() && r.URL.Path == "/healthz":
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodConnect || r.URL.Scheme != "http" || !strings.HasSuffix(r.URL.Hostname(), suffix):
			http.Error(w, "only plain HTTP to *"+suffix+" is forwarded", http.StatusForbidden)
		default:
			forward.ServeHTTP(w, r)
		}
	})
	log.Printf("forwarding to *%s on %s", suffix, *listen)
	log.Fatal(http.ListenAndServe(*listen, h))
}
