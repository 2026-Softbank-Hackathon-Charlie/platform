package plans

import (
	"encoding/json"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// Approvers must see whether written-record verification is part of this
// immutable plan, including plans whose input is an uploaded file.
func TestSummaryIncludesWrittenRecordsConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		configured  bool
	}{
		{"unconfigured", "", false},
		{"stream", `"verify":{"writtenRecords":"stream:demo"}`, true},
		{"file", `"verify":{"writtenRecords":"receipts.jsonl"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc := newService(DefaultPolicy())
			rec, err := svc.Submit(ctx, planJSON(tc.extra), "alice")
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(rec.Summary)
			if err != nil {
				t.Fatal(err)
			}
			var sum struct {
				Configured *bool `json:"writtenRecordsConfigured"`
			}
			if json.Unmarshal(body, &sum) != nil || sum.Configured == nil || *sum.Configured != tc.configured {
				t.Fatalf("written configuration absent or incorrect: %s", body)
			}
		})
	}
}

func TestHistoricalSummaryDoesNotInventWrittenConfiguration(t *testing.T) {
	rec, err := record(store.Plan{Summary: []byte(`{"planId":"historical"}`)})
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(rec.Summary)
	if err != nil {
		t.Fatal(err)
	}
	var sum map[string]any
	if json.Unmarshal(body, &sum) != nil {
		t.Fatalf("invalid summary: %s", body)
	}
	if _, present := sum["writtenRecordsConfigured"]; present {
		t.Fatalf("historical summary was guessed: %s", body)
	}
}
