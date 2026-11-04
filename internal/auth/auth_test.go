package auth

import (
	"bytes"
	"crypto/sha256"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// testHash hashes at the lowest cost so tests stay fast.
func testHash(t *testing.T, password string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	return string(h)
}

func TestParseAdmins(t *testing.T) {
	seohyun, minji := testHash(t, "correct horse"), testHash(t, "battery staple")
	a, err := ParseAdmins(" seohyun:"+seohyun+" , minji.k:"+minji+",", bcrypt.MinCost)
	if err != nil || a.Len() != 2 || !a.Has("SEOHYUN") || !a.Has("minji.k") || a.Has("nobody") {
		t.Fatalf("good spec: %v, %d admins", err, a.Len())
	}
	if a, err := ParseAdmins("", MinHashCost); err != nil || a.Len() != 0 {
		t.Fatalf("empty spec: %v, %d admins", err, a.Len())
	}

	notBcrypt := "$1$saltsalt$NotABcryptHashAtAllButLongEnoughToPassTheLengthCheck01"
	// bcrypt.Cost only reads the prefix, so this parses as cost 15 without
	// hashing anything; ParseAdmins must refuse it before making a dummy.
	cost15 := "$2a$15$" + strings.Repeat("a", 53)
	h, err := bcrypt.GenerateFromPassword([]byte("x"), bcrypt.MinCost+1)
	if err != nil {
		t.Fatal(err)
	}
	cost5 := string(h)
	for name, c := range map[string]struct {
		spec, want string
		min        int
	}{
		"too cheap":     {"seohyun:" + seohyun, `admin "seohyun": the password hash has bcrypt cost 4; use 10 to 14`, MinHashCost},
		"too costly":    {"seohyun:" + cost15, "bcrypt cost 15; use 4 to 14", 0},
		"mixed costs":   {"seohyun:" + seohyun + ",minji:" + cost5, `admin "minji": the password hash has bcrypt cost 5 but the ones before it have 4`, 0},
		"bad hash":      {"seohyun:" + notBcrypt, `admin "seohyun": the password hash is not a bcrypt hash`, 0},
		"short hash":    {"seohyun:$2a$12$abc", "not a bcrypt hash", 0},
		"no hash":       {"seohyun", "admin entry 1: want name:<bcrypt hash>", 0},
		"hash only":     {seohyun, "admin entry 1: want name:<bcrypt hash>", 0},
		"empty hash":    {"seohyun:", "want name:<bcrypt hash>", 0},
		"bad name":      {"minji:" + minji + ",se hyun:" + seohyun, "admin entry 2: name must use", 0},
		"reversed":      {seohyun + ":seohyun", "name must use", 0},
		"duplicate":     {"seohyun:" + seohyun + ",seohyun:" + minji, `admin "seohyun" appears twice`, 0},
		"case variants": {"seohyun:" + seohyun + ",SeoHyun:" + minji, `admin "SeoHyun" appears twice`, 0},
	} {
		if c.min == 0 {
			c.min = bcrypt.MinCost
		}
		_, err := ParseAdmins(c.spec, c.min)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, c.want)
			continue
		}
		// No error quotes a hash, not even its first bytes.
		for _, h := range []string{seohyun, minji, notBcrypt, "$2a$12$abc", cost15, cost5} {
			if strings.Contains(err.Error(), h[:7]) {
				t.Errorf("%s: error %q contains a hash", name, err)
			}
		}
	}
}

func TestAdminsCheck(t *testing.T) {
	long := strings.Repeat("x", 72)
	a, err := ParseAdmins("SeoHyun:"+testHash(t, "correct horse")+",long:"+testHash(t, long), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"SeoHyun", "seohyun", "SEOHYUN"} {
		p, ok := a.Check(name, "correct horse")
		if !ok || p != (Principal{Name: "SeoHyun", Role: apiv1.RoleOperator}) {
			t.Errorf("Check(%q) = %+v, %v; want the configured name as operator", name, p, ok)
		}
	}
	for _, c := range []struct{ name, password string }{
		{"seohyun", "wrong"},
		{"seohyun", ""},
		{"nobody", "correct horse"},
		{"", ""},
		// bcrypt reads 72 bytes: what follows must not log in.
		{"long", long + "anything"},
	} {
		if p, ok := a.Check(c.name, c.password); ok {
			t.Errorf("Check(%q, %q) = %+v, want refused", c.name, c.password, p)
		}
	}
	if _, ok := a.Check("long", long); !ok {
		t.Error("a 72-byte password was refused")
	}
	if _, ok := (&Admins{}).Check("seohyun", "correct horse"); ok {
		t.Error("no administrators accepted a login")
	}
}

func TestHashPassword(t *testing.T) {
	h, err := HashPassword([]byte("correct horse"))
	if err != nil {
		t.Fatal(err)
	}
	if cost, err := bcrypt.Cost([]byte(h)); err != nil || cost != HashCost {
		t.Fatalf("cost %d (%v), want %d", cost, err, HashCost)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(h), []byte("correct horse")); err != nil {
		t.Fatalf("the hash does not match its password: %v", err)
	}
	for _, pw := range []string{"", strings.Repeat("x", 73)} {
		if _, err := HashPassword([]byte(pw)); err == nil {
			t.Errorf("HashPassword(%d bytes) succeeded", len(pw))
		}
	}
}

