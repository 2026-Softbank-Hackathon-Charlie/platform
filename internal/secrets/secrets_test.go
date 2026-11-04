package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

var testKey = bytes.Repeat([]byte{7}, KeySize)

func newCipher(t *testing.T) *Cipher {
	t.Helper()
	c, err := NewCipher(testKey)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFromEnv(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k != KeyEnv {
				t.Fatalf("read %s", k)
			}
			return v
		}
	}
	if c, err := FromEnv(env("")); c != nil || err != nil {
		t.Fatalf("unset: %v, %v; want secrets off", c, err)
	}
	if c, err := FromEnv(env("  ")); c != nil || err != nil {
		t.Fatalf("blank: %v, %v; want secrets off", c, err)
	}
	good := base64.StdEncoding.EncodeToString(testKey)
	if c, err := FromEnv(env(good + "\n")); c == nil || err != nil {
		t.Fatalf("valid key: %v, %v", c, err)
	}
	short := base64.StdEncoding.EncodeToString(testKey[:16])
	for name, v := range map[string]string{
		"not base64": "not base64!",
		"16 bytes":   short,
		"raw text":   strings.Repeat("k", 32),
		"url base64": base64.URLEncoding.EncodeToString(append(bytes.Repeat([]byte{0xfb}, 31), 0xff)),
	} {
		c, err := FromEnv(env(v))
		if c != nil || err == nil {
			t.Errorf("%s: %v, %v; want an error", name, c, err)
			continue
		}
		if strings.Contains(err.Error(), v) {
			t.Errorf("%s: the error repeats the value: %v", name, err)
		}
	}
}

func TestSealOpen(t *testing.T) {
	c := newCipher(t)
	nonce, ct, v, err := c.Seal("moimroom", "", "HOST_CODE", "4821")
	if err != nil {
		t.Fatal(err)
	}
	if v != KeyVersion || len(nonce) != 12 || bytes.Contains(ct, []byte("4821")) {
		t.Fatalf("version %d, nonce %d bytes, ciphertext %q", v, len(nonce), ct)
	}
	got, err := c.Open("moimroom", "", "HOST_CODE", v, nonce, ct)
	if err != nil || got != "4821" {
		t.Fatalf("Open = %q, %v", got, err)
	}

	// Moving the ciphertext to another row, version or key fails, and the
	// error never shows the value.
	other, _ := NewCipher(bytes.Repeat([]byte{8}, KeySize))
	for name, open := range map[string]func() (string, error){
		"other app":      func() (string, error) { return c.Open("shop", "", "HOST_CODE", v, nonce, ct) },
		"other instance": func() (string, error) { return c.Open("moimroom", "moimroom--dev", "HOST_CODE", v, nonce, ct) },
		"other name":     func() (string, error) { return c.Open("moimroom", "", "ADMIN_TOKEN", v, nonce, ct) },
		"other key":      func() (string, error) { return other.Open("moimroom", "", "HOST_CODE", v, nonce, ct) },
		"short nonce":    func() (string, error) { return c.Open("moimroom", "", "HOST_CODE", v, nonce[:4], ct) },
		"tampered": func() (string, error) {
			bad := bytes.Clone(ct)
			bad[0] ^= 1
			return c.Open("moimroom", "", "HOST_CODE", v, nonce, bad)
		},
		"other version": func() (string, error) { return c.Open("moimroom", "", "HOST_CODE", 2, nonce, ct) },
	} {
		got, err := open()
		if err == nil {
			t.Errorf("%s: decrypted to %q", name, got)
		} else if strings.Contains(err.Error(), "4821") {
			t.Errorf("%s: error shows the value: %v", name, err)
		}
	}

	// Fields joined with a separator would let ("a/b", "", "C") and
	// ("a", "b/", "C") share additional data; length prefixes do not.
	n3, ct3, v3, _ := c.Seal("a/b", "", "C", "x")
	if got, err := c.Open("a", "b/", "C", v3, n3, ct3); err == nil {
		t.Fatalf("a shifted separator decrypted to %q", got)
	}

	// A random nonce every time.
	n2, ct2, _, _ := c.Seal("moimroom", "", "HOST_CODE", "4821")
	if bytes.Equal(nonce, n2) || bytes.Equal(ct, ct2) {
		t.Fatal("two seals of one value share a nonce or ciphertext")
	}
}

