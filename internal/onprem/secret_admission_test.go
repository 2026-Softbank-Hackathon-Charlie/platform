package onprem

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"strings"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
)

func secretAdmissionCA(t *testing.T, now time.Time) string {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "disposable admission test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func secretAdmissionFixture(t *testing.T, now time.Time) *corev1.Secret {
	t.Helper()
	suffix := "0123456789abcdef01234567"
	name := "hop-onprem-" + suffix
	config, _ := json.Marshal(map[string]any{"bearerToken": strings.Repeat("x", 40) + ".jwt.signature", "tlsClientConfig": map[string]any{"caData": secretAdmissionCA(t, now), "insecure": false, "serverName": "kubernetes.default.svc"}})
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "argocd", Labels: map[string]string{"argocd.argoproj.io/secret-type": "cluster", registrationOwnerLabel: "enr_" + suffix, registrationUIDLabel: "11111111-1111-4111-8111-111111111111"}, Annotations: map[string]string{"hopcharlie.dev/registration-version": "1", "hopcharlie.dev/token-expires-at": now.Add(time.Hour).Format(time.RFC3339)}}, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{"name": []byte(name), "server": []byte("https://hop-k3s-" + suffix + ".hopcharlie-db-network.svc.cluster.local:6443"), "config": config, "clusterResources": []byte("true")}}
}

func secretAdmissionReview(t *testing.T, next, old *corev1.Secret) admissionv1.AdmissionReview {
	t.Helper()
	data, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	r := &admissionv1.AdmissionRequest{UID: types.UID("admission-request-uid"), Kind: metav1.GroupVersionKind{Version: "v1", Kind: "Secret"}, Resource: metav1.GroupVersionResource{Version: "v1", Resource: "secrets"}, Namespace: "argocd", Name: next.Name, Operation: admissionv1.Create, UserInfo: authenticationv1.UserInfo{Username: "system:serviceaccount:charlie-platform:hopcharlie-platform-api"}, Object: runtime.RawExtension{Raw: data}}
	if old != nil {
		r.Operation = admissionv1.Update
		r.OldObject.Raw, _ = json.Marshal(old)
	}
	return admissionv1.AdmissionReview{TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"}, Request: r}
}

func changeSecretConfig(s *corev1.Secret, change func(map[string]any)) {
	var c map[string]any
	_ = json.Unmarshal(s.Data["config"], &c)
	change(c)
	s.Data["config"], _ = json.Marshal(c)
}

func TestClusterSecretAdmissionAcceptsCanonicalCreateAndMonotonicRenewal(t *testing.T) {
	now := time.Date(2026, 11, 7, 14, 0, 0, 0, time.UTC)
	s := secretAdmissionFixture(t, now)
	r := secretAdmissionReview(t, s, nil)
	out := ValidateClusterSecretAdmission(r, now)
	if out.Response == nil || !out.Response.Allowed || out.Response.UID != r.Request.UID || out.Kind != "AdmissionReview" || out.APIVersion != "admission.k8s.io/v1" || out.Request != nil {
		t.Fatal("canonical create was not accepted as a response-only AdmissionReview")
	}
	assigned := s.DeepCopy()
	assigned.UID = "apiserver-assigned-object-uid"
	if out = ValidateClusterSecretAdmission(secretAdmissionReview(t, assigned, nil), now); !out.Response.Allowed {
		t.Fatal("API-server assigned CREATE UID was rejected")
	}
	old := s.DeepCopy()
	old.UID = "secret-object-uid"
	next := old.DeepCopy()
	next.Annotations["hopcharlie.dev/registration-version"] = "2"
	next.Annotations["hopcharlie.dev/token-expires-at"] = now.Add(90 * time.Minute).Format(time.RFC3339)
	changeSecretConfig(next, func(c map[string]any) { c["bearerToken"] = strings.Repeat("y", 40) + ".jwt.signature" })
	if out = ValidateClusterSecretAdmission(secretAdmissionReview(t, next, old), now); !out.Response.Allowed {
		t.Fatal("monotonic short-lived token renewal was rejected")
	}
	if out = ValidateClusterSecretAdmission(secretAdmissionReview(t, next, next.DeepCopy()), now); !out.Response.Allowed {
		t.Fatal("idempotent same-version renewal was rejected")
	}
	old.Annotations["hopcharlie.dev/token-expires-at"] = now.Add(-time.Minute).Format(time.RFC3339)
	if out = ValidateClusterSecretAdmission(secretAdmissionReview(t, next, old), now); !out.Response.Allowed {
		t.Fatal("an expired previous token prevented a valid renewal")
	}
}

func TestClusterSecretAdmissionRejectsCredentialAndTargetChangesWithoutEcho(t *testing.T) {
	now := time.Date(2026, 11, 7, 14, 0, 0, 0, time.UTC)
	cases := map[string]func(*corev1.Secret){
		"foreign owner":         func(s *corev1.Secret) { s.Labels[registrationOwnerLabel] = "enr_ffffffffffffffffffffffff" },
		"non UUID identity":     func(s *corev1.Secret) { s.Labels[registrationUIDLabel] = "claimed-identity" },
		"arbitrary server":      func(s *corev1.Secret) { s.Data["server"] = []byte("https://169.254.169.254/") },
		"unknown data":          func(s *corev1.Secret) { s.Data["password"] = []byte("private-secret") },
		"non canonical version": func(s *corev1.Secret) { s.Annotations["hopcharlie.dev/registration-version"] = "01" },
		"expired":               func(s *corev1.Secret) { s.Annotations["hopcharlie.dev/token-expires-at"] = now.Format(time.RFC3339) },
		"excessive lifetime": func(s *corev1.Secret) {
			s.Annotations["hopcharlie.dev/token-expires-at"] = now.Add(3 * time.Hour).Format(time.RFC3339)
		},
		"non UTC": func(s *corev1.Secret) { s.Annotations["hopcharlie.dev/token-expires-at"] = "2026-11-07T16:00:00+01:00" },
		"exec provider": func(s *corev1.Secret) {
			changeSecretConfig(s, func(c map[string]any) { c["execProviderConfig"] = map[string]any{"command": "private-secret"} })
		},
		"unknown TLS": func(s *corev1.Secret) {
			changeSecretConfig(s, func(c map[string]any) { c["tlsClientConfig"].(map[string]any)["certData"] = "private-secret" })
		},
		"insecure TLS": func(s *corev1.Secret) {
			changeSecretConfig(s, func(c map[string]any) { c["tlsClientConfig"].(map[string]any)["insecure"] = true })
		},
		"wrong SNI": func(s *corev1.Secret) {
			changeSecretConfig(s, func(c map[string]any) { c["tlsClientConfig"].(map[string]any)["serverName"] = "attacker.test" })
		},
		"invalid CA base64": func(s *corev1.Secret) {
			changeSecretConfig(s, func(c map[string]any) { c["tlsClientConfig"].(map[string]any)["caData"] = "%%%private-secret" })
		},
		"private key CA": func(s *corev1.Secret) {
			changeSecretConfig(s, func(c map[string]any) {
				c["tlsClientConfig"].(map[string]any)["caData"] = base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("private-secret")}))
			})
		},
		"garbage after CA": func(s *corev1.Secret) {
			changeSecretConfig(s, func(c map[string]any) {
				tls := c["tlsClientConfig"].(map[string]any)
				raw, _ := base64.StdEncoding.DecodeString(tls["caData"].(string))
				tls["caData"] = base64.StdEncoding.EncodeToString(append(raw, []byte("private-secret")...))
			})
		},
		"token whitespace": func(s *corev1.Secret) {
			changeSecretConfig(s, func(c map[string]any) { c["bearerToken"] = "private-secret\nBearer injected" })
		},
		"duplicate config key": func(s *corev1.Secret) {
			s.Data["config"] = append([]byte(`{"bearerToken":"private-secret",`), s.Data["config"][1:]...)
		},
		"trailing config": func(s *corev1.Secret) { s.Data["config"] = append(s.Data["config"], []byte(` {}`)...) },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			s := secretAdmissionFixture(t, now)
			change(s)
			out := ValidateClusterSecretAdmission(secretAdmissionReview(t, s, nil), now)
			if out.Response == nil || out.Response.Allowed {
				t.Fatal("invalid cluster Secret was accepted")
			}
			raw, _ := json.Marshal(out)
			if strings.Contains(string(raw), "private-secret") || strings.Contains(string(raw), string(s.Data["config"])) || out.Request != nil {
				t.Fatal("denial disclosed submitted credentials")
			}
		})
	}
}

