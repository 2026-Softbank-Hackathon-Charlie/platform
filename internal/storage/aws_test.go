package storage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

func apiErr(code string) error { return &smithy.GenericAPIError{Code: code, Message: code} }

// fakeAWS is S3 and IAM in memory: one account and one region.
type fakeAWS struct {
	mu      sync.Mutex
	buckets map[string]*fakeBucket
	foreign map[string]bool // bucket names other accounts own
	users   map[string]*fakeUser
	keyN    int
	calls   []string
	fail    map[string]error // operation -> error
	page    int              // ListObjectVersions page size
}

type fakeBucket struct {
	region       string
	tags         map[string]string
	public       *s3types.PublicAccessBlockConfiguration
	versions     []string // "key@version"
	markers      []string
	uploads      []string
	deleteCalls  int
	maxBatchSize int
}

type fakeUser struct {
	path, boundary string
	tags           map[string]string
	policies       map[string]string
	keys           []string
}

func newFakeAWS() *fakeAWS {
	return &fakeAWS{buckets: map[string]*fakeBucket{}, foreign: map[string]bool{}, users: map[string]*fakeUser{}, fail: map[string]error{}, page: 2}
}

func (f *fakeAWS) call(op string) error {
	f.calls = append(f.calls, op)
	return f.fail[op]
}

func (f *fakeAWS) CreateBucket(_ context.Context, in *s3.CreateBucketInput, _ ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("CreateBucket"); err != nil {
		return nil, err
	}
	name := aws.ToString(in.Bucket)
	if f.foreign[name] {
		return nil, apiErr("BucketAlreadyExists")
	}
	if _, ok := f.buckets[name]; ok {
		return nil, apiErr("BucketAlreadyOwnedByYou")
	}
	region := "us-east-1"
	if in.CreateBucketConfiguration != nil {
		region = string(in.CreateBucketConfiguration.LocationConstraint)
	}
	f.buckets[name] = &fakeBucket{region: region}
	return &s3.CreateBucketOutput{}, nil
}

func (f *fakeAWS) bucket(in *string) (*fakeBucket, error) {
	b, ok := f.buckets[aws.ToString(in)]
	if !ok {
		return nil, apiErr("NoSuchBucket")
	}
	return b, nil
}

