package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
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

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// DefaultAWSRegion is the region read when PLATFORM_AWS_REGION is empty.
const DefaultAWSRegion = "ap-northeast-2"

// Limits of the AWS checks.
const (
	accessKeyMaxAge = 90 * 24 * time.Hour
	maxVendorFinds  = 50 // GuardDuty and Security Hub findings per collection, most severe first
	iamParallel     = 4  // users checked at once
)

// The SDK calls the checks make. Only Describe, List and Get calls: the
// collector never enables or changes anything, and a test fails if a method
// with another name is added here.
type (
	iamAPI interface {
		GetAccountSummary(context.Context, *iam.GetAccountSummaryInput, ...func(*iam.Options)) (*iam.GetAccountSummaryOutput, error)
		ListUsers(context.Context, *iam.ListUsersInput, ...func(*iam.Options)) (*iam.ListUsersOutput, error)
		GetLoginProfile(context.Context, *iam.GetLoginProfileInput, ...func(*iam.Options)) (*iam.GetLoginProfileOutput, error)
		ListMFADevices(context.Context, *iam.ListMFADevicesInput, ...func(*iam.Options)) (*iam.ListMFADevicesOutput, error)
		ListAccessKeys(context.Context, *iam.ListAccessKeysInput, ...func(*iam.Options)) (*iam.ListAccessKeysOutput, error)
	}
	ec2API interface {
		DescribeSecurityGroups(context.Context, *ec2.DescribeSecurityGroupsInput, ...func(*ec2.Options)) (*ec2.DescribeSecurityGroupsOutput, error)
		GetEbsEncryptionByDefault(context.Context, *ec2.GetEbsEncryptionByDefaultInput, ...func(*ec2.Options)) (*ec2.GetEbsEncryptionByDefaultOutput, error)
	}
	cloudtrailAPI interface {
		DescribeTrails(context.Context, *cloudtrail.DescribeTrailsInput, ...func(*cloudtrail.Options)) (*cloudtrail.DescribeTrailsOutput, error)
		GetTrailStatus(context.Context, *cloudtrail.GetTrailStatusInput, ...func(*cloudtrail.Options)) (*cloudtrail.GetTrailStatusOutput, error)
	}
	guarddutyAPI interface {
		ListDetectors(context.Context, *guardduty.ListDetectorsInput, ...func(*guardduty.Options)) (*guardduty.ListDetectorsOutput, error)
		GetDetector(context.Context, *guardduty.GetDetectorInput, ...func(*guardduty.Options)) (*guardduty.GetDetectorOutput, error)
		ListFindings(context.Context, *guardduty.ListFindingsInput, ...func(*guardduty.Options)) (*guardduty.ListFindingsOutput, error)
		GetFindings(context.Context, *guardduty.GetFindingsInput, ...func(*guardduty.Options)) (*guardduty.GetFindingsOutput, error)
	}
	securityhubAPI interface {
		DescribeHub(context.Context, *securityhub.DescribeHubInput, ...func(*securityhub.Options)) (*securityhub.DescribeHubOutput, error)
		GetFindings(context.Context, *securityhub.GetFindingsInput, ...func(*securityhub.Options)) (*securityhub.GetFindingsOutput, error)
	}
	s3controlAPI interface {
		GetPublicAccessBlock(context.Context, *s3control.GetPublicAccessBlockInput, ...func(*s3control.Options)) (*s3control.GetPublicAccessBlockOutput, error)
	}
	stsAPI interface {
		GetCallerIdentity(context.Context, *sts.GetCallerIdentityInput, ...func(*sts.Options)) (*sts.GetCallerIdentityOutput, error)
	}
)

// awsClients are the clients for one account and region.
type awsClients struct {
	iam         iamAPI
	ec2         ec2API
	cloudtrail  cloudtrailAPI
	guardduty   guarddutyAPI
	securityhub securityhubAPI
	s3control   s3controlAPI
	sts         stsAPI
}

// AWSCollector checks the AWS account the server's credentials belong to, in
// one region: IAM (root MFA and keys, console users without MFA, old access
// keys), security groups open to the internet, EBS default encryption,
// CloudTrail, GuardDuty and its findings, Security Hub and its findings, and
// the S3 account public access block. Credentials come from the SDK's
// default chain (environment, AWS_PROFILE, instance role).
type AWSCollector struct {
	region string
	load   func(ctx context.Context) (awsClients, error)
	now    func() time.Time

	mu      sync.Mutex
	clients *awsClients // loaded on first use; a failed load is retried
}

// NewAWSCollector reads region (DefaultAWSRegion when empty).
func NewAWSCollector(region string) *AWSCollector {
	if region == "" {
		region = DefaultAWSRegion
	}
	return newAWSCollector(region, func(ctx context.Context) (awsClients, error) { return loadAWS(ctx, region) })
}

func newAWSCollector(region string, load func(context.Context) (awsClients, error)) *AWSCollector {
	return &AWSCollector{region: region, load: load, now: func() time.Time { return time.Now().UTC() }}
}

func loadAWS(ctx context.Context, region string) (awsClients, error) {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return awsClients{}, fmt.Errorf("AWS SDK configuration could not be loaded: %s", shorten(err.Error(), 160))
	}
	if _, err := cfg.Credentials.Retrieve(ctx); err != nil {
		return awsClients{}, credentialsError(err)
	}
	return awsClients{
		iam:         iam.NewFromConfig(cfg),
		ec2:         ec2.NewFromConfig(cfg),
		cloudtrail:  cloudtrail.NewFromConfig(cfg),
		guardduty:   guardduty.NewFromConfig(cfg),
		securityhub: securityhub.NewFromConfig(cfg),
		s3control:   s3control.NewFromConfig(cfg),
		sts:         sts.NewFromConfig(cfg),
	}, nil
}

