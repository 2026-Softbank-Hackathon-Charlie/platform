package deploy

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/sealing"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/storage"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
)

// Store is what the executor keeps its records in: the deployments and,
// for the sealed values, the app secrets and the instances' buckets and
// keys. store.Store implements it.
type Store interface {
	store.DeployStore
	store.SecretStore
}

// Sealed is values.sealedSecrets of a gitops file (ADR 0016, ADR 0019):
// values sealed with the cluster's sealed-secrets certificate for the
// instance's namespace, which the app chart renders as SealedSecrets.
type Sealed struct {
	// S3 is the bucket's key, SealedSecret app-s3; nil without an s3
	// resource.
	S3 *SealedS3
	// App is the secrets the instance gets, SealedSecret app-secrets, by
	// name; empty without any.
	App map[string]string
	// AppChecksum changes exactly when what the pods get changes, so the
	// chart restarts them then; set with App.
	AppChecksum string
}

// SealedS3 is a sealed bucket key.
type SealedS3 struct{ AccessKey, SecretKey string }

func (s *Sealed) empty() bool { return s == nil || (s.S3 == nil && len(s.App) == 0) }

// checksumKeyID is where the bucket's key id goes in the checksum input. A
// secret name cannot look like it, so it never collides with one.
const checksumKeyID = "s3:keyId"

// oldKey is a bucket key a deployment replaced; it is deleted once the
// deployment is healthy and the pods have restarted with the new one.
type oldKey struct {
	env  string
	drv  storage.Driver
	in   storage.Instance
	id   string
	pods bool // the pods restart with this deployment, so they stop using it
}

// sealedFiles renders the gitops files of every environment of the
// deployment with their sealed values. It runs under the instance's lock:
// it makes the instance's bucket and key, so storage operations of one
// instance never overlap.
func (j *job) sealedFiles(ctx context.Context, app store.App, spec appspec.Spec, images map[string]string) (map[string][]byte, error) {
	values, err := j.appSecretValues(ctx, app)
	if err != nil {
		return nil, err
	}
	needS3 := slices.ContainsFunc(spec.Resources, func(r appspec.Resource) bool { return r.Type == appspec.TypeS3 })
	j.oldKeys = nil
	files := map[string][]byte{}
	for _, env := range j.d.Environments {
		var sealed *Sealed
		if needS3 || len(values) > 0 {
			sl, err := j.sealer(env.Env)
			if err != nil {
				return nil, err
			}
			sealed = &Sealed{}
			keyID := ""
			if needS3 {
				var replaced string
				var drv storage.Driver
				sealed.S3, keyID, replaced, drv, err = j.bucketKey(ctx, app, env.Env, sl)
				if err != nil {
					return nil, err
				}
				if replaced != "" {
					j.oldKeys = append(j.oldKeys, oldKey{env: env.Env, drv: drv, in: storage.Instance{App: app.Name, Name: j.d.Instance}, id: replaced, pods: len(values) > 0})
				}
			}
			if len(values) > 0 {
				if sealed.App, err = sl.SealAll(Namespace(j.d.Instance), sealing.AppSecret, values); err != nil {
					return nil, fmt.Errorf("seal the secrets of %s for %s: %w", j.d.Instance, env.Env, err)
				}
				in := maps.Clone(values)
				if keyID != "" {
					in[checksumKeyID] = keyID
				}
				sealed.AppChecksum = j.e.cfg.Secrets.Checksum(app.Name, j.d.Instance, in)
			}
		}
		b, err := Render(File{App: app.Name, Instance: j.d.Instance, Env: env.Env, Commit: j.d.Commit, Deployment: j.d.ID, Spec: spec, Images: images, Sealed: sealed})
		if err != nil {
			return nil, err
		}
		files[FilePath(j.d.Instance, env.Env)] = b
	}
	if len(values) > 0 {
		j.note(ctx, fmt.Sprintf("sealed %d secrets of %s for %s", len(values), j.d.Instance, envNames(j.d.Environments)))
	}
	return files, nil
}

// appSecretValues returns the secrets the deployment's instance gets,
// decrypted (ADR 0019); empty when it gets none.
func (j *job) appSecretValues(ctx context.Context, app store.App) (map[string]string, error) {
	rows, err := j.e.cfg.Store.ListAppSecrets(ctx, app.Name)
	if err != nil {
		return nil, fmt.Errorf("read the secrets of %s: %w", app.Name, err)
	}
	relevant := slices.ContainsFunc(rows, func(r store.AppSecret) bool { return r.Instance == "" || r.Instance == j.d.Instance })
	if !relevant {
		return nil, nil
	}
	if j.e.cfg.Secrets == nil {
		return nil, fmt.Errorf("app %s has secrets, but this server has no PLATFORM_SECRETS_KEY to decrypt them", app.Name)
	}
	values, err := j.e.cfg.Secrets.InstanceValues(rows, j.d.Instance)
	if err != nil {
		return nil, fmt.Errorf("decrypt the secrets of %s: %w", j.d.Instance, err)
	}
	return values, nil
}

