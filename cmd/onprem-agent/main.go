// onprem-agent renews delegated Kubernetes credentials from inside the enrolled
// cluster. It never reads or transmits the administrator's kubeconfig.
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type connectionRequest struct {
	ClusterUID     string    `json:"clusterUID"`
	CAData         string    `json:"caData"`
	PlatformToken  string    `json:"platformToken"`
	ArgoToken      string    `json:"argoToken"`
	TokenExpiresAt time.Time `json:"tokenExpiresAt"`
}
type credentialIssuer interface {
	Issue(context.Context) (connectionRequest, error)
}
type kubeIssuer struct {
	client                            kubernetes.Interface
	namespace, platform, argo, caData string
}

func (k *kubeIssuer) Issue(ctx context.Context) (connectionRequest, error) {
	ns, err := k.client.CoreV1().Namespaces().Get(ctx, "kube-system", metav1.GetOptions{})
	if err != nil || ns.UID == "" {
		return connectionRequest{}, errors.New("cluster identity could not be observed")
	}
	seconds := int64(3600)
	get := func(name string) (string, time.Time, error) {
		r, err := k.client.CoreV1().ServiceAccounts(k.namespace).CreateToken(ctx, name, &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &seconds}}, metav1.CreateOptions{})
		if err != nil || r.Status.Token == "" || time.Until(r.Status.ExpirationTimestamp.Time) < 10*time.Minute {
			return "", time.Time{}, errors.New("delegated short-lived credential could not be issued")
		}
		return r.Status.Token, r.Status.ExpirationTimestamp.Time, nil
	}
	platform, expiry, err := get(k.platform)
	if err != nil {
		return connectionRequest{}, err
	}
	argo, argoExpiry, err := get(k.argo)
	if err != nil {
		return connectionRequest{}, err
	}
	if argoExpiry.Before(expiry) {
		expiry = argoExpiry
	}
	return connectionRequest{ClusterUID: string(ns.UID), CAData: k.caData, PlatformToken: platform, ArgoToken: argo, TokenExpiresAt: expiry}, nil
}

type statusError struct{ status int }

func (e statusError) Error() string {
	return fmt.Sprintf("registration server returned HTTP %d", e.status)
}
func permanent(err error) bool {
	var e statusError
	return errors.As(err, &e) && e.status >= 400 && e.status < 500 && e.status != 408 && e.status != 429
}

type agent struct {
	origin, id string
	client     *http.Client
	issuer     credentialIssuer
	token      func() (string, error)
	ready      bool
}

func (a *agent) heartbeat(ctx context.Context) error {
	payload, err := a.issuer.Issue(ctx)
	if err != nil {
		return err
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return errors.New("credential request could not be encoded")
	}
	token, err := a.token()
	if err != nil {
		return errors.New("connection credential is unavailable")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.origin+"/v1/onprem/enrollments/"+a.id+"/connection", bytes.NewReader(data))
	if err != nil {
		return errors.New("registration request is invalid")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.client.Do(req)
	if err != nil {
		return errors.New("registration server is unreachable")
	}
	defer resp.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if readErr != nil || len(body) > 1<<20 {
		return errors.New("registration response is unavailable")
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return statusError{resp.StatusCode}
	}
	var result struct {
		Enrollment struct {
			Status string `json:"status"`
		} `json:"enrollment"`
	}
	a.ready = json.Unmarshal(body, &result) == nil && result.Enrollment.Status == "ready"
	return nil
}
func (a *agent) run(ctx context.Context) error {
	delay := time.Duration(0)
	backoff := 5 * time.Second
	for {
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		attempt, cancel := context.WithTimeout(ctx, 2*time.Minute+15*time.Second)
		err := a.heartbeat(attempt)
		cancel()
		if err == nil {
			log.Print("connection credentials renewed; server verification remains authoritative")
			delay = 15 * time.Second
			if a.ready {
				delay = 5 * time.Minute
			}
			backoff = 5 * time.Second
			continue
		}
		if permanent(err) {
			return err
		}
		log.Print("connection renewal failed; retrying without changing the registered cluster")
		delay = backoff
		if backoff < time.Minute {
			backoff *= 2
			if backoff > time.Minute {
				backoff = time.Minute
			}
		}
	}
}

var enrollmentID = regexp.MustCompile(`^enr_[a-f0-9]{24}$`)
var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{32,512}$`)

func validOrigin(value string) bool {
	u, e := url.Parse(value)
	return e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == "" && (u.Path == "" || u.Path == "/") && !strings.ContainsAny(value, "\r\n\t ")
}
func main() {
	if err := start(); err != nil && !errors.Is(err, context.Canceled) {
		log.Print("connector stopped: ", err)
		os.Exit(1)
	}
}
func start() error {
	origin, id := strings.TrimRight(os.Getenv("SERVER"), "/"), os.Getenv("ENROLLMENT_ID")
	if !validOrigin(origin) || !enrollmentID.MatchString(id) {
		return errors.New("invalid server or enrollment configuration")
	}
	name := "hop-" + strings.TrimPrefix(id, "enr_")
	if os.Getenv("PLATFORM_SERVICE_ACCOUNT") != name+"-platform" || os.Getenv("ARGO_SERVICE_ACCOUNT") != name+"-argo" {
		return errors.New("delegated identities do not match this enrollment")
	}
	tokenFile := os.Getenv("CONNECTION_TOKEN_FILE")
	if tokenFile != "/connection/token" {
		return errors.New("connection credential path is invalid")
	}
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return errors.New("in-cluster identity is unavailable")
	}
	cfg.Timeout = 15 * time.Second
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return errors.New("in-cluster client could not be initialized")
	}
	ca, err := os.ReadFile(cfg.TLSClientConfig.CAFile)
	if err != nil {
		return errors.New("cluster certificate authority is unavailable")
	}
	namespace, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
	if err != nil || strings.TrimSpace(string(namespace)) != "charlie-system" {
		return errors.New("connector must run in its protected namespace")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	httpClient := &http.Client{Transport: transport, Timeout: 2*time.Minute + 15*time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("registration redirects are disabled") }}
	a := agent{origin: origin, id: id, client: httpClient, issuer: &kubeIssuer{client: client, namespace: "charlie-system", platform: name + "-platform", argo: name + "-argo", caData: base64.StdEncoding.EncodeToString(ca)}, token: func() (string, error) {
		b, e := os.ReadFile(tokenFile)
		v := strings.TrimSpace(string(b))
		if e != nil || !tokenPattern.MatchString(v) {
			return "", errors.New("invalid connection credential")
		}
		return v, nil
	}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return a.run(ctx)
}