const noCredentials = "no AWS credentials: set AWS_PROFILE or AWS_ACCESS_KEY_ID, or run on a host whose instance role can read the security configuration"

// retrievalError is a failure to fetch credentials that exist or may exist,
// such as an instance metadata service that timed out. It is reported as
// error, not not_configured.
type retrievalError struct{ msg string }

func (e *retrievalError) Error() string { return e.msg }

// credentialsError explains why the default chain gave no credentials. Only
// "there are none" stays a plain error (reported as not_configured): the
// instance metadata service answering that the instance has no role (404), or
// being turned off. A timeout, a network failure or a rejection while
// fetching them is a retrievalError whose message names the kind of failure,
// not the SDK's text, which can carry endpoints, file paths and profile names.
func credentialsError(err error) error {
	const prefix = "AWS credentials could not be retrieved: "
	var status interface{ HTTPStatusCode() int }
	var ae smithy.APIError
	var ne net.Error
	switch {
	case errors.As(err, &status) && status.HTTPStatusCode() == http.StatusNotFound,
		strings.Contains(err.Error(), "AWS_EC2_METADATA_DISABLED"):
		return errors.New(noCredentials)
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || (errors.As(err, &ne) && ne.Timeout()):
		return &retrievalError{prefix + "the credential source timed out (on EC2: is the instance role attached, and can containers reach the metadata service?)"}
	case errors.As(err, &ne):
		return &retrievalError{prefix + "network error reaching the credential source"}
	case errors.As(err, &ae):
		return &retrievalError{prefix + "the credential source answered " + ae.ErrorCode()}
	case errors.As(err, &status):
		return &retrievalError{prefix + fmt.Sprintf("the credential source answered HTTP %d", status.HTTPStatusCode())}
	}
	return &retrievalError{prefix + "the credential source failed"}
}

var awsSources = []string{"aws-iam", "aws-ec2", "aws-cloudtrail", "aws-guardduty", "aws-securityhub", "aws-s3"}

func awsSource(name string) Source {
	return Source{Name: name, Provider: apiv1.SecurityProviderAws, Environment: EnvAWSAccount}
}

// Sources are one per AWS service.
func (c *AWSCollector) Sources() []Source {
	out := make([]Source, len(awsSources))
	for i, n := range awsSources {
		out[i] = awsSource(n)
	}
	return out
}

func (c *AWSCollector) getClients(ctx context.Context) (awsClients, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.clients == nil {
		cl, err := c.load(ctx)
		if err != nil {
			return awsClients{}, err
		}
		c.clients = &cl
	}
	return *c.clients, nil
}

// Collect runs the checks concurrently. Without credentials every source is
// not_configured; with credentials that could not be fetched or were
// rejected, every source is an error.
func (c *AWSCollector) Collect(ctx context.Context) []Report {
	cl, err := c.getClients(ctx)
	var re *retrievalError
	switch {
	case errors.As(err, &re):
		return failed(c.Sources(), apiv1.SecuritySourceStatusError, err.Error())
	case err != nil:
		return failed(c.Sources(), apiv1.SecuritySourceStatusNotConfigured, err.Error())
	}
	id, err := cl.sts.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		st, msg := awsError("sts GetCallerIdentity", err)
		return failed(c.Sources(), st, msg)
	}
	account := aws.ToString(id.Account)
	checks := []func(context.Context, awsClients, string) Report{
		c.checkIAM, c.checkEC2, c.checkCloudTrail, c.checkGuardDuty, c.checkSecurityHub, c.checkS3,
	}
	out := make([]Report, len(checks))
	var wg sync.WaitGroup
	for i, check := range checks {
		wg.Go(func() { out[i] = check(ctx, cl, account) })
	}
	wg.Wait()
	return out
}

// partial accumulates one source's findings and failures; checks may add
// from several goroutines.
type partial struct {
	mu       sync.Mutex
	src      Source
	findings []apiv1.SecurityFinding
	evals    []Evaluation
	errs     []string
	status   apiv1.SecuritySourceStatus // worst failure so far
	message  string                     // what was read, when nothing failed
}

func newPartial(src Source) *partial { return &partial{src: src} }

func (p *partial) add(f apiv1.SecurityFinding) {
	p.mu.Lock()
	p.findings = append(p.findings, f)
	p.mu.Unlock()
}

// eval records one control evaluation (ADR 0034).
func (p *partial) eval(check, asset string, result apiv1.SecurityControlResult, reason string) {
	p.mu.Lock()
	p.evals = append(p.evals, Evaluation{Check: check, Asset: asset, Result: result, Reason: reason})
	p.mu.Unlock()
}

// unknown records that a check could not read asset because op failed.
func (p *partial) unknown(check, asset, op string, err error) {
	_, msg := awsError(op, err)
	p.eval(check, asset, apiv1.SecurityControlResultUnknown, msg)
}

func (p *partial) fail(op string, err error) {
	st, msg := awsError(op, err)
	p.mu.Lock()
	defer p.mu.Unlock()
	if !slices.Contains(p.errs, msg) {
		p.errs = append(p.errs, msg)
	}
	if p.status != apiv1.SecuritySourceStatusError {
		p.status = st
	}
}

func (p *partial) failed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.errs) > 0
}

func (p *partial) report() Report {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := Report{Source: p.src, Findings: p.findings, Evaluations: p.evals, Status: apiv1.SecuritySourceStatusOk, Message: p.message}
	if len(p.errs) > 0 {
		r.Status, r.Message = p.status, strings.Join(p.errs, "; ")
	}
	return r
}

