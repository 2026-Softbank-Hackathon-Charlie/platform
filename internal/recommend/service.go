package recommend

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var ErrConflict = errors.New("recommendation snapshot or result conflict")

type Launch struct {
	ID, Nonce, Actor string
	Input            []byte
	CreatedAt        time.Time
}
type Starter interface {
	StartRecommendation(context.Context, Launch) error
}
type Loader func(context.Context) ([]Candidate, error)

// Service coalesces all viewers into one bounded assessment. Status performs
// no provider or Kubernetes work. This cache contains no prompt or credential.
// The runner also deduplicates Jobs across API restarts within the TTL window.
type Service struct {
	mu        sync.Mutex
	load      Loader
	runner    Starter
	now       func() time.Time
	report    Report
	input     Input
	id        string
	nonce     [32]byte
	accepted  [32]byte
	completed bool
}

func New(load Loader, runner Starter, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{load: load, runner: runner, now: now}
}

func emptyReport(now time.Time, status string) Report {
	return Report{Status: status, GeneratedAt: now.UTC(), NextRefreshAt: now.Add(TTL).UTC(), TTLSeconds: int(TTL.Seconds()), CacheStatus: "fresh", Recommendations: []Item{}, AI: AI{Status: status}}
}
func cloneReport(r Report) Report {
	b, _ := json.Marshal(r)
	var out Report
	_ = json.Unmarshal(b, &out)
	return out
}
func (s *Service) statusLocked() Report {
	now := s.now().UTC()
	if s.report.Status == "" {
		return emptyReport(now, "not_configured")
	}
	if s.report.Status == "generating" && !now.Before(s.input.ExpiresAt) {
		s.report.Status, s.report.AI.Status, s.report.NextCheckAt = "unavailable", "expired", nil
		s.nonce = [32]byte{}
	}
	out := cloneReport(s.report)
	out.CacheStatus = "cached"
	return out
}
func (s *Service) Status() Report { s.mu.Lock(); defer s.mu.Unlock(); return s.statusLocked() }

func (s *Service) Trigger(ctx context.Context, actor string) Report {
	s.mu.Lock()
	now := s.now().UTC()
	if s.report.Status != "" && now.Before(s.report.NextRefreshAt) {
		out := s.statusLocked()
		s.mu.Unlock()
		return out
	}
	if s.runner == nil || s.load == nil {
		s.report = emptyReport(now, "not_configured")
		out := cloneReport(s.report)
		s.mu.Unlock()
		return out
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		s.report = emptyReport(now, "unavailable")
		out := cloneReport(s.report)
		s.mu.Unlock()
		return out
	}
	id := "rec-" + hex.EncodeToString(random[:12])
	nonce := base64.RawURLEncoding.EncodeToString(random[:])
	s.id, s.nonce, s.completed, s.accepted = id, sha256.Sum256([]byte(nonce)), false, [32]byte{}
	s.input = Input{SnapshotID: id, GeneratedAt: now, ExpiresAt: now.Add(AnalysisTimeout), Candidates: []Candidate{}}
	s.report = emptyReport(now, "generating")
	check := now.Add(10 * time.Second)
	s.report.NextCheckAt = &check
	out := cloneReport(s.report)
	s.mu.Unlock()
	// Disconnecting the viewer does not create another assessment or cancel a
	// Job already accepted by Kubernetes. All work has its own finite deadline.
	go s.start(context.WithoutCancel(ctx), Launch{ID: id, Nonce: nonce, Actor: actor, CreatedAt: now})
	return out
}

func (s *Service) start(parent context.Context, launch Launch) {
	ctx, cancel := context.WithTimeout(parent, 25*time.Second)
	defer cancel()
	candidates, err := s.load(ctx)
	if err == nil {
		err = validateCandidates(candidates)
	}
	s.mu.Lock()
	if launch.ID != s.id || s.completed || s.report.Status != "generating" || !s.now().Before(s.input.ExpiresAt) {
		s.mu.Unlock()
		return
	}
	if err != nil {
		s.report.Status, s.report.AI.Status, s.report.NextCheckAt = "unavailable", "evidence_unavailable", nil
		s.mu.Unlock()
		return
	}
	s.input.Candidates = candidates
	launch.Input, err = json.Marshal(s.input)
	if err != nil || len(launch.Input) > MaxInputBytes {
		s.report.Status, s.report.AI.Status, s.report.NextCheckAt = "unavailable", "evidence_too_large", nil
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	if err := s.runner.StartRecommendation(ctx, launch); err != nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.id == launch.ID && !s.completed {
			s.report.Status, s.report.AI.Status, s.report.NextCheckAt = "unavailable", "runner_unavailable", nil
		}
	}
}

