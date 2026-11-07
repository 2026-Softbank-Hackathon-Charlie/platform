package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Ids and names break ties in byte order (COLLATE "C"), the same order the
// memory store uses, whatever the database's collation.

const appColumns = `name, repo, branch, environments, auto_deploy, spec, spec_commit, created_by, created_at`

func scanApp(row pgx.Row) (App, error) {
	var a App
	err := row.Scan(&a.Name, &a.Repo, &a.Branch, &a.Environments, &a.AutoDeploy, &a.Spec, &a.SpecCommit, &a.CreatedBy, &a.CreatedAt)
	if err != nil {
		return App{}, err
	}
	if a.Environments == nil {
		a.Environments = []string{}
	}
	a.CreatedAt = a.CreatedAt.UTC()
	return a, nil
}

func (s *Postgres) CreateApp(ctx context.Context, a App) error {
	a, err := a.Clean()
	if err != nil {
		return err
	}
	return insertApp(ctx, s.pool, a)
}

// insertApp inserts a cleaned app with q, the pool or a transaction.
func insertApp(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, a App) error {
	_, err := q.Exec(ctx, `INSERT INTO apps (`+appColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		a.Name, a.Repo, a.Branch, a.Environments, a.AutoDeploy, a.Spec, a.SpecCommit, a.CreatedBy, a.CreatedAt)
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: app %s exists", ErrConflict, a.Name)
	}
	return err
}

// CreateAppWithDatabase leaves the check of d's environments to the table's
// CHECK constraint, so a refused row rolls back the app inserted before it.
func (s *Postgres) CreateAppWithDatabase(ctx context.Context, a App, d AppDatabase) error {
	a, err := a.Clean()
	if err != nil {
		return err
	}
	d = d.Clean()
	d.App = a.Name
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	if err := insertApp(ctx, tx, a); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO app_databases (`+appDatabaseColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22)`,
		d.App, d.PrimaryEnv, d.PeerEnv, d.KeyVersion, d.KeyCheck, d.PasswordNonce, d.PasswordCiphertext,
		d.CACert, d.CAKeyNonce, d.CAKeyCiphertext, d.CreatedAt, d.PeerGeneration,
		d.SourceKind, d.Image, d.Database, d.Owner, d.StorageSize, d.ExternalHost,
		d.ReplicationPasswordNonce, d.ReplicationPasswordCiphertext, reindexArray(d.Reindex), parametersJSON(d.Parameters))
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: app %s already has a database pair", ErrConflict, a.Name)
	}
	if err != nil {
		return fmt.Errorf("store the database pair of app %s: %w", a.Name, err)
	}
	return tx.Commit(ctx)
}