// awsError turns an SDK error into a status and a short message. Access
// problems are errors that name the missing policy; timeouts and network
// failures mean the service was unavailable.
func awsError(op string, err error) (apiv1.SecuritySourceStatus, string) {
	var ae smithy.APIError
	var ne net.Error
	switch {
	case errors.As(err, &ae):
		switch code := ae.ErrorCode(); code {
		case "AccessDenied", "AccessDeniedException", "UnauthorizedOperation", "AuthorizationError", "UnauthorizedAccess":
			return apiv1.SecuritySourceStatusError, fmt.Sprintf("%s: access denied (%s); the server's AWS identity needs this read in its read-security-configuration policy (deploy/aws/main.tf)", op, code)
		case "InvalidClientTokenId", "ExpiredToken", "ExpiredTokenException", "UnrecognizedClientException", "SignatureDoesNotMatch":
			return apiv1.SecuritySourceStatusError, fmt.Sprintf("%s: AWS credentials rejected (%s)", op, code)
		default:
			return apiv1.SecuritySourceStatusError, fmt.Sprintf("%s: %s: %s", op, code, shorten(ae.ErrorMessage(), 160))
		}
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return apiv1.SecuritySourceStatusUnavailable, op + ": timed out"
	case errors.As(err, &ne):
		return apiv1.SecuritySourceStatusUnavailable, op + ": " + shorten(ne.Error(), 160)
	}
	return apiv1.SecuritySourceStatusError, op + ": " + shorten(err.Error(), 160)
}

// --- IAM ---

func (c *AWSCollector) checkIAM(ctx context.Context, cl awsClients, account string) Report {
	p := newPartial(awsSource("aws-iam"))
	root := "arn:aws:iam::" + account + ":root"
	if sum, err := cl.iam.GetAccountSummary(ctx, &iam.GetAccountSummaryInput{}); err != nil {
		p.fail("iam GetAccountSummary", err)
		p.unknown("aws.root-mfa", root, "iam GetAccountSummary", err)
		p.unknown("aws.root-access-key", root, "iam GetAccountSummary", err)
	} else {
		if sum.SummaryMap[string(iamtypes.SummaryKeyTypeAccountMFAEnabled)] == 0 {
			p.eval("aws.root-mfa", root, apiv1.SecurityControlResultFail, "the account summary reports AccountMFAEnabled 0")
			p.add(apiv1.SecurityFinding{Check: "aws.root-mfa", Category: apiv1.SecurityCategoryIdentity, Severity: apiv1.SecuritySeverityCritical,
				Title: "Root user has no MFA", Resource: root,
				Detail:         "The account's root user can sign in with a password alone.",
				Recommendation: "Add an MFA device to the root user and keep its credentials offline."})
		} else {
			p.eval("aws.root-mfa", root, apiv1.SecurityControlResultPass, "the account summary reports AccountMFAEnabled 1")
		}
		if sum.SummaryMap[string(iamtypes.SummaryKeyTypeAccountAccessKeysPresent)] > 0 {
			p.eval("aws.root-access-key", root, apiv1.SecurityControlResultFail, "the account summary reports root access keys")
			p.add(apiv1.SecurityFinding{Check: "aws.root-access-key", Category: apiv1.SecurityCategoryIdentity, Severity: apiv1.SecuritySeverityCritical,
				Title: "Root user has access keys", Resource: root,
				Detail:         "The root user has an access key, which can do anything in the account and cannot be limited by a policy.",
				Recommendation: "Delete the root access keys and use IAM roles."})
		} else {
			p.eval("aws.root-access-key", root, apiv1.SecurityControlResultPass, "the account summary reports no root access keys")
		}
	}

	var users []iamtypes.User
	pg := iam.NewListUsersPaginator(cl.iam, &iam.ListUsersInput{})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			p.fail("iam ListUsers", err)
			p.unknown("aws.user-mfa", AllAssets, "iam ListUsers", err)
			p.unknown("aws.access-key-age", AllAssets, "iam ListUsers", err)
			break
		}
		users = append(users, page.Users...)
	}
	sem := make(chan struct{}, iamParallel)
	var wg sync.WaitGroup
	for _, u := range users {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			c.checkUser(ctx, cl.iam, u, p)
		})
	}
	wg.Wait()
	p.message = many(len(users), "IAM user", "IAM users") + " and the root user checked"
	return p.report()
}

