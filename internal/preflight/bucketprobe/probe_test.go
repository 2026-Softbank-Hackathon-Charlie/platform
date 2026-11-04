package bucketprobe

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight"
)

// fakeS3 answers the S3 metadata calls the probe makes for one bucket, as
// AWS or MinIO would (path-style, XML).
type fakeS3 struct {
	mu          sync.Mutex
	location    string
	objects     []string // keys
	replication string   // ReplicationConfiguration XML body; "" = none
	lifecycle   string   // LifecycleConfiguration XML body; "" = none
	pageSize    int
	seen        []string
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	f.seen = append(f.seen, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	if r.Method != http.MethodGet || r.URL.Path != "/photos" {
		http.Error(w, "unexpected", http.StatusBadRequest)
		return
	}
	notFound := func(code string) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>none</Message></Error>`, code)
	}
	switch {
	case q.Has("location"):
		fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">%s</LocationConstraint>`, f.location)
	case q.Has("replication"):
		if f.replication == "" {
			notFound("ReplicationConfigurationNotFoundError")
			return
		}
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><ReplicationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+f.replication+`</ReplicationConfiguration>`)
	case q.Has("lifecycle"):
		if f.lifecycle == "" {
			notFound("NoSuchLifecycleConfiguration")
			return
		}
		fmt.Fprint(w, `<?xml version="1.0" encoding="UTF-8"?><LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`+f.lifecycle+`</LifecycleConfiguration>`)
	case q.Get("list-type") == "2":
		f.list(w, q)
	default:
		http.Error(w, "unexpected query", http.StatusBadRequest)
	}
}

