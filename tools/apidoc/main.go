// Command apidoc turns api/openapi.yaml into the team's Notion "API 명세서" page.
//
// It reads the contract at a git revision (origin/main by default), fills
// template.md with tables built from the contract and the Korean text in
// apidoc.yaml, and writes the page to out/page.md and, split into pieces small
// enough for one Notion MCP call each, to out/chunk-NN.md. Anything the
// contract has but apidoc.yaml does not is shown in English and listed as
// missing.
//
// Run it from the repository root:
//
//	go run ./tools/apidoc [-rev origin/main]
//
// The api-doc Claude Code skill (.claude/skills/api-doc) runs this and uploads
// the chunks.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

const specPath = "api/openapi.yaml"

var kst = time.FixedZone("KST", 9*60*60)

type config struct {
	NotionPage   string            `yaml:"notionPage"`
	Repo         string            `yaml:"repo"`
	Server       string            `yaml:"server"`
	Tags         []tag             `yaml:"tags"`
	Operations   yaml.Node         `yaml:"operations"`
	Params       map[string]string `yaml:"params"`
	SchemaGroups []struct {
		Title   string   `yaml:"title"`
		Schemas []string `yaml:"schemas"`
	} `yaml:"schemaGroups"`
	Schemas map[string]string            `yaml:"schemas"`
	Fields  map[string]string            `yaml:"fields"`
	Enums   map[string]map[string]string `yaml:"enums"`
}

type tag struct {
	Name  string `yaml:"name"`
	Title string `yaml:"title"`
}

type opText struct {
	Summary string `yaml:"summary"`
	Role    string `yaml:"role"`
	Notes   string `yaml:"notes"`
}

// meta is what the page header says about the documented contract.
type meta struct {
	Commit, Short, Subject string
	CommitTime             time.Time
	Now                    time.Time
	ServerNote             string // the 배포 서버 row; empty leaves the row out
}

func main() {
	rev := flag.String("rev", "origin/main", "git revision whose "+specPath+" is documented")
	dir := flag.String("dir", "tools/apidoc", "directory holding apidoc.yaml and template.md")
	out := flag.String("out", "tools/apidoc/out", "output directory")
	limit := flag.Int("chunk", 20000, "maximum bytes per chunk")
	flag.Parse()
	if err := run(*rev, *dir, *out, *limit); err != nil {
		fmt.Fprintln(os.Stderr, "apidoc:", err)
		os.Exit(1)
	}
}

func run(rev, dir, out string, limit int) error {
	spec, err := git("show", rev+":"+specPath)
	if err != nil {
		return err
	}
	logLine, err := git("log", "-1", "--format=%H%x00%h%x00%s%x00%at", rev, "--", specPath)
	if err != nil {
		return err
	}
	f := strings.Split(strings.TrimSpace(string(logLine)), "\x00")
	if len(f) != 4 {
		return fmt.Errorf("no commit touches %s at %s", specPath, rev)
	}
	at, err := strconv.ParseInt(f[3], 10, 64)
	if err != nil {
		return err
	}
	cfg, err := loadConfig(filepath.Join(dir, "apidoc.yaml"))
	if err != nil {
		return err
	}
	tpl, err := os.ReadFile(filepath.Join(dir, "template.md"))
	if err != nil {
		return err
	}
	m := meta{Commit: f[0], Short: f[1], Subject: f[2], CommitTime: time.Unix(at, 0).In(kst), Now: time.Now().In(kst)}
	serverResult := "not checked (no server in apidoc.yaml)"
	if cfg.Server != "" {
		serverResult, m.ServerNote = compareServer(cfg.Server, spec, m.Now)
	}

	page, missing, err := render(spec, cfg, string(tpl), m)
	if err != nil {
		return err
	}
	chunks := split(page, limit)

	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	old, _ := filepath.Glob(filepath.Join(out, "chunk-*.md"))
	for _, p := range old {
		if err := os.Remove(p); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(out, "page.md"), []byte(page), 0o644); err != nil {
		return err
	}
	fmt.Printf("documented: %s at %s (%s KST) %s\n", specPath, m.Short, m.CommitTime.Format("2006-01-02 15:04"), m.Subject)
	fmt.Printf("notion page: %s\n", cfg.NotionPage)
	fmt.Printf("deployed server: %s\n", serverResult)
	fmt.Printf("wrote %s (%d bytes) and %d chunks, upload in this order:\n", filepath.Join(out, "page.md"), len(page), len(chunks))
	for i, c := range chunks {
		p := filepath.Join(out, fmt.Sprintf("chunk-%02d.md", i+1))
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			return err
		}
		how := "insert_content at end"
		if i == 0 {
			how = "replace_content"
		}
		fmt.Printf("  %s  %6d bytes  %s\n", p, len(c), how)
	}
	for _, w := range lint(page) {
		fmt.Println("warning:", w)
	}
	if len(missing) == 0 {
		fmt.Println("missing Korean text: none")
	} else {
		fmt.Printf("missing Korean text in apidoc.yaml (%d, shown in English on the page):\n", len(missing))
		for _, k := range missing {
			fmt.Println("  " + k)
		}
	}
	return nil
}

