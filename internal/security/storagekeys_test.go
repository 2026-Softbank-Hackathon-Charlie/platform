package security

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/aws/smithy-go"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/envs"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/storage"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

type fakeRows struct {
	rows []store.InstanceStorage
	err  error
}

func (f fakeRows) ListInstanceStorage(context.Context, string, string) ([]store.InstanceStorage, error) {
	return f.rows, f.err
}

// keyDriver is a storage driver that only reads keys back.
type keyDriver struct {
	storage.Driver
	name  string
	keys  map[string][]storage.KeyInfo // by instance
	err   error
	scope storage.Scope
}

func (d keyDriver) Name() string { return d.name }

func (d keyDriver) Keys(_ context.Context, in storage.Instance) ([]storage.KeyInfo, error) {
	return d.keys[in.Name], d.err
}

func (d keyDriver) Scope() storage.Scope { return d.scope }

// panicDriver panics when it reads keys.
type panicDriver struct{ keyDriver }

func (panicDriver) Keys(context.Context, storage.Instance) ([]storage.KeyInfo, error) { panic("boom") }

// stuckDriver does not answer until release is closed, whatever its context
// says, like a GCS token refresh.
type stuckDriver struct {
	keyDriver
	release chan struct{}
}

func (d stuckDriver) Keys(context.Context, storage.Instance) ([]storage.KeyInfo, error) {
	<-d.release
	return nil, nil
}

// plainDriver cannot read keys back.
type plainDriver struct{ storage.Driver }

func (plainDriver) Name() string { return storage.DriverMinIO }

var keyMade = time.Date(2026, 11, 4, 8, 21, 52, 0, time.UTC)

func storageRow(instance, env, driver, key, old string) store.InstanceStorage {
	return store.InstanceStorage{Instance: instance, Env: env, App: strings.Split(instance, "--")[0], Driver: driver,
		Bucket: "hopcharlie-" + env + "-" + instance, KeyID: key, OldKeyID: old}
}

// collectAll runs every collector as the Service does, in order.
func collectAll(ctx context.Context, cs []Collector) []Report {
	var out []Report
	for _, c := range cs {
		out = append(out, c.Collect(ctx)...)
	}
	return out
}

