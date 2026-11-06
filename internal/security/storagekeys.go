package security

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sync"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/smithy-go"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/storage"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// StorageRows is the part of the store the storage key collectors read: the
// bucket and keys the platform made for every instance (ADR 0016).
type StorageRows interface {
	ListInstanceStorage(ctx context.Context, app, instance string) ([]store.InstanceStorage, error)
}

// SourceStorageKeys is the name of the storage key sources.
const SourceStorageKeys = "storage-keys"

// CheckUnrecordedKey is the finding for a key of an instance's user that the
// platform has no record of.
const CheckUnrecordedKey = "storage.unrecorded-key"

// maxKeyCalls caps the provider calls the storage key collectors of one
// server make at once.
const maxKeyCalls = 8

// errCollectorFailed stands for a panic while reading keys.
var errCollectorFailed = errors.New("the collector failed")

// StorageKeyCollector lists the bucket keys of app instances (ADR 0027): the
// platform's record of each instance's key in use and old key, with when the
// provider made it, whether it is active and what it may do. It reads the
// provider and never changes it. A key of an instance's user that the
// platform has no record of is also a finding.
//
// There is one collector per environment with a storage driver, so a
// provider that does not answer delays only its own environment, and one
// more for the records of environments without one.
type StorageKeyCollector struct {
	src        *Source         // nil: the records of every environment not in configured
	configured map[string]bool // environments with a collector of their own
	rows       StorageRows
	driver     func(env string) (storage.Driver, error)
	calls      chan struct{} // provider calls in progress, shared by a server's collectors
	log        *slog.Logger
}

// NewStorageKeyCollectors returns one collector per environment with a
// storage block and one for the records of other environments, reading rows
// and each environment's driver. They make at most maxKeyCalls provider
// calls at once together.
func NewStorageKeyCollectors(environments []envs.EnvConfig, rows StorageRows, driver func(env string) (storage.Driver, error), log *slog.Logger) []Collector {
	if log == nil {
		log = slog.Default()
	}
	calls := make(chan struct{}, maxKeyCalls)
	configured := map[string]bool{}
	var out []Collector
	for _, e := range environments {
		if e.Storage == nil {
			continue
		}
		configured[e.Name] = true
		src := Source{Name: SourceStorageKeys, Provider: apiv1.SecurityProvider(e.Provider), Environment: e.Name}
		out = append(out, &StorageKeyCollector{src: &src, rows: rows, driver: driver, calls: calls, log: log})
	}
	return append(out, &StorageKeyCollector{configured: configured, rows: rows, driver: driver, calls: calls, log: log})
}

// Sources is the collector's environment; none for the collector of the
// other environments, which reports only when there are such records.
func (c *StorageKeyCollector) Sources() []Source {
	if c.src == nil {
		return nil
	}
	return []Source{*c.src}
}

// rowResult is what one record gave.
type rowResult struct {
	keys     []apiv1.StorageKey
	findings []apiv1.SecurityFinding
	fail     string // why the provider was not read; "" when it was
	eval     *Evaluation
}

// Collect reads the records of the collector's environments and, for each,
// the keys the environment's driver finds for the instance.
func (c *StorageKeyCollector) Collect(ctx context.Context) []Report {
	all, err := c.rows.ListInstanceStorage(ctx, "", "")
	if err != nil {
		if c.src != nil {
			c.log.Error("storage keys: read the platform's records", "env", c.src.Environment, "err", err)
		}
		return failed(c.Sources(), apiv1.SecuritySourceStatusError, "could not read the platform's records of bucket keys")
	}
	var rows []store.InstanceStorage
	for _, r := range all {
		if (c.src != nil && r.Env == c.src.Environment) || (c.src == nil && !c.configured[r.Env]) {
			rows = append(rows, r)
		}
	}
	results := make([]rowResult, len(rows))
	var wg sync.WaitGroup
	for i, row := range rows {
		wg.Go(func() { results[i] = c.readRow(ctx, row) })
	}
	wg.Wait()

	if c.src == nil {
		// A source per environment, in error: the platform cannot read
		// or delete these keys (ADR 0016).
		var reps []Report
		at := map[string]int{}
		for i, row := range rows {
			j, ok := at[row.Env]
			if !ok {
				j = len(reps)
				at[row.Env] = j
				reps = append(reps, Report{
					Source: Source{Name: SourceStorageKeys, Provider: apiv1.SecurityProviderPlatform, Environment: row.Env},
					Status: apiv1.SecuritySourceStatusError,
					Message: fmt.Sprintf("environment %s has no storage driver in this server's configuration, but the platform has bucket keys there; an operator deletes them and their records (ADR 0016)",
						row.Env),
				})
			}
			reps[j].StorageKeys = append(reps[j].StorageKeys, results[i].keys...)
			reps[j].Findings = append(reps[j].Findings, results[i].findings...)
		}
		return reps
	}

	rep := Report{Source: *c.src, Status: apiv1.SecuritySourceStatusOk}
	var fails []string
	for _, r := range results {
		rep.StorageKeys = append(rep.StorageKeys, r.keys...)
		rep.Findings = append(rep.Findings, r.findings...)
		if r.eval != nil {
			rep.Evaluations = append(rep.Evaluations, *r.eval)
		}
		if r.fail != "" {
			fails = append(fails, r.fail)
		}
	}
	rep.Message = fmt.Sprintf("%s of %s", many(len(rep.StorageKeys), "key", "keys"), many(len(rows), "instance", "instances"))
	if len(fails) > 0 {
		rep.Status = apiv1.SecuritySourceStatusError
		rep.Message += "; not read: " + fails[0]
		if n := len(fails); n > 1 {
			rep.Message += fmt.Sprintf(" (and %d more)", n-1)
		}
	}
	return []Report{rep}
}