func git(args ...string) ([]byte, error) {
	cmd := exec.Command("git", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return b, nil
}

func loadConfig(p string) (*config, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var c config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	return &c, nil
}

// compareServer fetches the deployed contract and says whether it is the
// documented one.
func compareServer(server string, spec []byte, now time.Time) (result, note string) {
	day := fmt.Sprintf("%d/%d", now.Month(), now.Day())
	norm := func(b []byte) []byte { return bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")) }
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get(strings.TrimRight(server, "/") + "/openapi.yaml")
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			err = errors.New(resp.Status)
		}
	}
	var served []byte
	if err == nil {
		served, err = io.ReadAll(resp.Body)
	}
	switch {
	case err != nil:
		return "unreachable: " + err.Error(), fmt.Sprintf("%s (%s에 이 서버의 /openapi.yaml을 받지 못해 비교하지 못했다)", server, day)
	case bytes.Equal(norm(served), norm(spec)):
		return "same as the documented commit", fmt.Sprintf("%s (이 서버의 /openapi.yaml이 기준 커밋의 파일과 같음을 %s에 확인했다)", server, day)
	default:
		return "DIFFERENT from the documented commit", fmt.Sprintf("%s (%s에 확인했을 때 이 서버의 /openapi.yaml은 기준 커밋의 파일과 달랐다)", server, day)
	}
}

// ---------- YAML helpers ----------

func get(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func str(n *yaml.Node, key string) string {
	if v := get(n, key); v != nil {
		return v.Value
	}
	return ""
}

// each calls fn for every key and value of a mapping, in file order.
func each(n *yaml.Node, fn func(k string, v *yaml.Node)) {
	if n == nil || n.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		fn(n.Content[i].Value, n.Content[i+1])
	}
}

func values(n *yaml.Node) []string {
	var out []string
	if n == nil {
		return nil
	}
	if n.Kind != yaml.SequenceNode {
		return []string{n.Value}
	}
	for _, v := range n.Content {
		out = append(out, v.Value)
	}
	return out
}

func refName(r string) string { return r[strings.LastIndex(r, "/")+1:] }

func typ(s *yaml.Node) string {
	if s == nil {
		return "any"
	}
	if r := str(s, "$ref"); r != "" {
		return refName(r)
	}
	switch t := str(s, "type"); t {
	case "array":
		return typ(get(s, "items")) + "[]"
	case "object":
		if ap := get(s, "additionalProperties"); ap != nil && ap.Kind == yaml.MappingNode {
			return "map<string, " + typ(ap) + ">"
		}
		return "object"
	case "":
		return "any (JSON)"
	default:
		if f := str(s, "format"); f == "date-time" || f == "binary" {
			return f
		}
		if get(s, "enum") != nil {
			return "string enum"
		}
		return t
	}
}

func constraints(s *yaml.Node) []string {
	var out []string
	if s == nil {
		return nil
	}
	if e := get(s, "enum"); e != nil {
		out = append(out, "값: "+strings.Join(values(e), ", "))
	}
	if d := get(s, "default"); d != nil {
		out = append(out, "기본값 "+d.Value)
	}
	for _, c := range []struct{ key, label string }{{"minimum", "최소"}, {"maximum", "최대"}, {"maxLength", "최대 길이"}, {"minItems", "최소 개수"}, {"maxItems", "최대 개수"}} {
		if v := get(s, c.key); v != nil {
			out = append(out, c.label+" "+v.Value)
		}
	}
	if ex := get(s, "example"); ex != nil {
		out = append(out, "예: "+strings.Join(values(ex), ", "))
	}
	return out
}

func esc(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "<", "&lt;")
	return strings.ReplaceAll(s, "$", "&#36;")
}

