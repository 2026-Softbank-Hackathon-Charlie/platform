package storage

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// policyName is the name of the inline policy of every IAM user.
const policyName = "hopcharlie-bucket"

// s3API is the part of the S3 client the aws driver uses.
type s3API interface {
	CreateBucket(context.Context, *s3.CreateBucketInput, ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
	GetBucketTagging(context.Context, *s3.GetBucketTaggingInput, ...func(*s3.Options)) (*s3.GetBucketTaggingOutput, error)
	PutBucketTagging(context.Context, *s3.PutBucketTaggingInput, ...func(*s3.Options)) (*s3.PutBucketTaggingOutput, error)
	PutPublicAccessBlock(context.Context, *s3.PutPublicAccessBlockInput, ...func(*s3.Options)) (*s3.PutPublicAccessBlockOutput, error)
	ListObjectVersions(context.Context, *s3.ListObjectVersionsInput, ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error)
	DeleteObjects(context.Context, *s3.DeleteObjectsInput, ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
	ListMultipartUploads(context.Context, *s3.ListMultipartUploadsInput, ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
	DeleteBucket(context.Context, *s3.DeleteBucketInput, ...func(*s3.Options)) (*s3.DeleteBucketOutput, error)
}

// iamAPI is the part of the IAM client the aws driver uses.
type iamAPI interface {
	CreateUser(context.Context, *iam.CreateUserInput, ...func(*iam.Options)) (*iam.CreateUserOutput, error)
	GetUser(context.Context, *iam.GetUserInput, ...func(*iam.Options)) (*iam.GetUserOutput, error)
	PutUserPolicy(context.Context, *iam.PutUserPolicyInput, ...func(*iam.Options)) (*iam.PutUserPolicyOutput, error)
	DeleteUserPolicy(context.Context, *iam.DeleteUserPolicyInput, ...func(*iam.Options)) (*iam.DeleteUserPolicyOutput, error)
	ListAccessKeys(context.Context, *iam.ListAccessKeysInput, ...func(*iam.Options)) (*iam.ListAccessKeysOutput, error)
	CreateAccessKey(context.Context, *iam.CreateAccessKeyInput, ...func(*iam.Options)) (*iam.CreateAccessKeyOutput, error)
	DeleteAccessKey(context.Context, *iam.DeleteAccessKeyInput, ...func(*iam.Options)) (*iam.DeleteAccessKeyOutput, error)
	DeleteUser(context.Context, *iam.DeleteUserInput, ...func(*iam.Options)) (*iam.DeleteUserOutput, error)
}

// awsDriver makes S3 buckets and IAM users with the platform's own AWS
// credentials (the platform pod's role).
type awsDriver struct {
	prefix, region, path, boundary string
	s3                             s3API
	iam                            iamAPI
}

func newAWS(ctx context.Context, c Config) (*awsDriver, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(c.Region))
	if err != nil {
		return nil, fmt.Errorf("storage: aws config: %w", err)
	}
	return &awsDriver{
		prefix: c.BucketPrefix, region: c.Region, path: c.IAMPath, boundary: c.PermissionsBoundary,
		s3: s3.NewFromConfig(cfg), iam: iam.NewFromConfig(cfg),
	}, nil
}

var _ KeyReader = (*awsDriver)(nil)

func (d *awsDriver) Name() string                  { return DriverAWS }
func (d *awsDriver) Bucket(instance string) string { return d.prefix + instance }

// BucketARN is the ARN of a bucket in the driver's partition.
func (d *awsDriver) bucketARN(bucket string) string {
	return "arn:" + partition(d.region) + ":s3:::" + bucket
}

// partition is the AWS partition of a region.
func partition(region string) string {
	switch {
	case strings.HasPrefix(region, "cn-"):
		return "aws-cn"
	case strings.HasPrefix(region, "us-gov-"):
		return "aws-us-gov"
	}
	return "aws"
}

// errorCode is the AWS error code of err, or "".
func errorCode(err error) string {
	var api smithy.APIError
	if errors.As(err, &api) {
		return api.ErrorCode()
	}
	return ""
}

func (d *awsDriver) tags(in Instance) map[string]string {
	return map[string]string{TagApp: in.App, TagInstance: in.Name}
}

func (d *awsDriver) Ensure(ctx context.Context, in Instance, newKey bool, current, old string) (Result, error) {
	n, err := instanceNames(d.prefix, in)
	if err != nil {
		return Result{}, err
	}
	if err := d.ensureBucket(ctx, in, n.bucket); err != nil {
		return Result{}, err
	}
	created, err := d.ensureUser(ctx, in, n.user)
	if err != nil {
		return Result{}, err
	}
	policy := bucketPolicy(d.bucketARN(n.bucket))
	if _, err := d.iam.PutUserPolicy(ctx, &iam.PutUserPolicyInput{UserName: &n.user, PolicyName: aws.String(policyName), PolicyDocument: &policy}); err != nil {
		return Result{}, fmt.Errorf("storage: put policy of user %s: %w", n.user, err)
	}
	out := Result{Bucket: n.bucket}
	if !newKey && !created {
		if current == "" {
			return out, nil
		}
		ids, err := d.keyIDs(ctx, n.user)
		if err != nil {
			return Result{}, err
		}
		if slices.Contains(ids, current) {
			return out, nil
		}
		// The key in use is gone: a new one replaces it.
	}
	if err := d.deleteKeys(ctx, n.user, current, old); err != nil {
		return Result{}, err
	}
	k, err := d.iam.CreateAccessKey(ctx, &iam.CreateAccessKeyInput{UserName: &n.user})
	if err != nil {
		return Result{}, fmt.Errorf("storage: create access key of user %s: %w", n.user, err)
	}
	out.Key = &Key{ID: aws.ToString(k.AccessKey.AccessKeyId), Secret: aws.ToString(k.AccessKey.SecretAccessKey)}
	return out, nil
}

// ensureBucket creates the bucket in the driver's region, or takes it over
// when it already exists in the account and is not another app's, and
// then blocks public access and tags it.
func (d *awsDriver) ensureBucket(ctx context.Context, in Instance, bucket string) error {
	req := &s3.CreateBucketInput{Bucket: &bucket}
	if d.region != "us-east-1" { // us-east-1 takes no location constraint
		req.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{LocationConstraint: s3types.BucketLocationConstraint(d.region)}
	}
	_, err := d.s3.CreateBucket(ctx, req)
	switch code := errorCode(err); {
	case err == nil || code == "BucketAlreadyOwnedByYou":
		// Checked after a create too: in us-east-1 creating a bucket the
		// account already owns succeeds.
		tags, err := d.bucketTags(ctx, bucket)
		if err != nil {
			return err
		}
		if err := checkApp("bucket "+bucket, tags, in.App, true); err != nil {
			return err
		}
	case code == "BucketAlreadyExists":
		return fmt.Errorf("%w: bucket name %s is taken by another AWS account", ErrInUse, bucket)
	default:
		return fmt.Errorf("storage: create bucket %s: %w", bucket, err)
	}
	if _, err := d.s3.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket: &bucket,
		PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
			BlockPublicAcls: aws.Bool(true), IgnorePublicAcls: aws.Bool(true), BlockPublicPolicy: aws.Bool(true), RestrictPublicBuckets: aws.Bool(true),
		},
	}); err != nil {
		return fmt.Errorf("storage: block public access of bucket %s: %w", bucket, err)
	}
	var tagSet []s3types.Tag
	for _, k := range []string{TagApp, TagInstance} {
		tagSet = append(tagSet, s3types.Tag{Key: aws.String(k), Value: aws.String(d.tags(in)[k])})
	}
	if _, err := d.s3.PutBucketTagging(ctx, &s3.PutBucketTaggingInput{Bucket: &bucket, Tagging: &s3types.Tagging{TagSet: tagSet}}); err != nil {
		return fmt.Errorf("storage: tag bucket %s: %w", bucket, err)
	}
	return nil
}

