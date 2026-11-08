package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sort"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/recommend"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

func (s *Server) recommendationQuery(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.RawQuery != "" {
		s.fail(w, r, 400, "invalid_request", "recommendation inputs are server-owned")
		return false
	}
	w.Header().Set("Cache-Control", "no-store")
	return true
}
func (s *Server) getMigrationRecommendations(w http.ResponseWriter, r *http.Request) {
	if !s.recommendationQuery(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.recommendations.Trigger(r.Context(), actor(r)))
}
func (s *Server) getMigrationRecommendationStatus(w http.ResponseWriter, r *http.Request) {
	if !s.recommendationQuery(w, r) {
		return
	}
	writeJSON(w, http.StatusOK, s.recommendations.Status())
}
func (s *Server) completeMigrationRecommendation(w http.ResponseWriter, r *http.Request) {
	if !s.recommendationQuery(w, r) {
		return
	}
	var result recommend.Result
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if !s.decode(w, r, &result, false) {
		return
	}
	if err := s.recommendations.Complete(r.PathValue("id"), result); err != nil {
		if errors.Is(err, recommend.ErrConflict) {
			s.fail(w, r, 409, "recommendation_conflict", "result is not bound to the active recommendation snapshot")
			return
		}
		s.internal(w, r, err)
		return
	}
	// Never put the callback capability, input snapshot or model content in audit.
	noteAudit(r, "recommendation", r.PathValue("id"))
	writeJSON(w, http.StatusOK, s.recommendations.Status())
}

// Only public app identity, registered environment names and route state are
// copied. App specs, backend URLs, DSNs and secret names never enter the AI.
func (s *Server) loadRecommendationCandidates(ctx context.Context) ([]recommend.Candidate, error) {
	if s.d.Envs == nil || s.d.Apps == nil {
		return nil, errors.New("recommendation inventory unavailable")
	}
	now := s.now()
	environments := s.d.Envs.List(ctx)
	envs := make([]recommend.Environment, 0, len(environments))
	countries := map[string]string{}
	if policy := s.cfg.RecommendationResidency; policy != nil {
		for _, e := range policy.Environments {
			countries[e.ID] = e.Country
		}
	}
	backends := map[string]string{}
	duplicate := map[string]bool{}
	for _, e := range environments {
		if backends[e.GatewayBackend] != "" {
			duplicate[e.GatewayBackend] = true
		}
		backends[e.GatewayBackend] = e.Name
		region := ""
		if e.Region != nil {
			region = *e.Region
		}
		ready := e.Status.Reachable && e.Status.Error == nil && e.Status.Nodes != nil && e.Status.Nodes.Ready > 0
		envs = append(envs, recommend.Environment{Name: e.Name, Provider: string(e.Provider), Region: region, Country: countries[e.Name], Ready: ready, ObservedAt: e.Status.CheckedAt})
	}
	names, revisions := map[string]bool{}, map[string]string{}
	for _, a := range s.d.Apps.List() {
		names[a.Name] = true
	}
	if s.d.Store != nil {
		registered, err := s.d.Store.ListApps(ctx)
		if err != nil {
			return nil, errors.New("recommendation inventory unavailable")
		}
		for _, a := range registered {
			names[a.Name] = true
			revisions[a.Name] = a.SpecCommit
		}
	}
	current := map[string]string{}
	moving := map[string]bool{}
	if s.d.Gateway != nil {
		if routes, err := s.d.Gateway.Apps(ctx); err == nil {
			for _, route := range routes.Apps {
				if !duplicate[route.WriteBackend] {
					current[route.Name] = backends[route.WriteBackend]
				}
				moving[route.Name] = route.WritesHeld
			}
		}
	}
	sorted := make([]string, 0, len(names))
	for name := range names {
		sorted = append(sorted, name)
	}
	sort.Strings(sorted)
	applications := []recommend.Application{}
	for _, name := range sorted {
		if len(applications) >= recommend.MaxApps {
			break
		}
		a, known, err := s.d.Apps.Lookup(ctx, name)
		if err != nil {
			return nil, errors.New("recommendation inventory unavailable")
		}
		app := recommend.Application{Name: name, Revision: revisions[name], Moving: moving[name]}
		if known {
			for _, e := range a.Environments {
				app.Environments = append(app.Environments, e.Env)
			}
			app.Persistent = a.Resources.Postgres || a.Resources.S3
		}
		if env := current[name]; env != "" {
			app.Current = &env
			if known {
				app.Countries = s.recommendationCountries(a, env)
			}
		}
		applications = append(applications, app)
	}
	sources := map[string]recommend.Source{}
	for _, e := range envs {
		if e.Provider == string(apiv1.ProviderAws) {
			_, source := s.recommendationPrices.Reference(ctx, e.Region, now)
			sources[e.Name] = source
		}
	}
	candidates := recommend.Build(applications, envs, s.cfg.RecommendationCosts, now)
	for i := range candidates {
		for _, name := range []*string{candidates[i].Source, candidates[i].Target} {
			if name != nil && sources[*name].URL != "" {
				candidates[i].Sources = append(candidates[i].Sources, sources[*name])
			}
		}
	}
	return candidates, nil
}

// This reads only operator-owned classifications already loaded for the real
// residency gate. It does not send table names, rows or object keys to the AI.
// A missing/partial catalog never attests inventory completeness.
func (s *Server) recommendationCountries(app apiv1.App, source string) []string {
	policy := s.cfg.RecommendationResidency
	if policy == nil {
		return nil
	}
	var labels []string
	for _, e := range app.Environments {
		if e.Env != source {
			continue
		}
		if app.Resources.Postgres {
			for _, c := range policy.Catalog {
				if c.Scope.Environment == source && c.Scope.Namespace == app.Namespace && c.Scope.Cluster == e.CnpgCluster {
					for _, d := range c.Datasets {
						labels = append(labels, d.Label)
					}
				}
			}
		}
		if app.Resources.S3 && e.Bucket != nil {
			for _, b := range policy.Buckets {
				if b.Bucket == *e.Bucket && b.Environment == source {
					labels = append(labels, b.Label, b.BinaryLabel)
					for _, p := range b.Prefixes {
						labels = append(labels, p.Label)
					}
					for _, r := range b.ReviewedFiles {
						labels = append(labels, r.Label)
					}
				}
			}
		}
	}
	var allowed []string
	for _, label := range labels {
		var next []string
		switch label {
		case "kr-only":
			next = []string{"KR"}
		case "apac":
			next = slices.Clone(policy.APACCountries)
		default:
			continue
		}
		if allowed == nil {
			allowed = next
			continue
		}
		allowed = slices.DeleteFunc(allowed, func(country string) bool { return !slices.Contains(next, country) })
	}
	return allowed
}
