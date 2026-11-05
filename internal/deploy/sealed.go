package deploy

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
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
	// PGBackup is the key of the app's backup bucket, SealedSecret
	// app-pgbackup, with where the backups go (values.backup, ADR 0025);
	// nil without one.
	PGBackup *SealedBackup
	// App is the secrets the instance gets, SealedSecret app-secrets, by
	// name; empty without any.
	App map[string]string
	// AppChecksum changes exactly when what the pods get changes, so the
	// chart restarts them then; set with App.
	AppChecksum string
}

// SealedS3 is a sealed bucket key.
type SealedS3 struct{ AccessKey, SecretKey string }

// SealedBackup is the sealed key of an app's backup bucket, the bucket and
// the path in it (ADR 0025).
type SealedBackup struct {
	SealedS3
	Bucket, Path string
}

func (s *Sealed) empty() bool {
	return s == nil || (s.S3 == nil && s.PGBackup == nil && len(s.App) == 0)
}

// backupSuffix ends the name an app's backup bucket and its user are made
// under (ADR 0025). Instance names never hold "---" (app names have no "--"
// and branch slugs no runs of "-"), so it is never an instance's name.
const backupSuffix = "---pg"

// BackupInstance is the storage instance of app's backup bucket:
// <app>---pg, so the bucket is <bucketPrefix><app>---pg.
func BackupInstance(app string) string { return app + backupSuffix }

// BackupPath is the path in the backup bucket for one registration of an
// app: when it was registered. An app deleted and registered again under
// the same name gets a new, empty path, where Barman's check that a new
// database's WAL archive is empty passes (ADR 0025).
func BackupPath(registered time.Time) string { return registered.UTC().Format("20060102T150405Z") }

// checksumKeyID is where the bucket's key id goes in the checksum input. A
// secret name cannot look like it, so it never collides with one.
const checksumKeyID = "s3:keyId"

// sealedKey is the bucket key a deployment sealed for one environment.
// Once the deployment is healthy, the environment's old key goes when the
// pods restart with every key change.
type sealedKey struct {
	env string
	drv storage.Driver
	in  storage.Instance
	id  string
	// pods: nothing still uses the old key once the deployment is healthy,
	// so deleteOldKeys may delete it. With app secrets the key id is in the
	// pods' checksum, so they restart when it changes. A backup key is read
	// by the Barman Cloud plugin from the Secret for every upload.
	pods bool
	// backup: the app's backup bucket key (ADR 0025), whose row is in the
	// backup storage.
	backup bool
}

