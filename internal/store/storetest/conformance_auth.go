package storetest

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// ConformanceAuth runs the store.AuthStore contract against fresh stores
// from newStore, with the same rules as Conformance.
func ConformanceAuth(t *testing.T, newStore func(t *testing.T) store.Store) {
	cases := []struct {
		name string
		run  func(t *testing.T, s store.Store)
	}{
		{"SessionCreateGet", testSessionCreateGet},
		{"SessionDelete", testSessionDelete},
		{"SessionPurge", testSessionPurge},
		{"DeviceLoginCreateGet", testDeviceLoginCreateGet},
		{"DeviceLoginLimit", testDeviceLoginLimit},
		{"DeviceLoginLimitRace", testDeviceLoginLimitRace},
		{"DeviceLoginReview", testDeviceLoginReview},
		{"DeviceLoginReviewRace", testDeviceLoginReviewRace},
		{"DeviceLoginReviewKeepUntil", testDeviceLoginReviewKeepUntil},
		{"DeviceLoginExchange", testDeviceLoginExchange},
		{"DeviceLoginExchangeRace", testDeviceLoginExchangeRace},
		{"AuthText", testAuthText},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			s := newStore(t)
			t.Cleanup(s.Close)
			c.run(t, s)
		})
	}
}

func sum(secret string) []byte {
	h := sha256.Sum256([]byte(secret))
	return h[:]
}

const (
	consoleMinutes = 12 * 60
	cliMinutes     = 30 * 24 * 60
	deviceMinutes  = 10
	testClient     = "charlie v0.5.0 on rin-sbx-12"
)

func newSession(token string, createdAt int) store.Session {
	return store.Session{
		TokenHash: sum(token),
		Name:      "seohyun",
		Role:      "operator",
		Kind:      store.SessionConsole,
		CreatedAt: at(createdAt),
		ExpiresAt: at(createdAt + consoleMinutes),
	}
}

func newDeviceLogin(code, userCode string, createdAt int) store.DeviceLogin {
	return store.DeviceLogin{
		DeviceCodeHash: sum(code),
		UserCode:       userCode,
		Client:         testClient,
		Status:         store.DeviceLoginPending,
		CreatedAt:      at(createdAt),
		ExpiresAt:      at(createdAt + deviceMinutes),
	}
}

func eqSession(t *testing.T, got, want store.Session) {
	t.Helper()
	want.CreatedAt, want.ExpiresAt = pg(want.CreatedAt), pg(want.ExpiresAt)
	eqFields(t, "session", got, want)
}

func eqDeviceLogin(t *testing.T, got, want store.DeviceLogin) {
	t.Helper()
	want.CreatedAt, want.ExpiresAt, want.ReviewedAt = pg(want.CreatedAt), pg(want.ExpiresAt), pgPtr(want.ReviewedAt)
	eqFields(t, "device login "+want.UserCode, got, want)
}

// approve is an approval by by at minute n that gives the CLI token role.
func approve(by, role string, n int) store.DeviceLoginReview {
	return store.DeviceLoginReview{Status: store.DeviceLoginApproved, By: by, TokenRole: role, At: at(n)}
}

// deny is a denial by by at minute n.
func deny(by string, n int) store.DeviceLoginReview {
	return store.DeviceLoginReview{Status: store.DeviceLoginDenied, By: by, At: at(n)}
}

// reviewed returns d as ReviewDeviceLogin(r) leaves it.
func reviewed(d store.DeviceLogin, r store.DeviceLoginReview) store.DeviceLogin {
	d.Status, d.ReviewedBy, d.TokenRole, d.ReviewedAt = r.Status, r.By, r.TokenRole, ptr(r.At)
	if r.KeepUntil.After(d.ExpiresAt) {
		d.ExpiresAt = r.KeepUntil
	}
	return d
}

