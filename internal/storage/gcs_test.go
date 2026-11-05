package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGCS implements the public JSON API wire format, including etags and
// independent pages; it never issues real cloud requests or credentials.
type fakeGCS struct {
	mu                sync.Mutex
	buckets           map[string]*gcsBucket
	accounts          map[string]*gcsAccount
	keys              map[string]gcsKey
	policies          map[string]gcsPolicy
	objects           map[string][]gcsObject
	calls             []string
	fail              map[string]int
	failOnce          map[string]int
	foreign           map[string]bool
	keyN              int
	policyConflicts   int
	missingSecretOnce bool
	nullCreate        string
}

func newFakeGCS(t *testing.T) (*gcsDriver, *fakeGCS) {
	t.Helper()
	f := &fakeGCS{buckets: map[string]*gcsBucket{}, accounts: map[string]*gcsAccount{}, keys: map[string]gcsKey{}, policies: map[string]gcsPolicy{}, objects: map[string][]gcsObject{}, fail: map[string]int{}, foreign: map[string]bool{}}
	s := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(s.Close)
	return &gcsDriver{project: "project-demo", prefix: "hc-gke-", region: "asia-northeast3", client: s.Client(), storageURL: s.URL + "/storage/v1", iamURL: s.URL + "/v1"}, f
}

