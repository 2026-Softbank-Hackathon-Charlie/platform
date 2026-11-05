//go:build minio

package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/minio/madmin-go/v3"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/tags"
)

// fakeMinIO is a MinIO server in memory, answering with the error codes
// MinIO does.
type fakeMinIO struct {
	mu       sync.Mutex
	buckets  map[string]*fakeMinIOBucket
	policies map[string]string
	users    map[string]string            // user -> attached policy
	accounts map[string]map[string]string // user -> service account -> secret
	n        int
	fail     map[string]error
}

type fakeMinIOBucket struct {
	region  string
	tags    map[string]string
	objects []string
	uploads []string // keys of unfinished multipart uploads
}

func newFakeMinIO() *fakeMinIO {
	return &fakeMinIO{buckets: map[string]*fakeMinIOBucket{}, policies: map[string]string{}, users: map[string]string{},
		accounts: map[string]map[string]string{}, fail: map[string]error{}}
}

func s3Err(code string) error    { return minio.ErrorResponse{Code: code, Message: code} }
func adminErr(code string) error { return madmin.ErrorResponse{Code: code, Message: code} }

func (f *fakeMinIO) MakeBucket(_ context.Context, bucket string, opts minio.MakeBucketOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail["MakeBucket"]; err != nil {
		return err
	}
	if _, ok := f.buckets[bucket]; ok {
		return s3Err("BucketAlreadyOwnedByYou")
	}
	f.buckets[bucket] = &fakeMinIOBucket{region: opts.Region}
	return nil
}

func (f *fakeMinIO) GetBucketTagging(_ context.Context, bucket string) (*tags.Tags, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.buckets[bucket]
	if !ok {
		return nil, s3Err("NoSuchBucket")
	}
	if b.tags == nil {
		return nil, s3Err("NoSuchTagSet")
	}
	return tags.NewTags(b.tags, false)
}

func (f *fakeMinIO) SetBucketTagging(_ context.Context, bucket string, t *tags.Tags) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.buckets[bucket]
	if !ok {
		return s3Err("NoSuchBucket")
	}
	b.tags = t.ToMap()
	return nil
}

func (f *fakeMinIO) ListObjects(_ context.Context, bucket string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.buckets[bucket]
	n := 1
	if ok {
		n += len(b.objects)
	}
	ch := make(chan minio.ObjectInfo, n)
	defer close(ch)
	if !ok {
		ch <- minio.ObjectInfo{Err: s3Err("NoSuchBucket")}
		return ch
	}
	if !opts.Recursive || !opts.WithVersions {
		ch <- minio.ObjectInfo{Err: errors.New("fake: list recursively with versions")}
		return ch
	}
	for _, o := range b.objects {
		k, v := splitVersion(o)
		ch <- minio.ObjectInfo{Key: k, VersionID: v}
	}
	return ch
}

func (f *fakeMinIO) RemoveObjects(_ context.Context, bucket string, objects <-chan minio.ObjectInfo, _ minio.RemoveObjectsOptions) <-chan minio.RemoveObjectError {
	errs := make(chan minio.RemoveObjectError)
	go func() {
		defer close(errs)
		for o := range objects {
			f.mu.Lock()
			fail := f.fail["RemoveObjects"]
			if fail == nil {
				id := o.Key + "@" + o.VersionID
				b := f.buckets[bucket]
				b.objects = slices.DeleteFunc(b.objects, func(s string) bool { return s == id })
			}
			f.mu.Unlock()
			if fail != nil {
				errs <- minio.RemoveObjectError{ObjectName: o.Key, Err: fail}
			}
		}
	}()
	return errs
}

func (f *fakeMinIO) ListIncompleteUploads(_ context.Context, bucket, _ string, _ bool) <-chan minio.ObjectMultipartInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.buckets[bucket]
	n := 1
	if ok {
		n += len(b.uploads)
	}
	ch := make(chan minio.ObjectMultipartInfo, n)
	defer close(ch)
	if !ok {
		ch <- minio.ObjectMultipartInfo{Err: s3Err("NoSuchBucket")}
		return ch
	}
	for _, k := range b.uploads {
		ch <- minio.ObjectMultipartInfo{Key: k}
	}
	return ch
}

