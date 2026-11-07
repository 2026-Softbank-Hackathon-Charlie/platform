package gateway

import (
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/llmquality"
)

// LoadAIQualityPolicy accepts only an operator-owned local file. It carries no
// credentials, upstream addresses, prompts, outputs or arbitrary execution code.
func LoadAIQualityPolicy(path string) (llmquality.Policy, error) {
	var p llmquality.Policy
	f, err := os.Open(path)
	if err != nil {
		return p, errors.New("AI quality policy cannot be read")
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 64<<10))
	d.DisallowUnknownFields()
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF {
		return p, errors.New("invalid AI quality policy JSON")
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	return p, nil
}
