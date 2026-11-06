package store

import (
	"context"
	"time"
)

// ImageScan is the latest scan of one image digest (SEC-04).
type ImageScan struct {
	Digest     string
	ID         string
	Status     string // apiv1.SecurityImageScanStatus
	FinishedAt time.Time
	// Doc is the apiv1.SecurityImageScan JSON and Vulnerabilities the
	// apiv1.SecurityVulnerability JSON array. The columns above are copies.
	Doc             []byte
	Vulnerabilities []byte
	// Report is the scanner's original report, gzip-compressed; nil when the
	// scan has none and in listings.
	Report    []byte
	CreatedAt time.Time
}

// ScanStore keeps the latest scan of each image digest. It is part of Store.
type ScanStore interface {
	// PutImageScan stores s as its digest's scan. ErrConflict (wrapped) when
	// the stored scan of the digest finished later, or when s.ID is another
	// digest's scan id.
	PutImageScan(ctx context.Context, s ImageScan) error
	// ListImageScans returns the scans that finished at or after since,
	// without Report, ordered by digest.
	ListImageScans(ctx context.Context, since time.Time) ([]ImageScan, error)
}