func (s *Postgres) GetApp(ctx context.Context, name string) (App, error) {
	if !Clean(name) { // PostgreSQL would reject the parameter
		return App{}, fmt.Errorf("%w: app %q", ErrNotFound, name)
	}
	a, err := scanApp(s.pool.QueryRow(ctx, `SELECT `+appColumns+` FROM apps WHERE name = $1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return App{}, fmt.Errorf("%w: app %s", ErrNotFound, name)
	}
	return a, err
}

func (s *Postgres) ListApps(ctx context.Context) ([]App, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+appColumns+` FROM apps ORDER BY name COLLATE "C"`)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanApp)
}

// UpdateApp locks the app row (FOR UPDATE) before it checks the added
// branches, so a CreateAppBranch, which takes the same lock, cannot add the
// new branch in between.
func (s *Postgres) UpdateApp(ctx context.Context, a App) error {
	if !Clean(a.Name) {
		return fmt.Errorf("%w: app %q", ErrNotFound, a.Name)
	}
	a, err := a.Clean()
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	var added bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM app_branches WHERE app = $1 AND branch = $2)
		FROM apps WHERE name = $1 FOR UPDATE`, a.Name, a.Branch).Scan(&added)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: app %s", ErrNotFound, a.Name)
	}
	if err != nil {
		return err
	}
	if added {
		return fmt.Errorf("%w: %s is an added branch of app %s", ErrBranchExists, a.Branch, a.Name)
	}
	if _, err := tx.Exec(ctx, `UPDATE apps SET branch = $2, auto_deploy = $3, spec = $4, spec_commit = $5
		WHERE name = $1`, a.Name, a.Branch, a.AutoDeploy, a.Spec, a.SpecCommit); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// SetAppEnvironments compares and sets in one UPDATE; when it matches no
// row, a second read tells a missing app from other environments.
func (s *Postgres) SetAppEnvironments(ctx context.Context, name string, from, to []string) error {
	if !Clean(name) {
		return fmt.Errorf("%w: app %q", ErrNotFound, name)
	}
	if len(to) == 0 {
		return fmt.Errorf("app %s: an app keeps at least one environment", name)
	}
	clean := func(list []string) []string {
		out := make([]string, len(list))
		for i, e := range list {
			out[i] = CleanText(e)
		}
		return out
	}
	tag, err := s.pool.Exec(ctx, `UPDATE apps SET environments = $3 WHERE name = $1 AND environments = $2`, name, clean(from), clean(to))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	cur, err := s.GetApp(ctx, name)
	if err != nil {
		return err
	}
	return fmt.Errorf("%w: app %s is in %v, not %v", ErrConflict, name, cur.Environments, from)
}

// DeleteApp deletes the app row first. Its lock waits for a CreateDeployment,
// CreateAppBranch or ChangeAppSecrets that holds the row, so the branches,
// secrets and deployments deleted next include theirs; a later create finds
// no app. Deleting the
// deployments waits for appends that hold them, so the events deleted last
// include theirs.
func (s *Postgres) DeleteApp(ctx context.Context, name string) error {
	if !Clean(name) {
		return fmt.Errorf("%w: app %q", ErrNotFound, name)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	tag, err := tx.Exec(ctx, `DELETE FROM apps WHERE name = $1`, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: app %s", ErrNotFound, name)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM app_branches WHERE app = $1`, name); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM app_secrets WHERE app = $1`, name); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM app_databases WHERE app = $1`, name); err != nil {
		return err
	}
	if err := deleteDeployments(ctx, tx, `app = $1`, name); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// deleteDeployments deletes the deployments that match where, then their
// events.
func deleteDeployments(ctx context.Context, tx pgx.Tx, where string, args ...any) error {
	rows, err := tx.Query(ctx, `DELETE FROM deployments WHERE `+where+` RETURNING id`, args...)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `DELETE FROM deployment_events WHERE deployment_id = ANY($1::text[])`, ids)
	return err
}

const appBranchColumns = `app, branch, instance, auto_deploy, spec, spec_commit, created_by, created_at`

func scanAppBranch(row pgx.Row) (AppBranch, error) {
	var b AppBranch
	err := row.Scan(&b.App, &b.Branch, &b.Instance, &b.AutoDeploy, &b.Spec, &b.SpecCommit, &b.CreatedBy, &b.CreatedAt)
	if err != nil {
		return AppBranch{}, err
	}
	b.CreatedAt = b.CreatedAt.UTC()
	return b, nil
}

// CreateAppBranch locks the app row (FOR UPDATE) while it checks and
// inserts. Creates for one app therefore run one at a time and count each
// other's branches; an UpdateApp cannot change the app's branch in between;
// and a DeleteApp waits for it and then removes the new branch too. Another
// app's create can take the same instance name at the same moment: the
// unique index makes the later insert fail, and that is ErrInstanceExists.
// The locks go app, then branch, then deployments, then events, as in
// CreateDeployment, DeleteAppBranch and DeleteApp.
func (s *Postgres) CreateAppBranch(ctx context.Context, b AppBranch, maxBranches int) error {
	if !Clean(b.App) {
		return fmt.Errorf("%w: app %q", ErrNotFound, b.App)
	}
	b, err := b.Clean()
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	var appBranch string
	err = tx.QueryRow(ctx, `SELECT branch FROM apps WHERE name = $1 FOR UPDATE`, b.App).Scan(&appBranch)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: app %s", ErrNotFound, b.App)
	}
	if err != nil {
		return err
	}
	if b.Branch == appBranch {
		return fmt.Errorf("%w: %s is the branch of app %s", ErrBranchExists, b.Branch, b.App)
	}
	var added, taken bool
	var count int
	err = tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM app_branches WHERE app = $1 AND branch = $2),
			EXISTS (SELECT 1 FROM apps WHERE name = $3) OR EXISTS (SELECT 1 FROM app_branches WHERE instance = $3),
			(SELECT count(*) FROM app_branches WHERE app = $1)`,
		b.App, b.Branch, b.Instance).Scan(&added, &taken, &count)
	switch {
	case err != nil:
		return err
	case added:
		return fmt.Errorf("%w: app %s has branch %s", ErrBranchExists, b.App, b.Branch)
	case taken:
		return fmt.Errorf("%w: instance name %s is taken", ErrInstanceExists, b.Instance)
	case count >= maxBranches:
		return fmt.Errorf("%w: app %s has %d added branches", ErrInstanceLimit, b.App, count)
	}
	_, err = tx.Exec(ctx, `INSERT INTO app_branches (`+appBranchColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		b.App, b.Branch, b.Instance, b.AutoDeploy, b.Spec, b.SpecCommit, b.CreatedBy, b.CreatedAt)
	if isUniqueViolation(err) {
		// The app row lock rules out the same branch, so another app took
		// the instance name meanwhile.
		return fmt.Errorf("%w: instance name %s is taken", ErrInstanceExists, b.Instance)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Postgres) ListAppBranches(ctx context.Context, app string) ([]AppBranch, error) {
	if !Clean(app) {
		return []AppBranch{}, nil
	}
	rows, err := s.pool.Query(ctx, `SELECT `+appBranchColumns+` FROM app_branches
		WHERE app = $1 ORDER BY branch COLLATE "C"`, app)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanAppBranch)
}

func (s *Postgres) UpdateAppBranch(ctx context.Context, b AppBranch) error {
	if !Clean(b.App) || !Clean(b.Branch) {
		return fmt.Errorf("%w: app %q branch %q", ErrNotFound, b.App, b.Branch)
	}
	b, err := b.Clean()
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `UPDATE app_branches SET auto_deploy = $3, spec = $4, spec_commit = $5
		WHERE app = $1 AND branch = $2`, b.App, b.Branch, b.AutoDeploy, b.Spec, b.SpecCommit)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: app %s branch %s", ErrNotFound, b.App, b.Branch)
	}
	return nil
}

// DeleteAppBranch deletes the branch row first. Its lock waits for a
// CreateDeployment or ChangeAppSecrets that holds the row, so the instance's
// overrides and deployments deleted next include theirs; a later create
// finds no branch.
func (s *Postgres) DeleteAppBranch(ctx context.Context, app, branch string) error {
	if !Clean(app) || !Clean(branch) {
		return fmt.Errorf("%w: app %q branch %q", ErrNotFound, app, branch)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	var instance string
	err = tx.QueryRow(ctx, `DELETE FROM app_branches WHERE app = $1 AND branch = $2 RETURNING instance`, app, branch).Scan(&instance)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: app %s branch %s", ErrNotFound, app, branch)
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM app_secrets WHERE app = $1 AND instance = $2`, app, instance); err != nil {
		return err
	}
	if err := deleteDeployments(ctx, tx, `app = $1 AND instance = $2`, app, instance); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

const deploymentColumns = `id, app, branch, instance, ref, commit_sha, trigger, triggered_by, status, images, environments,
	error, build_id, spec, created_at, started_at, finished_at`

func scanDeployment(row pgx.Row) (Deployment, error) {
	var d Deployment
	var images, envs []byte
	err := row.Scan(&d.ID, &d.App, &d.Branch, &d.Instance, &d.Ref, &d.Commit, &d.Trigger, &d.TriggeredBy, &d.Status, &images, &envs,
		&d.Error, &d.BuildID, &d.Spec, &d.CreatedAt, &d.StartedAt, &d.FinishedAt)
	if err != nil {
		return Deployment{}, err
	}
	d.Images, d.Environments = []DeploymentImage{}, []DeploymentEnvironment{}
	if err := json.Unmarshal(images, &d.Images); err != nil {
		return Deployment{}, fmt.Errorf("deployment %s images: %w", d.ID, err)
	}
	if err := json.Unmarshal(envs, &d.Environments); err != nil {
		return Deployment{}, fmt.Errorf("deployment %s environments: %w", d.ID, err)
	}
	d.CreatedAt, d.StartedAt, d.FinishedAt = d.CreatedAt.UTC(), utcPtr(d.StartedAt), utcPtr(d.FinishedAt)
	return d, nil
}

func utcPtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// deploymentJSON encodes the jsonb columns of a cleaned deployment.
func deploymentJSON(d Deployment) (images, envs []byte, err error) {
	if images, err = json.Marshal(d.Images); err != nil {
		return nil, nil, err
	}
	if envs, err = json.Marshal(d.Environments); err != nil {
		return nil, nil, err
	}
	return images, envs, nil
}

// CreateDeployment holds the app row (FOR SHARE) while it inserts, so a
// DeleteApp waits for it and then removes the new deployment too. A
// deployment of an added branch also holds the branch row (FOR SHARE), so a
// DeleteAppBranch waits for it the same way.
func (s *Postgres) CreateDeployment(ctx context.Context, d Deployment) error {
	if !Clean(d.App) {
		return fmt.Errorf("%w: app %q", ErrNotFound, d.App)
	}
	if !Clean(d.Branch) || !Clean(d.Instance) {
		return fmt.Errorf("%w: branch %q instance %q", ErrInstanceNotFound, d.Branch, d.Instance)
	}
	d, err := d.Clean()
	if err != nil {
		return err
	}
	images, envs, err := deploymentJSON(d)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	var appBranch string
	err = tx.QueryRow(ctx, `SELECT branch FROM apps WHERE name = $1 FOR SHARE`, d.App).Scan(&appBranch)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: app %s", ErrNotFound, d.App)
	}
	if err != nil {
		return err
	}
	if d.Branch != appBranch || d.Instance != d.App {
		var one int
		err = tx.QueryRow(ctx, `SELECT 1 FROM app_branches WHERE app = $1 AND branch = $2 AND instance = $3 FOR SHARE`,
			d.App, d.Branch, d.Instance).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: app %s has no instance %s of branch %s", ErrInstanceNotFound, d.App, d.Instance, d.Branch)
		}
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO deployments (`+deploymentColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
		d.ID, d.App, d.Branch, d.Instance, d.Ref, d.Commit, d.Trigger, d.TriggeredBy, d.Status, images, envs,
		d.Error, d.BuildID, d.Spec, d.CreatedAt, d.StartedAt, d.FinishedAt)
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: deployment %s exists", ErrConflict, d.ID)
	}
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Postgres) GetDeployment(ctx context.Context, id string) (Deployment, error) {
	if !Clean(id) {
		return Deployment{}, fmt.Errorf("%w: deployment %q", ErrNotFound, id)
	}
	d, err := scanDeployment(s.pool.QueryRow(ctx, `SELECT `+deploymentColumns+` FROM deployments WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, fmt.Errorf("%w: deployment %s", ErrNotFound, id)
	}
	return d, err
}

