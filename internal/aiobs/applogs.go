package aiobs

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// This file summarises one app's logs for the console: how many lines, how many
// errors and warnings, in which service and when, and which kinds of error
// repeat. It never returns an ordinary log line. Error and warning lines are
// returned only as patterns: redacted (Redact) and with numbers and ids folded
// away, so that the same failure counts once.

// AppBucket is one five-minute slice.
type AppBucket struct {
	Time     time.Time `json:"time"`
	Total    int       `json:"total"`
	Errors   int       `json:"errors"`
	Warnings int       `json:"warnings"`
}

// AppService is one service (a container's OpenTelemetry service name) of the app.
type AppService struct {
	Name     string    `json:"name"`
	Total    int       `json:"total"`
	Errors   int       `json:"errors"`
	Warnings int       `json:"warnings"`
	LastSeen time.Time `json:"lastSeen"`
}

// AppPattern is a repeated error or warning, as text with no secrets in it.
type AppPattern struct {
	Severity string    `json:"severity"` // error or warning
	Service  string    `json:"service"`
	Text     string    `json:"text"`
	Count    int       `json:"count"`
	LastSeen time.Time `json:"lastSeen"`
}

// AppObservation is what the logs show about one app since a time.
type AppObservation struct {
	Since    time.Time    `json:"since"`
	Total    int          `json:"total"`
	Errors   int          `json:"errors"`
	Warnings int          `json:"warnings"`
	Buckets  []AppBucket  `json:"buckets"`
	Services []AppService `json:"services"`
	Patterns []AppPattern `json:"patterns"`
}

const (
	errorSeverities   = "('error','fatal','critical','alert','emerg')"
	warningSeverities = "('warn','warning')"
	// How many error and warning lines are read to build the patterns, and how many patterns are kept.
	patternLines = 500
	patternKeep  = 10
	patternLen   = 160
)

// appFilter selects the app's services: the one named like the app and those
// named <app>-<anything> (docker-doom-web, moimroom-mig-db).
const appFilter = "(ServiceName = {app:String} OR startsWith(ServiceName, {prefix:String}))" +
	" AND Timestamp >= fromUnixTimestamp64Milli({since:Int64})"

func appParams(app string, since time.Time) url.Values {
	v := url.Values{}
	v.Set("param_app", app)
	v.Set("param_prefix", app+"-")
	v.Set("param_since", strconv.FormatInt(since.UnixMilli(), 10))
	return v
}

