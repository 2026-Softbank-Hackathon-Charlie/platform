package security

import (
	"testing"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

var exceptionNow = time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)

func exception(id string, target apiv1.SecurityExceptionTarget, targetID string, expires time.Duration) apiv1.SecurityException {
	return apiv1.SecurityException{Id: id, TargetType: target, TargetId: targetID, Reason: "accepted " + id, CreatedBy: "alice",
		CreatedAt: exceptionNow.Add(-time.Hour), ExpiresAt: exceptionNow.Add(expires)}
}

func TestActiveExceptionsLeaveOutRevokedAndExpired(t *testing.T) {
	revoked := exception("sx_revoked", apiv1.SecurityExceptionTargetFinding, "f2", time.Hour)
	at, by := exceptionNow.Add(-time.Minute), "bob"
	revoked.RevokedAt, revoked.RevokedBy = &at, &by
	all := []apiv1.SecurityException{
		exception("sx_finding", apiv1.SecurityExceptionTargetFinding, "f1", time.Hour),
		revoked,
		exception("sx_expired", apiv1.SecurityExceptionTargetFinding, "f3", -time.Second),
		exception("sx_at_now", apiv1.SecurityExceptionTargetFinding, "f4", 0), // expiresAt must be after now
		exception("sx_vuln", apiv1.SecurityExceptionTargetVulnerability, "f1", time.Hour),
	}
	got := ActiveExceptions(all, exceptionNow)
	if len(got) != 2 || got["finding/f1"].Id != "sx_finding" || got["vulnerability/f1"].Id != "sx_vuln" {
		t.Fatalf("active: %+v", got)
	}
}

func TestAttachExceptionsToFindingsAndVulnerabilitiesKeepsSeverity(t *testing.T) {
	active := ActiveExceptions([]apiv1.SecurityException{
		exception("sx_1", apiv1.SecurityExceptionTargetFinding, "f1", time.Hour),
		exception("sx_2", apiv1.SecurityExceptionTargetVulnerability, "v2", time.Hour),
		exception("sx_3", apiv1.SecurityExceptionTargetVulnerability, "f2", time.Hour), // a vulnerability id, not finding f2
	}, exceptionNow)

	rep := apiv1.SecurityReport{Findings: []apiv1.SecurityFinding{
		{Id: "f1", Severity: apiv1.SecuritySeverityCritical}, {Id: "f2", Severity: apiv1.SecuritySeverityHigh}}}
	AttachFindingExceptions(&rep, active)
	if e := rep.Findings[0].Exception; e == nil || e.Id != "sx_1" || rep.Findings[0].Severity != apiv1.SecuritySeverityCritical {
		t.Fatalf("finding f1: %+v", rep.Findings[0])
	}
	if rep.Findings[1].Exception != nil {
		t.Fatalf("finding f2 got %+v", rep.Findings[1].Exception)
	}

	vr := apiv1.SecurityVulnerabilityReport{Vulnerabilities: []apiv1.SecurityVulnerability{
		{Id: "v1", Severity: apiv1.SecurityVulnerabilitySeverityHigh}, {Id: "v2", Severity: apiv1.SecurityVulnerabilitySeverityCritical}}}
	AttachVulnerabilityExceptions(&vr, active)
	if vr.Vulnerabilities[0].Exception != nil {
		t.Fatalf("v1 got %+v", vr.Vulnerabilities[0].Exception)
	}
	if e := vr.Vulnerabilities[1].Exception; e == nil || e.Id != "sx_2" || vr.Vulnerabilities[1].Severity != apiv1.SecurityVulnerabilitySeverityCritical {
		t.Fatalf("v2: %+v", vr.Vulnerabilities[1])
	}
	// Each item gets its own copy.
	vr.Vulnerabilities[1].Exception.Reason = "changed"
	if active["vulnerability/v2"].Reason == "changed" {
		t.Fatal("attached exception shares memory with active")
	}
}

func TestOCSFSuppressedByException(t *testing.T) {
	schemas := ocsfSchemas(t)
	for _, f := range sample {
		ex := exception("sx_1", apiv1.SecurityExceptionTargetFinding, f.Id, time.Hour)
		f.Exception = &ex
		e := OCSF(f)
		if e.StatusID != 3 || e.Status != "Suppressed" || e.StatusDetail != "accepted sx_1" {
			t.Fatalf("%s: status %d %q %q", f.Id, e.StatusID, e.Status, e.StatusDetail)
		}
		if sevID, _ := ocsfSeverity(f.Severity); e.SeverityID != sevID {
			t.Fatalf("%s: severity changed", f.Id)
		}
		if err := validate(t, schemas, e); err != nil {
			t.Fatalf("%s: %v", f.Id, err)
		}
	}
	if e := OCSF(sample[0]); e.StatusID != 1 || e.Status != "New" || e.StatusDetail != "" {
		t.Fatalf("no exception: %+v", e)
	}
}

func TestExceptionFromStore(t *testing.T) {
	at := exceptionNow
	row := store.SecurityException{ID: "sx_1", TargetType: "vulnerability", TargetID: "v1", Reason: "r", CreatedBy: "alice",
		CreatedAt: at.Add(-time.Hour), ExpiresAt: at.Add(time.Hour)}
	got := ExceptionFromStore(row)
	if got.Id != "sx_1" || got.TargetType != apiv1.SecurityExceptionTargetVulnerability || got.TargetId != "v1" || got.Reason != "r" ||
		got.CreatedBy != "alice" || !got.CreatedAt.Equal(row.CreatedAt) || !got.ExpiresAt.Equal(row.ExpiresAt) || got.RevokedAt != nil || got.RevokedBy != nil {
		t.Fatalf("%+v", got)
	}
	row.RevokedAt, row.RevokedBy = &at, "bob"
	got = ExceptionFromStore(row)
	if got.RevokedAt == nil || !got.RevokedAt.Equal(at) || got.RevokedBy == nil || *got.RevokedBy != "bob" {
		t.Fatalf("revoked %+v", got)
	}
}

func TestKEVListedReadsOnlyTheCache(t *testing.T) {
	var nilIntel *ExploitIntel
	if nilIntel.KEVListed("CVE-2024-6387") {
		t.Fatal("nil intel listed a CVE")
	}
	f := &exploitFake{}
	x, now := newExploitTest(t, f)
	if x.KEVListed("CVE-2024-6387") || f.kevCalls.Load() != 0 {
		t.Fatalf("never read: listed or fetched (%d calls)", f.kevCalls.Load())
	}
	r := vulnReport([]string{"CVE-2024-6387"})
	AttachExploitation(t.Context(), &r, x)
	calls := f.kevCalls.Load()
	if !x.KEVListed("CVE-2024-6387") || !x.KEVListed("cve-2024-6387") || x.KEVListed("CVE-2099-0001") || x.KEVListed("") {
		t.Fatal("cached catalog not read")
	}
	*now = now.Add(72*time.Hour + time.Second)
	if x.KEVListed("CVE-2024-6387") {
		t.Fatal("a catalog older than 72h still counts")
	}
	if f.kevCalls.Load() != calls {
		t.Fatal("KEVListed fetched the feed")
	}
}

func TestVulnerabilityCVE(t *testing.T) {
	if c := VulnerabilityCVE(apiv1.SecurityVulnerability{Advisory: "GHSA-x", Aliases: []string{"cve-2024-1"}}); c != "CVE-2024-1" {
		t.Fatalf("got %q", c)
	}
}
