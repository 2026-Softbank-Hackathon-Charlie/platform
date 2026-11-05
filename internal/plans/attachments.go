package plans

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/2026-Softbank-Hackathon-Charlie/migration-engine/pkg/plan"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// ErrAttachmentNotFound means the plan has no attachment of that name.
var ErrAttachmentNotFound = errors.New("attachment not found")

// SetStateDir sets the directory attachments are written to before a run:
// <dir>/plans/<ref>/<name>. Use the runner's state directory.
func (s *Service) SetStateDir(dir string) { s.stateDir = dir }

// PutAttachment stores a file the plan refers to as "attachment:<name>".
// Only a plan waiting for approval accepts attachments, and every upload
// raises the plan's revision, so the approver approves the content that will
// run (Approve).
func (s *Service) PutAttachment(ctx context.Context, ref, name string, content []byte, actor string) (apiv1.PlanAttachment, error) {
	sum := sha256.Sum256(content)
	a := store.PlanAttachment{
		Ref:       ref,
		Name:      name,
		Content:   content,
		SHA256:    hex.EncodeToString(sum[:]),
		Size:      int64(len(content)),
		CreatedBy: actor,
		CreatedAt: s.now(),
	}
	err := s.store.PutPlanAttachment(ctx, a)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return apiv1.PlanAttachment{}, ErrNotFound
	case errors.Is(err, store.ErrConflict):
		return apiv1.PlanAttachment{}, fmt.Errorf("%w: %v; attachments can only change while the plan is pending_approval", ErrWrongStatus, err)
	case err != nil:
		return apiv1.PlanAttachment{}, err
	}
	return attachmentInfo(a), nil
}

// GetAttachment returns an attachment with its content.
func (s *Service) GetAttachment(ctx context.Context, ref, name string) (store.PlanAttachment, error) {
	if _, err := s.get(ctx, ref); err != nil {
		return store.PlanAttachment{}, err
	}
	a, err := s.store.GetPlanAttachment(ctx, ref, name)
	if errors.Is(err, store.ErrNotFound) {
		return store.PlanAttachment{}, ErrAttachmentNotFound
	}
	return a, err
}

// recordOf is record with the plan's attachments.
func (s *Service) recordOf(ctx context.Context, row store.Plan) (apiv1.PlanRecord, error) {
	rec, err := record(row)
	if err != nil {
		return apiv1.PlanRecord{}, err
	}
	recs := []apiv1.PlanRecord{rec}
	if err := s.withAttachments(ctx, recs); err != nil {
		return apiv1.PlanRecord{}, err
	}
	if err := s.withRehearsal(ctx, []store.Plan{row}, recs); err != nil {
		return apiv1.PlanRecord{}, err
	}
	return recs[0], nil
}

// withRehearsal sets Rehearsal on the records of plans waiting for approval:
// whether approving needs rehearsal evidence and whether it is there. rows
// and recs are in the same order.
func (s *Service) withRehearsal(ctx context.Context, rows []store.Plan, recs []apiv1.PlanRecord) error {
	var refs []string
	for _, row := range rows {
		if row.Status == store.PlanPending {
			refs = append(refs, row.Ref)
		}
	}
	if len(refs) == 0 {
		return nil
	}
	list, err := s.store.ListPlanAttachments(ctx, refs)
	if err != nil {
		return err
	}
	byRef := map[string][]store.PlanAttachment{}
	for _, a := range list {
		byRef[a.Ref] = append(byRef[a.Ref], a)
	}
	for i, row := range rows {
		if row.Status != store.PlanPending {
			continue
		}
		g, err := s.rehearsalGate(ctx, row, byRef[row.Ref])
		if err != nil {
			return err
		}
		recs[i].Rehearsal = g
	}
	return nil
}

// withAttachments sets Attachments on every record with one store read.
func (s *Service) withAttachments(ctx context.Context, recs []apiv1.PlanRecord) error {
	refs := make([]string, len(recs))
	for i, r := range recs {
		refs[i] = r.Ref
	}
	list, err := s.store.ListPlanAttachments(ctx, refs)
	if err != nil {
		return err
	}
	byRef := map[string][]apiv1.PlanAttachment{}
	for _, a := range list {
		byRef[a.Ref] = append(byRef[a.Ref], attachmentInfo(a))
	}
	for i := range recs {
		infos := byRef[recs[i].Ref]
		if infos == nil {
			infos = []apiv1.PlanAttachment{}
		}
		recs[i].Attachments = &infos
	}
	return nil
}

// requireAttachments fails when a pending plan refers to an attachment that
// is not among attached. Attachments are never deleted, so a plan that passes
// here still has its attachment when it starts.
func requireAttachments(row store.Plan, attached []store.PlanAttachment) error {
	if row.Status != store.PlanPending {
		return nil // the transition reports the status
	}
	var refs struct {
		Target struct {
			Manifest string `json:"manifest"`
		} `json:"target"`
	}
	if err := json.Unmarshal(row.Raw, &refs); err != nil {
		return fmt.Errorf("decode plan %s: %w", row.Ref, err)
	}
	name, ok := ManifestAttachment(&plan.Plan{Target: plan.Endpoint{Manifest: refs.Target.Manifest}})
	if !ok {
		return nil
	}
	for _, a := range attached {
		if a.Name == name {
			return nil
		}
	}
	return fmt.Errorf("%w: target.manifest refers to attachment %q, which has not been uploaded (PUT /v1/plans/%s/attachments/%s)",
		ErrWrongStatus, name, row.Ref, name)
}

// materialize writes the attachment that target.manifest refers to under the
// state directory and points target.manifest at that file, which is what the
// engine reads in prepare. The policy has already checked the reference; the
// path it is replaced with never comes from the plan.
func (s *Service) materialize(ctx context.Context, ref string, p *plan.Plan) error {
	name, ok := ManifestAttachment(p)
	if !ok {
		return nil
	}
	a, err := s.store.GetPlanAttachment(ctx, ref, name)
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: target.manifest refers to attachment %q, which does not exist", ErrWrongStatus, name)
	}
	if err != nil {
		return err
	}
	dir := filepath.Join(s.stateDir, "plans", ref)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, name)
	if err := writeFile0600(path, a.Content); err != nil {
		return fmt.Errorf("write attachment %s of plan %s: %w", name, ref, err)
	}
	p.Target.Manifest = path
	return nil
}

// writeFile0600 replaces path atomically with a file only the server user can read.
func writeFile0600(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*") // mode 0600
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}

func attachmentInfo(a store.PlanAttachment) apiv1.PlanAttachment {
	return apiv1.PlanAttachment{Name: a.Name, Size: a.Size, Sha256: a.SHA256}
}