func TestStorageKeys(t *testing.T) {
	environments := []envs.EnvConfig{
		{Name: "gke", Provider: "gcp", Storage: &storage.Config{Driver: storage.DriverGCS}},
		{Name: "eks", Provider: "aws", Storage: &storage.Config{Driver: storage.DriverAWS}},
		{Name: "onprem", Provider: "onprem"}, // no storage: no source
	}
	rows := fakeRows{rows: []store.InstanceStorage{
		storageRow("moimroom", "eks", storage.DriverAWS, "AKIACURRENT1", "AKIAOLDKEY02"),
		storageRow("moimroom", "gke", storage.DriverGCS, "GOOGHMACKEY3", ""),
		storageRow("moimroom", "old-env", storage.DriverAWS, "AKIALEFTOVER", ""),
		storageRow("shop", "eks", storage.DriverMinIO, "SVCMINIOKEY4", ""),
	}}
	eks := keyDriver{name: storage.DriverAWS,
		keys: map[string][]storage.KeyInfo{"moimroom": {
			{ID: "AKIAHANDMADE", CreatedAt: keyMade.Add(time.Hour), Active: true},
			{ID: "AKIACURRENT1", CreatedAt: keyMade, Active: true},
		}},
		scope: storage.Scope{Actions: []string{"s3:GetObject"}, Boundary: "arn:aws:iam::123456789012:policy/hopcharlie-app-boundary"},
	}
	drivers := func(env string) (storage.Driver, error) {
		switch env {
		case "eks":
			return eks, nil
		case "gke":
			return nil, errors.New("storage: GCS Application Default Credentials are unavailable")
		}
		return nil, errors.New("environment " + env + " is not in PLATFORM_ENVS_FILE")
	}
	cs := NewStorageKeyCollectors(environments, rows, drivers, nil)
	if len(cs) != 3 {
		t.Fatalf("%d collectors, want gke, eks and the other environments", len(cs))
	}
	if s := cs[0].Sources(); len(s) != 1 || s[0].Environment != "gke" || s[0].Provider != apiv1.SecurityProviderGcp {
		t.Fatalf("gke sources %+v", s)
	}
	if s := cs[1].Sources(); len(s) != 1 || s[0].Environment != "eks" || s[0].Provider != apiv1.SecurityProviderAws {
		t.Fatalf("eks sources %+v", s)
	}
	if s := cs[2].Sources(); len(s) != 0 {
		t.Fatalf("the collector of other environments has fixed sources %+v", s)
	}
	reps := collectAll(t.Context(), cs)
	if len(reps) != 3 {
		t.Fatalf("%d reports, want gke, eks and the unconfigured old-env", len(reps))
	}
	gke, ekr, old := reps[0], reps[1], reps[2]

	// eks: the key in use with its CreateDate, the old key gone, a key the
	// platform has no record of, and a row of another driver.
	if ekr.Status != apiv1.SecuritySourceStatusError || !strings.Contains(ekr.Message, "4 keys of 2 instances") || !strings.Contains(ekr.Message, "minio driver made this key") {
		t.Fatalf("eks report %s: %s", ekr.Status, ekr.Message)
	}
	k := ekr.StorageKeys
	if len(k) != 4 {
		t.Fatalf("eks keys %+v", k)
	}
	if k[0].Use != apiv1.StorageKeyUseCurrent || k[0].State != apiv1.StorageKeyStateOk || k[0].KeyId != "...ENT1" || k[0].CreatedAt == nil || !k[0].CreatedAt.Equal(keyMade) ||
		k[0].Bucket != "hopcharlie-eks-moimroom" || k[0].App != "moimroom" || k[0].Driver != storage.DriverAWS || k[0].Message != nil {
		t.Fatalf("key in use %+v", k[0])
	}
	if s := k[0].Scope; s == nil || len(s.Actions) != 1 || s.Boundary == nil || *s.Boundary != eks.scope.Boundary {
		t.Fatalf("scope %+v", s)
	}
	if k[1].Use != apiv1.StorageKeyUseOld || k[1].State != apiv1.StorageKeyStateMissing || k[1].CreatedAt != nil || k[1].Message == nil {
		t.Fatalf("old key %+v", k[1])
	}
	if k[2].Use != apiv1.StorageKeyUseUnrecorded || k[2].State != apiv1.StorageKeyStateOk || k[2].KeyId != "...MADE" || k[2].CreatedAt == nil || k[2].Scope == nil || k[2].Message == nil {
		t.Fatalf("unrecorded key %+v", k[2])
	}
	if k[3].Instance != "shop" || k[3].State != apiv1.StorageKeyStateUnread || k[3].Scope != nil || k[3].Message == nil {
		t.Fatalf("key of another driver %+v", k[3])
	}
	// The unrecorded key is also a finding; its id is hashed by the Service.
	if f := ekr.Findings; len(f) != 1 || f[0].Check != CheckUnrecordedKey || f[0].Severity != apiv1.SecuritySeverityMedium ||
		f[0].Category != apiv1.SecurityCategoryIdentity || f[0].Resource != "bucket/hopcharlie-eks-moimroom/key/...MADE" || strings.Contains(f[0].Detail+f[0].Resource, "AKIAHANDMADE") {
		t.Fatalf("findings %+v", f)
	}

	// gke: the driver could not be built, so its key is listed unread.
	if gke.Status != apiv1.SecuritySourceStatusError || len(gke.StorageKeys) != 1 || gke.StorageKeys[0].State != apiv1.StorageKeyStateUnread ||
		gke.StorageKeys[0].CreatedAt != nil || strings.Contains(gke.Message, "Application Default Credentials") {
		t.Fatalf("gke report %s %q %+v", gke.Status, gke.Message, gke.StorageKeys)
	}
	// A row of an environment no longer configured is not dropped.
	if old.Environment != "old-env" || old.Provider != apiv1.SecurityProviderPlatform || old.Status != apiv1.SecuritySourceStatusError ||
		len(old.StorageKeys) != 1 || old.StorageKeys[0].State != apiv1.StorageKeyStateUnread {
		t.Fatalf("old-env report %+v", old)
	}

	// The keys go through the report and its environment filter; the
	// severity filter does not drop them.
	rep := assemble(t0, reps)
	if len(rep.StorageKeys) != 6 || len(rep.Findings) != 1 || rep.Findings[0].Id == "AKIAHANDMADE" {
		t.Fatalf("report keys %d, findings %+v", len(rep.StorageKeys), rep.Findings)
	}
	if f := Filter(rep, "eks", apiv1.SecuritySeverityCritical); len(f.StorageKeys) != 4 || f.StorageKeys[0].Environment != "eks" || len(f.Findings) != 0 {
		t.Fatalf("filtered keys %+v", f.StorageKeys)
	}
	if f := Filter(rep, EnvPlatform, ""); f.StorageKeys == nil || len(f.StorageKeys) != 0 {
		t.Fatalf("keys of the platform environment %+v", f.StorageKeys)
	}
}