func testSessionCreateGet(t *testing.T, s store.Store) {
	ctx := t.Context()
	a := newSession("hcs_a", 0)
	must(t, s.CreateSession(ctx, a))
	got, err := s.GetSession(ctx, a.TokenHash, at(1))
	must(t, err)
	eqSession(t, got, a)

	cli := newSession("hcs_cli", 0)
	cli.Kind, cli.Client, cli.ExpiresAt = store.SessionCLI, testClient, at(cliMinutes)
	must(t, s.CreateSession(ctx, cli))
	got, err = s.GetSession(ctx, cli.TokenHash, at(1))
	must(t, err)
	eqSession(t, got, cli)

	// Valid until its ExpiresAt, at microsecond precision.
	_, err = s.GetSession(ctx, a.TokenHash, at(consoleMinutes).Add(-time.Microsecond))
	must(t, err)
	_, err = s.GetSession(ctx, a.TokenHash, at(consoleMinutes))
	wantErr(t, err, store.ErrNotFound)
	_, err = s.GetSession(ctx, sum("hcs_missing"), at(1))
	wantErr(t, err, store.ErrNotFound)

	wantErr(t, s.CreateSession(ctx, newSession("hcs_a", 1)), store.ErrConflict)
}

func testSessionDelete(t *testing.T, s store.Store) {
	ctx := t.Context()
	a, b := newSession("hcs_a", 0), newSession("hcs_b", 0)
	must(t, s.CreateSession(ctx, a))
	must(t, s.CreateSession(ctx, b))
	must(t, s.DeleteSession(ctx, a.TokenHash))
	_, err := s.GetSession(ctx, a.TokenHash, at(1))
	wantErr(t, err, store.ErrNotFound)
	wantErr(t, s.DeleteSession(ctx, a.TokenHash), store.ErrNotFound)
	if _, err := s.GetSession(ctx, b.TokenHash, at(1)); err != nil {
		t.Fatalf("deleting one session removed another: %v", err)
	}
}

func testSessionPurge(t *testing.T, s store.Store) {
	ctx := t.Context()
	a, b := newSession("hcs_a", 0), newSession("hcs_b", 1)
	must(t, s.CreateSession(ctx, a))
	must(t, s.CreateSession(ctx, b))
	// Created when a expires: a is removed, b (one minute younger) stays.
	must(t, s.CreateSession(ctx, newSession("hcs_c", consoleMinutes)))
	if _, err := s.GetSession(ctx, a.TokenHash, at(1)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expired session not removed by the next create: %v", err)
	}
	if _, err := s.GetSession(ctx, b.TokenHash, at(1)); err != nil {
		t.Fatalf("live session removed: %v", err)
	}
}

func testDeviceLoginCreateGet(t *testing.T, s store.Store) {
	ctx := t.Context()
	d := newDeviceLogin("hcd_a", "BCDFGHJK", 0)
	must(t, s.CreateDeviceLogin(ctx, d, 50))
	got, err := s.GetDeviceLogin(ctx, "BCDFGHJK", at(1))
	must(t, err)
	eqDeviceLogin(t, got, d)

	_, err = s.GetDeviceLogin(ctx, "BCDFGHJK", at(deviceMinutes))
	wantErr(t, err, store.ErrNotFound)
	_, err = s.GetDeviceLogin(ctx, "ZZZZZZZZ", at(1))
	wantErr(t, err, store.ErrNotFound)

	// A live user code or device code is taken.
	for _, dup := range []store.DeviceLogin{newDeviceLogin("hcd_b", "BCDFGHJK", 1), newDeviceLogin("hcd_a", "CDFGHJKL", 1)} {
		err := s.CreateDeviceLogin(ctx, dup, 50)
		wantErr(t, err, store.ErrConflict)
		if errors.Is(err, store.ErrDeviceLoginLimit) {
			t.Fatalf("a taken code reported as the limit: %v", err)
		}
	}
	// Once it expired, the create removes it and the user code is free.
	c := newDeviceLogin("hcd_c", "BCDFGHJK", deviceMinutes)
	must(t, s.CreateDeviceLogin(ctx, c, 50))
	got, err = s.GetDeviceLogin(ctx, "BCDFGHJK", at(deviceMinutes+1))
	must(t, err)
	eqDeviceLogin(t, got, c)
}

