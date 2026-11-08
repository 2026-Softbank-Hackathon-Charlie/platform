package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/gateway"
)

const exclusivePoll = 100 * time.Millisecond

// A timed-out drain must retain a strong reference: os.File's finalizer must
// never release ownership while an old write handler can still run.
var retainedStateLock atomic.Pointer[stateLock]

// Both pods relay to one lock owner. No standby restores a potentially stale
// snapshot or admits writes independently. This is SAME-NODE pod failover,
// not a distributed lease: all processes must share one local filesystem.
func runExclusive(ctx context.Context, cfg gateway.Config, publicAddr, adminAddr string, log *slog.Logger) error {
	if cfg.AIDemo != nil || cfg.AIQuality != nil || cfg.AIFollow != nil {
		return errors.New("exclusive state is for the app traffic gateway; the dedicated AI gateway must remain single-instance")
	}
	if cfg.StateFile == "" {
		return errors.New("exclusive state requires a state file")
	}
	state, err := filepath.Abs(cfg.StateFile)
	if err != nil {
		return err
	}
	cfg.StateFile = state
	lock, err := openStateLock(state)
	if err != nil {
		return err
	}
	// Keep the lock open until process exit if drain fails. Releasing it while
	// old handlers can still dispatch would permit overlapping owners.
	release := true
	defer func() {
		if release {
			lock.Close()
		}
		runtime.KeepAlive(lock)
	}()
	publicSocket, adminSocket := state+".public.sock", state+".admin.sock"
	if len(publicSocket) >= 108 || len(adminSocket) >= 108 {
		return errors.New("exclusive state path is too long for Unix sockets")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	failures := make(chan error, 4)
	start := func(network, address string, handler http.Handler) (*http.Server, error) {
		ln, err := net.Listen(network, address)
		if err != nil {
			return nil, err
		}
		if network == "unix" {
			// The shared non-root gateway uid alone may use the control socket.
			if err := os.Chmod(address, 0600); err != nil {
				ln.Close()
				return nil, err
			}
		}
		s := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
		go func() {
			if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				failures <- err
				cancel()
			}
		}()
		return s, nil
	}
	publicRelay, publicTransport := unixRelay(publicSocket)
	adminRelay, adminTransport := unixRelay(adminSocket)
	defer publicTransport.CloseIdleConnections()
	defer adminTransport.CloseIdleConnections()
	public, err := start("tcp", publicAddr, publicRelay)
	if err != nil {
		return err
	}
	defer public.Close()
	admin, err := start("tcp", adminAddr, adminRelay)
	if err != nil {
		return err
	}
	defer admin.Close()
	log.Info("gateway exclusive relay listening; waiting for state ownership", "listen", publicAddr, "adminAddr", adminAddr)
	ticker := time.NewTicker(exclusivePoll)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return exclusiveFailure(failures)
		}
		ok, err := lock.TryLock()
		if err != nil {
			return fmt.Errorf("acquire exclusive state: %w", err)
		}
		if ok {
			break
		}
		select {
		case <-ctx.Done():
			return exclusiveFailure(failures)
		case <-ticker.C:
		}
	}
	// No other owner can update the JSON or sockets until this process has
	// drained and released the fixed lock inode (or the kernel kills it).
	unclean, err := markOwnerRunning(state)
	if err != nil {
		return err
	}
	gw, err := gateway.New(cfg)
	if err != nil {
		return fmt.Errorf("restore exclusive gateway: %w", err)
	}
	if unclean {
		if err := gw.HoldAfterOwnerLoss(); err != nil {
			return fmt.Errorf("persist owner recovery holds: %w", err)
		}
		log.Error("previous owner exit was unclean; writes and control changes require operator recovery")
	}
	for _, socket := range []string{publicSocket, adminSocket} {
		if st, err := os.Lstat(socket); err == nil {
			if st.Mode()&os.ModeSocket == 0 {
				return errors.New("exclusive socket path contains a non-socket file")
			}
			if err := os.Remove(socket); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	var accepting atomic.Bool
	var admission sync.Mutex
	var active sync.WaitGroup
	guard := func(next http.Handler, admin bool) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			admission.Lock()
			if !accepting.Load() {
				admission.Unlock()
				unavailable(w)
				return
			}
			active.Add(1)
			admission.Unlock()
			defer active.Done()
			if unclean {
				w.Header().Set("X-Hopcharlie-Recovery", "required")
				// A GET Upgrade is a bidirectional write channel. Admin state and
				// metrics also cannot attest that reset totals mean zero losses.
				blocked := hasUpgrade(r) || !readMethod(r.Method)
				if admin {
					blocked = blocked || r.URL.Path != "/healthz"
				}
				if blocked {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("Retry-After", "1")
					w.WriteHeader(http.StatusServiceUnavailable)
					_, _ = w.Write([]byte("{\"error\":\"owner_recovery_required\"}\n"))
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
	ownerPublic, err := start("unix", publicSocket, guard(gw.Proxy(), false))
	if err != nil {
		return err
	}
	defer ownerPublic.Close()
	ownerAdmin, err := start("unix", adminSocket, guard(gw.Admin(), true))
	if err != nil {
		return err
	}
	defer ownerAdmin.Close()
	healthCtx, stopHealth := context.WithCancel(context.Background())
	go gw.Run(healthCtx)
	accepting.Store(true)
	log.Info("gateway owns exclusive state; restored routing and holds")
	<-ctx.Done()
	admission.Lock()
	accepting.Store(false)
	admission.Unlock()
	gw.BeginShutdown()
	// Close owner listeners before waiting. Relay pods now fail closed, and
	// cannot reach a half-drained owner through a cached keep-alive connection.
	drained := true
	for _, s := range []*http.Server{public, ownerPublic, admin, ownerAdmin} {
		drainCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		if err := s.Shutdown(drainCtx); err != nil {
			drained = false
			log.Error("exclusive gateway drain incomplete; lock retained until process exit")
		}
		done()
	}
	// http.Server.Shutdown does not wait for hijacked connections (WebSocket
	// upgrades). Their owner handlers must also finish before ownership moves.
	finished := make(chan struct{})
	go func() { active.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		drained = false
		log.Error("exclusive owner still has active or upgraded requests; lock retained until process exit")
	}
	stopHealth()
	if drained {
		if err := gw.Close(); err != nil {
			return fmt.Errorf("persist final owner state: %w", err)
		}
		if !unclean {
			if err := clearOwnerRunning(state); err != nil {
				return err
			}
		}
	} else {
		release = false
		retainedStateLock.Store(lock)
	}
	return exclusiveFailure(failures)
}

func readMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions
}

func hasUpgrade(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return true
	}
	for _, line := range r.Header.Values("Connection") {
		for _, token := range strings.Split(line, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

// The marker precedes ANY serving. Removing it is allowed only after a clean
// drain and a durable final snapshot. Existing markers are never overwritten.
func markOwnerRunning(state string) (bool, error) {
	path := state + ".running"
	if st, err := os.Lstat(path); err == nil {
		if !st.Mode().IsRegular() {
			return false, errors.New("owner recovery marker must be a regular file")
		}
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false, err
	}
	if _, err := f.WriteString("owner exit may leave writes with unknown outcomes\n"); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return false, err
	}
	if err := f.Close(); err != nil {
		return false, err
	}
	return false, syncOwnerDirectory(filepath.Dir(state))
}

func clearOwnerRunning(state string) error {
	if err := os.Remove(state + ".running"); err != nil {
		return err
	}
	return syncOwnerDirectory(filepath.Dir(state))
}

func syncOwnerDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func exclusiveFailure(failures <-chan error) error {
	select {
	case err := <-failures:
		return err
	default:
		return nil
	}
}

func unixRelay(socket string) (http.Handler, *http.Transport) {
	transport := &http.Transport{
		// A fresh connection per request also prevents net/http's automatic
		// retry on a reused connection, including writes with Idempotency-Key.
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
		},
	}
	proxy := &httputil.ReverseProxy{
		Director:      func(r *http.Request) { r.URL.Scheme = "http"; r.URL.Host = "exclusive-gateway.local" },
		Transport:     transport,
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			unavailable(w)
		},
	}
	return proxy, transport
}

func unavailable(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Retry-After", "1")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte("{\"error\":\"gateway_unavailable\"}\n"))
}
