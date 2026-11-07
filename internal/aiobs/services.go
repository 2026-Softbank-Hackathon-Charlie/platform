package aiobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// AIServiceTopology contains observed app dependencies, never the platform's
// planning agent or endpoint credentials. Counts cover exported spans only.
type AIServiceTopology struct {
	App         string                 `json:"app"`
	GeneratedAt time.Time              `json:"generatedAt"`
	Window      AIServiceWindow        `json:"window"`
	Sources     []AIServiceSource      `json:"sources"`
	Services    []AIServiceObservation `json:"services"`
}
type AIServiceWindow struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}
type AIServiceSource struct {
	Name    string `json:"name"`
	Status  string `json:"status"`
	Message string `json:"message"`
}
type AIServiceObservation struct {
	ID        string              `json:"id"`
	ServiceID string              `json:"serviceId"`
	Name      string              `json:"name"`
	Kind      string              `json:"kind"`
	CallerEnv string              `json:"callerEnv"`
	Location  AIServiceLocation   `json:"location"`
	Model     string              `json:"model,omitempty"`
	Activity  AIServiceActivity   `json:"activity"`
	Evidence  []AIServiceEvidence `json:"evidence"`
}
type AIServiceLocation struct {
	Environment string `json:"environment,omitempty"`
	Provider    string `json:"provider,omitempty"`
	Region      string `json:"region,omitempty"`
	Accelerator string `json:"accelerator"`
}
type AIServiceActivity struct {
	State    string     `json:"state"`
	Requests *int64     `json:"requests,omitempty"`
	LastSeen *time.Time `json:"lastSeen,omitempty"`
}
type AIServiceEvidence struct {
	Source     string    `json:"source"`
	ObservedAt time.Time `json:"observedAt"`
	Detail     string    `json:"detail"`
}

type aiServiceRow struct {
	Identity    string `json:"identity"`
	Caller      string `json:"caller"`
	Host        string `json:"host"`
	Provider    string `json:"provider"`
	Kind        string `json:"kind"`
	Region      string `json:"region"`
	Accelerator string `json:"accelerator"`
	Model       string `json:"model"`
	Requests    string `json:"requests"`
	Last        string `json:"last"`
}

var aiLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:/-]{0,119}$`)

func safeAIName(v string) string {
	if !aiLabel.MatchString(v) || strings.Contains(v, "://") || strings.HasPrefix(v, "sk-") || strings.HasPrefix(v, "AKIA") {
		return ""
	}
	return v
}
func serviceHash(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:12])
}

// AIServices reads app-scoped client spans from the same ClickStack database as
// the configured log table. Ordinary dependency.name=ai spans establish usage
// even when deployment/model metadata was not instrumented. No URL is selected.
func (c *Client) AIServices(ctx context.Context, app string, since, until time.Time, exclude ...string) (AIServiceTopology, error) {
	out := AIServiceTopology{App: app, GeneratedAt: until.UTC(), Window: AIServiceWindow{since.UTC(), until.UTC()}, Sources: []AIServiceSource{}, Services: []AIServiceObservation{}}
	if !appName.MatchString(app) || !since.Before(until) || until.Sub(since) > 24*time.Hour {
		return out, errors.New("invalid AI observation window or app")
	}
	for _, x := range exclude {
		if !appName.MatchString(x) {
			return out, errors.New("invalid app exclusion")
		}
	}
	if !tableName.MatchString(c.Table) {
		return out, errors.New("invalid observation table")
	}
	table := "otel_traces"
	if i := strings.LastIndex(c.Table, "."); i >= 0 {
		table = c.Table[:i+1] + table
	}
	values := appParams(app, until.Add(-24*time.Hour), exclude)
	values.Set("param_recent", strconv.FormatInt(since.UnixMilli(), 10))
	values.Set("param_until", strconv.FormatInt(until.UnixMilli(), 10))
	// Explicit destination attributes describe the model host; resource
	// deployment.environment describes the caller and is never reused as host.
	fields := []string{
		"substring(SpanAttributes['hopcharlie.ai.service_id'],1,120) AS identity",
		"substring(coalesce(nullIf(ResourceAttributes['hopcharlie.environment.name'],''),nullIf(ResourceAttributes['deployment.environment.name'],''),ResourceAttributes['deployment.environment']),1,120) AS caller",
		"substring(SpanAttributes['hopcharlie.ai.environment'],1,120) AS host",
		"substring(coalesce(nullIf(SpanAttributes['gen_ai.provider.name'],''),SpanAttributes['gen_ai.system']),1,120) AS provider",
		"substring(SpanAttributes['hopcharlie.ai.kind'],1,120) AS kind",
		"substring(SpanAttributes['hopcharlie.ai.region'],1,120) AS region",
		"substring(SpanAttributes['hopcharlie.ai.accelerator'],1,120) AS accelerator",
		"substring(coalesce(nullIf(SpanAttributes['gen_ai.response.model'],''),SpanAttributes['gen_ai.request.model']),1,120) AS model",
	}
	query := "SELECT " + strings.Join(fields, ",") + ", toString(uniqExactIf(tuple(TraceId,SpanId),Timestamp >= fromUnixTimestamp64Milli({recent:Int64}))) AS requests, toString(toUnixTimestamp64Milli(max(Timestamp))) AS last FROM " + table +
		" WHERE " + appFilter(exclude) + " AND ServiceName != 'charlie-platform-hopai' AND Timestamp <= fromUnixTimestamp64Milli({until:Int64}) AND SpanKind IN ('Client','SPAN_KIND_CLIENT','client') AND (SpanAttributes['dependency.name']='ai' OR SpanAttributes['gen_ai.provider.name']!='' OR SpanAttributes['gen_ai.system']!='') GROUP BY identity,caller,host,provider,kind,region,accelerator,model ORDER BY max(Timestamp) DESC LIMIT 128 FORMAT JSONEachRow"
	body, err := c.do(ctx, query, values)
	if err != nil {
		return out, err
	}
	routes := map[string]int{}
	err = eachRow(body, func(line []byte) error {
		var row aiServiceRow
		if json.Unmarshal(line, &row) != nil {
			return errors.New("invalid AI service row")
		}
		n, err := strconv.ParseInt(row.Requests, 10, 64)
		if err != nil || n < 0 {
			return errors.New("invalid AI service count")
		}
		ms, err := strconv.ParseInt(row.Last, 10, 64)
		if err != nil {
			return errors.New("invalid AI service timestamp")
		}
		seen := time.UnixMilli(ms).UTC()
		if seen.After(until) || seen.Before(until.Add(-24*time.Hour)) {
			return errors.New("AI evidence outside observation history")
		}
		if n > 0 && seen.Before(since) {
			return errors.New("inconsistent AI activity evidence")
		}
		item := serviceFromRow(row, n, seen)
		if pos, ok := routes[item.ID]; ok {
			previous := &out.Services[pos]
			if n > math.MaxInt64-*previous.Activity.Requests {
				return errors.New("AI service count overflow")
			}
			if n > 0 {
				previous.Activity.State = "active"
			}
			*previous.Activity.Requests += n
			return nil
		}
		if len(out.Services) >= 128 {
			return errors.New("AI route limit exceeded")
		}
		routes[item.ID] = len(out.Services)
		out.Services = append(out.Services, item)
		return nil
	})
	if err != nil {
		return out, err
	}
	status, message := "unobserved", "No app AI dependency spans were observed; this does not prove the app has no AI service."
	if len(out.Services) > 0 {
		status, message = "observed", "App AI client spans from HyperDX/ClickStack; location and accelerator are reported attributes, not independent hardware attestation. Counts cover exported spans, not all requests. At most 128 observed routes from 24 hours are shown."
	}
	out.Sources = append(out.Sources, AIServiceSource{"hyperdx", status, message})
	sort.SliceStable(out.Services, func(i, j int) bool { return out.Services[i].ID < out.Services[j].ID })
	return out, nil
}

func serviceFromRow(r aiServiceRow, n int64, seen time.Time) AIServiceObservation {
	kind, provider := "unknown", ""
	switch strings.ToLower(r.Provider) {
	case "aws.bedrock", "bedrock", "aws_bedrock":
		kind, provider = "managed", "aws"
	case "gcp.vertex_ai", "vertex_ai", "vertexai", "google_vertexai":
		kind, provider = "managed", "gcp"
	case "azure.ai.openai", "azure_openai":
		kind, provider = "managed", "azure"
	case "anthropic", "openai", "cohere", "mistral_ai", "groq":
		kind, provider = "external", strings.ToLower(r.Provider)
	}
	if r.Kind == "self_hosted" {
		kind = "self_hosted"
		provider = ""
	} else if r.Kind == "unknown" {
		kind = "unknown"
		provider = ""
	}
	location := AIServiceLocation{Provider: provider, Accelerator: "unknown"}
	if kind == "self_hosted" {
		location.Environment = safeAIName(r.Host)
		if r.Accelerator == "gpu" || r.Accelerator == "cpu" {
			location.Accelerator = r.Accelerator
		}
	}
	if kind == "managed" {
		location.Region = safeAIName(r.Region)
	}
	identity := safeAIName(r.Identity)
	model := safeAIName(r.Model)
	// Missing physical IDs are grouped conservatively by reported deployment
	// attributes. They do not manufacture distinct GPUs from model names.
	id := serviceHash(identity, kind, provider, location.Environment, location.Region)
	name := identity
	if name == "" {
		name = provider
	}
	if name == "" {
		name = "AI service (location unobserved)"
	}
	state := "unknown"
	if n > 0 {
		state = "active"
	}
	detail := "Observed app AI client span; model execution location is unobserved."
	if kind != "unknown" {
		detail = "Reported provider/hosting attributes; caller deployment is separate from model hosting. GPU is shown only for explicit self-hosted accelerator evidence."
	}
	return AIServiceObservation{ID: serviceHash(id, safeAIName(r.Caller)), ServiceID: id, Name: name, Kind: kind, CallerEnv: safeAIName(r.Caller), Location: location, Model: model, Activity: AIServiceActivity{State: state, Requests: &n, LastSeen: &seen}, Evidence: []AIServiceEvidence{{Source: "hyperdx:otel_traces", ObservedAt: seen, Detail: detail}}}
}
