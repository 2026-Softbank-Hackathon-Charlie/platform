package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/plans"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/runs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// --- written records ---

// MaxWrittenRecords bounds one POST /v1/written-records/{stream}.
const MaxWrittenRecords = 5000

// maxPKChars bounds a written record's primary key text.
const maxPKChars = 200

// writtenTable is "schema.table" or "table" with unquoted PostgreSQL identifiers.
var writtenTable = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_$]{0,62}(\.[A-Za-z_][A-Za-z0-9_$]{0,62})?$`)

// POST /v1/written-records/{stream}: writes a load generator saw succeed.
func (s *Server) appendWrittenRecords(w http.ResponseWriter, r *http.Request) {
	stream := r.PathValue("stream")
	noteAudit(r, "stream", stream)
	if !plans.StreamName.MatchString(stream) {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "stream name must match "+plans.StreamName.String())
		return
	}
	var req apiv1.WrittenRecordsRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	n := len(req.Records)
	if n < 1 || n > MaxWrittenRecords {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", fmt.Sprintf("records must hold 1 to %d records, got %d", MaxWrittenRecords, n))
		return
	}
	now := time.Now().UTC()
	recs := make([]store.WrittenRecord, n)
	for i, rec := range req.Records {
		switch {
		case !writtenTable.MatchString(rec.Table): // no control characters either
			s.fail(w, r, http.StatusBadRequest, "invalid_request", fmt.Sprintf("records[%d].table %q must be schema.table or table", i, rec.Table))
			return
		case rec.Pk == "" || utf8.RuneCountInString(rec.Pk) > maxPKChars:
			s.fail(w, r, http.StatusBadRequest, "invalid_request", fmt.Sprintf("records[%d].pk must be 1 to %d characters", i, maxPKChars))
			return
		case strings.ContainsFunc(rec.Pk, unicode.IsControl): // PostgreSQL text cannot hold NUL
			s.fail(w, r, http.StatusBadRequest, "invalid_request", fmt.Sprintf("records[%d].pk must not contain control characters", i))
			return
		}
		at := now
		if rec.At != nil {
			at = rec.At.UTC()
		}
		// Unquoted identifiers are lower case in PostgreSQL, which is what verify looks up.
		recs[i] = store.WrittenRecord{Table: strings.ToLower(rec.Table), PK: rec.Pk, At: at, ReceivedAt: now}
	}
	if err := s.d.Store.AppendWrittenRecords(context.WithoutCancel(r.Context()), stream, recs); err != nil {
		s.internal(w, r, err)
		return
	}
	noteAudit(r, "accepted", n)
	writeJSON(w, http.StatusAccepted, apiv1.WrittenRecordsAccepted{Accepted: n})
}

func (s *Server) writtenRecordStats(w http.ResponseWriter, r *http.Request) {
	stream := r.PathValue("stream")
	if !plans.StreamName.MatchString(stream) {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "stream name must match "+plans.StreamName.String())
		return
	}
	st, err := s.d.Store.WrittenRecordStats(r.Context(), stream)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, apiv1.WrittenRecordStats{Stream: stream, Count: st.Count, LatestAt: st.LatestAt})
}

// --- plan attachments ---

// PUT /v1/plans/{ref}/attachments/{name}: the body is the file as is.
func (s *Server) putPlanAttachment(w http.ResponseWriter, r *http.Request) {
	ref, name := r.PathValue("ref"), r.PathValue("name")
	noteAudit(r, "planRef", ref)
	noteAudit(r, "attachment", name)
	if !plans.ValidAttachmentName(name) {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "attachment name must be 1 to 100 letters, digits, '.', '_' or '-', and not . or ..")
		return
	}
	body, ok := s.readBody(w, r)
	if !ok {
		return
	}
	if len(body) == 0 {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "the attachment is empty")
		return
	}
	info, err := s.d.Plans.PutAttachment(context.WithoutCancel(r.Context()), ref, name, body, actor(r))
	if err != nil {
		s.planError(w, r, err)
		return
	}
	noteAudit(r, "size", info.Size)
	noteAudit(r, "sha256", info.Sha256)
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) getPlanAttachment(w http.ResponseWriter, r *http.Request) {
	ref, name := r.PathValue("ref"), r.PathValue("name")
	a, err := s.d.Plans.GetAttachment(r.Context(), ref, name)
	if errors.Is(err, plans.ErrAttachmentNotFound) {
		s.fail(w, r, http.StatusNotFound, "attachment_not_found", "the plan has no attachment named "+name)
		return
	}
	if err != nil {
		s.planError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(a.Content)
}

// --- apps and plan drafts ---

// POST /v1/plans/draft: an engine plan built from the apps file. Not stored.
func (s *Server) draftPlan(w http.ResponseWriter, r *http.Request) {
	var req apiv1.PlanDraftRequest
	if !s.decode(w, r, &req, true) {
		return
	}
	noteAudit(r, "app", req.App)
	noteAudit(r, "route", req.Source+" -> "+req.Target)
	if s.d.Apps == nil {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", "this server has no apps configured (PLATFORM_APPS_FILE)")
		return
	}
	p, err := s.d.Apps.Draft(req)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if req.SourceStats == nil || !*req.SourceStats {
		writeJSON(w, http.StatusOK, p)
		return
	}
	res := apiv1.PlanDraftResult{Plan: p}
	if st, ok := s.measureSource(r.Context(), p.Source.Dsn); ok {
		res.SourceStats = st
	} else {
		reason := apiv1.PlanDraftResultSourceStatsReasonNotMeasured
		res.SourceStatsReason = &reason
	}
	noteAudit(r, "sourceStats", res.SourceStats != nil)
	writeJSON(w, http.StatusOK, res)
}

// measureSource expands a draft's source DSN with the server's environment, as
// a submitted plan is expanded at start, and measures it. A DSN that names an
// unset variable is not tried.
func (s *Server) measureSource(ctx context.Context, dsn string) (*apiv1.SourceStats, bool) {
	if s.d.SourceStats == nil {
		return nil, false
	}
	missing := false
	dsn = os.Expand(dsn, func(name string) string {
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			missing = true
		}
		return v
	})
	if missing {
		return nil, false
	}
	st, ok := s.d.SourceStats.MeasureSource(ctx, dsn)
	if !ok {
		return nil, false
	}
	return &apiv1.SourceStats{
		Sessions:           st.Sessions,
		MaxConnections:     st.MaxConnections,
		MaxWorkerProcesses: st.MaxWorkerProcesses,
		Bytes:              st.Bytes,
		WalBytesPerSecond:  st.WALBytesPerSecond,
		WindowSeconds:      st.Window.Seconds(),
		MeasuredAt:         st.MeasuredAt,
	}, true
}

// --- rollback ---

// POST /v1/migrations/{runId}/rollback: migrate a succeeded run back. The
// operator's call is the human approval of the reverse plan.
func (s *Server) rollbackMigration(w http.ResponseWriter, r *http.Request) {
	var req apiv1.RollbackRequest
	if !s.decode(w, r, &req, false) {
		return
	}
	id := r.PathValue("runId")
	noteAudit(r, "fromRun", id)
	if req.Reason != nil {
		noteAudit(r, "reason", *req.Reason)
	}
	// Like a start, every write below must finish even if the client goes away.
	ctx := context.WithoutCancel(r.Context())

	s.startMu.Lock()
	defer s.startMu.Unlock()
	res, f := s.startRollback(ctx, id, actor(r), deref(req.Reason), req.AutoSwitch == nil || *req.AutoSwitch)
	if res.ref != "" {
		noteAudit(r, "planRef", res.ref)
		noteAudit(r, "reusedPlan", res.reused)
	}
	if res.residency != "" {
		noteAudit(r, "residency", res.residency)
	}
	if res.capacity != "" {
		noteAudit(r, "capacity", map[string]any{"outcome": res.capacity})
	}
	if f != nil {
		switch f.kind {
		case failRun:
			s.runError(w, r, f.err)
		case failPlan:
			s.planError(w, r, f.err)
		case failConflict:
			s.fail(w, r, http.StatusConflict, "run_state_conflict", f.Error())
		default:
			s.internal(w, r, f.err)
		}
		return
	}
	noteAudit(r, "runId", res.run.RunId)
	noteAudit(r, "autoSwitch", res.run.AutoSwitch)
	w.Header().Set("Location", "/v1/migrations/"+res.run.RunId)
	writeJSON(w, http.StatusAccepted, startResponse(res.run.RunId, res.ref))
}

// Kinds of rollback failures, mapped to responses by the handler.
const (
	failRun = iota + 1
	failPlan
	failConflict
	failInternal
)

type rollbackFailure struct {
	kind int
	err  error
}

func (f *rollbackFailure) Error() string { return f.err.Error() }
func (f *rollbackFailure) Unwrap() error { return f.err }

type rollbackStarted struct {
	run       apiv1.Run
	ref       string
	reused    bool
	residency string // plans.Residency*, set once the plan is reserved
	capacity  string // plans.Capacity*, set once the plan is reserved
}

// startRollback builds (or reuses) the reverse plan of a succeeded run and
// starts it. Callers hold startMu. The handler and the observe phase's
// automatic rollback both use it, so they follow the same rules.
func (s *Server) startRollback(ctx context.Context, id, who, reason string, autoSwitch bool) (rollbackStarted, *rollbackFailure) {
	var out rollbackStarted
	orig, err := s.d.Runs.Get(ctx, id)
	if err != nil {
		return out, &rollbackFailure{failRun, err}
	}
	if orig.Status != apiv1.RunStatusSucceeded {
		return out, &rollbackFailure{failConflict, fmt.Errorf("run is %s; only a succeeded run can be rolled back", orig.Status)}
	}
	list, err := s.d.Runs.List(ctx)
	if err != nil {
		return out, &rollbackFailure{failInternal, err}
	}
	for _, run := range list {
		if run.RollbackOf != nil && *run.RollbackOf == id {
			return out, &rollbackFailure{failConflict, fmt.Errorf("run %s already rolls back this run; resume or watch it instead", run.RunId)}
		}
	}

	// A run is rolled back at most once. An earlier rollback whose start
	// failed left its plan approved: start that plan again rather than
	// creating another one next to it.
	prev, found, err := s.d.Plans.FindRollback(ctx, id)
	if err != nil {
		return out, &rollbackFailure{failInternal, err}
	}
	if found && prev.Status == apiv1.PlanStatusStarted {
		return out, &rollbackFailure{failConflict, fmt.Errorf("run %s already rolls back this run; resume or watch it instead", deref(prev.RunId))}
	}
	ref := prev.Ref
	if !found {
		comment := "rollback of run " + id
		if reason != "" {
			comment += ": " + reason
		}
		rec, err := s.d.Plans.Rollback(ctx, id, orig.PlanRef, who, comment)
		if err != nil {
			return out, &rollbackFailure{failPlan, err}
		}
		ref = rec.Ref
	}
	out.ref, out.reused = ref, found
	res, err := s.d.Plans.Reserve(ctx, ref)
	if err != nil {
		return out, &rollbackFailure{failPlan, err}
	}
	out.residency, out.capacity = res.Residency.Outcome, res.Capacity.Outcome
	run, err := s.d.Runs.Start(ctx, runs.StartOptions{
		PlanRef:    ref,
		Plan:       res.Plan,
		Source:     res.Summary.Source,
		Target:     res.Summary.Target,
		Actor:      who,
		AutoSwitch: autoSwitch,
		RollbackOf: id,
	})
	if err != nil {
		s.d.Plans.Release(ctx, ref) // stays approved; the next rollback call starts it again
		return out, &rollbackFailure{failRun, err}
	}
	if err := s.d.Plans.Started(ctx, ref, run.RunId); err != nil {
		s.cfg.Log.Error("link plan to run", "plan", ref, "run", run.RunId, "err", err)
	}
	out.run = run
	return out, nil
}

// autoRollback is the observe phase's rollback after an SLO violation
// (runs.AutoRollbackFunc). It is recorded in the audit log like an operator's
// call, with the actor slo-guard.
func (s *Server) autoRollback(ctx context.Context, id, reason string) (string, error) {
	s.startMu.Lock()
	res, f := s.startRollback(ctx, id, runs.AutoRollbackActor, reason, true)
	s.startMu.Unlock()

	note := map[string]any{"fromRun": id, "reason": reason, "automatic": true}
	status := http.StatusAccepted
	if res.ref != "" {
		note["planRef"], note["reusedPlan"] = res.ref, res.reused
	}
	if res.residency != "" {
		note["residency"] = res.residency
	}
	if res.capacity != "" {
		note["capacity"] = map[string]any{"outcome": res.capacity}
	}
	if f != nil {
		status = http.StatusInternalServerError
		if f.kind == failConflict {
			status = http.StatusConflict
		}
		note["error"] = f.Error()
	} else {
		note["runId"] = res.run.RunId
	}
	detail, _ := json.Marshal(note)
	actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.d.Store.AppendAudit(actx, store.Audit{
		Time: time.Now().UTC(), Actor: runs.AutoRollbackActor, Role: "system",
		Action: "POST /v1/migrations/{runId}/rollback", Target: "/v1/migrations/" + id + "/rollback",
		Status: status, Detail: detail,
	}); err != nil {
		s.cfg.Log.Error("audit log write failed", "err", err, "action", "automatic rollback", "run", id)
	}
	if f != nil {
		return "", f
	}
	return res.run.RunId, nil
}
