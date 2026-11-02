package agentlog

import (
	"bytes"
	"encoding/json"
	"testing"
)

func redactJSON(t *testing.T, in string) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(in)))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("%s: %v", in, err)
	}
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(Redact(v)); err != nil {
		t.Fatal(err)
	}
	return string(bytes.TrimSuffix(out.Bytes(), []byte("\n")))
}

func TestRedact(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"keys",
			`{"query":"select 1","DB_PASSWORD":"hunter2","apiKey":"k","api-key":"k","Authorization":"Bearer x","passwd":"p1","private_key":"pk","pwd":"p2","Cookie":"sid=1","access_key":"a","auth":{"token":"t","user":"u"}}`,
			`{"Authorization":"[redacted]","Cookie":"[redacted]","DB_PASSWORD":"[redacted]","access_key":"[redacted]","api-key":"[redacted]","apiKey":"[redacted]","auth":{"token":"[redacted]","user":"u"},"passwd":"[redacted]","private_key":"[redacted]","pwd":"[redacted]","query":"select 1"}`},
		{"every string below a secret key",
			`{"nested":[{"client_secret":{"value":"s","n":1},"ok":1}],"credentials":{"pass":"p","port":5432,"hosts":["a"]},"credentials.pass":"q"}`,
			`{"credentials":{"hosts":["[redacted]"],"pass":"[redacted]","port":5432},"credentials.pass":"[redacted]","nested":[{"client_secret":{"n":1,"value":"[redacted]"},"ok":1}]}`},
		{"pass only at the end of a key",
			`{"pass":"a","sudo_pass":"b","passive":"yes","passes":"no"}`,
			`{"pass":"[redacted]","passes":"no","passive":"yes","sudo_pass":"[redacted]"}`},
		{"numbers and booleans under secret keys stay",
			`{"usage":{"input_tokens":900,"max_tokens":1024,"cache_read_input_tokens":0},"token_valid":true,"tokens":null,"n":12345678901234567890}`,
			`{"n":12345678901234567890,"token_valid":true,"tokens":null,"usage":{"cache_read_input_tokens":0,"input_tokens":900,"max_tokens":1024}}`},
		{"name and value pairs",
			`{"env":[{"name":"DB_PASSWORD","value":"hunter2"},{"name":"LOG_LEVEL","value":"debug"}],"Tags":[{"Key":"api_token","Value":"t0k"}]}`,
			`{"Tags":[{"Key":"api_token","Value":"[redacted]"}],"env":[{"name":"DB_PASSWORD","value":"[redacted]"},{"name":"LOG_LEVEL","value":"debug"}]}`},
		{"assignment in output",
			`{"stdout":"PGPASSWORD=hunter3 psql -h db -U app"}`,
			`{"stdout":"PGPASSWORD=[redacted] psql -h db -U app"}`},
		{"libpq conninfo",
			`"host=db user=app password=s3cret dbname=app"`,
			`"host=db user=app password=[redacted] dbname=app"`},
		{"quoted and YAML values",
			`["export API_KEY='a b c'", "password: hunter4", "{\"password\":\"x\",\"user\":\"app\"}"]`,
			`["export API_KEY='[redacted]'","password: [redacted]","{\"password\":\"[redacted]\",\"user\":\"app\"}"]`},
		{"query string",
			`"https://x.test/cb?access_token=abc&state=1"`,
			`"https://x.test/cb?access_token=[redacted]&state=1"`},
		{"URL password",
			`{"dsn":"postgres://u:h5@host/db","line":"dsn: postgres://u:h5@host/db","no":"https://host:8443/a@b"}`,
			`{"dsn":"postgres://u:[redacted]@host/db","line":"dsn: postgres://u:[redacted]@host/db","no":"https://host:8443/a@b"}`},
		{"variable references are kept",
			`{"dsn":"postgres://app:${SRC_PGPASSWORD}@db/app","cmd":"PGPASSWORD=${DST_PGPASSWORD} psql"}`,
			`{"cmd":"PGPASSWORD=${DST_PGPASSWORD} psql","dsn":"postgres://app:${SRC_PGPASSWORD}@db/app"}`},
		{"bearer token",
			`["sent Bearer eyJhbGciOi.abc-def_1", "curl -H 'Authorization: Bearer abc' https://x.test"]`,
			`["sent Bearer [redacted]","curl -H 'Authorization: [redacted]' https://x.test"]`},
		{"token counts in text",
			`"max_tokens=1024 api_key=abc"`,
			`"max_tokens=1024 api_key=[redacted]"`},
		{"AWS access key id",
			`"using AKIAIOSFODNN7EXAMPLE and ASIAIOSFODNN7EXAMPLE"`,
			`"using [redacted] and [redacted]"`},
		{"PEM private key",
			`["-----BEGIN RSA PRIVATE KEY-----\nMIIEow\n-----END RSA PRIVATE KEY-----\nrest", "cut -----BEGIN PRIVATE KEY-----\nMIIEv"]`,
			`["[redacted]\nrest","cut [redacted]"]`},
		{"already redacted",
			`"password=[redacted] Bearer [redacted]"`,
			`"password=[redacted] Bearer [redacted]"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := redactJSON(t, c.in); got != c.want {
				t.Errorf("redacted:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}

func TestRedactDoesNotModifyItsInput(t *testing.T) {
	in := map[string]any{"DB_PASSWORD": "hunter2", "list": []any{"PGPASSWORD=x"}}
	Redact(in)
	if in["DB_PASSWORD"] != "hunter2" || in["list"].([]any)[0] != "PGPASSWORD=x" {
		t.Fatalf("Redact modified its input: %v", in)
	}
}
