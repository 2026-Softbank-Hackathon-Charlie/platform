package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"strconv"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// FileReader reads a file of a repository at a commit. ReadFile returns an
// error matching fs.ErrNotExist when the file does not exist.
type FileReader interface {
	ReadFile(ctx context.Context, repo, ref, path string) ([]byte, error)
}

// CheckSpec reads platform.yaml at commit of repo and checks it the way
// registration and every deployment do: the rules of pkg/appspec, and that
// every service's Dockerfile exists at that commit. The result's Commit is
// commit and its Spec is set when the file decodes; the returned spec is set
// when the file is valid. An error means GitHub could not be read; a missing
// file is a validation error, not an error.
func CheckSpec(ctx context.Context, gh FileReader, repo, commit string) (apiv1.AppSpecValidation, *appspec.Spec, error) {
	v := apiv1.AppSpecValidation{Commit: &commit, Errors: []apiv1.AppSpecFieldError{}}
	data, err := gh.ReadFile(ctx, repo, commit, appspec.FileName)
	if errors.Is(err, fs.ErrNotExist) {
		v.Errors = append(v.Errors, apiv1.AppSpecFieldError{Message: appspec.FileName + " is not at the root of " + repo + " at " + commit})
		return v, nil, nil
	}
	if err != nil {
		return v, nil, err
	}
	spec, errs := appspec.Check(data)
	for _, fe := range errs {
		v.Errors = append(v.Errors, apiv1.AppSpecFieldError{Path: fe.Path, Message: fe.Message})
	}
	if spec != nil {
		var out apiv1.AppSpec
		if json.Unmarshal(specJSON(*spec), &out) == nil {
			v.Spec = &out
		}
	}
	if spec == nil || len(errs) > 0 {
		return v, nil, nil
	}
	missing := map[string]bool{}
	for i, sv := range spec.Services {
		p := sv.DockerfilePath()
		gone, known := missing[p]
		if !known {
			gone = p == "." // path . with dockerfile . names the root, not a file
			if !gone {
				_, err := gh.ReadFile(ctx, repo, commit, p)
				switch {
				case errors.Is(err, fs.ErrNotExist):
					gone = true
				case err != nil:
					return v, nil, err
				}
			}
			missing[p] = gone
		}
		if gone {
			v.Errors = append(v.Errors, apiv1.AppSpecFieldError{
				Path:    "services[" + strconv.Itoa(i) + "].dockerfile",
				Message: p + " does not exist at " + commit,
			})
		}
	}
	if len(v.Errors) > 0 {
		return v, nil, nil
	}
	v.Valid = true
	return v, spec, nil
}
