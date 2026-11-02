package store

import (
	"context"
	"time"
)

// WrittenRecord is one write a load generator saw succeed. When a plan sets
// verify.writtenRecords to "stream:<name>", verification checks that every
// record of the stream exists on the target database.
type WrittenRecord struct {
	// Seq is assigned by AppendWrittenRecords and increases within a stream.
	Seq    int64
	Stream string
	Table  string // "schema.table" or "table"
	PK     string // as text; composite keys as "(a,b)"
	// At is when the write succeeded: the client's time, or ReceivedAt.
	At         time.Time
	ReceivedAt time.Time
}

// WrittenStats summarizes one stream.
type WrittenStats struct {
	Count    int64
	LatestAt *time.Time // the largest At; nil when the stream is empty
}

// PlanAttachment is a file a plan refers to as "attachment:<name>", such as
// the target CNPG Cluster manifest.
type PlanAttachment struct {
	Ref       string
	Name      string
	Content   []byte // nil in listings
	SHA256    string // hex
	Size      int64
	CreatedBy string
	CreatedAt time.Time
}

// PlanTools stores written-records streams and plan attachments. It is part
// of Store.
type PlanTools interface {
	// AppendWrittenRecords stores recs under stream in the given order and
	// assigns Seq. Each record's Stream and Seq are ignored.
	AppendWrittenRecords(ctx context.Context, stream string, recs []WrittenRecord) error
	// ListWrittenRecords returns a stream's records with Seq > afterSeq in Seq order, at most limit (<=0 means no limit).
	ListWrittenRecords(ctx context.Context, stream string, afterSeq int64, limit int) ([]WrittenRecord, error)
	// WrittenRecordStats counts a stream. An unknown stream has count 0.
	WrittenRecordStats(ctx context.Context, stream string) (WrittenStats, error)

	// PutPlanAttachment inserts or replaces an attachment of a plan that is
	// pending_approval, atomically with respect to plan transitions: an
	// approval either sees the write or makes it fail. ErrNotFound when the
	// plan does not exist; ErrConflict (wrapped, with the current status in
	// the message) when it is not pending_approval.
	PutPlanAttachment(ctx context.Context, a PlanAttachment) error
	// GetPlanAttachment returns ErrNotFound when the plan has no attachment of that name.
	GetPlanAttachment(ctx context.Context, ref, name string) (PlanAttachment, error)
	// ListPlanAttachments returns the attachments of refs without Content, ordered by ref and name.
	ListPlanAttachments(ctx context.Context, refs []string) ([]PlanAttachment, error)
}
