package vm

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// opensslImage and encryptScript are inspect.sh's and connect.sh's (infra
// terraform/platform/aws/onboarding): the test makes envelopes the way the
// VM does.
const opensslImage = "alpine/openssl:3.5.9@sha256:6821afa2476e11be343f29275f2ca24291720ff0a7148bd131fd8e314ff26d9e"

const encryptScript = `
set -eu
umask 077
cd /w
cat > p
printf "%s" "$HC_PUB" | openssl base64 -d -A > pub.der
openssl rand -hex 32 > k
openssl enc -aes-256-cbc -pbkdf2 -iter 10000 -md sha256 -salt -pass file:k -in p -out c
head -c 64 k > w
openssl dgst -sha256 -binary p >> w
openssl pkeyutl -encrypt -pubin -keyform DER -inkey pub.der -pkeyopt rsa_padding_mode:oaep -pkeyopt rsa_oaep_md:sha256 -pkeyopt rsa_mgf1_md:sha256 -in w -out wk
printf "hcenc1.%s.%s" "$(openssl base64 -A -in wk)" "$(openssl base64 -A -in c)"
`

// goldenPlain is what testdata/envelope.txt holds, made by the openssl
// container for testdata/key.der.b64 (HC_WRITE_GOLDEN=1 go test -run Golden
// writes both again).
const goldenPlain = "{\"moimroom-app-1\":[\"APP_SECRET=s3cr3t\",\"MULTI=line1\\nline2=x\",\"EMPTY=\"]}"

func dockerAvailable() bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	return exec.Command("docker", "info").Run() == nil
}

func opensslSeal(t *testing.T, k *Key, plain string) string {
	t.Helper()
	cmd := exec.Command("docker", "run", "--rm", "-i", "--network", "none", "--read-only", "--tmpfs", "/w:rw,mode=700,size=16m",
		"-e", "HC_PUB="+k.PublicKey(), "--entrypoint", "sh", opensslImage, "-c", encryptScript)
	cmd.Stdin = strings.NewReader(plain)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("openssl container: %v: %s", err, stderr.String())
	}
	return string(out)
}

func goldenKey(t *testing.T) *Key {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "key.der.b64"))
	if err != nil {
		t.Fatal(err)
	}
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	priv, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		t.Fatal(err)
	}
	return &Key{priv: priv.(*rsa.PrivateKey)}
}

func TestOpenGoldenEnvelope(t *testing.T) {
	if os.Getenv("HC_WRITE_GOLDEN") == "1" {
		if !dockerAvailable() {
			t.Fatal("writing the golden envelope needs docker")
		}
		k, err := NewKey()
		if err != nil {
			t.Fatal(err)
		}
		der, _ := x509.MarshalPKCS8PrivateKey(k.priv)
		_ = os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(filepath.Join("testdata", "key.der.b64"), []byte(base64.StdEncoding.EncodeToString(der)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("testdata", "envelope.txt"), []byte(opensslSeal(t, k, goldenPlain)+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env, err := os.ReadFile(filepath.Join("testdata", "envelope.txt"))
	if err != nil {
		t.Fatal(err)
	}
	k := goldenKey(t)
	got, err := k.Open(string(env))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != goldenPlain {
		t.Fatalf("plaintext %q", got)
	}
	v, err := OpenValues(k, string(env))
	if err != nil {
		t.Fatal(err)
	}
	if v["moimroom-app-1"]["MULTI"] != "line1\nline2=x" || v["moimroom-app-1"]["EMPTY"] != "" || len(v["moimroom-app-1"]) != 3 {
		t.Fatalf("values %d", len(v["moimroom-app-1"]))
	}
}

// TestOpenLiveOpenSSL encrypts with the pinned openssl container, as the
// VM does, when docker is available.
func TestOpenLiveOpenSSL(t *testing.T) {
	if testing.Short() || !dockerAvailable() {
		t.Skip("docker is not available")
	}
	k, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	for _, plain := range []string{"", "x", strings.Repeat("0123456789abcdef", 1000), goldenPlain} {
		got, err := k.Open(opensslSeal(t, k, plain))
		if err != nil {
			t.Fatalf("%d bytes: %v", len(plain), err)
		}
		if string(got) != plain {
			t.Fatalf("%d bytes: round trip differs", len(plain))
		}
	}
}

func TestOpenRefusesDamage(t *testing.T) {
	env, err := os.ReadFile(filepath.Join("testdata", "envelope.txt"))
	if err != nil {
		t.Fatal(err)
	}
	k := goldenKey(t)
	s := strings.TrimSpace(string(env))
	// Flip one character of the body's last block: the padding or the
	// digest no longer matches.
	i := len(s) - 6
	c := byte('A')
	if s[i] == 'A' {
		c = 'B'
	}
	bad := s[:i] + string(c) + s[i+1:]
	for name, e := range map[string]string{"damaged": bad, "prefix": "hcenc2." + s[7:], "cut": s[:len(s)/2], "empty": ""} {
		if _, err := k.Open(e); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
	other, _ := NewKey()
	if _, err := other.Open(s); err == nil {
		t.Error("opened with another key")
	}
}
