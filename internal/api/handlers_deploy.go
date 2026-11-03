package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/deploy"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// AppRemover removes a registered app's workloads and records
// (deploy.Executor.RemoveApp).
type AppRemover interface {
	// RemoveApp supersedes the app's unfinished deployments, commits the
	// removal of apps/<name>/ and of every added branch's apps/<instance>/
	// from the gitops repository, removes every instance from the traffic
	// gateway and deletes the app from the store.
	// store.ErrNotFound when it does not exist, deploy.ErrWritesHeld when the
	// gateway holds its writes, deploy.ErrGitOps and deploy.ErrGateway when
	// those could not be reached.
	RemoveApp(ctx context.Context, name string) error
	// RemoveInstance removes the instance of an added branch of an app: it
	// supersedes the instance's unfinished deployments, commits the removal
	// of apps/<instance>/ from the gitops repository, removes the instance
	// from the traffic gateway and deletes the branch with its deployments
	// from the store. store.ErrNotFound when the app does not have the
	// branch; otherwise the same errors as RemoveApp.
	RemoveInstance(ctx context.Context, app, branch string) error
}

// DeploymentEventPoll is how often a deployment's event stream reads new
// events from the store. Deployments run in the deployer, and the store is
// where its events meet the API.
var DeploymentEventPoll = 500 * time.Millisecond

// deploymentEventPage is how many stored events a stream reads at a time.
const deploymentEventPage = 500

// registeredApp returns the app registered from GitHub, or answers 404
// app_not_found or, for an app only in PLATFORM_APPS_FILE, 409
// app_not_registered.
func (s *Server) registeredApp(w http.ResponseWriter, r *http.Request, name string) (store.App, bool) {
	app, err := s.d.Store.GetApp(r.Context(), name)
	switch {
	case err == nil:
		return app, true
	case !errors.Is(err, store.ErrNotFound):
		s.internal(w, r, err)
	case s.isFileApp(name):
		s.fail(w, r, http.StatusConflict, "app_not_registered", "app "+name+" is only in PLATFORM_APPS_FILE; register it from GitHub (POST /v1/apps) to deploy it")
	default:
		s.fail(w, r, http.StatusNotFound, "app_not_found", "app not found")
	}
	return store.App{}, false
}

// isFileApp reports whether PLATFORM_APPS_FILE has the app.
func (s *Server) isFileApp(name string) bool {
	_, ok := s.fileApp(name)
	return ok
}

