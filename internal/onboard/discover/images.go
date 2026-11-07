package discover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// ImageRepo is the CNPG operand image repository.
const ImageRepo = "ghcr.io/cloudnative-pg/postgresql"

// Images resolves an image tag of ImageRepo to its digest. A tag that does
// not exist is ErrNoImage.
type Images interface {
	Digest(ctx context.Context, tag string) (string, error)
}

// ErrNoImage means the registry has no such tag.
var ErrNoImage = errors.New("no such image tag")

// GHCR resolves tags with ghcr.io's anonymous pull token.
type GHCR struct {
	HTTP *http.Client
	URL  string // default https://ghcr.io
}

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// Digest implements Images: the manifest list's (index's) digest, so every
// architecture gets the same reference.
func (g GHCR) Digest(ctx context.Context, tag string) (string, error) {
	c := g.HTTP
	if c == nil {
		c = &http.Client{Timeout: 15 * time.Second}
	}
	base := g.URL
	if base == "" {
		base = "https://ghcr.io"
	}
	name := strings.TrimPrefix(ImageRepo, "ghcr.io/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/token?service=ghcr.io&scope=repository:"+name+":pull", nil)
	if err != nil {
		return "", err
	}
	res, err := c.Do(req)
	if err != nil {
		return "", fmt.Errorf("ghcr: token: %w", err)
	}
	var tok struct {
		Token string `json:"token"`
	}
	err = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&tok)
	res.Body.Close()
	if err != nil || res.StatusCode != http.StatusOK || tok.Token == "" {
		return "", fmt.Errorf("ghcr: token: HTTP %d", res.StatusCode)
	}
	req, err = http.NewRequestWithContext(ctx, http.MethodHead, base+"/v2/"+name+"/manifests/"+tag, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+tok.Token)
	req.Header.Set("Accept", strings.Join([]string{"application/vnd.oci.image.index.v1+json",
		"application/vnd.docker.distribution.manifest.list.v2+json", "application/vnd.oci.image.manifest.v1+json",
		"application/vnd.docker.distribution.manifest.v2+json"}, ", "))
	res, err = c.Do(req)
	if err != nil {
		return "", fmt.Errorf("ghcr: %s: %w", tag, err)
	}
	res.Body.Close()
	switch {
	case res.StatusCode == http.StatusNotFound:
		return "", fmt.Errorf("%w: %s:%s", ErrNoImage, ImageRepo, tag)
	case res.StatusCode != http.StatusOK:
		return "", fmt.Errorf("ghcr: %s: HTTP %d", tag, res.StatusCode)
	}
	d := res.Header.Get("Docker-Content-Digest")
	if !digestRE.MatchString(d) {
		return "", fmt.Errorf("ghcr: %s: no digest in the answer", tag)
	}
	return d, nil
}