func TestStorageKeysInactive(t *testing.T) {
	environments := []envs.EnvConfig{{Name: "eks", Provider: "aws", Storage: &storage.Config{Driver: storage.DriverAWS}}}
	row := storageRow("moimroom", "eks", storage.DriverAWS, "AKIACURRENT1", "AKIAOLDKEY02")
	drv := keyDriver{name: storage.DriverAWS, keys: map[string][]storage.KeyInfo{"moimroom": {
		{ID: "AKIACURRENT1", CreatedAt: keyMade},
		{ID: "AKIAOLDKEY02", CreatedAt: keyMade},
		{ID: "AKIAHANDMADE", CreatedAt: keyMade},
	}}}
	reps := collectAll(t.Context(), NewStorageKeyCollectors(environments, fakeRows{rows: []store.InstanceStorage{row}}, func(string) (storage.Driver, error) { return drv, nil }, nil))
	k := reps[0].StorageKeys
	if reps[0].Status != apiv1.SecuritySourceStatusOk || len(k) != 3 {
		t.Fatalf("report %+v", reps[0])
	}
	for _, key := range k {
		if key.State != apiv1.StorageKeyStateInactive || key.CreatedAt == nil {
			t.Fatalf("key %+v, want inactive with its creation time", key)
		}
	}
	if k[0].Message == nil || !strings.Contains(*k[0].Message, "cannot use its bucket") || k[1].Message != nil {
		t.Fatalf("messages %v %v", k[0].Message, k[1].Message)
	}
	if f := reps[0].Findings; len(f) != 1 || f[0].Severity != apiv1.SecuritySeverityLow {
		t.Fatalf("an inactive unrecorded key is a low finding: %+v", f)
	}
}