// POST /v1/apps/{name}/deployments
func (s *Server) createDeployment(w http.ResponseWriter, r *http.Request) {
	var req apiv1.DeploymentCreateRequest
	if !s.decode(w, r, &req, false) {
		return
	}
	name := r.PathValue("name")
	noteAudit(r, "app", name)
	app, ok := s.registeredApp(w, r, name)
	if !ok {
		return
	}
	branch := app.Branch
	if req.Branch != nil {
		branch = *req.Branch
	}
	in, ok, err := s.instanceOf(r.Context(), app, branch)
	switch {
	case err != nil:
		s.internal(w, r, err)
		return
	case !ok:
		s.fail(w, r, http.StatusBadRequest, "invalid_request", fmt.Sprintf("app %s does not deploy branch %q; add it with POST /v1/apps/%s/instances", name, branch, name))
		return
	}
	noteAudit(r, "instance", in.Instance)
	targets := app.Environments
	if req.Environments != nil {
		asked := *req.Environments
		if len(asked) == 0 {
			s.fail(w, r, http.StatusBadRequest, "invalid_request", "environments must name at least one environment, or be left out to deploy to all of them")
			return
		}
		for i, e := range asked {
			if !slices.Contains(app.Environments, e) {
				s.fail(w, r, http.StatusBadRequest, "invalid_request", fmt.Sprintf("app %s is not registered for environment %q (it is for %v)", name, e, app.Environments))
				return
			}
			if slices.Contains(asked[:i], e) {
				s.fail(w, r, http.StatusBadRequest, "invalid_request", fmt.Sprintf("environment %q is listed twice", e))
				return
			}
		}
		// In the app's order, whatever order they were asked in.
		targets = slices.DeleteFunc(slices.Clone(app.Environments), func(e string) bool { return !slices.Contains(asked, e) })
	}
	ref := branch
	if req.Ref != nil {
		if *req.Ref == "" {
			s.fail(w, r, http.StatusBadRequest, "invalid_request", "ref must not be empty; leave it out to deploy the head of the branch")
			return
		}
		ref = *req.Ref
	}
	noteAudit(r, "ref", ref)
	noteAudit(r, "environments", targets)
	if s.d.GitHub == nil {
		s.githubNotConfigured(w, r)
		return
	}

	// From here on the writes must finish even if the client goes away.
	ctx := context.WithoutCancel(r.Context())
	commit, err := s.d.GitHub.ResolveRef(ctx, app.Repo, ref)
	if errors.Is(err, fs.ErrNotExist) {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", fmt.Sprintf("%s has no branch, tag or commit %q", app.Repo, ref))
		return
	}
	if err != nil {
		s.githubUnreachable(w, r, err)
		return
	}
	noteAudit(r, "commit", commit)
	if s.appSpecAt(ctx, w, r, app, commit) == nil {
		return
	}

	to := app
	to.Environments = targets
	d := newDeployment(to, in, ref, commit, apiv1.DeploymentTriggerManual, actor(r), storeTime())
	if err := s.d.Store.CreateDeployment(ctx, d); err != nil {
		switch {
		case errors.Is(err, store.ErrInstanceNotFound): // the app's branch changed or the instance was removed meanwhile
			s.fail(w, r, http.StatusBadRequest, "invalid_request", fmt.Sprintf("branch %s is not deployed by app %s", d.Branch, name))
		case errors.Is(err, store.ErrNotFound):
			s.fail(w, r, http.StatusNotFound, "app_not_found", "app not found")
		default:
			s.internal(w, r, err)
		}
		return
	}
	noteAudit(r, "deploymentId", d.ID)
	s.enqueue(ctx, d.ID)
	stored, err := s.d.Store.GetDeployment(ctx, d.ID)
	if err != nil {
		stored = d // deleted with its app in between: answer what was created
	}
	w.Header().Set("Location", "/v1/deployments/"+d.ID)
	writeJSON(w, http.StatusAccepted, deploymentJSON(stored))
}

// GET /v1/apps/{name}/deployments
func (s *Server) listDeployments(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 200 {
			s.fail(w, r, http.StatusBadRequest, "invalid_request", "limit must be between 1 and 200")
			return
		}
		limit = n
	}
	name := r.PathValue("name")
	out := apiv1.DeploymentList{Deployments: []apiv1.Deployment{}}
	app, err := s.d.Store.GetApp(r.Context(), name)
	if err != nil {
		switch {
		case !errors.Is(err, store.ErrNotFound):
			s.internal(w, r, err)
		case s.isFileApp(name):
			writeJSON(w, http.StatusOK, out)
		default:
			s.fail(w, r, http.StatusNotFound, "app_not_found", "app not found")
		}
		return
	}
	var list []store.Deployment
	if branch := r.URL.Query().Get("branch"); branch == "" {
		list, err = s.d.Store.ListDeployments(r.Context(), name, limit)
	} else {
		in, ok, ierr := s.instanceOf(r.Context(), app, branch)
		switch {
		case ierr != nil:
			s.internal(w, r, ierr)
			return
		case !ok:
			s.fail(w, r, http.StatusBadRequest, "invalid_request", fmt.Sprintf("branch %q is not deployed by app %s", branch, name))
			return
		}
		list, err = s.d.Store.ListInstanceDeployments(r.Context(), in.Instance, limit)
	}
	if err != nil {
		s.internal(w, r, err)
		return
	}
	for _, d := range list {
		out.Deployments = append(out.Deployments, deploymentJSON(d))
	}
	writeJSON(w, http.StatusOK, out)
}

