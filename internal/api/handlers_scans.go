package api

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/security"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// MaxScanBodyBytes bounds a scan submission: Grype's report of a large image
// is several MiB.
const MaxScanBodyBytes = 32 << 20

var scanDigestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// scanSubmission is apiv1.SecurityScanSubmission with the report kept as
// sent, so its hash is of the scanner's bytes.
type scanSubmission struct {
	Image          string          `json:"image"`
	Digest         string          `json:"digest"`
	Status         string          `json:"status"`
	Message        *string         `json:"message"`
	ScannerVersion *string         `json:"scannerVersion"`
	StartedAt      *time.Time      `json:"startedAt"`
	FinishedAt     *time.Time      `json:"finishedAt"`
	Report         json.RawMessage `json:"report"`
}

// POST /v1/security/scans: stores one scan the image scan workflow ran
// (SEC-04). The API parses and stores; it never scans.
func (s *Server) submitSecurityScan(w http.ResponseWriter, r *http.Request) {
	if s.d.Store == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "store_not_configured", "this server has no store for scans")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxScanBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			s.fail(w, r, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("body exceeds %d bytes", MaxScanBodyBytes))
		} else {
			s.fail(w, r, http.StatusBadRequest, "invalid_request", "could not read body")
		}
		return
	}
	var in scanSubmission
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "invalid JSON: "+err.Error())
		return
	}
	now := time.Now().UTC()
	scan, msg := checkSubmission(in, now)
	if msg != "" {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", msg)
		return
	}
	noteAudit(r, "digest", in.Digest)
	noteAudit(r, "status", in.Status)
	vulns := []apiv1.SecurityVulnerability{}
	var report []byte
	if scan.Status == apiv1.SecurityImageScanStatusSucceeded {
		if scan, vulns, err = security.ParseGrype(in.Report, scan); err != nil {
			s.fail(w, r, http.StatusBadRequest, "invalid_report", err.Error())
			return
		}
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write(in.Report)
		_ = zw.Close()
		report = buf.Bytes()
	}
	doc, err := json.Marshal(scan)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	vdoc, err := json.Marshal(vulns)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	err = s.d.Store.PutImageScan(r.Context(), store.ImageScan{Digest: scan.Digest, ID: scan.Id, Status: string(scan.Status),
		FinishedAt: *scan.FinishedAt, Doc: doc, Vulnerabilities: vdoc, Report: report, CreatedAt: now})
	switch {
	case errors.Is(err, store.ErrConflict):
		s.fail(w, r, http.StatusConflict, "scan_conflict", err.Error())
	case err != nil:
		s.internal(w, r, err)
	default:
		writeJSON(w, http.StatusCreated, scan)
	}
}

// checkSubmission validates in and returns the scan it describes, or why it
// is refused.
func checkSubmission(in scanSubmission, now time.Time) (apiv1.SecurityImageScan, string) {
	var scan apiv1.SecurityImageScan
	if !scanDigestRE.MatchString(in.Digest) {
		return scan, "digest must be sha256: and 64 lowercase hex digits"
	}
	repo, ok := strings.CutSuffix(in.Image, "@"+in.Digest)
	if !ok || repo == "" || strings.ContainsAny(repo, " @") {
		return scan, "image must be a repository followed by @digest"
	}
	hasReport := len(bytes.TrimSpace(in.Report)) > 0 && string(bytes.TrimSpace(in.Report)) != "null"
	status := apiv1.SecurityImageScanStatus(in.Status)
	switch status {
	case apiv1.SecurityImageScanStatusSucceeded:
		if !hasReport {
			return scan, "a succeeded scan needs Grype's report"
		}
	case apiv1.SecurityImageScanStatusFailed, apiv1.SecurityImageScanStatusUnsupported:
		if hasReport {
			return scan, "a scan that did not succeed has no report"
		}
		if in.Message == nil || strings.TrimSpace(*in.Message) == "" {
			return scan, "a scan that did not succeed needs a message saying why"
		}
	default:
		return scan, "status must be succeeded, failed or unsupported"
	}
	finished := now
	if in.FinishedAt != nil {
		finished = in.FinishedAt.UTC()
	}
	if finished.After(now.Add(5 * time.Minute)) {
		return scan, "finishedAt is in the future"
	}
	started := finished
	if in.StartedAt != nil {
		started = in.StartedAt.UTC()
	}
	if started.After(finished) {
		return scan, "startedAt is after finishedAt"
	}
	image := in.Image
	scan = apiv1.SecurityImageScan{Id: security.ScanID(in.Digest, finished), Digest: in.Digest, Image: &image, Status: status,
		StartedAt: &started, FinishedAt: &finished}
	scan.Scanner.Name, scan.Scanner.Version = security.ScannerName, "unknown"
	if in.ScannerVersion != nil && *in.ScannerVersion != "" {
		scan.Scanner.Version = *in.ScannerVersion
	}
	if in.Message != nil && *in.Message != "" {
		m := shortenText(*in.Message, 500)
		scan.Message = &m
	}
	return scan, ""
}

func shortenText(s string, n int) string {
	if r := []rune(strings.TrimSpace(s)); len(r) > n {
		return string(r[:n]) + "..."
	}
	return strings.TrimSpace(s)
}