func (c *AWSCollector) checkUser(ctx context.Context, api iamAPI, u iamtypes.User, p *partial) {
	name, arn := aws.ToString(u.UserName), aws.ToString(u.Arn)
	_, err := api.GetLoginProfile(ctx, &iam.GetLoginProfileInput{UserName: &name})
	var noProfile *iamtypes.NoSuchEntityException
	switch {
	case errors.As(err, &noProfile):
		p.eval("aws.user-mfa", arn, apiv1.SecurityControlResultNotApplicable, "the user has no console password")
	case err != nil:
		p.fail("iam GetLoginProfile", err)
		p.unknown("aws.user-mfa", arn, "iam GetLoginProfile", err)
	default:
		mfa, err := api.ListMFADevices(ctx, &iam.ListMFADevicesInput{UserName: &name})
		switch {
		case err != nil:
			p.fail("iam ListMFADevices", err)
			p.unknown("aws.user-mfa", arn, "iam ListMFADevices", err)
		case len(mfa.MFADevices) > 0:
			p.eval("aws.user-mfa", arn, apiv1.SecurityControlResultPass, fmt.Sprintf("the user has a console password and %s", many(len(mfa.MFADevices), "MFA device", "MFA devices")))
		default:
			p.eval("aws.user-mfa", arn, apiv1.SecurityControlResultFail, "the user has a console password and no MFA device")
			p.add(apiv1.SecurityFinding{Check: "aws.user-mfa", Category: apiv1.SecurityCategoryIdentity, Severity: apiv1.SecuritySeverityMedium,
				Title: "Console user without MFA", Resource: arn,
				Detail:         fmt.Sprintf("IAM user %s can sign in to the console with a password and has no MFA device.", name),
				Recommendation: "Require MFA for the user, or remove the console password and use SSO."})
		}
	}

	now := c.now()
	active, old := 0, 0
	pg := iam.NewListAccessKeysPaginator(api, &iam.ListAccessKeysInput{UserName: &name})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			p.fail("iam ListAccessKeys", err)
			p.unknown("aws.access-key-age", arn, "iam ListAccessKeys", err)
			return
		}
		for _, k := range page.AccessKeyMetadata {
			if k.Status == iamtypes.StatusTypeActive {
				active++
			}
			if k.Status != iamtypes.StatusTypeActive || k.CreateDate == nil || now.Sub(*k.CreateDate) <= accessKeyMaxAge {
				continue
			}
			old++
			key := maskKey(aws.ToString(k.AccessKeyId))
			// The full key id only goes into the hashed finding id.
			p.add(apiv1.SecurityFinding{Id: aws.ToString(k.AccessKeyId), Check: "aws.access-key-age", Category: apiv1.SecurityCategoryIdentity, Severity: apiv1.SecuritySeverityMedium,
				Title: "Access key older than 90 days", Resource: arn + "/access-key/" + key,
				Detail:         fmt.Sprintf("Active access key %s of IAM user %s was created %d days ago.", key, name, int(now.Sub(*k.CreateDate).Hours()/24)),
				Recommendation: "Rotate the key, or replace it with a role or SSO credentials."})
		}
	}
	switch {
	case active == 0:
		p.eval("aws.access-key-age", arn, apiv1.SecurityControlResultNotApplicable, "the user has no active access key")
	case old > 0:
		p.eval("aws.access-key-age", arn, apiv1.SecurityControlResultFail, fmt.Sprintf("%d of %s are older than 90 days", old, many(active, "active access key", "active access keys")))
	default:
		p.eval("aws.access-key-age", arn, apiv1.SecurityControlResultPass, many(active, "active access key is", "active access keys are")+" 90 days old or younger")
	}
}

// maskKey keeps the last four characters of an access key id.
func maskKey(id string) string {
	if len(id) <= 4 {
		return "****"
	}
	return "****" + id[len(id)-4:]
}

// --- EC2 ---

func (c *AWSCollector) checkEC2(ctx context.Context, cl awsClients, _ string) Report {
	p := newPartial(awsSource("aws-ec2"))
	n := 0
	pg := ec2.NewDescribeSecurityGroupsPaginator(cl.ec2, &ec2.DescribeSecurityGroupsInput{})
	for pg.HasMorePages() {
		page, err := pg.NextPage(ctx)
		if err != nil {
			p.fail("ec2 DescribeSecurityGroups", err)
			p.unknown("aws.sg-open-ingress", AllAssets, "ec2 DescribeSecurityGroups", err)
			break
		}
		for _, sg := range page.SecurityGroups {
			n++
			failing, open := 0, 0
			for _, perm := range sg.IpPermissions {
				if f, ok := openIngress(sg, perm); ok {
					p.add(f)
					open++
					if f.Severity != apiv1.SecuritySeverityInfo {
						failing++
					}
				}
			}
			id := aws.ToString(sg.GroupId)
			switch {
			case failing > 0:
				p.eval("aws.sg-open-ingress", id, apiv1.SecurityControlResultFail, fmt.Sprintf("%s open to 0.0.0.0/0 or ::/0 beyond TCP 80 and 443", many(failing, "ingress rule is", "ingress rules are")))
			case open > 0:
				p.eval("aws.sg-open-ingress", id, apiv1.SecurityControlResultPass, "only TCP 80 or 443 is open to the internet")
			default:
				p.eval("aws.sg-open-ingress", id, apiv1.SecurityControlResultPass, "no ingress rule from 0.0.0.0/0 or ::/0")
			}
		}
	}
	ebs := "ebs-encryption-by-default/" + c.region
	if out, err := cl.ec2.GetEbsEncryptionByDefault(ctx, &ec2.GetEbsEncryptionByDefaultInput{}); err != nil {
		p.fail("ec2 GetEbsEncryptionByDefault", err)
		p.unknown("aws.ebs-default-encryption", ebs, "ec2 GetEbsEncryptionByDefault", err)
	} else if aws.ToBool(out.EbsEncryptionByDefault) {
		p.eval("aws.ebs-default-encryption", ebs, apiv1.SecurityControlResultPass, "EBS encryption by default is on in "+c.region)
	} else {
		p.eval("aws.ebs-default-encryption", ebs, apiv1.SecurityControlResultFail, "EBS encryption by default is off in "+c.region)
		p.add(apiv1.SecurityFinding{Check: "aws.ebs-default-encryption", Category: apiv1.SecurityCategoryData, Severity: apiv1.SecuritySeverityMedium,
			Title: "EBS encryption by default is off", Resource: ebs,
			Detail:         fmt.Sprintf("New EBS volumes in %s are not encrypted unless each one asks for it.", c.region),
			Recommendation: "Turn on EBS encryption by default for the region."})
	}
	p.message = fmt.Sprintf("%s and EBS defaults in %s checked", many(n, "security group", "security groups"), c.region)
	return p.report()
}

