// Command fakegithub answers the GitHub API calls the platform's GitHub App
// client makes (internal/github), from local git repositories, so the deploy
// E2E registers and deploys apps without GitHub.
//
//	fakegithub -listen 127.0.0.1:18392 -root repos -key app-key.pem
//
// Repository owner/name is the git checkout <root>/<owner>/<name>; the
// platform's docker builder clones that path too (PLATFORM_DOCKER_CLONE_URL).
// -key writes a fresh RSA key for PLATFORM_GITHUB_APP_PRIVATE_KEY_FILE; the
// fake checks no signature or token.
//
//	POST /app/installations/{id}/access_tokens   a token valid for an hour
//	GET  /orgs/{org}/installation                installation 1
//	GET  /repos/{owner}/{name}/commits/{ref}     the commit sha (git rev-parse)
//	GET  /repos/{owner}/{name}/contents/{path}   the file at ?ref= (git show)
//	GET  /installation/repositories              every repository under root
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"flag"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var shaPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func main() {
	listen := flag.String("listen", "127.0.0.1:18392", "address to listen on")
	root := flag.String("root", "", "directory of the checkouts <owner>/<name>")
	keyFile := flag.String("key", "", "write a new RSA private key here (PEM)")
	flag.Parse()
	if *root == "" {
		log.Fatal("-root is required")
	}
	if *keyFile != "" {
		key, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			log.Fatal(err)
		}
		pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
		if err := os.WriteFile(*keyFile, pemBytes, 0o600); err != nil {
			log.Fatal(err)
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("POST /app/installations/{id}/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"token": "ghs_e2e" + randomHex(), "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	})
	mux.HandleFunc("GET /orgs/{org}/installation", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"id": 1})
	})
	mux.HandleFunc("GET /repos/{owner}/{name}/commits/{ref...}", func(w http.ResponseWriter, r *http.Request) {
		dir, ok := repoDir(w, *root, r)
		if !ok {
			return
		}
		sha, err := git(dir, "rev-parse", "--verify", "--quiet", r.PathValue("ref")+"^{commit}")
		if err != nil || !shaPattern.MatchString(sha) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "No commit found for SHA: " + r.PathValue("ref")})
			return
		}
		w.Header().Set("Content-Type", "application/vnd.github.sha")
		_, _ = w.Write([]byte(sha))
	})
	mux.HandleFunc("GET /repos/{owner}/{name}/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		dir, ok := repoDir(w, *root, r)
		if !ok {
			return
		}
		ref := r.URL.Query().Get("ref")
		if ref == "" {
			ref = "HEAD"
		}
		object := ref + ":" + r.PathValue("path")
		kind, err := git(dir, "cat-file", "-t", object)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		if kind != "blob" {
			writeJSON(w, http.StatusOK, map[string]any{"type": "dir", "path": r.PathValue("path")})
			return
		}
		content, err := exec.Command("git", "-C", dir, "cat-file", "blob", object).Output()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"type": "file", "encoding": "base64", "size": len(content),
			"path": r.PathValue("path"), "content": base64.StdEncoding.EncodeToString(content),
		})
	})
	mux.HandleFunc("GET /installation/repositories", func(w http.ResponseWriter, _ *http.Request) {
		dirs, _ := filepath.Glob(filepath.Join(*root, "*", "*", ".git"))
		repos := []map[string]any{}
		for _, d := range dirs {
			name := filepath.Base(filepath.Dir(d))
			owner := filepath.Base(filepath.Dir(filepath.Dir(d)))
			repos = append(repos, map[string]any{"full_name": owner + "/" + name, "private": false, "default_branch": "main"})
		}
		writeJSON(w, http.StatusOK, map[string]any{"total_count": len(repos), "repositories": repos})
	})
	log.Printf("serving %s on %s", *root, *listen)
	log.Fatal(http.ListenAndServe(*listen, logged(mux)))
}

func repoDir(w http.ResponseWriter, root string, r *http.Request) (string, bool) {
	dir := filepath.Join(root, r.PathValue("owner"), r.PathValue("name"))
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return "", false
	}
	return dir, true
}

func git(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).Output()
	return strings.TrimSpace(string(out)), err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func randomHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return strings.ToLower(base64.RawURLEncoding.EncodeToString(b))
}

func logged(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.RequestURI())
		h.ServeHTTP(w, r)
	})
}
