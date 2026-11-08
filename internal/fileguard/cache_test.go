package fileguard

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

func TestContentCacheConcurrentAccessAndCopiesAreIsolated(t *testing.T) {
	cache := NewClassificationCache(2)
	key := sha256.Sum256([]byte("fixture"))
	cache.put(classification{key: key, labels: []string{"kr-only"}, findings: map[string]int{"email": 1}})
	var pending sync.WaitGroup
	for range 20 {
		pending.Go(func() {
			v, ok := cache.get(key)
			if !ok {
				t.Error("entry disappeared")
				return
			}
			v.labels[0] = "global"
			v.findings["email"] = 0
		})
	}
	pending.Wait()
	v, _ := cache.get(key)
	if v.labels[0] != "kr-only" || v.findings["email"] != 1 {
		t.Fatal("caller mutated cached classification")
	}
}

func cacheCounts(t *testing.T, output string) (int, int) {
	t.Helper()
	var evidence struct {
		Cache struct{ Hits, Misses int } `json:"inspectionCache"`
	}
	if err := json.Unmarshal([]byte(output), &evidence); err != nil {
		t.Fatal(err)
	}
	return evidence.Cache.Hits, evidence.Cache.Misses
}

func TestLocalRcloneSuccessfulReturnMigrationStillReadsAndVerifies(t *testing.T) {
	if _, err := exec.LookPath("rclone"); err != nil {
		t.Skip("rclone is not installed")
	}
	a, b := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(a, "body.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	cache := NewClassificationCache(8)
	scope := strings.Repeat("a", 64)
	authorizations := 0
	guard := func(context.Context, Report) error { authorizations++; return nil }
	forward := New(a, b, nil, "", func(context.Context) error { return nil }, guard).WithClassificationCache(cache, scope)
	first, err := forward.Sync(context.Background(), a, b)
	if err != nil {
		t.Fatal(err)
	}
	if hits, misses := cacheCounts(t, first.Output); hits != 0 || misses != 1 {
		t.Fatal("initial copy did not inspect")
	}
	// New Syncer, new successful run, opposite direction; not an engine rollback.
	back := New(b, a, nil, "", func(context.Context) error { return nil }, guard).WithClassificationCache(cache, scope)
	second, err := back.Sync(context.Background(), b, a)
	if err != nil {
		t.Fatal(err)
	}
	if hits, misses := cacheCounts(t, second.Output); hits != 1 || misses != 0 {
		t.Fatal("return move did not reuse exact classification")
	}
	if authorizations != 2 {
		t.Fatal("old approval replaced current policy evaluation")
	}
	if err := os.WriteFile(filepath.Join(b, "body.txt"), []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	third, err := back.Sync(context.Background(), b, a)
	if err != nil {
		t.Fatal(err)
	}
	if hits, misses := cacheCounts(t, third.Output); hits != 0 || misses != 1 {
		t.Fatal("same-size remote change was not downloaded and inspected")
	}
	if data, err := os.ReadFile(filepath.Join(a, "body.txt")); err != nil || string(data) != "other" {
		t.Fatal("new source bytes were not sent")
	}
	if err := os.Remove(filepath.Join(b, "body.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b, "new.txt"), []byte("added"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := back.Sync(context.Background(), b, a); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a, "body.txt")); !os.IsNotExist(err) {
		t.Fatal("deleted source object survived from cached inventory")
	}
	if data, err := os.ReadFile(filepath.Join(a, "new.txt")); err != nil || string(data) != "added" {
		t.Fatal("new source object was omitted")
	}
	// Warm classification is never approval. Re-evaluate the current policy
	// even when every byte is a cache hit, before any destination mutation.
	back.authorize = func(context.Context, Report) error { return ErrDenied }
	uploads := 0
	execute := back.execute
	back.execute = func(ctx context.Context, out io.Writer, args ...string) error {
		if args[0] == "sync" {
			uploads++
		}
		return execute(ctx, out, args...)
	}
	denied, err := back.Sync(context.Background(), b, a)
	if !errors.Is(err, ErrDenied) || uploads != 0 {
		t.Fatal("old approval bypassed changed authorization")
	}
	if hits, _ := cacheCounts(t, denied.Output); hits != 1 {
		t.Fatal("denial did not exercise a warm classification")
	}
}

func cachedScan(t *testing.T, cache *ClassificationCache, binding, path string, data []byte, reviews []Review, binary string) (Report, *classificationMemo, error) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, path), data, 0600); err != nil {
		t.Fatal(err)
	}
	memo := newClassificationMemo(cache, binding, reviews, binary)
	r, err := scanEachCached(context.Background(), dir, func(visit func(object) error) error { return visit(object{Path: path, Size: int64(len(data))}) }, reviews, binary, memo)
	return r, memo, err
}

