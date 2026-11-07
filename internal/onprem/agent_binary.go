package onprem

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
)

// AgentBinary exposes only the two binaries shipped with this platform release.
// Neither an enrollment nor an HTTP path can select an arbitrary local file.
func AgentBinary(directory, arch string) (string, string, error) {
	if arch != "amd64" && arch != "arm64" {
		return "", "", ErrInvalid
	}
	if directory == "" {
		directory = "/usr/local/lib/hopcharlie-onprem-agent"
	}
	path := filepath.Join(directory, arch)
	f, err := os.Open(path)
	if err != nil {
		return "", "", errors.New("onprem connector binary is not installed in this release")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 || info.Size() > 128<<20 {
		return "", "", errors.New("invalid onprem connector binary")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", "", errors.New("onprem connector binary could not be read")
	}
	return path, hex.EncodeToString(hash.Sum(nil)), nil
}