// readRow lists one record's keys. A panic leaves the recorded keys unread.
func (c *StorageKeyCollector) readRow(ctx context.Context, row store.InstanceStorage) (res rowResult) {
	key := func(id string, use apiv1.StorageKeyUse) apiv1.StorageKey {
		return apiv1.StorageKey{App: row.App, Instance: row.Instance, Environment: row.Env, Driver: row.Driver, Bucket: row.Bucket,
			KeyId: storage.MaskKey(id), Use: use, State: apiv1.StorageKeyStateUnread}
	}
	var recorded []apiv1.StorageKey
	ids := map[string]int{}
	for _, k := range []struct {
		id  string
		use apiv1.StorageKeyUse
	}{{row.KeyID, apiv1.StorageKeyUseCurrent}, {row.OldKeyID, apiv1.StorageKeyUseOld}} {
		if k.id != "" {
			ids[k.id] = len(recorded)
			recorded = append(recorded, key(k.id, k.use))
		}
	}
	unread := func(why string) rowResult {
		out := slices.Clone(recorded)
		for i := range out {
			out[i].State, out[i].CreatedAt, out[i].Message = apiv1.StorageKeyStateUnread, nil, &why
		}
		return rowResult{keys: out, fail: why, eval: &Evaluation{Check: CheckUnrecordedKey, Asset: "bucket/" + row.Bucket, Result: apiv1.SecurityControlResultUnknown, Reason: why}}
	}
	defer func() {
		if v := recover(); v != nil {
			c.log.Error("storage keys: reading a record panicked", "instance", row.Instance, "env", row.Env, "panic", v)
			res = unread("the collector failed; see the server log")
		}
	}()

	if c.driver == nil {
		return unread("this server has no storage drivers")
	}
	drv, err := c.driver(row.Env)
	if err != nil {
		return unread("environment " + row.Env + " has no usable storage driver")
	}
	if drv.Name() != row.Driver {
		return unread(fmt.Sprintf("the %s driver made this key, but %s now has the %s driver", row.Driver, row.Env, drv.Name()))
	}
	kr, ok := drv.(storage.KeyReader)
	if !ok {
		return unread("the " + drv.Name() + " driver cannot read keys back")
	}
	scope := kr.Scope()
	s := &apiv1.StorageKeyScope{Actions: scope.Actions}
	if scope.Boundary != "" {
		s.Boundary = &scope.Boundary
	}
	for i := range recorded {
		recorded[i].Scope = s
	}
	found, err := c.keys(ctx, kr, storage.Instance{App: row.App, Name: row.Instance})
	if err != nil {
		return unread(c.keyReadError(row, err))
	}

	var extra []apiv1.StorageKey
	seen := map[int]bool{}
	for _, f := range found {
		i, known := ids[f.ID]
		k := key(f.ID, apiv1.StorageKeyUseUnrecorded)
		if known {
			k, seen[i] = recorded[i], true
		} else {
			msg := "the platform has no record of this key; delete it if nobody uses it (the next new key deletes it too)"
			k.Scope, k.Message = s, &msg
			res.findings = append(res.findings, unrecordedFinding(row, f))
		}
		k.State = apiv1.StorageKeyStateOk
		if !f.Active {
			k.State = apiv1.StorageKeyStateInactive
			if k.Use == apiv1.StorageKeyUseCurrent {
				msg := "the key the pods are given is deactivated, so the app cannot use its bucket; the platform still counts it as present and does not replace it. Activate it, or delete it so the next deployment makes a new one"
				k.Message = &msg
			}
		}
		if !f.CreatedAt.IsZero() {
			t := f.CreatedAt
			k.CreatedAt = &t
		}
		if known {
			recorded[i] = k
		} else {
			extra = append(extra, k)
		}
	}
	for i := range recorded {
		if !seen[i] {
			recorded[i].State = apiv1.StorageKeyStateMissing
			msg := "the provider has no such key; the instance's next deployment makes a new one (the record of a deleted instance goes when its cleanup finishes)"
			if recorded[i].Use == apiv1.StorageKeyUseOld {
				msg = "the provider has no such key any more; the record is cleared after a healthy deployment restarts the pods"
			}
			recorded[i].Message = &msg
		}
	}
	res.keys = slices.Concat(recorded, extra)
	ev := Evaluation{Check: CheckUnrecordedKey, Asset: "bucket/" + row.Bucket, Result: apiv1.SecurityControlResultPass,
		Reason: fmt.Sprintf("%s of instance %s read; every one is recorded", many(len(found), "key", "keys"), row.Instance)}
	if len(extra) > 0 {
		ev.Result, ev.Reason = apiv1.SecurityControlResultFail, fmt.Sprintf("%s of instance %s not recorded by the platform", many(len(extra), "key is", "keys are"), row.Instance)
	}
	res.eval = &ev
	return res
}

