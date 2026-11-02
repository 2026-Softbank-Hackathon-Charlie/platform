// Command app is the E2E test backend. run.sh starts one per environment,
// each connected to that environment's CNPG database, behind the gateway.
//
//	POST /items {"id":"..."}  insert a row; 201
//	GET  /items/{id}          200 when the row exists, 404 when it does not
//	GET  /healthz             200 when the database answers (the gateway's health check)
//
// Database errors are 500, so the gateway counts them as failed requests.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
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

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := db.Ping(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /items", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.ID == "" {
			http.Error(w, "body must be {\"id\":\"...\"}", http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, err := db.Exec(ctx, "INSERT INTO items (id) VALUES ($1)", in.ID); err != nil {
			log.Printf("insert %s: %v", in.ID, err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusCreated)
	})
	mux.HandleFunc("GET /items/{id}", func(w http.ResponseWriter, r *http.Request) {
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
	})
	log.Printf("listening on %s", listen)
	log.Fatal(http.ListenAndServe(listen, mux))
}