func (f *fakeMinIO) RemoveIncompleteUpload(_ context.Context, bucket, object string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.buckets[bucket]
	if !ok {
		return s3Err("NoSuchBucket")
	}
	b.uploads = slices.DeleteFunc(b.uploads, func(k string) bool { return k == object })
	return nil
}

func (f *fakeMinIO) RemoveBucket(_ context.Context, bucket string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.buckets[bucket]
	if !ok {
		return s3Err("NoSuchBucket")
	}
	if len(b.objects)+len(b.uploads) > 0 {
		return s3Err("BucketNotEmpty")
	}
	delete(f.buckets, bucket)
	return nil
}

func (f *fakeMinIO) AddCannedPolicy(_ context.Context, name string, policy []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.policies[name] = string(policy)
	return nil
}

func (f *fakeMinIO) RemoveCannedPolicy(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.policies[name]; !ok {
		return adminErr("XMinioAdminNoSuchPolicy")
	}
	for u, p := range f.users {
		if p == name {
			return adminErr("XMinioAdminPolicyInUse:" + u)
		}
	}
	delete(f.policies, name)
	return nil
}

func (f *fakeMinIO) GetUserInfo(_ context.Context, name string) (madmin.UserInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.users[name]
	if !ok {
		return madmin.UserInfo{}, adminErr("XMinioAdminNoSuchUser")
	}
	return madmin.UserInfo{PolicyName: p, Status: madmin.AccountEnabled}, nil
}

func (f *fakeMinIO) AddUser(_ context.Context, accessKey, secretKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail["AddUser"]; err != nil {
		return err
	}
	if len(secretKey) < 8 {
		return adminErr("XMinioAdminInvalidSecretKey")
	}
	f.users[accessKey] = ""
	return nil
}

func (f *fakeMinIO) SetPolicy(_ context.Context, policyName, entityName string, isGroup bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.policies[policyName]; !ok || isGroup {
		return adminErr("XMinioAdminNoSuchPolicy")
	}
	if _, ok := f.users[entityName]; !ok {
		return adminErr("XMinioAdminNoSuchUser")
	}
	f.users[entityName] = policyName
	return nil
}

func (f *fakeMinIO) RemoveUser(_ context.Context, accessKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.users[accessKey]; !ok {
		return adminErr("XMinioAdminNoSuchUser")
	}
	delete(f.users, accessKey)
	delete(f.accounts, accessKey)
	return nil
}

func (f *fakeMinIO) AddServiceAccount(_ context.Context, req madmin.AddServiceAccountReq) (madmin.Credentials, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail["AddServiceAccount"]; err != nil {
		return madmin.Credentials{}, err
	}
	if _, ok := f.users[req.TargetUser]; !ok {
		return madmin.Credentials{}, adminErr("XMinioAdminNoSuchUser")
	}
	f.n++
	id := fmt.Sprintf("SVC%04d", f.n)
	if f.accounts[req.TargetUser] == nil {
		f.accounts[req.TargetUser] = map[string]string{}
	}
	f.accounts[req.TargetUser][id] = "secret-" + id
	return madmin.Credentials{AccessKey: id, SecretKey: "secret-" + id}, nil
}

func (f *fakeMinIO) ListServiceAccounts(_ context.Context, user string) (madmin.ListServiceAccountsResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out madmin.ListServiceAccountsResp
	for id := range f.accounts[user] {
		out.Accounts = append(out.Accounts, madmin.ServiceAccountInfo{AccessKey: id, ParentUser: user})
	}
	return out, nil
}

