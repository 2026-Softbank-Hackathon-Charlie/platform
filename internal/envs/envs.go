package envs

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/sealing"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/storage"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/apiv1"
)

// ErrNotFound means no environment has the requested name.
var ErrNotFound = errors.New("environment not found")

// Errors of Sealer and Storage for an environment that lacks the setting.
var (
	ErrSealingNotConfigured = errors.New("the environment has no sealedSecretsCert")
	ErrStorageNotConfigured = errors.New("the environment has no storage driver")
)

// Defaults for Options.
const (
	DefaultProbeTimeout = 3 * time.Second
	DefaultCacheTTL     = 5 * time.Second
)

// Prober reads one environment's cluster status. Failures are reported in the
// status (reachable=false, error), never invented away. The Service sets
// CheckedAt, so probers need not.
type Prober interface {
	Probe(ctx context.Context) apiv1.EnvironmentStatus
}

// Options tunes a Service. Zero values select the defaults.
type Options struct {
	// ProbeTimeout bounds one environment's probe, including building its clients (default 3s).
	ProbeTimeout time.Duration
	// CacheTTL is how long a probe result is served before probing again (default 5s).
	CacheTTL time.Duration
	// Now is the clock for checkedAt and cache expiry (default time.Now in UTC).
	Now func() time.Time
	// NewProber builds an environment's prober on its first probe. Nil uses
	// NewKubeProber (client-go); tests inject fakes.
	NewProber func(EnvConfig) (Prober, error)
	// NewStorage builds the storage driver of an environment with a storage
	// block, in New. Nil uses storage.New with the process environment;
	// tests inject fakes.
	NewStorage func(storage.Config) (storage.Driver, error)
}

// Service is the environment registry. It is safe for concurrent use.
type Service struct {
	envs      []*env
	byName    map[string]*env
	timeout   time.Duration
	ttl       time.Duration
	now       func() time.Time
	newProber func(EnvConfig) (Prober, error)
}

// env is one environment and its cached status.
type env struct {
	cfg        EnvConfig
	storage    storage.Driver // nil without a storage block or when it could not be built
	storageErr error          // why the storage block's driver could not be built

	sealMu   sync.Mutex
	sealer   *sealing.Sealer // nil until the certificate is read
	certMod  time.Time       // the file's modification time and size when read
	certSize int64
	certHash [sha256.Size]byte // of the file's content

	buildMu sync.Mutex
	prober  Prober // built on first use; a failed build is retried on the next probe

	mu      sync.Mutex
	status  apiv1.EnvironmentStatus
	expires time.Time // zero until the first probe finishes
	flight  *flight   // the probe in progress, shared by concurrent callers
}

// flight is one probe shared by every caller that asked while it ran.
type flight struct {
	done   chan struct{}
	status apiv1.EnvironmentStatus // set before done is closed
}

