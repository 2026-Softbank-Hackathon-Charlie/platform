//go:build minio

// The whole gate against a real MinIO server and the embedded policy: the
// observation comes from the store, the verdict from Rego. Run with the
// BUCKETPROBE_MINIO_* variables of internal/preflight/bucketprobe's live test.
package gate

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/bucketprobe"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/residency"
)

func TestLiveGateWithMinIO(t *testing.T) {
	a, b := os.Getenv("BUCKETPROBE_MINIO_A"), os.Getenv("BUCKETPROBE_MINIO_B")
	key, secret := os.Getenv("BUCKETPROBE_MINIO_KEY"), os.Getenv("BUCKETPROBE_MINIO_SECRET")
	if a == "" || b == "" || key == "" {
		t.Skip("BUCKETPROBE_MINIO_* not set")
	}
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("rclone is required for real content inspection")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	suffix := time.Now().Format("150405")
	src, dst := "gate-src-"+suffix, "gate-dst-"+suffix
	for _, x := range []struct{ ep, bucket string }{{a, src}, {b, dst}} {
		c, err := minio.New(x.ep, &minio.Options{Creds: credentials.NewStaticV4(key, secret, "")})
		if err != nil {
			t.Fatal(err)
		}
		if err := c.MakeBucket(ctx, x.bucket, minio.MakeBucketOptions{}); err != nil {
			t.Fatal(err)
		}
		if x.bucket == src {
			for _, k := range []string{"rooms/1.txt", "hosts/photo.txt"} {
				if _, err := c.PutObject(ctx, src, k, bytes.NewReader([]byte("x")), 1, minio.PutObjectOptions{}); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	vars := map[string]string{}
	for remote, ep := range map[string]string{"ONPREM": a, "DR": b} {
		vars["RCLONE_CONFIG_"+remote+"_TYPE"], vars["RCLONE_CONFIG_"+remote+"_PROVIDER"] = "s3", "Minio"
		vars["RCLONE_CONFIG_"+remote+"_ENDPOINT"] = "http://" + ep
		vars["RCLONE_CONFIG_"+remote+"_ACCESS_KEY_ID"], vars["RCLONE_CONFIG_"+remote+"_SECRET_ACCESS_KEY"] = key, secret
	}
	for k, v := range vars {
		t.Setenv(k, v)
	} // real rclone must read the same remotes as the real prober
	prober := bucketprobe.Prober{Getenv: func(k string) string { return vars[k] }}

	check := func(network, prefixes string) []string {
		t.Helper()
		doc := strings.NewReplacer("SRC", src, "DST", dst, "NET", network, "PREFIXES", prefixes).Replace(`
version: 1
inspectionEnvironment: onprem-kr
environments: [{id: onprem-kr, country: KR}, {id: eks-kr, country: KR}, {id: eks-jp, country: JP}]
apacCountries: [KR, JP]
catalog:
  - scope: {environment: onprem-kr, namespace: shop, cluster: shop-db}
    datasets:
      - {dataset: {database: app, schema: public, table: products}, label: global}
      - {dataset: {database: app, schema: public, table: customers}, label: global}
buckets:
  - {bucket: "onprem:SRC", environment: onprem-kr, label: global, expect: {endpointCIDRs: [NET]}PREFIXES}
  - {bucket: "dr:DST", environment: eks-jp, label: global, expect: {endpointCIDRs: [NET]}}
`)
		cfg, err := residency.ParseConfig([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		g, err := New(ctx, cfg, complete())
		if err != nil {
			t.Fatal(err)
		}
		g.WithProber(prober)
		p := shopPlan("eks-kr")
		p.Files = &plan.FilesSpec{Source: "onprem:" + src, Target: "dr:" + dst}
		v, err := g.Check(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("network %s prefixes %q: allowed=%v reasons=%v buckets=%+v", network, prefixes, v.Allowed, codes(v), v.Buckets)
		if v.Allowed != (len(v.Reasons) == 0) || len(v.Buckets) != 2 {
			t.Fatalf("verdict %+v", v)
		}
		return codes(v)
	}

	if got := check("127.0.0.0/8", ""); len(got) != 0 {
		t.Fatalf("on the declared network: %v", got)
	}
	if got := check("10.0.0.0/8", ""); !slices.Contains(got, "bucket_endpoint_outside_network") {
		t.Fatalf("endpoint off the declared network: %v", got)
	}
	if got := check("127.0.0.0/8", ", prefixes: [{prefix: rooms/, label: global}]"); !slices.Equal(got, []string{"unclassified_prefix"}) {
		t.Fatalf("hosts/ unclassified: %v", got)
	}
	if got := check("127.0.0.0/8", ", prefixes: [{prefix: rooms/, label: global}, {prefix: hosts/, label: kr-only}]"); !slices.Equal(got, []string{"prefix_country_not_allowed"}) {
		t.Fatalf("kr-only hosts/ to Japan: %v", got)
	}
}
