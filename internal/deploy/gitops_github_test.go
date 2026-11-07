package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeGitData is the part of the GitHub Git Data API GitHubGitOps uses, over
// a linear history.
type fakeGitData struct {
	mu      sync.Mutex
	trees   map[string]map[string]string // tree id -> path -> blob sha
	commits map[string][2]string         // commit id -> tree, parent
	head    string
	n       int
	patches int
	// beforePatch runs before every ref update, to land a commit of
	// someone else first.
	beforePatch func(f *fakeGitData)
}

func newFakeGitData() *fakeGitData {
	f := &fakeGitData{trees: map[string]map[string]string{}, commits: map[string][2]string{}}
	f.head = f.commitLocked(map[string]string{"README.md": blobSHA([]byte("gitops\n"))}, "")
	return f
}

func (f *fakeGitData) id() string { f.n++; return fmt.Sprintf("%040x", f.n) }

func (f *fakeGitData) commitLocked(files map[string]string, parent string) string {
	tree := f.id()
	f.trees[tree] = files
	c := f.id()
	f.commits[c] = [2]string{tree, parent}
	return c
}

func (f *fakeGitData) files(commit string) map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.trees[f.commits[commit][0]])
}

func (f *fakeGitData) ancestor(a, b string) bool { // a is b or an ancestor of b
	for c := b; c != ""; c = f.commits[c][1] {
		if c == a {
			return true
		}
	}
	return false
}

func (f *fakeGitData) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		http.Error(w, `{"message":"Bad credentials"}`, 401)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/repos/org/gitops")
	reply := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	var body map[string]any
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	switch {
	case r.Method == "GET" && path == "/git/ref/heads/main":
		reply(map[string]any{"object": map[string]string{"sha": f.head}})
	case r.Method == "GET" && strings.HasPrefix(path, "/git/commits/"):
		reply(map[string]any{"tree": map[string]string{"sha": f.commits[strings.TrimPrefix(path, "/git/commits/")][0]}})
	case r.Method == "GET" && strings.HasPrefix(path, "/git/trees/"):
		var entries []map[string]string
		for p, sha := range f.trees[strings.TrimPrefix(path, "/git/trees/")] {
			entries = append(entries, map[string]string{"path": p, "mode": "100644", "type": "blob", "sha": sha})
		}
		reply(map[string]any{"tree": entries, "truncated": false})
	case r.Method == "POST" && path == "/git/trees":
		files := maps.Clone(f.trees[body["base_tree"].(string)])
		for _, e := range body["tree"].([]any) {
			e := e.(map[string]any)
			p := e["path"].(string)
			if content, ok := e["content"].(string); ok {
				files[p] = blobSHA([]byte(content))
			} else if sha, ok := e["sha"]; ok && sha == nil {
				delete(files, p)
			}
		}
		tree := f.id()
		f.trees[tree] = files
		w.WriteHeader(201)
		reply(map[string]string{"sha": tree})
	case r.Method == "POST" && path == "/git/commits":
		parents := body["parents"].([]any)
		c := f.id()
		f.commits[c] = [2]string{body["tree"].(string), parents[0].(string)}
		w.WriteHeader(201)
		reply(map[string]string{"sha": c})
	case r.Method == "PATCH" && path == "/git/refs/heads/main":
		f.patches++
		if f.beforePatch != nil {
			f.beforePatch(f)
		}
		sha := body["sha"].(string)
		if body["force"] != false || f.commits[sha][1] != f.head {
			w.WriteHeader(422)
			reply(map[string]string{"message": "Update is not a fast forward"})
			return
		}
		f.head = sha
		reply(map[string]any{"object": map[string]string{"sha": sha}})
	case r.Method == "GET" && strings.HasPrefix(path, "/compare/"):
		base, head, _ := strings.Cut(strings.TrimPrefix(path, "/compare/"), "...")
		_, okB := f.commits[base]
		_, okH := f.commits[head]
		switch {
		case !okB || !okH:
			w.WriteHeader(404)
			reply(map[string]string{"message": "Not Found"})
		case base == head:
			reply(map[string]string{"status": "identical"})
		case f.ancestor(base, head):
			reply(map[string]string{"status": "ahead"})
		default:
			reply(map[string]string{"status": "behind"})
		}
	default:
		w.WriteHeader(404)
		reply(map[string]string{"message": "Not Found: " + r.Method + " " + path})
	}
}

