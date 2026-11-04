//go:build minio

package storage

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"

	"github.com/minio/madmin-go/v3"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/minio/minio-go/v7/pkg/tags"
)

// MinIOBuilt reports whether this binary has the minio driver: it is built
// with -tags minio.
const MinIOBuilt = true

// minioS3 is the part of the MinIO S3 client the minio driver uses.
type minioS3 interface {
	MakeBucket(ctx context.Context, bucket string, opts minio.MakeBucketOptions) error
	GetBucketTagging(ctx context.Context, bucket string) (*tags.Tags, error)
	SetBucketTagging(ctx context.Context, bucket string, t *tags.Tags) error
	ListObjects(ctx context.Context, bucket string, opts minio.ListObjectsOptions) <-chan minio.ObjectInfo
	RemoveObjects(ctx context.Context, bucket string, objects <-chan minio.ObjectInfo, opts minio.RemoveObjectsOptions) <-chan minio.RemoveObjectError
	ListIncompleteUploads(ctx context.Context, bucket, prefix string, recursive bool) <-chan minio.ObjectMultipartInfo
	RemoveIncompleteUpload(ctx context.Context, bucket, object string) error
	RemoveBucket(ctx context.Context, bucket string) error
}

// minioAdmin is the part of the MinIO admin client the minio driver uses.
type minioAdmin interface {
	AddCannedPolicy(ctx context.Context, name string, policy []byte) error
	RemoveCannedPolicy(ctx context.Context, name string) error
	GetUserInfo(ctx context.Context, name string) (madmin.UserInfo, error)
	AddUser(ctx context.Context, accessKey, secretKey string) error
	SetPolicy(ctx context.Context, policyName, entityName string, isGroup bool) error
	RemoveUser(ctx context.Context, accessKey string) error
	AddServiceAccount(ctx context.Context, req madmin.AddServiceAccountReq) (madmin.Credentials, error)
	ListServiceAccounts(ctx context.Context, user string) (madmin.ListServiceAccountsResp, error)
	InfoServiceAccount(ctx context.Context, accessKey string) (madmin.InfoServiceAccountResp, error)
	DeleteServiceAccount(ctx context.Context, accessKey string) error
}

// minioDriver makes MinIO buckets and users with the MinIO admin key. The
// user of an instance has a canned policy of the same name that allows its
// bucket only; the app's key is a service account of the user, which has
// the user's policy.
type minioDriver struct {
	prefix, region string
	s3             minioS3
	admin          minioAdmin
}

func newMinIO(c Config, getenv func(string) string) (Driver, error) {
	host, secure, err := c.minioEndpoint()
	if err != nil {
		return nil, err
	}
	ak, sk := getenv(c.AdminAccessKeyEnv), getenv(c.AdminSecretKeyEnv)
	if ak == "" || sk == "" {
		return nil, fmt.Errorf("storage: the minio admin key is not set: %s and %s are required", c.AdminAccessKeyEnv, c.AdminSecretKeyEnv)
	}
	creds := credentials.NewStaticV4(ak, sk, "")
	s3c, err := minio.New(host, &minio.Options{Creds: creds, Secure: secure, Region: c.Region})
	if err != nil {
		return nil, fmt.Errorf("storage: minio client: %w", err)
	}
	adm, err := madmin.NewWithOptions(host, &madmin.Options{Creds: creds, Secure: secure})
	if err != nil {
		return nil, fmt.Errorf("storage: minio admin client: %w", err)
	}
	return &minioDriver{prefix: c.BucketPrefix, region: c.Region, s3: s3c, admin: adm}, nil
}

func (d *minioDriver) Name() string                  { return DriverMinIO }
func (d *minioDriver) Bucket(instance string) string { return d.prefix + instance }

// minioCode is the S3 or admin API error code of err, or "".
func minioCode(err error) string {
	if err == nil {
		return ""
	}
	if c := minio.ToErrorResponse(err).Code; c != "" {
		return c
	}
	return madmin.ToErrorResponse(err).Code
}