func TestLoginLimiter(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	l := NewLoginLimiter(3, 10*time.Minute, 100)
	for i := range 3 {
		if _, ok := l.Attempt("Alice", t0.Add(time.Duration(i)*time.Minute)); !ok {
			t.Fatalf("attempt %d refused", i+1)
		}
	}
	// Any case is the same username; the oldest attempt leaves at 10:00.
	if wait, ok := l.Attempt("ALICE", t0.Add(4*time.Minute)); ok || wait != 6*time.Minute {
		t.Fatalf("fourth attempt: %v %v, want refused for 6m", ok, wait)
	}
	if _, ok := l.Attempt("bob", t0.Add(4*time.Minute)); !ok {
		t.Fatal("another username is limited too")
	}
	// A refused attempt is not counted: once the first leaves, one more fits.
	if _, ok := l.Attempt("alice", t0.Add(10*time.Minute)); !ok {
		t.Fatal("attempt after the oldest left the window refused")
	}
	if wait, ok := l.Attempt("alice", t0.Add(10*time.Minute)); ok || wait != time.Minute {
		t.Fatalf("next attempt: %v %v, want refused for 1m", ok, wait)
	}
	l.Reset("Alice")
	if _, ok := l.Attempt("alice", t0.Add(10*time.Minute)); !ok {
		t.Fatal("attempt after Reset refused")
	}
}

func TestLoginLimiterBounded(t *testing.T) {
	t0 := time.Date(2026, 11, 4, 9, 0, 0, 0, time.UTC)
	l := NewLoginLimiter(2, 10*time.Minute, 3)
	l.Attempt("old", t0)
	l.Attempt("target", t0.Add(time.Minute))
	l.Attempt("target", t0.Add(time.Minute))
	l.Attempt("x", t0.Add(2*time.Minute))
	// Full: "old" is forgotten first (its newest attempt is oldest), not target.
	l.Attempt("y", t0.Add(3*time.Minute))
	if len(l.attempts) != 3 {
		t.Fatalf("%d usernames kept, want at most 3", len(l.attempts))
	}
	if _, ok := l.attempts["old"]; ok {
		t.Fatal("the username with the oldest attempt was kept")
	}
	if _, ok := l.Attempt("target", t0.Add(3*time.Minute)); ok {
		t.Fatal("target's attempts were forgotten while others were older")
	}
	// Spraying usernames never grows it past the bound; entries whose
	// attempts left the window go first.
	for i := range 1000 {
		l.Attempt("spray"+strings.Repeat("x", i%7)+string(rune('a'+i%26)), t0.Add(30*time.Minute))
	}
	if len(l.attempts) > 3 {
		t.Fatalf("%d usernames kept after spraying, want at most 3", len(l.attempts))
	}
}

func TestUserCodes(t *testing.T) {
	seen := map[rune]int{}
	for range 1000 {
		c := NewUserCode()
		if len(c) != UserCodeLength {
			t.Fatalf("user code %q", c)
		}
		for _, r := range c {
			if !strings.ContainsRune(userCodeAlphabet, r) {
				t.Fatalf("user code %q has %q", c, r)
			}
			seen[r]++
		}
	}
	// 8000 letters over 20: each about 400.
	for _, r := range userCodeAlphabet {
		if seen[r] < 250 || seen[r] > 550 {
			t.Errorf("letter %c drawn %d times of 8000", r, seen[r])
		}
	}

	for in, want := range map[string]string{
		"BCDFGHJK":       "BCDFGHJK",
		"bcdf-ghjk":      "BCDFGHJK",
		" bcdf ghjk ":    "BCDFGHJK",
		"B-C-D-F-GHJK":   "BCDFGHJK",
		"BCDF-GHJA":      "", // A is not in the alphabet
		"BCDF-GHJ":       "",
		"BCDF-GHJKL":     "",
		"BCDF\tGHJK":     "",
		"bcdf-ghj\u212a": "", // KELVIN SIGN upper-cases to itself, not K
	} {
		got, ok := NormalizeUserCode(in)
		if got != want || ok != (want != "") {
			t.Errorf("NormalizeUserCode(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	if got := FormatUserCode("BCDFGHJK"); got != "BCDF-GHJK" {
		t.Errorf("FormatUserCode = %q", got)
	}
}

func TestSecrets(t *testing.T) {
	tok, sum := NewSessionToken()
	code, codeSum := NewDeviceCode()
	tok2, _ := NewSessionToken()
	for _, c := range []struct {
		secret, prefix string
		sum            []byte
	}{{tok, SessionPrefix, sum}, {code, DeviceCodePrefix, codeSum}} {
		want := sha256.Sum256([]byte(c.secret))
		if !strings.HasPrefix(c.secret, c.prefix) || len(c.secret) != len(c.prefix)+43 ||
			!bytes.Equal(c.sum, want[:]) || !bytes.Equal(HashSecret(c.secret), want[:]) {
			t.Errorf("secret %q (sum %x)", c.secret, c.sum)
		}
		if strings.ContainsAny(c.secret[len(c.prefix):], "+/=") {
			t.Errorf("secret %q is not base64url without padding", c.secret)
		}
	}
	if tok == tok2 {
		t.Error("two session tokens are equal")
	}
}
