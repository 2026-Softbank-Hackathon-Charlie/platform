package deploy

import (
	"context"
	"regexp"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// Builder builds and pushes one image per service of an app commit.
type Builder interface {
	// Build returns one image per service, pinned by digest
	// (<registry>/hopcharlie/apps/<app>/<service>@sha256:<digest>). Each
	// image is also tagged with the commit. It calls hooks.Started once the
	// build has an id, and stops with that error if Started fails.
	Build(ctx context.Context, req BuildRequest, hooks BuildHooks) (BuildResult, error)
}

// BuildRequest is one build.
type BuildRequest struct {
	DeploymentID string
	App          string
	Repo         string // owner/name on GitHub
	Commit       string // full sha
	Services     []appspec.Service
	// Token is a short-lived GitHub installation token to clone Repo with.
	// Builders must not print it.
	Token string
}

// BuildHooks report a build's progress.
type BuildHooks struct {
	Started func(buildID string) error
	// Log receives one line of build output at a time.
	Log func(line string)
}

// BuildResult is a finished build.
type BuildResult struct {
	BuildID string
	Images  []store.DeploymentImage
}

// ImageRepository is where a service's images go in a registry.
func ImageRepository(registry, app, service string) string {
	return registry + "/hopcharlie/apps/" + app + "/" + service
}

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func (h BuildHooks) started(id string) error {
	if h.Started == nil {
		return nil
	}
	return h.Started(id)
}

func (h BuildHooks) log(line string) {
	if h.Log != nil {
		h.Log(line)
	}
}