// instanceOf returns the instance that deploys branch of app, and false
// when the app does not deploy the branch. The app's own branch is the
// default instance (defaultInstance), named after the app.
func (s *Server) instanceOf(ctx context.Context, app store.App, branch string) (store.AppBranch, bool, error) {
	if branch == app.Branch {
		return defaultInstance(app), true, nil
	}
	branches, err := s.d.Store.ListAppBranches(ctx, app.Name)
	if err != nil {
		return store.AppBranch{}, false, err
	}
	for _, b := range branches {
		if b.Branch == branch {
			return b, true, nil
		}
	}
	return store.AppBranch{}, false, nil
}

// GET /v1/deployments/{id}
func (s *Server) getDeployment(w http.ResponseWriter, r *http.Request) {
	d, err := s.d.Store.GetDeployment(r.Context(), r.PathValue("id"))
	if err != nil {
		s.deploymentError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, deploymentJSON(d))
}

func (s *Server) deploymentError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, http.StatusNotFound, "deployment_not_found", "deployment not found")
		return
	}
	s.internal(w, r, err)
}

// DELETE /v1/apps/{name}
func (s *Server) deleteApp(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	noteAudit(r, "app", name)
	if _, ok := s.registeredApp(w, r, name); !ok {
		return
	}
	if s.d.Remover == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "github_not_configured", "this server has no deployer (PLATFORM_DEPLOY_BUILDER), so it cannot remove the app's files from the gitops repository")
		return
	}
	err := s.d.Remover.RemoveApp(context.WithoutCancel(r.Context()), name)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusAccepted)
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "app_not_found", "app not found")
	case errors.Is(err, deploy.ErrWritesHeld):
		s.fail(w, r, http.StatusConflict, "cluster_busy", "the traffic gateway holds the app's writes (a migration is switching it); delete the app once the migration is done")
	case errors.Is(err, deploy.ErrGitOps):
		s.githubUnreachable(w, r, err)
	case errors.Is(err, deploy.ErrGateway):
		s.cfg.Log.Warn("gateway", "err", err)
		s.fail(w, r, http.StatusBadGateway, "gateway_unreachable", "could not remove the app from the traffic gateway; its gitops files are already removed, so try again")
	default:
		s.internal(w, r, err)
	}
}

// DELETE /v1/apps/{name}/instances/{instance}
func (s *Server) deleteAppInstance(w http.ResponseWriter, r *http.Request) {
	name, instance := r.PathValue("name"), r.PathValue("instance")
	noteAudit(r, "app", name)
	noteAudit(r, "instance", instance)
	app, ok := s.registeredApp(w, r, name)
	if !ok {
		return
	}
	if instance == app.Name {
		s.fail(w, r, http.StatusConflict, "default_instance", fmt.Sprintf("%s is app %s's default instance; it is removed only with the app (DELETE /v1/apps/%s)", instance, name, name))
		return
	}
	branches, err := s.d.Store.ListAppBranches(r.Context(), name)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	i := slices.IndexFunc(branches, func(b store.AppBranch) bool { return b.Instance == instance })
	if i < 0 {
		s.fail(w, r, http.StatusNotFound, "instance_not_found", fmt.Sprintf("app %s has no instance %s", name, instance))
		return
	}
	branch := branches[i].Branch
	noteAudit(r, "branch", branch)
	if s.d.Remover == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "github_not_configured", "this server has no deployer (PLATFORM_DEPLOY_BUILDER), so it cannot remove the instance's files from the gitops repository")
		return
	}
	err = s.d.Remover.RemoveInstance(context.WithoutCancel(r.Context()), app.Name, branch)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusAccepted)
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, r, http.StatusNotFound, "instance_not_found", fmt.Sprintf("app %s has no instance %s", name, instance))
	case errors.Is(err, deploy.ErrWritesHeld):
		s.fail(w, r, http.StatusConflict, "cluster_busy", "the traffic gateway holds the instance's writes (a migration is switching it); delete the instance once the migration is done")
	case errors.Is(err, deploy.ErrGitOps):
		s.githubUnreachable(w, r, err)
	case errors.Is(err, deploy.ErrGateway):
		s.cfg.Log.Warn("gateway", "err", err)
		s.fail(w, r, http.StatusBadGateway, "gateway_unreachable", "could not remove the instance from the traffic gateway; its gitops files are already removed, so try again")
	default:
		s.internal(w, r, err)
	}
}

