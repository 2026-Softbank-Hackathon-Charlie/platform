package security

import (
	"slices"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// PolicyVersion names this version of the control catalog and of the rules
// that set a finding's severity (ADR 0034). Change it whenever a control, a
// mapping or a severity rule changes.
const PolicyVersion = "hopcharlie-security-policy/2026-11-06.1"

// Issuers of severity values.
const (
	IssuerHopCharlie  = "HopCharlie"
	IssuerSecurityHub = "AWS Security Hub CSPM"
	IssuerGuardDuty   = "Amazon GuardDuty"
)

// Official references, pinned to what was read on 2026-11-06.
const (
	refAsOf        = "control reference as of 2026-11-06"
	nistFramework  = "NIST SP 800-53"
	nistVersion    = "Rev. 5 Update 1"
	nistURL        = "https://csrc.nist.gov/pubs/sp/800/53/r5/upd1/final"
	awsFramework   = "AWS Security Hub CSPM"
	pssFramework   = "Kubernetes Pod Security Standards"
	pssVersion     = "Kubernetes documentation as of 2026-11-06"
	pssURL         = "https://kubernetes.io/docs/concepts/security/pod-security-standards/"
	cisK8sName     = "CIS Kubernetes Benchmark"
	cisK8sURL      = "https://www.cisecurity.org/benchmark/kubernetes"
	awsIAMControls = "https://docs.aws.amazon.com/securityhub/latest/userguide/iam-controls.html"
	awsEC2Controls = "https://docs.aws.amazon.com/securityhub/latest/userguide/ec2-controls.html"
)

func nist(id, title, note string) apiv1.SecurityControlReference {
	return ref(nistFramework, nistVersion, id, title, nistURL, apiv1.SecurityControlRelationPartial, "", note)
}

func awsControl(id, title, page, severity string, rel apiv1.SecurityControlRelation, note string) apiv1.SecurityControlReference {
	return ref(awsFramework, refAsOf, id, title, page+"#"+lowerID(id), rel, severity, note)
}

func pss(profile, title string, rel apiv1.SecurityControlRelation, note string) apiv1.SecurityControlReference {
	return ref(pssFramework, pssVersion, profile+": "+title, title, pssURL, rel, "", note)
}

func ref(framework, version, id, title, url string, rel apiv1.SecurityControlRelation, severity, note string) apiv1.SecurityControlReference {
	r := apiv1.SecurityControlReference{Framework: framework, Version: version, Id: id, Url: url, Relation: rel}
	if title != "" {
		r.Title = &title
	}
	if severity != "" {
		r.Severity = &severity
	}
	if note != "" {
		r.Note = &note
	}
	return r
}

// lowerID turns "IAM.5" into the page anchor "iam-5".
func lowerID(id string) string {
	b := []byte(id)
	for i, c := range b {
		switch {
		case c >= 'A' && c <= 'Z':
			b[i] = c + 'a' - 'A'
		case c == '.':
			b[i] = '-'
		}
	}
	return string(b)
}

// Basis returns a severity basis of this policy version.
func Basis(t apiv1.SecuritySeverityBasisType, issuer, reason string) apiv1.SecuritySeverityBasis {
	b := apiv1.SecuritySeverityBasis{Type: t, Issuer: issuer, Policy: PolicyVersion}
	if reason != "" {
		b.Reason = &reason
	}
	return b
}

// vendorControl is the basis of a check that matches a vendor control
// exactly: the control's own severity.
func vendorControl(r apiv1.SecurityControlReference) apiv1.SecuritySeverityBasis {
	b := Basis(apiv1.SecuritySeverityBasisTypeVendor, IssuerSecurityHub, "the check matches "+r.Id+" exactly, so the finding has the control's own severity")
	b.SourceSeverity, b.SourceUrl = r.Severity, &r.Url
	return b
}

func platformRule(reason string) apiv1.SecuritySeverityBasis {
	return Basis(apiv1.SecuritySeverityBasisTypePlatformPolicy, IssuerHopCharlie, reason)
}

// The references the catalog uses more than once.
var (
	awsIAM9 = awsControl("IAM.9", "MFA should be enabled for the root user", awsIAMControls, "Critical", apiv1.SecurityControlRelationPartial,
		"IAM.9 also passes an account whose root user has no credentials; this check reads only the account summary's AccountMFAEnabled and fails such an account.")
	awsIAM4 = awsControl("IAM.4", "IAM root user access key should not exist", awsIAMControls, "Critical", apiv1.SecurityControlRelationExact, "")
	awsIAM5 = awsControl("IAM.5", "MFA should be enabled for all IAM users that have a console password", awsIAMControls, "Medium", apiv1.SecurityControlRelationExact, "")
	awsIAM3 = awsControl("IAM.3", "IAM users' access keys should be rotated every 90 days or less", awsIAMControls, "Medium", apiv1.SecurityControlRelationExact, "")
	awsEC27 = awsControl("EC2.7", "EBS default encryption should be enabled", awsEC2Controls, "Medium", apiv1.SecurityControlRelationExact,
		"Evaluated for the platform's region only, as Security Hub evaluates each region.")
	awsEC218 = awsControl("EC2.18", "Security groups should only allow unrestricted incoming traffic for authorized ports", awsEC2Controls, "High", apiv1.SecurityControlRelationExact,
		"With the control's default authorizedTcpPorts 80 and 443.")
	awsEC219 = awsControl("EC2.19", "Security groups should not allow unrestricted access to ports with high risk", awsEC2Controls, "Critical", apiv1.SecurityControlRelationExact,
		"Same 24 blocked ports as the control.")
	awsCloudTrail1 = awsControl("CloudTrail.1", "CloudTrail should be enabled and configured with at least one multi-Region trail that includes read and write management events",
		"https://docs.aws.amazon.com/securityhub/latest/userguide/cloudtrail-controls.html", "High", apiv1.SecurityControlRelationPartial,
		"This check asks for one logging trail that applies to the region. It does not read event selectors, so it cannot tell whether read and write management events are recorded.")
	awsS31 = awsControl("S3.1", "S3 general purpose buckets should have block public access settings enabled",
		"https://docs.aws.amazon.com/securityhub/latest/userguide/s3-controls.html", "Medium", apiv1.SecurityControlRelationExact, "")
	awsGuardDuty1 = awsControl("GuardDuty.1", "GuardDuty should be enabled",
		"https://docs.aws.amazon.com/securityhub/latest/userguide/guardduty-controls.html", "High", apiv1.SecurityControlRelationExact,
		"Evaluated for the platform's region only.")
)

// ec219Ports are the ports AWS Security Hub EC2.19 blocks from 0.0.0.0/0
// and ::/0 (the control's blockedPorts, not customisable).
var ec219Ports = []int32{20, 21, 22, 23, 25, 110, 135, 143, 445, 1433, 1434, 3000, 3306, 3389, 4333, 5000, 5432, 5500, 5601, 8080, 8088, 8888, 9200, 9300}

// catalog is the platform's controls, in the order of the domain enum.
var catalog = []apiv1.SecurityControl{
	// --- identity ---
	{Id: "HC-IDN-01", Title: "Root user has MFA", Domain: apiv1.SecurityControlDomainIdentity, Check: "aws.root-mfa", Source: "aws-iam",
		Asset: "the AWS account's root user", Condition: "the account summary reports AccountMFAEnabled",
		References:   []apiv1.SecurityControlReference{awsIAM9, nist("IA-2(1)", "Multi-factor authentication to privileged accounts", "")},
		FailSeverity: "critical", SeverityBasis: platformRule("partial match to IAM.9; the platform uses IAM.9's level, Critical"),
		Limitations: []string{"An account whose root user has no credentials fails here but passes IAM.9."}},
	{Id: "HC-IDN-02", Title: "Root user has no access keys", Domain: apiv1.SecurityControlDomainIdentity, Check: "aws.root-access-key", Source: "aws-iam",
		Asset: "the AWS account's root user", Condition: "the account summary reports no root access keys",
		References:   []apiv1.SecurityControlReference{awsIAM4, nist("AC-6", "Least privilege", "")},
		FailSeverity: "critical", SeverityBasis: vendorControl(awsIAM4), Limitations: []string{}},
	{Id: "HC-IDN-03", Title: "Console users have MFA", Domain: apiv1.SecurityControlDomainIdentity, Check: "aws.user-mfa", Source: "aws-iam",
		Asset: "IAM user with a console password", Condition: "the user has at least one MFA device; users without a console password are not applicable",
		References:   []apiv1.SecurityControlReference{awsIAM5, nist("IA-2(1)", "Multi-factor authentication to privileged accounts", "IA-2(2) covers non-privileged accounts.")},
		FailSeverity: "medium", SeverityBasis: vendorControl(awsIAM5),
		Limitations: []string{"Users who sign in through IAM Identity Center or another identity provider are not IAM users and are not checked."}},
	{Id: "HC-IDN-04", Title: "Access keys are younger than 90 days", Domain: apiv1.SecurityControlDomainIdentity, Check: "aws.access-key-age", Source: "aws-iam",
		Asset: "IAM user with an active access key", Condition: "every active access key was created 90 days ago or less; users with no active key are not applicable",
		References:   []apiv1.SecurityControlReference{awsIAM3, nist("IA-5(1)", "Password-based authentication", "Authenticator lifetime only.")},
		FailSeverity: "medium", SeverityBasis: vendorControl(awsIAM3), Limitations: []string{}},
	{Id: "HC-IDN-05", Title: "cluster-admin is bound only to system identities", Domain: apiv1.SecurityControlDomainIdentity, Check: "k8s.cluster-admin", Source: "kubernetes",
		Asset: "the cluster's ClusterRoleBindings", Condition: "no ClusterRoleBinding outside the system identities binds the cluster-admin ClusterRole",
		References: []apiv1.SecurityControlReference{
			ref(cisK8sName, "section 5.1.1 (manual); benchmark version not pinned", "5.1.1", "Ensure that the cluster-admin role is only used where required", cisK8sURL, apiv1.SecurityControlRelationPartial, "",
				"The benchmark leaves the judgement of where it is required to the operator; this check fails every binding outside system identities."),
			nist("AC-6", "Least privilege", "")},
		FailSeverity: "varies", SeverityBasis: platformRule("high; critical when bound to a group of every caller or every service account"),
		Limitations: []string{"Roles with the same rights under another name, and RoleBindings to cluster-admin in a namespace, are not checked.", "Managed control plane identities (system:, eks:) are skipped."}},
	{Id: "HC-IDN-06", Title: "Bucket keys are all recorded by the platform", Domain: apiv1.SecurityControlDomainIdentity, Check: CheckUnrecordedKey, Source: "storage-keys",
		Asset: "an app instance's bucket", Condition: "every key of the instance's storage user is one the platform recorded (ADR 0027)",
		References:   []apiv1.SecurityControlReference{nist("AC-2", "Account management", "Only the platform-made credentials of app buckets.")},
		FailSeverity: "varies", SeverityBasis: platformRule("medium for an active unrecorded key, low for an inactive one"),
		Limitations: []string{"Only the users the platform made for app buckets are read, not every IAM user, service account or MinIO user."}},

	// --- data ---
	{Id: "HC-DAT-01", Title: "EBS encryption by default is on", Domain: apiv1.SecurityControlDomainData, Check: "aws.ebs-default-encryption", Source: "aws-ec2",
		Asset: "the account's EBS default in the platform's region", Condition: "GetEbsEncryptionByDefault is true",
		References:   []apiv1.SecurityControlReference{awsEC27, nist("SC-28", "Protection of information at rest", "")},
		FailSeverity: "medium", SeverityBasis: vendorControl(awsEC27),
		Limitations: []string{"Applies to new volumes only; existing volumes, snapshots and RDS storage are not checked.", "GKE and on-premises disks are not checked."}},
	{Id: "HC-DAT-02", Title: "S3 account-level public access block is fully on", Domain: apiv1.SecurityControlDomainData, Check: "aws.s3-public-access-block", Source: "aws-s3",
		Asset: "the account's S3 public access block", Condition: "BlockPublicAcls, IgnorePublicAcls, BlockPublicPolicy and RestrictPublicBuckets are all true",
		References:   []apiv1.SecurityControlReference{awsS31, nist("AC-3", "Access enforcement", "")},
		FailSeverity: "medium", SeverityBasis: vendorControl(awsS31),
		Limitations: []string{"GCS buckets and MinIO buckets are not checked."}},
	{Id: "HC-DAT-03", Title: "Plan database hosts are restricted", Domain: apiv1.SecurityControlDomainData, Check: "platform.dsn-hosts", Source: "platform-config",
		Asset: "the platform server's PLATFORM_DSN_HOST_SUFFIXES", Condition: "the setting names at least one host suffix",
		References: []apiv1.SecurityControlReference{}, FailSeverity: "medium",
		SeverityBasis: platformRule("the server would send a database password to any host a plan names"),
		Limitations:   []string{"The suffixes themselves are not judged."}},

	// --- logging ---
	{Id: "HC-LOG-01", Title: "A CloudTrail trail is logging", Domain: apiv1.SecurityControlDomainLogging, Check: "aws.cloudtrail-logging", Source: "aws-cloudtrail",
		Asset: "CloudTrail in the platform's region", Condition: "at least one trail that applies to the region is logging",
		References:   []apiv1.SecurityControlReference{awsCloudTrail1, nist("AU-12", "Audit record generation", "")},
		FailSeverity: "high", SeverityBasis: platformRule("partial match to CloudTrail.1; the platform uses CloudTrail.1's level, High"),
		Limitations: []string{"Event selectors (read and write management events) and log retention are not read.", "GCP Cloud Audit Logs and on-premises Kubernetes audit logs are not checked."}},
	{Id: "HC-LOG-02", Title: "GuardDuty is enabled", Domain: apiv1.SecurityControlDomainLogging, Check: "aws.guardduty-enabled", Source: "aws-guardduty",
		Asset: "GuardDuty in the platform's region", Condition: "the region has a detector and it is enabled",
		References:   []apiv1.SecurityControlReference{awsGuardDuty1, nist("SI-4", "System monitoring", "")},
		FailSeverity: "high", SeverityBasis: vendorControl(awsGuardDuty1),
		Limitations: []string{"Protection plans (EKS, S3, runtime) are not read."}},
	{Id: "HC-LOG-03", Title: "Security Hub is enabled", Domain: apiv1.SecurityControlDomainLogging, Check: "aws.securityhub-enabled", Source: "aws-securityhub",
		Asset: "Security Hub in the platform's region", Condition: "DescribeHub answers",
		References: []apiv1.SecurityControlReference{}, FailSeverity: "info",
		SeverityBasis: platformRule("information: the platform checks its own controls without it, and enabling it is billed per check"),
		Limitations:   []string{"Which standards are enabled is not read."}},

	// --- policy ---
	{Id: "HC-POL-01", Title: "Security groups allow no unrestricted ingress except HTTP and HTTPS", Domain: apiv1.SecurityControlDomainPolicy, Check: "aws.sg-open-ingress", Source: "aws-ec2",
		Asset: "security group", Condition: "no ingress rule from 0.0.0.0/0 or ::/0 other than TCP 80 or 443 alone",
		References:   []apiv1.SecurityControlReference{awsEC219, awsEC218, nist("SC-7", "Boundary protection", "")},
		FailSeverity: "varies", SeverityBasis: platformRule("critical for all traffic or a port EC2.19 blocks (EC2.19's level), high for another TCP port (EC2.18's level), medium for UDP, low for ICMP, info for TCP 80 or 443 alone"),
		Limitations: []string{"Load balancers, network ACLs and whether an instance listens on the port are not read."}},
	{Id: "HC-POL-02", Title: "No privileged containers", Domain: apiv1.SecurityControlDomainPolicy, Check: "k8s.privileged", Source: "kubernetes",
		Asset: "running workload", Condition: "no init, app or ephemeral container sets securityContext.privileged",
		References:   []apiv1.SecurityControlReference{pss("Baseline", "Privileged Containers", apiv1.SecurityControlRelationExact, ""), nist("AC-6", "Least privilege", "")},
		FailSeverity: "high", SeverityBasis: platformRule("Pod Security Standards give no severity"), Limitations: []string{"Only running pods are read; workloads scaled to zero are not."}},
	{Id: "HC-POL-03", Title: "No host network", Domain: apiv1.SecurityControlDomainPolicy, Check: "k8s.host-network", Source: "kubernetes",
		Asset: "running workload", Condition: "spec.hostNetwork is not true",
		References:   []apiv1.SecurityControlReference{pss("Baseline", "Host Namespaces", apiv1.SecurityControlRelationExact, "hostNetwork part.")},
		FailSeverity: "medium", SeverityBasis: platformRule("Pod Security Standards give no severity"), Limitations: []string{"Only running pods are read."}},
	{Id: "HC-POL-04", Title: "No host PID namespace", Domain: apiv1.SecurityControlDomainPolicy, Check: "k8s.host-pid", Source: "kubernetes",
		Asset: "running workload", Condition: "spec.hostPID is not true",
		References:   []apiv1.SecurityControlReference{pss("Baseline", "Host Namespaces", apiv1.SecurityControlRelationExact, "hostPID part.")},
		FailSeverity: "high", SeverityBasis: platformRule("Pod Security Standards give no severity"), Limitations: []string{"Only running pods are read."}},
	{Id: "HC-POL-05", Title: "No host IPC namespace", Domain: apiv1.SecurityControlDomainPolicy, Check: "k8s.host-ipc", Source: "kubernetes",
		Asset: "running workload", Condition: "spec.hostIPC is not true",
		References:   []apiv1.SecurityControlReference{pss("Baseline", "Host Namespaces", apiv1.SecurityControlRelationExact, "hostIPC part.")},
		FailSeverity: "high", SeverityBasis: platformRule("Pod Security Standards give no severity"), Limitations: []string{"Only running pods are read."}},
	{Id: "HC-POL-06", Title: "No hostPath volumes", Domain: apiv1.SecurityControlDomainPolicy, Check: "k8s.host-path", Source: "kubernetes",
		Asset: "running workload", Condition: "no volume is a hostPath",
		References:   []apiv1.SecurityControlReference{pss("Baseline", "HostPath Volumes", apiv1.SecurityControlRelationExact, "")},
		FailSeverity: "medium", SeverityBasis: platformRule("Pod Security Standards give no severity"), Limitations: []string{"Only running pods are read."}},
	{Id: "HC-POL-07", Title: "Containers run as non-root", Domain: apiv1.SecurityControlDomainPolicy, Check: "k8s.run-as-non-root", Source: "kubernetes",
		Asset: "running workload", Condition: "every container has runAsNonRoot true or a non-zero runAsUser, in the container or the pod",
		References: []apiv1.SecurityControlReference{pss("Restricted", "Running as Non-root", apiv1.SecurityControlRelationPartial,
			"The standard asks for runAsNonRoot true; this check also accepts a non-zero runAsUser.")},
		FailSeverity: "low", SeverityBasis: platformRule("Pod Security Standards give no severity"), Limitations: []string{"The image's own USER is not read."}},
	{Id: "HC-POL-08", Title: "Containers set resource limits", Domain: apiv1.SecurityControlDomainPolicy, Check: "k8s.no-limits", Source: "kubernetes",
		Asset: "running workload", Condition: "every app container sets at least one resource limit",
		References: []apiv1.SecurityControlReference{}, FailSeverity: "low",
		SeverityBasis: platformRule("availability rule of the platform; no official control is mapped"), Limitations: []string{"Init containers are not checked."}},
	{Id: "HC-POL-09", Title: "Namespaces have a NetworkPolicy", Domain: apiv1.SecurityControlDomainPolicy, Check: "k8s.no-network-policy", Source: "kubernetes",
		Asset: "namespace that runs pods", Condition: "the namespace has at least one NetworkPolicy",
		References:   []apiv1.SecurityControlReference{nist("SC-7", "Boundary protection", "")},
		FailSeverity: "medium", SeverityBasis: platformRule("no official severity for a missing NetworkPolicy"),
		Limitations: []string{"Whether the policies deny by default, and whether the CNI enforces them, are not checked."}},
	{Id: "HC-POL-10", Title: "No Services on cloud load balancers", Domain: apiv1.SecurityControlDomainPolicy, Check: "k8s.load-balancer", Source: "kubernetes",
		Asset: "Service", Condition: "the Service is not of type LoadBalancer",
		References:   []apiv1.SecurityControlReference{nist("SC-7", "Boundary protection", "")},
		FailSeverity: "medium", SeverityBasis: platformRule("apps are meant to be reached through the platform gateway (ADR 0036 in infra)"),
		Limitations: []string{"An internal load balancer is counted like a public one."}},
	{Id: "HC-POL-11", Title: "No Services on node ports", Domain: apiv1.SecurityControlDomainPolicy, Check: "k8s.node-port", Source: "kubernetes",
		Asset: "Service", Condition: "the Service is not of type NodePort",
		References:   []apiv1.SecurityControlReference{nist("SC-7", "Boundary protection", "")},
		FailSeverity: "low", SeverityBasis: platformRule("node ports are reachable where the nodes are"),
		Limitations: []string{"Whether the nodes are reachable from outside is not read."}},
	{Id: "HC-POL-12", Title: "The traffic gateway is configured", Domain: apiv1.SecurityControlDomainPolicy, Check: "platform.gateway", Source: "platform-config",
		Asset: "the platform server's PLATFORM_GATEWAY_URL", Condition: "the setting is not empty",
		References: []apiv1.SecurityControlReference{}, FailSeverity: "info",
		SeverityBasis: platformRule("information: canary reads and gateway-mode plans are off"), Limitations: []string{}},

	// --- vulnerability ---
	{Id: "HC-VUL-01", Title: "Running images have a scan result", Domain: apiv1.SecurityControlDomainVulnerability, Check: CheckImageScanned, Source: "image-scan",
		Asset: "running container", Condition: "the digest the container runs has a succeeded scan",
		References:   []apiv1.SecurityControlReference{nist("RA-5", "Vulnerability monitoring and scanning", "")},
		FailSeverity: "medium", SeverityBasis: platformRule("an unscanned running image hides its vulnerabilities"),
		Limitations: []string{"A scan result is not zero vulnerabilities; see GET /v1/security/vulnerabilities.", "Node OS, kernel, container runtime and managed control plane are not scanned."}},
}

// CheckImageScanned is the check of HC-VUL-01.
const CheckImageScanned = "image.scanned"

// init sets each control's mapping to the closest relation of its
// references: exact if one matches exactly, partial if any reference is
// partial, unmapped when there is none.
func init() {
	for i := range catalog {
		c := &catalog[i]
		c.Mapping = apiv1.SecurityControlMappingUnmapped
		for _, r := range c.References {
			if r.Relation == apiv1.SecurityControlRelationExact {
				c.Mapping = apiv1.SecurityControlMappingExact
				break
			}
			c.Mapping = apiv1.SecurityControlMappingPartial
		}
	}
}

// Catalog returns the platform's controls. It must not be modified.
func Catalog() []apiv1.SecurityControl { return catalog }

// ControlFor returns the control a check belongs to.
func ControlFor(check string) (apiv1.SecurityControl, bool) {
	i := slices.IndexFunc(catalog, func(c apiv1.SecurityControl) bool { return c.Check == check })
	if i < 0 {
		return apiv1.SecurityControl{}, false
	}
	return catalog[i], true
}
