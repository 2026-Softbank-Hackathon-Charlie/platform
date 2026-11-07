package api

import (
	"encoding/pem"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onprem"
)

// The issuer and claim are real HTTP handlers over TLS. Only host commands
// with installation side effects and the upstream download are test doubles.
func TestInstallerAuthenticatesBeforeInstallation(t *testing.T) {
	missingPrerequisite := func(reason string) {
		t.Helper()
		if os.Getenv("GITHUB_ACTIONS") == "true" {
			t.Fatal(reason)
		}
		t.Skip(reason)
	}
	if runtime.GOOS == "windows" {
		missingPrerequisite("Linux installer shell fixture")
	}
	for _, path := range []string{"/etc/rancher/k3s", "/etc/systemd/system/k3s.service"} {
		if _, err := os.Stat(path); err == nil {
			missingPrerequisite("host has k3s configuration; fixture must run on a clean CI host")
		}
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		missingPrerequisite("sh is not installed")
	}
	realCurl, err := exec.LookPath("curl")
	if err != nil {
		missingPrerequisite("curl is not installed")
	}
	for _, scenario := range []string{"wrong-code", "expired", "revoked", "accepted", "checksum-failed"} {
		t.Run(scenario, func(t *testing.T) {
			h := newHarness(t)
			now := time.Now().UTC()
			h.api.now = func() time.Time { return now }
			status, b := h.do("POST", "/v1/onprem/enrollments", opToken, map[string]any{"environment": "onprem", "installK3s": true}, "Idempotency-Key", "installer-test")
			if status != 201 {
				t.Fatal("issuance failed")
			}
			issued := decode[onprem.Created](t, b)
			code := issued.Code
			switch scenario {
			case "wrong-code":
				code = "onp_" + strings.Repeat("z", 43)
			case "expired":
				now = now.Add(15 * time.Minute)
			case "revoked":
				if status, _ = h.do("POST", "/v1/onprem/enrollments/"+issued.Enrollment.ID+"/revoke", opToken, nil); status != 200 {
					t.Fatal("revoke failed")
				}
			}
			tlsServer := httptest.NewTLSServer(h.api)
			defer tlsServer.Close()
			dir := t.TempDir()
			bin := filepath.Join(dir, "bin")
			if err = os.Mkdir(bin, 0700); err != nil {
				t.Fatal(err)
			}
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(name, []byte(body), 0700); err != nil {
					t.Fatal(err)
				}
			}
			ca := filepath.Join(dir, "ca.pem")
			if err = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsServer.Certificate().Raw}), 0600); err != nil {
				t.Fatal(err)
			}
			installer := filepath.Join(dir, "installer.sh")
			write(installer, onprem.Installer)
			trace := filepath.Join(dir, "trace")
			argsFile := filepath.Join(dir, "curl-args")
			write(filepath.Join(bin, "uname"), "#!/bin/sh\nprintf 'Linux\\n'\n")
			write(filepath.Join(bin, "id"), "#!/bin/sh\nprintf '0\\n'\n")
			write(filepath.Join(bin, "rc-service"), "#!/bin/sh\nexit 0\n")
			write(filepath.Join(bin, "sha256sum"), "#!/bin/sh\nprintf 'checksum\\n' >> \"$TRACE\"\n[ \"$SCENARIO\" != checksum-failed ]\n")
			upstream := filepath.Join(dir, "fake-k3s-install.sh")
			write(upstream, "#!/bin/sh\nprintf 'install\\n' >> \"$TRACE\"\nprintf '#!/bin/sh\\nexit 0\\n' > \"$FAKE_BIN/k3s\"\nchmod 700 \"$FAKE_BIN/k3s\"\n")
			write(filepath.Join(bin, "curl"), `#!/bin/sh
printf '%s\n' "$*" >> "$CURL_ARGS"
case "$*" in
  *raw.githubusercontent.com*)
    printf 'download\n' >> "$TRACE"
    while [ "$#" -gt 0 ]; do
      if [ "$1" = -o ]; then cp "$UPSTREAM_FIXTURE" "$2"; exit 0; fi
      shift
    done
    exit 2;;
  *) printf 'claim\n' >> "$TRACE"; exec "$REAL_CURL" "$@";;
esac
`)
			cmd := exec.Command(sh, installer, "--server", tlsServer.URL, "--enrollment", issued.Enrollment.ID, "--environment", "onprem", "--code", code, "--install-k3s")
			cmd.Env = append(os.Environ(), "PATH="+bin+":/usr/bin:/bin", "TMPDIR="+dir, "CURL_CA_BUNDLE="+ca, "REAL_CURL="+realCurl, "TRACE="+trace, "CURL_ARGS="+argsFile, "FAKE_BIN="+bin, "UPSTREAM_FIXTURE="+upstream, "SCENARIO="+scenario, "NO_PROXY=127.0.0.1,localhost", "no_proxy=127.0.0.1,localhost")
			out, runErr := cmd.CombinedOutput()
			events, _ := os.ReadFile(trace)
			if scenario == "accepted" {
				if runErr != nil || string(events) != "claim\ndownload\nchecksum\ninstall\n" || !strings.Contains(string(out), "not a ready environment") {
					t.Fatalf("bootstrap sequence failed: %v, events=%q, output=%s", runErr, events, out)
				}
			} else {
				if runErr == nil || strings.Contains(string(events), "install\n") {
					t.Fatalf("rejected bootstrap installed k3s: scenario=%s events=%q", scenario, events)
				}
				if scenario != "checksum-failed" && string(events) != "claim\n" {
					t.Fatalf("download before successful claim: %q", events)
				}
			}
			args, _ := os.ReadFile(argsFile)
			if strings.Contains(string(args), code) {
				t.Fatal("curl arguments exposed bootstrap code")
			}
			_, result := h.do("GET", "/v1/onprem/enrollments/"+issued.Enrollment.ID, opToken, nil)
			if scenario == "checksum-failed" && !strings.Contains(string(result), `"status":"claimed"`) {
				t.Fatal("failed installation incorrectly restored consumed grant")
			}
		})
	}
}