// New validates cfg and returns a Service. It does not contact any cluster:
// clients are built on the first probe, so the server can start before the
// clusters exist. It builds the storage drivers, which do not contact the
// storage either. A driver that cannot be built (e.g. the minio admin key is
// not set, or no Google credentials for gcs) does not stop New: Storage
// returns that error for its environment, so one environment's credentials
// do not keep the whole server down.
func New(cfg Config, opt Options) (*Service, error) {
	cfg, err := cfg.normalize()
	if err != nil {
		return nil, err
	}
	s := &Service{
		byName:    map[string]*env{},
		timeout:   opt.ProbeTimeout,
		ttl:       opt.CacheTTL,
		now:       opt.Now,
		newProber: opt.NewProber,
	}
	if s.timeout <= 0 {
		s.timeout = DefaultProbeTimeout
	}
	if s.ttl <= 0 {
		s.ttl = DefaultCacheTTL
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.newProber == nil {
		timeout := s.timeout
		s.newProber = func(c EnvConfig) (Prober, error) { return NewKubeProber(c, timeout) }
	}
	newStorage := opt.NewStorage
	if newStorage == nil {
		newStorage = func(c storage.Config) (storage.Driver, error) { return storage.New(context.Background(), c, os.Getenv) }
	}
	for _, c := range cfg.Environments {
		e := &env{cfg: c}
		if c.Storage != nil {
			d, err := newStorage(*c.Storage)
			if err != nil {
				e.storageErr = fmt.Errorf("environment %q: %w", c.Name, err)
			} else {
				e.storage = d
			}
		}
		s.envs = append(s.envs, e)
		s.byName[c.Name] = e
	}
	return s, nil
}

// Sealer returns the sealer for an environment's sealed-secrets controller.
// Every call checks the certificate file and reads it again when its
// modification time or size changed; a new sealer is made only when the
// content changed. So a certificate added or replaced later is used
// without a restart. A file that is missing, cannot be read, is not a
// certificate, has expired or has a short key is an error, also when an
// earlier version was read. ErrNotFound for an unknown environment;
// ErrSealingNotConfigured when it has no sealedSecretsCert; an error
// wrapping fs.ErrNotExist when the file is not there.
func (s *Service) Sealer(name string) (*sealing.Sealer, error) {
	e, ok := s.byName[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if e.cfg.SealedSecretsCert == "" {
		return nil, fmt.Errorf("environment %s: %w", name, ErrSealingNotConfigured)
	}
	e.sealMu.Lock()
	defer e.sealMu.Unlock()
	path := e.cfg.SealedSecretsCert
	// Like the kube clients' errors, these name no file paths.
	fileErr := func(err error) error {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("environment %s: sealed-secrets certificate file not found: %w", name, fs.ErrNotExist)
		}
		return fmt.Errorf("environment %s: sealed-secrets certificate file could not be read", name)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fileErr(err)
	}
	if e.sealer != nil && fi.ModTime().Equal(e.certMod) && fi.Size() == e.certSize {
		return e.sealer, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fileErr(err)
	}
	sum := sha256.Sum256(data)
	if e.sealer == nil || sum != e.certHash {
		sl, err := sealing.ParseCert(data)
		if err != nil {
			return nil, fmt.Errorf("environment %s: %w", name, err)
		}
		e.sealer, e.certHash = sl, sum
	}
	e.certMod, e.certSize = fi.ModTime(), fi.Size()
	return e.sealer, nil
}

// Storage returns an environment's storage driver. ErrNotFound for an
// unknown environment; ErrStorageNotConfigured when it has no storage
// block; the error of New when its driver could not be built.
func (s *Service) Storage(name string) (storage.Driver, error) {
	e, ok := s.byName[name]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if e.storageErr != nil {
		return nil, e.storageErr
	}
	if e.storage == nil {
		return nil, fmt.Errorf("environment %s: %w", name, ErrStorageNotConfigured)
	}
	return e.storage, nil
}

// List returns every environment in configuration order. Environments are
// probed concurrently, so a slow one delays the list by at most ProbeTimeout.
// Live is always false; the API layer sets it from the gateway.
func (s *Service) List(ctx context.Context) []apiv1.Environment {
	out := make([]apiv1.Environment, len(s.envs))
	var wg sync.WaitGroup
	for i, e := range s.envs {
		wg.Go(func() { out[i] = e.environment(s.status(ctx, e)) })
	}
	wg.Wait()
	return out
}

// Get returns one environment with its status, or ErrNotFound.
func (s *Service) Get(ctx context.Context, name string) (apiv1.Environment, error) {
	e, ok := s.byName[name]
	if !ok {
		return apiv1.Environment{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return e.environment(s.status(ctx, e)), nil
}

// Names returns the environment names in configuration order.
func (s *Service) Names() []string {
	out := make([]string, len(s.envs))
	for i, e := range s.envs {
		out[i] = e.cfg.Name
	}
	return out
}

// Config returns an environment's configuration with defaults applied.
func (s *Service) Config(name string) (EnvConfig, bool) {
	e, ok := s.byName[name]
	if !ok {
		return EnvConfig{}, false
	}
	return e.cfg, true
}

// status returns the cached status, or joins (or starts) the probe in
// progress. The returned status shares slices with the cache: read only.
func (s *Service) status(ctx context.Context, e *env) apiv1.EnvironmentStatus {
	e.mu.Lock()
	if s.now().Before(e.expires) {
		st := e.status
		e.mu.Unlock()
		return st
	}
	f := e.flight
	if f == nil {
		f = &flight{done: make(chan struct{})}
		e.flight = f
		go s.probe(e, f)
	}
	e.mu.Unlock()

	select {
	case <-f.done:
		return f.status
	case <-ctx.Done():
		// The probe keeps running for the other callers and the cache.
		st := failed("status check cancelled: " + ctx.Err().Error())
		st.CheckedAt = s.now()
		return st
	}
}

// probe runs one probe detached from the callers, so a caller giving up does
// not fail the result shared with the others, then publishes it.
func (s *Service) probe(e *env, f *flight) {
	st := s.probeWithTimeout(e)
	st.CheckedAt = s.now()
	e.mu.Lock()
	e.status, e.expires, e.flight = st, st.CheckedAt.Add(s.ttl), nil
	e.mu.Unlock()
	f.status = st
	close(f.done)
}

// probeWithTimeout returns the prober's status, or a failure if it has not
// answered within ProbeTimeout. A prober that ignores ctx is left to finish
// in the background.
func (s *Service) probeWithTimeout(e *env) apiv1.EnvironmentStatus {
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	defer cancel()
	res := make(chan apiv1.EnvironmentStatus, 1)
	go func() {
		p, err := e.getProber(s.newProber)
		if err != nil {
			res <- failed(err.Error())
			return
		}
		res <- p.Probe(ctx)
	}()
	select {
	case st := <-res:
		return st
	case <-ctx.Done():
		return failed(fmt.Sprintf("no answer within %s", s.timeout))
	}
}

func (e *env) getProber(build func(EnvConfig) (Prober, error)) (Prober, error) {
	e.buildMu.Lock()
	defer e.buildMu.Unlock()
	if e.prober == nil {
		p, err := build(e.cfg)
		if err != nil {
			return nil, err
		}
		e.prober = p
	}
	return e.prober, nil
}

func (e *env) environment(st apiv1.EnvironmentStatus) apiv1.Environment {
	out := apiv1.Environment{
		Name:           e.cfg.Name,
		Provider:       apiv1.Provider(e.cfg.Provider),
		DisplayName:    e.cfg.DisplayName,
		GatewayBackend: e.cfg.GatewayBackend,
		KubeContext:    e.cfg.KubeContext,
		Status:         st,
	}
	if e.cfg.Region != "" {
		region := e.cfg.Region
		out.Region = &region
	}
	return out
}

// failed is the status of an environment that could not be read.
func failed(msg string) apiv1.EnvironmentStatus {
	return apiv1.EnvironmentStatus{Reachable: false, Error: &msg}
}