func (d *minioDriver) Ensure(ctx context.Context, in Instance, newKey bool, keep string) (Result, error) {
	n, err := instanceNames(d.prefix, in)
	if err != nil {
		return Result{}, err
	}
	if err := d.ensureBucket(ctx, in, n.bucket); err != nil {
		return Result{}, err
	}
	// The policy and the user share the name.
	if err := d.admin.AddCannedPolicy(ctx, n.user, []byte(bucketPolicy("arn:aws:s3:::"+n.bucket))); err != nil {
		return Result{}, fmt.Errorf("storage: add policy %s: %w", n.user, err)
	}
	created := false
	_, err = d.admin.GetUserInfo(ctx, n.user)
	switch minioCode(err) {
	case "":
		if err != nil {
			return Result{}, fmt.Errorf("storage: read user %s: %w", n.user, err)
		}
	case "XMinioAdminNoSuchUser":
		if err := d.admin.AddUser(ctx, n.user, randomSecret()); err != nil {
			return Result{}, fmt.Errorf("storage: add user %s: %w", n.user, err)
		}
		created = true
	default:
		return Result{}, fmt.Errorf("storage: read user %s: %w", n.user, err)
	}
	if err := d.admin.SetPolicy(ctx, n.user, n.user, false); err != nil {
		return Result{}, fmt.Errorf("storage: set policy of user %s: %w", n.user, err)
	}
	out := Result{Bucket: n.bucket}
	if !newKey && !created {
		if keep == "" {
			return out, nil
		}
		list, err := d.admin.ListServiceAccounts(ctx, n.user)
		if err != nil {
			return Result{}, fmt.Errorf("storage: list service accounts of user %s: %w", n.user, err)
		}
		if slices.ContainsFunc(list.Accounts, func(a madmin.ServiceAccountInfo) bool { return a.AccessKey == keep }) {
			return out, nil
		}
		// The key in use is gone: a new one replaces it.
	}
	if err := d.deleteKeys(ctx, n.user, keep); err != nil {
		return Result{}, err
	}
	cred, err := d.admin.AddServiceAccount(ctx, madmin.AddServiceAccountReq{TargetUser: n.user, Description: "HopCharlie key of instance " + in.Name})
	if err != nil {
		return Result{}, fmt.Errorf("storage: add service account of user %s: %w", n.user, err)
	}
	out.Key = &Key{ID: cred.AccessKey, Secret: cred.SecretKey}
	return out, nil
}

// ensureBucket creates the bucket, or takes it over when it exists and is
// not another app's, and tags it. MinIO buckets are private unless a
// bucket policy says otherwise, and the driver sets none.
func (d *minioDriver) ensureBucket(ctx context.Context, in Instance, bucket string) error {
	err := d.s3.MakeBucket(ctx, bucket, minio.MakeBucketOptions{Region: d.region})
	switch minioCode(err) {
	case "":
		if err != nil {
			return fmt.Errorf("storage: make bucket %s: %w", bucket, err)
		}
	case "BucketAlreadyOwnedByYou", "BucketAlreadyExists":
		// One MinIO server and one admin: an existing bucket is ours.
		t, err := d.bucketTags(ctx, bucket)
		if err != nil {
			return err
		}
		if err := checkApp("bucket "+bucket, t, in.App, true); err != nil {
			return err
		}
	default:
		return fmt.Errorf("storage: make bucket %s: %w", bucket, err)
	}
	t, err := tags.NewTags(map[string]string{TagApp: in.App, TagInstance: in.Name}, false)
	if err != nil {
		return fmt.Errorf("storage: tags of bucket %s: %w", bucket, err)
	}
	if err := d.s3.SetBucketTagging(ctx, bucket, t); err != nil {
		return fmt.Errorf("storage: tag bucket %s: %w", bucket, err)
	}
	return nil
}

// bucketTags returns the bucket's tags; errNoBucket when it does not exist.
func (d *minioDriver) bucketTags(ctx context.Context, bucket string) (map[string]string, error) {
	t, err := d.s3.GetBucketTagging(ctx, bucket)
	switch minioCode(err) {
	case "":
		if err != nil {
			return nil, fmt.Errorf("storage: read tags of bucket %s: %w", bucket, err)
		}
		return t.ToMap(), nil
	case "NoSuchTagSet":
		return map[string]string{}, nil
	case "NoSuchBucket":
		return nil, noBucket(bucket)
	default:
		return nil, fmt.Errorf("storage: read tags of bucket %s: %w", bucket, err)
	}
}

// deleteKeys deletes the user's service accounts except keep.
func (d *minioDriver) deleteKeys(ctx context.Context, user, keep string) error {
	list, err := d.admin.ListServiceAccounts(ctx, user)
	if err != nil {
		return fmt.Errorf("storage: list service accounts of user %s: %w", user, err)
	}
	for _, a := range list.Accounts {
		if a.AccessKey == keep {
			continue
		}
		if err := d.deleteKey(ctx, a.AccessKey); err != nil {
			return err
		}
	}
	return nil
}

// serviceAccountGone reports the error codes of a service account that
// does not exist. MinIO answers an unknown one with
// XMinioInvalidIAMCredentials ("The specified service account is not
// found"); a bad admin key is InvalidAccessKeyId or SignatureDoesNotMatch.
func serviceAccountGone(err error) bool {
	c := minioCode(err)
	return c == "XMinioAdminServiceAccountNotFound" || c == "XMinioInvalidIAMCredentials"
}

