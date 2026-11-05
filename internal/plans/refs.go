package plans

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
)

// A submitted plan may name two kinds of server-side files by reference
// instead of by path. The server resolves both at run time; the engine only
// ever sees real paths below the state directory.
//
//	target.manifest:       "attachment:<name>"  an uploaded plan attachment
//	verify.writtenRecords: "stream:<name>"      a written-records stream
const (
	attachmentPrefix = "attachment:"
	streamPrefix     = "stream:"
)

var (
	// StreamName is the name of a written-records stream.
	StreamName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	// attachmentName also allows "." and ".."; ValidAttachmentName rejects them.
	attachmentName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// ValidAttachmentName reports whether name can name a plan attachment. The
// name becomes a file name on the server, so "." and ".." are refused.
func ValidAttachmentName(name string) bool {
	return attachmentName.MatchString(name) && name != "." && name != ".."
}

// ManifestAttachment returns the attachment name when target.manifest is
// "attachment:<name>".
func ManifestAttachment(p *plan.Plan) (string, bool) {
	return strings.CutPrefix(p.Target.Manifest, attachmentPrefix)
}

// WrittenStream returns the stream name when verify.writtenRecords is
// "stream:<name>".
func WrittenStream(p *plan.Plan) (string, bool) {
	return strings.CutPrefix(p.Verify.WrittenRecords, streamPrefix)
}

// checkManifest accepts an attachment reference or, as before, a relative path.
func checkManifest(v string) error {
	if name, ok := strings.CutPrefix(v, attachmentPrefix); ok {
		if !ValidAttachmentName(name) {
			return fmt.Errorf("target.manifest: attachment name %q must match %s and not be . or ..", name, attachmentName)
		}
		if name == RehearsalReportName {
			return fmt.Errorf("target.manifest: %q is the rehearsal report, not a manifest", name)
		}
		return nil
	}
	return safeRelPath("target.manifest", v)
}

// checkWrittenRecords accepts a stream reference or, as before, a relative path.
func checkWrittenRecords(v string) error {
	if name, ok := strings.CutPrefix(v, streamPrefix); ok {
		if !StreamName.MatchString(name) {
			return fmt.Errorf("verify.writtenRecords: stream name %q must match %s", name, StreamName)
		}
		return nil
	}
	return safeRelPath("verify.writtenRecords", v)
}
