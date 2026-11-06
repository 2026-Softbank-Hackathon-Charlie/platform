package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	cttypes "github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/guardduty"
	gdtypes "github.com/aws/aws-sdk-go-v2/service/guardduty/types"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	iamtypes "github.com/aws/aws-sdk-go-v2/service/iam/types"
	"github.com/aws/aws-sdk-go-v2/service/s3control"
	s3ctypes "github.com/aws/aws-sdk-go-v2/service/s3control/types"
	"github.com/aws/aws-sdk-go-v2/service/securityhub"
	shtypes "github.com/aws/aws-sdk-go-v2/service/securityhub/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// TestAWSCallsAreReadOnly keeps the collector from ever calling an API that
// changes the account: every SDK method it can reach is a Describe, List or Get.
func TestAWSCallsAreReadOnly(t *testing.T) {
	for _, api := range []reflect.Type{
		reflect.TypeFor[iamAPI](), reflect.TypeFor[ec2API](), reflect.TypeFor[cloudtrailAPI](), reflect.TypeFor[guarddutyAPI](),
		reflect.TypeFor[securityhubAPI](), reflect.TypeFor[s3controlAPI](), reflect.TypeFor[stsAPI](),
	} {
		for i := range api.NumMethod() {
			name := api.Method(i).Name
			if !strings.HasPrefix(name, "Describe") && !strings.HasPrefix(name, "List") && !strings.HasPrefix(name, "Get") {
				t.Errorf("%s.%s is not a read-only call", api.Name(), name)
			}
		}
	}
}

// --- fakes (tests only) ---

type fakeIAM struct {
	summary  map[string]int32
	users    []iamtypes.User
	console  map[string]bool
	mfa      map[string]int
	keys     map[string][]iamtypes.AccessKeyMetadata
	usersErr error
}

func (f *fakeIAM) GetAccountSummary(context.Context, *iam.GetAccountSummaryInput, ...func(*iam.Options)) (*iam.GetAccountSummaryOutput, error) {
	return &iam.GetAccountSummaryOutput{SummaryMap: f.summary}, nil
}
func (f *fakeIAM) ListUsers(context.Context, *iam.ListUsersInput, ...func(*iam.Options)) (*iam.ListUsersOutput, error) {
	return &iam.ListUsersOutput{Users: f.users}, f.usersErr
}
func (f *fakeIAM) GetLoginProfile(_ context.Context, in *iam.GetLoginProfileInput, _ ...func(*iam.Options)) (*iam.GetLoginProfileOutput, error) {
	if !f.console[*in.UserName] {
		return nil, &iamtypes.NoSuchEntityException{Message: aws.String("no login profile")}
	}
	return &iam.GetLoginProfileOutput{}, nil
}
func (f *fakeIAM) ListMFADevices(_ context.Context, in *iam.ListMFADevicesInput, _ ...func(*iam.Options)) (*iam.ListMFADevicesOutput, error) {
	return &iam.ListMFADevicesOutput{MFADevices: make([]iamtypes.MFADevice, f.mfa[*in.UserName])}, nil
}
func (f *fakeIAM) ListAccessKeys(_ context.Context, in *iam.ListAccessKeysInput, _ ...func(*iam.Options)) (*iam.ListAccessKeysOutput, error) {
	return &iam.ListAccessKeysOutput{AccessKeyMetadata: f.keys[*in.UserName]}, nil
}

type fakeEC2 struct {
	groups []ec2types.SecurityGroup
	ebs    bool
	err    error
}

func (f *fakeEC2) DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &ec2.DescribeSecurityGroupsOutput{SecurityGroups: f.groups}, nil
}
func (f *fakeEC2) GetEbsEncryptionByDefault(context.Context, *ec2.GetEbsEncryptionByDefaultInput, ...func(*ec2.Options)) (*ec2.GetEbsEncryptionByDefaultOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &ec2.GetEbsEncryptionByDefaultOutput{EbsEncryptionByDefault: aws.Bool(f.ebs)}, nil
}

type fakeTrail struct {
	trails  []cttypes.Trail
	logging map[string]bool
}

