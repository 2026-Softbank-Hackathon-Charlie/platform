package onprem

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAgentBinaryIsRestrictedToShippedArchitectures(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "amd64"), []byte("connector fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"../amd64", "", "AMD64", "../../etc/passwd"} {
		if _, _, err := AgentBinary(d, arch); err == nil {
			t.Fatalf("accepted unsupported architecture %q", arch)
		}
	}
	if _, digest, err := AgentBinary(d, "amd64"); err != nil || len(digest) != 64 {
		t.Fatalf("binary/digest: %q %v", digest, err)
	}
	if _, _, err := AgentBinary(d, "arm64"); err == nil {
		t.Fatal("missing release binary was accepted")
	}
}
