package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
)

func TestExampleResidencyPolicyLoads(t *testing.T) {
	envCfg, err := envs.LoadConfig(filepath.Join("..", "..", "examples", "envs.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range envCfg.Environments {
		names = append(names, e.Name)
	}
	g, err := residencyGate(context.Background(), filepath.Join("..", "..", "examples", "residency-policy.yaml"), names)
	if err != nil || len(g.PolicyDigest()) != 64 {
		t.Fatalf("gate %v err %v", g, err)
	}
}

func TestResidencyPolicyMustNameRegisteredEnvironments(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "residency-policy.yaml")
	if _, err := residencyGate(context.Background(), path, []string{"gke", "eks"}); err == nil || !strings.Contains(err.Error(), "onprem") {
		t.Fatalf("unregistered environment: %v", err)
	}
	if _, err := residencyGate(context.Background(), filepath.Join(t.TempDir(), "missing.yaml"), nil); err == nil {
		t.Fatal("missing file accepted")
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := residencyGate(context.Background(), bad, nil); err == nil {
		t.Fatal("invalid policy accepted")
	}
}