// openIngress reports an ingress rule open to 0.0.0.0/0 or ::/0 (ADR 0034):
// critical for all traffic or a TCP range with a port AWS Security Hub
// EC2.19 blocks, high for another TCP port (EC2.18), info for TCP 80 or 443
// alone (EC2.18's authorized ports), medium for UDP and low for ICMP (the
// platform's rules: neither control sets them).
func openIngress(sg ec2types.SecurityGroup, perm ec2types.IpPermission) (apiv1.SecurityFinding, bool) {
	var from []string
	for _, r := range perm.IpRanges {
		if aws.ToString(r.CidrIp) == "0.0.0.0/0" {
			from = append(from, "0.0.0.0/0")
		}
	}
	for _, r := range perm.Ipv6Ranges {
		if aws.ToString(r.CidrIpv6) == "::/0" {
			from = append(from, "::/0")
		}
	}
	if len(from) == 0 {
		return apiv1.SecurityFinding{}, false
	}
	proto := protocolName(aws.ToString(perm.IpProtocol))
	lo, hi := aws.ToInt32(perm.FromPort), aws.ToInt32(perm.ToPort)
	ports := strconv.Itoa(int(lo))
	if hi != lo {
		ports += "-" + strconv.Itoa(int(hi))
	}
	what := proto + " " + ports
	sev := apiv1.SecuritySeverityMedium
	basis := platformRule("unrestricted UDP: neither EC2.18 (no default authorized UDP ports) nor EC2.19 sets a level, so the platform uses medium")
	rec := "Restrict the source range to the addresses that need it."
	blocked := slices.ContainsFunc(ec219Ports, func(p int32) bool { return lo <= p && p <= hi })
	switch {
	case proto == "all":
		what, ports, sev, basis = "all traffic", "all", apiv1.SecuritySeverityCritical, vendorControl(awsEC219)
	case proto == "icmp" || proto == "icmpv6":
		what, ports, sev = proto, "all", apiv1.SecuritySeverityLow
		basis = platformRule("unrestricted ICMP: no AWS control sets a level, so the platform uses low")
	case proto == "tcp" && blocked:
		sev, basis = apiv1.SecuritySeverityCritical, vendorControl(awsEC219)
	case proto == "tcp" && lo == hi && (lo == 80 || lo == 443):
		sev = apiv1.SecuritySeverityInfo
		basis = platformRule("TCP 80 or 443 is an authorized port of EC2.18; the platform keeps it as information")
		rec = "Expected for a public web endpoint; keep it to ports 80 and 443."
	case proto == "tcp":
		sev, basis = apiv1.SecuritySeverityHigh, vendorControl(awsEC218)
	}
	if sev == apiv1.SecuritySeverityHigh || sev == apiv1.SecuritySeverityCritical {
		rec = "Allow only known addresses, or reach the port through SSM Session Manager, Tailscale or the gateway."
	}
	id := aws.ToString(sg.GroupId)
	// Two ICMP rules both read "icmp/all": the raw protocol, ports and CIDRs
	// tell them apart.
	key := fmt.Sprintf("%s/%d-%d/%s", aws.ToString(perm.IpProtocol), lo, hi, strings.Join(from, ","))
	return apiv1.SecurityFinding{Id: key, Check: "aws.sg-open-ingress", Category: apiv1.SecurityCategoryNetwork, Severity: sev, SeverityBasis: &basis,
		Title: "Security group allows " + what + " from the internet", Resource: id + "/" + proto + "/" + ports,
		Detail: fmt.Sprintf("Security group %s (%s) in %s allows inbound %s from %s.",
			id, aws.ToString(sg.GroupName), aws.ToString(sg.VpcId), what, strings.Join(from, " and ")),
		Recommendation: rec}, true
}

func protocolName(p string) string {
	switch p {
	case "-1":
		return "all"
	case "6":
		return "tcp"
	case "17":
		return "udp"
	case "1":
		return "icmp"
	case "58":
		return "icmpv6"
	}
	return p
}

// --- CloudTrail ---

func (c *AWSCollector) checkCloudTrail(ctx context.Context, cl awsClients, _ string) Report {
	p := newPartial(awsSource("aws-cloudtrail"))
	resource := "cloudtrail/" + c.region
	out, err := cl.cloudtrail.DescribeTrails(ctx, &cloudtrail.DescribeTrailsInput{IncludeShadowTrails: aws.Bool(true)})
	if err != nil {
		p.fail("cloudtrail DescribeTrails", err)
		p.unknown("aws.cloudtrail-logging", resource, "cloudtrail DescribeTrails", err)
		return p.report()
	}
	var logging []string
	multiRegion := false
	for _, t := range out.TrailList {
		st, err := cl.cloudtrail.GetTrailStatus(ctx, &cloudtrail.GetTrailStatusInput{Name: t.TrailARN})
		if err != nil {
			p.fail("cloudtrail GetTrailStatus", err)
			continue
		}
		if aws.ToBool(st.IsLogging) {
			logging = append(logging, aws.ToString(t.Name))
			multiRegion = multiRegion || aws.ToBool(t.IsMultiRegionTrail)
		}
	}
	switch {
	case len(logging) > 0:
		reason := "logging: " + strings.Join(logging, ", ")
		if multiRegion {
			reason += " (one is multi-region)"
		} else {
			reason += " (none is multi-region)"
		}
		p.eval("aws.cloudtrail-logging", resource, apiv1.SecurityControlResultPass, reason+"; event selectors are not read")
	case len(out.TrailList) == 0:
		p.eval("aws.cloudtrail-logging", resource, apiv1.SecurityControlResultFail, "no trail applies to "+c.region)
	case p.failed():
		p.eval("aws.cloudtrail-logging", resource, apiv1.SecurityControlResultUnknown, "no trail read so far is logging, and the status of some trails could not be read")
	default:
		p.eval("aws.cloudtrail-logging", resource, apiv1.SecurityControlResultFail, many(len(out.TrailList), "trail applies", "trails apply")+" and none is logging")
	}
	switch {
	case len(out.TrailList) == 0:
		p.add(apiv1.SecurityFinding{Check: "aws.cloudtrail-logging", Category: apiv1.SecurityCategoryLogging, Severity: apiv1.SecuritySeverityHigh,
			Title: "No CloudTrail trail", Resource: resource,
			Detail:         fmt.Sprintf("No trail applies to %s, so API activity is kept only in the 90-day event history and not delivered anywhere.", c.region),
			Recommendation: "Create a multi-region trail that logs management events to an S3 bucket."})
	case len(logging) == 0 && !p.failed():
		p.add(apiv1.SecurityFinding{Check: "aws.cloudtrail-logging", Category: apiv1.SecurityCategoryLogging, Severity: apiv1.SecuritySeverityHigh,
			Title: "No CloudTrail trail is logging", Resource: resource,
			Detail:         fmt.Sprintf("%d trails apply to %s and none of them is logging.", len(out.TrailList), c.region),
			Recommendation: "Start logging on a multi-region trail."})
	}
	p.message = many(len(out.TrailList), "trail", "trails") + ", logging: " + strings.Join(logging, ", ")
	if len(logging) == 0 {
		p.message = many(len(out.TrailList), "trail", "trails") + ", none logging"
	}
	return p.report()
}

