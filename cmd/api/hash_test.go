package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/auth"
)

// stdin returns a file holding content, like a pipe into -hash-password.
func stdin(t *testing.T, content string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestHashPasswordFlag(t *testing.T) {
	var out, prompt bytes.Buffer
	// A Windows pipe ends the line with CRLF; only the first line counts.
	if err := hashPassword(stdin(t, "correct horse \r\nsecond line\n"), &out, &prompt); err != nil {
		t.Fatal(err)
	}
	h := strings.TrimSuffix(out.String(), "\n")
	if cost, err := bcrypt.Cost([]byte(h)); err != nil || cost != auth.HashCost || strings.Contains(h, "\n") {
		t.Fatalf("output %q is not one bcrypt hash of cost %d", out.String(), auth.HashCost)
	}
	if err := bcrypt.CompareHashAndPassword([]byte(h), []byte("correct horse ")); err != nil {
		t.Fatalf("the hash is not of the first line: %v", err)
	}
	if prompt.Len() != 0 {
		t.Errorf("prompted %q without a terminal", prompt.String())
	}

	out.Reset()
	for _, in := range []string{"", "\n", strings.Repeat("x", 73) + "\n"} {
		if err := hashPassword(stdin(t, in), &out, &prompt); err == nil || out.Len() != 0 {
			t.Errorf("input of %d bytes: err %v, output %q", len(in), err, out.String())
		}
	}
}
