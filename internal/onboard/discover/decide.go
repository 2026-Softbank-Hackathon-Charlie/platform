package discover

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/internal/onboard/vm"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/appspec"
	"github.com/2026-Softbank-Hackathon-Charlie/platform/pkg/onboard/facts"
)

// report collects findings.
type report struct{ r *facts.Report }

func (r report) block(code, subject, format string, a ...any) {
	r.r.Blockers = append(r.r.Blockers, facts.Finding{Code: code, Subject: subject, Message: fmt.Sprintf(format, a...)})
}

func (r report) warn(code, subject, format string, a ...any) {
	r.r.Warnings = append(r.r.Warnings, facts.Finding{Code: code, Subject: subject, Message: fmt.Sprintf(format, a...)})
}

var (
	postgresImage = regexp.MustCompile(`(?i)postgres|postgis`)
	minioImage    = regexp.MustCompile(`(?i)(^|/)minio[^/]*(:|@|$)`)
	proxyImage    = regexp.MustCompile(`(?i)(^|/)(caddy|nginx|traefik|haproxy|envoy|httpd|openresty|swag|nginx-proxy[^/]*)(:|@|$)`)
	proxyService  = regexp.MustCompile(`(?i)^(proxy|reverse-proxy|caddy|nginx|traefik|web-proxy|edge|tls)$`)
	appService    = regexp.MustCompile(`(?i)^(app|web|api|server|backend|frontend|service)$`)

	// Names whose values the platform keeps sealed (platform.yaml secrets).
	secretName = regexp.MustCompile(`(?i)(pass|secret|token|key|credential|private|auth|dsn|database_url|cookie|salt|signing|cert)`)
	// Hosts of managed databases (blocker managed_database).
	managedDBHost = regexp.MustCompile(`(?i)(\.rds\.amazonaws\.com|\.rds\.[a-z0-9.-]+\.amazonaws\.com|/cloudsql/|\.sql\.goog|cloudsql)`)
)

// containerName is docker's name without the slash.
func containerName(c vm.Container) string { return strings.TrimPrefix(c.Name, "/") }

// chooseProject returns the compose project the app runs as: the one that
// publishes 443, else the one with a postgres container, else the largest.
func chooseProject(doc vm.Document) string {
	type score struct {
		https, pg bool
		n         int
	}
	scores := map[string]*score{}
	for _, c := range doc.Containers {
		if c.Project == "" {
			continue
		}
		s := scores[c.Project]
		if s == nil {
			s = &score{}
			scores[c.Project] = s
		}
		s.n++
		if slices.Contains(hostPorts(c), 443) {
			s.https = true
		}
		if postgresImage.MatchString(c.Image) {
			s.pg = true
		}
	}
	best, bs := "", score{}
	for name, s := range scores {
		better := func() bool {
			if s.https != bs.https {
				return s.https
			}
			if s.pg != bs.pg {
				return s.pg
			}
			if s.n != bs.n {
				return s.n > bs.n
			}
			return best == "" || name < best
		}()
		if best == "" || better {
			best, bs = name, *s
		}
	}
	return best
}