func TestRowAndInstanceValues(t *testing.T) {
	c := newCipher(t)
	row := func(instance, name, value string) store.AppSecret {
		r, err := c.Row("moimroom", instance, name, value, "seohyun")
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// As ListAppSecrets returns them: by instance, "" first.
	rows := []store.AppSecret{
		row("", "ADMIN_TOKEN", "app-admin"),
		row("", "HOST_CODE", "app-host"),
		row("moimroom", "HOST_CODE", "default-host"),
		row("moimroom--dev", "DEV_ONLY", "dev"),
		row("moimroom--dev", "HOST_CODE", "dev-host"),
	}
	if r := rows[2]; r.App != "moimroom" || r.Instance != "moimroom" || r.Name != "HOST_CODE" || r.UpdatedBy != "seohyun" ||
		r.KeyVersion != KeyVersion || r.KeyCheck != c.KeyCheck() || len(r.KeyCheck) != 12 {
		t.Fatalf("row %+v", r)
	}
	for instance, want := range map[string]map[string]string{
		"moimroom":      {"ADMIN_TOKEN": "app-admin", "HOST_CODE": "default-host"},
		"moimroom--dev": {"ADMIN_TOKEN": "app-admin", "HOST_CODE": "dev-host", "DEV_ONLY": "dev"},
		"moimroom--qa":  {"ADMIN_TOKEN": "app-admin", "HOST_CODE": "app-host"},
	} {
		// The order of rows does not matter.
		for _, order := range [][]store.AppSecret{rows, {rows[4], rows[2], rows[0], rows[3], rows[1]}} {
			got, err := c.InstanceValues(order, instance)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(want) {
				t.Fatalf("%s: %v, want %v", instance, got, want)
			}
			for k, v := range want {
				if got[k] != v {
					t.Fatalf("%s: %s = %q, want %q", instance, k, got[k], v)
				}
			}
		}
	}
	got, err := c.InstanceValues(nil, "moimroom")
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("no rows: %v, %v; want an empty map", got, err)
	}
	if _, err := c.InstanceValues(rows, ""); err == nil {
		t.Fatal("no instance: no error")
	}
	// A row that does not decrypt fails the whole set.
	bad := rows[1]
	bad.Ciphertext = bytes.Clone(bad.Ciphertext)
	bad.Ciphertext[0] ^= 1
	if _, err := c.InstanceValues([]store.AppSecret{rows[0], bad}, "moimroom"); err == nil {
		t.Fatal("a tampered row decrypted")
	}
	if v, err := c.OpenRow(rows[3]); err != nil || v != "dev" {
		t.Fatalf("OpenRow = %q, %v", v, err)
	}

	// A row written with another key says so, and shows only key checks.
	other, _ := NewCipher(bytes.Repeat([]byte{8}, KeySize))
	if other.KeyCheck() == c.KeyCheck() {
		t.Fatal("two keys share a key check")
	}
	_, err = other.OpenRow(rows[3])
	if !errors.Is(err, ErrKeyMismatch) || !strings.Contains(err.Error(), c.KeyCheck()) || strings.Contains(err.Error(), "dev\"") {
		t.Fatalf("other key: %v", err)
	}
	if _, err := other.InstanceValues(rows, "moimroom"); !errors.Is(err, ErrKeyMismatch) {
		t.Fatalf("InstanceValues with another key: %v", err)
	}
}

type fakeLister struct {
	apps []store.App
	rows map[string][]store.AppSecret
}

func (f fakeLister) ListApps(context.Context) ([]store.App, error) { return f.apps, nil }
func (f fakeLister) ListAppSecrets(_ context.Context, app string) ([]store.AppSecret, error) {
	return f.rows[app], nil
}

func TestOtherKeyRows(t *testing.T) {
	c := newCipher(t)
	other, _ := NewCipher(bytes.Repeat([]byte{8}, KeySize))
	mine, _ := c.Row("shop", "", "A", "1", "op")
	theirs, _ := other.Row("shop", "", "B", "2", "op")
	theirs2, _ := other.Row("moimroom", "moimroom", "C", "3", "op")
	l := fakeLister{apps: []store.App{{Name: "shop"}, {Name: "moimroom"}, {Name: "empty"}},
		rows: map[string][]store.AppSecret{"shop": {mine, theirs}, "moimroom": {theirs2}}}
	if n, err := c.OtherKeyRows(t.Context(), l); err != nil || n != 2 {
		t.Fatalf("OtherKeyRows = %d, %v; want 2", n, err)
	}
	if n, _ := other.OtherKeyRows(t.Context(), l); n != 1 {
		t.Fatalf("OtherKeyRows with the other key = %d; want 1", n)
	}
}

func TestChecksum(t *testing.T) {
	c := newCipher(t)
	sum := func(set map[string]string) string { return c.Checksum("moimroom", "moimroom--dev", set) }
	a := sum(map[string]string{"HOST_CODE": "4821", "ADMIN_TOKEN": "t"})
	if len(a) != 64 || strings.ToLower(a) != a {
		t.Fatalf("checksum %q is not lowercase hex SHA-256", a)
	}
	// Same set, same checksum, whatever the map order.
	for range 20 {
		if b := sum(map[string]string{"ADMIN_TOKEN": "t", "HOST_CODE": "4821"}); b != a {
			t.Fatalf("checksum changed: %s, %s", a, b)
		}
	}
	// Another instance or app with the same values: another checksum.
	if c.Checksum("moimroom", "moimroom", map[string]string{"HOST_CODE": "4821", "ADMIN_TOKEN": "t"}) == a ||
		c.Checksum("shop", "moimroom--dev", map[string]string{"HOST_CODE": "4821", "ADMIN_TOKEN": "t"}) == a {
		t.Fatal("the checksum does not depend on the app and the instance")
	}
	seen := map[string]string{a: "base"}
	for name, set := range map[string]map[string]string{
		"other value":   {"HOST_CODE": "4822", "ADMIN_TOKEN": "t"},
		"other name":    {"HOST_CODEX": "4821", "ADMIN_TOKEN": "t"},
		"one less":      {"HOST_CODE": "4821"},
		"empty":         {},
		"empty value":   {"HOST_CODE": ""},
		"joined values": {"A": "1\nB=2"},
		"split values":  {"A": "1", "B": "2"},
		"moved prefix":  {"AB": "", "C": "x"},
		"moved suffix":  {"A": "B", "C": "x"},
	} {
		got := sum(set)
		if prev, ok := seen[got]; ok {
			t.Errorf("%s has the checksum of %s", name, prev)
		}
		seen[got] = name
	}
	// Another key, another checksum: the checksum does not reveal the values.
	other, _ := NewCipher(bytes.Repeat([]byte{8}, KeySize))
	if other.Checksum("moimroom", "moimroom--dev", map[string]string{"HOST_CODE": "4821", "ADMIN_TOKEN": "t"}) == a {
		t.Fatal("two keys give the same checksum")
	}
}
