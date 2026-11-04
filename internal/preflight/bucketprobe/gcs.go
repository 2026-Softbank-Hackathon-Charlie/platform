package bucketprobe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const gcsReadOnly = "https://www.googleapis.com/auth/devstorage.read_only"

// gcsStore reads bucket metadata with the GCS JSON API. GCS has no bucket
// replication into other buckets (dual-regions are reported as such) and
// lifecycle rules only change storage classes in place.
type gcsStore struct {
	r    remote
	hc   *http.Client
	base string
}

func newGCS(ctx context.Context, r remote, hc *http.Client, base string) (*gcsStore, string) {
	if base == "" {
		base = "https://" + gcsHost
	}
	var creds *google.Credentials
	var err error
	switch {
	case r.saCredentials != "":
		creds, err = google.CredentialsFromJSON(ctx, []byte(r.saCredentials), gcsReadOnly)
	case r.saFile != "":
		var raw []byte
		if raw, err = os.ReadFile(r.saFile); err == nil {
			creds, err = google.CredentialsFromJSON(ctx, raw, gcsReadOnly)
		}
	case r.envAuth:
		creds, err = google.FindDefaultCredentials(ctx, gcsReadOnly)
	}
	if err != nil {
		return nil, CodeCredentials
	}
	if creds != nil {
		ctx = context.WithValue(ctx, oauth2.HTTPClient, hc)
		hc = oauth2.NewClient(ctx, creds.TokenSource)
	}
	return &gcsStore{r: r, hc: hc, base: strings.TrimSuffix(base, "/")}, ""
}

func (s *gcsStore) endpointHost() string { return gcsHost }

func (s *gcsStore) get(ctx context.Context, path string, q url.Values, v any) error {
	u := s.base + "/storage/v1/b/" + url.PathEscape(s.r.bucket) + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	res, err := s.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return errors.New("gcs: unexpected status")
	}
	return json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(v)
}

func (s *gcsStore) location(ctx context.Context) (string, string, error) {
	var b struct {
		Location     string `json:"location"`
		LocationType string `json:"locationType"`
	}
	if err := s.get(ctx, "", url.Values{"fields": {"location,locationType"}}, &b); err != nil {
		return "", "", err
	}
	t := strings.ToLower(b.LocationType)
	if t != "region" && t != "dual-region" && t != "multi-region" {
		return "", "", errors.New("gcs: unknown location type")
	}
	return strings.ToLower(b.Location), t, nil
}

func (s *gcsStore) prefixes(ctx context.Context, path string) ([]string, string) {
	base := ""
	if path != "" {
		base = path + "/"
	}
	out := []string{}
	q := url.Values{"delimiter": {"/"}, "fields": {"prefixes,items(name),nextPageToken"}, "maxResults": {"1000"}}
	if base != "" {
		q.Set("prefix", base)
	}
	for page := 0; ; page++ {
		if page >= maxListPages {
			return nil, CodeTooManyPrefixes
		}
		var res struct {
			Prefixes      []string          `json:"prefixes"`
			Items         []json.RawMessage `json:"items"`
			NextPageToken string            `json:"nextPageToken"`
		}
		if err := s.get(ctx, "/o", q, &res); err != nil {
			return nil, CodeListFailed
		}
		var code string
		for _, p := range res.Prefixes {
			if out, code = addPrefix(out, strings.TrimPrefix(p, base)); code != "" {
				return nil, code
			}
		}
		if len(res.Items) > 0 {
			if out, code = addPrefix(out, ""); code != "" {
				return nil, code
			}
		}
		if res.NextPageToken == "" {
			break
		}
		q.Set("pageToken", res.NextPageToken)
	}
	slices.Sort(out)
	return out, ""
}

func (s *gcsStore) replicas(context.Context) ([]string, error) { return []string{}, nil }

func (s *gcsStore) tiered(context.Context) (bool, error) { return false, nil }

func (s *gcsStore) sample(ctx context.Context, prefix string, n int) ([]object, bool, string) {
	var res struct {
		Items []struct {
			Name        string            `json:"name"`
			Size        string            `json:"size"`
			ContentType string            `json:"contentType"`
			Metadata    map[string]string `json:"metadata"`
		} `json:"items"`
		NextPageToken string `json:"nextPageToken"`
	}
	q := url.Values{"prefix": {prefix}, "maxResults": {strconv.Itoa(n)}, "fields": {"items(name,size,contentType,metadata),nextPageToken"}}
	if err := s.get(ctx, "/o", q, &res); err != nil {
		return nil, false, CodeListFailed
	}
	out := make([]object, 0, len(res.Items))
	for _, it := range res.Items {
		size, _ := strconv.ParseInt(it.Size, 10, 64)
		o := object{key: it.Name, size: size, contentType: it.ContentType}
		for k := range it.Metadata {
			o.metadataKeys = append(o.metadataKeys, k)
		}
		slices.Sort(o.metadataKeys)
		out = append(out, o)
	}
	return out, res.NextPageToken != "", ""
}
