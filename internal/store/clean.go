package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

// CleanText makes s storable in a PostgreSQL text column, which rejects NUL
// and invalid UTF-8: NUL characters are removed and each run of invalid bytes
// becomes U+FFFD.
func CleanText(s string) string {
	return strings.ReplaceAll(strings.ToValidUTF8(s, "�"), "\x00", "")
}

// Clean reports whether CleanText leaves s as it is. A lookup by a key that
// is not clean finds nothing: no stored key has NUL or invalid UTF-8.
func Clean(s string) bool { return CleanText(s) == s }

// CleanJSON makes a JSON value storable in a jsonb column, which rejects
// \u0000 and lone surrogates: every string and key is passed through
// CleanText (decoding already turns invalid UTF-8 and lone surrogates into
// U+FFFD). Numbers keep their exact digits. nil stays nil; anything that is
// not one JSON value is an error.
func CleanJSON(b []byte) ([]byte, error) {
	if b == nil {
		return nil, nil
	}
	if !json.Valid(b) {
		return nil, errors.New("not valid JSON")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(cleanValue(v)); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func cleanValue(v any) any {
	switch x := v.(type) {
	case string:
		return CleanText(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[CleanText(k)] = cleanValue(val)
		}
		return out
	case []any:
		for i := range x {
			x[i] = cleanValue(x[i])
		}
		return x
	}
	return v
}