func TestStorageKeysReadFailures(t *testing.T) {
	environments := []envs.EnvConfig{{Name: "eks", Provider: "aws", Storage: &storage.Config{Driver: storage.DriverAWS}}}
	row := storageRow("moimroom", "eks", storage.DriverAWS, "AKIACURRENT1", "")
	collect := func(t *testing.T, ctx context.Context, rows StorageRows, drv storage.Driver) Report {
		t.Helper()
		reps := collectAll(ctx, NewStorageKeyCollectors(environments, rows, func(string) (storage.Driver, error) { return drv, nil }, nil))
		if len(reps) != 1 {
			t.Fatalf("%d reports", len(reps))
		}
		return reps[0]
	}
	one := fakeRows{rows: []store.InstanceStorage{row}}
	unread := func(t *testing.T, rep Report, want string) {
		t.Helper()
		k := rep.StorageKeys
		if rep.Status != apiv1.SecuritySourceStatusError || len(k) != 1 || k[0].State != apiv1.StorageKeyStateUnread || k[0].Message == nil || !strings.Contains(*k[0].Message, want) {
			t.Fatalf("want an unread key saying %q: %s %q %+v", want, rep.Status, rep.Message, k)
		}
	}

	t.Run("denied", func(t *testing.T) {
		denied := keyDriver{name: storage.DriverAWS, err: &smithy.GenericAPIError{Code: "AccessDenied", Message: "User: arn:aws:sts::123456789012:assumed-role/x is not authorized"}}
		rep := collect(t, t.Context(), one, denied)
		unread(t, rep, "AccessDenied")
		if rep.StorageKeys[0].Scope == nil || strings.Contains(rep.Message, "arn:aws") {
			t.Fatalf("%+v", rep)
		}
	})
	t.Run("network and HTTP errors name no provider text", func(t *testing.T) {
		netErr := fmt.Errorf("storage: list access keys of user x: %w", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connect to iam.amazonaws.com:443 refused")})
		rep := collect(t, t.Context(), one, keyDriver{name: storage.DriverAWS, err: netErr})
		unread(t, rep, "network error")
		if strings.Contains(rep.Message, "amazonaws") {
			t.Fatalf("message %q", rep.Message)
		}
		rep = collect(t, t.Context(), one, keyDriver{name: storage.DriverAWS, err: errors.New("something at /home/x/.aws/config")})
		unread(t, rep, "see the server log")
		if strings.Contains(rep.Message, ".aws") {
			t.Fatalf("message %q", rep.Message)
		}
	})
	t.Run("driver changed", func(t *testing.T) {
		unread(t, collect(t, t.Context(), one, plainDriver{}), "aws driver made this key")
	})
	t.Run("panic", func(t *testing.T) {
		unread(t, collect(t, t.Context(), one, panicDriver{keyDriver{name: storage.DriverAWS}}), "the collector failed")
	})
	t.Run("timeout", func(t *testing.T) {
		stuck := stuckDriver{keyDriver{name: storage.DriverAWS}, make(chan struct{})}
		defer close(stuck.release)
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		start := time.Now()
		rep := collect(t, ctx, one, stuck)
		if time.Since(start) > 5*time.Second {
			t.Fatal("the collector waited for a call that does not follow its context")
		}
		unread(t, rep, "timed out")
	})
	t.Run("store down", func(t *testing.T) {
		rep := collect(t, t.Context(), fakeRows{err: errors.New("connection refused")}, nil)
		if rep.Status != apiv1.SecuritySourceStatusError || len(rep.StorageKeys) != 0 || strings.Contains(rep.Message, "refused") {
			t.Fatalf("%+v", rep)
		}
	})
	t.Run("no rows", func(t *testing.T) {
		rep := collect(t, t.Context(), fakeRows{}, nil)
		if rep.Status != apiv1.SecuritySourceStatusOk || rep.Message != "0 keys of 0 instances" {
			t.Fatalf("%+v", rep)
		}
	})
}

// TestStorageKeysCapCalls: the collectors of one server make at most
// maxKeyCalls provider calls at once, and one stuck environment does not
// hold up another.
func TestStorageKeysCapCalls(t *testing.T) {
	var environments []envs.EnvConfig
	var rows []store.InstanceStorage
	for i := range maxKeyCalls + 2 {
		rows = append(rows, storageRow(fmt.Sprintf("app%d", i), "gke", storage.DriverGCS, fmt.Sprintf("GOOGKEY%05d", i), ""))
	}
	rows = append(rows, storageRow("moimroom", "eks", storage.DriverAWS, "AKIACURRENT1", ""))
	environments = append(environments,
		envs.EnvConfig{Name: "gke", Provider: "gcp", Storage: &storage.Config{Driver: storage.DriverGCS}},
		envs.EnvConfig{Name: "eks", Provider: "aws", Storage: &storage.Config{Driver: storage.DriverAWS}})
	stuck := stuckDriver{keyDriver{name: storage.DriverGCS}, make(chan struct{})}
	eks := keyDriver{name: storage.DriverAWS, keys: map[string][]storage.KeyInfo{"moimroom": {{ID: "AKIACURRENT1", Active: true}}}}
	cs := NewStorageKeyCollectors(environments, fakeRows{rows: rows}, func(env string) (storage.Driver, error) {
		if env == "gke" {
			return stuck, nil
		}
		return eks, nil
	}, nil)
	gkeDone := make(chan Report, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	go func() { gkeDone <- cs[0].Collect(ctx)[0] }()
	// Wait until the stuck gke calls fill every slot, then eks waits for one
	// only as long as its own context.
	k := cs[1].(*StorageKeyCollector)
	for deadline := time.Now().Add(2 * time.Second); len(k.calls) < maxKeyCalls; {
		if time.Now().After(deadline) {
			t.Fatalf("%d calls in flight, want %d", len(k.calls), maxKeyCalls)
		}
		time.Sleep(time.Millisecond)
	}
	short, cancelShort := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancelShort()
	if rep := cs[1].Collect(short)[0]; rep.StorageKeys[0].State != apiv1.StorageKeyStateUnread || !strings.Contains(*rep.StorageKeys[0].Message, "timed out") {
		t.Fatalf("eks while every slot is taken: %+v", rep.StorageKeys)
	}
	gke := <-gkeDone
	if len(gke.StorageKeys) != maxKeyCalls+2 || gke.StorageKeys[0].State != apiv1.StorageKeyStateUnread {
		t.Fatalf("gke %+v", gke)
	}
	close(stuck.release)
	// Once the stuck calls return, their slots are free again.
	for deadline := time.Now().Add(2 * time.Second); len(k.calls) > 0; {
		if time.Now().After(deadline) {
			t.Fatalf("%d slots still taken", len(k.calls))
		}
		time.Sleep(time.Millisecond)
	}
	if rep := cs[1].Collect(t.Context())[0]; rep.Status != apiv1.SecuritySourceStatusOk || rep.StorageKeys[0].State != apiv1.StorageKeyStateOk {
		t.Fatalf("eks afterwards %+v", rep)
	}
}

func TestCollectorsListStorageKeysOnlyWithBoth(t *testing.T) {
	environments := []envs.EnvConfig{{Name: "eks", Provider: "aws", KubeContext: "eks", Storage: &storage.Config{Driver: storage.DriverAWS}}}
	count := func(cs []Collector) int {
		n := 0
		for _, c := range cs {
			if _, ok := c.(*StorageKeyCollector); ok {
				n++
			}
		}
		return n
	}
	if n := count(Collectors(Config{Environments: environments})); n != 0 {
		t.Fatalf("%d storage key collectors without a store", n)
	}
	if n := count(Collectors(Config{Environments: environments, StorageRows: fakeRows{}, Storage: func(string) (storage.Driver, error) { return nil, errors.New("x") }})); n != 2 {
		t.Fatalf("%d storage key collectors, want eks and the other environments", n)
	}
}