func (f *fakeTrail) DescribeTrails(context.Context, *cloudtrail.DescribeTrailsInput, ...func(*cloudtrail.Options)) (*cloudtrail.DescribeTrailsOutput, error) {
	return &cloudtrail.DescribeTrailsOutput{TrailList: f.trails}, nil
}
func (f *fakeTrail) GetTrailStatus(_ context.Context, in *cloudtrail.GetTrailStatusInput, _ ...func(*cloudtrail.Options)) (*cloudtrail.GetTrailStatusOutput, error) {
	return &cloudtrail.GetTrailStatusOutput{IsLogging: aws.Bool(f.logging[*in.Name])}, nil
}

type fakeGuardDuty struct {
	detectors []string
	status    gdtypes.DetectorStatus
	findings  []gdtypes.Finding
	more      bool // findings beyond the page
}

func (f *fakeGuardDuty) ListDetectors(context.Context, *guardduty.ListDetectorsInput, ...func(*guardduty.Options)) (*guardduty.ListDetectorsOutput, error) {
	return &guardduty.ListDetectorsOutput{DetectorIds: f.detectors}, nil
}
func (f *fakeGuardDuty) GetDetector(context.Context, *guardduty.GetDetectorInput, ...func(*guardduty.Options)) (*guardduty.GetDetectorOutput, error) {
	return &guardduty.GetDetectorOutput{Status: f.status}, nil
}
func (f *fakeGuardDuty) ListFindings(_ context.Context, in *guardduty.ListFindingsInput, _ ...func(*guardduty.Options)) (*guardduty.ListFindingsOutput, error) {
	if c := in.FindingCriteria.Criterion["service.archived"]; !slices.Equal(c.Equals, []string{"false"}) || in.SortCriteria.OrderBy != gdtypes.OrderByDesc {
		return nil, errors.New("expected unarchived findings, most severe first")
	}
	var ids []string
	for _, x := range f.findings {
		ids = append(ids, *x.Id)
	}
	out := &guardduty.ListFindingsOutput{FindingIds: ids}
	if f.more {
		out.NextToken = aws.String("next")
	}
	return out, nil
}
func (f *fakeGuardDuty) GetFindings(context.Context, *guardduty.GetFindingsInput, ...func(*guardduty.Options)) (*guardduty.GetFindingsOutput, error) {
	return &guardduty.GetFindingsOutput{Findings: f.findings}, nil
}

type fakeHub struct {
	enabled  bool
	findings map[shtypes.SeverityLabel][]shtypes.AwsSecurityFinding
}

func (f *fakeHub) DescribeHub(context.Context, *securityhub.DescribeHubInput, ...func(*securityhub.Options)) (*securityhub.DescribeHubOutput, error) {
	if !f.enabled {
		return nil, &shtypes.InvalidAccessException{Message: aws.String("Account is not subscribed to AWS Security Hub")}
	}
	return &securityhub.DescribeHubOutput{}, nil
}
func (f *fakeHub) GetFindings(_ context.Context, in *securityhub.GetFindingsInput, _ ...func(*securityhub.Options)) (*securityhub.GetFindingsOutput, error) {
	label := shtypes.SeverityLabel(*in.Filters.SeverityLabel[0].Value)
	return &securityhub.GetFindingsOutput{Findings: f.findings[label]}, nil
}

type fakeS3 struct {
	pab *s3ctypes.PublicAccessBlockConfiguration
}

func (f *fakeS3) GetPublicAccessBlock(context.Context, *s3control.GetPublicAccessBlockInput, ...func(*s3control.Options)) (*s3control.GetPublicAccessBlockOutput, error) {
	if f.pab == nil {
		return nil, &s3ctypes.NoSuchPublicAccessBlockConfiguration{Message: aws.String("none")}
	}
	return &s3control.GetPublicAccessBlockOutput{PublicAccessBlockConfiguration: f.pab}, nil
}

type fakeSTS struct{ err error }

func (f *fakeSTS) GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error) {
	if f.err != nil {
		return nil, f.err
	}
	return &sts.GetCallerIdentityOutput{Account: aws.String("123456789012")}, nil
}