func TestGitHubGitOps(t *testing.T) {
	fake := newFakeGitData()
	srv := httptest.NewServer(fake)
	defer srv.Close()
	g := &GitHubGitOps{Repo: "org/gitops", Branch: "main", APIURL: srv.URL, Token: func(context.Context) (string, error) { return testToken, nil }}
	ctx := t.Context()
	start := fake.head

	files := map[string][]byte{"apps/shop/gke.yaml": []byte("a: 1\n"), "apps/shop/eks.yaml": []byte("a: 2\n")}
	sha1, changed, err := g.Write(ctx, files, "deploy shop")
	if err != nil || !changed || sha1 == start {
		t.Fatalf("write: %q %v %v", sha1, changed, err)
	}
	got := fake.files(sha1)
	if got["apps/shop/gke.yaml"] != blobSHA([]byte("a: 1\n")) || got["README.md"] == "" || len(got) != 3 {
		t.Fatalf("files %v", got)
	}

	// Rewriting the same content is no commit.
	if again, changed, err := g.Write(ctx, files, "again"); err != nil || changed || again != sha1 {
		t.Fatalf("rewrite: %q %v %v", again, changed, err)
	}

	// Someone else commits first: the ref update fails, and the write starts
	// over on the new head.
	fake.beforePatch = func(f *fakeGitData) {
		f.beforePatch = nil
		files := maps.Clone(f.trees[f.commits[f.head][0]])
		files["apps/cart/gke.yaml"] = blobSHA([]byte("b\n"))
		f.head = f.commitLocked(files, f.head)
	}
	sha2, changed, err := g.Write(ctx, map[string][]byte{"apps/shop/gke.yaml": []byte("a: 3\n")}, "deploy shop 3")
	if err != nil || !changed || fake.head != sha2 {
		t.Fatalf("write after a race: %q %v %v", sha2, changed, err)
	}
	if got := fake.files(sha2); got["apps/cart/gke.yaml"] == "" || got["apps/shop/gke.yaml"] != blobSHA([]byte("a: 3\n")) {
		t.Fatalf("files after retry %v", got)
	}
	if fake.patches != 3 {
		t.Errorf("ref updates %d, want 3", fake.patches)
	}

	for _, tc := range []struct {
		rev, commit string
		want        bool
	}{{sha2, sha1, true}, {sha1, sha1, true}, {sha1, sha2, false}, {strings.Repeat("f", 40), sha1, false}} {
		if got, err := g.Contains(ctx, tc.rev, tc.commit); err != nil || got != tc.want {
			t.Errorf("Contains(%s, %s) = %v, %v; want %v", short(tc.rev), short(tc.commit), got, err, tc.want)
		}
	}

	// Delete removes every directory it is given in one commit, and nothing
	// next to them: apps/shop is not apps/shop--dev.
	instances := map[string][]byte{"apps/shop--dev/gke.yaml": []byte("d\n"), "apps/shop--qa/gke.yaml": []byte("q\n")}
	if _, _, err := g.Write(ctx, instances, "deploy shop--dev and shop--qa"); err != nil {
		t.Fatal(err)
	}
	sha3, changed, err := g.Delete(ctx, []string{"apps/shop", "apps/shop--qa"}, "delete app shop")
	if err != nil || !changed {
		t.Fatalf("delete: %v %v", changed, err)
	}
	if got := fake.files(sha3); got["apps/shop/gke.yaml"] != "" || got["apps/shop/eks.yaml"] != "" || got["apps/shop--qa/gke.yaml"] != "" ||
		got["apps/cart/gke.yaml"] == "" || got["apps/shop--dev/gke.yaml"] == "" {
		t.Fatalf("files after delete %v", got)
	}
	if again, changed, err := g.Delete(ctx, []string{"apps/shop", "apps/shop--qa"}, "again"); err != nil || changed || again != sha3 {
		t.Fatalf("second delete: %q %v %v", again, changed, err)
	}

	// A file is deleted on its own (an environment taken out, ADR 0037).
	if _, _, err := g.Write(ctx, map[string][]byte{"apps/shop--dev/eks.yaml": []byte("e")}, "deploy shop--dev to eks"); err != nil {
		t.Fatal(err)
	}
	sha3, changed, err = g.Delete(ctx, []string{FilePath("shop--dev", "eks")}, "remove eks")
	if got := fake.files(sha3); err != nil || !changed || got["apps/shop--dev/eks.yaml"] != "" || got["apps/shop--dev/gke.yaml"] == "" {
		t.Fatalf("delete a file: %v %v, files %v", changed, err, got)
	}

	// Nothing that is not one instance's directory, such as AppDir of an
	// empty name, is deleted: the request fails before GitHub sees it.
	patches := fake.patches
	for _, dirs := range refusedDeletes {
		if _, _, err := g.Delete(ctx, dirs, "delete everything"); err == nil {
			t.Errorf("Delete(%q) succeeded", dirs)
		}
	}
	if got := fake.files(fake.head); fake.head != sha3 || fake.patches != patches || got["apps/cart/gke.yaml"] == "" {
		t.Fatalf("a refused delete changed the repository: head %s, files %v", short(fake.head), got)
	}

	bad := *g
	bad.Token = func(context.Context) (string, error) { return "wrong", nil }
	if _, _, err := bad.Write(ctx, files, "x"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("bad token: %v", err)
	}
}

func TestBlobSHA(t *testing.T) {
	// git hash-object of "hello\n"
	if got := blobSHA([]byte("hello\n")); got != "ce013625030ba8dba906f756967f9e9ca394464a" {
		t.Fatal(got)
	}
}