// --- GuardDuty ---

func (c *AWSCollector) checkGuardDuty(ctx context.Context, cl awsClients, _ string) Report {
	p := newPartial(awsSource("aws-guardduty"))
	resource := "guardduty/" + c.region
	det, err := cl.guardduty.ListDetectors(ctx, &guardduty.ListDetectorsInput{})
	if err != nil {
		p.fail("guardduty ListDetectors", err)
		p.unknown("aws.guardduty-enabled", resource, "guardduty ListDetectors", err)
		return p.report()
	}
	if len(det.DetectorIds) == 0 {
		p.eval("aws.guardduty-enabled", resource, apiv1.SecurityControlResultFail, "no detector in "+c.region)
		p.add(apiv1.SecurityFinding{Check: "aws.guardduty-enabled", Category: apiv1.SecurityCategoryThreat, Severity: apiv1.SecuritySeverityHigh,
			Title: "GuardDuty disabled", Resource: resource,
			Detail:         fmt.Sprintf("There is no GuardDuty detector in %s, so CloudTrail, VPC flow and DNS logs are not watched for threats.", c.region),
			Recommendation: fmt.Sprintf("Enable GuardDuty in %s (30-day free trial, then billed by log volume).", c.region)})
		p.message = "no detector in " + c.region
		return p.report()
	}
	id := det.DetectorIds[0] // one detector per account and region
	status := "status unknown"
	if d, err := cl.guardduty.GetDetector(ctx, &guardduty.GetDetectorInput{DetectorId: &id}); err != nil {
		p.fail("guardduty GetDetector", err)
		p.unknown("aws.guardduty-enabled", resource, "guardduty GetDetector", err)
	} else {
		status = strings.ToLower(string(d.Status))
		if d.Status == gdtypes.DetectorStatusEnabled {
			p.eval("aws.guardduty-enabled", resource, apiv1.SecurityControlResultPass, "the detector in "+c.region+" is enabled")
		} else {
			p.eval("aws.guardduty-enabled", resource, apiv1.SecurityControlResultFail, "the detector in "+c.region+" is "+status)
			p.add(apiv1.SecurityFinding{Check: "aws.guardduty-enabled", Category: apiv1.SecurityCategoryThreat, Severity: apiv1.SecuritySeverityHigh,
				Title: "GuardDuty detector suspended", Resource: resource,
				Detail:         fmt.Sprintf("The GuardDuty detector in %s is %s and produces no findings.", c.region, status),
				Recommendation: "Resume the detector."})
		}
	}
	list, err := cl.guardduty.ListFindings(ctx, &guardduty.ListFindingsInput{
		DetectorId:      &id,
		MaxResults:      aws.Int32(maxVendorFinds),
		FindingCriteria: &gdtypes.FindingCriteria{Criterion: map[string]gdtypes.Condition{"service.archived": {Equals: []string{"false"}}}},
		SortCriteria:    &gdtypes.SortCriteria{AttributeName: aws.String("severity"), OrderBy: gdtypes.OrderByDesc},
	})
	n, more := 0, false
	switch {
	case err != nil:
		p.fail("guardduty ListFindings", err)
	case len(list.FindingIds) > 0:
		more = aws.ToString(list.NextToken) != ""
		got, err := cl.guardduty.GetFindings(ctx, &guardduty.GetFindingsInput{DetectorId: &id, FindingIds: list.FindingIds})
		if err != nil {
			p.fail("guardduty GetFindings", err)
			break
		}
		for _, f := range got.Findings {
			p.add(guardDutyFinding(f))
			n++
		}
	}
	p.message = fmt.Sprintf("detector %s, %s read", status, many(n, "active finding", "active findings"))
	if more {
		p.message += fmt.Sprintf("; there are more: only the %d most severe are shown", maxVendorFinds)
	}
	return p.report()
}

// guardDutySeverity maps GuardDuty's 1.0-10.0 scale to its own labels.
func guardDutySeverity(s float64) apiv1.SecuritySeverity {
	switch {
	case s >= 9:
		return apiv1.SecuritySeverityCritical
	case s >= 7:
		return apiv1.SecuritySeverityHigh
	case s >= 4:
		return apiv1.SecuritySeverityMedium
	}
	return apiv1.SecuritySeverityLow
}