func TestContentCacheReusesOnlyExactBytesAndPolicyScope(t *testing.T) {
	cache := NewClassificationCache(8)
	scope := strings.Repeat("a", 64)
	first, m1, err := cachedScan(t, cache, scope, "body.txt", []byte("hello"), nil, "")
	if err != nil || m1.hits != 0 || m1.misses != 1 {
		t.Fatalf("first inspection: %+v %v", m1, err)
	}
	second, m2, err := cachedScan(t, cache, scope, "body.txt", []byte("hello"), nil, "")
	if err != nil || m2.hits != 1 || second.ManifestDigest != first.ManifestDigest {
		t.Fatalf("exact content was not reused: %+v %v", m2, err)
	}
	changed, m3, err := cachedScan(t, cache, scope, "body.txt", []byte("other"), nil, "")
	if err != nil || m3.hits != 0 || changed.ManifestDigest == first.ManifestDigest {
		t.Fatal("same-size content change reused old result")
	}
	_, m4, err := cachedScan(t, cache, strings.Repeat("b", 64), "body.txt", []byte("hello"), nil, "")
	if err != nil || m4.hits != 0 {
		t.Fatal("another app/policy/environment scope reused evidence")
	}
	_, binaryMemo, err := cachedScan(t, cache, scope, "body.txt", []byte("hello"), nil, "kr-only")
	if err != nil || binaryMemo.hits != 0 {
		t.Fatal("binary classification policy change reused evidence")
	}
	digest := sha256.Sum256([]byte("hello"))
	review := []Review{{SHA256: hex.EncodeToString(digest[:]), Label: "kr-only"}}
	r, m5, err := cachedScan(t, cache, scope, "body.txt", []byte("hello"), review, "")
	if err != nil || m5.hits != 0 || !slices.Contains(r.Labels, "kr-only") {
		t.Fatal("review policy revision was ignored")
	}
}

func TestContentCacheStillInspectsCurrentObjectPath(t *testing.T) {
	cache := NewClassificationCache(8)
	scope := strings.Repeat("a", 64)
	_, _, err := cachedScan(t, cache, scope, "clean.txt", []byte("hello"), nil, "")
	if err != nil {
		t.Fatal(err)
	}
	r, memo, err := cachedScan(t, cache, scope, "demo@example.invalid", []byte("hello"), nil, "")
	if err != nil || memo.hits != 1 || r.Findings["email"] != 1 || !slices.Contains(r.Labels, "kr-only") {
		t.Fatal("cached body hid a sensitive new key")
	}
}

func TestCachedArchivesConsumeExpansionAndMemberBudgets(t *testing.T) {
	cache := NewClassificationCache(8)
	scope := strings.Repeat("a", 64)
	data := archive(t, "records.txt", []byte("demo@example.invalid"), nil)
	_, _, err := cachedScan(t, cache, scope, "data.zip", data, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(data)
	for _, which := range []string{"bytes", "members", "cancelled"} {
		t.Run(which, func(t *testing.T) {
			s := scanner{reviews: map[string]string{}, findings: map[string]int{}}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if which == "bytes" {
				s.expanded = MaxExpandedBytes
			}
			if which == "members" {
				s.members = MaxObjects
			}
			if which == "cancelled" {
				cancel()
			}
			_, err := newClassificationMemo(cache, scope, nil, "").inspect(ctx, &s, data, hex.EncodeToString(digest[:]))
			if which == "cancelled" {
				if !errors.Is(err, ErrRead) {
					t.Fatalf("%v", err)
				}
			} else if !errors.Is(err, ErrLimit) {
				t.Fatalf("cached archive bypassed %s limit: %v", which, err)
			}
		})
	}
	r, memo, err := cachedScan(t, cache, scope, "data.zip", data, nil, "")
	if err != nil || memo.hits != 1 || r.Findings["email"] != 1 {
		t.Fatal("cached archive lost nested DLP findings")
	}
}

func TestContentCacheEvictsAndNeverCachesFailures(t *testing.T) {
	cache := NewClassificationCache(2)
	scope := strings.Repeat("a", 64)
	for _, data := range []string{"one", "two", "three"} {
		if _, _, err := cachedScan(t, cache, scope, "body.txt", []byte(data), nil, ""); err != nil {
			t.Fatal(err)
		}
	}
	_, memo, err := cachedScan(t, cache, scope, "body.txt", []byte("one"), nil, "")
	if err != nil || memo.hits != 0 || cache.Len() != 2 {
		t.Fatal("LRU capacity or eviction failed")
	}
	for range 2 {
		_, memo, err := cachedScan(t, cache, scope, "body.txt", []byte{0xff, 0x00}, nil, "")
		if !errors.Is(err, ErrUnsupported) || memo.hits != 0 {
			t.Fatal("failed inspection was reused")
		}
	}
}
