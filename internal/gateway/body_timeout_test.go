package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSlowHeldBodyCannotOutliveHoldBudget(t *testing.T) {
	tg := newTestGW(t, Config{MaxHold: 80 * time.Millisecond, BodyReadTimeout: time.Second}, "src")
	if err := tg.client.HoldWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", tg.proxy.URL+"/orders", pr)
	go func() { _, _ = pw.Write([]byte("incomplete")) }()
	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 503 || !strings.Contains(string(body), "hold_timeout") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if time.Since(start) > time.Second {
		t.Fatal("body read bypassed hold budget")
	}
	if tg.heldWrites() != 0 || tg.backends["src"].got("incomplete") != 0 {
		t.Fatal("request leaked slot or was forwarded")
	}
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestHeldBodyHasItsOwnReceiveDeadline(t *testing.T) {
	tg := newTestGW(t, Config{MaxHold: 2 * time.Second, BodyReadTimeout: 50 * time.Millisecond}, "src")
	if err := tg.client.HoldWrites(context.Background()); err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	defer pw.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", tg.proxy.URL+"/orders", pr)
	go func() { _, _ = pw.Write([]byte("partial")) }()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 408 || !strings.Contains(string(body), "body_timeout") {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	if tg.heldWrites() != 0 || tg.g.bufferedBytes.Load() != 0 {
		t.Fatal("timed out request retained resources")
	}
	if err := tg.client.ReleaseWrites(ctx); err != nil {
		t.Fatal(err)
	}
	if tg.backends["src"].got("partial") != 0 {
		t.Fatal("incomplete request was forwarded")
	}
}