// bucketTags returns the bucket's tags; errNoBucket when it does not exist.
func (d *awsDriver) bucketTags(ctx context.Context, bucket string) (map[string]string, error) {
	out, err := d.s3.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: &bucket})
	switch errorCode(err) {
	case "":
		if err != nil {
			return nil, fmt.Errorf("storage: read tags of bucket %s: %w", bucket, err)
		}
	case "NoSuchTagSet":
		return map[string]string{}, nil
	case "NoSuchBucket":
		return nil, noBucket(bucket)
	default:
		return nil, fmt.Errorf("storage: read tags of bucket %s: %w", bucket, err)
	}
	tags := map[string]string{}
	for _, t := range out.TagSet {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return tags, nil
}

// ensureUser creates the user under the driver's path with the permissions
// boundary and the tags, and reports whether it did. An existing user is
// kept when it is under the path and tagged with the instance's app.
func (d *awsDriver) ensureUser(ctx context.Context, in Instance, user string) (bool, error) {
	var tags []iamtypes.Tag
	for _, k := range []string{TagApp, TagInstance} {
		tags = append(tags, iamtypes.Tag{Key: aws.String(k), Value: aws.String(d.tags(in)[k])})
	}
	_, err := d.iam.CreateUser(ctx, &iam.CreateUserInput{UserName: &user, Path: &d.path, PermissionsBoundary: &d.boundary, Tags: tags})
	if err == nil {
		return true, nil
	}
	if errorCode(err) != "EntityAlreadyExists" {
		return false, fmt.Errorf("storage: create user %s: %w", user, err)
	}
	if err := d.checkUser(ctx, in, user); err != nil {
		return false, err
	}
	return false, nil
}