// hostPorts are the host ports a container publishes.
func hostPorts(c vm.Container) []int {
	var out []int
	for _, binds := range c.Ports {
		for _, b := range binds {
			if p := atoi(b.HostPort); p > 0 && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
	}
	slices.Sort(out)
	return out
}

// containerPorts are a container's exposed or published container ports.
func containerPorts(c vm.Container) []facts.Port {
	seen := map[string]bool{}
	var out []facts.Port
	add := func(spec string, host int) {
		num, proto, _ := strings.Cut(spec, "/")
		p := atoi(num)
		if p <= 0 || seen[spec] {
			return
		}
		seen[spec] = true
		out = append(out, facts.Port{Container: p, Host: host, Protocol: cmp.Or(proto, "tcp")})
	}
	for spec, binds := range c.Ports {
		host := 0
		for _, b := range binds {
			if h := atoi(b.HostPort); h > 0 {
				host = h
				break
			}
		}
		add(spec, host)
	}
	for spec := range c.Exposed {
		add(spec, 0)
	}
	slices.SortFunc(out, func(a, b facts.Port) int { return cmp.Compare(a.Container, b.Container) })
	return out
}

// isReverseProxy: a proxy image or service name that publishes 443 and
// nothing but 80 and 443.
func isReverseProxy(c vm.Container) bool {
	hp := hostPorts(c)
	if !slices.Contains(hp, 443) {
		return false
	}
	for _, p := range hp {
		if p != 80 && p != 443 {
			return false
		}
	}
	return proxyImage.MatchString(c.Image) || proxyService.MatchString(c.Service)
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// inspected is what fromInspection derived from the VM.
type inspected struct {
	dbContainer string
	// superuserPassword is the fence user's password (the DB container's
	// POSTGRES_PASSWORD), "" when unknown.
	superuserPassword string
}

// fromInspection fills f.VM, f.DB, f.Storage and the findings that follow
// from them.
func fromInspection(f *facts.Facts, doc vm.Document, values vm.Values, proxyTargets []ServicePort, r report) inspected {
	var out inspected
	project := chooseProject(doc)
	v := &facts.VM{DockerVersion: doc.Docker, ComposeVersion: doc.Compose, FreeDiskBytes: doc.FreeDiskBytes, Project: project,
		ConfigFiles: []string{}, Networks: []facts.Network{}, Containers: []facts.Container{}, Volumes: []facts.Volume{}}
	f.VM = v
	if v.FreeDiskBytes < onboard.MinFreeDisk {
		r.block(facts.BlockDiskTooSmall, doc.DockerRoot, "%d MiB free on %s; wal_keep_size needs at least 2 GiB", v.FreeDiskBytes>>20, doc.DockerRoot)
	}
	if project == "" {
		r.block(facts.BlockNoCompose, "", "no docker compose project runs on the VM")
		return out
	}
	var cs []vm.Container
	for _, c := range doc.Containers {
		if c.Project == project {
			cs = append(cs, c)
		}
	}
	slices.SortFunc(cs, func(a, b vm.Container) int { return cmp.Compare(a.Service+a.Name, b.Service+b.Name) })
	services := map[string]bool{}
	for _, c := range cs {
		services[c.Service] = true
		if v.WorkingDir == "" {
			v.WorkingDir = c.WorkingDir
			for _, p := range strings.Split(c.ConfigFiles, ",") {
				if p = strings.TrimSpace(p); p != "" {
					v.ConfigFiles = append(v.ConfigFiles, p)
				}
			}
		}
	}

	// The database container.
	var pg []vm.Container
	for _, c := range cs {
		if postgresImage.MatchString(c.Image) && c.State == "running" {
			pg = append(pg, c)
		}
	}
	dbc, dbRes := pickDB(pg, cs, doc, values)
	if dbc != nil && (dbRes == nil || dbRes.Facts == nil) {
		why := "inspect.sh has no result for it"
		if dbRes != nil && dbRes.Error != "" {
			why = cleanError(dbRes.Error)
		}
		r.block(facts.BlockDBUnreadable, containerName(*dbc), "psql could not read the database in %s: %s", containerName(*dbc), why)
	}
	if dbc == nil {
		code, msg := facts.BlockDBNotContainer, "no PostgreSQL container runs in the compose project"
		for _, c := range cs {
			for _, val := range values[containerName(c)] {
				if managedDBHost.MatchString(val) {
					code, msg = facts.BlockManagedDatabase, "the app uses a managed database (RDS or Cloud SQL); physical replication is not possible"
				}
			}
		}
		r.block(code, "", "%s", msg)
	}

	// Containers, their environment and volumes.
	volUsers := map[string][]string{}
	volRole := map[string]string{}
	for _, c := range cs {
		name := containerName(c)
		fc := facts.Container{Name: name, Service: c.Service, Image: c.Image, ImageID: c.ImageID, Revision: c.Revision,
			State: c.State, Restart: c.Restart, Health: c.Health, MemoryLimitBytes: c.Memory,
			Ports: containerPorts(c), Networks: []string{}, Mounts: []facts.Mount{}, Env: []facts.EnvVar{},
			ReverseProxy: isReverseProxy(c)}
		if len(c.RepoDigests) > 0 {
			fc.RepoDigest = c.RepoDigests[0]
		}
		if fc.Ports == nil {
			fc.Ports = []facts.Port{}
		}
		if t, err := time.Parse(time.RFC3339Nano, c.StartedAt); err == nil && t.Year() > 1 {
			fc.Started = t.UTC()
		}
		for n := range c.Networks {
			fc.Networks = append(fc.Networks, n)
		}
		slices.Sort(fc.Networks)
		isDB := dbc != nil && c.ID == dbc.ID
		for _, m := range c.Mounts {
			fc.Mounts = append(fc.Mounts, facts.Mount{Type: m.Type, Source: cmp.Or(m.Name, m.Source), Destination: m.Destination, ReadOnly: !m.RW})
			if m.Type != "volume" || m.Name == "" {
				continue
			}
			volUsers[m.Name] = append(volUsers[m.Name], name)
			role := facts.VolumeData
			switch {
			case isDB && dbRes != nil && dbRes.Facts != nil && (strings.HasPrefix(dbRes.Facts.DataDirectory+"/", strings.TrimSuffix(m.Destination, "/")+"/")):
				role = facts.VolumeDatabase
			case isDB && strings.HasPrefix(m.Destination, "/var/lib/postgresql"):
				role = facts.VolumeDatabase
			case minioImage.MatchString(c.Image):
				role = facts.VolumeObjectStorage
			case fc.ReverseProxy:
				role = facts.VolumeProxy
			}
			// The strongest claim on a shared volume wins.
			if rank(role) > rank(volRole[m.Name]) {
				volRole[m.Name] = role
			}
		}
		fc.Env = classifyEnv(c, values[name], services, isDB)
		v.Containers = append(v.Containers, fc)
	}
	for _, vol := range doc.Volumes {
		users, ok := volUsers[vol.Name]
		if !ok {
			continue
		}
		role := volRole[vol.Name]
		// du of an empty directory is one block.
		if role == facts.VolumeData && vol.SizeBytes <= 4096 {
			role = facts.VolumeEmpty
		}
		slices.Sort(users)
		v.Volumes = append(v.Volumes, facts.Volume{Name: vol.Name, SizeBytes: vol.SizeBytes, UsedBy: users, Role: role})
		switch role {
		case facts.VolumeObjectStorage:
			r.block(facts.BlockStorageOutOfScope, vol.Name, "volume %s is an object store inside the VM; only external S3-compatible storage over HTTPS is supported", vol.Name)
		case facts.VolumeData:
			r.block(facts.BlockDataVolume, vol.Name, "volume %s (%d MiB, used by %s) holds data that is neither the database nor object storage", vol.Name, vol.SizeBytes>>20, strings.Join(users, ", "))
		}
	}
	slices.SortFunc(v.Volumes, func(a, b facts.Volume) int { return cmp.Compare(a.Name, b.Name) })

	// Networks of the project's containers.
	nets := map[string]bool{}
	for _, c := range v.Containers {
		for _, n := range c.Networks {
			nets[n] = true
		}
	}
	for _, n := range doc.Networks {
		if !nets[n.Name] {
			continue
		}
		fn := facts.Network{Name: n.Name, Driver: n.Driver}
		for _, ipam := range n.IPAM {
			if strings.Contains(ipam.Subnet, ".") {
				fn.Subnet, fn.Gateway = ipam.Subnet, ipam.Gateway
				break
			}
		}
		v.Networks = append(v.Networks, fn)
	}
	slices.SortFunc(v.Networks, func(a, b facts.Network) int { return cmp.Compare(a.Name, b.Name) })

	if dbc != nil {
		out.dbContainer = containerName(*dbc)
		if dbRes != nil && dbRes.Facts != nil {
			f.DB = dbFromFacts(*dbc, *dbRes, values[out.dbContainer], r)
			if f.DB.FenceUser != "" {
				out.superuserPassword = values[out.dbContainer]["POSTGRES_PASSWORD"]
			}
		}
	}
	f.Storage = storageFrom(v, cs, values, services, r)
	if t := publicService(v, proxyTargets, out.dbContainer); t.Service != "" {
		if f.Target == nil {
			f.Target = &facts.Target{Reindex: []string{}}
		}
		f.Target.PublicService, f.Target.Port = t.Service, t.Port
	}
	return out
}

func rank(role string) int {
	switch role {
	case facts.VolumeDatabase:
		return 4
	case facts.VolumeObjectStorage:
		return 3
	case facts.VolumeData:
		return 2
	case facts.VolumeProxy:
		return 1
	}
	return 0
}

// pickDB returns the postgres container the app uses and its facts: the
// one whose service name the app's values name, else the first.
func pickDB(pg, all []vm.Container, doc vm.Document, values vm.Values) (*vm.Container, *vm.DBResult) {
	if len(pg) == 0 {
		return nil, nil
	}
	chosen := pg[0]
	if len(pg) > 1 {
	outer:
		for _, c := range pg {
			for _, other := range all {
				if postgresImage.MatchString(other.Image) {
					continue
				}
				for _, val := range values[containerName(other)] {
					if refersTo(val, c.Service) {
						chosen = c
						break outer
					}
				}
			}
		}
	}
	for i, d := range doc.DB {
		if d.Container == containerName(chosen) {
			return &chosen, &doc.DB[i]
		}
	}
	return &chosen, nil
}

// refersTo reports whether a value names a compose service as a host:
// db, db:5432, postgres://u:p@db:5432/x, http://minio:9000.
func refersTo(value, service string) bool {
	if service == "" {
		return false
	}
	if value == service {
		return true
	}
	if u, err := url.Parse(value); err == nil && u.Host != "" && u.Hostname() == service {
		return true
	}
	re := regexp.MustCompile(`(^|[@/,;\s=])` + regexp.QuoteMeta(service) + `:[0-9]{1,5}\b`)
	return re.MatchString(value)
}

// classifyEnv returns a container's variables as Facts keep them: no
// image defaults, each with its secret flag and why it is not carried
// over.
func classifyEnv(c vm.Container, values map[string]string, services map[string]bool, isDB bool) []facts.EnvVar {
	out := []facts.EnvVar{}
	for _, e := range c.Env {
		if e.Image {
			continue
		}
		ev := facts.EnvVar{Name: e.Name, Secret: secretName.MatchString(e.Name)}
		switch {
		case isDB:
			ev.Excluded = facts.ExcludeDBContainer
		case appspec.Reserved(e.Name) || strings.HasPrefix(e.Name, "PG"):
			ev.Excluded = facts.ExcludePlatform
		default:
			for s := range services {
				if refersTo(values[e.Name], s) {
					ev.Excluded = facts.ExcludeComposeRef
					break
				}
			}
		}
		out = append(out, ev)
	}
	slices.SortFunc(out, func(a, b facts.EnvVar) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

var pgdg = regexp.MustCompile(`pgdg(11|12|13)0`)

// dbFromFacts maps psql's facts and checks the settings of ADR 0042's
// table.
func dbFromFacts(c vm.Container, res vm.DBResult, env map[string]string, r report) *facts.DB {
	d := *res.Facts
	db := &facts.DB{Container: containerName(c), Service: c.Service, Port: cmp.Or(d.Port, 5432),
		ServerVersionNum: d.ServerVersionNum, Version: d.Version, EnvDatabase: d.Database, EnvUser: d.User,
		Database: d.Database, Owner: d.Owner, Databases: []facts.Database{},
		LCMonetary: d.LCMonetary, LCNumeric: d.LCNumeric, LCTime: d.LCTime, LCMessages: d.LCMessages,
		AutoConf: nonNil(d.AutoConf), AllowAlterSystem: deref(d.AllowAlterSystem), WALLevel: d.WALLevel,
		MaxWALSenders: d.MaxWALSenders, PasswordEncryption: d.PasswordEncryption,
		ReadOnlyRoleSettings: nonNil(d.ReadOnlyRoleSettings), WALKeepSize: deref(d.WALKeepSize),
		WALKeepSizeSource: deref(d.WALKeepSizeSource), Superusers: nonNil(d.Superusers), PostgresRole: d.PostgresRole,
		HBAFile: d.HBAFile, ConfigFile: d.ConfigFile, DataDirectory: d.DataDirectory,
		WALBytesPerSecond: d.WALBytesPerSecond, Connections: d.Connections, MaxConnections: d.MaxConnections,
		MaxWorkerProcesses: d.MaxWorkerProcesses, MaxPreparedTransactions: d.MaxPreparedXacts,
		MaxLocksPerTransaction: d.MaxLocksPerXact, WALSendersUsed: d.WALSendersUsed,
		PendingConfig: nonNil(d.PendingConfig), ConfigWritable: res.ConfigWritable,
		Indexes: []facts.Index{}, CollationDependent: nonNil(d.CollationDependent), Tables: []facts.Table{}}
	if strings.Contains(d.Version, "musl") {
		db.Libc = facts.LibcMusl
	} else {
		db.Libc = facts.LibcGlibc
		if m := pgdg.FindStringSubmatch(d.Version); m != nil {
			db.Debian = map[string]string{"11": "bullseye", "12": "bookworm", "13": "trixie"}[m[1]]
		}
	}
	for _, x := range d.Databases {
		db.Databases = append(db.Databases, facts.Database{Name: x.Name, Owner: x.Owner, LocProvider: x.LocProvider,
			Collate: x.Collate, CType: x.CType, ICULocale: deref(x.Locale), CollVersion: deref(x.CollVersion), SizeBytes: derefInt(x.SizeBytes)})
	}
	for _, i := range d.Indexes {
		coll := i.Collation
		if i.Provider == "i" {
			coll = "icu:" + coll
		}
		db.Indexes = append(db.Indexes, facts.Index{Name: i.Name, Table: i.Table, Collation: coll, SizeBytes: i.SizeBytes})
	}
	for _, t := range d.Tables {
		db.Tables = append(db.Tables, facts.Table{Name: t.Name, Rows: t.Rows, SizeBytes: t.SizeBytes})
	}

	if len(db.AutoConf) > 0 {
		r.block(facts.BlockAutoConf, strings.Join(db.AutoConf, ","), "postgresql.auto.conf sets %s; bootstrap would copy it to the target", strings.Join(db.AutoConf, ", "))
	}
	if len(db.ReadOnlyRoleSettings) > 0 {
		r.block(facts.BlockRoleReadOnly, strings.Join(db.ReadOnlyRoleSettings, ","), "a role or database setting of default_transaction_read_only (%s) would override the fence", strings.Join(db.ReadOnlyRoleSettings, ", "))
	}
	if db.WALKeepSizeSource == "command line" {
		r.block(facts.BlockWALKeepSizeCmdLine, "wal_keep_size", "wal_keep_size is set on the command line; postgresql.conf cannot change it")
	}
	if db.AllowAlterSystem == "off" {
		r.block(facts.BlockAlterSystemOff, "allow_alter_system", "allow_alter_system is off; the fence uses ALTER SYSTEM")
	}
	if db.WALLevel == "minimal" {
		r.block(facts.BlockWALLevelMinimal, "wal_level", "wal_level is minimal; streaming replication needs replica")
	}
	if free := db.MaxWALSenders - db.WALSendersUsed; free < 3 {
		r.block(facts.BlockNoWALSenders, "max_wal_senders", "%d of max_wal_senders %d are free; pg_basebackup and streaming need 3", max(free, 0), db.MaxWALSenders)
	}
	if len(db.PendingConfig) > 0 {
		r.block(facts.BlockPendingConfig, strings.Join(db.PendingConfig, ","), "settings wait for a restart or the configuration has errors (%s); connecting's reload would apply them", strings.Join(db.PendingConfig, ", "))
	}
	if !db.ConfigWritable {
		r.block(facts.BlockConfigNotWritable, db.HBAFile, "pg_hba.conf or postgresql.conf is not writable in the container")
	}
	// Other databases: REINDEX runs only in the app's database, so on a
	// path where collation order changes their libc or ICU indexes and
	// collation-dependent constraints cannot be fixed.
	if libcBreaks(db) {
		for _, o := range res.Others {
			var hit []string
			for _, i := range o.Indexes {
				if !cLike(i.Collation) {
					hit = append(hit, i.Name)
				}
			}
			hit = append(hit, o.CollationDependent...)
			if len(hit) > 0 {
				r.block(facts.BlockOtherDBCollation, o.Database, "database %s has %s, which depend on a collation that sorts differently on the target; only the app's database is reindexed", o.Database, strings.Join(hit, ", "))
			}
		}
		for _, e := range res.OthersError {
			name, _, _ := strings.Cut(e, ":")
			r.block(facts.BlockOtherDBCollation, name, "database %s could not be read to check its collations", name)
		}
	}
	if db.PasswordEncryption == "md5" {
		r.block(facts.BlockPasswordMD5, "password_encryption", "password_encryption is md5; the replication account's scram hba line would refuse it")
	}
	// The fence user is the container's own user when it is a superuser
	// whose password discovery has (POSTGRES_PASSWORD).
	if slices.Contains(db.Superusers, d.User) && env["POSTGRES_PASSWORD"] != "" {
		db.FenceUser = d.User
	} else {
		r.block(facts.BlockNoSuperuser, d.User, "no superuser with a known password (the container's POSTGRES_USER and POSTGRES_PASSWORD) was found")
	}
	if db.FenceUser != "" && db.FenceUser == db.Owner {
		r.warn(facts.WarnAppSuperuser, db.FenceUser, "the app's database account %s is a superuser", db.FenceUser)
	}
	return db
}

// storageFrom finds the app's object storage in its variables.
func storageFrom(v *facts.VM, cs []vm.Container, values vm.Values, services map[string]bool, r report) *facts.Storage {
	var minio string
	for _, c := range cs {
		if minioImage.MatchString(c.Image) {
			minio = c.Service
		}
	}
	for _, c := range cs {
		if postgresImage.MatchString(c.Image) || minioImage.MatchString(c.Image) {
			continue
		}
		vals := values[containerName(c)]
		bucketEnv := firstName(vals, `^(S3_BUCKET|AWS_S3_BUCKET|AWS_BUCKET|BUCKET|BUCKET_NAME)$`, `(?i)bucket`)
		if bucketEnv == "" || !bucketName.MatchString(vals[bucketEnv]) {
			continue
		}
		s := &facts.Storage{Container: containerName(c), BucketEnv: bucketEnv, Bucket: vals[bucketEnv],
			EndpointEnv: firstName(vals, `^(AWS_ENDPOINT_URL_S3|S3_ENDPOINT|S3_ENDPOINT_URL|AWS_ENDPOINT_URL|AWS_S3_ENDPOINT)$`,
				`(?i)^.*(S3|STORAGE|MINIO|BUCKET|OBJECT)[A-Z0-9_]*ENDPOINT[A-Z0-9_]*$`, `(?i)ENDPOINT`),
			AccessKeyEnv: firstName(vals, `^(AWS_ACCESS_KEY_ID|S3_ACCESS_KEY_ID|S3_ACCESS_KEY)$`, `(?i)ACCESS_KEY`),
			SecretKeyEnv: firstName(vals, `^(AWS_SECRET_ACCESS_KEY|S3_SECRET_ACCESS_KEY|S3_SECRET_KEY)$`, `(?i)(SECRET_ACCESS|SECRET_KEY)`),
			RegionEnv:    firstName(vals, `^(AWS_REGION|AWS_DEFAULT_REGION|S3_REGION)$`, `(?i)(S3|STORAGE)[A-Z0-9_]*REGION`)}
		s.Region = vals[s.RegionEnv]
		raw := vals[s.EndpointEnv]
		host := ""
		if raw != "" {
			if u, err := url.Parse(raw); err == nil {
				host = u.Hostname()
			}
			ep, err := facts.Endpoint(raw)
			if err != nil {
				s.Provider = facts.StorageOther
				r.block(facts.BlockStorageOutOfScope, s.EndpointEnv, "the object storage endpoint in %s is not an http(s) URL", s.EndpointEnv)
				return s
			}
			s.Endpoint = ep
		}
		switch {
		case services[host] || (minio != "" && host == minio) || host == "localhost" || host == "127.0.0.1":
			s.Provider = facts.StorageInVM
			r.block(facts.BlockStorageOutOfScope, s.EndpointEnv, "files are in %s inside the VM; only external S3-compatible storage over HTTPS is supported", host)
		case raw == "":
			s.Provider = facts.StorageAWS
			s.Endpoint = "https://s3." + cmp.Or(s.Region, "us-east-1") + ".amazonaws.com"
		case strings.HasSuffix(host, ".amazonaws.com"):
			s.Provider = facts.StorageAWS
		case host == "storage.googleapis.com":
			s.Provider = facts.StorageGCS
		default:
			s.Provider = facts.StorageOther
		}
		if s.Provider != facts.StorageInVM && strings.HasPrefix(s.Endpoint, "http://") {
			r.block(facts.BlockStorageOutOfScope, s.EndpointEnv, "the object storage endpoint is not HTTPS")
		}
		return s
	}
	if minio != "" {
		r.block(facts.BlockStorageOutOfScope, minio, "service %s is an object store inside the VM; only external S3-compatible storage over HTTPS is supported", minio)
		return &facts.Storage{Provider: facts.StorageInVM, Container: minio}
	}
	return nil
}

// bucketName is an S3 bucket name.
var bucketName = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// firstName returns the variable name with a value that matches the first
// of res that any matches, the first in name order among those.
func firstName(vals map[string]string, res ...string) string {
	for _, re := range res {
		rx := regexp.MustCompile(re)
		var names []string
		for n, v := range vals {
			if v != "" && rx.MatchString(n) {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			slices.Sort(names)
			return names[0]
		}
	}
	return ""
}

// cleanError keeps a psql error to one short line of printable text.
func cleanError(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// publicService picks the service the reverse proxy sends to: the
// repository's proxy configuration first, else the non-database,
// non-proxy service with a port (one named like an app first).
func publicService(v *facts.VM, proxyTargets []ServicePort, dbContainer string) ServicePort {
	bySvc := map[string]facts.Container{}
	for _, c := range v.Containers {
		bySvc[c.Service] = c
	}
	for _, t := range proxyTargets {
		if c, ok := bySvc[t.Service]; ok && !c.ReverseProxy && c.Name != dbContainer {
			return t
		}
	}
	var best ServicePort
	bestScore := -1
	for _, c := range v.Containers {
		if c.ReverseProxy || c.Name == dbContainer || minioImage.MatchString(c.Image) || len(c.Ports) == 0 {
			continue
		}
		port := 0
		for _, p := range c.Ports {
			if p.Protocol == "tcp" {
				port = p.Container
				break
			}
		}
		if port == 0 {
			continue
		}
		score := 0
		if appService.MatchString(c.Service) {
			score = 2
		}
		if score > bestScore || (score == bestScore && c.Service < best.Service) {
			best, bestScore = ServicePort{Service: c.Service, Port: port}, score
		}
	}
	return best
}

// libcBreaks says whether the target's collation order differs from the
// source's: a musl source, or a glibc source whose Debian release is not
// known.
func libcBreaks(db *facts.DB) bool {
	return db.Libc == facts.LibcMusl || db.Debian == ""
}

// cLike is a libc locale that sorts by bytes.
func cLike(locale string) bool {
	switch locale {
	case "C", "POSIX", "C.UTF-8", "C.utf8", "ucs_basic", "":
		return true
	}
	return false
}

// chooseTarget fills f.Target from f.DB (ADR 0042's image rules) and the
// storage size.
func chooseTarget(ctx context.Context, images Images, f *facts.Facts, env string, r report) error {
	if f.Target == nil {
		f.Target = &facts.Target{Reindex: []string{}}
	}
	t := f.Target
	t.Env = env
	if f.HTTPS != nil && len(f.HTTPS.HealthCandidates) > 0 {
		t.HealthCheck = f.HTTPS.HealthCandidates[0]
	}
	db := f.DB
	if db == nil {
		return nil
	}
	var size int64
	var appDB *facts.Database
	for i, d := range db.Databases {
		size += d.SizeBytes
		if d.Name == db.Database {
			appDB = &db.Databases[i]
		}
	}
	t.StorageSize = storageSize(size)

	major, minor := db.ServerVersionNum/10000, db.ServerVersionNum%10000
	if major < 13 {
		r.block(facts.BlockNoImage, strconv.Itoa(db.ServerVersionNum), "PostgreSQL %d is older than the CNPG images (13 and later)", db.ServerVersionNum)
		return nil
	}
	icu := appDB != nil && appDB.LocProvider == "i"
	for _, d := range db.Databases {
		if d.LocProvider == "i" {
			icu = true
		}
	}
	debian := db.Debian
	switch {
	case db.Libc == facts.LibcGlibc && debian != "":
		t.ImageReason = "glibc source on Debian " + debian + ": the image of the same Debian release"
	case db.Libc == facts.LibcMusl:
		debian = "bookworm"
		t.ImageReason = "musl source: -standard-bookworm"
	default:
		debian = "bookworm"
		t.ImageReason = "glibc source of unknown release: -standard-bookworm"
		r.warn(facts.WarnGlibcRelease, db.Version, "the source's glibc release is not known; collation version warnings may remain (ALTER DATABASE ... REFRESH COLLATION VERSION after the cutover)")
	}
	for _, i := range db.Indexes {
		icu = icu || strings.HasPrefix(i.Collation, "icu:")
	}
	if icu && libcBreaks(db) {
		r.block(facts.BlockICUVersion, db.Database, "the database uses the ICU provider and no CNPG image is known to have the source's ICU version")
	}

	// REINDEX when the target's libc sorts differently: the indexes of a
	// libc collation that is not C-like.
	if libcBreaks(db) {
		dbLocale := ""
		if appDB != nil && appDB.LocProvider != "b" && appDB.LocProvider != "i" {
			dbLocale = appDB.Collate
		}
		for _, i := range db.Indexes {
			if strings.HasPrefix(i.Collation, "icu:") || cLike(i.Collation) {
				continue
			}
			t.Reindex = append(t.Reindex, i.Name)
		}
		if len(t.Reindex) > 0 || !cLike(dbLocale) {
			t.ImageReason += fmt.Sprintf("; libc locale %s: indexes are rebuilt after the promotion", cmp.Or(dbLocale, "of an index"))
		}
		for _, c := range db.CollationDependent {
			r.block(facts.BlockCollationConstraint, c, "%s depends on a libc collation that sorts differently on the target; REINDEX cannot fix it", c)
		}
		if len(t.Reindex) > 0 {
			r.warn(facts.WarnReindex, strings.Join(t.Reindex, ","), "%d index(es) are rebuilt on the target after the promotion", len(t.Reindex))
		}
	}

	tags := imageTags(major, minor, debian)
	for _, tag := range tags {
		d, err := images.Digest(ctx, tag)
		if errors.Is(err, ErrNoImage) {
			continue
		}
		if err != nil {
			return err
		}
		t.Image = ImageRepo + ":" + tag + "@" + d
		return nil
	}
	r.block(facts.BlockNoImage, strings.Join(tags, ","), "no CNPG image %s exists", strings.Join(tags, " or "))
	return nil
}

// imageTags are the CNPG tags for a major.minor and Debian release, the
// exact minor first.
func imageTags(major, minor int, debian string) []string {
	suffix := "-standard-" + debian
	tags := []string{fmt.Sprintf("%d.%d%s", major, minor, suffix), fmt.Sprintf("%d%s", major, suffix)}
	if debian == "bullseye" { // the default tags are bullseye too
		tags = append(tags, fmt.Sprintf("%d.%d", major, minor), strconv.Itoa(major))
	}
	return tags
}

// storageSize is twice the size, at least 1Gi, in whole Gi.
func storageSize(bytes int64) string {
	const gi = 1 << 30
	n := (2*bytes + gi - 1) / gi
	return strconv.FormatInt(max(n, 1), 10) + "Gi"
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefInt(n *int64) int64 {
	if n == nil {
		return 0
	}
	return *n
}
