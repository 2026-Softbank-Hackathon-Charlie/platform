package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// AIPlacement is optional operator-declared model-host metadata. It does not
// attest hardware and never controls routing or backend execution.
type AIPlacement struct {
	ServiceID   string `json:"serviceId"`
	Kind        string `json:"kind"`
	Environment string `json:"environment,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Region      string `json:"region,omitempty"`
	Accelerator string `json:"accelerator,omitempty"`
}

var aiMetadataName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
var aiMetadataHeaders = []string{"X-Hopcharlie-AI-Service-Id", "X-Hopcharlie-AI-Kind", "X-Hopcharlie-AI-Environment", "X-Hopcharlie-AI-Provider", "X-Hopcharlie-AI-Region", "X-Hopcharlie-AI-Accelerator", "X-Hopcharlie-AI-Evidence"}

func ParseAIPlacements(raw string) (map[string]AIPlacement, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if len(raw) > 4096 {
		return nil, errors.New("AI placement metadata too large")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	d.DisallowUnknownFields()
	var out map[string]AIPlacement
	if d.Decode(&out) != nil || d.Decode(&struct{}{}) != io.EOF || len(out) > 2 {
		return nil, errors.New("invalid AI placement metadata")
	}
	for key, p := range out {
		if !aiMetadataName.MatchString(key) || !validAIPlacement(p) {
			return nil, errors.New("invalid AI placement metadata")
		}
	}
	return out, nil
}
func validAIPlacement(p AIPlacement) bool {
	if !aiMetadataName.MatchString(p.ServiceID) {
		return false
	}
	for _, s := range []string{p.Environment, p.Region} {
		if s != "" && !aiMetadataName.MatchString(s) {
			return false
		}
	}
	switch p.Kind {
	case "self_hosted":
		return p.Environment != "" && p.Provider == "" && (p.Accelerator == "gpu" || p.Accelerator == "cpu" || p.Accelerator == "unknown" || p.Accelerator == "")
	case "managed":
		return p.Environment == "" && (p.Provider == "aws.bedrock" || p.Provider == "gcp.vertex_ai" || p.Provider == "azure.ai.openai") && (p.Accelerator == "" || p.Accelerator == "unknown")
	case "external":
		return p.Environment == "" && p.Region == "" && (p.Provider == "anthropic" || p.Provider == "openai" || p.Provider == "cohere" || p.Provider == "groq") && (p.Accelerator == "" || p.Accelerator == "unknown")
	}
	return false
}
func stripAIMetadata(h http.Header) {
	for _, name := range aiMetadataHeaders {
		h.Del(name)
	}
}

// net/http populates response trailers while reading the body, after
// ModifyResponse. Scrub again before ReverseProxy copies late trailers.
type aiMetadataBody struct {
	io.ReadCloser
	response *http.Response
}

func (b *aiMetadataBody) scrub() {
	stripAIMetadata(b.response.Trailer)
	b.response.Trailer.Del("X-Hop-Backend")
}
func (b *aiMetadataBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.scrub()
	}
	return n, err
}
func (b *aiMetadataBody) Close() error {
	err := b.ReadCloser.Close()
	b.scrub()
	return err
}

func (rt *router) setAIMetadata(h http.Header, backend string) {
	stripAIMetadata(h)
	h.Set("X-Hopcharlie-AI-Service-Id", "ai-gateway-"+backend)
	h.Set("X-Hopcharlie-AI-Kind", "unknown")
	h.Set("X-Hopcharlie-AI-Accelerator", "unknown")
	h.Set("X-Hopcharlie-AI-Evidence", "gateway-route")
	if p, ok := rt.g.cfg.AIDemo.Placements[backend]; ok {
		h.Set("X-Hopcharlie-AI-Service-Id", p.ServiceID)
		h.Set("X-Hopcharlie-AI-Kind", p.Kind)
		for k, v := range map[string]string{"Environment": p.Environment, "Provider": p.Provider, "Region": p.Region, "Accelerator": p.Accelerator} {
			if v != "" {
				h.Set("X-Hopcharlie-AI-"+k, v)
			}
		}
		h.Set("X-Hopcharlie-AI-Evidence", "operator-declared-placement")
	}
}
