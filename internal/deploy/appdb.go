package deploy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/secrets"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/store"
)

// The database pair of an app registered in exactly two environments (ADR
// 0030): the default instance's database is born primary in the app's first
// environment and a replica of it in the other. Both clusters get the same
// app password (Secret <cluster>-app) and the same CA (Secret app-pg-ca),
// with which CNPG issues each cluster's server and streaming_replica
// certificates, so each side's replication certificate passes on the other.

// AppDBPasswordLength is the length of an app database password: letters and
// digits only, so it goes into a URL as it is.
const AppDBPasswordLength = 32

// AppDBCAValidity is how long the CA of a database pair is valid. CNPG
// renews the certificates it issues with it (every 90 days) but never the CA
// itself; it only warns before it expires. Renewing it is an operator's job
// and not handled yet (ADR 0030).
const AppDBCAValidity = 10 * 365 * 24 * time.Hour

// AppDBPort is the port of the clusters' -rw Services.
const AppDBPort = "5432"

// NewAppDatabase makes the database pair of app, registered in envs at now:
// a new password and CA, encrypted with c. The database is born primary in
// envs[0]. It needs exactly two environments.
func NewAppDatabase(c *secrets.Cipher, app string, envs []string, now time.Time) (store.AppDatabase, error) {
	if len(envs) != 2 || envs[0] == envs[1] {
		return store.AppDatabase{}, fmt.Errorf("a database pair needs exactly two environments, not %q", envs)
	}
	password, err := newAppDBPassword()
	if err != nil {
		return store.AppDatabase{}, err
	}
	cert, key, err := newAppDBCA(app, now)
	if err != nil {
		return store.AppDatabase{}, err
	}
	row, err := c.DatabaseRow(app, envs[0], envs[1], password, cert, key)
	if err != nil {
		return store.AppDatabase{}, err
	}
	row.CreatedAt = now
	return row, nil
}

const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// newAppDBPassword returns AppDBPasswordLength random letters and digits.
func newAppDBPassword() (string, error) {
	out := make([]byte, AppDBPasswordLength)
	limit := big.NewInt(int64(len(passwordAlphabet)))
	for i := range out {
		n, err := rand.Int(rand.Reader, limit)
		if err != nil {
			return "", err
		}
		out[i] = passwordAlphabet[n.Int64()]
	}
	return string(out), nil
}

// newAppDBCA returns a self-signed ECDSA P-256 CA valid for AppDBCAValidity,
// its certificate and its key in PEM. CNPG reads the key only as a SEC1
// "EC PRIVATE KEY" (its certs.ParseECPrivateKey).
func newAppDBCA(app string, now time.Time) (certPEM, keyPEM string, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: app + " postgres CA", Organization: []string{"HopCharlie"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(AppDBCAValidity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}
	sec1, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: sec1})), nil
}

// ClusterName is the CNPG Cluster the app chart makes for the app's
// postgres resource: <app>-<resource>.
func ClusterName(app, resource string) string { return app + "-" + resource }

// pgAppSecret returns the keys of Secret <cluster>-app in namespace ns, as
// CNPG writes them for the database app owned by user app: the services'
// DATABASE_URL, PGHOST and the others come from it (the chart's
// platformEnv).
func pgAppSecret(cluster, ns, password string) map[string]string {
	host := cluster + "-rw"
	addr := host + "." + ns + ":" + AppDBPort
	return map[string]string{
		"username": "app",
		"password": password,
		"dbname":   "app",
		"host":     host,
		"port":     AppDBPort,
		"uri":      "postgresql://app:" + password + "@" + addr + "/app",
		"jdbc-uri": "jdbc:postgresql://" + addr + "/app?password=" + password + "&user=app",
	}
}
