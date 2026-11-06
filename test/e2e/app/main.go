// Command app is the E2E test backend. run.sh starts one per environment,
// each connected to that environment's CNPG database, behind the gateway.
//
//	POST /items {"id":"..."}  insert a row; 201, also when the id already exists
//	GET  /items/{id}          200 when the row exists, 404 when it does not
//	GET  /healthz             200 when the database answers (the gateway's health check)
//	PUT  /_fault?status=500&delayMs=1200&method=GET&dropEvery=5   inject a fault (failure scenarios)
//	DELETE /_fault            clear it
//
// Writes are idempotent by the client's id: a retry of a write that was
// committed but whose answer was lost inserts nothing and answers 201 again,
// so a client may retry any failed write with the same id (platform ADR 0028).
//
// Database errors are 500, so the gateway counts them as failed requests.
// A fault applies to /items requests of the given method (all methods when
// empty): wait delayMs, then answer status when it is not 0. dropEvery N
// commits every Nth POST and then closes the connection without an answer: the
// write exists but its client never hears so. /healthz is never faulted, so the
// gateway keeps the backend in rotation and the gate sees the fault. The driver
// calls /_fault on the app's own port, not through the gateway.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	listen, dsn := os.Getenv("APP_LISTEN"), os.Getenv("APP_DSN")
	if listen == "" || dsn == "" {
		log.Fatal("APP_LISTEN and APP_DSN are required")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		log.Fatal("APP_DSN: ", err)
	}
	cfg.MaxConns = 8
	// Connects lazily: the target database does not exist until the engine creates it.
	db, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		log.Fatal(err)
	}

	var fmu sync.Mutex
	var fault struct {
		status    int
		delay     time.Duration
		method    string
		dropEvery int
		posts     int
	}
	// dropAnswer reports whether this committed write's answer is to be lost.
	dropAnswer := func() bool {
		fmu.Lock()
		defer fmu.Unlock()
		if fault.dropEvery <= 0 {
			return false
		}
		fault.posts++
		return fault.posts%fault.dropEvery == 0
	}
	faulted := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			fmu.Lock()
			f := fault
			fmu.Unlock()
			if f.method == "" || f.method == r.Method {
				if f.delay > 0 {
					time.Sleep(f.delay)
				}
				if f.status != 0 {
					http.Error(w, "injected fault", f.status)
					return
				}
			}
			next(w, r)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("PUT /_fault", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		status, _ := strconv.Atoi(q.Get("status"))
		delay, _ := strconv.Atoi(q.Get("delayMs"))
		drop, _ := strconv.Atoi(q.Get("dropEvery"))
		fmu.Lock()
		fault.status, fault.delay, fault.method = status, time.Duration(delay)*time.Millisecond, q.Get("method")
		fault.dropEvery, fault.posts = drop, 0
		fmu.Unlock()
		log.Printf("fault set: status=%d delay=%dms method=%q dropEvery=%d", status, delay, q.Get("method"), drop)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /_fault", func(w http.ResponseWriter, r *http.Request) {
		fmu.Lock()
		fault.status, fault.delay, fault.method, fault.dropEvery = 0, 0, "", 0
		fmu.Unlock()
		log.Printf("fault cleared")
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /items", faulted(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ID == "" {
			http.Error(w, "body must be {\"id\":\"...\"}", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, err := db.Exec(ctx, "INSERT INTO items (id) VALUES ($1) ON CONFLICT (id) DO NOTHING", in.ID); err != nil {
			log.Printf("insert %s: %v", in.ID, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if dropAnswer() {
			// Committed, but the client never gets the answer.
			if hj, ok := w.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					conn.Close()
					return
				}
			}
		}
		w.WriteHeader(http.StatusCreated)
	}))
	mux.HandleFunc("GET /items/{id}", faulted(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		var one int
		err := db.QueryRow(ctx, "SELECT 1 FROM items WHERE id = $1", r.PathValue("id")).Scan(&one)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			http.Error(w, "not found", http.StatusNotFound)
		case err != nil:
			log.Printf("select: %v", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	log.Printf("listening on %s", listen)
	log.Fatal(http.ListenAndServe(listen, mux))
}
