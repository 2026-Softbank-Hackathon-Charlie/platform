// Package auth maps bearer tokens to principals and decides what each role may do.
//
// Roles are ordered: viewer < agent < operator. An agent (the AI planner) may
// read and submit plans; only an operator may approve, start, switch or abort.
// Keeping the AI's token below the approval line is what makes "a human
// approves before anything runs" hold even if the agent misbehaves.
package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// Principal is an authenticated caller.
type Principal struct {
	Name string
	Role apiv1.Role
}

var rank = map[apiv1.Role]int{apiv1.RoleViewer: 1, apiv1.RoleAgent: 2, apiv1.RoleOperator: 3}

// Allows reports whether p may perform an action that needs role need.
func (p Principal) Allows(need apiv1.Role) bool { return rank[p.Role] >= rank[need] }

type entry struct {
	sum [sha256.Size]byte
	p   Principal
}

// Tokens authenticates requests against a fixed token list.
type Tokens struct{ entries []entry }

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]+$`)

// ParseTokens reads "name:role:token" entries separated by commas, e.g.
// "seohyun:operator:s3cr3t,planner:agent:t0k3n". Names end up in the audit log.
func ParseTokens(spec string) (*Tokens, error) {
	t := &Tokens{}
	seen := map[string]bool{}
	for _, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, rest, ok1 := strings.Cut(item, ":")
		role, token, ok2 := strings.Cut(rest, ":")
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("token entry %q: want name:role:token", redact(item))
		}
		if !namePattern.MatchString(name) {
			return nil, fmt.Errorf("token entry name %q: use letters, digits, '.', '_' or '-'", name)
		}
		r := apiv1.Role(role)
		if rank[r] == 0 {
			return nil, fmt.Errorf("token entry %q: role must be viewer, agent or operator", name)
		}
		if len(token) < 16 {
			return nil, fmt.Errorf("token entry %q: token must be at least 16 characters", name)
		}
		if seen[name] {
			return nil, fmt.Errorf("token entry %q appears twice", name)
		}
		seen[name] = true
		t.Add(name, r, token)
	}
	return t, nil
}

// Add registers one token.
func (t *Tokens) Add(name string, role apiv1.Role, token string) {
	t.entries = append(t.entries, entry{sum: sha256.Sum256([]byte(token)), p: Principal{Name: name, Role: role}})
}

// Len is the number of tokens.
func (t *Tokens) Len() int { return len(t.entries) }

// Authenticate checks the request's bearer token.
func (t *Tokens) Authenticate(r *http.Request) (Principal, bool) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || raw == "" {
		return Principal{}, false
	}
	sum := sha256.Sum256([]byte(raw))
	// Compare against every entry so timing does not reveal which one matched.
	var found Principal
	match := 0
	for _, e := range t.entries {
		if subtle.ConstantTimeCompare(sum[:], e.sum[:]) == 1 {
			found, match = e.p, 1
		}
	}
	return found, match == 1
}

func redact(item string) string {
	if i := strings.LastIndex(item, ":"); i >= 0 {
		return item[:i+1] + "***"
	}
	return "***"
}

type ctxKey struct{}

// WithPrincipal stores p in ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// FromContext returns the principal stored by WithPrincipal.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}