func testDeviceLoginLimit(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateDeviceLogin(ctx, newDeviceLogin("hcd_a", "BBBBBBBB", 0), 2))
	must(t, s.CreateDeviceLogin(ctx, newDeviceLogin("hcd_b", "CCCCCCCC", 0), 2))
	wantErr(t, s.CreateDeviceLogin(ctx, newDeviceLogin("hcd_c", "DDDDDDDD", 1), 2), store.ErrDeviceLoginLimit)

	// Only pending ones count.
	_, err := s.ReviewDeviceLogin(ctx, "BBBBBBBB", approve("seohyun", "operator", 1))
	must(t, err)
	must(t, s.CreateDeviceLogin(ctx, newDeviceLogin("hcd_c", "DDDDDDDD", 1), 2))
	wantErr(t, s.CreateDeviceLogin(ctx, newDeviceLogin("hcd_d", "FFFFFFFF", 2), 2), store.ErrDeviceLoginLimit)

	// Expired ones do not: a and b expire at 10, c at 11.
	must(t, s.CreateDeviceLogin(ctx, newDeviceLogin("hcd_d", "FFFFFFFF", deviceMinutes), 2))
	wantErr(t, s.CreateDeviceLogin(ctx, newDeviceLogin("hcd_e", "GGGGGGGG", deviceMinutes), 2), store.ErrDeviceLoginLimit)
}

func testDeviceLoginLimitRace(t *testing.T, s store.Store) {
	ctx := t.Context()
	const n, limit = 8, 3
	errs := make([]error, n)
	parallel(n, func(i int) {
		errs[i] = s.CreateDeviceLogin(ctx, newDeviceLogin(fmt.Sprint("hcd_", i), "BCDFGHJ"+string(rune('K'+i)), 0), limit)
	})
	created := 0
	for _, err := range errs {
		switch {
		case err == nil:
			created++
		case !errors.Is(err, store.ErrDeviceLoginLimit):
			t.Fatalf("concurrent create: %v", err)
		}
	}
	if created != limit {
		t.Fatalf("%d of %d concurrent creates succeeded with a limit of %d", created, n, limit)
	}
}

func testDeviceLoginReview(t *testing.T, s store.Store) {
	ctx := t.Context()
	d := newDeviceLogin("hcd_a", "BCDFGHJK", 0)
	must(t, s.CreateDeviceLogin(ctx, d, 50))
	got, err := s.ReviewDeviceLogin(ctx, "BCDFGHJK", approve("seohyun", "agent", 2))
	must(t, err)
	want := reviewed(d, approve("seohyun", "agent", 2))
	eqDeviceLogin(t, got, want)
	got, err = s.GetDeviceLogin(ctx, "BCDFGHJK", at(3))
	must(t, err)
	eqDeviceLogin(t, got, want)

	// Only a pending one can be reviewed.
	for _, r := range []store.DeviceLoginReview{approve("minji", "operator", 3), deny("minji", 3)} {
		_, err = s.ReviewDeviceLogin(ctx, "BCDFGHJK", r)
		wantErr(t, err, store.ErrConflict)
		if !strings.Contains(err.Error(), store.DeviceLoginApproved) {
			t.Errorf("conflict %q does not name the current status", err)
		}
	}
	_, err = s.ReviewDeviceLogin(ctx, "ZZZZZZZZ", approve("minji", "operator", 3))
	wantErr(t, err, store.ErrNotFound)

	// An expired one is not found, pending or not.
	must(t, s.CreateDeviceLogin(ctx, newDeviceLogin("hcd_b", "CDFGHJKL", 0), 50))
	_, err = s.ReviewDeviceLogin(ctx, "CDFGHJKL", deny("minji", deviceMinutes))
	wantErr(t, err, store.ErrNotFound)
	_, err = s.ReviewDeviceLogin(ctx, "BCDFGHJK", deny("minji", deviceMinutes))
	wantErr(t, err, store.ErrNotFound)
}