var label = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
var candidateLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,95}$`)
var resourceLabel = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
var modelLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,119}$`)

func validateCandidates(list []Candidate) error {
	if len(list) > MaxCandidates {
		return ErrConflict
	}
	seen, apps := map[string]bool{}, map[string]bool{}
	for _, c := range list {
		if !candidateLabel.MatchString(c.ID) || !resourceLabel.MatchString(c.App) || seen[c.ID] || c.Title == "" || utf8.RuneCountInString(c.Title) > 80 || len(c.Reasons) == 0 || len(c.Reasons) > 6 || len(c.Missing) > 12 || len(c.Sources) > 8 {
			return ErrConflict
		}
		for _, value := range []*string{c.Source, c.Target} {
			if value != nil && !resourceLabel.MatchString(*value) {
				return ErrConflict
			}
		}
		for _, reason := range c.Reasons {
			if strings.TrimSpace(reason) == "" || utf8.RuneCountInString(reason) > 120 || strings.ContainsAny(reason, "\r\n\x00") {
				return ErrConflict
			}
		}
		if len(c.AllowedActions) == 0 {
			return ErrConflict
		}
		for _, action := range c.AllowedActions {
			if !slices.Contains([]string{"stay", "insufficient", "move"}, action) {
				return ErrConflict
			}
		}
		if slices.Contains(c.AllowedActions, "move") && (c.Source == nil || c.Target == nil || *c.Source == *c.Target) {
			return ErrConflict
		}
		seen[c.ID], apps[c.App] = true, true
	}
	if len(apps) > MaxApps {
		return ErrConflict
	}
	return nil
}

func safeModel(value string) *string {
	lower := strings.ToLower(value)
	if !modelLabel.MatchString(value) || strings.Contains(value, "://") || strings.HasPrefix(lower, "sk-") || strings.HasPrefix(lower, "akia") {
		return nil
	}
	return &value
}

// Complete accepts only the active snapshot and one nonce-bound result.
// AI selects existing evidence: it cannot invent a target, source or saving.
func (s *Service) Complete(id string, result Result) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	digest := sha256.Sum256([]byte(result.Nonce))
	if id != s.id || result.SnapshotID != s.input.SnapshotID || subtle.ConstantTimeCompare(digest[:], s.nonce[:]) != 1 || !s.now().Before(s.input.ExpiresAt) {
		return ErrConflict
	}
	b, _ := json.Marshal(result)
	resultDigest := sha256.Sum256(b)
	if s.completed {
		if resultDigest == s.accepted {
			return nil
		}
		return ErrConflict
	}
	if s.report.Status != "generating" || (result.Status != "ready" && result.Status != "unavailable") {
		return ErrConflict
	}
	if result.Status == "unavailable" {
		if !slices.Contains([]string{"ai_unavailable", "invalid_input"}, result.ErrorCode) || len(result.Recommendations) != 0 {
			return ErrConflict
		}
		s.report.Status, s.report.AI.Status = "unavailable", result.ErrorCode
	} else {
		if result.Recommendations == nil || len(result.Recommendations) > 4 || safeModel(result.Model) == nil {
			return ErrConflict
		}
		known := map[string]Candidate{}
		for _, c := range s.input.Candidates {
			known[c.ID] = c
		}
		seen := map[string]bool{}
		items := []Item{}
		for _, selection := range result.Recommendations {
			c, ok := known[selection.ID]
			if !ok || seen[selection.ID] || !slices.Contains(c.AllowedActions, selection.Action) || len(selection.Reasons) == 0 || len(selection.Reasons) > 2 {
				return ErrConflict
			}
			for _, reason := range selection.Reasons {
				if !slices.Contains(c.Reasons, reason) {
					return ErrConflict
				}
			}
			item := c.Item
			item.Action, item.Reasons = selection.Action, slices.Clone(selection.Reasons)
			if selection.Action != "move" {
				item.EstimatedMonthlySavingsUSD = nil
			}
			items = append(items, item)
			seen[selection.ID] = true
		}
		s.report.Recommendations, s.report.Status, s.report.AI.Status = items, "ready", "completed"
		s.report.AI.Model = safeModel(result.Model)
		if label.MatchString(result.SessionID) {
			value := result.SessionID
			s.report.AI.SessionID = &value
		}
	}
	s.report.NextCheckAt = nil
	s.accepted, s.completed = resultDigest, true
	return nil
}