func guardDutyFinding(f gdtypes.Finding) apiv1.SecurityFinding {
	score := aws.ToFloat64(f.Severity)
	basis := Basis(apiv1.SecuritySeverityBasisTypeVendor, IssuerGuardDuty, "GuardDuty's severity bands: 1.0-3.9 low, 4.0-6.9 medium, 7.0-8.9 high, 9.0-10.0 critical")
	basis.SourceScore, basis.SourceFindingId = &score, f.Id
	gdURL := "https://docs.aws.amazon.com/guardduty/latest/ug/guardduty_findings-severity.html"
	basis.SourceUrl = &gdURL
	out := apiv1.SecurityFinding{
		SeverityBasis:  &basis,
		Id:             aws.ToString(f.Id), // one instance can have several findings of a type
		Check:          "guardduty." + aws.ToString(f.Type),
		Category:       apiv1.SecurityCategoryThreat,
		Severity:       guardDutySeverity(aws.ToFloat64(f.Severity)),
		Title:          aws.ToString(f.Title),
		Resource:       guardDutyResource(f),
		Detail:         shorten(aws.ToString(f.Description), 400),
		Recommendation: "Investigate the finding in GuardDuty; archive it if the activity is expected.",
	}
	if f.Service != nil && aws.ToInt32(f.Service.Count) > 1 {
		out.Detail += fmt.Sprintf(" Seen %d times.", aws.ToInt32(f.Service.Count))
	}
	if t, err := time.Parse(time.RFC3339, aws.ToString(f.UpdatedAt)); err == nil {
		out.ObservedAt = t.UTC()
	}
	return out
}

func guardDutyResource(f gdtypes.Finding) string {
	r := f.Resource
	switch {
	case r == nil:
		return aws.ToString(f.Id)
	case r.InstanceDetails != nil && r.InstanceDetails.InstanceId != nil:
		return "instance/" + aws.ToString(r.InstanceDetails.InstanceId)
	case r.AccessKeyDetails != nil && r.AccessKeyDetails.UserName != nil:
		return "iam-user/" + aws.ToString(r.AccessKeyDetails.UserName) + "/access-key/" + maskKey(aws.ToString(r.AccessKeyDetails.AccessKeyId))
	case r.EksClusterDetails != nil && r.EksClusterDetails.Name != nil:
		return "eks-cluster/" + aws.ToString(r.EksClusterDetails.Name)
	case r.ResourceType != nil:
		return aws.ToString(r.ResourceType) + "/" + aws.ToString(f.Id)
	}
	return aws.ToString(f.Id)
}

// --- Security Hub ---

func (c *AWSCollector) checkSecurityHub(ctx context.Context, cl awsClients, _ string) Report {
	p := newPartial(awsSource("aws-securityhub"))
	_, err := cl.securityhub.DescribeHub(ctx, &securityhub.DescribeHubInput{})
	var notEnabled *shtypes.InvalidAccessException
	var notFound *shtypes.ResourceNotFoundException
	switch {
	case errors.As(err, &notEnabled) || errors.As(err, &notFound):
		p.eval("aws.securityhub-enabled", "securityhub/"+c.region, apiv1.SecurityControlResultFail, "Security Hub is not enabled in "+c.region)
		p.add(apiv1.SecurityFinding{Check: "aws.securityhub-enabled", Category: apiv1.SecurityCategoryLogging, Severity: apiv1.SecuritySeverityInfo,
			Title: "Security Hub is not enabled", Resource: "securityhub/" + c.region,
			Detail:         fmt.Sprintf("Security Hub is not enabled in %s, so the AWS Foundational Security Best Practices are not evaluated.", c.region),
			Recommendation: "Enable Security Hub with the AWS Foundational Security Best Practices standard if posture checks are wanted (billed per check)."})
		p.message = "not enabled in " + c.region
		return p.report()
	case err != nil:
		p.fail("securityhub DescribeHub", err)
		p.unknown("aws.securityhub-enabled", "securityhub/"+c.region, "securityhub DescribeHub", err)
		return p.report()
	}
	p.eval("aws.securityhub-enabled", "securityhub/"+c.region, apiv1.SecurityControlResultPass, "Security Hub is enabled in "+c.region)
	// Most severe first, without the GuardDuty findings read above.
	eq := func(vs ...string) []shtypes.StringFilter {
		out := make([]shtypes.StringFilter, len(vs))
		for i, v := range vs {
			out[i] = shtypes.StringFilter{Comparison: shtypes.StringFilterComparisonEquals, Value: aws.String(v)}
		}
		return out
	}
	n := 0
	for _, label := range []shtypes.SeverityLabel{shtypes.SeverityLabelCritical, shtypes.SeverityLabelHigh, shtypes.SeverityLabelMedium, shtypes.SeverityLabelLow} {
		out, err := cl.securityhub.GetFindings(ctx, &securityhub.GetFindingsInput{
			MaxResults: aws.Int32(int32(maxVendorFinds - n)),
			Filters: &shtypes.AwsSecurityFindingFilters{
				RecordState:    eq("ACTIVE"),
				WorkflowStatus: eq("NEW", "NOTIFIED"),
				SeverityLabel:  eq(string(label)),
				ProductName:    []shtypes.StringFilter{{Comparison: shtypes.StringFilterComparisonNotEquals, Value: aws.String("GuardDuty")}},
			},
		})
		if err != nil {
			p.fail("securityhub GetFindings", err)
			break
		}
		for _, f := range out.Findings {
			p.add(hubFinding(f))
			n++
		}
		if n >= maxVendorFinds {
			break
		}
	}
	p.message = fmt.Sprintf("enabled, %s read (most severe first, at most %d)", many(n, "active finding", "active findings"), maxVendorFinds)
	return p.report()
}

var hubSeverity = map[shtypes.SeverityLabel]apiv1.SecuritySeverity{
	shtypes.SeverityLabelCritical: apiv1.SecuritySeverityCritical, shtypes.SeverityLabelHigh: apiv1.SecuritySeverityHigh,
	shtypes.SeverityLabelMedium: apiv1.SecuritySeverityMedium, shtypes.SeverityLabelLow: apiv1.SecuritySeverityLow,
	shtypes.SeverityLabelInformational: apiv1.SecuritySeverityInfo,
}