// GET /v1/deployments/{id}/events
//
// The stream contract is the run events' one (see migrationEvents): a
// "snapshot" with the deployment first, then stored events after
// Last-Event-ID (or ?after=), then new ones as they are stored, with the
// event type as the SSE event name and the seq as the SSE id; once the
// deployment is finished, a final "snapshot" and the stream closes. The
// stream reads the store every DeploymentEventPoll, so it follows a
// deployment whichever process runs it, and a queued one too.
func (s *Server) deploymentEvents(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ctx := r.Context()
	after, err := lastEventID(r)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		s.internal(w, r, errors.New("response writer cannot stream"))
		return
	}
	d, err := s.d.Store.GetDeployment(ctx, id)
	if err != nil {
		s.deploymentError(w, r, err)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if writeSSE(w, "snapshot", 0, deploymentJSON(d)) != nil {
		return
	}
	last := after
	// send writes the stored events after last and reports whether one was
	// the deployment's done event.
	send := func() (done bool, err error) {
		for {
			page, err := s.d.Store.ListDeploymentEvents(ctx, id, last, deploymentEventPage)
			if err != nil {
				return false, err
			}
			for _, ev := range page {
				if err := writeSSE(w, ev.Type, ev.Seq, deploymentEventJSON(ev)); err != nil {
					return false, err
				}
				last = ev.Seq
				done = done || ev.Type == deploy.EventDone
			}
			fl.Flush()
			if len(page) < deploymentEventPage {
				return done, nil
			}
		}
	}
	done, err := send()
	if err != nil {
		return
	}
	// A finished deployment's status is stored just before its done event,
	// so a finished snapshot without the done event waits a little for it.
	finishedPolls := 0
	if store.DeploymentFinished(d.Status) {
		finishedPolls = 1
	}

	poll := time.NewTicker(DeploymentEventPoll)
	defer poll.Stop()
	beat := time.NewTicker(SSEHeartbeat)
	defer beat.Stop()
	for {
		if done || finishedPolls > 2 {
			if d, err := s.d.Store.GetDeployment(ctx, id); err == nil {
				_ = writeSSE(w, "snapshot", 0, deploymentJSON(d))
				fl.Flush()
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-beat.C:
			if _, err := io.WriteString(w, ": keep-alive\n\n"); err != nil {
				return
			}
			fl.Flush()
		case <-poll.C:
			if done, err = send(); err != nil {
				return
			}
			if finishedPolls > 0 {
				finishedPolls++
				continue
			}
			cur, err := s.d.Store.GetDeployment(ctx, id)
			if errors.Is(err, store.ErrNotFound) {
				return // deleted with its app
			}
			if err == nil && store.DeploymentFinished(cur.Status) {
				finishedPolls = 1
			}
		}
	}
}

func deploymentEventJSON(e store.DeploymentEvent) apiv1.DeploymentEvent {
	out := apiv1.DeploymentEvent{Seq: e.Seq, DeploymentId: e.DeploymentID, Time: e.Time, Type: apiv1.DeploymentEventType(e.Type)}
	if e.Phase != "" {
		p := apiv1.DeploymentStatus(e.Phase)
		out.Phase = &p
	}
	out.Env, out.Message = optional(e.Env), optional(e.Message)
	return out
}