func testDeviceLoginReviewRace(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateDeviceLogin(ctx, newDeviceLogin("hcd_a", "BCDFGHJK", 0), 50))
	const n = 8
	var mu sync.Mutex
	var won []store.DeviceLogin
	errs := make([]error, n)
	parallel(n, func(i int) {
		r := approve(fmt.Sprint("admin", i), "operator", 1)
		if i%2 == 1 {
			r = deny(fmt.Sprint("admin", i), 1)
		}
		d, err := s.ReviewDeviceLogin(ctx, "BCDFGHJK", r)
		errs[i] = err
		if err == nil {
			mu.Lock()
			won = append(won, d)
			mu.Unlock()
		}
	})
	for _, err := range errs {
		if err != nil && !errors.Is(err, store.ErrConflict) {
			t.Fatalf("concurrent review: %v", err)
		}
	}
	if len(won) != 1 {
		t.Fatalf("%d of %d concurrent reviews applied, want 1", len(won), n)
	}
	got, err := s.GetDeviceLogin(ctx, "BCDFGHJK", at(2))
	must(t, err)
	if got.Status != won[0].Status || got.ReviewedBy != won[0].ReviewedBy {
		t.Fatalf("stored %s by %s, but the winner set %s by %s", got.Status, got.ReviewedBy, won[0].Status, won[0].ReviewedBy)
	}
}

func testDeviceLoginReviewKeepUntil(t *testing.T, s store.Store) {
	ctx := t.Context()
	// Approved at 9 with 10 more minutes: it now expires at 19, not 10.
	d := newDeviceLogin("hcd_a", "BCDFGHJK", 0)
	must(t, s.CreateDeviceLogin(ctx, d, 50))
	r := approve("seohyun", "agent", 9)
	r.KeepUntil = at(19)
	got, err := s.ReviewDeviceLogin(ctx, "BCDFGHJK", r)
	must(t, err)
	want := reviewed(d, r)
	if !want.ExpiresAt.Equal(at(19)) {
		t.Fatalf("test expects ExpiresAt 19, reviewed gives %v", want.ExpiresAt)
	}
	eqDeviceLogin(t, got, want)
	got, err = s.GetDeviceLogin(ctx, "BCDFGHJK", at(deviceMinutes+5))
	must(t, err)
	eqDeviceLogin(t, got, want)
	_, sess, err := s.ExchangeDeviceLogin(ctx, sum("hcd_a"), cliSession("hcs_late", 18), at(18))
	must(t, err)
	if sess.Name != "seohyun" || sess.Role != "agent" {
		t.Fatalf("late exchange gave %+v", sess)
	}

	// An earlier KeepUntil, or none (a denial), leaves ExpiresAt alone.
	for i, r := range []store.DeviceLoginReview{approve("seohyun", "agent", 1), deny("seohyun", 1)} {
		code := []string{"CDFGHJKL", "DFGHJKLM"}[i]
		d := newDeviceLogin(fmt.Sprint("hcd_", code), code, 0)
		must(t, s.CreateDeviceLogin(ctx, d, 50))
		if r.Status == store.DeviceLoginApproved {
			r.KeepUntil = at(5)
		}
		got, err := s.ReviewDeviceLogin(ctx, code, r)
		must(t, err)
		if !got.ExpiresAt.Equal(pg(at(deviceMinutes))) {
			t.Errorf("%s with KeepUntil %v moved ExpiresAt to %v", r.Status, r.KeepUntil, got.ExpiresAt)
		}
	}
}