func hubFinding(f shtypes.AwsSecurityFinding) apiv1.SecurityFinding {
	sev := apiv1.SecuritySeverityInfo
	basis := Basis(apiv1.SecuritySeverityBasisTypeVendor, IssuerSecurityHub, "the finding's own severity label")
	basis.SourceFindingId = f.Id
	if f.Severity != nil {
		if s, ok := hubSeverity[f.Severity.Label]; ok {
			sev = s
		}
		label := string(f.Severity.Label)
		basis.SourceSeverity = &label
		if f.Severity.Normalized != nil {
			n := float64(*f.Severity.Normalized)
			basis.SourceScore = &n
		}
	}
	if f.Compliance != nil && f.Compliance.SecurityControlId != nil {
		r := "the finding's own severity label; Security Hub control " + aws.ToString(f.Compliance.SecurityControlId)
		basis.Reason = &r
	}
	resource, rtype := aws.ToString(f.Id), ""
	if len(f.Resources) > 0 {
		resource, rtype = aws.ToString(f.Resources[0].Id), aws.ToString(f.Resources[0].Type)
	}
	rec := "See the finding in Security Hub."
	if f.Remediation != nil && f.Remediation.Recommendation != nil {
		r := f.Remediation.Recommendation
		rec = strings.TrimSpace(aws.ToString(r.Text) + " " + aws.ToString(r.Url))
	}
	out := apiv1.SecurityFinding{
		SeverityBasis:  &basis,
		Id:             aws.ToString(f.Id), // the finding ARN
		Check:          "securityhub." + aws.ToString(f.GeneratorId),
		Category:       hubCategory(f.Types, rtype),
		Severity:       sev,
		Title:          aws.ToString(f.Title),
		Resource:       resource,
		Detail:         shorten(aws.ToString(f.Description), 400),
		Recommendation: rec,
	}
	if t, err := time.Parse(time.RFC3339, aws.ToString(f.UpdatedAt)); err == nil {
		out.ObservedAt = t.UTC()
	}
	return out
}

// hubCategory maps a finding's ASFF types and resource type to a category.
func hubCategory(types []string, resourceType string) apiv1.SecurityCategory {
	for _, t := range types {
		for _, p := range []string{"TTPs", "Effects", "Unusual Behaviors"} {
			if strings.HasPrefix(t, p) {
				return apiv1.SecurityCategoryThreat
			}
		}
	}
	has := func(prefixes ...string) bool {
		return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(resourceType, p) })
	}
	switch {
	case has("AwsIam", "AwsAccount"):
		return apiv1.SecurityCategoryIdentity
	case has("AwsEc2SecurityGroup", "AwsEc2Vpc", "AwsEc2NetworkAcl", "AwsEc2Subnet", "AwsEc2Eip", "AwsElb", "AwsCloudFront", "AwsWaf", "AwsApiGateway"):
		return apiv1.SecurityCategoryNetwork
	case has("AwsS3", "AwsRds", "AwsEc2Volume", "AwsKms", "AwsDynamoDb", "AwsSecretsManager", "AwsBackup", "AwsEfs"):
		return apiv1.SecurityCategoryData
	case has("AwsCloudTrail", "AwsLogs", "AwsCloudWatch", "AwsConfig"):
		return apiv1.SecurityCategoryLogging
	}
	return apiv1.SecurityCategoryWorkload
}

// --- S3 ---

func (c *AWSCollector) checkS3(ctx context.Context, cl awsClients, account string) Report {
	p := newPartial(awsSource("aws-s3"))
	resource := "s3-account-public-access-block/" + account
	out, err := cl.s3control.GetPublicAccessBlock(ctx, &s3control.GetPublicAccessBlockInput{AccountId: &account})
	var none *s3ctypes.NoSuchPublicAccessBlockConfiguration
	switch {
	case errors.As(err, &none):
		p.eval("aws.s3-public-access-block", resource, apiv1.SecurityControlResultFail, "the account has no public access block")
		p.add(apiv1.SecurityFinding{Check: "aws.s3-public-access-block", Category: apiv1.SecurityCategoryData, Severity: apiv1.SecuritySeverityMedium,
			Title: "S3 account public access block is not set", Resource: resource,
			Detail:         "The account has no S3 public access block, so a bucket policy or ACL can make a bucket public.",
			Recommendation: "Turn on all four S3 Block Public Access settings for the account."})
		p.message = "no account-level public access block"
	case err != nil:
		p.fail("s3control GetPublicAccessBlock", err)
		p.unknown("aws.s3-public-access-block", resource, "s3control GetPublicAccessBlock", err)
	default:
		cfg := out.PublicAccessBlockConfiguration
		var off []string
		if cfg == nil || !aws.ToBool(cfg.BlockPublicAcls) {
			off = append(off, "BlockPublicAcls")
		}
		if cfg == nil || !aws.ToBool(cfg.IgnorePublicAcls) {
			off = append(off, "IgnorePublicAcls")
		}
		if cfg == nil || !aws.ToBool(cfg.BlockPublicPolicy) {
			off = append(off, "BlockPublicPolicy")
		}
		if cfg == nil || !aws.ToBool(cfg.RestrictPublicBuckets) {
			off = append(off, "RestrictPublicBuckets")
		}
		if len(off) == 0 {
			p.eval("aws.s3-public-access-block", resource, apiv1.SecurityControlResultPass, "all four settings are on")
		} else {
			p.eval("aws.s3-public-access-block", resource, apiv1.SecurityControlResultFail, "off: "+strings.Join(off, ", "))
		}
		if len(off) > 0 {
			p.add(apiv1.SecurityFinding{Check: "aws.s3-public-access-block", Category: apiv1.SecurityCategoryData, Severity: apiv1.SecuritySeverityMedium,
				Title: "S3 account public access block is partly off", Resource: resource,
				Detail:         "These account-level settings are off: " + strings.Join(off, ", ") + ".",
				Recommendation: "Turn on all four S3 Block Public Access settings for the account."})
		}
		p.message = "account-level public access block read"
	}
	return p.report()
}
