package security

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// domains in the order of the enum.
var domains = []apiv1.SecurityControlDomain{
	apiv1.SecurityControlDomainIdentity, apiv1.SecurityControlDomainData, apiv1.SecurityControlDomainLogging,
	apiv1.SecurityControlDomainPolicy, apiv1.SecurityControlDomainVulnerability,
}

// ScanResults reads stored image scans (ADR 0034). Scans run outside the
// request; a ScanResults only reads what they stored.
type ScanResults interface {
	// Latest returns the latest scan of each digest and the vulnerabilities
	// of the succeeded ones.
	Latest() ([]apiv1.SecurityImageScan, []apiv1.SecurityVulnerability, error)
}

// ControlReport builds the control report of a collection for environment (all
// when empty). Running images become the evaluations of HC-VUL-01 with the
// scan state the scanner gives them.
func ControlReport(col Collection, environment string, scans ScanResults) apiv1.SecurityControlReport {
	images, scanner := runningImages(col.Images, scans)
	evals := slices.Clone(col.Evaluations)
	evals = append(evals, imageEvaluations(col, images, scanner)...)

	envs := Environments(col.Report)
	out := apiv1.SecurityControlReport{GeneratedAt: col.Report.GeneratedAt, PolicyVersion: PolicyVersion,
		Controls: Catalog(), Evaluations: []apiv1.SecurityControlEvaluation{}, Coverage: []apiv1.SecurityControlCoverage{}}
	order := map[string]int{}
	for i, e := range envs {
		order[e] = i
	}
	for _, e := range evals {
		if environment == "" || e.Environment == environment {
			out.Evaluations = append(out.Evaluations, e)
		}
	}
	slices.SortStableFunc(out.Evaluations, func(a, b apiv1.SecurityControlEvaluation) int {
		return cmp.Or(cmp.Compare(order[a.Environment], order[b.Environment]), cmp.Compare(a.ControlId, b.ControlId), cmp.Compare(a.Asset, b.Asset))
	})
	for _, env := range envs {
		if environment != "" && env != environment {
			continue
		}
		for _, d := range domains {
			out.Coverage = append(out.Coverage, coverage(col.Report, env, d, out.Evaluations))
		}
	}
	return out
}

// coverage counts one environment's evaluations of one domain and says
// whether the domain is evaluated there at all.
func coverage(rep apiv1.SecurityReport, env string, d apiv1.SecurityControlDomain, evals []apiv1.SecurityControlEvaluation) apiv1.SecurityControlCoverage {
	cov := apiv1.SecurityControlCoverage{Environment: env, Domain: d}
	applies := false
	for _, c := range catalog {
		if c.Domain == d && controlApplies(rep, env, c) {
			applies = true
		}
	}
	byID := map[string]apiv1.SecurityControlDomain{}
	for _, c := range catalog {
		byID[c.Id] = c.Domain
	}
	for _, e := range evals {
		if e.Environment != env || byID[e.ControlId] != d {
			continue
		}
		switch e.Result {
		case apiv1.SecurityControlResultPass:
			cov.Counts.Pass++
		case apiv1.SecurityControlResultFail:
			cov.Counts.Fail++
		case apiv1.SecurityControlResultUnknown:
			cov.Counts.Unknown++
		case apiv1.SecurityControlResultNotApplicable:
			cov.Counts.NotApplicable++
		}
	}
	cov.Counts.Evaluated = cov.Counts.Pass + cov.Counts.Fail
	cov.Counts.Applicable = cov.Counts.Evaluated + cov.Counts.Unknown
	switch {
	case !applies:
		cov.State = apiv1.SecurityControlCoverageStateUnsupported
		r := unsupportedReason(rep, env, d)
		cov.Reason = &r
	case cov.Counts.Evaluated == 0 && cov.Counts.Unknown > 0:
		cov.State = apiv1.SecurityControlCoverageStateNotConnected
		r := "every evaluation is unknown: the sources of these controls could not be read"
		cov.Reason = &r
	default:
		cov.State = apiv1.SecurityControlCoverageStateEvaluated
	}
	return cov
}

