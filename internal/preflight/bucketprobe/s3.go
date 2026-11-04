package bucketprobe

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

type s3Store struct {
	r      remote
	client *s3.Client
	opts   s3.Options
	host   string
}

func newS3(ctx context.Context, r remote, hc *http.Client) (*s3Store, string) {
	region := r.region
	if region == "" {
		region = "us-east-1"
	}
	o := s3.Options{Region: region, HTTPClient: hc, UsePathStyle: !strings.EqualFold(r.provider, "AWS")}
	switch {
	case r.accessKey != "" && r.secretKey != "":
		o.Credentials = credentials.NewStaticCredentialsProvider(r.accessKey, r.secretKey, r.sessionToken)
	case r.envAuth:
		cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(region))
		if err != nil {
			return nil, CodeCredentials
		}
		o.Credentials = cfg.Credentials
	default:
		o.Credentials = aws.AnonymousCredentials{}
	}
	st := &s3Store{r: r}
	if r.endpoint != "" {
		host, base, ok := endpointHost(r.endpoint)
		if !ok {
			return nil, CodeUnsupportedBackend
		}
		o.BaseEndpoint, st.host = aws.String(base), host
	}
	st.opts = o
	st.client = s3.New(o)
	return st, ""
}

func (s *s3Store) endpointHost() string {
	if s.host != "" {
		return s.host
	}
	return "s3." + s.opts.Region + ".amazonaws.com"
}

func (s *s3Store) location(ctx context.Context) (string, string, error) {
	out, err := s.client.GetBucketLocation(ctx, &s3.GetBucketLocationInput{Bucket: aws.String(s.r.bucket)})
	if err != nil {
		return "", "", err
	}
	loc := strings.ToLower(string(out.LocationConstraint))
	if s.r.providerHosted() && strings.EqualFold(s.r.provider, "AWS") {
		// S3 reports us-east-1 as empty and eu-west-1 as the legacy "EU".
		switch loc {
		case "":
			loc = "us-east-1"
		case "eu":
			loc = "eu-west-1"
		}
		// Later calls go to the bucket's own region.
		if loc != s.opts.Region {
			o := s.opts
			o.Region = loc
			s.opts, s.client = o, s3.New(o)
		}
		return loc, "region", nil
	}
	if strings.EqualFold(s.r.provider, "GCS") {
		return loc, gcsLocationType(loc), nil
	}
	// A MinIO started with MINIO_REGION rejects requests signed for another
	// region; GetBucketLocation alone is exempt. Sign the rest for the region
	// it reports, as the AWS case does.
	if loc != "" && loc != s.opts.Region {
		o := s.opts
		o.Region = loc
		s.opts, s.client = o, s3.New(o)
	}
	return loc, "", nil
}

func (s *s3Store) prefixes(ctx context.Context, path string) ([]string, string) {
	base := ""
	if path != "" {
		base = path + "/"
	}
	out := []string{}
	in := &s3.ListObjectsV2Input{Bucket: aws.String(s.r.bucket), Delimiter: aws.String("/")}
	if base != "" {
		in.Prefix = aws.String(base)
	}
	for page := 0; ; page++ {
		if page >= maxListPages {
			return nil, CodeTooManyPrefixes
		}
		res, err := s.client.ListObjectsV2(ctx, in)
		if err != nil {
			return nil, CodeListFailed
		}
		var code string
		for _, cp := range res.CommonPrefixes {
			if out, code = addPrefix(out, strings.TrimPrefix(aws.ToString(cp.Prefix), base)); code != "" {
				return nil, code
			}
		}
		if len(res.Contents) > 0 {
			if out, code = addPrefix(out, ""); code != "" {
				return nil, code
			}
		}
		if !aws.ToBool(res.IsTruncated) || res.NextContinuationToken == nil {
			break
		}
		in.ContinuationToken = res.NextContinuationToken
	}
	slices.Sort(out)
	return out, ""
}

// replicas lists the destination buckets of enabled replication rules. AWS
// names them "arn:aws:s3:::bucket" and MinIO "arn:minio:replication::id:bucket";
// the bucket is the part after the last colon either way.
func (s *s3Store) replicas(ctx context.Context) ([]string, error) {
	out, err := s.client.GetBucketReplication(ctx, &s3.GetBucketReplicationInput{Bucket: aws.String(s.r.bucket)})
	if apiCode(err) == "ReplicationConfigurationNotFoundError" {
		return []string{}, nil
	}
	if err != nil || out.ReplicationConfiguration == nil {
		return nil, errors.New("replication")
	}
	names := []string{}
	for _, rule := range out.ReplicationConfiguration.Rules {
		if rule.Status != s3types.ReplicationRuleStatusEnabled || rule.Destination == nil {
			continue
		}
		arn := aws.ToString(rule.Destination.Bucket)
		name := arn[strings.LastIndex(arn, ":")+1:]
		if name == "" {
			return nil, errors.New("replication destination")
		}
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names, nil
}

// tiered reports an enabled lifecycle rule that transitions objects. On a
// self-hosted store (MinIO ILM) a transition's storage class is a remote tier.
func (s *s3Store) tiered(ctx context.Context) (bool, error) {
	out, err := s.client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(s.r.bucket)})
	if apiCode(err) == "NoSuchLifecycleConfiguration" {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, rule := range out.Rules {
		if rule.Status == s3types.ExpirationStatusEnabled && (len(rule.Transitions) > 0 || len(rule.NoncurrentVersionTransitions) > 0) {
			return true, nil
		}
	}
	return false, nil
}

func apiCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}
	return ""
}

// gcsLocationType classifies a GCS location code: a region has a number
// (asia-northeast3), a dual-region a short code (asia1, nam4) or a "+"
// configuration, and a multi-region is a bare area (asia, eu, us).
func gcsLocationType(loc string) string {
	switch {
	case strings.Contains(loc, "-") && strings.IndexAny(loc, "0123456789") > 0:
		return "region"
	case strings.IndexAny(loc, "0123456789") > 0:
		return "dual-region"
	default:
		return "multi-region"
	}
}
