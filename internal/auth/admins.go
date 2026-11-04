package auth

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// HashCost is the bcrypt cost of HashPassword (hopcharlie-api -hash-password).
const HashCost = 12

// The bcrypt costs PLATFORM_ADMINS may use at startup. Below 10 a stolen hash
// is cheap to crack; above 14 every login, and every unknown name, costs
// seconds of CPU.
const (
	MinHashCost = 10
	MaxHashCost = 14
)

// maxPasswordBytes is as much of a password as bcrypt reads. A longer one
// would match every password that shares its first 72 bytes, so HashPassword
// refuses it and Check never accepts it.
const maxPasswordBytes = 72

type admin struct {
	name string
	hash []byte
}

// Admins are the administrators who log in to the console with a username
// and password (PLATFORM_ADMINS). Every administrator has role operator.
type Admins struct {
	byKey map[string]admin // by lowercased name
	// dummy is compared for unknown names, so they take as long as known ones.
	dummy []byte
}

// ParseAdmins reads "name:bcrypt-hash" entries separated by commas, e.g.
// "seohyun:$2a$12$...,minji:$2a$12$...". Names follow the token name rules;
// logins match them case-insensitively, so two names may not differ only in
// case. Every hash must have the same bcrypt cost, from minCost to
// MaxHashCost, so the dummy comparison for an unknown name takes as long as a
// real one. The server passes MinHashCost; tests pass bcrypt.MinCost to stay
// fast. Errors never contain a hash.
func ParseAdmins(spec string, minCost int) (*Admins, error) {
	a := &Admins{byKey: map[string]admin{}}
	cost := 0
	for i, item := range strings.Split(spec, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		// Entry numbers, not text: a malformed entry may be all hash.
		name, hash, ok := strings.Cut(item, ":")
		if !ok || hash == "" {
			return nil, fmt.Errorf("admin entry %d: want name:<bcrypt hash>", i+1)
		}
		if !ValidName(name) {
			return nil, fmt.Errorf("admin entry %d: name must use letters, digits, '.', '_' or '-'", i+1)
		}
		c, err := bcrypt.Cost([]byte(hash))
		if err != nil { // bcrypt's errors quote the hash's first byte
			return nil, fmt.Errorf("admin %q: the password hash is not a bcrypt hash; make one with hopcharlie-api -hash-password", name)
		}
		// Checked before the dummy hash is made, which costs 2^cost.
		if c < minCost || c > MaxHashCost {
			return nil, fmt.Errorf("admin %q: the password hash has bcrypt cost %d; use %d to %d (hopcharlie-api -hash-password makes %d)",
				name, c, minCost, MaxHashCost, HashCost)
		}
		if cost != 0 && c != cost {
			return nil, fmt.Errorf("admin %q: the password hash has bcrypt cost %d but the ones before it have %d; all must be the same so an unknown name takes as long as a known one",
				name, c, cost)
		}
		key := strings.ToLower(name)
		if _, dup := a.byKey[key]; dup {
			return nil, fmt.Errorf("admin %q appears twice (names are not case-sensitive)", name)
		}
		a.byKey[key] = admin{name: name, hash: []byte(hash)}
		cost = c
	}
	if len(a.byKey) > 0 {
		secret := make([]byte, 32)
		_, _ = rand.Read(secret)
		dummy, err := bcrypt.GenerateFromPassword(secret, cost)
		if err != nil {
			return nil, err
		}
		a.dummy = dummy
	}
	return a, nil
}

// Len is the number of administrators.
func (a *Admins) Len() int { return len(a.byKey) }

// Has reports whether name (in any case) is an administrator.
func (a *Admins) Has(name string) bool {
	_, ok := a.byKey[strings.ToLower(name)]
	return ok
}

// Check returns the administrator called name (in any case) when password is
// theirs. An unknown name costs one bcrypt comparison too, so the time an
// answer takes does not tell which names exist.
func (a *Admins) Check(name, password string) (Principal, bool) {
	ad, known := a.byKey[strings.ToLower(name)]
	hash := ad.hash
	if !known {
		hash = a.dummy
	}
	if hash == nil { // no administrators
		return Principal{}, false
	}
	err := bcrypt.CompareHashAndPassword(hash, []byte(password))
	if !known || err != nil || len(password) > maxPasswordBytes {
		return Principal{}, false
	}
	return Principal{Name: ad.name, Role: apiv1.RoleOperator}, true
}

// HashPassword returns the bcrypt hash of password at HashCost, for an entry
// of PLATFORM_ADMINS.
func HashPassword(password []byte) (string, error) {
	switch {
	case len(password) == 0:
		return "", errors.New("the password is empty")
	case len(password) > maxPasswordBytes:
		return "", fmt.Errorf("the password is longer than %d bytes", maxPasswordBytes)
	}
	h, err := bcrypt.GenerateFromPassword(password, HashCost)
	if err != nil {
		return "", err
	}
	return string(h), nil
}

// ValidName reports whether s may name a token or an administrator.
func ValidName(s string) bool { return namePattern.MatchString(s) }