func (f *fakeGCS) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	call := r.Method + " " + r.URL.Path
	f.calls = append(f.calls, call)
	if code := f.fail[call]; code != 0 {
		http.Error(w, `secret response must not be reported`, code)
		return
	}
	if code := f.failOnce[call]; code != 0 {
		delete(f.failOnce, call)
		http.Error(w, `secret response must not be reported`, code)
		return
	}
	write := func(v any) { w.Header().Set("Content-Type", "application/json"); _ = json.NewEncoder(w).Encode(v) }
	decode := func(v any) {
		if err := json.NewDecoder(r.Body).Decode(v); err != nil {
			panic(err)
		}
	}
	if r.URL.Path == "/storage/v1/b" {
		if r.URL.Query().Get("project") != "project-demo" {
			http.Error(w, "project", 403)
			return
		}
		if r.Method == "GET" {
			items := []*gcsBucket{}
			for name, b := range f.buckets {
				if strings.HasPrefix(name, r.URL.Query().Get("prefix")) && !f.foreign[name] {
					items = append(items, b)
				}
			}
			write(map[string]any{"items": items})
			return
		}
		var b gcsBucket
		decode(&b)
		if f.buckets[b.Name] != nil || f.foreign[b.Name] {
			http.Error(w, "exists", 409)
			return
		}
		b.Metageneration = "1"
		f.buckets[b.Name] = &b
		if f.nullCreate == "bucket" {
			f.nullCreate = ""
			write(nil)
			return
		}
		write(b)
		return
	}
	if path, ok := strings.CutPrefix(r.URL.Path, "/storage/v1/b/"); ok {
		name, tail, _ := strings.Cut(path, "/")
		b := f.buckets[name]
		if b == nil {
			http.NotFound(w, r)
			return
		}
		if tail == "" && r.Method == "GET" {
			write(b)
			return
		}
		if tail == "iam" {
			p := f.policies[name]
			if p.Etag == "" {
				p.Etag = "etag-1"
				p.Version = 3
			}
			if r.Method == "GET" {
				if r.URL.Query().Get("optionsRequestedPolicyVersion") != "3" {
					http.Error(w, "policy version", 400)
					return
				}
				write(p)
				return
			}
			var next gcsPolicy
			decode(&next)
			if f.policyConflicts > 0 {
				f.policyConflicts--
				http.Error(w, "conflict", 412)
				return
			}
			if next.Etag != p.Etag {
				http.Error(w, "etag", 412)
				return
			}
			f.policies[name] = next
			write(next)
			return
		}
		if tail == "o" && r.Method == "GET" {
			if r.URL.Query().Get("versions") != "true" {
				http.Error(w, "versions", 400)
				return
			}
			all := slices.Clone(f.objects[name])
			slices.SortFunc(all, func(a, b gcsObject) int { return strings.Compare(a.Name+"@"+a.Generation, b.Name+"@"+b.Generation) })
			if after := r.URL.Query().Get("pageToken"); after != "" {
				all = slices.DeleteFunc(all, func(o gcsObject) bool { return o.Name+"@"+o.Generation <= after })
			}
			var next string
			if len(all) > 1 {
				all = all[:1]
				next = all[0].Name + "@" + all[0].Generation
			}
			write(map[string]any{"items": all, "nextPageToken": next})
			return
		}
		if key, ok := strings.CutPrefix(tail, "o/"); ok && r.Method == "DELETE" {
			gen := r.URL.Query().Get("generation")
			if gen == "" || r.URL.Query().Get("ifGenerationMatch") != gen {
				http.Error(w, "generation", 400)
				return
			}
			f.objects[name] = slices.DeleteFunc(f.objects[name], func(o gcsObject) bool { return o.Name == key && o.Generation == gen })
			w.WriteHeader(204)
			return
		}
		if tail == "" && r.Method == "PATCH" {
			if r.URL.Query().Get("ifMetagenerationMatch") != b.Metageneration {
				http.Error(w, "metageneration", 412)
				return
			}
			var patch gcsBucket
			decode(&patch)
			b.IAM = patch.IAM
			g, _ := strconv.Atoi(b.Metageneration)
			b.Metageneration = strconv.Itoa(g + 1)
			write(b)
			return
		}
		if tail == "" && r.Method == "DELETE" {
			if len(f.objects[name]) > 0 || r.URL.Query().Get("ifMetagenerationMatch") != b.Metageneration {
				http.Error(w, "not empty or changed", 409)
				return
			}
			delete(f.buckets, name)
			w.WriteHeader(204)
			return
		}
	}
	if r.URL.Path == "/v1/projects/project-demo/serviceAccounts" && r.Method == "POST" {
		var in struct {
			AccountID string     `json:"accountId"`
			Account   gcsAccount `json:"serviceAccount"`
		}
		decode(&in)
		email := in.AccountID + "@project-demo.iam.gserviceaccount.com"
		if f.accounts[email] != nil {
			http.Error(w, "exists", 409)
			return
		}
		in.Account.Email = email
		in.Account.Name = "projects/project-demo/serviceAccounts/" + email
		in.Account.ProjectID = "project-demo"
		f.accounts[email] = &in.Account
		if f.nullCreate == "account" {
			f.nullCreate = ""
			write(nil)
			return
		}
		write(in.Account)
		return
	}
	if email, ok := strings.CutPrefix(r.URL.Path, "/v1/projects/project-demo/serviceAccounts/"); ok {
		a := f.accounts[email]
		if a == nil {
			http.NotFound(w, r)
			return
		}
		if r.Method == "DELETE" {
			delete(f.accounts, email)
			w.WriteHeader(204)
			return
		}
		write(a)
		return
	}
	if path, ok := strings.CutPrefix(r.URL.Path, "/storage/v1/projects/project-demo/hmacKeys"); ok {
		id := strings.TrimPrefix(path, "/")
		if id != "" {
			k, ok := f.keys[id]
			if !ok {
				http.NotFound(w, r)
				return
			}
			switch r.Method {
			case "GET":
				write(k)
			case "PUT":
				var v gcsKey
				decode(&v)
				if v.State != "INACTIVE" || v.Etag != k.Etag {
					http.Error(w, "invalid state/etag", 400)
					return
				}
				k.State = v.State
				f.keys[id] = k
				write(k)
			case "DELETE":
				if k.State != "INACTIVE" {
					http.Error(w, "must deactivate", 400)
					return
				}
				delete(f.keys, id)
				w.WriteHeader(204)
			}
			return
		}
		if r.Method == "POST" {
			f.keyN++
			id := fmt.Sprintf("access-%d", f.keyN)
			k := gcsKey{AccessID: id, State: "ACTIVE", ServiceAccountEmail: r.URL.Query().Get("serviceAccountEmail"), Etag: "key-etag", TimeCreated: fmt.Sprintf("2026-11-05T01:02:%02dZ", f.keyN)}
			f.keys[id] = k
			secret := "one-time-secret"
			if f.missingSecretOnce {
				f.missingSecretOnce = false
				secret = ""
			}
			write(map[string]any{"metadata": k, "secret": secret})
			return
		}
		items := []gcsKey{}
		for _, k := range f.keys {
			if k.ServiceAccountEmail == r.URL.Query().Get("serviceAccountEmail") {
				items = append(items, k)
			}
		}
		slices.SortFunc(items, func(a, b gcsKey) int { return strings.Compare(a.AccessID, b.AccessID) })
		if after := r.URL.Query().Get("pageToken"); after != "" {
			items = slices.DeleteFunc(items, func(k gcsKey) bool { return k.AccessID <= after })
		}
		var next string
		if len(items) > 1 {
			items = items[:1]
			next = items[0].AccessID
		}
		write(map[string]any{"items": items, "nextPageToken": next})
		return
	}
	http.Error(w, "unexpected "+call, 400)
}