func collectAWS(t *testing.T, cl awsClients) map[string]Report {
	t.Helper()
	c := newAWSCollector("ap-northeast-2", func(context.Context) (awsClients, error) { return cl, nil })
	c.now = func() time.Time { return t0 }
	reps := c.Collect(t.Context())
	out := map[string]Report{}
	for _, r := range reps {
		if r.Environment != EnvAWSAccount || r.Provider != apiv1.SecurityProviderAws {
			t.Errorf("source %+v", r.Source)
		}
		out[r.Name] = r
	}
	if len(out) != len(awsSources) {
		t.Fatalf("reports for %d sources, want %d", len(out), len(awsSources))
	}
	return out
}

func sg(id string, perms ...ec2types.IpPermission) ec2types.SecurityGroup {
	return ec2types.SecurityGroup{GroupId: aws.String(id), GroupName: aws.String(id + "-name"), VpcId: aws.String("vpc-1"), IpPermissions: perms}
}

func tcp(from, to int32, cidrs ...string) ec2types.IpPermission {
	p := ec2types.IpPermission{IpProtocol: aws.String("tcp"), FromPort: aws.Int32(from), ToPort: aws.Int32(to)}
	for _, c := range cidrs {
		if strings.Contains(c, ":") {
			p.Ipv6Ranges = append(p.Ipv6Ranges, ec2types.Ipv6Range{CidrIpv6: aws.String(c)})
		} else {
			p.IpRanges = append(p.IpRanges, ec2types.IpRange{CidrIp: aws.String(c)})
		}
	}
	return p
}

func TestAWSFreshAccount(t *testing.T) {
	old := t0.Add(-200 * 24 * time.Hour)
	recent := t0.Add(-10 * 24 * time.Hour)
	reps := collectAWS(t, awsClients{
		iam: &fakeIAM{
			summary: map[string]int32{"AccountMFAEnabled": 0, "AccountAccessKeysPresent": 0},
			users:   []iamtypes.User{{UserName: aws.String("alice"), Arn: aws.String("arn:aws:iam::123456789012:user/alice")}, {UserName: aws.String("ci"), Arn: aws.String("arn:aws:iam::123456789012:user/ci")}},
			console: map[string]bool{"alice": true},
			keys: map[string][]iamtypes.AccessKeyMetadata{
				"ci": {
					{AccessKeyId: aws.String("AKIAEXAMPLE0000WXYZ"), Status: iamtypes.StatusTypeActive, CreateDate: &old},
					{AccessKeyId: aws.String("AKIAEXAMPLE0000NEW1"), Status: iamtypes.StatusTypeActive, CreateDate: &recent},
					{AccessKeyId: aws.String("AKIAEXAMPLE0000OFF1"), Status: iamtypes.StatusTypeInactive, CreateDate: &old},
				},
			},
		},
		ec2: &fakeEC2{groups: []ec2types.SecurityGroup{
			sg("sg-web", tcp(443, 443, "0.0.0.0/0", "::/0"), tcp(80, 80, "0.0.0.0/0")),
			sg("sg-ssh", tcp(22, 22, "0.0.0.0/0"), tcp(5432, 5432, "10.0.0.0/8")),
			sg("sg-wide", tcp(1000, 2000, "::/0"), ec2types.IpPermission{IpProtocol: aws.String("-1"), IpRanges: []ec2types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}}),
		}},
		cloudtrail:  &fakeTrail{},
		guardduty:   &fakeGuardDuty{},
		securityhub: &fakeHub{},
		s3control:   &fakeS3{},
		sts:         &fakeSTS{},
	})
	want := map[string][]string{
		"aws-iam": {
			"critical aws.root-mfa arn:aws:iam::123456789012:root",
			"medium aws.access-key-age arn:aws:iam::123456789012:user/ci/access-key/****WXYZ",
			"medium aws.user-mfa arn:aws:iam::123456789012:user/alice", // IAM.5 is Medium
		},
		"aws-ec2": {
			"critical aws.sg-open-ingress sg-ssh/tcp/22",         // EC2.19 blocks 22
			"critical aws.sg-open-ingress sg-wide/all/all",       // all ports include EC2.19's
			"critical aws.sg-open-ingress sg-wide/tcp/1000-2000", // includes 1433 and 1434
			"info aws.sg-open-ingress sg-web/tcp/443",
			"info aws.sg-open-ingress sg-web/tcp/80",
			"medium aws.ebs-default-encryption ebs-encryption-by-default/ap-northeast-2",
		},
		"aws-cloudtrail":  {"high aws.cloudtrail-logging cloudtrail/ap-northeast-2"},
		"aws-guardduty":   {"high aws.guardduty-enabled guardduty/ap-northeast-2"},
		"aws-securityhub": {"info aws.securityhub-enabled securityhub/ap-northeast-2"},
		"aws-s3":          {"medium aws.s3-public-access-block s3-account-public-access-block/123456789012"},
	}
	for name, w := range want {
		r := reps[name]
		if r.Status != apiv1.SecuritySourceStatusOk {
			t.Errorf("%s: %s %s", name, r.Status, r.Message)
		}
		if got := checks(r); !slices.Equal(got, w) {
			t.Errorf("%s findings:\n%s\nwant:\n%s", name, strings.Join(got, "\n"), strings.Join(w, "\n"))
		}
		for _, f := range r.Findings {
			if f.Title == "" || f.Detail == "" || f.Recommendation == "" {
				t.Errorf("incomplete finding %+v", f)
			}
			if strings.Contains(f.Detail+f.Resource, "AKIAEXAMPLE") {
				t.Errorf("access key id shown in full: %+v", f)
			}
		}
	}
}