// errNoUser means the user does not exist.
var errNoUser = errors.New("no such user")

// checkUser returns ErrInUse when the user is not under the driver's path
// or not tagged with the instance's app, and errNoUser when it does not
// exist.
func (d *awsDriver) checkUser(ctx context.Context, in Instance, user string) error {
	out, err := d.iam.GetUser(ctx, &iam.GetUserInput{UserName: &user})
	if errorCode(err) == "NoSuchEntity" {
		return errNoUser
	}
	if err != nil {
		return fmt.Errorf("storage: read user %s: %w", user, err)
	}
	if p := aws.ToString(out.User.Path); p != d.path {
		return fmt.Errorf("%w: user %s has path %s, not %s", ErrInUse, user, p, d.path)
	}
	tags := map[string]string{}
	for _, t := range out.User.Tags {
		tags[aws.ToString(t.Key)] = aws.ToString(t.Value)
	}
	return checkApp("user "+user, tags, in.App, false)
}

// keyIDs lists the user's access key ids.
func (d *awsDriver) keyIDs(ctx context.Context, user string) ([]string, error) {
	pages := iam.NewListAccessKeysPaginator(d.iam, &iam.ListAccessKeysInput{UserName: &user})
	var ids []string
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("storage: list access keys of user %s: %w", user, err)
		}
		for _, k := range page.AccessKeyMetadata {
			ids = append(ids, aws.ToString(k.AccessKeyId))
		}
	}
	return ids, nil
}

// deleteKeys deletes the user's access keys except keep.
func (d *awsDriver) deleteKeys(ctx context.Context, user, current, old string) error {
	ids, err := d.keyIDs(ctx, user)
	if err != nil {
		return err
	}
	del, err := keysToDelete(user, ids, current, old)
	if err != nil {
		return err
	}
	for _, id := range del {
		if err := d.deleteKey(ctx, user, id); err != nil {
			return err
		}
	}
	return nil
}

func (d *awsDriver) deleteKey(ctx context.Context, user, id string) error {
	_, err := d.iam.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{UserName: &user, AccessKeyId: &id})
	if err != nil && errorCode(err) != "NoSuchEntity" {
		return fmt.Errorf("storage: delete access key %s of user %s: %w", MaskKey(id), user, err)
	}
	return nil
}

// Keys lists the user's access keys with their CreateDate.
func (d *awsDriver) Keys(ctx context.Context, in Instance) ([]KeyInfo, error) {
	n, err := instanceNames(d.prefix, in)
	if err != nil {
		return nil, err
	}
	pages := iam.NewListAccessKeysPaginator(d.iam, &iam.ListAccessKeysInput{UserName: &n.user})
	var keys []KeyInfo
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if errorCode(err) == "NoSuchEntity" {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("storage: list access keys of user %s: %w", n.user, err)
		}
		for _, k := range page.AccessKeyMetadata {
			keys = append(keys, KeyInfo{ID: aws.ToString(k.AccessKeyId), CreatedAt: aws.ToTime(k.CreateDate).UTC()})
		}
	}
	return keys, nil
}

// Scope is the user's inline policy and its permissions boundary.
func (d *awsDriver) Scope() Scope {
	return Scope{Actions: slices.Concat(bucketActions, objectActions), Boundary: d.boundary}
}

