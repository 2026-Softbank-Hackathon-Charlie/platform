package security

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"

	"github.com/aws/smithy-go"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/storage"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// StorageRows is the part of the store the storage key collector reads: the
// bucket and keys the platform made for every instance (ADR 0016).
type StorageRows interface {
	ListInstanceStorage(ctx context.Context, app, instance string) ([]store.InstanceStorage, error)
}

// SourceStorageKeys is the name of the storage key sources.
const SourceStorageKeys = "storage-keys"

// StorageKeyCollector lists the bucket keys of app instances (ADR 0027):
// the platform's record of each instance's key in use and old key, with
// when the provider made it and what it may do. Every environment with a
// storage driver is one source. It reads the provider, never changes it,
// and reports no findings.
type StorageKeyCollector struct {
	srcs   []Source
	rows   StorageRows
	driver func(env string) (storage.Driver, error)
}

// NewStorageKeyCollector reports on the environments that have a storage
// block, reading rows and each environment's driver.
func NewStorageKeyCollector(environments []envs.EnvConfig, rows StorageRows, driver func(env string) (storage.Driver, error)) *StorageKeyCollector {
	c := &StorageKeyCollector{rows: rows, driver: driver}
	for _, e := range environments {
		if e.Storage != nil {
			c.srcs = append(c.srcs, Source{Name: SourceStorageKeys, Provider: apiv1.SecurityProvider(e.Provider), Environment: e.Name})
		}
	}
	return c
}

// Sources are the environments with a storage driver.
func (c *StorageKeyCollector) Sources() []Source { return c.srcs }

// Collect reads every stored row and the keys its environment's driver
// finds for the instance. A row of an environment that is no longer
// configured gets a source of its own, in error.
func (c *StorageKeyCollector) Collect(ctx context.Context) []Report {
	rows, err := c.rows.ListInstanceStorage(ctx, "", "")
	if err != nil {
		return failed(c.srcs, apiv1.SecuritySourceStatusError, "could not read the platform's records of bucket keys")
	}
	keys := make([][]apiv1.StorageKey, len(rows))
	fails := make([]string, len(rows))
	var wg sync.WaitGroup
	for i, row := range rows {
		wg.Go(func() {
			// Service.run recovers only the collector's own goroutine.
			defer func() {
				if v := recover(); v != nil {
					keys[i], fails[i] = nil, "the collector failed; see the server log"
					slog.Error("storage key collector panicked", "instance", row.Instance, "env", row.Env, "panic", v)
				}
			}()
			keys[i], fails[i] = c.read(ctx, row)
		})
	}
	wg.Wait()

	var reps []Report
	at := map[string]int{}
	for _, src := range c.srcs {
		at[src.Environment] = len(reps)
		reps = append(reps, Report{Source: src, Status: apiv1.SecuritySourceStatusOk})
	}
	type tally struct {
		instances int
		fails     []string
	}
	tallies := make([]tally, len(reps))
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
			tallies = append(tallies, tally{})
		}
		reps[j].StorageKeys = append(reps[j].StorageKeys, keys[i]...)
		tallies[j].instances++
		if fails[i] != "" {
			tallies[j].fails = append(tallies[j].fails, fails[i])
		}
	}
	for j := range c.srcs {
		t := tallies[j]
		msg := fmt.Sprintf("%s of %s", many(len(reps[j].StorageKeys), "key", "keys"), many(t.instances, "instance", "instances"))
		if len(t.fails) > 0 {
			reps[j].Status = apiv1.SecuritySourceStatusError
			msg += "; not read: " + t.fails[0]
			if n := len(t.fails); n > 1 {
				msg += fmt.Sprintf(" (and %d more)", n-1)
			}
		}
		reps[j].Message = msg
	}
	return reps
}

// read lists one row's keys. fail says why the provider was not read; ""
// when it was.
func (c *StorageKeyCollector) read(ctx context.Context, row store.InstanceStorage) (keys []apiv1.StorageKey, fail string) {
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
	unread := func(why string) ([]apiv1.StorageKey, string) {
		for i := range recorded {
			recorded[i].Message = &why
		}
		return recorded, why
	}

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
	found, err := kr.Keys(ctx, storage.Instance{App: row.App, Name: row.Instance})
	if err != nil {
		return unread(keyReadError(row.Driver, err))
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
		}
		k.State = apiv1.StorageKeyStateOk
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
			msg := "the provider has no such key; the next deployment makes a new one"
			if recorded[i].Use == apiv1.StorageKeyUseOld {
				msg = "the provider has no such key any more; the record is cleared after a healthy deployment restarts the pods"
			}
			recorded[i].Message = &msg
		}
	}
	return slices.Concat(recorded, extra), ""
}

// keyReadError says why a driver could not read the keys, without the
// provider's message: an AWS error code, a timeout, or the driver's own
// words (the gcs and minio drivers leave provider bodies out).
func keyReadError(driver string, err error) string {
	var ae smithy.APIError
	switch {
	case errors.As(err, &ae):
		return fmt.Sprintf("the %s driver could not list the keys: %s", driver, ae.ErrorCode())
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return fmt.Sprintf("the %s driver could not list the keys: timed out", driver)
	}
	return shorten(err.Error(), 160)
}
