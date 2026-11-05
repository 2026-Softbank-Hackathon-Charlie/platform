package security

import (
	"context"
	"errors"
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

// plainDriver cannot read keys back.
type plainDriver struct{ storage.Driver }

func (plainDriver) Name() string { return storage.DriverMinIO }

var keyMade = time.Date(2026, 11, 4, 8, 21, 52, 0, time.UTC)

func storageRow(instance, env, driver, key, old string) store.InstanceStorage {
	return store.InstanceStorage{Instance: instance, Env: env, App: strings.Split(instance, "--")[0], Driver: driver,
		Bucket: "hopcharlie-" + env + "-" + instance, KeyID: key, OldKeyID: old}
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
			{ID: "AKIAHANDMADE", CreatedAt: keyMade.Add(time.Hour)},
			{ID: "AKIACURRENT1", CreatedAt: keyMade},
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
	c := NewStorageKeyCollector(environments, rows, drivers)
	if got := c.Sources(); len(got) != 2 || got[0].Environment != "gke" || got[1].Environment != "eks" || got[1].Provider != apiv1.SecurityProviderAws {
		t.Fatalf("sources %+v", got)
	}
	reps := c.Collect(t.Context())
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
	if len(rep.StorageKeys) != 6 {
		t.Fatalf("report keys %d", len(rep.StorageKeys))
	}
	if f := Filter(rep, "eks", apiv1.SecuritySeverityCritical); len(f.StorageKeys) != 4 || f.StorageKeys[0].Environment != "eks" {
		t.Fatalf("filtered keys %+v", f.StorageKeys)
	}
	if f := Filter(rep, EnvPlatform, ""); f.StorageKeys == nil || len(f.StorageKeys) != 0 {
		t.Fatalf("keys of the platform environment %+v", f.StorageKeys)
	}
}

func TestStorageKeysReadFailures(t *testing.T) {
	environments := []envs.EnvConfig{{Name: "eks", Provider: "aws", Storage: &storage.Config{Driver: storage.DriverAWS}}}
	row := storageRow("moimroom", "eks", storage.DriverAWS, "AKIACURRENT1", "")

	denied := keyDriver{name: storage.DriverAWS, err: &smithy.GenericAPIError{Code: "AccessDenied", Message: "User: arn:aws:sts::123456789012:assumed-role/x is not authorized"}}
	reps := NewStorageKeyCollector(environments, fakeRows{rows: []store.InstanceStorage{row}}, func(string) (storage.Driver, error) { return denied, nil }).Collect(t.Context())
	k := reps[0].StorageKeys
	if reps[0].Status != apiv1.SecuritySourceStatusError || len(k) != 1 || k[0].State != apiv1.StorageKeyStateUnread || k[0].Scope == nil ||
		k[0].Message == nil || !strings.Contains(*k[0].Message, "AccessDenied") || strings.Contains(reps[0].Message, "arn:aws") {
		t.Fatalf("denied: %s %q %+v", reps[0].Status, reps[0].Message, k)
	}

	reps = NewStorageKeyCollector(environments, fakeRows{rows: []store.InstanceStorage{row}}, func(string) (storage.Driver, error) { return plainDriver{}, nil }).Collect(t.Context())
	if k := reps[0].StorageKeys; k[0].State != apiv1.StorageKeyStateUnread || !strings.Contains(*k[0].Message, "aws driver made this key") {
		t.Fatalf("driver changed: %+v", k)
	}

	reps = NewStorageKeyCollector(environments, fakeRows{err: errors.New("connection refused")}, nil).Collect(t.Context())
	if len(reps) != 1 || reps[0].Status != apiv1.SecuritySourceStatusError || len(reps[0].StorageKeys) != 0 || strings.Contains(reps[0].Message, "refused") {
		t.Fatalf("store down: %+v", reps)
	}

	none := NewStorageKeyCollector(environments, fakeRows{}, nil).Collect(t.Context())
	if len(none) != 1 || none[0].Status != apiv1.SecuritySourceStatusOk || none[0].Message != "0 keys of 0 instances" {
		t.Fatalf("no rows: %+v", none)
	}
}

func TestCollectorsListStorageKeysOnlyWithBoth(t *testing.T) {
	environments := []envs.EnvConfig{{Name: "eks", Provider: "aws", KubeContext: "eks", Storage: &storage.Config{Driver: storage.DriverAWS}}}
	has := func(cs []Collector) bool {
		for _, c := range cs {
			if _, ok := c.(*StorageKeyCollector); ok {
				return true
			}
		}
		return false
	}
	if has(Collectors(Config{Environments: environments})) {
		t.Fatal("storage key collector without a store")
	}
	if !has(Collectors(Config{Environments: environments, StorageRows: fakeRows{}, Storage: func(string) (storage.Driver, error) { return nil, errors.New("x") }})) {
		t.Fatal("no storage key collector")
	}
}