func (f *fakeMinIO) InfoServiceAccount(_ context.Context, accessKey string) (madmin.InfoServiceAccountResp, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for user, accts := range f.accounts {
		if _, ok := accts[accessKey]; ok {
			return madmin.InfoServiceAccountResp{ParentUser: user}, nil
		}
	}
	return madmin.InfoServiceAccountResp{}, adminErr("XMinioInvalidIAMCredentials")
}

func (f *fakeMinIO) DeleteServiceAccount(_ context.Context, accessKey string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, accts := range f.accounts {
		if _, ok := accts[accessKey]; ok {
			delete(accts, accessKey)
			return nil
		}
	}
	return adminErr("XMinioInvalidIAMCredentials")
}

func (f *fakeMinIO) keys(user string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for id := range f.accounts[user] {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

func newMinIODriver(f *fakeMinIO) *minioDriver {
	return &minioDriver{prefix: "hopcharlie-op-", region: "us-east-1", s3: f, admin: f}
}

const (
	devMinIOBucket = "hopcharlie-op-moimroom--dev"
)

func TestMinIOConfig(t *testing.T) {
	m := validMinIO()
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
	m.Region, m.Endpoint = "us-east-1", "https://minio.example.com/"
	if err := m.Validate(); err != nil {
		t.Errorf("minio with region and https: %v", err)
	}
	env := map[string]string{"MINIO_ADMIN_ACCESS_KEY": "admin", "MINIO_ADMIN_SECRET_KEY": "secret-key"}
	d, err := New(t.Context(), validMinIO(), func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if d.Name() != DriverMinIO || d.Bucket("moimroom--dev") != "hopcharlie-op-moimroom--dev" {
		t.Fatalf("driver %s, bucket %s", d.Name(), d.Bucket("moimroom--dev"))
	}
	delete(env, "MINIO_ADMIN_SECRET_KEY")
	_, err = New(t.Context(), validMinIO(), func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), "MINIO_ADMIN_SECRET_KEY") || strings.Contains(err.Error(), "secret-key") {
		t.Fatalf("missing secret key: %v", err)
	}
}

func TestMinIOEnsure(t *testing.T) {
	f := newFakeMinIO()
	d := newMinIODriver(f)
	ctx := t.Context()

	res, err := d.Ensure(ctx, dev, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.Bucket != devMinIOBucket || res.Key == nil || res.Key.ID != "SVC0001" || res.Key.Secret != "secret-SVC0001" {
		t.Fatalf("result %+v %+v", res, res.Key)
	}
	b := f.buckets[devMinIOBucket]
	if b.region != "us-east-1" || b.tags[TagApp] != "moimroom" || b.tags[TagInstance] != "moimroom--dev" {
		t.Fatalf("bucket %+v", b)
	}
	if f.users[devUser] != devUser || !strings.Contains(f.policies[devUser], `"arn:aws:s3:::`+devMinIOBucket+`/*"`) {
		t.Fatalf("user policy %q, policy %s", f.users[devUser], f.policies[devUser])
	}

	res, err = d.Ensure(ctx, dev, false, "", "")
	if err != nil || res.Key != nil {
		t.Fatalf("second ensure %+v, %v", res, err)
	}
	res, err = d.Ensure(ctx, dev, true, "SVC0001", "")
	if err != nil || res.Key == nil || res.Key.ID != "SVC0002" {
		t.Fatalf("rotate %+v, %v", res, err)
	}
	if k := f.keys(devUser); !slices.Equal(k, []string{"SVC0001", "SVC0002"}) {
		t.Fatalf("keys after rotating %v", k)
	}
	// Two keys at most, like AWS: refused while both may be in use.
	if _, err := d.Ensure(ctx, dev, true, "SVC0002", "SVC0001"); !errors.Is(err, ErrKeyLimit) {
		t.Fatalf("a third key: %v", err)
	}
	if k := f.keys(devUser); !slices.Equal(k, []string{"SVC0001", "SVC0002"}) {
		t.Fatalf("keys after a refused third key %v", k)
	}
	must(t, d.DeleteKey(ctx, dev, "SVC0001"))
	must(t, d.DeleteKey(ctx, dev, "SVC0001"))
	// Another instance's key is not deleted through this one.
	other := Instance{App: "moimroom", Name: "moimroom"}
	resOther, err := d.Ensure(ctx, other, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteKey(ctx, dev, resOther.Key.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("deleting another instance's key: %v", err)
	}
	if k := f.keys(UserPrefix + "moimroom"); !slices.Equal(k, []string{resOther.Key.ID}) {
		t.Fatalf("the other instance's keys %v", k)
	}
	// The key in use still exists: nothing changes. Gone: a new one.
	if res, err := d.Ensure(ctx, dev, false, "SVC0002", ""); err != nil || res.Key != nil {
		t.Fatalf("keep an existing key: %+v, %v", res, err)
	}
	must(t, d.DeleteKey(ctx, dev, "SVC0002"))
	if res, err := d.Ensure(ctx, dev, false, "SVC0002", ""); err != nil || res.Key == nil {
		t.Fatalf("keep a key that is gone: %+v, %v", res, err)
	}
	res, err = d.Ensure(ctx, dev, true, "", "")
	if err != nil || res.Key == nil {
		t.Fatalf("retry %+v, %v", res, err)
	}
	if k := f.keys(devUser); !slices.Equal(k, []string{res.Key.ID}) {
		t.Fatalf("keys after a retry %v", k)
	}
}

// TestMinIOKeys: Keys lists the user's service accounts without a creation
// time, which MinIO does not record.
func TestMinIOKeys(t *testing.T) {
	f := newFakeMinIO()
	d := newMinIODriver(f)
	ctx := t.Context()
	keys, err := d.Keys(ctx, dev)
	if err != nil || len(keys) != 0 {
		t.Fatalf("keys of a missing user = %v, %v", keys, err)
	}
	if _, err := d.Ensure(ctx, dev, false, "", ""); err != nil {
		t.Fatal(err)
	}
	keys, err = d.Keys(ctx, dev)
	if err != nil || len(keys) != 1 || keys[0].ID != "SVC0001" || !keys[0].CreatedAt.IsZero() {
		t.Fatalf("keys = %+v, %v", keys, err)
	}
	if s := d.Scope(); !slices.Equal(s.Actions, slices.Concat(bucketActions, objectActions)) || s.Boundary != "" {
		t.Fatalf("scope %+v", s)
	}
}

func TestMinIOEnsureRefusesAnotherApp(t *testing.T) {
	f := newFakeMinIO()
	f.buckets[devMinIOBucket] = &fakeMinIOBucket{tags: map[string]string{TagApp: "other"}}
	if _, err := newMinIODriver(f).Ensure(t.Context(), dev, true, "", ""); !errors.Is(err, ErrInUse) {
		t.Fatalf("err = %v, want ErrInUse", err)
	}
	if len(f.users) != 0 {
		t.Fatal("a user was made")
	}
	// An untagged one (a crash between making and tagging) is taken over.
	f.buckets[devMinIOBucket] = &fakeMinIOBucket{}
	if _, err := newMinIODriver(f).Ensure(t.Context(), dev, true, "", ""); err != nil {
		t.Fatal(err)
	}
}

func TestMinIOEnsureFailures(t *testing.T) {
	for _, op := range []string{"MakeBucket", "AddUser", "AddServiceAccount"} {
		f := newFakeMinIO()
		f.fail[op] = errors.New("boom")
		if _, err := newMinIODriver(f).Ensure(t.Context(), dev, true, "", ""); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("%s fails: err = %v", op, err)
		}
		delete(f.fail, op)
		if res, err := newMinIODriver(f).Ensure(t.Context(), dev, true, "", ""); err != nil || res.Key == nil {
			t.Errorf("retry after %s: %+v, %v", op, res, err)
		}
	}
}

func TestMinIODelete(t *testing.T) {
	f := newFakeMinIO()
	d := newMinIODriver(f)
	ctx := t.Context()
	if _, err := d.Ensure(ctx, dev, false, "", ""); err != nil {
		t.Fatal(err)
	}
	for i := range 250 {
		f.buckets[devMinIOBucket].objects = append(f.buckets[devMinIOBucket].objects, fmt.Sprintf("photos/%d.jpg@v1", i))
	}
	f.buckets[devMinIOBucket].uploads = []string{"big.bin"}
	must(t, d.Delete(ctx, dev))
	if len(f.buckets) != 0 || len(f.users) != 0 || len(f.policies) != 0 || len(f.accounts) != 0 {
		t.Fatalf("left %d buckets, %d users, %d policies, %d accounts", len(f.buckets), len(f.users), len(f.policies), len(f.accounts))
	}
	must(t, d.Delete(ctx, dev))

	// A failed removal is reported, and a retry finishes.
	if _, err := d.Ensure(ctx, dev, false, "", ""); err != nil {
		t.Fatal(err)
	}
	f.buckets[devMinIOBucket].objects = []string{"a@1", "b@1"}
	f.fail["RemoveObjects"] = errors.New("boom")
	if err := d.Delete(ctx, dev); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v", err)
	}
	delete(f.fail, "RemoveObjects")
	must(t, d.Delete(ctx, dev))
	if len(f.buckets) != 0 {
		t.Fatal("bucket kept")
	}

	// Another app's bucket, or one without the app tag, is not deleted.
	for _, tags := range []map[string]string{{TagApp: "other"}, {}} {
		f.buckets[devMinIOBucket] = &fakeMinIOBucket{tags: tags, objects: []string{"keep@1"}}
		if err := d.Delete(ctx, dev); !errors.Is(err, ErrInUse) {
			t.Fatalf("tags %v: err = %v, want ErrInUse", tags, err)
		}
		if len(f.buckets[devMinIOBucket].objects) != 1 {
			t.Fatalf("tags %v: objects deleted", tags)
		}
	}
}

// TestMinIOLive runs the driver against a real MinIO server, the same one
// the kind E2E runs: set PLATFORM_TEST_MINIO_ENDPOINT (http://host:port),
// PLATFORM_TEST_MINIO_ACCESS_KEY and PLATFORM_TEST_MINIO_SECRET_KEY. It
// checks that the key reaches its bucket only.
func TestMinIOLive(t *testing.T) {
	endpoint := os.Getenv("PLATFORM_TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("PLATFORM_TEST_MINIO_ENDPOINT is not set")
	}
	c := Config{Driver: DriverMinIO, BucketPrefix: "hctest-", Endpoint: endpoint,
		AdminAccessKeyEnv: "PLATFORM_TEST_MINIO_ACCESS_KEY", AdminSecretKeyEnv: "PLATFORM_TEST_MINIO_SECRET_KEY"}
	d, err := New(t.Context(), c, os.Getenv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1e8)
	a := Instance{App: "live", Name: "live--a" + suffix}
	b := Instance{App: "live", Name: "live--b" + suffix}
	t.Cleanup(func() {
		_ = d.Delete(context.Background(), a)
		_ = d.Delete(context.Background(), b)
	})

	resA, err := d.Ensure(ctx, a, false, "", "")
	if err != nil || resA.Key == nil {
		t.Fatalf("ensure a: %+v, %v", resA, err)
	}
	resB, err := d.Ensure(ctx, b, false, "", "")
	if err != nil || resB.Key == nil {
		t.Fatalf("ensure b: %+v, %v", resB, err)
	}
	again, err := d.Ensure(ctx, a, false, "", "")
	if err != nil || again.Key != nil {
		t.Fatalf("ensure a again: %+v, %v", again, err)
	}

	host, secure, _ := c.minioEndpoint()
	asApp := func(k *Key) *minio.Client {
		cl, err := minio.New(host, &minio.Options{Creds: credentials.NewStaticV4(k.ID, k.Secret, ""), Secure: secure})
		if err != nil {
			t.Fatal(err)
		}
		return cl
	}
	appA := asApp(resA.Key)
	// The key may take a moment to be usable on every node.
	var putErr error
	for range 20 {
		_, putErr = appA.PutObject(ctx, resA.Bucket, "hello.txt", strings.NewReader("hi"), 2, minio.PutObjectOptions{})
		if putErr == nil {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if putErr != nil {
		t.Fatalf("a's key cannot write its bucket: %v", putErr)
	}
	if _, err := appA.StatObject(ctx, resA.Bucket, "hello.txt", minio.StatObjectOptions{}); err != nil {
		t.Fatalf("a's key cannot read its bucket: %v", err)
	}
	if _, err := appA.PutObject(ctx, resB.Bucket, "x.txt", strings.NewReader("no"), 2, minio.PutObjectOptions{}); minio.ToErrorResponse(err).Code != "AccessDenied" {
		t.Fatalf("a's key wrote b's bucket: %v", err)
	}
	// MinIO lists only the buckets a key may use.
	if list, err := appA.ListBuckets(ctx); err == nil {
		for _, bk := range list {
			if bk.Name != resA.Bucket {
				t.Fatalf("a's key lists bucket %s", bk.Name)
			}
		}
	} else if minio.ToErrorResponse(err).Code != "AccessDenied" {
		t.Fatalf("a's key lists buckets: %v", err)
	}
	if err := appA.MakeBucket(ctx, "hctest-other"+suffix, minio.MakeBucketOptions{}); err == nil {
		t.Fatal("a's key made a bucket")
	}

	// Rotation: the new key works, then the old one is deleted.
	rot, err := d.Ensure(ctx, a, true, resA.Key.ID, "")
	if err != nil || rot.Key == nil {
		t.Fatalf("rotate: %+v, %v", rot, err)
	}
	if _, err := asApp(rot.Key).StatObject(ctx, rot.Bucket, "hello.txt", minio.StatObjectOptions{}); err != nil {
		t.Fatalf("the new key: %v", err)
	}
	if _, err := d.Ensure(ctx, a, true, rot.Key.ID, resA.Key.ID); !errors.Is(err, ErrKeyLimit) {
		t.Fatalf("a third key while two may be in use: %v", err)
	}
	must(t, d.DeleteKey(ctx, a, resA.Key.ID))
	must(t, d.DeleteKey(ctx, a, resA.Key.ID))
	if err := d.DeleteKey(ctx, a, resB.Key.ID); !errors.Is(err, ErrInUse) {
		t.Fatalf("deleting b's key through a: %v", err)
	}
	if _, err := appA.StatObject(ctx, resA.Bucket, "hello.txt", minio.StatObjectOptions{}); err == nil {
		t.Fatal("the deleted key still reads")
	}
	// An unfinished multipart upload does not keep the bucket.
	core := minio.Core{Client: asApp(rot.Key)}
	if _, err := core.NewMultipartUpload(ctx, rot.Bucket, "big.bin", minio.PutObjectOptions{}); err != nil {
		t.Fatalf("start a multipart upload: %v", err)
	}

	// Delete empties and removes the bucket and the user, and is
	// idempotent.
	must(t, d.Delete(ctx, a))
	must(t, d.Delete(ctx, a))
	admin, _ := minio.New(host, &minio.Options{Creds: credentials.NewStaticV4(os.Getenv(c.AdminAccessKeyEnv), os.Getenv(c.AdminSecretKeyEnv), ""), Secure: secure})
	if ok, err := admin.BucketExists(ctx, resA.Bucket); err != nil || ok {
		t.Fatalf("bucket a exists %v (%v) after Delete", ok, err)
	}
	if ok, err := admin.BucketExists(ctx, resB.Bucket); err != nil || !ok {
		t.Fatalf("bucket b exists %v (%v); deleting a must not touch it", ok, err)
	}
}