// controlApplies reports whether env has the source that evaluates c (for
// HC-VUL-01, a cluster).
func controlApplies(rep apiv1.SecurityReport, env string, c apiv1.SecurityControl) bool {
	want := c.Source
	if c.Check == CheckImageScanned {
		want = "kubernetes"
	}
	for _, s := range rep.Sources {
		if s.Environment == env && s.Name == want && appliesTo(c, s.Provider) {
			return true
		}
	}
	return false
}

// unsupportedReason says what is not checked in a domain of an environment.
func unsupportedReason(rep apiv1.SecurityReport, env string, d apiv1.SecurityControlDomain) string {
	var names []string
	for _, s := range rep.Sources {
		if s.Environment == env {
			names = append(names, fmt.Sprintf("%s (%s)", s.Name, s.Status))
		}
	}
	return fmt.Sprintf("no control of the %s domain is checked in %s yet; its sources are %v. Not checked is not a pass.", d, env, names)
}

// runningImages gives each running container the scan state of its digest.
func runningImages(images []apiv1.SecurityRunningImage, scans ScanResults) ([]apiv1.SecurityRunningImage, apiv1.SecurityScannerState) {
	scanner := apiv1.SecurityScannerState{Status: apiv1.SecurityScannerStateStatusNotConfigured}
	msg := "no image scanner results are connected (ADR 0034): running images are not scanned, which is not the same as having no vulnerabilities"
	scanner.Message = &msg
	latest := map[string]apiv1.SecurityImageScan{}
	if scans != nil {
		got, _, err := scans.Latest()
		if err != nil {
			scanner.Status = apiv1.SecurityScannerStateStatusError
			m := "stored scans could not be read: " + shorten(err.Error(), 160)
			scanner.Message = &m
		} else {
			scanner.Status, scanner.Message = apiv1.SecurityScannerStateStatusConnected, nil
			for _, s := range got {
				latest[s.Digest] = s
			}
		}
	}
	out := make([]apiv1.SecurityRunningImage, len(images))
	for i, img := range images {
		switch {
		case img.Digest == nil:
			img.ScanState = apiv1.SecurityRunningImageScanStateUnknownDigest
		default:
			s, ok := latest[*img.Digest]
			switch {
			case !ok:
				img.ScanState = apiv1.SecurityRunningImageScanStateNotScanned
			case s.Status == apiv1.SecurityImageScanStatusSucceeded:
				id := s.Id
				img.ScanState, img.ScanId = apiv1.SecurityRunningImageScanStateScanned, &id
			default:
				id := s.Id
				img.ScanState, img.ScanId = apiv1.SecurityRunningImageScanStateScanFailed, &id
			}
		}
		out[i] = img
	}
	return out, scanner
}