func (d *awsDriver) DeleteKey(ctx context.Context, in Instance, keyID string) error {
	n, err := instanceNames(d.prefix, in)
	if err != nil {
		return err
	}
	return d.deleteKey(ctx, n.user, keyID)
}

func (d *awsDriver) Delete(ctx context.Context, in Instance) error {
	n, err := instanceNames(d.prefix, in)
	if err != nil {
		return err
	}
	// Check both first, so nothing is deleted when either is another app's.
	userErr := d.checkUser(ctx, in, n.user)
	if userErr != nil && !errors.Is(userErr, errNoUser) {
		return userErr
	}
	tags, bucketErr := d.bucketTags(ctx, n.bucket)
	if bucketErr != nil && !errors.Is(bucketErr, errNoBucket) {
		return bucketErr
	}
	if bucketErr == nil {
		if err := checkApp("bucket "+n.bucket, tags, in.App, false); err != nil {
			return err
		}
	}

	// The user first: the app loses access before its objects go.
	if userErr == nil {
		if err := d.deleteKeys(ctx, n.user, "", ""); err != nil {
			return err
		}
		if _, err := d.iam.DeleteUserPolicy(ctx, &iam.DeleteUserPolicyInput{UserName: &n.user, PolicyName: aws.String(policyName)}); err != nil && errorCode(err) != "NoSuchEntity" {
			return fmt.Errorf("storage: delete policy of user %s: %w", n.user, err)
		}
		if _, err := d.iam.DeleteUser(ctx, &iam.DeleteUserInput{UserName: &n.user}); err != nil && errorCode(err) != "NoSuchEntity" {
			return fmt.Errorf("storage: delete user %s: %w", n.user, err)
		}
	}
	if bucketErr == nil {
		if err := d.emptyBucket(ctx, n.bucket); err != nil {
			return err
		}
		if _, err := d.s3.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: &n.bucket}); err != nil && errorCode(err) != "NoSuchBucket" {
			return fmt.Errorf("storage: delete bucket %s: %w", n.bucket, err)
		}
	}
	return nil
}

// emptyBucket deletes every object version and delete marker of the
// bucket, a page (at most 1000) at a time, and aborts its unfinished
// multipart uploads.
func (d *awsDriver) emptyBucket(ctx context.Context, bucket string) error {
	versions := s3.NewListObjectVersionsPaginator(d.s3, &s3.ListObjectVersionsInput{Bucket: &bucket})
	for versions.HasMorePages() {
		page, err := versions.NextPage(ctx)
		if errorCode(err) == "NoSuchBucket" {
			return nil
		}
		if err != nil {
			return fmt.Errorf("storage: list objects of bucket %s: %w", bucket, err)
		}
		var objects []s3types.ObjectIdentifier
		for _, v := range page.Versions {
			objects = append(objects, s3types.ObjectIdentifier{Key: v.Key, VersionId: v.VersionId})
		}
		for _, m := range page.DeleteMarkers {
			objects = append(objects, s3types.ObjectIdentifier{Key: m.Key, VersionId: m.VersionId})
		}
		if len(objects) == 0 {
			continue
		}
		out, err := d.s3.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: &bucket, Delete: &s3types.Delete{Objects: objects, Quiet: aws.Bool(true)}})
		if err != nil {
			return fmt.Errorf("storage: delete objects of bucket %s: %w", bucket, err)
		}
		if len(out.Errors) > 0 {
			e := out.Errors[0]
			return fmt.Errorf("storage: delete %d objects of bucket %s failed, the first %s: %s", len(out.Errors), bucket, aws.ToString(e.Key), aws.ToString(e.Code))
		}
	}
	uploads := s3.NewListMultipartUploadsPaginator(d.s3, &s3.ListMultipartUploadsInput{Bucket: &bucket})
	for uploads.HasMorePages() {
		page, err := uploads.NextPage(ctx)
		if errorCode(err) == "NoSuchBucket" {
			return nil
		}
		if err != nil {
			return fmt.Errorf("storage: list multipart uploads of bucket %s: %w", bucket, err)
		}
		for _, u := range page.Uploads {
			_, err := d.s3.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &bucket, Key: u.Key, UploadId: u.UploadId})
			if err != nil && errorCode(err) != "NoSuchUpload" {
				return fmt.Errorf("storage: abort multipart upload of bucket %s: %w", bucket, err)
			}
		}
	}
	return nil
}