func cliSession(token string, createdAt int) store.Session {
	return store.Session{TokenHash: sum(token), Kind: store.SessionCLI, CreatedAt: at(createdAt), ExpiresAt: at(createdAt + cliMinutes)}
}

func testDeviceLoginExchange(t *testing.T, s store.Store) {
	ctx := t.Context()
	d := newDeviceLogin("hcd_a", "BCDFGHJK", 0)
	must(t, s.CreateDeviceLogin(ctx, d, 50))
	sess := cliSession("hcs_cli", 3)

	// Pending: nothing changes.
	got, gotS, err := s.ExchangeDeviceLogin(ctx, sum("hcd_a"), sess, at(1))
	must(t, err)
	eqDeviceLogin(t, got, d)
	if gotS.TokenHash != nil {
		t.Fatalf("a pending exchange returned session %+v", gotS)
	}
	if _, err := s.GetSession(ctx, sess.TokenHash, at(1)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a pending exchange stored a session: %v", err)
	}

	// Approved: the device login becomes the session, once, with the
	// approver's name and the role they picked.
	_, err = s.ReviewDeviceLogin(ctx, "BCDFGHJK", approve("seohyun", "viewer", 2))
	must(t, err)
	got, gotS, err = s.ExchangeDeviceLogin(ctx, sum("hcd_a"), sess, at(3))
	must(t, err)
	eqDeviceLogin(t, got, reviewed(d, approve("seohyun", "viewer", 2)))
	wantS := sess
	wantS.Name, wantS.Role, wantS.Client = "seohyun", "viewer", testClient
	eqSession(t, gotS, wantS)
	stored, err := s.GetSession(ctx, sess.TokenHash, at(4))
	must(t, err)
	eqSession(t, stored, wantS)
	_, _, err = s.ExchangeDeviceLogin(ctx, sum("hcd_a"), cliSession("hcs_again", 4), at(4))
	wantErr(t, err, store.ErrNotFound)
	_, err = s.GetDeviceLogin(ctx, "BCDFGHJK", at(4))
	wantErr(t, err, store.ErrNotFound)
	_, _, err = s.ExchangeDeviceLogin(ctx, sum("hcd_missing"), cliSession("hcs_x", 4), at(4))
	wantErr(t, err, store.ErrNotFound)

	// Denied: removed, no session.
	must(t, s.CreateDeviceLogin(ctx, newDeviceLogin("hcd_b", "CDFGHJKL", 0), 50))
	_, err = s.ReviewDeviceLogin(ctx, "CDFGHJKL", deny("minji", 1))
	must(t, err)
	got, gotS, err = s.ExchangeDeviceLogin(ctx, sum("hcd_b"), cliSession("hcs_denied", 2), at(2))
	must(t, err)
	if got.Status != store.DeviceLoginDenied || gotS.TokenHash != nil {
		t.Fatalf("denied exchange = %+v, %+v", got, gotS)
	}
	if _, err := s.GetSession(ctx, sum("hcs_denied"), at(2)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a denied exchange stored a session: %v", err)
	}
	_, _, err = s.ExchangeDeviceLogin(ctx, sum("hcd_b"), cliSession("hcs_denied", 2), at(2))
	wantErr(t, err, store.ErrNotFound)

	// Approved but expired: not found, no session.
	must(t, s.CreateDeviceLogin(ctx, newDeviceLogin("hcd_c", "DFGHJKLM", 0), 50))
	_, err = s.ReviewDeviceLogin(ctx, "DFGHJKLM", approve("minji", "operator", 1))
	must(t, err)
	_, _, err = s.ExchangeDeviceLogin(ctx, sum("hcd_c"), cliSession("hcs_late", deviceMinutes), at(deviceMinutes))
	wantErr(t, err, store.ErrNotFound)
	if _, err := s.GetSession(ctx, sum("hcs_late"), at(deviceMinutes)); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("an expired exchange stored a session: %v", err)
	}

	// A taken token hash changes nothing: the device login can still be exchanged.
	must(t, s.CreateDeviceLogin(ctx, newDeviceLogin("hcd_e", "FGHJKLMN", 20), 50))
	_, err = s.ReviewDeviceLogin(ctx, "FGHJKLMN", approve("minji", "operator", 21))
	must(t, err)
	_, _, err = s.ExchangeDeviceLogin(ctx, sum("hcd_e"), cliSession("hcs_cli", 21), at(21))
	wantErr(t, err, store.ErrConflict)
	got, gotS, err = s.ExchangeDeviceLogin(ctx, sum("hcd_e"), cliSession("hcs_new", 21), at(21))
	must(t, err)
	if got.Status != store.DeviceLoginApproved || gotS.Name != "minji" {
		t.Fatalf("exchange after a conflict = %+v, %+v", got, gotS)
	}
}