func (s *Postgres) ListDeployments(ctx context.Context, app string, limit int) ([]Deployment, error) {
	if !Clean(app) {
		return []Deployment{}, nil
	}
	var lim *int // LIMIT NULL means no limit
	if limit > 0 {
		lim = &limit
	}
	rows, err := s.pool.Query(ctx, `SELECT `+deploymentColumns+` FROM deployments
		WHERE app = $1 ORDER BY created_at DESC, id COLLATE "C" DESC LIMIT $2`, app, lim)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanDeployment)
}

func (s *Postgres) ListInstanceDeployments(ctx context.Context, instance string, limit int) ([]Deployment, error) {
	if !Clean(instance) {
		return []Deployment{}, nil
	}
	var lim *int // LIMIT NULL means no limit
	if limit > 0 {
		lim = &limit
	}
	rows, err := s.pool.Query(ctx, `SELECT `+deploymentColumns+` FROM deployments
		WHERE instance = $1 ORDER BY created_at DESC, id COLLATE "C" DESC LIMIT $2`, instance, lim)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanDeployment)
}

// UpdateDeployment is one conditional UPDATE: a writer that marks the
// deployment superseded and its worker serialize on the row lock, and the
// one that comes second re-checks the status against the first one's write.
func (s *Postgres) UpdateDeployment(ctx context.Context, d Deployment) (Deployment, error) {
	if !Clean(d.ID) {
		return Deployment{}, fmt.Errorf("%w: deployment %q", ErrNotFound, d.ID)
	}
	d, err := d.Clean()
	if err != nil {
		return Deployment{}, err
	}
	images, envs, err := deploymentJSON(d)
	if err != nil {
		return Deployment{}, err
	}
	out, err := scanDeployment(s.pool.QueryRow(ctx, `UPDATE deployments SET
			status = $2, images = $3, environments = $4, error = $5, build_id = $6, spec = $7, started_at = $8, finished_at = $9
		WHERE id = $1 AND status <> ALL($10::text[])
		RETURNING `+deploymentColumns,
		d.ID, d.Status, images, envs, d.Error, d.BuildID, d.Spec, d.StartedAt, d.FinishedAt, finishedDeployment))
	if !errors.Is(err, pgx.ErrNoRows) {
		return out, err
	}
	var current string
	err = s.pool.QueryRow(ctx, `SELECT status FROM deployments WHERE id = $1`, d.ID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return Deployment{}, fmt.Errorf("%w: deployment %s", ErrNotFound, d.ID)
	}
	if err != nil {
		return Deployment{}, err
	}
	return Deployment{}, fmt.Errorf("%w: deployment is %s", ErrConflict, current)
}