func (f *fakeAWS) GetBucketTagging(_ context.Context, in *s3.GetBucketTaggingInput, _ ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GetBucketTagging"); err != nil {
		return nil, err
	}
	b, err := f.bucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	if b.tags == nil {
		return nil, apiErr("NoSuchTagSet")
	}
	out := &s3.GetBucketTaggingOutput{}
	for k, v := range b.tags {
		out.TagSet = append(out.TagSet, s3types.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out, nil
}

func (f *fakeAWS) PutBucketTagging(_ context.Context, in *s3.PutBucketTaggingInput, _ ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("PutBucketTagging"); err != nil {
		return nil, err
	}
	b, err := f.bucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	b.tags = map[string]string{}
	for _, t := range in.Tagging.TagSet {
		b.tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return &s3.PutBucketTaggingOutput{}, nil
}

func (f *fakeAWS) PutPublicAccessBlock(_ context.Context, in *s3.PutPublicAccessBlockInput, _ ...func(*s3.Options)) (*s3.PutPublicAccessBlockOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("PutPublicAccessBlock"); err != nil {
		return nil, err
	}
	b, err := f.bucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	b.public = in.PublicAccessBlockConfiguration
	return &s3.PutPublicAccessBlockOutput{}, nil
}

func splitVersion(s string) (string, string) {
	k, v, _ := strings.Cut(s, "@")
	return k, v
}

func (f *fakeAWS) ListObjectVersions(_ context.Context, in *s3.ListObjectVersionsInput, _ ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("ListObjectVersions"); err != nil {
		return nil, err
	}
	b, err := f.bucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	// Pages by position: the marker is the index of the next entry.
	all := append(slices.Clone(b.versions), b.markers...)
	start := 0
	if in.KeyMarker != nil {
		start, _ = strconv.Atoi(aws.ToString(in.KeyMarker))
	}
	end := min(start+f.page, len(all))
	out := &s3.ListObjectVersionsOutput{}
	for i := start; i < end; i++ {
		k, v := splitVersion(all[i])
		if i < len(b.versions) {
			out.Versions = append(out.Versions, s3types.ObjectVersion{Key: aws.String(k), VersionId: aws.String(v)})
		} else {
			out.DeleteMarkers = append(out.DeleteMarkers, s3types.DeleteMarkerEntry{Key: aws.String(k), VersionId: aws.String(v)})
		}
	}
	if end < len(all) {
		// Deleting a page shifts the rest, so the next page starts where
		// this one started.
		out.IsTruncated = aws.Bool(true)
		out.NextKeyMarker, out.NextVersionIdMarker = aws.String(strconv.Itoa(start)), aws.String("x")
	}
	return out, nil
}

func (f *fakeAWS) DeleteObjects(_ context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("DeleteObjects"); err != nil {
		return nil, err
	}
	b, err := f.bucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	b.deleteCalls++
	b.maxBatchSize = max(b.maxBatchSize, len(in.Delete.Objects))
	for _, o := range in.Delete.Objects {
		id := aws.ToString(o.Key) + "@" + aws.ToString(o.VersionId)
		b.versions = slices.DeleteFunc(b.versions, func(s string) bool { return s == id })
		b.markers = slices.DeleteFunc(b.markers, func(s string) bool { return s == id })
	}
	return &s3.DeleteObjectsOutput{}, nil
}

func (f *fakeAWS) ListMultipartUploads(_ context.Context, in *s3.ListMultipartUploadsInput, _ ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("ListMultipartUploads"); err != nil {
		return nil, err
	}
	b, err := f.bucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	out := &s3.ListMultipartUploadsOutput{}
	for _, u := range b.uploads {
		k, id := splitVersion(u)
		out.Uploads = append(out.Uploads, s3types.MultipartUpload{Key: aws.String(k), UploadId: aws.String(id)})
	}
	return out, nil
}

func (f *fakeAWS) AbortMultipartUpload(_ context.Context, in *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("AbortMultipartUpload"); err != nil {
		return nil, err
	}
	b, err := f.bucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	id := aws.ToString(in.Key) + "@" + aws.ToString(in.UploadId)
	b.uploads = slices.DeleteFunc(b.uploads, func(s string) bool { return s == id })
	return &s3.AbortMultipartUploadOutput{}, nil
}

func (f *fakeAWS) DeleteBucket(_ context.Context, in *s3.DeleteBucketInput, _ ...func(*s3.Options)) (*s3.DeleteBucketOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("DeleteBucket"); err != nil {
		return nil, err
	}
	b, err := f.bucket(in.Bucket)
	if err != nil {
		return nil, err
	}
	if len(b.versions)+len(b.markers)+len(b.uploads) > 0 {
		return nil, apiErr("BucketNotEmpty")
	}
	delete(f.buckets, aws.ToString(in.Bucket))
	return &s3.DeleteBucketOutput{}, nil
}

func (f *fakeAWS) user(name *string) (*fakeUser, error) {
	u, ok := f.users[aws.ToString(name)]
	if !ok {
		return nil, apiErr("NoSuchEntity")
	}
	return u, nil
}

func (f *fakeAWS) CreateUser(_ context.Context, in *iam.CreateUserInput, _ ...func(*iam.Options)) (*iam.CreateUserOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("CreateUser"); err != nil {
		return nil, err
	}
	name := aws.ToString(in.UserName)
	if _, ok := f.users[name]; ok {
		return nil, apiErr("EntityAlreadyExists")
	}
	u := &fakeUser{path: aws.ToString(in.Path), boundary: aws.ToString(in.PermissionsBoundary), tags: map[string]string{}, policies: map[string]string{}}
	for _, t := range in.Tags {
		u.tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	f.users[name] = u
	return &iam.CreateUserOutput{}, nil
}

func (f *fakeAWS) GetUser(_ context.Context, in *iam.GetUserInput, _ ...func(*iam.Options)) (*iam.GetUserOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("GetUser"); err != nil {
		return nil, err
	}
	u, err := f.user(in.UserName)
	if err != nil {
		return nil, err
	}
	out := &iam.GetUserOutput{User: &iamtypes.User{UserName: in.UserName, Path: aws.String(u.path)}}
	for k, v := range u.tags {
		out.User.Tags = append(out.User.Tags, iamtypes.Tag{Key: aws.String(k), Value: aws.String(v)})
	}
	return out, nil
}

func (f *fakeAWS) PutUserPolicy(_ context.Context, in *iam.PutUserPolicyInput, _ ...func(*iam.Options)) (*iam.PutUserPolicyOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("PutUserPolicy"); err != nil {
		return nil, err
	}
	u, err := f.user(in.UserName)
	if err != nil {
		return nil, err
	}
	u.policies[aws.ToString(in.PolicyName)] = aws.ToString(in.PolicyDocument)
	return &iam.PutUserPolicyOutput{}, nil
}

func (f *fakeAWS) DeleteUserPolicy(_ context.Context, in *iam.DeleteUserPolicyInput, _ ...func(*iam.Options)) (*iam.DeleteUserPolicyOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("DeleteUserPolicy"); err != nil {
		return nil, err
	}
	u, err := f.user(in.UserName)
	if err != nil {
		return nil, err
	}
	if _, ok := u.policies[aws.ToString(in.PolicyName)]; !ok {
		return nil, apiErr("NoSuchEntity")
	}
	delete(u.policies, aws.ToString(in.PolicyName))
	return &iam.DeleteUserPolicyOutput{}, nil
}

func (f *fakeAWS) ListAccessKeys(_ context.Context, in *iam.ListAccessKeysInput, _ ...func(*iam.Options)) (*iam.ListAccessKeysOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("ListAccessKeys"); err != nil {
		return nil, err
	}
	u, err := f.user(in.UserName)
	if err != nil {
		return nil, err
	}
	out := &iam.ListAccessKeysOutput{}
	for _, k := range u.keys {
		out.AccessKeyMetadata = append(out.AccessKeyMetadata, iamtypes.AccessKeyMetadata{AccessKeyId: aws.String(k), CreateDate: aws.Time(fakeKeyMade)})
	}
	return out, nil
}

// fakeKeyMade is the CreateDate of every fake access key.
var fakeKeyMade = time.Date(2026, 11, 4, 8, 21, 52, 0, time.UTC)

func (f *fakeAWS) CreateAccessKey(_ context.Context, in *iam.CreateAccessKeyInput, _ ...func(*iam.Options)) (*iam.CreateAccessKeyOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("CreateAccessKey"); err != nil {
		return nil, err
	}
	u, err := f.user(in.UserName)
	if err != nil {
		return nil, err
	}
	if len(u.keys) >= 2 {
		return nil, apiErr("LimitExceeded")
	}
	f.keyN++
	id := fmt.Sprintf("AKIA%04d", f.keyN)
	u.keys = append(u.keys, id)
	return &iam.CreateAccessKeyOutput{AccessKey: &iamtypes.AccessKey{AccessKeyId: aws.String(id), SecretAccessKey: aws.String("secret-" + id)}}, nil
}

func (f *fakeAWS) DeleteAccessKey(_ context.Context, in *iam.DeleteAccessKeyInput, _ ...func(*iam.Options)) (*iam.DeleteAccessKeyOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("DeleteAccessKey"); err != nil {
		return nil, err
	}
	u, err := f.user(in.UserName)
	if err != nil {
		return nil, err
	}
	id := aws.ToString(in.AccessKeyId)
	if !slices.Contains(u.keys, id) {
		return nil, apiErr("NoSuchEntity")
	}
	u.keys = slices.DeleteFunc(u.keys, func(k string) bool { return k == id })
	return &iam.DeleteAccessKeyOutput{}, nil
}

func (f *fakeAWS) DeleteUser(_ context.Context, in *iam.DeleteUserInput, _ ...func(*iam.Options)) (*iam.DeleteUserOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.call("DeleteUser"); err != nil {
		return nil, err
	}
	u, err := f.user(in.UserName)
	if err != nil {
		return nil, err
	}
	if len(u.keys) > 0 || len(u.policies) > 0 {
		return nil, apiErr("DeleteConflict")
	}
	delete(f.users, aws.ToString(in.UserName))
	return &iam.DeleteUserOutput{}, nil
}

func newAWSDriver(f *fakeAWS) *awsDriver {
	c := validAWS()
	return &awsDriver{prefix: c.BucketPrefix, region: c.Region, path: c.IAMPath, boundary: c.PermissionsBoundary, s3: f, iam: f}
}

var dev = Instance{App: "moimroom", Name: "moimroom--dev"}

const (
	devBucket = "hopcharlie-eks-moimroom--dev"
	devUser   = "hopcharlie-app-moimroom--dev"
)

func TestAWSEnsure(t *testing.T) {
	f := newFakeAWS()
	d := newAWSDriver(f)
	ctx := t.Context()

	res, err := d.Ensure(ctx, dev, false, "", "")
	if err != nil {
		t.Fatal(err)
	}
	// A new user always gets a key.
	if res.Bucket != devBucket || res.Key == nil || res.Key.ID != "AKIA0001" || res.Key.Secret != "secret-AKIA0001" {
		t.Fatalf("result %+v %+v", res, res.Key)
	}
	b := f.buckets[devBucket]
	if b.region != "ap-northeast-2" || b.tags[TagApp] != "moimroom" || b.tags[TagInstance] != "moimroom--dev" {
		t.Fatalf("bucket %+v", b)
	}
	if p := b.public; p == nil || !*p.BlockPublicAcls || !*p.IgnorePublicAcls || !*p.BlockPublicPolicy || !*p.RestrictPublicBuckets {
		t.Fatalf("public access block %+v", p)
	}
	u := f.users[devUser]
	if u.path != "/hopcharlie-apps/" || u.boundary != "arn:aws:iam::123456789012:policy/hopcharlie-app-boundary" ||
		u.tags[TagApp] != "moimroom" || u.tags[TagInstance] != "moimroom--dev" || !slices.Equal(u.keys, []string{"AKIA0001"}) {
		t.Fatalf("user %+v", u)
	}
	var doc struct{ Statement []struct{ Resource string } }
	if err := json.Unmarshal([]byte(u.policies[policyName]), &doc); err != nil || len(doc.Statement) != 2 ||
		doc.Statement[0].Resource != "arn:aws:s3:::"+devBucket || doc.Statement[1].Resource != "arn:aws:s3:::"+devBucket+"/*" {
		t.Fatalf("policy %s (%v)", u.policies[policyName], err)
	}

	// Again: everything exists; no new key unless asked.
	res, err = d.Ensure(ctx, dev, false, "", "")
	if err != nil || res.Key != nil || res.Bucket != devBucket {
		t.Fatalf("second ensure %+v, %v", res, err)
	}
	if !slices.Equal(f.users[devUser].keys, []string{"AKIA0001"}) {
		t.Fatalf("keys %v", f.users[devUser].keys)
	}

	// A new key keeping the old one: rotation.
	res, err = d.Ensure(ctx, dev, true, "AKIA0001", "")
	if err != nil || res.Key == nil || res.Key.ID != "AKIA0002" {
		t.Fatalf("rotate %+v, %v", res, err)
	}
	if !slices.Equal(f.users[devUser].keys, []string{"AKIA0001", "AKIA0002"}) {
		t.Fatalf("keys after rotating %v", f.users[devUser].keys)
	}
	// Another new key while both may be in use: refused, nothing changes.
	_, err = d.Ensure(ctx, dev, true, "AKIA0002", "AKIA0001")
	if !errors.Is(err, ErrKeyLimit) || strings.Contains(err.Error(), "AKIA0001") || !strings.Contains(err.Error(), "...0001") {
		t.Fatalf("a third key: %v", err)
	}
	if !slices.Equal(f.users[devUser].keys, []string{"AKIA0001", "AKIA0002"}) {
		t.Fatalf("keys after a refused third key %v", f.users[devUser].keys)
	}
	must(t, d.DeleteKey(ctx, dev, "AKIA0001"))
	must(t, d.DeleteKey(ctx, dev, "AKIA0001")) // gone already
	if !slices.Equal(f.users[devUser].keys, []string{"AKIA0002"}) {
		t.Fatalf("keys after deleting the old one %v", f.users[devUser].keys)
	}

	// The key in use still exists: nothing changes. Gone: a new one.
	res, err = d.Ensure(ctx, dev, false, "AKIA0002", "")
	if err != nil || res.Key != nil {
		t.Fatalf("keep an existing key: %+v, %v", res, err)
	}
	must(t, d.DeleteKey(ctx, dev, "AKIA0002"))
	res, err = d.Ensure(ctx, dev, false, "AKIA0002", "")
	if err != nil || res.Key == nil || res.Key.ID == "AKIA0002" {
		t.Fatalf("keep a key that is gone: %+v, %v", res, err)
	}
	if !slices.Equal(f.users[devUser].keys, []string{res.Key.ID}) {
		t.Fatalf("keys after replacing a gone key %v", f.users[devUser].keys)
	}

	// A new key keeping none: a retry after a crash deletes keys nobody
	// knows, so the two-key limit is never hit.
	for i := range 3 {
		res, err = d.Ensure(ctx, dev, true, "", "")
		if err != nil || res.Key == nil {
			t.Fatalf("retry %d: %+v, %v", i, res, err)
		}
		if !slices.Equal(f.users[devUser].keys, []string{res.Key.ID}) {
			t.Fatalf("retry %d: keys %v", i, f.users[devUser].keys)
		}
	}

	// The key in use is gone and the old one may still be in use: a new
	// key beside the old one.
	old := res.Key.ID
	res, err = d.Ensure(ctx, dev, false, "AKIAGONE", old)
	if err != nil || res.Key == nil || !slices.Equal(f.users[devUser].keys, []string{old, res.Key.ID}) {
		t.Fatalf("a gone key with an old one: %+v, %v, keys %v", res, err, f.users[devUser].keys)
	}
}

// usEast1 is S3 in us-east-1: creating a bucket the account owns succeeds.
type usEast1 struct{ *fakeAWS }

func (f usEast1) CreateBucket(ctx context.Context, in *s3.CreateBucketInput, o ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	out, err := f.fakeAWS.CreateBucket(ctx, in, o...)
	if errorCode(err) == "BucketAlreadyOwnedByYou" {
		return &s3.CreateBucketOutput{}, nil
	}
	return out, err
}

// TestAWSKeys: Keys reads the user's keys with their CreateDate and changes
// nothing, and Scope is the policy Ensure writes.
func TestAWSKeys(t *testing.T) {
	f := newFakeAWS()
	d := newAWSDriver(f)
	ctx := t.Context()

	keys, err := d.Keys(ctx, dev)
	if err != nil || len(keys) != 0 {
		t.Fatalf("keys of a missing user = %v, %v", keys, err)
	}
	if _, err := d.Ensure(ctx, dev, false, "", ""); err != nil {
		t.Fatal(err)
	}
	f.calls = nil
	keys, err = d.Keys(ctx, dev)
	if err != nil || len(keys) != 1 || keys[0].ID != "AKIA0001" || !keys[0].CreatedAt.Equal(fakeKeyMade) {
		t.Fatalf("keys = %+v, %v", keys, err)
	}
	if !slices.Equal(f.calls, []string{"ListAccessKeys"}) {
		t.Fatalf("calls %v, want only ListAccessKeys", f.calls)
	}

	var doc struct{ Statement []struct{ Action []string } }
	if err := json.Unmarshal([]byte(f.users[devUser].policies[policyName]), &doc); err != nil {
		t.Fatal(err)
	}
	var written []string
	for _, st := range doc.Statement {
		written = append(written, st.Action...)
	}
	if s := d.Scope(); !slices.Equal(s.Actions, written) || s.Boundary != validAWS().PermissionsBoundary {
		t.Fatalf("scope %+v, the policy allows %v", s, written)
	}

	f.fail["ListAccessKeys"] = apiErr("AccessDenied")
	if _, err := d.Keys(ctx, dev); errorCode(err) != "AccessDenied" {
		t.Fatalf("keys with ListAccessKeys denied: %v", err)
	}
}

func TestAWSEnsureUSEast1(t *testing.T) {
	f := newFakeAWS()
	d := newAWSDriver(f)
	d.region, d.s3 = "us-east-1", usEast1{f}
	if _, err := d.Ensure(t.Context(), dev, false, "", ""); err != nil {
		t.Fatal(err)
	}
	if r := f.buckets[devBucket].region; r != "us-east-1" {
		t.Fatalf("region %s", r)
	}
	// Another app's bucket is still refused although the create succeeds.
	f.buckets[devBucket].tags[TagApp] = "other"
	if _, err := d.Ensure(t.Context(), dev, false, "", ""); !errors.Is(err, ErrInUse) {
		t.Fatalf("err = %v, want ErrInUse", err)
	}
	if f.buckets[devBucket].tags[TagApp] != "other" {
		t.Fatal("the other app's bucket was retagged")
	}
}

func TestAWSEnsureTakesOverLeftovers(t *testing.T) {
	f := newFakeAWS()
	d := newAWSDriver(f)
	// A crash left the bucket untagged and the user (tagged when created)
	// without a policy or key.
	f.buckets[devBucket] = &fakeBucket{region: "ap-northeast-2"}
	f.users[devUser] = &fakeUser{path: "/hopcharlie-apps/", tags: map[string]string{TagApp: "moimroom", TagInstance: "moimroom--dev"}, policies: map[string]string{}}
	res, err := d.Ensure(t.Context(), dev, true, "", "")
	if err != nil || res.Key == nil {
		t.Fatalf("%+v, %v", res, err)
	}
	if f.buckets[devBucket].tags[TagApp] != "moimroom" || f.users[devUser].policies[policyName] == "" {
		t.Fatalf("bucket %+v user %+v", f.buckets[devBucket], f.users[devUser])
	}
}

func TestAWSEnsureRefusesOthers(t *testing.T) {
	ctx := t.Context()
	cases := map[string]func(f *fakeAWS){
		"bucket of another app": func(f *fakeAWS) {
			f.buckets[devBucket] = &fakeBucket{tags: map[string]string{TagApp: "other"}}
		},
		"bucket of another account": func(f *fakeAWS) { f.foreign[devBucket] = true },
		"user of another app": func(f *fakeAWS) {
			f.users[devUser] = &fakeUser{path: "/hopcharlie-apps/", tags: map[string]string{TagApp: "other"}, policies: map[string]string{}}
		},
		"user on another path": func(f *fakeAWS) {
			f.users[devUser] = &fakeUser{path: "/", tags: map[string]string{TagApp: "moimroom"}, policies: map[string]string{}}
		},
		// CreateUser tags the user, so one without the tag is not ours.
		"untagged user": func(f *fakeAWS) {
			f.users[devUser] = &fakeUser{path: "/hopcharlie-apps/", tags: map[string]string{}, policies: map[string]string{}}
		},
	}
	for name, setup := range cases {
		f := newFakeAWS()
		setup(f)
		_, err := newAWSDriver(f).Ensure(ctx, dev, true, "", "")
		if !errors.Is(err, ErrInUse) {
			t.Errorf("%s: err = %v, want ErrInUse", name, err)
		}
		if u := f.users[devUser]; u != nil && len(u.keys) > 0 {
			t.Errorf("%s: a key was made", name)
		}
	}
}

func TestAWSEnsureFailures(t *testing.T) {
	for _, op := range []string{"CreateBucket", "PutPublicAccessBlock", "PutBucketTagging", "CreateUser", "PutUserPolicy", "ListAccessKeys", "CreateAccessKey"} {
		f := newFakeAWS()
		f.fail[op] = errors.New("boom")
		if _, err := newAWSDriver(f).Ensure(t.Context(), dev, true, "", ""); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("%s fails: err = %v", op, err)
		}
		// The retry succeeds.
		delete(f.fail, op)
		if res, err := newAWSDriver(f).Ensure(t.Context(), dev, true, "", ""); err != nil || res.Key == nil {
			t.Errorf("retry after %s: %+v, %v", op, res, err)
		}
	}
}

func TestAWSDelete(t *testing.T) {
	f := newFakeAWS()
	d := newAWSDriver(f)
	ctx := t.Context()
	if _, err := d.Ensure(ctx, dev, false, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Ensure(ctx, dev, true, "AKIA0001", ""); err != nil {
		t.Fatal(err)
	}
	b := f.buckets[devBucket]
	for i := range 5 {
		b.versions = append(b.versions, fmt.Sprintf("photos/%d.jpg@v%d", i, i))
	}
	b.markers = []string{"photos/9.jpg@m1"}
	b.uploads = []string{"big.bin@u1"}
	other := Instance{App: "moimroom", Name: "moimroom"}
	if _, err := d.Ensure(ctx, other, false, "", ""); err != nil {
		t.Fatal(err)
	}

	must(t, d.Delete(ctx, dev))
	if _, ok := f.buckets[devBucket]; ok {
		t.Fatal("bucket kept")
	}
	if _, ok := f.users[devUser]; ok {
		t.Fatal("user kept")
	}
	if b.deleteCalls < 3 || b.maxBatchSize > f.page {
		t.Fatalf("objects deleted in %d calls of at most %d, want pages of %d", b.deleteCalls, b.maxBatchSize, f.page)
	}
	// The other instance is untouched.
	if _, ok := f.buckets["hopcharlie-eks-moimroom"]; !ok {
		t.Fatal("another instance's bucket deleted")
	}
	// Deleting again finds nothing and succeeds.
	must(t, d.Delete(ctx, dev))
}

func TestAWSDeleteResumes(t *testing.T) {
	f := newFakeAWS()
	d := newAWSDriver(f)
	ctx := t.Context()
	if _, err := d.Ensure(ctx, dev, false, "", ""); err != nil {
		t.Fatal(err)
	}
	f.buckets[devBucket].versions = []string{"a@1"}
	f.fail["DeleteBucket"] = errors.New("boom")
	if err := d.Delete(ctx, dev); err == nil {
		t.Fatal("no error")
	}
	if _, ok := f.users[devUser]; ok {
		t.Fatal("the user goes before the bucket")
	}
	delete(f.fail, "DeleteBucket")
	must(t, d.Delete(ctx, dev))
	if len(f.buckets) != 0 {
		t.Fatal("bucket kept")
	}
}

func TestAWSDeleteRefusesOthers(t *testing.T) {
	f := newFakeAWS()
	d := newAWSDriver(f)
	ctx := t.Context()
	if _, err := d.Ensure(ctx, dev, false, "", ""); err != nil {
		t.Fatal(err)
	}
	f.buckets[devBucket].tags[TagApp] = "other"
	if err := d.Delete(ctx, dev); !errors.Is(err, ErrInUse) {
		t.Fatalf("err = %v, want ErrInUse", err)
	}
	if _, ok := f.users[devUser]; !ok {
		t.Fatal("the user was deleted although the bucket is another app's")
	}
	// A bucket without the app tag is not deleted either.
	f.buckets[devBucket].tags = map[string]string{}
	f.buckets[devBucket].versions = []string{"keep@1"}
	if err := d.Delete(ctx, dev); !errors.Is(err, ErrInUse) {
		t.Fatalf("untagged bucket: err = %v, want ErrInUse", err)
	}
	if len(f.buckets[devBucket].versions) != 1 {
		t.Fatal("an untagged bucket was emptied")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
