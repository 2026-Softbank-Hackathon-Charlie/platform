package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/jackc/pgx/v5"
)

func TestWrittenAppendCancelledWhileWaitingForStreamLock(t *testing.T) {
	schema, dsn := newSchema(t)
	s := openSchema(t, dsn)
	defer s.Close()
	conn, err := pgx.Connect(t.Context(), testDatabaseURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	tx, err := conn.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	stream := "cancel-" + schema
	if _, err := tx.Exec(t.Context(), `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "written-records:"+stream); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	now := time.Now()
	records := []store.WrittenRecord{{Table: "app.reservations", PK: "1", EventID: "reservation-1", At: now, ReceivedAt: now}}
	started := time.Now()
	if err := s.AppendWrittenRecords(ctx, stream, records); err == nil || time.Since(started) > 2*time.Second {
		t.Fatalf("cancelled lock wait did not stop promptly: %v", err)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendWrittenRecords(t.Context(), stream, records); err != nil {
		t.Fatal(err)
	}
	stats, err := s.WrittenRecordStats(t.Context(), stream)
	if err != nil || stats.Count != 1 {
		t.Fatalf("cancelled request left partial records: %+v %v", stats, err)
	}
}
