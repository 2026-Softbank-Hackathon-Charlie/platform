package bucketprobe

import (
	"context"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/residency"
)

// MaxSample bounds the objects one Sample reads.
const MaxSample = 50

// Sample describes the objects under one first-level prefix without
// revealing them: object keys and metadata values never leave this package.
// Key names are only matched against fixed patterns, and only the counts are
// kept, so an agent can tell "kyc/ holds keys shaped like resident numbers"
// without seeing a single key.
type Sample struct {
	Prefix       string         `json:"prefix"`
	Objects      int            `json:"objects"`
	Truncated    bool           `json:"truncated"`
	ContentTypes map[string]int `json:"contentTypes"`
	Extensions   map[string]int `json:"extensions"`
	// MetadataKeys counts user metadata names (never values).
	MetadataKeys map[string]int `json:"metadataKeys"`
	// KeyPatterns counts keys matching each fixed pattern (KeyPatternNames).
	KeyPatterns map[string]int `json:"keyPatterns"`
	MinSize     int64          `json:"minSize"`
	MaxSize     int64          `json:"maxSize"`
	MaxDepth    int            `json:"maxDepth"`
}

// object is one listed object, internal to this package.
type object struct {
	key          string
	size         int64
	contentType  string
	metadataKeys []string
}

// keyPatterns are personal-data shapes looked for in object key names.
var keyPatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"email", regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)},
	{"kr_resident_number", regexp.MustCompile(`(^|[^0-9])[0-9]{2}(0[1-9]|1[0-2])(0[1-9]|[12][0-9]|3[01])[-_]?[1-4][0-9]{6}([^0-9]|$)`)},
	{"kr_phone", regexp.MustCompile(`(^|[^0-9])01[016789][-_.]?[0-9]{3,4}[-_.]?[0-9]{4}([^0-9]|$)`)},
	{"card_number", regexp.MustCompile(`(^|[^0-9])[0-9]{4}[-_ ]?[0-9]{4}[-_ ]?[0-9]{4}[-_ ]?[0-9]{4}([^0-9]|$)`)},
	{"date", regexp.MustCompile(`(^|[^0-9])(19|20)[0-9]{2}[-_]?(0[1-9]|1[0-2])[-_]?(0[1-9]|[12][0-9]|3[01])([^0-9]|$)`)},
	{"uuid", regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)},
	{"non_ascii", regexp.MustCompile(`[^\x00-\x7F]`)}, // e.g. a name in Hangul
}

// KeyPatternNames lists the patterns Sample counts.
func KeyPatternNames() []string {
	out := make([]string, len(keyPatterns))
	for i, p := range keyPatterns {
		out[i] = p.name
	}
	return out
}

var (
	tokenPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.+-]{0,63}$`)
	extPattern   = regexp.MustCompile(`^\.[a-z0-9]{1,10}$`)
)

// Sample reads up to n objects (at most MaxSample) under prefix, a
// first-level prefix of bucket. The objects read are the first in key order.
func (p Prober) Sample(ctx context.Context, bucket, prefix string, n int) (Sample, Result) {
	out := Sample{Prefix: prefix, ContentTypes: map[string]int{}, Extensions: map[string]int{}, MetadataKeys: map[string]int{}, KeyPatterns: map[string]int{}}
	if !residency.ValidPrefix(prefix) {
		return out, Result{Code: CodeUnsupportedPrefix}
	}
	if n <= 0 || n > MaxSample {
		n = MaxSample
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	fail := func(code string) (Sample, Result) {
		if ctx.Err() != nil {
			code = CodeTimeout
		}
		return Sample{Prefix: prefix, ContentTypes: map[string]int{}, Extensions: map[string]int{}, MetadataKeys: map[string]int{}, KeyPatterns: map[string]int{}}, Result{Code: code}
	}
	getenv := p.Getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	r, code := readRemote(getenv, bucket)
	if code != "" {
		return fail(code)
	}
	full := prefix
	if r.path != "" {
		full = r.path + "/" + prefix
	}
	hc := p.HTTPClient
	if hc == nil {
		hc = &http.Client{}
	}
	var objs []object
	var truncated bool
	switch r.typ {
	case typeS3:
		st, code := newS3(ctx, r, hc)
		if code != "" {
			return fail(code)
		}
		if objs, truncated, code = st.sample(ctx, full, n); code != "" {
			return fail(code)
		}
	case typeGCS:
		st, code := newGCS(ctx, r, hc, p.gcsBase)
		if code != "" {
			return fail(code)
		}
		if objs, truncated, code = st.sample(ctx, full, n); code != "" {
			return fail(code)
		}
	default:
		return fail(CodeUnsupportedBackend)
	}
	out.Objects, out.Truncated = len(objs), truncated
	for i, o := range objs {
		rel := strings.TrimPrefix(o.key, full)
		ct := strings.ToLower(strings.TrimSpace(strings.SplitN(o.contentType, ";", 2)[0]))
		if !tokenPattern.MatchString(strings.ReplaceAll(ct, "/", "-")) {
			ct = "other"
		}
		out.ContentTypes[ct]++
		ext := strings.ToLower(path.Ext(rel))
		switch {
		case ext == "":
			ext = "(none)"
		case !extPattern.MatchString(ext):
			ext = "other"
		}
		out.Extensions[ext]++
		for _, k := range o.metadataKeys {
			k = strings.ToLower(k)
			if !tokenPattern.MatchString(k) {
				k = "other"
			}
			out.MetadataKeys[k]++
		}
		for _, kp := range keyPatterns {
			if kp.re.MatchString(rel) {
				out.KeyPatterns[kp.name]++
			}
		}
		if i == 0 || o.size < out.MinSize {
			out.MinSize = o.size
		}
		out.MaxSize = max(out.MaxSize, o.size)
		out.MaxDepth = max(out.MaxDepth, strings.Count(rel, "/")+1)
	}
	return out, Result{}
}

func (s *s3Store) sample(ctx context.Context, prefix string, n int) ([]object, bool, string) {
	res, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String(s.r.bucket), Prefix: aws.String(prefix), MaxKeys: aws.Int32(int32(n))})
	if err != nil {
		return nil, false, CodeListFailed
	}
	var out []object
	for _, c := range res.Contents {
		o := object{key: aws.ToString(c.Key), size: aws.ToInt64(c.Size)}
		head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.r.bucket), Key: c.Key})
		if err != nil {
			return nil, false, CodeListFailed
		}
		o.contentType = aws.ToString(head.ContentType)
		for k := range head.Metadata {
			o.metadataKeys = append(o.metadataKeys, k)
		}
		sort.Strings(o.metadataKeys)
		out = append(out, o)
	}
	return out, aws.ToBool(res.IsTruncated), ""
}
