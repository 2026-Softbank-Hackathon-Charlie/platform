package main

import (
	"os"
	"strings"
	"testing"
	"time"
)

// TestRenderRepoContract renders the repository's own contract. It does not
// require Korean text for every endpoint: a new endpoint shows up in English
// until apidoc.yaml catches up.
func TestRenderRepoContract(t *testing.T) {
	spec, err := os.ReadFile("../../" + specPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig("apidoc.yaml")
	if err != nil {
		t.Fatal(err)
	}
	tpl, err := os.ReadFile("template.md")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 2, 22, 45, 0, 0, kst)
	page, _, err := render(spec, cfg, string(tpl), meta{Commit: "c", Short: "c", Subject: "s", CommitTime: now, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(page, "{{") {
		t.Error("page still has a placeholder")
	}
	for _, id := range []string{"submitPlan", "startMigration", "getSecurity"} {
		if !strings.Contains(page, "operationId: `"+id+"`") {
			t.Errorf("page has no details for %s", id)
		}
	}
	if w := lint(page); len(w) > 0 {
		t.Errorf("Notion markdown warnings:\n%s", strings.Join(w, "\n"))
	}

	const limit = 20000
	chunks := split(page, limit)
	if got, want := strings.Join(chunks, "\n\n"), strings.TrimRight(page, "\n"); got != want {
		t.Error("chunks joined with a blank line differ from the page")
	}
	for i, c := range chunks {
		if len(c) > limit {
			t.Errorf("chunk %d is %d bytes, over %d", i+1, len(c), limit)
		}
		if strings.Count(c, "<toggle>") != strings.Count(c, "</toggle>") || strings.Count(c, "```")%2 != 0 {
			t.Errorf("chunk %d cuts a toggle or code block", i+1)
		}
	}
}

func TestUnknownPlaceholder(t *testing.T) {
	spec := []byte("openapi: 3.0.3\ninfo: {title: T, version: '1'}\npaths: {}\n")
	if _, _, err := render(spec, &config{}, "{{nope}}", meta{}); err == nil {
		t.Error("an unknown placeholder rendered without an error")
	}
}
