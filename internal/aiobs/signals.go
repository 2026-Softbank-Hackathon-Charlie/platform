package aiobs

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/preflight/parity"
)

// SignalCounts reads aggregate receiving evidence through the existing readonly
// ClickHouse client. Every query binds the exact app service allowlist,
// namespace, environment and collector cluster. A global sample cannot prove a
// different environment. No raw logs, spans, prompts or token values are read.
func (c *Client) SignalCounts(ctx context.Context, scope parity.SignalScope, since, until time.Time) ([]parity.SignalCount, error) {
	bad := errors.New("invalid observation scope or window")
	if c == nil || c.HTTP == nil || !parity.ValidTarget(scope.Target) || !appName.MatchString(scope.Cluster) || !appName.MatchString(scope.DeploymentEnvironment) || !since.Before(until) || until.Sub(since) > 10*time.Minute || len(scope.Services) == 0 || len(scope.Services) > 16 || !tableName.MatchString(c.Table) {
		return nil, bad
	}
	for _, service := range scope.Services {
		if !appName.MatchString(service) {
			return nil, bad
		}
	}
	owned := *c
	client := *c.HTTP
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	owned.HTTP = &client
	traces := "otel_traces"
	if i := strings.LastIndex(c.Table, "."); i >= 0 {
		traces = c.Table[:i+1] + traces
	}
	values := url.Values{"param_services": {strings.Join(scope.Services, ",")}, "param_namespace": {scope.Target.Namespace}, "param_environment": {scope.DeploymentEnvironment}, "param_cluster": {scope.Cluster}, "param_since": {strconv.FormatInt(since.UnixMilli(), 10)}, "param_until": {strconv.FormatInt(until.UnixMilli(), 10)}}
	out := make([]parity.SignalCount, 0, 2)
	for _, signal := range []struct{ name, table string }{{"logs", c.Table}, {"traces", traces}} {
		query := "SELECT toString(count()) AS samples, toString(toUnixTimestamp64Milli(max(Timestamp))) AS lastSeen FROM " + signal.table +
			" WHERE Timestamp >= fromUnixTimestamp64Milli({since:Int64}) AND Timestamp <= fromUnixTimestamp64Milli({until:Int64})" +
			" AND has(splitByChar(',',{services:String}),ServiceName) AND ResourceAttributes['k8s.namespace.name']={namespace:String}" +
			" AND ResourceAttributes['deployment.environment.name']={environment:String} AND ResourceAttributes['k8s.cluster.name']={cluster:String} FORMAT JSONEachRow"
		body, err := owned.do(ctx, query, values)
		record := parity.SignalCount{Signal: signal.name, Code: "signal_query_failed"}
		if err == nil {
			var row struct {
				Samples string `json:"samples"`
				Last    string `json:"lastSeen"`
			}
			if json.Unmarshal(body, &row) == nil {
				count, e1 := strconv.ParseInt(row.Samples, 10, 64)
				stamp, e2 := strconv.ParseInt(row.Last, 10, 64)
				if e1 == nil && e2 == nil && count >= 0 {
					record.Samples, record.Code = count, "signal_counted"
					if count > 0 {
						record.LastSeen = time.UnixMilli(stamp).UTC()
					}
				}
			}
		}
		out = append(out, record)
	}
	return out, nil
}