func TestAWSVendorFindingsAndFailures(t *testing.T) {
	denied := &smithy.GenericAPIError{Code: "UnauthorizedOperation", Message: "You are not authorized to perform this operation."}
	reps := collectAWS(t, awsClients{
		iam: &fakeIAM{summary: map[string]int32{"AccountMFAEnabled": 1}, usersErr: &smithy.GenericAPIError{Code: "AccessDenied", Message: "no"}},
		ec2: &fakeEC2{err: denied},
		cloudtrail: &fakeTrail{trails: []cttypes.Trail{{Name: aws.String("main"), TrailARN: aws.String("arn:trail/main")}},
			logging: map[string]bool{"arn:trail/main": true}},
		guardduty: &fakeGuardDuty{detectors: []string{"det1"}, status: gdtypes.DetectorStatusEnabled, findings: []gdtypes.Finding{{
			Id: aws.String("gd1"), Type: aws.String("Recon:EC2/PortProbeUnprotectedPort"), Severity: aws.Float64(7.5),
			Title: aws.String("Unprotected port on EC2 instance i-1 is being probed."), Description: aws.String("Port 22 is being probed."),
			UpdatedAt: aws.String("2026-11-02T08:00:00.000Z"), Service: &gdtypes.Service{Count: aws.Int32(4)},
			Resource: &gdtypes.Resource{ResourceType: aws.String("Instance"), InstanceDetails: &gdtypes.InstanceDetails{InstanceId: aws.String("i-1")}},
		}}},
		securityhub: &fakeHub{enabled: true, findings: map[shtypes.SeverityLabel][]shtypes.AwsSecurityFinding{
			shtypes.SeverityLabelHigh: {{
				Id: aws.String("hub1"), GeneratorId: aws.String("security-control/S3.8"), Title: aws.String("S3 general purpose buckets should block public access"),
				Severity: &shtypes.Severity{Label: shtypes.SeverityLabelHigh}, Resources: []shtypes.Resource{{Id: aws.String("arn:aws:s3:::shop-media"), Type: aws.String("AwsS3Bucket")}},
				Remediation: &shtypes.Remediation{Recommendation: &shtypes.Recommendation{Text: aws.String("Block public access."), Url: aws.String("https://docs.aws.amazon.com/x")}},
				UpdatedAt:   aws.String("2026-11-02T07:00:00Z"),
			}},
		}},
		s3control: &fakeS3{pab: &s3ctypes.PublicAccessBlockConfiguration{BlockPublicAcls: aws.Bool(true), IgnorePublicAcls: aws.Bool(true), BlockPublicPolicy: aws.Bool(true), RestrictPublicBuckets: aws.Bool(true)}},
		sts:       &fakeSTS{},
	})

	if r := reps["aws-iam"]; r.Status != apiv1.SecuritySourceStatusError || !strings.Contains(r.Message, "iam ListUsers: access denied (AccessDenied)") || len(r.Findings) != 0 {
		t.Errorf("iam: %+v", r)
	}
	if r := reps["aws-ec2"]; r.Status != apiv1.SecuritySourceStatusError || !strings.Contains(r.Message, "read-security-configuration") {
		t.Errorf("ec2: %+v", r)
	}
	if r := reps["aws-cloudtrail"]; r.Status != apiv1.SecuritySourceStatusOk || len(r.Findings) != 0 || !strings.Contains(r.Message, "logging: main") {
		t.Errorf("cloudtrail: %+v", r)
	}
	gd := reps["aws-guardduty"]
	if gd.Status != apiv1.SecuritySourceStatusOk || len(gd.Findings) != 1 {
		t.Fatalf("guardduty: %+v", gd)
	}
	if f := gd.Findings[0]; f.Severity != apiv1.SecuritySeverityHigh || f.Category != apiv1.SecurityCategoryThreat || f.Resource != "instance/i-1" ||
		!strings.Contains(f.Detail, "Seen 4 times") || !f.ObservedAt.Equal(time.Date(2026, 11, 2, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("guardduty finding: %+v", f)
	}
	hub := reps["aws-securityhub"]
	if hub.Status != apiv1.SecuritySourceStatusOk || len(hub.Findings) != 1 {
		t.Fatalf("securityhub: %+v", hub)
	}
	if f := hub.Findings[0]; f.Severity != apiv1.SecuritySeverityHigh || f.Category != apiv1.SecurityCategoryData || f.Resource != "arn:aws:s3:::shop-media" ||
		f.Check != "securityhub.security-control/S3.8" || !strings.Contains(f.Recommendation, "https://docs.aws.amazon.com/x") {
		t.Errorf("securityhub finding: %+v", f)
	}
	if r := reps["aws-s3"]; r.Status != apiv1.SecuritySourceStatusOk || len(r.Findings) != 0 {
		t.Errorf("s3: %+v", r)
	}
}

func TestAWSWithoutCredentials(t *testing.T) {
	c := newAWSCollector("ap-northeast-2", func(context.Context) (awsClients, error) {
		return awsClients{}, errors.New("no AWS credentials: set AWS_PROFILE")
	})
	for _, r := range c.Collect(t.Context()) {
		if r.Status != apiv1.SecuritySourceStatusNotConfigured || !strings.Contains(r.Message, "no AWS credentials") {
			t.Errorf("%s: %+v", r.Name, r)
		}
	}
	c = newAWSCollector("ap-northeast-2", func(context.Context) (awsClients, error) {
		return awsClients{sts: &fakeSTS{err: &smithy.GenericAPIError{Code: "ExpiredToken", Message: "expired"}}}, nil
	})
	for _, r := range c.Collect(t.Context()) {
		if r.Status != apiv1.SecuritySourceStatusError || !strings.Contains(r.Message, "credentials rejected") {
			t.Errorf("%s: %+v", r.Name, r)
		}
	}
}

func TestAWSCredentialRetrievalFailures(t *testing.T) {
	imds404 := &smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 404}}, Err: errors.New("request to EC2 IMDS failed")}
	imdsURL := "http://169.254.169.254/latest/meta-data/iam/security-credentials/"
	refresh := func(err error) error {
		return fmt.Errorf("failed to refresh cached credentials, no EC2 IMDS role found, %w", err)
	}
	for _, c := range []struct {
		name   string
		err    error
		status apiv1.SecuritySourceStatus
		msg    string
	}{
		{"no instance role", refresh(imds404), apiv1.SecuritySourceStatusNotConfigured, "no AWS credentials"},
		{"metadata service turned off", refresh(&smithy.OperationError{ServiceID: "ec2imds", OperationName: "GetMetadata",
			Err: errors.New(`access disabled to EC2 IMDS via client option, or "AWS_EC2_METADATA_DISABLED" environment variable`)}),
			apiv1.SecuritySourceStatusNotConfigured, "no AWS credentials"},
		{"metadata service timeout", refresh(&smithy.OperationError{ServiceID: "ec2imds", OperationName: "GetMetadata", Err: context.DeadlineExceeded}),
			apiv1.SecuritySourceStatusError, "timed out"},
		{"dial timeout", refresh(&url.Error{Op: "Get", URL: imdsURL, Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}}),
			apiv1.SecuritySourceStatusError, "timed out"},
		{"network error", refresh(&url.Error{Op: "Get", URL: imdsURL, Err: &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect: network is unreachable")}}),
			apiv1.SecuritySourceStatusError, "network error"},
		{"metadata service error", refresh(&smithyhttp.ResponseError{Response: &smithyhttp.Response{Response: &http.Response{StatusCode: 500}}, Err: errors.New("x")}),
			apiv1.SecuritySourceStatusError, "HTTP 500"},
		{"STS rejects the profile's role", fmt.Errorf("failed to refresh cached credentials, %w", &smithy.GenericAPIError{Code: "AccessDenied", Message: "arn:aws:iam::123456789012:role/x is not authorized"}),
			apiv1.SecuritySourceStatusError, "answered AccessDenied"},
		{"anything else", errors.New("open /home/app/.aws/sso/cache/abc.json: no such file"), apiv1.SecuritySourceStatusError, "the credential source failed"},
	} {
		t.Run(c.name, func(t *testing.T) {
			col := newAWSCollector("ap-northeast-2", func(context.Context) (awsClients, error) { return awsClients{}, credentialsError(c.err) })
			for _, r := range col.Collect(t.Context()) {
				if r.Status != c.status || !strings.Contains(r.Message, c.msg) {
					t.Errorf("%s: %s %q, want %s with %q", r.Name, r.Status, r.Message, c.status, c.msg)
				}
				if strings.ContainsAny(r.Message, "/\\") || strings.Contains(r.Message, "169.254") || strings.Contains(r.Message, "123456789012") {
					t.Errorf("%s: SDK text in the message: %q", r.Name, r.Message)
				}
			}
		})
	}
}