// list implements ListObjectsV2 with a delimiter of "/" and paging.
func (f *fakeS3) list(w http.ResponseWriter, q url.Values) {
	prefix, start := q.Get("prefix"), q.Get("continuation-token")
	type entry struct {
		key    string
		common bool
	}
	var entries []entry
	seen := map[string]bool{}
	for _, k := range f.objects {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := strings.TrimPrefix(k, prefix)
		if i := strings.Index(rest, "/"); i >= 0 {
			p := prefix + rest[:i+1]
			if !seen[p] {
				seen[p] = true
				entries = append(entries, entry{p, true})
			}
			continue
		}
		entries = append(entries, entry{k, false})
	}
	from := 0
	if start != "" {
		fmt.Sscanf(start, "%d", &from)
	}
	size := f.pageSize
	if size == 0 {
		size = 1000
	}
	to := min(from+size, len(entries))
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>photos</Name>`)
	for _, e := range entries[from:to] {
		if e.common {
			fmt.Fprintf(&b, `<CommonPrefixes><Prefix>%s</Prefix></CommonPrefixes>`, e.key)
		} else {
			fmt.Fprintf(&b, `<Contents><Key>%s</Key><Size>1</Size></Contents>`, e.key)
		}
	}
	if to < len(entries) {
		fmt.Fprintf(&b, `<IsTruncated>true</IsTruncated><NextContinuationToken>%d</NextContinuationToken>`, to)
	} else {
		b.WriteString(`<IsTruncated>false</IsTruncated>`)
	}
	b.WriteString(`</ListBucketResult>`)
	fmt.Fprint(w, b.String())
}

type fixedResolver map[string][]netip.Addr

func (f fixedResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if a, ok := f[host]; ok {
		return a, nil
	}
	return nil, fmt.Errorf("no such host")
}

func env(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

// minioEnv configures remote "minio" the way the e2e and on-prem setups do.
func minioEnv(endpoint string) map[string]string {
	return map[string]string{
		"RCLONE_CONFIG_MINIO_TYPE": "s3", "RCLONE_CONFIG_MINIO_PROVIDER": "Minio",
		"RCLONE_CONFIG_MINIO_ENDPOINT": endpoint, "RCLONE_CONFIG_MINIO_ACCESS_KEY_ID": "AKIDTOPSECRET",
		"RCLONE_CONFIG_MINIO_SECRET_ACCESS_KEY": "TOPSECRET",
	}
}

func TestProbeSelfHostedMinIO(t *testing.T) {
	f := &fakeS3{location: "us-east-1", pageSize: 2, objects: []string{
		"cover.jpg", "kyc/1.jpg", "kyc/2.jpg", "rooms/a/1.jpg", "rooms/b.jpg", "thumbs/x.jpg",
	}, lifecycle: `<Rule><ID>expire</ID><Status>Enabled</Status><Filter><Prefix></Prefix></Filter><Expiration><Days>30</Days></Expiration></Rule>`}
	srv := httptest.NewServer(f)
	defer srv.Close()
	p := Prober{Getenv: env(minioEnv(srv.URL))}
	obs, res := p.Probe(context.Background(), "minio:photos", Options{Prefixes: true})
	if res.Code != "" {
		t.Fatalf("code %q (requests %v)", res.Code, f.seen)
	}
	want := preflight.BucketObservation{
		Hosting: preflight.HostingSelf, Location: "us-east-1", LocationType: "region",
		EndpointIPs: []string{"127.0.0.1"}, Prefixes: []string{"", "kyc/", "rooms/", "thumbs/"}, Replicas: []string{},
	}
	if !reflect.DeepEqual(obs, want) {
		t.Fatalf("observation\n got %+v\nwant %+v", obs, want)
	}
	raw, _ := json.Marshal(obs)
	if strings.Contains(string(raw), "TOPSECRET") || strings.Contains(string(raw), srv.URL) {
		t.Fatalf("observation leaks: %s", raw)
	}
}

func TestProbeListsUnderPath(t *testing.T) {
	f := &fakeS3{location: "us-east-1", objects: []string{"other/x", "app/a/1", "app/b/2", "app/root.txt"}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	obs, res := Prober{Getenv: env(minioEnv(srv.URL))}.Probe(context.Background(), "minio:photos/app", Options{Prefixes: true})
	if res.Code != "" || !reflect.DeepEqual(obs.Prefixes, []string{"", "a/", "b/"}) {
		t.Fatalf("%q %v", res.Code, obs.Prefixes)
	}
}

func TestProbeReplicationAndTiering(t *testing.T) {
	f := &fakeS3{location: "us-east-1",
		replication: `<Role></Role>` +
			`<Rule><ID>dr</ID><Status>Enabled</Status><Priority>1</Priority><Filter><Prefix></Prefix></Filter><Destination><Bucket>arn:minio:replication::5f1c:photos-dr</Bucket></Destination></Rule>` +
			`<Rule><ID>old</ID><Status>Disabled</Status><Priority>2</Priority><Filter><Prefix></Prefix></Filter><Destination><Bucket>arn:aws:s3:::photos-off</Bucket></Destination></Rule>`,
		lifecycle: `<Rule><ID>tier</ID><Status>Enabled</Status><Filter><Prefix></Prefix></Filter><Transition><Days>30</Days><StorageClass>WARM-S3</StorageClass></Transition></Rule>`,
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	obs, res := Prober{Getenv: env(minioEnv(srv.URL))}.Probe(context.Background(), "minio:photos", Options{Replication: true})
	if res.Code != "" || !reflect.DeepEqual(obs.Replicas, []string{"photos-dr"}) || !obs.Tiered {
		t.Fatalf("%q %+v", res.Code, obs)
	}
}

// "provider = AWS" pointed at any other endpoint is self-hosted: its region is
// only what that server says.
func TestProbeAWSProviderWithForeignEndpointIsSelfHosted(t *testing.T) {
	f := &fakeS3{location: "ap-northeast-2"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	vars := minioEnv(srv.URL)
	vars["RCLONE_CONFIG_MINIO_PROVIDER"] = "AWS"
	obs, res := Prober{Getenv: env(vars)}.Probe(context.Background(), "minio:photos", Options{})
	if res.Code != "" || obs.Hosting != preflight.HostingSelf {
		t.Fatalf("%q %+v", res.Code, obs)
	}
}

func TestProbeFailures(t *testing.T) {
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "TOPSECRET internal detail", http.StatusInternalServerError)
	}))
	defer broken.Close()
	for name, tc := range map[string]struct {
		vars   map[string]string
		bucket string
		code   string
	}{
		"bad spec":            {minioEnv(broken.URL), ":s3,endpoint=x:photos", CodeInvalidBucket},
		"dot segment":         {minioEnv(broken.URL), "minio:photos/../etc", CodeInvalidBucket},
		"remote not in env":   {map[string]string{}, "minio:photos", CodeRemoteNotConfigured},
		"unsupported type":    {map[string]string{"RCLONE_CONFIG_MINIO_TYPE": "sftp"}, "minio:photos", CodeUnsupportedBackend},
		"store error":         {minioEnv(broken.URL), "minio:photos", CodeLocationFailed},
		"unresolvable host":   {minioEnv("http://minio.invalid:9000"), "minio:photos", CodeLocationFailed},
		"gcs custom endpoint": {map[string]string{"RCLONE_CONFIG_G_TYPE": "google cloud storage", "RCLONE_CONFIG_G_ENDPOINT": "https://evil.example"}, "g:photos", CodeUnsupportedBackend},
	} {
		t.Run(name, func(t *testing.T) {
			obs, res := Prober{Getenv: env(tc.vars), Resolver: fixedResolver{}}.Probe(context.Background(), tc.bucket, Options{Prefixes: true})
			if res.Code != tc.code {
				t.Fatalf("code %q, want %q", res.Code, tc.code)
			}
			raw, _ := json.Marshal(obs)
			if strings.Contains(string(raw), "TOPSECRET") || obs.Hosting != "" {
				t.Fatalf("failed observation must be empty: %s", raw)
			}
		})
	}
}

func TestProbeRejectsUnnameablePrefixAndHugeListing(t *testing.T) {
	f := &fakeS3{location: "us-east-1", objects: []string{"has space/x"}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	p := Prober{Getenv: env(minioEnv(srv.URL))}
	if _, res := p.Probe(context.Background(), "minio:photos", Options{Prefixes: true}); res.Code != CodeUnsupportedPrefix {
		t.Fatalf("code %q", res.Code)
	}
	f.objects = nil
	for i := 0; i <= 1000; i++ {
		f.objects = append(f.objects, fmt.Sprintf("p%04d/x", i))
	}
	if _, res := p.Probe(context.Background(), "minio:photos", Options{Prefixes: true}); res.Code != CodeTooManyPrefixes {
		t.Fatalf("code %q", res.Code)
	}
}

func TestProbeResolvesEndpointHost(t *testing.T) {
	f := &fakeS3{location: "us-east-1"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	vars := minioEnv("http://minio.lan:" + u.Port())
	res := fixedResolver{"minio.lan": {netip.MustParseAddr("::ffff:127.0.0.1"), netip.MustParseAddr("127.0.0.1")}}
	// The SDK dials minio.lan; route it to the test server.
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, u.Host)
	}}}
	obs, r := Prober{Getenv: env(vars), Resolver: res, HTTPClient: hc}.Probe(context.Background(), "minio:photos", Options{})
	if r.Code != "" || !reflect.DeepEqual(obs.EndpointIPs, []string{"127.0.0.1"}) {
		t.Fatalf("%q %+v", r.Code, obs.EndpointIPs)
	}
}

func TestGCSProbe(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/storage/v1/b/photos":
			fmt.Fprint(w, `{"location":"ASIA-NORTHEAST3","locationType":"region"}`)
		case "/storage/v1/b/photos/o":
			if r.URL.Query().Get("pageToken") == "" {
				fmt.Fprint(w, `{"prefixes":["rooms/"],"items":[{"name":"cover.jpg"}],"nextPageToken":"p2"}`)
				return
			}
			fmt.Fprint(w, `{"prefixes":["kyc/"]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	p := Prober{
		Getenv:   env(map[string]string{"RCLONE_CONFIG_GCS_TYPE": "google cloud storage"}),
		Resolver: fixedResolver{gcsHost: {netip.MustParseAddr("142.250.1.1")}},
		gcsBase:  srv.URL,
	}
	obs, res := p.Probe(context.Background(), "gcs:photos", Options{Prefixes: true, Replication: true})
	want := preflight.BucketObservation{Hosting: preflight.HostingProvider, Location: "asia-northeast3", LocationType: "region",
		EndpointIPs: []string{"142.250.1.1"}, Prefixes: []string{"", "kyc/", "rooms/"}, Replicas: []string{}}
	if res.Code != "" || !reflect.DeepEqual(obs, want) || gotAuth != "" {
		t.Fatalf("%q %+v auth=%q", res.Code, obs, gotAuth)
	}
}

func TestGCSLocationType(t *testing.T) {
	for loc, want := range map[string]string{
		"asia-northeast3": "region", "us-central1": "region", "asia1": "dual-region", "nam4": "dual-region",
		"asia": "multi-region", "eu": "multi-region", "": "multi-region",
	} {
		if got := gcsLocationType(loc); got != want {
			t.Errorf("%q: %q, want %q", loc, got, want)
		}
	}
}