// errStuck means the platform cannot delete a deleted instance's bucket
// and key itself: the environment is gone or has another driver now. An
// operator deletes them and the row (ADR 0016).
var errStuck = errors.New("only an operator can delete them")

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
	// The default instance's postgres resource gets a backup bucket (ADR
	// 0025) where the environment's configuration names the aws driver.
	// Branch instances get none; GKE's database is backed up with Workload
	// Identity instead. It goes by the configuration, not by whether the
	// driver could be built: a deployment without the backup values would
	// take the plugin off the running database.
	needBackup := j.d.Instance == app.Name && j.e.cfg.StorageDriver != nil &&
		slices.ContainsFunc(spec.Resources, func(r appspec.Resource) bool { return r.Type == appspec.TypePostgres })
	j.keys = nil
	files := map[string][]byte{}
	for _, env := range j.d.Environments {
		var backupDrv storage.Driver
		if needBackup && j.e.cfg.StorageDriver(env.Env) == storage.DriverAWS {
			drv, err := j.driver(env.Env)
			if err != nil {
				return nil, fmt.Errorf("the postgres resource of %s needs a backup bucket in %s: %w", app.Name, env.Env, err)
			}
			backupDrv = drv
		}
		var sealed *Sealed
		if needS3 || len(values) > 0 || backupDrv != nil {
			sl, err := j.sealer(env.Env)
			if err != nil {
				return nil, err
			}
			sealed = &Sealed{}
			keyID := ""
			if needS3 {
				var drv storage.Driver
				sealed.S3, keyID, drv, err = j.bucketKey(ctx, app, env.Env, sl, false)
				if err != nil {
					return nil, err
				}
				j.keys = append(j.keys, sealedKey{env: env.Env, drv: drv, in: storage.Instance{App: app.Name, Name: j.d.Instance}, id: keyID, pods: len(values) > 0})
			}
			if backupDrv != nil {
				key, backupKeyID, _, err := j.bucketKey(ctx, app, env.Env, sl, true)
				if err != nil {
					return nil, err
				}
				sealed.PGBackup = &SealedBackup{SealedS3: *key, Bucket: backupDrv.Bucket(BackupInstance(app.Name)), Path: BackupPath(app.CreatedAt)}
				j.keys = append(j.keys, sealedKey{env: env.Env, drv: backupDrv, in: storage.Instance{App: app.Name, Name: BackupInstance(app.Name)}, id: backupKeyID, pods: true, backup: true})
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
		envImages := images
		if env.Env == "gke" && j.e.cfg.GKEImageRegistry != "" {
			envImages = make(map[string]string, len(images))
			for service, image := range images {
				_, digest, ok := strings.Cut(image, "@")
				if !ok || !digestPattern.MatchString(digest) {
					return nil, fmt.Errorf("gke image for %s has no valid digest", service)
				}
				envImages[service] = j.e.cfg.GKEImageRegistry + "/" + app.Name + "/" + service + "@" + digest
			}
		}
		b, err := Render(File{App: app.Name, Instance: j.d.Instance, Env: env.Env, Commit: j.d.Commit, Deployment: j.d.ID, Spec: spec, Images: envImages, Sealed: sealed})
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
	// A secret stored before the platform reserved its name (ADR 0024) would
	// clash with the platform's variable, and the chart refuses it.
	var reserved []string
	for _, k := range sortedKeys(values) {
		if appspec.Reserved(k) {
			reserved = append(reserved, k)
			delete(values, k)
		}
	}
	if len(reserved) > 0 {
		j.note(ctx, "not sealing secrets "+strings.Join(reserved, ", ")+": the platform sets these variables now; delete the secrets")
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
// returns the key sealed for the instance's Secret app-s3, the key's id and
// the driver. With backup it does the same for the app's backup bucket
// (ADR 0025): the driver's instance <app>---pg, the Secret app-pgbackup and
// a row in the backup storage, which no cleanup ever deletes.
//
//   - No stored key: a new bucket key, sealed and stored.
//   - A stored key sealed with the current certificate: the driver checks
//     that the key still exists; the stored sealed values are used, or a new
//     key when it is gone.
//   - A stored key sealed with another certificate: the plain key is not
//     kept, so a new key is made and sealed. The replaced key becomes the
//     row's old key, which pods that have not restarted may still use; it
//     is deleted after a healthy deployment restarted them
//     (deleteOldKeys). Until then the driver keeps it, so another new key
//     fails with storage.ErrKeyLimit.
//
// A row of another app (being deleted) or of another driver (the
// environment's driver changed) is refused, not replaced.
func (j *job) bucketKey(ctx context.Context, app store.App, env string, sl *sealing.Sealer, backup bool) (*SealedS3, string, storage.Driver, error) {
	in, secret, what := storage.Instance{App: app.Name, Name: j.d.Instance}, sealing.S3Secret, "bucket"
	if backup {
		in.Name, secret, what = BackupInstance(app.Name), sealing.PGBackupSecret, "backup bucket"
	}
	drv, err := j.driver(env)
	if err != nil {
		return nil, "", nil, fmt.Errorf("the s3 resource of %s needs a bucket in %s: %w", app.Name, env, err)
	}
	row, err := j.getRow(ctx, backup, app.Name, env)
	found := err == nil
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, "", nil, fmt.Errorf("read the bucket key of %s in %s: %w", j.d.Instance, env, err)
	case row.App != app.Name:
		return nil, "", nil, fmt.Errorf("the %s of %s in %s was made for app %s; it is being deleted, so deploy again later", what, in.Name, env, row.App)
	case row.Driver != drv.Name():
		return nil, "", nil, fmt.Errorf("the %s %s of %s in %s was made by the %s driver, but %s now has the %s driver; an operator deletes that bucket, its user and the platform's record of them (ADR 0016, 0025) before %s gets a new bucket there",
			what, row.Bucket, in.Name, env, row.Driver, env, drv.Name(), in.Name)
	}
	newKey, current, old := true, "", ""
	if found {
		current, old = row.KeyID, row.OldKeyID
		newKey = row.CertFingerprint != sl.Fingerprint()
	}
	res, err := drv.Ensure(ctx, in, newKey, current, old)
	if errors.Is(err, storage.ErrKeyLimit) {
		return nil, "", nil, fmt.Errorf("%s in %s needs a new %s key for a new sealed-secrets certificate, but the key %s that the last key change replaced may still be used by pods that did not restart since (%w); an operator restarts them and deletes that key (ADR 0016), then deploy again",
			in.Name, env, what, storage.MaskKey(old), err)
	}
	if err != nil {
		return nil, "", nil, fmt.Errorf("make the %s and key of %s in %s: %w", what, in.Name, env, err)
	}
	if res.Key == nil {
		return &SealedS3{AccessKey: row.SealedAccessKey, SecretKey: row.SealedSecretKey}, row.KeyID, drv, nil
	}
	ns := Namespace(j.d.Instance)
	ak, err := sl.Seal(ns, secret, []byte(res.Key.ID))
	if err != nil {
		return nil, "", nil, fmt.Errorf("seal the %s key of %s for %s: %w", what, in.Name, env, err)
	}
	sk, err := sl.Seal(ns, secret, []byte(res.Key.Secret))
	if err != nil {
		return nil, "", nil, fmt.Errorf("seal the %s key of %s for %s: %w", what, in.Name, env, err)
	}
	rotated := found && newKey
	if rotated {
		old = row.KeyID
	}
	next := store.InstanceStorage{
		Instance: in.Name, Env: env, App: app.Name, Driver: drv.Name(), Bucket: res.Bucket, KeyID: res.Key.ID, OldKeyID: old,
		SealedAccessKey: ak, SealedSecretKey: sk, CertFingerprint: sl.Fingerprint(), CreatedAt: j.e.now(),
	}
	if err := j.putRow(ctx, backup, next); err != nil {
		return nil, "", nil, fmt.Errorf("store the %s key of %s in %s: %w", what, in.Name, env, err)
	}
	why := "new"
	switch {
	case rotated:
		why = "new, for a new sealed-secrets certificate, keeping key " + storage.MaskKey(row.KeyID) + " until the pods restart,"
	case found:
		why = "new, as key " + storage.MaskKey(row.KeyID) + " was gone,"
	}
	j.note(ctx, fmt.Sprintf("%s %s in %s: %s key %s, sealed for %s/%s", what, res.Bucket, env, why, storage.MaskKey(res.Key.ID), ns, secret))
	return &SealedS3{AccessKey: ak, SecretKey: sk}, res.Key.ID, drv, nil
}

// getRow and putRow read and write the row of a bucket key: the deployment
// instance's (instance storage) or, with backup, the backup bucket's of app
// (backup storage, ADR 0025).
func (j *job) getRow(ctx context.Context, backup bool, app, env string) (store.InstanceStorage, error) {
	if backup {
		return j.e.cfg.Store.GetBackupStorage(ctx, app, env)
	}
	return j.e.cfg.Store.GetInstanceStorage(ctx, j.d.Instance, env)
}

func (j *job) putRow(ctx context.Context, backup bool, row store.InstanceStorage) error {
	if backup {
		return j.e.cfg.Store.PutBackupStorage(ctx, row)
	}
	return j.e.cfg.Store.PutInstanceStorage(ctx, row)
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

// deleteOldKeys runs once the deployment is healthy: it deletes the old
// bucket key (store.InstanceStorage.OldKeyID) of every environment it
// sealed the current key for. With app secrets the key id is in the pods'
// checksum, so every pod of a healthy deployment started with the current
// key. Without them nothing restarts the pods on a key change and the old
// key stays, until a later healthy deployment with app secrets (ADR 0016).
// A failure is noted, not returned: the deployment is healthy either way,
// and the next healthy deployment tries again.
func (j *job) deleteOldKeys(ctx context.Context) {
	for _, k := range j.keys {
		var msg string
		err := j.locked(ctx, func() error {
			row, err := j.getRow(ctx, k.backup, k.in.App, k.env)
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err != nil {
				return err
			}
			if row.OldKeyID == "" || row.KeyID != k.id {
				return nil // nothing old, or a newer deployment changed the key since
			}
			old := storage.MaskKey(row.OldKeyID)
			if !k.pods {
				msg = fmt.Sprintf("kept the old bucket key %s in %s: without app secrets the pods do not restart on a key change and may still use it", old, k.env)
				return nil
			}
			if err := k.drv.DeleteKey(ctx, k.in, row.OldKeyID); err != nil {
				return err
			}
			row.OldKeyID = ""
			if err := j.putRow(ctx, k.backup, row); err != nil {
				return err
			}
			msg = fmt.Sprintf("deleted the old bucket key %s in %s", old, k.env)
			return nil
		})
		if errors.Is(err, errStopped) {
			return
		}
		if err != nil {
			msg = fmt.Sprintf("could not delete the old bucket key in %s; the next healthy deployment tries again: %v", k.env, err)
		}
		if msg != "" {
			j.note(ctx, msg)
		}
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
		if err := e.deleteStorage(ctx, row); errors.Is(err, errStuck) {
			e.cfg.Log.Error("cannot delete the bucket and key of a deleted instance; an operator deletes them and the instance_storage row (ADR 0016)",
				"instance", row.Instance, "env", row.Env, "driver", row.Driver, "bucket", row.Bucket, "err", err)
			continue
		} else if err != nil {
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
		return fmt.Errorf("%w: %v", errStuck, err)
	}
	if drv.Name() != row.Driver {
		return fmt.Errorf("%w: the row was made by the %s driver, but %s now has the %s driver", errStuck, row.Driver, row.Env, drv.Name())
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
