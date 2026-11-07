// Package recommend builds bounded evidence for the existing sandbox AI. It
// never migrates an app or treats an AI conclusion as preflight authorization.
package recommend

import "time"

const TTL = 15 * time.Minute
const AnalysisTimeout = 5 * time.Minute
const MaxInputBytes = 64 << 10
const MaxCandidates = 16
const MaxApps = 8

type Source struct {
	Title      string     `json:"title"`
	URL        string     `json:"url"`
	ObservedAt *time.Time `json:"observedAt"`
}

type Item struct {
	ID                         string   `json:"id"`
	App                        string   `json:"app"`
	Source                     *string  `json:"source"`
	Target                     *string  `json:"target"`
	Action                     string   `json:"action,omitempty"`
	Title                      string   `json:"title"`
	Reasons                    []string `json:"reasons"`
	Missing                    []string `json:"missing"`
	Sources                    []Source `json:"sources"`
	EstimatedMonthlySavingsUSD *float64 `json:"estimatedMonthlySavingsUsd"`
}

type Candidate struct {
	Item
	AllowedActions []string `json:"allowedActions"`
}

type Input struct {
	SnapshotID  string      `json:"snapshotId"`
	GeneratedAt time.Time   `json:"generatedAt"`
	ExpiresAt   time.Time   `json:"expiresAt"`
	Candidates  []Candidate `json:"candidates"`
}

type AI struct {
	Status    string  `json:"status"`
	Model     *string `json:"model"`
	SessionID *string `json:"sessionId"`
}

type Report struct {
	Status          string     `json:"status"`
	GeneratedAt     time.Time  `json:"generatedAt"`
	NextRefreshAt   time.Time  `json:"nextRefreshAt"`
	NextCheckAt     *time.Time `json:"nextCheckAt"`
	TTLSeconds      int        `json:"ttlSeconds"`
	CacheStatus     string     `json:"cacheStatus"`
	Recommendations []Item     `json:"recommendations"`
	AI              AI         `json:"ai"`
}

type Selection struct {
	ID      string   `json:"id"`
	Action  string   `json:"action"`
	Title   string   `json:"title,omitempty"`
	Reasons []string `json:"reasons"`
}

type Result struct {
	Nonce           string      `json:"nonce"`
	SnapshotID      string      `json:"snapshotId"`
	Status          string      `json:"status"`
	Model           string      `json:"model,omitempty"`
	SessionID       string      `json:"sessionId,omitempty"`
	Recommendations []Selection `json:"recommendations,omitempty"`
	ErrorCode       string      `json:"errorCode,omitempty"`
}