func TestGCSWaitsForNewAccountIAMPropagation(t *testing.T) {
	d, f := newFakeGCS(t)
	in := Instance{App: "shop", Name: "shop"}
	call := "PUT /storage/v1/b/" + d.Bucket(in.Name) + "/iam"
	f.failOnce = map[string]int{call: 400}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	got, err := d.Ensure(ctx, in, true, "", "")
	if err != nil || got.Key == nil {
		t.Fatalf("new account did not become usable: %+v, %v", got, err)
	}
	if n := strings.Count(strings.Join(f.calls, "\n"), call); n != 2 {
		t.Fatalf("IAM PUT attempts = %d, want 2", n)
	}
	if len(f.accounts) != 1 || len(f.keys) != 1 {
		t.Fatalf("resources: %d accounts, %d keys", len(f.accounts), len(f.keys))
	}
}

func TestGCSDoesNotRetryUnrelatedIAMErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing bool
		conflict bool
		method   string
		status   int
	}{
		{"existing account", true, false, "PUT", 400},
		{"create conflict", true, true, "PUT", 400},
		{"forbidden", false, false, "PUT", 403},
		{"server error", false, false, "PUT", 500},
		{"read error", false, false, "GET", 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, f := newFakeGCS(t)
			in := Instance{App: "shop", Name: "shop"}
			bucket := d.Bucket(in.Name)
			if tc.existing {
				if _, err := d.Ensure(t.Context(), in, true, "", ""); err != nil {
					t.Fatal(err)
				}
				delete(f.policies, bucket)
				f.calls = nil
			}
			if tc.conflict {
				_, email := d.account(in)
				f.failOnce = map[string]int{"GET /v1/projects/project-demo/serviceAccounts/" + email: 404}
			}
			call := tc.method + " /storage/v1/b/" + bucket + "/iam"
			f.fail[call] = tc.status
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, err := d.Ensure(ctx, in, true, "", "")
			if gcsStatus(err) != tc.status || strings.Contains(err.Error(), "secret response") {
				t.Fatalf("IAM error = %v, want sanitized HTTP %d", err, tc.status)
			}
			if n := strings.Count(strings.Join(f.calls, "\n"), call); n != 1 {
				t.Fatalf("IAM attempts = %d, want 1", n)
			}
		})
	}
}

type gcsRoundTripperFunc func(*http.Request) (*http.Response, error)

func (f gcsRoundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestGCSCancelsAccountPropagationWait(t *testing.T) {
	d, f := newFakeGCS(t)
	in := Instance{App: "shop", Name: "shop"}
	call := "PUT /storage/v1/b/" + d.Bucket(in.Name) + "/iam"
	f.fail[call] = 400
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	failed := make(chan struct{})
	transport := d.client.Transport
	d.client.Transport = gcsRoundTripperFunc(func(r *http.Request) (*http.Response, error) {
		res, err := transport.RoundTrip(r)
		if r.Method+" "+r.URL.Path == call && res != nil && res.StatusCode == 400 {
			close(failed)
			<-ctx.Done()
		}
		return res, err
	})
	finished := make(chan error, 1)
	go func() {
		_, err := d.Ensure(ctx, in, true, "", "")
		finished <- err
	}()
	select {
	case <-failed:
	case err := <-finished:
		t.Fatalf("ensure returned before IAM failure: %v", err)
	}
	cancel()
	err := <-finished
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("propagation wait = %v, want context cancellation", err)
	}
	if n := strings.Count(strings.Join(f.calls, "\n"), call); n != 1 {
		t.Fatalf("IAM attempts = %d, want 1 before cancellation", n)
	}
	if len(f.keys) != 0 {
		t.Fatal("created an HMAC key after cancellation")
	}
}

