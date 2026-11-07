package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/http"
	"regexp"
	"strings"
)

// WrittenTokens can append only to their named stream. They do not belong to
// the viewer/agent/operator hierarchy and cannot access other APIs.
type WrittenTokens struct{ entries map[string][32]byte }

var writtenStream = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func ParseWrittenTokens(spec string) (*WrittenTokens, error) {
	t := &WrittenTokens{entries: map[string][32]byte{}}
	for _, part := range strings.Split(spec, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		stream, token, ok := strings.Cut(part, ":")
		if !ok || !writtenStream.MatchString(stream) || len(token) < 32 {
			return nil, errors.New("written token entries require stream:token with valid stream and at least 32 token characters")
		}
		if _, ok := t.entries[stream]; ok {
			return nil, errors.New("duplicate written record token stream")
		}
		t.entries[stream] = sha256.Sum256([]byte(token))
	}
	return t, nil
}
func (t *WrittenTokens) Allows(r *http.Request, stream string) bool {
	if t == nil {
		return false
	}
	want, ok := t.entries[stream]
	if !ok {
		return false
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(want[:], sum[:]) == 1
}
