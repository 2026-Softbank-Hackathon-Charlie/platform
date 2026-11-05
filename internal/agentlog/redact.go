package agentlog

import (
	"regexp"
	"strings"
)

// Redacted replaces every value that looks like a credential.
const Redacted = "[redacted]"

// SecretKey matches object keys and names whose string values are stored as
// Redacted. "pass" counts only at the end of a key, so "passive" does not.
var SecretKey = regexp.MustCompile(`(?i)(passw(or)?d|passwd|pwd|pass$|secret|token|authorization|cookie|api[_-]?key|private[_-]?key|credential|access[_-]?key)`)

// secretWords are SecretKey's words for names found inside strings, where
// "pass" at the end of a name is matched on its own.
const secretWords = `passw(?:or)?d|pwd|secret|token|authorization|cookie|api[_-]?key|private[_-]?key|credential|access[_-]?key`

// String patterns, applied to every string that is kept.
var (
	// A PEM private key, to its END line or, when cut off, to the end.
	pemKey = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?(?:-----END [A-Z0-9 ]*PRIVATE KEY-----|$)`)
	// The password of URL userinfo: scheme://user:password@host.
	urlPassword = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://[^\s:/@]*):([^\s/@]*)@`)
	// An HTTP bearer token.
	bearer = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9\-._~+/]+=*`)
	// NAME=value and NAME: value where NAME looks like a credential, as in
	// environment variables, libpq conninfo (password=...), query strings,
	// YAML and HTTP headers. The value is a ${VAR} reference, a quoted
	// string, an auth scheme with its token, or a run of characters up to a
	// space or separator.
	assignment = regexp.MustCompile(`(?i)([A-Za-z0-9_.\-]*(?:` + secretWords + `)[A-Za-z0-9_.\-]*|[A-Za-z0-9_.\-]*pass)` +
		`(["']?[ \t]*[:=][ \t]*)` +
		`(\[redacted\]|\$\{\w+\}|"(?:[^"\\]|\\.)*"|'[^']*'|(?:basic|bearer|digest|token)[ \t]+[^\s"'&;,]+|[^\s"'&;,}\]]+)`)
	// An AWS access key id; temporary (ASIA) ones too.
	awsKeyID = regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)
	// A GitHub token: OAuth, personal (classic), installation, user to
	// server and refresh tokens, and fine-grained personal access tokens.
	githubToken = regexp.MustCompile(`\bgh[opsur]_[A-Za-z0-9]{20,}\b|\bgithub_pat_[A-Za-z0-9_]{20,}`)
	// ${NAME}: a reference to a variable, not a value. Plans name their
	// database passwords this way, and the reference is worth keeping.
	placeholder = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)
	digits      = regexp.MustCompile(`^[0-9]+$`)
)

// Redact returns v, a value decoded from JSON, with credentials replaced by
// Redacted, at any depth. It does not modify v.
//
//   - Every string under a key that matches SecretKey, at any depth below it.
//     Numbers, booleans and null stay, so counts such as input_tokens do.
//   - The "value" of an object whose "name" or "key" matches SecretKey, as in
//     {"name": "DB_PASSWORD", "value": "..."} (Kubernetes, ECS, AWS tags).
//   - Within every other string: NAME=value and NAME: value assignments,
//     URL passwords, bearer tokens, AWS access key ids, GitHub tokens and
//     PEM private keys.
func Redact(v any) any { return redact(v, false) }

func redact(v any, secret bool) any {
	switch x := v.(type) {
	case string:
		if secret {
			return Redacted
		}
		return RedactString(x)
	case map[string]any:
		pair := false
		for k, val := range x {
			if s, ok := val.(string); ok && (strings.EqualFold(k, "name") || strings.EqualFold(k, "key")) && SecretKey.MatchString(s) {
				pair = true
			}
		}
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = redact(val, secret || SecretKey.MatchString(k) || (pair && strings.EqualFold(k, "value")))
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = redact(val, secret)
		}
		return out
	}
	return v
}

// RedactString replaces the credentials found within s.
func RedactString(s string) string {
	s = pemKey.ReplaceAllLiteralString(s, Redacted)
	s = urlPassword.ReplaceAllStringFunc(s, func(m string) string {
		g := urlPassword.FindStringSubmatch(m)
		if placeholder.MatchString(g[2]) {
			return m
		}
		return g[1] + ":" + Redacted + "@"
	})
	s = bearer.ReplaceAllString(s, "${1}"+Redacted)
	s = assignment.ReplaceAllStringFunc(s, func(m string) string {
		g := assignment.FindStringSubmatch(m)
		name, sep, val := g[1], g[2], g[3]
		switch {
		case placeholder.MatchString(strings.Trim(val, `"'`)):
			return m
		case strings.HasSuffix(strings.ToLower(name), "tokens") && digits.MatchString(val):
			return m // max_tokens=1024 is a count, not a credential
		case strings.HasPrefix(val, `"`):
			val = `"` + Redacted + `"`
		case strings.HasPrefix(val, `'`):
			val = `'` + Redacted + `'`
		default:
			val = Redacted
		}
		return name + sep + val
	})
	s = githubToken.ReplaceAllLiteralString(s, Redacted)
	return awsKeyID.ReplaceAllLiteralString(s, Redacted)
}
