package security

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// ScanMaxAge is how old a stored scan may be and still count. The scan
// workflow rescans running digests daily; a digest whose last scan is older
// is not running or not being scanned, and counts as not scanned.
const ScanMaxAge = 7 * 24 * time.Hour

// ScanRecords reads stored scans (store.Store).
type ScanRecords interface {
	ListImageScans(ctx context.Context, since time.Time) ([]store.ImageScan, error)
}

// StoredScans is the ScanResults of the scans the scan workflow posted
// (SEC-04). It only reads; it never scans.
type StoredScans struct {
	Store ScanRecords
	Now   func() time.Time // time.Now when nil
}

// Latest returns the stored scans younger than ScanMaxAge and the
// vulnerabilities of the succeeded ones.
func (s StoredScans) Latest() ([]apiv1.SecurityImageScan, []apiv1.SecurityVulnerability, error) {
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := s.Store.ListImageScans(ctx, now().Add(-ScanMaxAge))
	if err != nil {
		return nil, nil, err
	}
	scans, vulns := []apiv1.SecurityImageScan{}, []apiv1.SecurityVulnerability{}
	for _, row := range rows {
		var sc apiv1.SecurityImageScan
		if err := json.Unmarshal(row.Doc, &sc); err != nil {
			return nil, nil, fmt.Errorf("stored scan %s: %w", row.ID, err)
		}
		scans = append(scans, sc)
		if sc.Status != apiv1.SecurityImageScanStatusSucceeded {
			continue
		}
		var vs []apiv1.SecurityVulnerability
		if err := json.Unmarshal(row.Vulnerabilities, &vs); err != nil {
			return nil, nil, fmt.Errorf("stored vulnerabilities of scan %s: %w", row.ID, err)
		}
		vulns = append(vulns, vs...)
	}
	return scans, vulns, nil
}
