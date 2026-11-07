package discover

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"golang.org/x/oauth2/google"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
)

// GCPLocator finds a GCE instance by its external IP with
// instances.aggregatedList (compute.instances.list on the project). Inside
// the VM is the next step (blocker gcp_inspection_not_yet).
type GCPLocator struct {
	Project string
	HTTP    *http.Client // authorized; NewGCPLocator uses Application Default Credentials
	BaseURL string       // default https://compute.googleapis.com/compute/v1
}

// NewGCPLocator returns a locator with Application Default Credentials,
// as internal/storage's GCS driver.
func NewGCPLocator(ctx context.Context, project string) (*GCPLocator, error) {
	c, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/compute.readonly")
	if err != nil {
		return nil, fmt.Errorf("gcp: Application Default Credentials are unavailable")
	}
	c.Timeout = 30 * time.Second
	return &GCPLocator{Project: project, HTTP: c}, nil
}

// FindInstance implements Locator.
func (g *GCPLocator) FindInstance(ctx context.Context, ip string) (*facts.Instance, error) {
	base := g.BaseURL
	if base == "" {
		base = "https://compute.googleapis.com/compute/v1"
	}
	token := ""
	for page := 0; page < 50; page++ {
		q := url.Values{"maxResults": {"500"}, "fields": {"items/*/instances(name,zone,machineType,labels,networkInterfaces/accessConfigs/natIP),nextPageToken"}}
		if token != "" {
			q.Set("pageToken", token)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/projects/"+url.PathEscape(g.Project)+"/aggregated/instances?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		res, err := g.HTTP.Do(req)
		if err != nil {
			return nil, fmt.Errorf("gcp: list instances: request failed")
		}
		var body struct {
			Items map[string]struct {
				Instances []struct {
					Name              string            `json:"name"`
					Zone              string            `json:"zone"`
					MachineType       string            `json:"machineType"`
					Labels            map[string]string `json:"labels"`
					NetworkInterfaces []struct {
						AccessConfigs []struct {
							NatIP string `json:"natIP"`
						} `json:"accessConfigs"`
					} `json:"networkInterfaces"`
				} `json:"instances"`
			} `json:"items"`
			NextPageToken string `json:"nextPageToken"`
		}
		err = nil
		if res.StatusCode != http.StatusOK {
			err = fmt.Errorf("gcp: list instances: HTTP %d", res.StatusCode)
		} else if derr := json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(&body); derr != nil {
			err = fmt.Errorf("gcp: list instances: invalid answer")
		}
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, scope := range body.Items {
			for _, in := range scope.Instances {
				for _, ni := range in.NetworkInterfaces {
					for _, ac := range ni.AccessConfigs {
						if ac.NatIP != ip {
							continue
						}
						zone := path.Base(in.Zone)
						region := zone
						if i := strings.LastIndex(zone, "-"); i > 0 {
							region = zone[:i]
						}
						tags := map[string]string{}
						for k, v := range in.Labels {
							tags[k] = v
						}
						return &facts.Instance{Cloud: facts.CloudGCP, ID: in.Name, Region: region, Zone: zone,
							Project: g.Project, Type: path.Base(in.MachineType), PublicIP: ip, Tags: tags}, nil
					}
				}
			}
		}
		if body.NextPageToken == "" {
			break
		}
		token = body.NextPageToken
	}
	return nil, nil
}
