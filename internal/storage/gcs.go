package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2/google"
)

const (
	gcsLabelApp      = "hopcharlie-app"
	gcsLabelInstance = "hopcharlie-instance"
)

type gcsDriver struct {
	project, prefix, region string
	client                  *http.Client
	storageURL, iamURL      string
}

type gcsBucket struct {
	Name           string            `json:"name,omitempty"`
	Location       string            `json:"location,omitempty"`
	LocationType   string            `json:"locationType,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
	Metageneration string            `json:"metageneration,omitempty"`
	IAM            *gcsBucketIAM     `json:"iamConfiguration,omitempty"`
}

type gcsBucketIAM struct {
	Uniform struct {
		Enabled bool `json:"enabled"`
	} `json:"uniformBucketLevelAccess"`
	PublicAccessPrevention string `json:"publicAccessPrevention"`
}

type gcsAccount struct {
	Name        string `json:"name,omitempty"`
	Email       string `json:"email,omitempty"`
	ProjectID   string `json:"projectId,omitempty"`
	Description string `json:"description,omitempty"`
	DisplayName string `json:"displayName,omitempty"`
	Disabled    bool   `json:"disabled,omitempty"`
}

type gcsKey struct {
	AccessID            string `json:"accessId,omitempty"`
	ServiceAccountEmail string `json:"serviceAccountEmail,omitempty"`
	State               string `json:"state,omitempty"`
	Etag                string `json:"etag,omitempty"`
	TimeCreated         string `json:"timeCreated,omitempty"`
}

type gcsBinding struct {
	Role      string          `json:"role"`
	Members   []string        `json:"members"`
	Condition json.RawMessage `json:"condition,omitempty"`
}

type gcsPolicy struct {
	Version  int          `json:"version,omitempty"`
	Etag     string       `json:"etag"`
	Bindings []gcsBinding `json:"bindings"`
}

type gcsObject struct {
	Name       string `json:"name"`
	Generation string `json:"generation"`
}

type gcsHTTPError int

func (e gcsHTTPError) Error() string { return "Google API returned HTTP " + strconv.Itoa(int(e)) }

// HTTPStatus is the HTTP status a gcs driver call failed with, or 0 for any
// other error. It names no provider text.
func HTTPStatus(err error) int { return gcsStatus(err) }

func gcsStatus(err error) int {
	var e gcsHTTPError
	if errors.As(err, &e) {
		return int(e)
	}
	return 0
}

func newGCS(ctx context.Context, c Config) (*gcsDriver, error) {
	client, err := google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, errors.New("storage: GCS Application Default Credentials are unavailable")
	}
	client.Timeout = 30 * time.Second
	return &gcsDriver{project: c.Project, prefix: c.BucketPrefix, region: c.Region, client: client, storageURL: "https://storage.googleapis.com/storage/v1", iamURL: "https://iam.googleapis.com/v1"}, nil
}

var _ KeyReader = (*gcsDriver)(nil)

func (d *gcsDriver) Name() string                  { return DriverGCS }
func (d *gcsDriver) Bucket(instance string) string { return d.prefix + instance }

func (d *gcsDriver) account(in Instance) (string, string) {
	hash := sha256.Sum256([]byte(in.Name))
	id := "hc-app-" + hex.EncodeToString(hash[:11])
	return id, id + "@" + d.project + ".iam.gserviceaccount.com"
}

func gcsDescription(in Instance) string { return "HopCharlie app=" + in.App + "; instance=" + in.Name }
func (d *gcsDriver) accountPath(email string) string {
	return d.iamURL + "/projects/" + url.PathEscape(d.project) + "/serviceAccounts/" + url.PathEscape(email)
}
func (d *gcsDriver) bucketPath(bucket string) string {
	return d.storageURL + "/b/" + url.PathEscape(bucket)
}
func (d *gcsDriver) keyPath(id string) string {
	path := d.storageURL + "/projects/" + url.PathEscape(d.project) + "/hmacKeys"
	if id != "" {
		path += "/" + url.PathEscape(id)
	}
	return path
}

// request never includes credentials or provider response bodies in an
// error. In particular, HMAC create responses contain a one-time secret.
func (d *gcsDriver) request(ctx context.Context, method, endpoint string, q url.Values, body, out any) error {
	var data []byte
	if body != nil {
		var err error
		data, err = json.Marshal(body)
		if err != nil {
			return errors.New("storage: encode Google API request")
		}
	}
	if len(q) > 0 {
		endpoint += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(data))
	if err != nil {
		return errors.New("storage: prepare Google API request")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := d.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("storage: Google API request failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return gcsHTTPError(res.StatusCode)
	}
	if out != nil {
		if err = json.NewDecoder(io.LimitReader(res.Body, 8<<20)).Decode(out); err != nil {
			return errors.New("storage: invalid Google API response")
		}
	}
	return nil
}

// bucket lists within the configured project: matching labels alone do
// not prove ownership when ADC can also access another GCP project.
func (d *gcsDriver) bucket(ctx context.Context, name string) (*gcsBucket, error) {
	q := url.Values{"project": {d.project}, "prefix": {name}, "maxResults": {"1000"}}
	for {
		var page struct {
			Items []gcsBucket `json:"items"`
			Next  string      `json:"nextPageToken"`
		}
		if err := d.request(ctx, "GET", d.storageURL+"/b", q, nil, &page); err != nil {
			return nil, fmt.Errorf("storage: find bucket %s in GCP project: %w", name, err)
		}
		for _, b := range page.Items {
			if b.Name == name {
				return &b, nil
			}
		}
		if page.Next == "" {
			break
		}
		if page.Next == q.Get("pageToken") {
			return nil, errors.New("storage: bucket pagination did not advance")
		}
		q.Set("pageToken", page.Next)
	}
	// Distinguish a deleted bucket from a global name owned elsewhere,
	// before removing the instance's identity during Delete.
	var b gcsBucket
	err := d.request(ctx, "GET", d.bucketPath(name), nil, nil, &b)
	if gcsStatus(err) == 404 {
		return nil, errNoBucket
	}
	if err == nil || gcsStatus(err) == 403 {
		return nil, fmt.Errorf("%w: bucket %s is not owned by GCP project %s", ErrInUse, name, d.project)
	}
	return nil, fmt.Errorf("storage: read bucket %s: %w", name, err)
}

func (d *gcsDriver) checkBucket(b *gcsBucket, in Instance) error {
	if b == nil {
		return errors.New("storage: empty GCS bucket response")
	}
	if b.Labels[gcsLabelApp] != in.App || b.Labels[gcsLabelInstance] != in.Name {
		return fmt.Errorf("%w: bucket %s does not belong to this app instance", ErrInUse, b.Name)
	}
	if !strings.EqualFold(b.Location, d.region) || (b.LocationType != "" && b.LocationType != "region") {
		return fmt.Errorf("storage: bucket %s is outside single region %s", b.Name, d.region)
	}
	return nil
}

func (d *gcsDriver) readAccount(ctx context.Context, in Instance, email string) (*gcsAccount, error) {
	var a gcsAccount
	err := d.request(ctx, "GET", d.accountPath(email), nil, nil, &a)
	if gcsStatus(err) == 404 {
		return nil, errNoUser
	}
	if err != nil {
		return nil, fmt.Errorf("storage: read GCP service account: %w", err)
	}
	if a.Email != email || a.ProjectID != d.project || a.Description != gcsDescription(in) {
		return nil, fmt.Errorf("%w: GCP service account does not belong to this app instance", ErrInUse)
	}
	return &a, nil
}

func (d *gcsDriver) keys(ctx context.Context, email string) ([]gcsKey, error) {
	q := url.Values{"serviceAccountEmail": {email}, "showDeletedKeys": {"false"}, "maxResults": {"1000"}}
	var keys []gcsKey
	for {
		var page struct {
			Items []gcsKey `json:"items"`
			Next  string   `json:"nextPageToken"`
		}
		if err := d.request(ctx, "GET", d.keyPath(""), q, nil, &page); err != nil {
			return nil, fmt.Errorf("storage: list GCP HMAC keys: %w", err)
		}
		for _, k := range page.Items {
			if k.ServiceAccountEmail != email {
				return nil, fmt.Errorf("%w: HMAC key belongs to another service account", ErrInUse)
			}
			if k.State != "DELETED" {
				keys = append(keys, k)
			}
		}
		if page.Next == "" {
			return keys, nil
		}
		if page.Next == q.Get("pageToken") {
			return nil, errors.New("storage: HMAC key pagination did not advance")
		}
		q.Set("pageToken", page.Next)
	}
}

func (d *gcsDriver) Ensure(ctx context.Context, in Instance, newKey bool, current, old string) (Result, error) {
	n, err := instanceNames(d.prefix, in)
	if err != nil {
		return Result{}, err
	}
	id, email := d.account(in)
	b, bErr := d.bucket(ctx, n.bucket)
	if bErr != nil && !errors.Is(bErr, errNoBucket) {
		return Result{}, bErr
	}
	if bErr == nil {
		if err = d.checkBucket(b, in); err != nil {
			return Result{}, err
		}
	}
	a, aErr := d.readAccount(ctx, in, email)
	if aErr != nil && !errors.Is(aErr, errNoUser) {
		return Result{}, aErr
	}
	if a != nil && a.Disabled {
		return Result{}, errors.New("storage: app's GCP service account is disabled")
	}
	var keys []gcsKey
	if aErr == nil {
		keys, err = d.keys(ctx, email)
		if err != nil {
			return Result{}, err
		}
	}
	wantKey := newKey || a == nil
	if current != "" && !slices.ContainsFunc(keys, func(k gcsKey) bool { return k.AccessID == current && k.State == "ACTIVE" }) {
		wantKey = true
	}
	var del []string
	if wantKey {
		ids := make([]string, len(keys))
		for i, k := range keys {
			ids[i] = k.AccessID
		}
		del, err = keysToDelete(email, ids, current, old)
		if err != nil {
			return Result{}, err
		}
	}
	if bErr != nil {
		security := &gcsBucketIAM{PublicAccessPrevention: "enforced"}
		security.Uniform.Enabled = true
		created := gcsBucket{Name: n.bucket, Location: d.region, Labels: map[string]string{gcsLabelApp: in.App, gcsLabelInstance: in.Name}, IAM: security}
		err = d.request(ctx, "POST", d.storageURL+"/b", url.Values{"project": {d.project}}, created, &b)
		if gcsStatus(err) == 409 {
			b, err = d.bucket(ctx, n.bucket)
		}
		if err != nil {
			return Result{}, fmt.Errorf("storage: create GCS bucket %s: %w", n.bucket, err)
		}
		if err = d.checkBucket(b, in); err != nil {
			return Result{}, err
		}
	}
	if b.IAM == nil || !b.IAM.Uniform.Enabled || b.IAM.PublicAccessPrevention != "enforced" {
		security := &gcsBucketIAM{PublicAccessPrevention: "enforced"}
		security.Uniform.Enabled = true
		if err = d.request(ctx, "PATCH", d.bucketPath(n.bucket), url.Values{"ifMetagenerationMatch": {b.Metageneration}}, gcsBucket{IAM: security}, nil); err != nil {
			return Result{}, fmt.Errorf("storage: make GCS bucket private: %w", err)
		}
	}
	accountCreated := false
	if aErr != nil {
		body := struct {
			ID      string     `json:"accountId"`
			Account gcsAccount `json:"serviceAccount"`
		}{id, gcsAccount{Description: gcsDescription(in), DisplayName: "HopCharlie " + in.Name}}
		err = d.request(ctx, "POST", d.iamURL+"/projects/"+url.PathEscape(d.project)+"/serviceAccounts", nil, body, &a)
		accountCreated = err == nil
		if gcsStatus(err) == 409 {
			a, err = d.readAccount(ctx, in, email)
		}
		if err != nil {
			return Result{}, fmt.Errorf("storage: create GCP service account: %w", err)
		}
		if a == nil {
			return Result{}, errors.New("storage: empty GCP service account response")
		}
	}
	if err = d.bindAccount(ctx, n.bucket, email, true, accountCreated); err != nil {
		return Result{}, err
	}
	result := Result{Bucket: n.bucket}
	if !wantKey {
		return result, nil
	}
	for _, id := range del {
		if err = d.deleteKey(ctx, email, id); err != nil {
			return Result{}, err
		}
	}
	var made struct {
		Metadata gcsKey `json:"metadata"`
		Secret   string `json:"secret"`
	}
	if err = d.request(ctx, "POST", d.keyPath(""), url.Values{"serviceAccountEmail": {email}}, nil, &made); err != nil {
		return Result{}, fmt.Errorf("storage: create GCP HMAC key: %w", err)
	}
	if made.Metadata.AccessID == "" || made.Secret == "" || made.Metadata.ServiceAccountEmail != email || made.Metadata.State != "ACTIVE" {
		return Result{}, errors.New("storage: invalid GCP HMAC create response; retry to remove any orphan key")
	}
	result.Key = &Key{ID: made.Metadata.AccessID, Secret: made.Secret}
	return result, nil
}

// gcsRoles are the roles an instance's service account has on its bucket.
var gcsRoles = []string{"roles/storage.objectAdmin", "roles/storage.legacyBucketReader"}

func (d *gcsDriver) bindAccount(ctx context.Context, bucket, email string, add, accountCreated bool) error {
	member := "serviceAccount:" + email
	roles := gcsRoles
	delay := time.Second
	for attempt := 0; attempt < 3; attempt++ {
		var p gcsPolicy
		if err := d.request(ctx, "GET", d.bucketPath(bucket)+"/iam", url.Values{"optionsRequestedPolicyVersion": {"3"}}, nil, &p); err != nil {
			if !add && gcsStatus(err) == 404 {
				return nil
			}
			return fmt.Errorf("storage: read GCS bucket IAM: %w", err)
		}
		if p.Etag == "" {
			return errors.New("storage: GCS bucket IAM response has no etag")
		}
		changed := false
		for _, role := range roles {
			found := false
			for i := range p.Bindings {
				b := &p.Bindings[i]
				if b.Role != role || (len(b.Condition) > 0 && string(b.Condition) != "null") {
					continue
				}
				found = true
				if add && !slices.Contains(b.Members, member) {
					b.Members = append(b.Members, member)
					changed = true
				}
				if !add && slices.Contains(b.Members, member) {
					b.Members = slices.DeleteFunc(b.Members, func(m string) bool { return m == member })
					changed = true
				}
			}
			if add && !found {
				p.Bindings = append(p.Bindings, gcsBinding{Role: role, Members: []string{member}})
				changed = true
			}
		}
		if !changed {
			return nil
		}
		p.Bindings = slices.DeleteFunc(p.Bindings, func(b gcsBinding) bool { return len(b.Members) == 0 })
		err := d.request(ctx, "PUT", d.bucketPath(bucket)+"/iam", nil, p, nil)
		// New service accounts can take over 60 seconds to become visible to bucket IAM.
		for accountCreated && gcsStatus(err) == 400 && delay <= 64*time.Second {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(delay):
			}
			delay *= 2
			err = d.request(ctx, "PUT", d.bucketPath(bucket)+"/iam", nil, p, nil)
		}
		if gcsStatus(err) == 409 || gcsStatus(err) == 412 {
			continue
		}
		if !add && gcsStatus(err) == 404 {
			return nil
		}
		if err != nil {
			return fmt.Errorf("storage: update GCS bucket IAM: %w", err)
		}
		return nil
	}
	return errors.New("storage: GCS bucket IAM kept changing; retry")
}

func (d *gcsDriver) deleteKey(ctx context.Context, email, id string) error {
	if id == "" {
		return nil
	}
	var k gcsKey
	err := d.request(ctx, "GET", d.keyPath(id), nil, nil, &k)
	if gcsStatus(err) == 404 || (err == nil && k.State == "DELETED") {
		return nil
	}
	if err != nil {
		return fmt.Errorf("storage: read GCP HMAC key %s: %w", MaskKey(id), err)
	}
	if k.AccessID != id || k.ServiceAccountEmail != email {
		return fmt.Errorf("%w: GCP HMAC key does not belong to this app instance", ErrInUse)
	}
	if k.State != "INACTIVE" {
		err = d.request(ctx, "PUT", d.keyPath(id), nil, gcsKey{State: "INACTIVE", Etag: k.Etag}, nil)
		if gcsStatus(err) == 404 {
			return nil
		}
		if err != nil {
			return fmt.Errorf("storage: deactivate GCP HMAC key %s: %w", MaskKey(id), err)
		}
	}
	// State changes can take minutes to propagate. A failed delete leaves
	// the key inactive and is safely resumed by the caller's next retry.
	err = d.request(ctx, "DELETE", d.keyPath(id), nil, nil, nil)
	if err != nil && gcsStatus(err) != 404 {
		return fmt.Errorf("storage: delete inactive GCP HMAC key %s (retry after state propagation): %w", MaskKey(id), err)
	}
	return nil
}

// Keys lists the service account's HMAC keys that are not deleted, with
// their timeCreated. An inactive key is listed too: it is on its way out.
func (d *gcsDriver) Keys(ctx context.Context, in Instance) ([]KeyInfo, error) {
	if _, err := instanceNames(d.prefix, in); err != nil {
		return nil, err
	}
	_, email := d.account(in)
	keys, err := d.keys(ctx, email)
	if err != nil {
		return nil, err
	}
	out := make([]KeyInfo, 0, len(keys))
	for _, k := range keys {
		info := KeyInfo{ID: k.AccessID, Active: k.State == "ACTIVE"}
		if t, err := time.Parse(time.RFC3339, k.TimeCreated); err == nil {
			info.CreatedAt = t.UTC()
		}
		out = append(out, info)
	}
	return out, nil
}

// Scope is the service account's roles on the bucket.
func (d *gcsDriver) Scope() Scope { return Scope{Actions: slices.Clone(gcsRoles)} }

func (d *gcsDriver) DeleteKey(ctx context.Context, in Instance, id string) error {
	if _, err := instanceNames(d.prefix, in); err != nil {
		return err
	}
	_, email := d.account(in)
	_, err := d.readAccount(ctx, in, email)
	if errors.Is(err, errNoUser) {
		return nil
	}
	if err != nil {
		return err
	}
	return d.deleteKey(ctx, email, id)
}

func (d *gcsDriver) Delete(ctx context.Context, in Instance) error {
	n, err := instanceNames(d.prefix, in)
	if err != nil {
		return err
	}
	_, email := d.account(in)
	b, bErr := d.bucket(ctx, n.bucket)
	if bErr != nil && !errors.Is(bErr, errNoBucket) {
		return bErr
	}
	if bErr == nil {
		if err = d.checkBucket(b, in); err != nil {
			return err
		}
	}
	_, aErr := d.readAccount(ctx, in, email)
	if aErr != nil && !errors.Is(aErr, errNoUser) {
		return aErr
	}
	if aErr == nil {
		keys, err := d.keys(ctx, email)
		if err != nil {
			return err
		}
		for _, k := range keys {
			if err = d.deleteKey(ctx, email, k.AccessID); err != nil {
				return err
			}
		}
	}
	if bErr == nil {
		if err = d.bindAccount(ctx, n.bucket, email, false, false); err != nil {
			return err
		}
	}
	if aErr == nil {
		err = d.request(ctx, "DELETE", d.accountPath(email), nil, nil, nil)
		if err != nil && gcsStatus(err) != 404 {
			return fmt.Errorf("storage: delete GCP service account: %w", err)
		}
	}
	if bErr != nil {
		return nil
	}
	if err = d.emptyBucket(ctx, n.bucket); err != nil {
		return err
	}
	// Recheck labels before the final delete, then bind it to that exact
	// metadata generation so a concurrent ownership change cannot win.
	b, err = d.bucket(ctx, n.bucket)
	if errors.Is(err, errNoBucket) {
		return nil
	}
	if err != nil {
		return err
	}
	if err = d.checkBucket(b, in); err != nil {
		return err
	}
	err = d.request(ctx, "DELETE", d.bucketPath(n.bucket), url.Values{"ifMetagenerationMatch": {b.Metageneration}}, nil, nil)
	if err != nil && gcsStatus(err) != 404 {
		return fmt.Errorf("storage: delete GCS bucket %s: %w", n.bucket, err)
	}
	return nil
}

func (d *gcsDriver) emptyBucket(ctx context.Context, bucket string) error {
	q := url.Values{"versions": {"true"}, "maxResults": {"1000"}}
	for {
		var page struct {
			Items []gcsObject `json:"items"`
			Next  string      `json:"nextPageToken"`
		}
		err := d.request(ctx, "GET", d.bucketPath(bucket)+"/o", q, nil, &page)
		if gcsStatus(err) == 404 {
			return nil
		}
		if err != nil {
			return fmt.Errorf("storage: list GCS object generations: %w", err)
		}
		for _, o := range page.Items {
			if o.Name == "" || o.Generation == "" {
				return errors.New("storage: GCS object has no name or generation")
			}
			err = d.request(ctx, "DELETE", d.bucketPath(bucket)+"/o/"+url.PathEscape(o.Name), url.Values{"generation": {o.Generation}, "ifGenerationMatch": {o.Generation}}, nil, nil)
			if err != nil && gcsStatus(err) != 404 {
				return fmt.Errorf("storage: delete GCS object generation: %w", err)
			}
		}
		if page.Next == "" {
			return nil
		}
		if page.Next == q.Get("pageToken") {
			return errors.New("storage: GCS object pagination did not advance")
		}
		q.Set("pageToken", page.Next)
	}
}
