// Package llmobs reads app-scoped Langfuse generations. It never returns prompts,
// outputs, status messages, arbitrary metadata or provider credentials.
package llmobs

import "time"

const SampleLimit = 100

type Source struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Version string `json:"version,omitempty"`
}

type Window struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}
type Sample struct {
	Limit     int  `json:"limit"`
	Returned  int  `json:"returned"`
	Truncated bool `json:"truncated"`
}

// Pointer metrics distinguish a reported zero from an unreported value.
type Metrics struct {
	InputTokens   *float64 `json:"inputTokens"`
	OutputTokens  *float64 `json:"outputTokens"`
	TotalTokens   *float64 `json:"totalTokens"`
	CostUSD       *float64 `json:"costUsd"`
	LatencyMillis *float64 `json:"latencyMillis"`
}
type Observation struct {
	ID        string    `json:"id"`
	TraceID   string    `json:"traceId"`
	StartTime time.Time `json:"startTime"`
	Model     string    `json:"model,omitempty"`
	Level     *string   `json:"level"`
	URL       string    `json:"url,omitempty"`
	Metrics
}
type Summary struct {
	Generations int  `json:"generations"`
	Traces      int  `json:"traces"`
	Errors      *int `json:"errors"`
	Metrics
}
type Report struct {
	App          string        `json:"app"`
	GeneratedAt  time.Time     `json:"generatedAt"`
	Window       Window        `json:"window"`
	Source       Source        `json:"source"`
	Sample       Sample        `json:"sample"`
	Summary      *Summary      `json:"summary"`
	Observations []Observation `json:"observations"`
}

func Empty(app string, from, to time.Time, status, reason string) Report {
	return Report{App: app, GeneratedAt: to, Window: Window{from, to}, Source: Source{Name: "langfuse", Status: status, Reason: reason}, Sample: Sample{Limit: SampleLimit}, Observations: []Observation{}}
}
