// Command demo is a small stateless web service.
//
//	GET /         200 "<version> <DEMO_MESSAGE>"
//	GET /healthz  200, or 500 when DEMO_HEALTHY is "false"
//
// It listens on PORT and finishes the requests it has on SIGTERM. The
// version is the content of version.txt.
package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed version.txt
var version string

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		log.Fatal("PORT is not set")
	}
	healthy := os.Getenv("DEMO_HEALTHY") != "false"
	answer := strings.TrimSpace(version)
	if m := os.Getenv("DEMO_MESSAGE"); m != "" {
		answer += " " + m
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !healthy {
			http.Error(w, "unhealthy (DEMO_HEALTHY=false)", http.StatusInternalServerError)
			return
		}
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, answer)
	})
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		log.Printf("demo %s listening on :%s (healthy=%v)", answer, port, healthy)
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	<-ctx.Done()
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		log.Print(err)
	}
}