func TestGCSCreateKeepAndRotate(t *testing.T) {
	d, f := newFakeGCS(t)
	ctx := context.Background()
	in := Instance{App: "shop", Name: "shop-preview"}
	got, err := d.Ensure(ctx, in, true, "", "")
	if err != nil || got.Key == nil || got.Key.Secret != "one-time-secret" || got.Bucket != "hc-gke-shop-preview" {
		t.Fatalf("create: %+v, %v", got, err)
	}
	b := f.buckets[got.Bucket]
	if b.Location != "asia-northeast3" || b.Labels[gcsLabelApp] != in.App || b.Labels[gcsLabelInstance] != in.Name || !b.IAM.Uniform.Enabled || b.IAM.PublicAccessPrevention != "enforced" {
		t.Fatalf("unsafe bucket: %+v", b)
	}
	if len(f.accounts) != 1 || len(f.keys) != 1 {
		t.Fatalf("resources: %d accounts, %d keys", len(f.accounts), len(f.keys))
	}
	kept, err := d.Ensure(ctx, in, false, got.Key.ID, "")
	if err != nil || kept.Key != nil || len(f.keys) != 1 {
		t.Fatalf("keep: %+v,%v", kept, err)
	}
	rotated, err := d.Ensure(ctx, in, true, got.Key.ID, "")
	if err != nil || rotated.Key == nil || rotated.Key.ID == got.Key.ID || len(f.keys) != 2 {
		t.Fatalf("rotate: %+v,%v", rotated, err)
	}
	before := len(f.calls)
	_, err = d.Ensure(ctx, in, true, rotated.Key.ID, got.Key.ID)
	if !errors.Is(err, ErrKeyLimit) {
		t.Fatalf("limit: %v", err)
	}
	for _, call := range f.calls[before:] {
		if strings.HasPrefix(call, "POST ") || strings.HasPrefix(call, "PUT ") || strings.HasPrefix(call, "PATCH ") || strings.HasPrefix(call, "DELETE ") {
			t.Fatalf("limit mutated: %s", call)
		}
	}
	if err = d.DeleteKey(ctx, in, got.Key.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := f.keys[got.Key.ID]; ok {
		t.Fatal("old key retained")
	}
	if err = d.DeleteKey(ctx, in, got.Key.ID); err != nil {
		t.Fatalf("repeat key deletion: %v", err)
	}
}

// TestGCSKeys: Keys reads the service account's HMAC keys with their
// timeCreated without changing anything, and Scope is the roles Ensure binds.
func TestGCSKeys(t *testing.T) {
	d, f := newFakeGCS(t)
	ctx := context.Background()
	in := Instance{App: "shop", Name: "shop-preview"}
	keys, err := d.Keys(ctx, in)
	if err != nil || len(keys) != 0 {
		t.Fatalf("keys of a missing account = %v, %v", keys, err)
	}
	got, err := d.Ensure(ctx, in, true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.calls)
	keys, err = d.Keys(ctx, in)
	if err != nil || len(keys) != 1 || keys[0].ID != got.Key.ID || !keys[0].CreatedAt.Equal(time.Date(2026, 11, 5, 1, 2, 1, 0, time.UTC)) || !keys[0].Active {
		t.Fatalf("keys = %+v, %v", keys, err)
	}
	k := f.keys[got.Key.ID]
	k.State = "INACTIVE"
	f.keys[got.Key.ID] = k
	if keys, err := d.Keys(ctx, in); err != nil || len(keys) != 1 || keys[0].Active {
		t.Fatalf("an inactive key = %+v, %v", keys, err)
	}
	for _, call := range f.calls[before:] {
		if !strings.HasPrefix(call, "GET ") {
			t.Fatalf("Keys changed something: %s", call)
		}
	}
	_, email := d.account(in)
	var bound []string
	for _, b := range f.policies[got.Bucket].Bindings {
		if slices.Contains(b.Members, "serviceAccount:"+email) {
			bound = append(bound, b.Role)
		}
	}
	if s := d.Scope(); !slices.Equal(s.Actions, bound) || s.Boundary != "" {
		t.Fatalf("scope %+v, the bucket binds %v", s, bound)
	}
	f.fail["GET /storage/v1/projects/project-demo/hmacKeys"] = 403
	if _, err := d.Keys(ctx, in); gcsStatus(err) != 403 || strings.Contains(err.Error(), "secret") {
		t.Fatalf("keys with the list denied: %v", err)
	}
}

func TestGCSDeleteVersionsAndRetry(t *testing.T) {
	d, f := newFakeGCS(t)
	ctx := context.Background()
	in := Instance{App: "shop", Name: "shop"}
	got, err := d.Ensure(ctx, in, true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	f.objects[got.Bucket] = []gcsObject{{Name: "dir/a #?.jpg", Generation: "1"}, {Name: "dir/a #?.jpg", Generation: "2"}, {Name: "z", Generation: "3"}}
	if err = d.Delete(ctx, in); err != nil {
		t.Fatal(err)
	}
	if len(f.accounts) != 0 || len(f.keys) != 0 || len(f.buckets) != 0 || len(f.objects[got.Bucket]) != 0 {
		t.Fatalf("resources remain: %+v", f)
	}
	if err = d.Delete(ctx, in); err != nil {
		t.Fatalf("repeat delete: %v", err)
	}
}

func TestGCSRefusesForeignResourcesWithoutChanges(t *testing.T) {
	for name, change := range map[string]func(*gcsDriver, *fakeGCS, Instance, string){
		"another app bucket": func(_ *gcsDriver, f *fakeGCS, _ Instance, bucket string) {
			f.buckets[bucket].Labels[gcsLabelApp] = "other"
		},
		"unlabelled bucket": func(_ *gcsDriver, f *fakeGCS, _ Instance, bucket string) { f.buckets[bucket].Labels = nil },
		"another instance bucket": func(_ *gcsDriver, f *fakeGCS, _ Instance, bucket string) {
			f.buckets[bucket].Labels[gcsLabelInstance] = "other"
		},
		"another project bucket": func(_ *gcsDriver, f *fakeGCS, _ Instance, bucket string) { f.foreign[bucket] = true },
		"another app account": func(d *gcsDriver, f *fakeGCS, in Instance, _ string) {
			_, email := d.account(in)
			f.accounts[email].Description = "other app"
		},
		"another project account": func(d *gcsDriver, f *fakeGCS, in Instance, _ string) {
			_, email := d.account(in)
			f.accounts[email].ProjectID = "other-project"
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, f := newFakeGCS(t)
			ctx := context.Background()
			in := Instance{App: "shop", Name: "shop"}
			got, err := d.Ensure(ctx, in, true, "", "")
			if err != nil {
				t.Fatal(err)
			}
			change(d, f, in, got.Bucket)
			before := len(f.calls)
			if _, err = d.Ensure(ctx, in, true, got.Key.ID, ""); !errors.Is(err, ErrInUse) {
				t.Fatalf("ensure: %v", err)
			}
			if err = d.Delete(ctx, in); !errors.Is(err, ErrInUse) {
				t.Fatalf("delete: %v", err)
			}
			assertGCSNoMutation(t, f.calls[before:])
		})
	}
}

func assertGCSNoMutation(t *testing.T, calls []string) {
	t.Helper()
	for _, call := range calls {
		if !strings.HasPrefix(call, "GET ") {
			t.Fatalf("unexpected mutation: %s", call)
		}
	}
}

func TestGCSPreservesIAMConditionsAndRetriesEtag(t *testing.T) {
	d, f := newFakeGCS(t)
	ctx := context.Background()
	in := Instance{App: "shop", Name: "shop"}
	got, err := d.Ensure(ctx, in, true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	condition := json.RawMessage(`{"title":"temporary","expression":"request.time < timestamp('2030-01-01T00:00:00Z')"}`)
	sameCondition := func(raw json.RawMessage) bool {
		var before, after any
		return json.Unmarshal(condition, &before) == nil && json.Unmarshal(raw, &after) == nil && reflect.DeepEqual(before, after)
	}
	f.policies[got.Bucket] = gcsPolicy{Version: 3, Etag: "existing-etag", Bindings: []gcsBinding{
		{Role: "roles/storage.objectAdmin", Members: []string{"user:owner@example.com"}, Condition: condition},
		{Role: "roles/storage.objectAdmin", Members: []string{"user:another@example.com"}},
	}}
	f.policyConflicts = 2
	if _, err = d.Ensure(ctx, in, false, got.Key.ID, ""); err != nil {
		t.Fatal(err)
	}
	p := f.policies[got.Bucket]
	if p.Version != 3 || p.Etag != "existing-etag" || len(p.Bindings) != 3 || !sameCondition(p.Bindings[0].Condition) || !slices.Equal(p.Bindings[0].Members, []string{"user:owner@example.com"}) {
		t.Fatalf("IAM lost: %+v", p)
	}
	_, email := d.account(in)
	if !slices.Contains(p.Bindings[1].Members, "user:another@example.com") || !slices.Contains(p.Bindings[1].Members, "serviceAccount:"+email) {
		t.Fatalf("unconditional members: %+v", p)
	}
	if err = d.Delete(ctx, in); err != nil {
		t.Fatal(err)
	}
	p = f.policies[got.Bucket]
	if len(p.Bindings) != 2 || len(p.Bindings[1].Members) != 1 || p.Bindings[1].Members[0] != "user:another@example.com" || !sameCondition(p.Bindings[0].Condition) {
		t.Fatalf("delete changed unrelated policy: %+v", p)
	}
}

func TestGCSRecoversOrphanKeysAndMissingCurrent(t *testing.T) {
	d, f := newFakeGCS(t)
	ctx := context.Background()
	in := Instance{App: "shop", Name: "shop"}
	f.missingSecretOnce = true
	if _, err := d.Ensure(ctx, in, true, "", ""); err == nil {
		t.Fatal("accepted key without a secret")
	}
	if len(f.keys) != 1 {
		t.Fatal("fake did not create the lost key")
	}
	got, err := d.Ensure(ctx, in, true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(f.keys) != 1 || got.Key == nil || got.Key.ID != "access-2" {
		t.Fatalf("orphan not replaced: %+v, keys %+v", got, f.keys)
	}
	delete(f.keys, got.Key.ID)
	restored, err := d.Ensure(ctx, in, false, got.Key.ID, "")
	if err != nil || restored.Key == nil || restored.Key.ID == got.Key.ID {
		t.Fatalf("missing current not replaced: %+v,%v", restored, err)
	}
	k := f.keys[restored.Key.ID]
	k.State = "INACTIVE"
	f.keys[k.AccessID] = k
	again, err := d.Ensure(ctx, in, false, k.AccessID, "")
	if err != nil || again.Key == nil || f.keys[k.AccessID].State != "INACTIVE" {
		t.Fatalf("inactive current: %+v,%v", again, err)
	}
}

func TestGCSDeletesAllOrphanKeyPages(t *testing.T) {
	d, f := newFakeGCS(t)
	ctx := context.Background()
	in := Instance{App: "shop", Name: "shop"}
	got, err := d.Ensure(ctx, in, true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	_, email := d.account(in)
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("orphan-%d", i)
		f.keys[id] = gcsKey{AccessID: id, State: "ACTIVE", ServiceAccountEmail: email, Etag: "x"}
	}
	next, err := d.Ensure(ctx, in, true, got.Key.ID, "")
	if err != nil || next.Key == nil || len(f.keys) != 2 {
		t.Fatalf("orphan pagination: %+v,%v, keys=%+v", next, err, f.keys)
	}
}

func TestGCSDeleteKeyRefusesForeignKeyAndAccount(t *testing.T) {
	d, f := newFakeGCS(t)
	ctx := context.Background()
	in := Instance{App: "shop", Name: "shop"}
	got, err := d.Ensure(ctx, in, true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	f.keys["foreign-key"] = gcsKey{AccessID: "foreign-key", State: "ACTIVE", ServiceAccountEmail: "other@project-demo.iam.gserviceaccount.com", Etag: "x"}
	before := len(f.calls)
	if err = d.DeleteKey(ctx, in, "foreign-key"); !errors.Is(err, ErrInUse) {
		t.Fatalf("foreign key: %v", err)
	}
	_, email := d.account(in)
	f.accounts[email].Description = "foreign"
	if err = d.DeleteKey(ctx, in, got.Key.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("foreign account: %v", err)
	}
	assertGCSNoMutation(t, f.calls[before:])
}

func TestGCSDeleteResumesAfterStatePropagation(t *testing.T) {
	d, f := newFakeGCS(t)
	ctx := context.Background()
	in := Instance{App: "shop", Name: "shop"}
	got, err := d.Ensure(ctx, in, true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	call := "DELETE /storage/v1/projects/project-demo/hmacKeys/" + got.Key.ID
	f.fail[call] = 400
	err = d.Delete(ctx, in)
	if err == nil || strings.Contains(err.Error(), "secret response") || f.keys[got.Key.ID].State != "INACTIVE" || len(f.buckets) != 1 || len(f.accounts) != 1 {
		t.Fatalf("partial cleanup: %v, %+v", err, f)
	}
	delete(f.fail, call)
	if err = d.Delete(ctx, in); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(f.keys) != 0 || len(f.buckets) != 0 || len(f.accounts) != 0 {
		t.Fatal("retry left resources")
	}
}

func TestGCSMakesExistingOwnedBucketPrivate(t *testing.T) {
	d, f := newFakeGCS(t)
	ctx := context.Background()
	in := Instance{App: "shop", Name: "shop"}
	got, err := d.Ensure(ctx, in, true, "", "")
	if err != nil {
		t.Fatal(err)
	}
	f.buckets[got.Bucket].IAM = nil
	if _, err = d.Ensure(ctx, in, false, got.Key.ID, ""); err != nil {
		t.Fatal(err)
	}
	b := f.buckets[got.Bucket]
	if b.IAM == nil || !b.IAM.Uniform.Enabled || b.IAM.PublicAccessPrevention != "enforced" {
		t.Fatalf("not private: %+v", b)
	}
	b.Location = "US"
	b.LocationType = "multi-region"
	before := len(f.calls)
	if _, err = d.Ensure(ctx, in, true, got.Key.ID, ""); err == nil {
		t.Fatal("accepted wrong-region bucket")
	}
	assertGCSNoMutation(t, f.calls[before:])
}

func TestGCSAccountIDsAreDeterministicAndWithinIAMLimit(t *testing.T) {
	d, _ := newFakeGCS(t)
	for _, name := range []string{"a", strings.Repeat("longname", 6)} {
		id, email := d.account(Instance{App: "shop", Name: name})
		if len(id) < 6 || len(id) > 30 || email != id+"@project-demo.iam.gserviceaccount.com" {
			t.Fatalf("invalid account: %q %q", id, email)
		}
		again, _ := d.account(Instance{App: "shop", Name: name})
		if again != id {
			t.Fatal("non-deterministic account")
		}
	}
}

func TestGCSRejectsNullCreateResponseAndResumes(t *testing.T) {
	for _, resource := range []string{"account", "bucket"} {
		t.Run(resource, func(t *testing.T) {
			d, f := newFakeGCS(t)
			in := Instance{App: "shop", Name: "shop"}
			f.nullCreate = resource
			if _, err := d.Ensure(context.Background(), in, true, "", ""); err == nil {
				t.Fatal("accepted null create response")
			}
			got, err := d.Ensure(context.Background(), in, true, "", "")
			if err != nil || got.Key == nil || len(f.keys) != 1 {
				t.Fatalf("resume: %+v, %v", got, err)
			}
		})
	}
}
