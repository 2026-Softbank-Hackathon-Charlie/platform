package onprem

import (
	"bytes"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"regexp"
	"strconv"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const clusterSecretAdmissionActor = "system:serviceaccount:charlie-platform:hopcharlie-platform-api"

var clusterSecretAdmissionName = regexp.MustCompile(`^hop-onprem-([a-f0-9]{24})$`)
var clusterSecretAdmissionUID = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)
var clusterSecretAdmissionVersion = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)
var clusterSecretAdmissionToken = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidateClusterSecretAdmission performs pure validation of the registrar's
// narrow Argo cluster Secret contract. It never contacts a cluster, reads a
// store, mutates an object, or includes submitted credentials in its response.
// The public endpoint does not authenticate its caller: UserInfo is validated
// request metadata, not proof of caller identity. A direct public caller obtains
// only this side-effect-free predicate result. The API server authenticates the
// webhook's HTTPS server and correlates its response by AdmissionReview UID.
func ValidateClusterSecretAdmission(review admissionv1.AdmissionReview, now time.Time) admissionv1.AdmissionReview {
	out := admissionv1.AdmissionReview{TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"}, Response: &admissionv1.AdmissionResponse{Allowed: false, Result: &metav1.Status{Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: 403, Message: "cluster registration Secret denied"}}}
	req := review.Request
	if req == nil {
		return out
	}
	out.Response.UID = req.UID
	if review.APIVersion != "admission.k8s.io/v1" || review.Kind != "AdmissionReview" || req.UID == "" || req.UserInfo.Username != clusterSecretAdmissionActor || req.Namespace != "argocd" || req.SubResource != "" || req.Resource.Group != "" || req.Resource.Version != "v1" || req.Resource.Resource != "secrets" || req.Kind.Group != "" || req.Kind.Version != "v1" || req.Kind.Kind != "Secret" || (req.Operation != admissionv1.Create && req.Operation != admissionv1.Update) {
		return out
	}
	next, ok := decodeClusterSecretAdmission(req.Object.Raw, now, false)
	if !ok || req.Name != next.secret.Name {
		return out
	}
	if req.Operation == admissionv1.Update {
		old, valid := decodeClusterSecretAdmission(req.OldObject.Raw, now, true)
		if !valid {
			return out
		}
		if old.secret.UID != "" && next.secret.UID != old.secret.UID {
			return out
		}
		if next.secret.Name != old.secret.Name || next.secret.Namespace != old.secret.Namespace || next.secret.Labels[registrationOwnerLabel] != old.secret.Labels[registrationOwnerLabel] || next.secret.Labels[registrationUIDLabel] != old.secret.Labels[registrationUIDLabel] || !bytes.Equal(next.secret.Data["name"], old.secret.Data["name"]) || !bytes.Equal(next.secret.Data["server"], old.secret.Data["server"]) || next.caData != old.caData || next.version < old.version || next.expires.Before(old.expires) || (next.version == old.version && !bytes.Equal(next.secret.Data["config"], old.secret.Data["config"])) {
			return out
		}
	}
	out.Response.Allowed = true
	out.Response.Result = nil
	return out
}

type decodedClusterSecretAdmission struct {
	secret  corev1.Secret
	caData  string
	version int64
	expires time.Time
}

func decodeClusterSecretAdmission(raw []byte, now time.Time, allowExpired bool) (decodedClusterSecretAdmission, bool) {
	var out decodedClusterSecretAdmission
	if len(raw) == 0 || len(raw) > 64<<10 || json.Unmarshal(raw, &out.secret) != nil {
		return out, false
	}
	s := &out.secret
	match := clusterSecretAdmissionName.FindStringSubmatch(s.Name)
	if len(match) != 2 || s.Namespace != "argocd" || s.Type != corev1.SecretTypeOpaque || len(s.StringData) != 0 || len(s.Data) != 4 || s.Labels[registrationOwnerLabel] != "enr_"+match[1] || !clusterSecretAdmissionUID.MatchString(s.Labels[registrationUIDLabel]) || s.Labels["argocd.argoproj.io/secret-type"] != "cluster" {
		return out, false
	}
	if string(s.Data["name"]) != s.Name || string(s.Data["server"]) != "https://hop-k3s-"+match[1]+".hopcharlie-db-network.svc.cluster.local:6443" || string(s.Data["clusterResources"]) != "true" {
		return out, false
	}
	for _, k := range []string{"name", "server", "config", "clusterResources"} {
		if _, ok := s.Data[k]; !ok {
			return out, false
		}
	}
	version := s.Annotations["hopcharlie.dev/registration-version"]
	if !clusterSecretAdmissionVersion.MatchString(version) {
		return out, false
	}
	var err error
	out.version, err = strconv.ParseInt(version, 10, 64)
	if err != nil {
		return out, false
	}
	expiry := s.Annotations["hopcharlie.dev/token-expires-at"]
	out.expires, err = time.Parse(time.RFC3339, expiry)
	if err != nil || out.expires.UTC().Format(time.RFC3339) != expiry || out.expires.After(now.Add(2*time.Hour)) || (!allowExpired && !out.expires.After(now)) {
		return out, false
	}
	// json.Unmarshal rejects trailing JSON. Re-marshalling a map recursively
	// canonicalizes its keys, rejecting duplicates, whitespace, and hidden fields
	// when compared with the exact producer's canonical encoding.
	var cfg map[string]any
	if json.Unmarshal(s.Data["config"], &cfg) != nil || len(cfg) != 2 {
		return out, false
	}
	canonical, err := json.Marshal(cfg)
	if err != nil || !bytes.Equal(canonical, s.Data["config"]) {
		return out, false
	}
	token, ok := cfg["bearerToken"].(string)
	if !ok || len(token) < 20 || len(token) > 16<<10 || !clusterSecretAdmissionToken.MatchString(token) {
		return out, false
	}
	tls, ok := cfg["tlsClientConfig"].(map[string]any)
	if !ok || len(tls) != 3 {
		return out, false
	}
	insecure, ok := tls["insecure"].(bool)
	if !ok || insecure {
		return out, false
	}
	serverName, ok := tls["serverName"].(string)
	if !ok || serverName != "kubernetes.default.svc" {
		return out, false
	}
	out.caData, ok = tls["caData"].(string)
	if !ok || !validClusterSecretAdmissionCA(out.caData) {
		return out, false
	}
	return out, true
}

func validClusterSecretAdmissionCA(encoded string) bool {
	ca, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(ca) == 0 || len(ca) > 24<<10 || base64.StdEncoding.EncodeToString(ca) != encoded {
		return false
	}
	count := 0
	for len(bytes.TrimSpace(ca)) != 0 {
		ca = bytes.TrimSpace(ca)
		if !bytes.HasPrefix(ca, []byte("-----BEGIN CERTIFICATE-----")) {
			return false
		}
		block, rest := pem.Decode(ca)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return false
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA {
			return false
		}
		count++
		ca = rest
	}
	return count > 0
}