var appName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// App summarises the app's logs since the given time. The app name must be a
// registered app's: the caller checks that, and the name is matched against
// service names as a prefix, so only a valid DNS-label-like name is accepted.
func (c *Client) App(ctx context.Context, app string, since time.Time) (AppObservation, error) {
	if !appName.MatchString(app) {
		return AppObservation{}, fmt.Errorf("aiobs: %q is not an app name", app)
	}
	out := AppObservation{Since: since.UTC(), Buckets: []AppBucket{}, Services: []AppService{}, Patterns: []AppPattern{}}
	counts := "toString(count()) AS total, toString(countIf(lower(SeverityText) IN " + errorSeverities + ")) AS errors," +
		" toString(countIf(lower(SeverityText) IN " + warningSeverities + ")) AS warnings"

	// per service
	body, err := c.do(ctx, "SELECT ServiceName AS name, "+counts+", toString(toUnixTimestamp64Milli(max(Timestamp))) AS last FROM "+c.Table+
		" WHERE "+appFilter+" GROUP BY ServiceName ORDER BY count() DESC LIMIT 20 FORMAT JSONEachRow", appParams(app, since))
	if err != nil {
		return out, err
	}
	var rows []struct {
		Name, Total, Errors, Warnings, Last string
	}
	if err := eachRow(body, func(line []byte) error {
		var r struct{ Name, Total, Errors, Warnings, Last string }
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		rows = append(rows, r)
		return nil
	}); err != nil {
		return out, err
	}
	for _, r := range rows {
		x := AppService{Name: r.Name, Total: atoi(r.Total), Errors: atoi(r.Errors), Warnings: atoi(r.Warnings), LastSeen: millis(r.Last)}
		out.Services = append(out.Services, x)
		out.Total += x.Total
		out.Errors += x.Errors
		out.Warnings += x.Warnings
	}

	// per five minutes
	body, err = c.do(ctx, "SELECT toString(toUnixTimestamp(toStartOfInterval(Timestamp, INTERVAL 5 MINUTE))) AS t, "+counts+" FROM "+c.Table+
		" WHERE "+appFilter+" GROUP BY t ORDER BY t FORMAT JSONEachRow", appParams(app, since))
	if err != nil {
		return out, err
	}
	if err := eachRow(body, func(line []byte) error {
		var r struct{ T, Total, Errors, Warnings string }
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		secs, err := strconv.ParseInt(r.T, 10, 64)
		if err != nil {
			return fmt.Errorf("aiobs: bad bucket time %q", r.T)
		}
		out.Buckets = append(out.Buckets, AppBucket{Time: time.Unix(secs, 0).UTC(), Total: atoi(r.Total), Errors: atoi(r.Errors), Warnings: atoi(r.Warnings)})
		return nil
	}); err != nil {
		return out, err
	}

	// the repeated errors and warnings
	if out.Errors+out.Warnings == 0 {
		return out, nil
	}
	v := appParams(app, since)
	v.Set("param_limit", strconv.Itoa(patternLines))
	body, err = c.do(ctx, "SELECT toString(toUnixTimestamp64Milli(Timestamp)) AS ts, ServiceName AS svc, lower(SeverityText) AS sev, substring(Body, 1, 600) AS body FROM "+c.Table+
		" WHERE "+appFilter+" AND lower(SeverityText) IN "+errorSeverities[:len(errorSeverities)-1]+","+warningSeverities[1:]+
		" ORDER BY Timestamp DESC LIMIT {limit:UInt32} FORMAT JSONEachRow", v)
	if err != nil {
		return out, err
	}
	type key struct{ sev, svc, text string }
	seen := map[key]*AppPattern{}
	var order []key
	if err := eachRow(body, func(line []byte) error {
		var r struct{ TS, Svc, Sev, Body string }
		if err := json.Unmarshal(line, &r); err != nil {
			return err
		}
		sev := "warning"
		if strings.Contains(errorSeverities, "'"+r.Sev+"'") {
			sev = "error"
		}
		k := key{sev, r.Svc, Pattern(r.Body)}
		if k.text == "" {
			return nil
		}
		if p, ok := seen[k]; ok {
			p.Count++
			return nil
		}
		seen[k] = &AppPattern{Severity: sev, Service: r.Svc, Text: k.text, Count: 1, LastSeen: millis(r.TS)}
		order = append(order, k)
		return nil
	}); err != nil {
		return out, err
	}
	for _, k := range order {
		out.Patterns = append(out.Patterns, *seen[k])
	}
	sort.SliceStable(out.Patterns, func(i, j int) bool {
		a, b := out.Patterns[i], out.Patterns[j]
		if a.Severity != b.Severity {
			return a.Severity == "error"
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.LastSeen.After(b.LastSeen)
	})
	if len(out.Patterns) > patternKeep {
		out.Patterns = out.Patterns[:patternKeep]
	}
	return out, nil
}

func eachRow(body []byte, f func(line []byte) error) error {
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if err := f([]byte(line)); err != nil {
			return fmt.Errorf("aiobs: bad row: %w", err)
		}
	}
	return nil
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func millis(s string) time.Time {
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

var (
	reCredURL  = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^\s/@:]+:[^\s/@]+@`)
	reBearer   = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)
	reKeyValue = regexp.MustCompile(`(?i)\b([a-z0-9_.-]*(?:password|passwd|pwd|secret|token|api[_-]?key|apikey|authorization|cookie|credential|private[_-]?key)[a-z0-9_.-]*)(["']?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;&}]+)`)
	reAWSKey   = regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|ANPA)[A-Z0-9]{16}\b`)
	reEmail    = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	reLongTok  = regexp.MustCompile(`[A-Za-z0-9_\-+/=]{24,}`)
	reUUID     = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reHex      = regexp.MustCompile(`(?i)\b[0-9a-f]{12,}\b`)
	reNumber   = regexp.MustCompile(`\d+`)
	reSpaces   = regexp.MustCompile(`\s+`)
)

// Redact hides what looks like a secret or a person: the password of a URL,
// bearer and basic tokens, the value of a key whose name says password, token,
// secret, key or cookie, email addresses and any long token-like string. It
// errs on the side of hiding. It cannot know every secret; the console says so.
func Redact(s string) string {
	s = reCredURL.ReplaceAllString(s, "${1}«가림»@")
	s = reBearer.ReplaceAllString(s, "$1 «가림»")
	s = reKeyValue.ReplaceAllString(s, "${1}${2}«가림»")
	s = reAWSKey.ReplaceAllString(s, "«가림»")
	s = reEmail.ReplaceAllString(s, "«이메일»")
	s = reLongTok.ReplaceAllStringFunc(s, func(t string) string {
		// A long run of letters is a word or a path; one that mixes letters and digits, or is very long, is a key.
		if len(t) >= 32 || (strings.IndexFunc(t, unicode.IsDigit) >= 0 && strings.IndexFunc(t, unicode.IsLetter) >= 0) {
			return "«가림»"
		}
		return t
	})
	return s
}

// Pattern redacts a log line and folds away what differs between two
// occurrences of the same failure (ids, numbers), so that they count as one.
// The result is at most patternLen characters.
func Pattern(body string) string {
	s := Redact(body)
	s = reUUID.ReplaceAllString(s, "«id»")
	s = reHex.ReplaceAllString(s, "«id»")
	s = reNumber.ReplaceAllString(s, "#")
	s = strings.TrimSpace(reSpaces.ReplaceAllString(s, " "))
	if r := []rune(s); len(r) > patternLen {
		s = string(r[:patternLen]) + "…"
	}
	return s
}
