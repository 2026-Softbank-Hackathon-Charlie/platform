// Command gitserver serves bare git repositories over smart HTTP (git
// http-backend), without authentication and with pushes allowed. The deploy
// E2E runs it in the kind cluster as the gitops repository and the chart
// repository: run.sh pushes from the host through a NodePort, Argo CD fetches
// through the Service.
//
//	gitserver -listen :8080 -root /srv/git -init gitops,infra
//
// GET /healthz answers 200. Everything else goes to git http-backend, so
// http://<host>/<name>.git is the repository <root>/<name>.git.
package main

import (
	"flag"
	"log"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

func main() {
	listen := flag.String("listen", ":8080", "address to listen on")
	root := flag.String("root", "/srv/git", "directory holding the bare repositories <name>.git")
	repos := flag.String("init", "", "comma-separated repositories to create when missing, each with branch main")
	flag.Parse()

	dir, err := filepath.Abs(*root)
	if err != nil {
		log.Fatal(err)
	}
	out, err := exec.Command("git", "--exec-path").Output()
	if err != nil {
		log.Fatal("git --exec-path: ", err)
	}
	backend := filepath.Join(strings.TrimSpace(string(out)), "git-http-backend")
	if runtime.GOOS == "windows" {
		backend += ".exe"
	}
	for _, name := range strings.Split(*repos, ",") {
		if name = strings.TrimSpace(name); name == "" {
			continue
		}
		repo := filepath.Join(dir, name+".git")
		if _, err := os.Stat(repo); err == nil {
			continue
		}
		if out, err := exec.Command("git", "init", "--quiet", "--bare", "--initial-branch=main", repo).CombinedOutput(); err != nil {
			log.Fatalf("git init %s: %v: %s", repo, err, out)
		}
		log.Printf("created %s", repo)
	}

	git := &cgi.Handler{
		Path: backend,
		Dir:  dir,
		Env: []string{
			"GIT_PROJECT_ROOT=" + dir,
			"GIT_HTTP_EXPORT_ALL=1",
			// No REMOTE_USER, so pushes need http.receivepack. Fetching any
			// sha lets clients ask for a commit by id.
			"GIT_CONFIG_COUNT=2",
			"GIT_CONFIG_KEY_0=http.receivepack", "GIT_CONFIG_VALUE_0=true",
			"GIT_CONFIG_KEY_1=uploadpack.allowAnySHA1InWant", "GIT_CONFIG_VALUE_1=true",
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.Handle("/", git)
	log.Printf("serving %s on %s", dir, *listen)
	log.Fatal(http.ListenAndServe(*listen, mux))
}