// keys calls kr.Keys holding one of the shared call slots, and returns when
// ctx ends even if the call goes on: a call that does not follow ctx (such as
// a GCS token refresh, which uses the context the driver was made with)
// keeps its slot until it returns, and the others wait for a slot only as
// long as ctx lasts.
func (c *StorageKeyCollector) keys(ctx context.Context, kr storage.KeyReader, in storage.Instance) ([]storage.KeyInfo, error) {
	select {
	case c.calls <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	type result struct {
		keys []storage.KeyInfo
		err  error
	}
	done := make(chan result, 1)
	go func() {
		defer func() { <-c.calls }()
		defer func() {
			if v := recover(); v != nil {
				c.log.Error("storage keys: listing keys panicked", "instance", in.Name, "panic", v)
				done <- result{err: errCollectorFailed}
			}
		}()
		k, err := kr.Keys(ctx, in)
		done <- result{k, err}
	}()
	select {
	case r := <-done:
		return r.keys, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// keyReadError says why a driver could not read the keys without the
// provider's or the SDK's own words, which can carry endpoints, ARNs and
// file paths: an AWS error code, why AWS credentials could not be had, a
// timeout, a network error or an HTTP status. Anything else goes to the
// server log.
func (c *StorageKeyCollector) keyReadError(row store.InstanceStorage, err error) string {
	prefix := "the " + row.Driver + " driver could not list the keys: "
	var se *v4.SigningError
	var ae smithy.APIError
	var ne net.Error
	switch {
	case errors.Is(err, errCollectorFailed):
		return "the collector failed; see the server log"
	case errors.As(err, &se):
		return prefix + credentialsError(se.Err).Error()
	case errors.As(err, &ae):
		return prefix + ae.ErrorCode()
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || (errors.As(err, &ne) && ne.Timeout()):
		return prefix + "timed out"
	case errors.As(err, &ne):
		return prefix + "network error"
	case storage.HTTPStatus(err) != 0:
		return prefix + fmt.Sprintf("HTTP %d", storage.HTTPStatus(err))
	}
	c.log.Warn("storage keys: list keys", "instance", row.Instance, "env", row.Env, "err", err)
	return prefix + "see the server log"
}

// unrecordedFinding is the finding for a key the platform has no record of.
// An active one can use the bucket; an inactive one only matters once
// someone activates it.
func unrecordedFinding(row store.InstanceStorage, f storage.KeyInfo) apiv1.SecurityFinding {
	sev := apiv1.SecuritySeverityMedium
	if !f.Active {
		sev = apiv1.SecuritySeverityLow
	}
	return apiv1.SecurityFinding{
		Id: f.ID, Check: CheckUnrecordedKey, Category: apiv1.SecurityCategoryIdentity, Severity: sev,
		Title:    "Bucket key the platform has no record of",
		Resource: "bucket/" + row.Bucket + "/key/" + storage.MaskKey(f.ID),
		Detail: fmt.Sprintf("The %s user of instance %s (app %s) has a key the platform did not make or no longer records. While active it can use the bucket %s.",
			row.Driver, row.Instance, row.App, row.Bucket),
		Recommendation: "Delete the key if nobody uses it. The platform also deletes it before it makes the instance's next new key.",
	}
}