// sealer returns the environment's sealer, or an error that says why there
// is none.
func (j *job) sealer(env string) (*sealing.Sealer, error) {
	if j.e.cfg.Sealer == nil {
		return nil, fmt.Errorf("%s needs sealed values in %s, but this server has no sealed-secrets certificates", j.d.Instance, env)
	}
	sl, err := j.e.cfg.Sealer(env)
	if err != nil {
		return nil, fmt.Errorf("%s needs sealed values in %s, but there is no usable sealed-secrets certificate: %w", j.d.Instance, env, err)
	}
	return sl, nil
}

// bucketKey makes sure the instance has its bucket and a key in env and
// returns the key sealed for the instance's Secret app-s3, the key's id,
// and the id of a key it replaced (to delete once healthy), or "".
//
//   - No stored key: a new bucket key, sealed and stored.
//   - A stored key sealed with the current certificate: the driver checks
//     that the key still exists; the stored sealed values are used, or a new
//     key when it is gone.
//   - A stored key sealed with another certificate: the plain key is not
//     kept, so a new key is made and sealed, keeping the old one until the
//     deployment is healthy.
func (j *job) bucketKey(ctx context.Context, app store.App, env string, sl *sealing.Sealer) (*SealedS3, string, string, storage.Driver, error) {
	drv, err := j.driver(env)
	if err != nil {
		return nil, "", "", nil, fmt.Errorf("the s3 resource of %s needs a bucket in %s: %w", app.Name, env, err)
	}
	in := storage.Instance{App: app.Name, Name: j.d.Instance}
	row, err := j.e.cfg.Store.GetInstanceStorage(ctx, j.d.Instance, env)
	found := err == nil
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, "", "", nil, fmt.Errorf("read the bucket key of %s in %s: %w", j.d.Instance, env, err)
	case row.App != app.Name:
		return nil, "", "", nil, fmt.Errorf("the bucket of %s in %s was made for app %s; it is being deleted, so deploy again later", j.d.Instance, env, row.App)
	}
	newKey, keep, replaced := true, "", ""
	if found {
		keep = row.KeyID
		if row.CertFingerprint == sl.Fingerprint() {
			newKey = false
		} else {
			replaced = row.KeyID
		}
	}
	res, err := drv.Ensure(ctx, in, newKey, keep)
	if err != nil {
		return nil, "", "", nil, fmt.Errorf("make the bucket and key of %s in %s: %w", j.d.Instance, env, err)
	}
	if res.Key == nil {
		return &SealedS3{AccessKey: row.SealedAccessKey, SecretKey: row.SealedSecretKey}, row.KeyID, "", drv, nil
	}
	ns := Namespace(j.d.Instance)
	ak, err := sl.Seal(ns, sealing.S3Secret, []byte(res.Key.ID))
	if err != nil {
		return nil, "", "", nil, fmt.Errorf("seal the bucket key of %s for %s: %w", j.d.Instance, env, err)
	}
	sk, err := sl.Seal(ns, sealing.S3Secret, []byte(res.Key.Secret))
	if err != nil {
		return nil, "", "", nil, fmt.Errorf("seal the bucket key of %s for %s: %w", j.d.Instance, env, err)
	}
	next := store.InstanceStorage{
		Instance: j.d.Instance, Env: env, App: app.Name, Driver: drv.Name(), Bucket: res.Bucket, KeyID: res.Key.ID,
		SealedAccessKey: ak, SealedSecretKey: sk, CertFingerprint: sl.Fingerprint(), CreatedAt: j.e.now(),
	}
	if err := j.e.cfg.Store.PutInstanceStorage(ctx, next); err != nil {
		return nil, "", "", nil, fmt.Errorf("store the bucket key of %s in %s: %w", j.d.Instance, env, err)
	}
	why := "new"
	switch {
	case replaced != "":
		why = "new, for a new sealed-secrets certificate"
	case found:
		why = "new, as key " + row.KeyID + " was gone"
	}
	j.note(ctx, fmt.Sprintf("bucket %s in %s: %s key %s, sealed for %s/%s", res.Bucket, env, why, res.Key.ID, ns, sealing.S3Secret))
	return &SealedS3{AccessKey: ak, SecretKey: sk}, res.Key.ID, replaced, drv, nil
}