func (d *minioDriver) deleteKey(ctx context.Context, id string) error {
	if err := d.admin.DeleteServiceAccount(ctx, id); err != nil && !serviceAccountGone(err) {
		return fmt.Errorf("storage: delete service account %s: %w", id, err)
	}
	return nil
}

// DeleteKey deletes the service account only when it belongs to the
// instance's user.
func (d *minioDriver) DeleteKey(ctx context.Context, in Instance, keyID string) error {
	n, err := instanceNames(d.prefix, in)
	if err != nil {
		return err
	}
	info, err := d.admin.InfoServiceAccount(ctx, keyID)
	switch {
	case serviceAccountGone(err):
		return nil
	case err != nil:
		return fmt.Errorf("storage: read service account %s: %w", keyID, err)
	case info.ParentUser != n.user:
		return fmt.Errorf("%w: service account %s belongs to %s, not %s", ErrInUse, keyID, info.ParentUser, n.user)
	}
	return d.deleteKey(ctx, keyID)
}

func (d *minioDriver) Delete(ctx context.Context, in Instance) error {
	n, err := instanceNames(d.prefix, in)
	if err != nil {
		return err
	}
	t, bucketErr := d.bucketTags(ctx, n.bucket)
	if bucketErr != nil && !errors.Is(bucketErr, errNoBucket) {
		return bucketErr
	}
	if bucketErr == nil {
		if err := checkApp("bucket "+n.bucket, t, in.App, false); err != nil {
			return err
		}
	}
	// The user first: removing it removes its service accounts too, and the
	// app loses access before its objects go. Then its policy.
	if err := d.admin.RemoveUser(ctx, n.user); err != nil && minioCode(err) != "XMinioAdminNoSuchUser" {
		return fmt.Errorf("storage: remove user %s: %w", n.user, err)
	}
	if err := d.admin.RemoveCannedPolicy(ctx, n.user); err != nil && minioCode(err) != "XMinioAdminNoSuchPolicy" {
		return fmt.Errorf("storage: remove policy %s: %w", n.user, err)
	}
	if bucketErr != nil {
		return nil
	}
	if err := d.emptyBucket(ctx, n.bucket); err != nil {
		return err
	}
	if err := d.s3.RemoveBucket(ctx, n.bucket); err != nil && minioCode(err) != "NoSuchBucket" {
		return fmt.Errorf("storage: remove bucket %s: %w", n.bucket, err)
	}
	return nil
}

// emptyBucket removes every object version of the bucket, then its
// unfinished multipart uploads.
func (d *minioDriver) emptyBucket(ctx context.Context, bucket string) error {
	if err := d.removeObjects(ctx, bucket); err != nil {
		return err
	}
	for u := range d.s3.ListIncompleteUploads(ctx, bucket, "", true) {
		if u.Err != nil {
			if minioCode(u.Err) == "NoSuchBucket" {
				return nil
			}
			return fmt.Errorf("storage: list multipart uploads of bucket %s: %w", bucket, u.Err)
		}
		if err := d.s3.RemoveIncompleteUpload(ctx, bucket, u.Key); err != nil && minioCode(err) != "NoSuchUpload" {
			return fmt.Errorf("storage: abort multipart upload of bucket %s: %w", bucket, err)
		}
	}
	return nil
}

// removeObjects removes every object version of the bucket.
func (d *minioDriver) removeObjects(ctx context.Context, bucket string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var listErr error
	objects := make(chan minio.ObjectInfo)
	go func() {
		defer close(objects)
		for o := range d.s3.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
			if o.Err != nil {
				if minioCode(o.Err) != "NoSuchBucket" {
					listErr = o.Err
				}
				return
			}
			select {
			case objects <- o:
			case <-ctx.Done():
				return
			}
		}
	}()
	var removeErr error
	// Read every error until the channel closes, so neither goroutine is
	// left behind; the first error stops the listing.
	for e := range d.s3.RemoveObjects(ctx, bucket, objects, minio.RemoveObjectsOptions{}) {
		if e.Err != nil && removeErr == nil {
			removeErr = fmt.Errorf("storage: remove object %s of bucket %s: %w", e.ObjectName, bucket, e.Err)
			cancel()
		}
	}
	if removeErr != nil {
		return removeErr
	}
	if listErr != nil {
		return fmt.Errorf("storage: list objects of bucket %s: %w", bucket, listErr)
	}
	return nil
}

// randomSecret is a password for a MinIO user, which is never used: the
// app gets a service account of the user.
func randomSecret() string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 40)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return string(b)
}
