package vm

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
)

// Document is what inspect.sh prints, version 1 (the script's header
// comment has the same shape). Values are only in Secrets, encrypted.
type Document struct {
	Version       int         `json:"version"`
	At            time.Time   `json:"at"`
	Docker        string      `json:"docker"`
	Compose       string      `json:"compose"`
	DockerRoot    string      `json:"dockerRoot"`
	FreeDiskBytes int64       `json:"freeDiskBytes"`
	Containers    []Container `json:"containers"`
	Networks      []Network   `json:"networks"`
	Volumes       []Volume    `json:"volumes"`
	DB            []DBResult  `json:"db"`
	Secrets       string      `json:"secrets"`
}

// Container is one compose container as docker inspect shows it.
type Container struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"` // with docker's leading slash
	Image       string   `json:"image"`
	ImageID     string   `json:"imageId"`
	RepoDigests []string `json:"repoDigests"`
	Project     string   `json:"project"`
	Service     string   `json:"service"`
	WorkingDir  string   `json:"workingDir"`
	ConfigFiles string   `json:"configFiles"` // comma-separated, as compose labels it
	Revision    string   `json:"revision"`
	State       string   `json:"state"`
	Health      string   `json:"health"`
	StartedAt   string   `json:"startedAt"`
	Restart     string   `json:"restart"`
	Memory      int64    `json:"memory"`
	// Ports is docker's NetworkSettings.Ports: "8080/tcp" to the host
	// bindings (null when not published); Exposed Config.ExposedPorts.
	Ports    map[string][]PortBinding `json:"ports"`
	Exposed  map[string]struct{}      `json:"exposed"`
	Networks map[string]Endpoint      `json:"networks"`
	Mounts   []Mount                  `json:"mounts"`
	Env      []EnvName                `json:"env"`
}

// PortBinding is one host binding of a port.
type PortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// Endpoint is a container's attachment to a network.
type Endpoint struct {
	IPAddress string   `json:"IPAddress"`
	Aliases   []string `json:"Aliases"`
	DNSNames  []string `json:"DNSNames"`
}

// Mount is one of docker's Mounts.
type Mount struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

// EnvName is a variable's name; Image is true when the variable is the
// image's own (NAME=value exactly as the image sets it).
type EnvName struct {
	Name  string `json:"name"`
	Image bool   `json:"image"`
}

// Network is one docker network the containers are on.
type Network struct {
	Name     string `json:"name"`
	Driver   string `json:"driver"`
	Internal bool   `json:"internal"`
	IPAM     []struct {
		Subnet  string `json:"Subnet"`
		Gateway string `json:"Gateway"`
	} `json:"ipam"`
}