// driver returns the environment's storage driver, or an error that says
// why there is none.
func (j *job) driver(env string) (storage.Driver, error) {
	if j.e.cfg.Storage == nil {
		return nil, errors.New("this server has no storage drivers")
	}
	drv, err := j.e.cfg.Storage(env)
	if err != nil {
		return nil, err
	}
	return drv, nil
}

// deleteOldKeys deletes the bucket keys the deployment replaced, once it
// is healthy. A key the pods may still use stays: only a pod restart makes
// them read the new one, and without app secrets nothing restarts them.
// A failure is noted, not returned: the deployment is healthy either way,
// and the next key change deletes what is left.
func (j *job) deleteOldKeys(ctx context.Context) {
	for _, k := range j.oldKeys {
		if !k.pods {
			j.note(ctx, fmt.Sprintf("kept the old bucket key %s in %s: the pods were not restarted and may still use it; the next key change deletes it", k.id, k.env))
			continue
		}
		err := j.locked(ctx, func() error { return k.drv.DeleteKey(ctx, k.in, k.id) })
		if errors.Is(err, errStopped) {
			return
		}
		if err != nil {
			j.note(ctx, fmt.Sprintf("could not delete the old bucket key %s in %s: %v", k.id, k.env, err))
			continue
		}
		j.note(ctx, fmt.Sprintf("deleted the old bucket key %s in %s", k.id, k.env))
	}
}

// DefaultCleanupInterval is how often the executor retries deleting the
// buckets and keys of deleted instances.
const DefaultCleanupInterval = 5 * time.Minute

// CleanupStorage deletes the buckets and keys of instances that no longer
// exist (ADR 0016): for every stored row whose app or instance is gone it
// calls the environment's driver to delete the user, its keys and the
// bucket, and then deletes the row. A failure keeps the row for the next
// run. It returns how many rows it deleted.
func (e *Executor) CleanupStorage(ctx context.Context) (int, error) {
	if e.cfg.Storage == nil {
		return 0, nil
	}
	rows, err := e.cfg.Store.ListInstanceStorage(ctx, "", "")
	if err != nil {
		return 0, fmt.Errorf("list the instances' buckets: %w", err)
	}
	done := 0
	for _, row := range rows {
		live, err := e.instanceExists(ctx, row.App, row.Instance)
		if err != nil {
			return done, err
		}
		if live {
			continue
		}
		if err := e.deleteStorage(ctx, row); err != nil {
			e.cfg.Log.Warn("delete the bucket and key of a deleted instance; trying again later",
				"instance", row.Instance, "env", row.Env, "bucket", row.Bucket, "err", err)
			continue
		}
		done++
		e.cfg.Log.Info("deleted the bucket and key of a deleted instance", "instance", row.Instance, "env", row.Env, "bucket", row.Bucket)
	}
	return done, nil
}

// deleteStorage deletes one row's bucket and key under the instance's
// lock, then the row.
func (e *Executor) deleteStorage(ctx context.Context, row store.InstanceStorage) error {
	l := e.lock(row.Instance)
	l.Lock()
	defer l.Unlock()
	drv, err := e.cfg.Storage(row.Env)
	if err != nil {
		return err
	}
	if drv.Name() != row.Driver {
		return fmt.Errorf("the row was made by the %s driver, but %s now has the %s driver", row.Driver, row.Env, drv.Name())
	}
	if err := drv.Delete(ctx, storage.Instance{App: row.App, Name: row.Instance}); err != nil {
		return err
	}
	if err := e.cfg.Store.DeleteInstanceStorage(ctx, row.Instance, row.Env); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}

// instanceExists reports whether app is registered and has the instance.
func (e *Executor) instanceExists(ctx context.Context, app, instance string) (bool, error) {
	if _, err := e.cfg.Store.GetApp(ctx, app); errors.Is(err, store.ErrNotFound) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if instance == app {
		return true, nil
	}
	branches, err := e.cfg.Store.ListAppBranches(ctx, app)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(branches, func(b store.AppBranch) bool { return b.Instance == instance }), nil
}

// cleanupLoop runs CleanupStorage now and then every CleanupInterval, and
// whenever wakeCleanup asks, until ctx ends.
func (e *Executor) cleanupLoop(ctx context.Context) {
	tick := time.NewTicker(e.cfg.CleanupInterval)
	defer tick.Stop()
	for {
		if _, err := e.CleanupStorage(ctx); err != nil && ctx.Err() == nil {
			e.cfg.Log.Warn("clean up the buckets and keys of deleted instances", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-e.cleanupNow:
		}
	}
}

// wakeCleanup asks the cleanup loop to run now.
func (e *Executor) wakeCleanup() {
	select {
	case e.cleanupNow <- struct{}{}:
	default:
	}
}