func TestAWSGuardDutyTruncated(t *testing.T) {
	gd := &fakeGuardDuty{detectors: []string{"det1"}, status: gdtypes.DetectorStatusEnabled, more: true, findings: []gdtypes.Finding{{
		Id: aws.String("gd1"), Type: aws.String("Recon:EC2/PortProbeUnprotectedPort"), Severity: aws.Float64(5), Title: aws.String("probe"),
	}}}
	reps := collectAWS(t, awsClients{iam: &fakeIAM{}, ec2: &fakeEC2{ebs: true}, cloudtrail: &fakeTrail{}, guardduty: gd,
		securityhub: &fakeHub{}, s3control: &fakeS3{}, sts: &fakeSTS{}})
	if r := reps["aws-guardduty"]; r.Status != apiv1.SecuritySourceStatusOk || !strings.Contains(r.Message, "only the 50 most severe are shown") {
		t.Fatalf("guardduty: %s %q", r.Status, r.Message)
	}
	gd.more = false
	reps = collectAWS(t, awsClients{iam: &fakeIAM{}, ec2: &fakeEC2{ebs: true}, cloudtrail: &fakeTrail{}, guardduty: gd,
		securityhub: &fakeHub{}, s3control: &fakeS3{}, sts: &fakeSTS{}})
	if r := reps["aws-guardduty"]; strings.Contains(r.Message, "more") {
		t.Fatalf("guardduty without more findings: %q", r.Message)
	}
}