// imageEvaluations are HC-VUL-01's: a container whose digest has a succeeded
// scan passes; one with no or a failed scan fails; one without a digest, or
// any container while stored scans cannot be read, is unknown.
func imageEvaluations(col Collection, images []apiv1.SecurityRunningImage, scanner apiv1.SecurityScannerState) []apiv1.SecurityControlEvaluation {
	c, _ := ControlFor(CheckImageScanned)
	var out []apiv1.SecurityControlEvaluation
	for _, img := range images {
		asset := img.Namespace + "/" + img.Workload + "/" + img.Container
		if img.Digest != nil { // a rollout runs two digests of one container
			asset += "@" + *img.Digest
		}
		ev := apiv1.SecurityControlEvaluation{Id: EvaluationID(img.Environment, c.Id, asset), ControlId: c.Id, Environment: img.Environment,
			Source: "kubernetes", Asset: asset, EvaluatedAt: col.Report.GeneratedAt, FindingIds: []string{}}
		switch {
		case scanner.Status == apiv1.SecurityScannerStateStatusError:
			ev.Result, ev.Reason = apiv1.SecurityControlResultUnknown, "stored scans could not be read"
		case img.ScanState == apiv1.SecurityRunningImageScanStateUnknownDigest:
			ev.Result, ev.Reason = apiv1.SecurityControlResultUnknown, "the runtime reported no image digest for the container"
		case img.ScanState == apiv1.SecurityRunningImageScanStateScanned:
			ev.Result, ev.Reason = apiv1.SecurityControlResultPass, "digest "+*img.Digest+" has a succeeded scan"
		case img.ScanState == apiv1.SecurityRunningImageScanStateScanFailed:
			ev.Result, ev.Reason = apiv1.SecurityControlResultFail, "the latest scan of digest "+*img.Digest+" did not succeed"
		case scanner.Status == apiv1.SecurityScannerStateStatusNotConfigured:
			ev.Result, ev.Reason = apiv1.SecurityControlResultFail, "digest "+*img.Digest+" has no scan: no image scanner results are connected"
		default:
			ev.Result, ev.Reason = apiv1.SecurityControlResultFail, "digest "+*img.Digest+" has no scan"
		}
		out = append(out, ev)
	}
	// A cluster that could not be read has no running images: its HC-VUL-01
	// is unknown, not empty.
	for _, s := range col.Report.Sources {
		if s.Name != "kubernetes" || s.Status == apiv1.SecuritySourceStatusOk {
			continue
		}
		reason := "the cluster's running images could not be read"
		if s.Message != nil {
			reason += ": " + *s.Message
		}
		out = append(out, apiv1.SecurityControlEvaluation{Id: EvaluationID(s.Environment, c.Id, AllAssets), ControlId: c.Id, Environment: s.Environment,
			Source: "kubernetes", Asset: AllAssets, Result: apiv1.SecurityControlResultUnknown, Reason: reason, EvaluatedAt: s.CheckedAt, FindingIds: []string{}})
	}
	return out
}

// VulnerabilityReport builds the vulnerability report of a collection for
// environment (all when empty).
func VulnerabilityReport(col Collection, environment string, scans ScanResults) apiv1.SecurityVulnerabilityReport {
	images, scanner := runningImages(col.Images, scans)
	out := apiv1.SecurityVulnerabilityReport{GeneratedAt: col.Report.GeneratedAt, Scanner: scanner,
		Scans: []apiv1.SecurityImageScan{}, Vulnerabilities: []apiv1.SecurityVulnerability{}, RunningImages: []apiv1.SecurityRunningImage{}}
	for _, img := range images {
		if environment == "" || img.Environment == environment {
			out.RunningImages = append(out.RunningImages, img)
		}
	}
	if scanner.Status == apiv1.SecurityScannerStateStatusConnected {
		got, vulns, _ := scans.Latest()
		used := map[string]bool{}
		for _, img := range out.RunningImages {
			if img.Digest != nil {
				used[*img.Digest] = true
			}
		}
		for _, s := range got {
			if environment == "" || used[s.Digest] {
				out.Scans = append(out.Scans, s)
			}
		}
		for _, v := range vulns {
			if environment == "" || used[v.Digest] {
				out.Vulnerabilities = append(out.Vulnerabilities, v)
			}
		}
	}
	out.Summary = summarize(out)
	return out
}

// summarize counts each unit separately: advisories, scanned digests with a
// vulnerability, affected running containers, occurrences and running
// containers without a succeeded scan.
func summarize(r apiv1.SecurityVulnerabilityReport) apiv1.SecurityVulnerabilitySummary {
	adv, digests, occ := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, v := range r.Vulnerabilities {
		adv[v.Advisory] = true
		digests[v.Digest] = true
		occ[v.Id] = true
	}
	var s apiv1.SecurityVulnerabilitySummary
	for _, img := range r.RunningImages {
		if img.ScanState != apiv1.SecurityRunningImageScanStateScanned {
			s.NotScannedContainers++
		} else if img.Digest != nil && digests[*img.Digest] {
			s.AffectedContainers++
		}
	}
	s.Advisories, s.Images, s.Occurrences = len(adv), len(digests), len(occ)
	return s
}