func testDeviceLoginExchangeRace(t *testing.T, s store.Store) {
	ctx := t.Context()
	must(t, s.CreateDeviceLogin(ctx, newDeviceLogin("hcd_a", "BCDFGHJK", 0), 50))
	_, err := s.ReviewDeviceLogin(ctx, "BCDFGHJK", approve("seohyun", "operator", 1))
	must(t, err)
	const n = 8
	errs := make([]error, n)
	statuses := make([]string, n)
	parallel(n, func(i int) {
		d, _, err := s.ExchangeDeviceLogin(ctx, sum("hcd_a"), cliSession(fmt.Sprint("hcs_", i), 2), at(2))
		errs[i], statuses[i] = err, d.Status
	})
	exchanged, sessions := 0, 0
	for i, err := range errs {
		switch {
		case err == nil && statuses[i] == store.DeviceLoginApproved:
			exchanged++
		case !errors.Is(err, store.ErrNotFound):
			t.Fatalf("concurrent exchange: %s %v", statuses[i], err)
		}
		if _, err := s.GetSession(ctx, sum(fmt.Sprint("hcs_", i)), at(3)); err == nil {
			sessions++
		}
	}
	if exchanged != 1 || sessions != 1 {
		t.Fatalf("%d of %d concurrent exchanges got the token and %d sessions exist, want 1 and 1", exchanged, n, sessions)
	}
}

func testAuthText(t *testing.T, s store.Store) {
	ctx := t.Context()
	d := newDeviceLogin("hcd_a", "BCDFGHJK", 0)
	d.Client = "charlie\x00 on \xffhost"
	must(t, s.CreateDeviceLogin(ctx, d, 50))
	got, err := s.GetDeviceLogin(ctx, "BCDFGHJK", at(1))
	must(t, err)
	if got.Client != "charlie on �host" {
		t.Fatalf("client stored as %q", got.Client)
	}

	// A user code that is not clean names nothing.
	_, err = s.GetDeviceLogin(ctx, "BCDF\x00GHJK", at(1))
	wantErr(t, err, store.ErrNotFound)
	_, err = s.ReviewDeviceLogin(ctx, "BCDF\x00GHJK", approve("seohyun", "operator", 1))
	wantErr(t, err, store.ErrNotFound)

	_, err = s.ReviewDeviceLogin(ctx, "BCDFGHJK", approve("sco\x00tt", "operator", 1))
	must(t, err)
	_, sess, err := s.ExchangeDeviceLogin(ctx, sum("hcd_a"), cliSession("hcs_a", 2), at(2))
	must(t, err)
	if sess.Name != "scott" || sess.Client != "charlie on �host" {
		t.Fatalf("session stored as %q from %q", sess.Name, sess.Client)
	}

	a := newSession("hcs_b", 0)
	a.Name = "min\x00ji"
	must(t, s.CreateSession(ctx, a))
	stored, err := s.GetSession(ctx, a.TokenHash, at(1))
	must(t, err)
	if stored.Name != "minji" {
		t.Fatalf("session name stored as %q", stored.Name)
	}
}