func TestClusterSecretAdmissionUpdatePinsAndVersionRules(t *testing.T) {
	now := time.Date(2026, 11, 7, 14, 0, 0, 0, time.UTC)
	old := secretAdmissionFixture(t, now)
	old.UID = "secret-object-uid"
	old.Annotations["hopcharlie.dev/registration-version"] = "2"
	cases := map[string]func(*corev1.Secret){"regression": func(s *corev1.Secret) { s.Annotations["hopcharlie.dev/registration-version"] = "1" }, "same version changed config": func(s *corev1.Secret) {
		s.Annotations["hopcharlie.dev/registration-version"] = "2"
		changeSecretConfig(s, func(c map[string]any) { c["bearerToken"] = strings.Repeat("y", 40) + ".jwt.signature" })
	}, "CA replacement": func(s *corev1.Secret) {
		changeSecretConfig(s, func(c map[string]any) { c["tlsClientConfig"].(map[string]any)["caData"] = secretAdmissionCA(t, now) })
	}, "UID replacement": func(s *corev1.Secret) { s.UID = "another-secret-uid" }, "cluster replacement": func(s *corev1.Secret) { s.Labels[registrationUIDLabel] = "22222222-2222-4222-8222-222222222222" }, "expiry regression": func(s *corev1.Secret) {
		s.Annotations["hopcharlie.dev/token-expires-at"] = now.Add(30 * time.Minute).Format(time.RFC3339)
	}}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			next := old.DeepCopy()
			next.Annotations["hopcharlie.dev/registration-version"] = "3"
			change(next)
			if out := ValidateClusterSecretAdmission(secretAdmissionReview(t, next, old), now); out.Response.Allowed {
				t.Fatal("unsafe update accepted")
			}
		})
	}
}

func TestClusterSecretAdmissionRejectsMalformedReview(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	s := secretAdmissionFixture(t, now)
	cases := map[string]func(*admissionv1.AdmissionReview){"missing request": func(r *admissionv1.AdmissionReview) { r.Request = nil }, "wrong actor": func(r *admissionv1.AdmissionReview) {
		r.Request.UserInfo.Username = "system:serviceaccount:default:other"
	}, "wrong operation": func(r *admissionv1.AdmissionReview) { r.Request.Operation = admissionv1.Delete }, "wrong namespace": func(r *admissionv1.AdmissionReview) { r.Request.Namespace = "default" }, "invalid outer base64": func(r *admissionv1.AdmissionReview) {
		r.Request.Object.Raw = []byte(`{"apiVersion":"v1","kind":"Secret","data":{"config":"%%%"}}`)
	}}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			r := secretAdmissionReview(t, s, nil)
			change(&r)
			if out := ValidateClusterSecretAdmission(r, now); out.Response == nil || out.Response.Allowed {
				t.Fatal("malformed review accepted")
			}
		})
	}
}