// TestFindingIDsAreUnique collects an account where several findings share a
// check and a resource, as real accounts have: GuardDuty findings of one type
// about one instance, Security Hub findings of one control about one group
// (from two products), two ICMP rules of one security group and two access
// keys with the same last four characters.
func TestFindingIDsAreUnique(t *testing.T) {
	old := t0.Add(-200 * 24 * time.Hour)
	instance := &gdtypes.Resource{ResourceType: aws.String("Instance"), InstanceDetails: &gdtypes.InstanceDetails{InstanceId: aws.String("i-1")}}
	probe := func(id string, sev float64) gdtypes.Finding {
		return gdtypes.Finding{Id: aws.String(id), Type: aws.String("Recon:EC2/PortProbeUnprotectedPort"), Severity: aws.Float64(sev),
			Title: aws.String("Unprotected port on EC2 instance i-1 is being probed."), Description: aws.String("probed"), Resource: instance}
	}
	control := func(arn string) shtypes.AwsSecurityFinding {
		return shtypes.AwsSecurityFinding{Id: aws.String(arn), GeneratorId: aws.String("security-control/EC2.18"), Title: aws.String("Security groups should only allow unrestricted incoming traffic for authorized ports"),
			Severity: &shtypes.Severity{Label: shtypes.SeverityLabelHigh}, Resources: []shtypes.Resource{{Id: aws.String("arn:aws:ec2:ap-northeast-2:123456789012:security-group/sg-1"), Type: aws.String("AwsEc2SecurityGroup")}}}
	}
	icmp := func(typ int32) ec2types.IpPermission {
		return ec2types.IpPermission{IpProtocol: aws.String("icmp"), FromPort: aws.Int32(typ), ToPort: aws.Int32(-1), IpRanges: []ec2types.IpRange{{CidrIp: aws.String("0.0.0.0/0")}}}
	}
	reps := collectAWS(t, awsClients{
		iam: &fakeIAM{summary: map[string]int32{"AccountMFAEnabled": 1}, users: []iamtypes.User{{UserName: aws.String("ci"), Arn: aws.String("arn:aws:iam::123456789012:user/ci")}},
			keys: map[string][]iamtypes.AccessKeyMetadata{"ci": {
				{AccessKeyId: aws.String("AKIAAAAAAAAAAAAA1234"), Status: iamtypes.StatusTypeActive, CreateDate: &old},
				{AccessKeyId: aws.String("AKIABBBBBBBBBBBB1234"), Status: iamtypes.StatusTypeActive, CreateDate: &old},
			}}},
		ec2: &fakeEC2{ebs: true, groups: []ec2types.SecurityGroup{
			sg("sg-1", icmp(8), icmp(0), tcp(22, 22, "0.0.0.0/0"), tcp(443, 443, "0.0.0.0/0", "::/0")),
			sg("sg-2", icmp(8), tcp(22, 22, "::/0")),
		}},
		cloudtrail: &fakeTrail{trails: []cttypes.Trail{{Name: aws.String("main"), TrailARN: aws.String("arn:trail/main")}}, logging: map[string]bool{"arn:trail/main": true}},
		guardduty:  &fakeGuardDuty{detectors: []string{"det1"}, status: gdtypes.DetectorStatusEnabled, findings: []gdtypes.Finding{probe("gd1", 5), probe("gd2", 5), probe("gd3", 2)}},
		securityhub: &fakeHub{enabled: true, findings: map[shtypes.SeverityLabel][]shtypes.AwsSecurityFinding{shtypes.SeverityLabelHigh: {
			control("arn:aws:securityhub:ap-northeast-2:123456789012:security-control/EC2.18/finding/a1"),
			control("arn:aws:securityhub:ap-northeast-2:123456789012:subscription/aws-foundational-security-best-practices/v/1.0.0/EC2.18/finding/b2"),
		}}},
		s3control: &fakeS3{pab: &s3ctypes.PublicAccessBlockConfiguration{BlockPublicAcls: aws.Bool(true), IgnorePublicAcls: aws.Bool(true), BlockPublicPolicy: aws.Bool(true), RestrictPublicBuckets: aws.Bool(true)}},
		sts:       &fakeSTS{},
	})
	var batch []Report
	for _, name := range awsSources {
		batch = append(batch, reps[name])
	}
	rep := assemble(t0, batch).Report
	ids := map[string]string{}
	shared := map[string]int{}
	for _, f := range rep.Findings {
		what := f.Source + " " + f.Check + " " + f.Resource
		if prev, dup := ids[f.Id]; dup {
			t.Errorf("id %s for %q and %q", f.Id, prev, what)
		}
		ids[f.Id] = what
		shared[what]++
	}
	n := 0
	for _, c := range shared {
		if c > 1 {
			n++
		}
	}
	if len(rep.Findings) != 13 || n != 4 {
		t.Fatalf("%d findings, %d check and resource pairs shared; the batch needs 13 and 4:\n%v", len(rep.Findings), n, shared)
	}
	idList := func(fs []apiv1.SecurityFinding) string {
		var out []string
		for _, f := range fs {
			out = append(out, f.Id)
		}
		return strings.Join(out, ",")
	}
	if again := assemble(t0, batch).Report; idList(again.Findings) != idList(rep.Findings) {
		t.Fatal("ids differ between two collections of the same account")
	}
}