func (s *Postgres) ListUnfinishedDeployments(ctx context.Context) ([]Deployment, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+deploymentColumns+` FROM deployments
		WHERE status <> ALL($1::text[]) ORDER BY created_at, id COLLATE "C"`, finishedDeployment)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanDeployment)
}

const deploymentEventColumns = `seq, deployment_id, time, type, phase, env, message`

func scanDeploymentEvent(row pgx.Row) (DeploymentEvent, error) {
	var e DeploymentEvent
	if err := row.Scan(&e.Seq, &e.DeploymentID, &e.Time, &e.Type, &e.Phase, &e.Env, &e.Message); err != nil {
		return DeploymentEvent{}, err
	}
	e.Time = e.Time.UTC()
	return e, nil
}

// AppendDeploymentEvent locks the deployment row for the insert, so
// concurrent appends to one deployment commit one at a time in Seq order.
func (s *Postgres) AppendDeploymentEvent(ctx context.Context, e DeploymentEvent) (DeploymentEvent, error) {
	if !Clean(e.DeploymentID) {
		return DeploymentEvent{}, fmt.Errorf("%w: deployment %q", ErrNotFound, e.DeploymentID)
	}
	e = e.Clean()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return DeploymentEvent{}, err
	}
	defer tx.Rollback(ctx) // no-op after Commit

	var one int
	err = tx.QueryRow(ctx, `SELECT 1 FROM deployments WHERE id = $1 FOR UPDATE`, e.DeploymentID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return DeploymentEvent{}, fmt.Errorf("%w: deployment %s", ErrNotFound, e.DeploymentID)
	}
	if err != nil {
		return DeploymentEvent{}, err
	}
	out, err := scanDeploymentEvent(tx.QueryRow(ctx, `INSERT INTO deployment_events (deployment_id, time, type, phase, env, message)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+deploymentEventColumns,
		e.DeploymentID, e.Time, e.Type, e.Phase, e.Env, e.Message))
	if err != nil {
		return DeploymentEvent{}, err
	}
	return out, tx.Commit(ctx)
}

func (s *Postgres) ListDeploymentEvents(ctx context.Context, deploymentID string, afterSeq int64, limit int) ([]DeploymentEvent, error) {
	if !Clean(deploymentID) {
		return []DeploymentEvent{}, nil
	}
	var lim *int
	if limit > 0 {
		lim = &limit
	}
	rows, err := s.pool.Query(ctx, `SELECT `+deploymentEventColumns+` FROM deployment_events
		WHERE deployment_id = $1 AND seq > $2 ORDER BY seq LIMIT $3`, deploymentID, afterSeq, lim)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanDeploymentEvent)
}

func (s *Postgres) RecordWebhookDelivery(ctx context.Context, id string, at time.Time) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO webhook_deliveries (id, received_at) VALUES ($1, $2)
		ON CONFLICT (id) DO NOTHING`, CleanText(id), at)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// parametersJSON is p for a jsonb NOT NULL column.
func parametersJSON(p map[string]string) map[string]string {
	if p == nil {
		return map[string]string{}
	}
	return p
}

// reindexArray is l for a text[] NOT NULL column: pgx sends a nil slice
// as NULL.
func reindexArray(l []string) []string {
	if l == nil {
		return []string{}
	}
	return l
}