func joinNonEmpty(parts []string, sep string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

// ---------- rendering ----------

type operation struct {
	tag, method, path, id string
	op                    *yaml.Node
	params                []*yaml.Node
}

type renderer struct {
	cfg        *config
	root       *yaml.Node
	schemas    *yaml.Node
	ops        []operation
	text       map[string]opText
	missing    []string
	missingSet map[string]bool
}

func (r *renderer) miss(key string) {
	if !r.missingSet[key] {
		r.missingSet[key] = true
		r.missing = append(r.missing, key)
	}
}

// render fills the template and returns the page and the keys apidoc.yaml lacks.
func render(spec []byte, cfg *config, tpl string, m meta) (string, []string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(spec, &doc); err != nil {
		return "", nil, fmt.Errorf("%s: %w", specPath, err)
	}
	if len(doc.Content) == 0 {
		return "", nil, fmt.Errorf("%s is empty", specPath)
	}
	r := &renderer{cfg: cfg, root: doc.Content[0], text: map[string]opText{}, missingSet: map[string]bool{}}
	r.schemas = get(get(r.root, "components"), "schemas")
	opOrder := map[string]int{}
	for i := 0; i+1 < len(cfg.Operations.Content); i += 2 {
		var t opText
		if err := cfg.Operations.Content[i+1].Decode(&t); err != nil {
			return "", nil, fmt.Errorf("apidoc.yaml operations.%s: %w", cfg.Operations.Content[i].Value, err)
		}
		r.text[cfg.Operations.Content[i].Value] = t
		opOrder[cfg.Operations.Content[i].Value] = i / 2
	}
	r.collectOps(opOrder)

	var err error
	page := regexp.MustCompile(`\{\{([^{}]+)\}\}`).ReplaceAllStringFunc(tpl, func(p string) string {
		name := p[2 : len(p)-2]
		switch {
		case name == "meta":
			return r.meta(m)
		case name == "endpoint_count":
			return strconv.Itoa(len(r.ops))
		case name == "endpoint_table":
			return r.endpointTable()
		case name == "endpoint_details":
			return r.endpointDetails()
		case name == "schema_groups":
			return r.schemaGroups()
		case name == "enum_list":
			return r.enumList()
		case strings.HasPrefix(name, "enum:"):
			f := strings.SplitN(name, ":", 4)
			if len(f) != 4 {
				err = fmt.Errorf("template: %s needs the form {{enum:Name:column:column}}", p)
				return p
			}
			return r.enumTable(f[1], f[2], f[3])
		}
		err = fmt.Errorf("template: unknown placeholder %s", p)
		return p
	})
	if err != nil {
		return "", nil, err
	}
	return page, r.missing, nil
}

func (r *renderer) collectOps(opOrder map[string]int) {
	tagIdx := map[string]int{}
	for i, t := range r.cfg.Tags {
		tagIdx[t.Name] = i
	}
	params := get(get(r.root, "components"), "parameters")
	resolve := func(p *yaml.Node) *yaml.Node {
		if ref := str(p, "$ref"); ref != "" {
			return get(params, refName(ref))
		}
		return p
	}
	each(get(r.root, "paths"), func(path string, item *yaml.Node) {
		var common []*yaml.Node
		if ps := get(item, "parameters"); ps != nil {
			common = ps.Content
		}
		for _, method := range []string{"get", "post", "put", "patch", "delete"} {
			op := get(item, method)
			if op == nil {
				continue
			}
			o := operation{method: strings.ToUpper(method), path: path, id: str(op, "operationId"), op: op}
			if tags := get(op, "tags"); tags != nil && len(tags.Content) > 0 {
				o.tag = tags.Content[0].Value
			}
			for _, p := range common {
				o.params = append(o.params, resolve(p))
			}
			if ps := get(op, "parameters"); ps != nil {
				for _, p := range ps.Content {
					o.params = append(o.params, resolve(p))
				}
			}
			if _, ok := tagIdx[o.tag]; !ok {
				r.miss("tags." + o.tag)
				tagIdx[o.tag] = len(tagIdx)
				r.cfg.Tags = append(r.cfg.Tags, tag{o.tag, o.tag})
			}
			r.ops = append(r.ops, o)
		}
	})
	// Tag order, then apidoc.yaml order; operations apidoc.yaml lacks come last
	// in contract order.
	pos := func(o operation) int {
		if i, ok := opOrder[o.id]; ok {
			return i
		}
		return len(opOrder)
	}
	sort.SliceStable(r.ops, func(i, j int) bool {
		a, b := r.ops[i], r.ops[j]
		if tagIdx[a.tag] != tagIdx[b.tag] {
			return tagIdx[a.tag] < tagIdx[b.tag]
		}
		return pos(a) < pos(b)
	})
}

// opInfo returns the Korean text of an operation, falling back to the contract.
func (r *renderer) opInfo(o operation) opText {
	t, ok := r.text[o.id]
	if !ok {
		r.miss("operations." + o.id)
	}
	if t.Summary == "" {
		t.Summary = o.id
	}
	if t.Notes == "" {
		t.Notes = strings.TrimSpace(str(o.op, "description"))
	}
	if t.Role == "" {
		t.Role = inferRole(o)
	}
	return t
}

var roleRe = regexp.MustCompile(`Roles?:?\s+([a-z]+(?:,\s*[a-z]+)*)`)

func inferRole(o operation) string {
	if sec := get(o.op, "security"); sec != nil && len(sec.Content) == 0 {
		return "인증 없음"
	}
	if m := roleRe.FindStringSubmatch(str(o.op, "description")); m != nil {
		return m[1]
	}
	if o.method == "GET" {
		return "모든 역할"
	}
	return "확인 필요"
}

func (r *renderer) meta(m meta) string {
	info := get(r.root, "info")
	repo := strings.TrimRight(r.cfg.Repo, "/")
	rows := []string{
		"| 항목 | 내용 |",
		"|---|---|",
		fmt.Sprintf("| 원본 파일 | [%s/%s](%s/blob/%s/%s) |", path.Base(repo), specPath, repo, m.Commit, specPath),
		fmt.Sprintf("| 기준 커밋 | [%s](%s/commit/%s) %s |", m.Short, repo, m.Commit, esc(m.Subject)),
		fmt.Sprintf("| 커밋 시각 | %s (KST) |", m.CommitTime.Format("2006-01-02 15:04")),
		fmt.Sprintf(`| 페이지 최신화 | <date start="%s" start-time="%s" time-zone="Asia/Seoul" /> |`, m.Now.Format("2006-01-02"), m.Now.Format("15:04")),
		fmt.Sprintf("| 스펙 버전 | OpenAPI %s, %s %s |", str(r.root, "openapi"), str(info, "title"), str(info, "version")),
	}
	if m.ServerNote != "" {
		rows = append(rows, "| 배포 서버 | "+esc(m.ServerNote)+" |")
	}
	return strings.Join(rows, "\n")
}

var methodColor = map[string]string{"GET": "blue_bg", "POST": "green_bg", "PUT": "orange_bg", "PATCH": "purple_bg", "DELETE": "red_bg"}

func (r *renderer) endpointTable() string {
	l := []string{
		`<table fit-page-width="true" header-row="true">`,
		`<colgroup><col width="110" /><col width="70" /><col width="300" /><col width="150" /><col /></colgroup>`,
		"<tr><th>그룹</th><th>메서드</th><th>경로</th><th>역할</th><th>설명</th></tr>",
	}
	for _, o := range r.ops {
		t := r.opInfo(o)
		l = append(l, fmt.Sprintf(`<tr><td>%s</td><td color="%s">%s</td><td>`+"`%s`"+`</td><td>%s</td><td>%s</td></tr>`,
			o.tag, methodColor[o.method], o.method, o.path, t.Role, t.Summary))
	}
	l = append(l, "</table>")
	return strings.Join(l, "\n")
}

var errName = map[string]string{"BadRequest": "400", "Unauthorized": "401", "Forbidden": "403", "NotFound": "404", "Conflict": "409", "TooLarge": "413", "AppSpecInvalid": "422", "TooManyRequests": "429", "BadGateway": "502", "Internal": "500", "Unavailable": "503", "NotImplemented": "501"}

func (r *renderer) endpointDetails() string {
	var l []string
	w := func(s ...string) { l = append(l, s...) }
	for ti, tag := range r.cfg.Tags {
		var ops []operation
		for _, o := range r.ops {
			if o.tag == tag.Name {
				ops = append(ops, o)
			}
		}
		if len(ops) == 0 {
			continue
		}
		if ti > 0 && len(l) > 0 {
			w("")
		}
		w("### "+tag.Title, "")
		for i, o := range ops {
			t := r.opInfo(o)
			if i > 0 {
				w("")
			}
			w("<toggle>", fmt.Sprintf("<b>%s</b> `%s`  %s", o.method, o.path, t.Summary), "")
			w("- operationId: `"+o.id+"`", "- 역할: "+t.Role, "")
			w(t.Notes, "")
			if len(o.params) > 0 {
				w("| 파라미터 | 위치 | 타입 | 필수 | 설명 |", "|---|---|---|---|---|")
				for _, p := range o.params {
					name := str(p, "name")
					d, ok := r.cfg.Params[name+"@"+o.id]
					if !ok {
						d, ok = r.cfg.Params[name]
					}
					if !ok {
						r.miss("params." + name + "@" + o.id)
						d = str(p, "description")
					}
					req := ""
					if str(p, "required") == "true" {
						req = "필수"
					}
					d = joinNonEmpty(append([]string{d}, constraints(get(p, "schema"))...), "; ")
					w(fmt.Sprintf("| `%s` | %s | `%s` | %s | %s |", name, str(p, "in"), typ(get(p, "schema")), req, esc(d)))
				}
				w("")
			}
			if rb := get(o.op, "requestBody"); rb != nil {
				ct, sch := firstContent(get(rb, "content"))
				need := "선택"
				if str(rb, "required") == "true" {
					need = "필수"
				}
				w(fmt.Sprintf("요청 본문: `%s` (%s, %s)", typ(sch), ct, need), "")
			}
			var oks, errs []string
			each(get(o.op, "responses"), func(code string, resp *yaml.Node) {
				if ref := str(resp, "$ref"); ref != "" {
					errs = append(errs, errName[refName(ref)])
					return
				}
				var parts []string
				each(get(resp, "content"), func(ct string, v *yaml.Node) {
					parts = append(parts, fmt.Sprintf("`%s` (%s)", typ(get(v, "schema")), ct))
				})
				body := "본문 없음"
				if len(parts) > 0 {
					body = strings.Join(parts, " 또는 ")
				}
				if get(get(resp, "headers"), "Location") != nil {
					body += " + Location 헤더"
				}
				oks = append(oks, fmt.Sprintf("`%s` %s", code, body))
			})
			w("성공 응답: " + strings.Join(oks, ", "))
			if len(errs) > 0 {
				errs = sortedUnique(errs)
				for i, e := range errs {
					errs[i] = "`" + e + "`"
				}
				w("", "오류 응답: "+strings.Join(errs, ", "))
			}
			w("</toggle>")
		}
	}
	return strings.Join(l, "\n")
}

func firstContent(c *yaml.Node) (string, *yaml.Node) {
	if c == nil || len(c.Content) < 2 {
		return "", nil
	}
	return c.Content[0].Value, get(c.Content[1], "schema")
}

func sortedUnique(s []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range s {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

func isEnum(s *yaml.Node) bool { return get(s, "enum") != nil && get(s, "properties") == nil }

func (r *renderer) fieldRows(schema string, s *yaml.Node, prefix string) [][4]string {
	var rows [][4]string
	req := map[string]bool{}
	for _, v := range values(get(s, "required")) {
		req[v] = true
	}
	each(get(s, "properties"), func(f string, fs *yaml.Node) {
		key := schema + "." + prefix + f
		d, ok := r.cfg.Fields[key]
		if !ok {
			if d = str(fs, "description"); d != "" {
				r.miss("fields." + key)
			}
		}
		need := ""
		if req[f] {
			need = "필수"
		}
		rows = append(rows, [4]string{prefix + f, typ(fs), need, joinNonEmpty(append([]string{d}, constraints(fs)...), "; ")})
		if str(fs, "type") == "object" && get(fs, "properties") != nil {
			rows = append(rows, r.fieldRows(schema, fs, prefix+f+".")...)
		}
	})
	return rows
}

func (r *renderer) schemaGroups() string {
	type group struct {
		title   string
		schemas []string
	}
	var groups []group
	listed := map[string]bool{}
	for _, g := range r.cfg.SchemaGroups {
		groups = append(groups, group{g.Title, g.Schemas})
		for _, n := range g.Schemas {
			listed[n] = true
		}
	}
	var rest []string
	each(r.schemas, func(n string, s *yaml.Node) {
		if !listed[n] && !isEnum(s) {
			r.miss("schemaGroups: " + n)
			rest = append(rest, n)
		}
	})
	if len(rest) > 0 {
		groups = append(groups, group{"기타", rest})
	}
	var l []string
	w := func(s ...string) { l = append(l, s...) }
	for gi, g := range groups {
		if gi > 0 {
			w("")
		}
		w("### "+g.title, "")
		for i, n := range g.schemas {
			s := get(r.schemas, n)
			if s == nil {
				continue // listed in apidoc.yaml but gone from the contract
			}
			if i > 0 {
				w("")
			}
			w("<toggle>", "<b>"+n+"</b>", "")
			d, ok := r.cfg.Schemas[n]
			if !ok {
				if d = strings.TrimSpace(str(s, "description")); d != "" {
					r.miss("schemas." + n)
				}
			}
			if d != "" {
				w(d, "")
			}
			if rows := r.fieldRows(n, s, ""); len(rows) > 0 {
				w("| 필드 | 타입 | 필수 | 설명 |", "|---|---|---|---|")
				for _, row := range rows {
					w(fmt.Sprintf("| `%s` | `%s` | %s | %s |", row[0], row[1], row[2], esc(row[3])))
				}
			}
			w("</toggle>")
		}
	}
	return strings.Join(l, "\n")
}

func (r *renderer) enumList() string {
	l := []string{"| 이름 | 값 |", "|---|---|"}
	each(r.schemas, func(n string, s *yaml.Node) {
		if !isEnum(s) {
			return
		}
		var vs []string
		for _, v := range values(get(s, "enum")) {
			vs = append(vs, "`"+v+"`")
		}
		l = append(l, fmt.Sprintf("| `%s` | %s |", n, strings.Join(vs, ", ")))
	})
	return strings.Join(l, "\n")
}

func (r *renderer) enumTable(name, col1, col2 string) string {
	l := []string{"| " + col1 + " | " + col2 + " |", "|---|---|"}
	for _, v := range values(get(get(r.schemas, name), "enum")) {
		d, ok := r.cfg.Enums[name][v]
		if !ok {
			r.miss("enums." + name + "." + v)
		}
		l = append(l, fmt.Sprintf("| `%s` | %s |", v, esc(d)))
	}
	return strings.Join(l, "\n")
}

// ---------- checks and chunking ----------

var (
	fenceRe   = regexp.MustCompile("(?s)```.*?```")
	notNL     = regexp.MustCompile(`[^\n]`)
	codeRe    = regexp.MustCompile("`[^`\n]*`")
	knownTags = regexp.MustCompile(`</?(toggle|table|tr|td|th|b|colgroup|col|table-of-contents|date)\b[^>]*>`)
)

// lint reports text Notion would read as a tag or as math: "<" and "$"
// outside code. Wrap such text in backticks in apidoc.yaml or template.md.
func lint(page string) []string {
	blank := func(s string) string { return strings.Repeat(" ", len(s)) }
	t := fenceRe.ReplaceAllStringFunc(page, func(s string) string {
		return notNL.ReplaceAllString(s, " ")
	})
	t = codeRe.ReplaceAllStringFunc(t, blank)
	t = knownTags.ReplaceAllStringFunc(t, blank)
	var out []string
	for i, line := range strings.Split(t, "\n") {
		if strings.ContainsAny(line, "<$") {
			out = append(out, fmt.Sprintf("page.md line %d: '<' or '$' outside code: %s", i+1, strings.Split(page, "\n")[i]))
		}
	}
	return out
}

// split cuts the page into chunks of at most limit bytes, only before a
// heading or a toggle that starts at the top level, so no table, toggle or
// code block is cut. A single block larger than limit becomes its own chunk.
func split(page string, limit int) []string {
	lines := strings.Split(strings.TrimRight(page, "\n"), "\n")
	var blocks []string
	start, depth, inFence, inTable := 0, 0, false, false
	for i, line := range lines {
		boundary := !inFence && !inTable && depth == 0 && i > 0 && lines[i-1] == "" &&
			(strings.HasPrefix(line, "## ") || strings.HasPrefix(line, "### ") || line == "<toggle>")
		if boundary {
			blocks = append(blocks, strings.Join(lines[start:i], "\n"))
			start = i
		}
		switch {
		case strings.HasPrefix(line, "```"):
			inFence = !inFence
		case inFence:
		case line == "<toggle>":
			depth++
		case line == "</toggle>":
			depth--
		case strings.HasPrefix(line, "<table "), line == "<table>":
			inTable = true
		case line == "</table>":
			inTable = false
		}
	}
	blocks = append(blocks, strings.Join(lines[start:], "\n"))

	var chunks []string
	cur := ""
	for _, b := range blocks {
		if cur != "" && len(cur)+len(b) > limit {
			chunks = append(chunks, strings.TrimRight(cur, "\n"))
			cur = ""
		}
		cur += b + "\n"
	}
	if strings.TrimSpace(cur) != "" {
		chunks = append(chunks, strings.TrimRight(cur, "\n"))
	}
	return chunks
}
