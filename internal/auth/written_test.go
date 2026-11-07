package auth

import (
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWrittenTokenStreamIsolation(t *testing.T) {
	token := strings.Repeat("a", 32)
	tokens, err := ParseWrittenTokens("app-shop:" + token)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	if !tokens.Allows(req, "app-shop") || tokens.Allows(req, "app-other") {
		t.Fatal("token escaped stream boundary")
	}
	req.Header.Set("Authorization", "Bearer "+token+"wrong")
	if tokens.Allows(req, "app-shop") {
		t.Fatal("accepted wrong token")
	}
	for _, spec := range []string{"bad:short", "app-shop:" + token + ",app-shop:" + token} {
		if _, err := ParseWrittenTokens(spec); err == nil || strings.Contains(err.Error(), token) {
			t.Fatal("invalid or leaked token configuration")
		}
	}
}