// Volume is one named volume a container mounts.
type Volume struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"sizeBytes"`
}

// DBResult is what psql read in one postgres container, or why it could
// not.
type DBResult struct {
	Container string   `json:"container"`
	Facts     *DBFacts `json:"facts"`
	Error     string   `json:"error"`
	// Others are the same facts read in each other database of the
	// server (only their indexes and collationDependent matter), and
	// OthersError the databases that could not be read.
	Others      []DBFacts `json:"others"`
	OthersError []string  `json:"othersError"`
	// ConfigWritable says the hba and config files are writable.
	ConfigWritable bool `json:"configWritable"`
}

// DBFacts is the psql query's object (inspect.sh db_sql).
type DBFacts struct {
	ServerVersionNum     int          `json:"serverVersionNum"`
	Version              string       `json:"version"`
	User                 string       `json:"user"`
	Database             string       `json:"database"`
	Owner                string       `json:"owner"`
	Databases            []DBDatabase `json:"databases"`
	LCMonetary           string       `json:"lcMonetary"`
	LCNumeric            string       `json:"lcNumeric"`
	LCTime               string       `json:"lcTime"`
	LCMessages           string       `json:"lcMessages"`
	AutoConf             []string     `json:"autoConf"`
	AllowAlterSystem     *string      `json:"allowAlterSystem"`
	WALLevel             string       `json:"walLevel"`
	MaxWALSenders        int          `json:"maxWalSenders"`
	PasswordEncryption   string       `json:"passwordEncryption"`
	SynchronousCommit    string       `json:"synchronousCommit"`
	ReadOnlyRoleSettings []string     `json:"readOnlyRoleSettings"`
	WALKeepSize          *string      `json:"walKeepSize"`
	WALKeepSizeSource    *string      `json:"walKeepSizeSource"`
	Superusers           []string     `json:"superusers"`
	PostgresRole         bool         `json:"postgresRole"`
	HBAFile              string       `json:"hbaFile"`
	ConfigFile           string       `json:"configFile"`
	DataDirectory        string       `json:"dataDirectory"`
	Port                 int          `json:"port"`
	WALBytesPerSecond    int64        `json:"walBytesPerSecond"`
	Connections          int          `json:"connections"`
	MaxConnections       int          `json:"maxConnections"`
	MaxWorkerProcesses   int          `json:"maxWorkerProcesses"`
	MaxPreparedXacts     int          `json:"maxPreparedTransactions"`
	MaxLocksPerXact      int          `json:"maxLocksPerTransaction"`
	WALSendersUsed       int          `json:"walSendersUsed"`
	PendingConfig        []string     `json:"pendingConfig"`
	Indexes              []DBIndex    `json:"indexes"`
	CollationDependent   []string     `json:"collationDependent"`
	Tables               []DBTable    `json:"tables"`
}

// DBDatabase is one row of pg_database.
type DBDatabase struct {
	Name        string  `json:"name"`
	Owner       string  `json:"owner"`
	LocProvider string  `json:"locProvider"`
	Collate     string  `json:"collate"`
	CType       string  `json:"ctype"`
	Locale      *string `json:"locale"`
	CollVersion *string `json:"collVersion"`
	SizeBytes   *int64  `json:"sizeBytes"`
}

// DBIndex is an index whose collation is not C-like; Provider is
// pg_collation's (c libc, i icu).
type DBIndex struct {
	Name      string `json:"name"`
	Table     string `json:"table"`
	Provider  string `json:"provider"`
	Collation string `json:"collation"`
	SizeBytes int64  `json:"sizeBytes"`
}

// DBTable is a user table.
type DBTable struct {
	Name      string `json:"name"`
	Rows      int64  `json:"rows"`
	SizeBytes int64  `json:"sizeBytes"`
}

// Values are the containers' environment values the VM encrypted:
// container name (without the slash) to name to value. It is never
// logged or put in Facts.
type Values map[string]map[string]string

// Inspection is one HopCharlie-Inspect run's result.
type Inspection struct {
	Document Document
	Values   Values
}

var headerLine = regexp.MustCompile(`^HOPCHARLIE-INSPECT v1 chunk=(\d+) chunks=(\d+) sha256=([0-9a-f]{64})$`)

// chunk is one step's output.
type chunk struct {
	index, of int
	sha       string
	body      string
}

func parseChunk(stdout string) (chunk, error) {
	head, body, _ := strings.Cut(strings.TrimLeft(stdout, "\r\n"), "\n")
	m := headerLine.FindStringSubmatch(strings.TrimSpace(head))
	if m == nil {
		return chunk{}, errors.New("the inspection's output has no HOPCHARLIE-INSPECT header")
	}
	i, _ := strconv.Atoi(m[1])
	n, _ := strconv.Atoi(m[2])
	return chunk{index: i, of: n, sha: m[3], body: strings.TrimSpace(body)}, nil
}

// Assemble joins the steps' outputs (chunk 0 first) into the document.
// It needs at least as many outputs as the header's chunk count.
func Assemble(outputs []string) (Document, error) {
	if len(outputs) == 0 {
		return Document{}, errors.New("no inspection output")
	}
	first, err := parseChunk(outputs[0])
	if err != nil {
		return Document{}, err
	}
	if first.index != 0 || first.of < 1 || first.of > len(outputs) {
		return Document{}, fmt.Errorf("the inspection's output has %d chunks, %d were read", first.of, len(outputs))
	}
	var b strings.Builder
	for i := range first.of {
		c, err := parseChunk(outputs[i])
		if err != nil {
			return Document{}, err
		}
		if c.index != i || c.of != first.of || c.sha != first.sha {
			return Document{}, fmt.Errorf("chunk %d of the inspection's output belongs to another run", i)
		}
		b.WriteString(c.body)
	}
	sum := sha256.Sum256([]byte(b.String()))
	if hex.EncodeToString(sum[:]) != first.sha {
		return Document{}, errors.New("the inspection's output is damaged (sha256 mismatch)")
	}
	gz, err := base64.StdEncoding.DecodeString(b.String())
	if err != nil {
		return Document{}, errors.New("the inspection's output is not base64")
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return Document{}, errors.New("the inspection's output is not gzip")
	}
	raw, err := io.ReadAll(io.LimitReader(zr, 64<<20))
	if err != nil {
		return Document{}, errors.New("the inspection's output does not decompress")
	}
	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Document{}, fmt.Errorf("the inspection's document is not JSON: %v", err)
	}
	if doc.Version != 1 {
		return Document{}, fmt.Errorf("the inspection's document is version %d; this platform reads 1", doc.Version)
	}
	return doc, nil
}

// OpenValues decrypts the document's Secrets: {"<container>":["NAME=value",
// ...]} into Values.
func OpenValues(k *Key, envelope string) (Values, error) {
	plain, err := k.Open(envelope)
	if err != nil {
		return nil, err
	}
	var raw map[string][]string
	if err := json.Unmarshal(plain, &raw); err != nil {
		return nil, errors.New("the VM's environment values are not the expected JSON")
	}
	out := make(Values, len(raw))
	for c, env := range raw {
		m := make(map[string]string, len(env))
		for _, kv := range env {
			if name, value, ok := strings.Cut(kv, "="); ok {
				m[name] = value
			}
		}
		out[c] = m
	}
	return out, nil
}

// Inspect runs HopCharlie-Inspect on instance and returns the document
// and the decrypted values. The key pair is made here and dropped when it
// returns.
func (r *Runner) Inspect(ctx context.Context, instance string) (*Inspection, error) {
	key, err := NewKey()
	if err != nil {
		return nil, err
	}
	id, err := r.Send(ctx, instance, onboard.InspectDocument, map[string]string{
		"publicKey": key.PublicKey(), "maxChunk": strconv.Itoa(MaxChunk),
	})
	if err != nil {
		return nil, err
	}
	res, err := r.Wait(ctx, instance, id, InspectStep)
	if err != nil {
		return nil, err
	}
	if res.Failed() {
		return nil, stepError(onboard.InspectDocument, res)
	}
	first, err := parseChunk(res.Stdout)
	if err != nil {
		return nil, err
	}
	if first.of > InspectChunks {
		return nil, fmt.Errorf("the inspection's output has %d chunks; the document has %d", first.of, InspectChunks)
	}
	outputs := []string{res.Stdout}
	for i := 1; i < first.of; i++ {
		res, err := r.Wait(ctx, instance, id, ChunkStep(i))
		if err != nil {
			return nil, err
		}
		if res.Failed() {
			return nil, stepError(onboard.InspectDocument, res)
		}
		outputs = append(outputs, res.Stdout)
	}
	doc, err := Assemble(outputs)
	if err != nil {
		return nil, err
	}
	values, err := OpenValues(key, doc.Secrets)
	if err != nil {
		return nil, err
	}
	doc.Secrets = ""
	return &Inspection{Document: doc, Values: values}, nil
}
