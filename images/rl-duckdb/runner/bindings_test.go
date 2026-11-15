package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests hold bindings.go to its contract with the fake engine (the test binary, main_test.go) and to the plan's rules about what a descriptor may say, what a value may be, what the
// script looks like, and what is left in a log. What only the real engine can say is in bindings_engine_test.go.

// ---- the rig --------------------------------------------------------------------------------------------------------------------------------------------------

// bindRig is the directory the control plane would mount at /run/rl: the descriptor, and the secrets directory beside it. It is NOT the work directory.
type bindRig struct {
	t  *testing.T
	rl string
}

func newBindRig(t *testing.T) *bindRig {
	t.Helper()
	r := &bindRig{t: t, rl: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(r.rl, "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *bindRig) file() string       { return filepath.Join(r.rl, "bindings.json") }
func (r *bindRig) secretsDir() string { return filepath.Join(r.rl, "secrets") }

func (r *bindRig) write(doc string) {
	r.t.Helper()
	if err := os.WriteFile(r.file(), []byte(doc), 0o644); err != nil {
		r.t.Fatal(err)
	}
}

func (r *bindRig) secret(name string, v []byte) {
	r.t.Helper()
	_ = os.Remove(filepath.Join(r.secretsDir(), name))
	if err := os.WriteFile(filepath.Join(r.secretsDir(), name), v, 0o440); err != nil {
		r.t.Fatal(err)
	}
}

func (r *bindRig) env(extra map[string]string) map[string]string {
	env := map[string]string{"RL_BINDINGS_FILE": r.file()}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// proofEnv is the environment of a runner that may make the http bearer type.
func proofEnv() map[string]string { return map[string]string{"RL_BINDINGS_PROOF": "1"} }

const stubScope = "http://127.0.0.1:18080/"

// httpDesc is a descriptor of one http binding named name, with an optional access object spliced in (empty for none).
func httpDesc(name, access string) string {
	acc := ""
	if access != "" {
		acc = `"access":` + access + `,`
	}
	return fmt.Sprintf(`{"v":1,%s"bindings":[{"binding":%q,"type":"http","scope":%q,"http_proxy":"direct"}]}`, acc, name, stubScope)
}

func loadFrom(r *bindRig, doc string, mod func(*config)) (*bindingsDescriptor, error) {
	r.write(doc)
	c := config{bindingsFile: r.file(), workDir: "/work", resultFile: "/work/result.parquet"}
	if mod != nil {
		mod(&c)
	}
	return loadBindings(c)
}

// pg is one postgres binding with the given extra or replaced fields (a JSON fragment), for the field corpus.
func pg(fields string) string {
	base := `"binding":"source_1","type":"postgres","host":"db.example.com","port":5432,"database":"app","user":"rl_reader"`
	return `{"v":1,"bindings":[{` + base + fields + `}]}`
}

// pgWith replaces one field of the valid postgres binding.
func pgWith(key, jsonValue string) string {
	fields := map[string]string{"binding": `"source_1"`, "type": `"postgres"`, "host": `"db.example.com"`, "port": `5432`, "database": `"app"`, "user": `"rl_reader"`}
	if jsonValue == "" {
		delete(fields, key)
	} else {
		fields[key] = jsonValue
	}
	var parts []string
	for _, k := range []string{"binding", "type", "host", "port", "database", "user"} {
		if v, ok := fields[k]; ok {
			parts = append(parts, fmt.Sprintf("%q:%s", k, v))
		}
	}
	for _, k := range []string{"tls", "scope", "http_proxy"} {
		if v, ok := fields[k]; ok {
			parts = append(parts, fmt.Sprintf("%q:%s", k, v))
		}
	}
	return `{"v":1,"bindings":[{` + strings.Join(parts, ",") + `}]}`
}

func jq(s string) string { b, _ := json.Marshal(s); return string(b) }

// ---- the loader: the shape ------------------------------------------------------------------------------------------------------------------------------------

func TestTheDescriptorIsHeldToItsShape(t *testing.T) {
	r := newBindRig(t)
	many := func(n int) string {
		var bs []string
		for i := 0; i < n; i++ {
			bs = append(bs, fmt.Sprintf(`{"binding":"b%d","type":"http","scope":%q,"http_proxy":"direct"}`, i, stubScope))
		}
		return `{"v":1,"bindings":[` + strings.Join(bs, ",") + `]}`
	}
	for _, tc := range []struct {
		name string
		doc  string
		ok   bool
	}{
		{"a valid http binding", httpDesc("source_1", ""), true},
		{"a valid postgres binding", pg(""), true},
		{"an empty list of bindings (an access-only descriptor)", `{"v":1,"bindings":[]}`, true},
		{"sixteen bindings", many(16), true},
		{"seventeen bindings", many(17), false},
		{"not JSON", `{"v":1,`, false},
		{"empty", ``, false},
		{"just whitespace", " \n", false},
		{"two documents", httpDesc("a", "") + httpDesc("b", ""), false},
		{"trailing garbage", httpDesc("a", "") + " x", false},
		{"an array", `[]`, false},
		{"version 2", `{"v":2,"bindings":[]}`, false},
		{"version 0", `{"v":0,"bindings":[]}`, false},
		{"no version", `{"bindings":[]}`, false},
		{"the version as text", `{"v":"1","bindings":[]}`, false},
		{"the version as 1.0", `{"v":1.0,"bindings":[]}`, false},
		{"no bindings key", `{"v":1}`, false},
		{"bindings null", `{"v":1,"bindings":null}`, false},
		{"access null", `{"v":1,"access":null,"bindings":[]}`, false},
		{"a null field", pgWith("tls", "null"), false},
		{"a null port", pgWith("port", "null"), false},
		{"an unknown top-level key", `{"v":1,"bindings":[],"extra":1}`, false},
		{"an unknown binding key", strings.Replace(httpDesc("a", ""), `"type":"http"`, `"type":"http","extra":1`, 1), false},
		{"an unknown access key", `{"v":1,"access":{"extra":[]},"bindings":[]}`, false},
		{"a value key", strings.Replace(httpDesc("a", ""), `"type":"http"`, `"type":"http","value":"x"`, 1), false},
		{"a password key", strings.Replace(httpDesc("a", ""), `"type":"http"`, `"type":"http","password":"x"`, 1), false},
		{"a token key", strings.Replace(httpDesc("a", ""), `"type":"http"`, `"type":"http","token":"x"`, 1), false},
		{"a file key", strings.Replace(httpDesc("a", ""), `"type":"http"`, `"type":"http","file":"/x"`, 1), false},
		{"a path key", strings.Replace(httpDesc("a", ""), `"type":"http"`, `"type":"http","path":"/x"`, 1), false},
		{"a duplicate version", `{"v":1,"v":1,"bindings":[]}`, false},
		{"a duplicate bindings", `{"v":1,"bindings":[],"bindings":[]}`, false},
		{"a duplicate key in a binding", strings.Replace(httpDesc("a", ""), `"type":"http"`, `"type":"http","type":"http"`, 1), false},
		{"a duplicate scope that would be last-wins", strings.Replace(httpDesc("a", ""), `"scope":`, `"scope":"https://good.example/","scope":`, 1), false},
		{"a duplicate in access", `{"v":1,"access":{"directories":[],"directories":["/run/rl/params"]},"bindings":[]}`, false},
		{"a key in the wrong case", `{"V":1,"bindings":[]}`, false},
		{"a binding key in the wrong case", strings.Replace(httpDesc("a", ""), `"binding"`, `"Binding"`, 1), false},
		{"an escaped spelling of a key is the key", strings.Replace(httpDesc("a", ""), `"scope":`, `"sc\u006fpe":"https://x.example/","scope":`, 1), false},
		{"two bindings of one name", `{"v":1,"bindings":[` + strings.Repeat(`{"binding":"a","type":"http","scope":"`+stubScope+`","http_proxy":"direct"},`, 1) + `{"binding":"a","type":"http","scope":"` + stubScope + `","http_proxy":"direct"}]}`, false},
		{"a binding object where a list goes", `{"v":1,"bindings":{}}`, false},
		{"an access list where an object goes", `{"v":1,"access":[],"bindings":[]}`, false},
		{"an object among the directories", `{"v":1,"access":{"directories":[{}]},"bindings":[]}`, false},
		{"a number among the directories", `{"v":1,"access":{"directories":[1]},"bindings":[]}`, false},
		{"invalid UTF-8", "{\"v\":1,\"bindings\":[{\"binding\":\"a\xff\"}]}", false},
		{"a byte-order mark", "\xef\xbb\xbf" + `{"v":1,"bindings":[]}`, false},
	} {
		_, err := loadFrom(r, tc.doc, nil)
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}

func TestTheDescriptorIsRefusedWhenItIsNotAFileOrIsTooLarge(t *testing.T) {
	r := newBindRig(t)
	c := config{bindingsFile: filepath.Join(r.rl, "missing.json"), workDir: "/work", resultFile: "/work/result.parquet"}
	if _, err := loadBindings(c); err == nil {
		t.Error("a descriptor that does not exist was accepted")
	}
	c.bindingsFile = r.rl // a directory
	if _, err := loadBindings(c); err == nil {
		t.Error("a directory was accepted as a descriptor")
	}
	// exactly the cap is read, one more is refused; the padding is whitespace so the document stays valid.
	base := `{"v":1,"bindings":[]}`
	r.write(base + strings.Repeat(" ", maxBindingsFileBytes-len(base)))
	c.bindingsFile = r.file()
	if _, err := loadBindings(c); err != nil {
		t.Errorf("a descriptor of exactly %d bytes was refused: %v", maxBindingsFileBytes, err)
	}
	r.write(base + strings.Repeat(" ", maxBindingsFileBytes-len(base)+1))
	if _, err := loadBindings(c); err == nil {
		t.Errorf("a descriptor of %d bytes was accepted", maxBindingsFileBytes+1)
	}
	r.write(`{"v":1,"bindings":[],"access":{"directories":["` + strings.Repeat("a", 100<<10) + `"]}}`)
	if _, err := loadBindings(c); err == nil {
		t.Error("a 100 KiB descriptor was accepted")
	}
}

func TestABindingsNameIsHeldToItsGrammar(t *testing.T) {
	r := newBindRig(t)
	for name, ok := range map[string]bool{
		"source_1": true, "a": true, "a1": true, "a_b_c": true, strings.Repeat("a", 32): true,
		"": false, "Source_1": false, "1a": false, "a-b": false, "a__b": false, "a_": false, "_a": false, strings.Repeat("a", 33): false, "catalog_token": false,
		"a b": false, "é": false, "a\u0000b": false, "../x": false, "a/b": false, "a.b": false, "a\nb": false, "rl_bind_x": true, "a'b": false,
	} {
		_, err := loadFrom(r, httpDesc(name, ""), nil)
		if (err == nil) != ok {
			t.Errorf("binding name %q: err = %v, want ok=%v", name, err, ok)
		}
	}
}

func TestATypeIsOneOfTheClosedSet(t *testing.T) {
	r := newBindRig(t)
	for typ, ok := range map[string]bool{"postgres": true, "mysql": true, "http": true, "": false, "redis": false, "POSTGRES": false, "s3": false, "iceberg": false, "postgresql": false, "http ": false} {
		doc := strings.Replace(pg(""), `"postgres"`, jq(typ), 1)
		if typ == "http" {
			doc = httpDesc("source_1", "")
		}
		_, err := loadFrom(r, doc, nil)
		if (err == nil) != ok {
			t.Errorf("type %q: err = %v, want ok=%v", typ, err, ok)
		}
	}
}

// ---- the loader: the fields of a connection -------------------------------------------------------------------------------------------------------------------

func TestAHostIsHeldToTheRepositorysGrammarForItsType(t *testing.T) {
	r := newBindRig(t)
	long := func(n int) string { return strings.Repeat("a", n) }
	for _, tc := range []struct {
		host            string
		postgres, mysql bool
	}{
		{"db.example.com", true, true},
		{"10.0.0.1", true, true},
		{"localhost", true, true},
		{"a", true, true},
		{"2001:db8::1", true, false},
		{"::1", true, false},
		// "::" is a valid IPv6 address (the unspecified one) and sourcehost takes it as netip does; "1:2" is not an address. A character class would have taken both.
		{"::", true, false},
		{"1:2", false, false},
		{"fe80::1%eth0", false, false},
		{"[::1]", false, false},
		{"db.example.com:5432", false, false},
		{"db_host", false, true},
		{"db_host.example.com", false, true},
		{"-a", false, false},
		{".a", false, false},
		{"a b", false, false},
		{"a,b", false, false},
		{"a/b", false, false},
		{"a@b", false, false},
		{"", false, false},
		{long(253), true, true},
		{long(254), false, false},
		{"dbé.example", false, false},
		{"a\nb", false, false},
		{"db.example.com\n", false, false},
		{"a\u00a0b", false, false},
		{"a：b", false, false}, // a fullwidth colon is not a colon
		{"a\u0000b", false, false},
		{"2001:db8::1:", false, false},
		{"1.2.3.4.5", true, true}, // a name of five numeric labels: sourcehost judges nothing of a name beyond its characters
	} {
		for typ, want := range map[string]bool{"postgres": tc.postgres, "mysql": tc.mysql} {
			doc := strings.Replace(pgWith("host", jq(tc.host)), `"postgres"`, jq(typ), 1)
			_, err := loadFrom(r, doc, nil)
			if (err == nil) != want {
				t.Errorf("%s host %q: err = %v, want ok=%v", typ, tc.host, err, want)
			}
		}
	}
}

func TestAPortIsAWholeNumberFrom1To65535WrittenAsANumber(t *testing.T) {
	r := newBindRig(t)
	for port, ok := range map[string]bool{
		"5432": true, "1": true, "65535": true, "3306": true,
		"0": false, "65536": false, "-1": false, `"5432"`: false, "5432.0": false, "1e3": false, "true": false, "05432": false, "99999999999": false, `""`: false, "[]": false, "5432.5": false, "+5432": false,
	} {
		_, err := loadFrom(r, pgWith("port", port), nil)
		if (err == nil) != ok {
			t.Errorf("port %s: err = %v, want ok=%v", port, err, ok)
		}
	}
	if _, err := loadFrom(r, pgWith("port", ""), nil); err == nil {
		t.Error("a postgres binding with no port was accepted")
	}
}

func TestADatabaseNameIsHeldToTheDeclarableGrammar(t *testing.T) {
	r := newBindRig(t)
	for db, ok := range map[string]bool{
		"app": true, "café": true, "my db_1": false, "日本語": true, strings.Repeat("a", 255): true, "a.b": true, "a-b": true,
		"": false, ".": false, "..": false, "a/b": false, "a?b": false, "a b": false, "a;b": false, "a%41": false, "a+b": false, "a&b": false, "a#b": false,
		"a\u0001b": false, "a\u200bb": false, "a\u202eb": false, "a\u2028b": false, "a\u2029b": false, "a\u0000b": false, "a\nb": false, "a\u007fb": false, "a\u00adb": false, "\ufeffa": false,
		strings.Repeat("a", 256): false, "db x=y": false, "a'b": true, `a"b`: true, `a\b`: true,
	} {
		_, err := loadFrom(r, pgWith("database", jq(db)), nil)
		if (err == nil) != ok {
			t.Errorf("database %q: err = %v, want ok=%v", db, err, ok)
		}
	}
	if _, err := loadFrom(r, pgWith("database", ""), nil); err == nil {
		t.Error("a postgres binding with no database was accepted")
	}
}

func TestAUserIsHeldToTheUserGrammarAndIsNeverEmpty(t *testing.T) {
	r := newBindRig(t)
	for user, ok := range map[string]bool{
		"rl_reader": true, "a": true, "_a": true, "a.b$c-d": true, "A1": true, "a" + strings.Repeat("b", 62): true,
		"": false, "1a": false, "a b": false, "a'b": false, "a" + strings.Repeat("b", 63): false, "a\nb": false, "é": false, "a;b": false, "-a": false,
	} {
		_, err := loadFrom(r, pgWith("user", jq(user)), nil)
		if (err == nil) != ok {
			t.Errorf("user %q: err = %v, want ok=%v", user, err, ok)
		}
	}
	if _, err := loadFrom(r, pgWith("user", ""), nil); err == nil {
		t.Error("a postgres binding with no user was accepted")
	}
}

func TestTLSIsOneOfTheFourWordsOrAbsent(t *testing.T) {
	r := newBindRig(t)
	for tls, ok := range map[string]bool{`"require"`: true, `"prefer"`: true, `"verify-full"`: true, `"disable"`: true, `"REQUIRE"`: false, `"verify-ca"`: false, `""`: false, `"require "`: false, `true`: false} {
		_, err := loadFrom(r, pgWith("tls", tls), nil)
		if (err == nil) != ok {
			t.Errorf("tls %s: err = %v, want ok=%v", tls, err, ok)
		}
	}
	if _, err := loadFrom(r, pgWith("tls", ""), nil); err != nil {
		t.Errorf("a binding with no tls was refused: %v", err)
	}
}

func TestTheFieldsOfOneTypeAreForbiddenOnTheOther(t *testing.T) {
	r := newBindRig(t)
	for name, doc := range map[string]string{
		"scope on postgres":      pgWith("scope", jq("https://x.example/")),
		"http_proxy on postgres": pgWith("http_proxy", jq("direct")),
		"host on http":           strings.Replace(httpDesc("a", ""), `"type":"http"`, `"type":"http","host":"db.example.com"`, 1),
		"port on http":           strings.Replace(httpDesc("a", ""), `"type":"http"`, `"type":"http","port":5432`, 1),
		"database on http":       strings.Replace(httpDesc("a", ""), `"type":"http"`, `"type":"http","database":"app"`, 1),
		"user on http":           strings.Replace(httpDesc("a", ""), `"type":"http"`, `"type":"http","user":"u"`, 1),
		"tls on http":            strings.Replace(httpDesc("a", ""), `"type":"http"`, `"type":"http","tls":"require"`, 1),
		"an empty host on http":  strings.Replace(httpDesc("a", ""), `"type":"http"`, `"type":"http","host":""`, 1),
		"no scope on http":       strings.Replace(httpDesc("a", ""), fmt.Sprintf(`"scope":%q,`, stubScope), ``, 1),
		// A scoped http secret with no proxy value goes direct past the plane's unscoped proxy secret: an absent key is a refusal, never a silent direct.
		"no http_proxy on http":    strings.Replace(httpDesc("a", ""), `,"http_proxy":"direct"`, ``, 1),
		"an empty http_proxy":      strings.Replace(httpDesc("a", ""), `"http_proxy":"direct"`, `"http_proxy":""`, 1),
		"a null http_proxy":        strings.Replace(httpDesc("a", ""), `"http_proxy":"direct"`, `"http_proxy":null`, 1),
		"an upper-case DIRECT":     strings.Replace(httpDesc("a", ""), `"http_proxy":"direct"`, `"http_proxy":"DIRECT"`, 1),
		"a padded direct":          strings.Replace(httpDesc("a", ""), `"http_proxy":"direct"`, `"http_proxy":" direct"`, 1),
		"an address with no port":  strings.Replace(httpDesc("a", ""), `"http_proxy":"direct"`, `"http_proxy":"10.0.0.1"`, 1),
		"a name and a port":        strings.Replace(httpDesc("a", ""), `"http_proxy":"direct"`, `"http_proxy":"proxy.example:80"`, 1),
		"port 0":                   strings.Replace(httpDesc("a", ""), `"http_proxy":"direct"`, `"http_proxy":"10.0.0.1:0"`, 1),
		"a zero-padded port":       strings.Replace(httpDesc("a", ""), `"http_proxy":"direct"`, `"http_proxy":"10.0.0.1:04750"`, 1),
		"a scheme on the proxy":    strings.Replace(httpDesc("a", ""), `"http_proxy":"direct"`, `"http_proxy":"http://10.0.0.1:80"`, 1),
		"a quote in the proxy":     strings.Replace(httpDesc("a", ""), `"http_proxy":"direct"`, `"http_proxy":"10.0.0.1:80'"`, 1),
		"a non-loopback http":      strings.Replace(httpDesc("a", ""), stubScope, "http://example.com/", 1),
		"no trailing slash":        strings.Replace(httpDesc("a", ""), stubScope, "https://example.com", 1),
		"a path in the scope":      strings.Replace(httpDesc("a", ""), stubScope, "https://example.com/v1/", 1),
		"a space in the scope":     strings.Replace(httpDesc("a", ""), stubScope, "https://exa mple.com/", 1),
		"a quote in the scope":     strings.Replace(httpDesc("a", ""), stubScope, "https://exa'mple.com/", 1),
		"a user in the scope":      strings.Replace(httpDesc("a", ""), stubScope, "https://u@example.com/", 1),
		"a loopback with no port":  strings.Replace(httpDesc("a", ""), stubScope, "http://127.0.0.1/", 1),
		"a loopback port too high": strings.Replace(httpDesc("a", ""), stubScope, "http://127.0.0.1:99999/", 1),
		"another loopback address": strings.Replace(httpDesc("a", ""), stubScope, "http://127.0.0.2:80/", 1),
		"localhost":                strings.Replace(httpDesc("a", ""), stubScope, "http://localhost:80/", 1),
		"an https scope with a ..": strings.Replace(httpDesc("a", ""), stubScope, "https://a..b/../", 1),
	} {
		if _, err := loadFrom(r, doc, nil); err == nil {
			t.Errorf("%s: accepted\n%s", name, doc)
		}
	}
	for name, doc := range map[string]string{
		"an https scope":              strings.Replace(httpDesc("a", ""), stubScope, "https://api.example.com/", 1),
		"an https scope with a port":  strings.Replace(httpDesc("a", ""), stubScope, "https://api.example.com:8443/", 1),
		"the loopback stub":           httpDesc("a", ""),
		"an address and a port proxy": strings.Replace(httpDesc("a", ""), `"http_proxy":"direct"`, `"http_proxy":"10.0.0.1:4750"`, 1),
		"an IPv6 proxy":               strings.Replace(httpDesc("a", ""), `"http_proxy":"direct"`, `"http_proxy":"[2001:db8::1]:4750"`, 1),
	} {
		if _, err := loadFrom(r, doc, nil); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}

// ---- the loader: access ---------------------------------------------------------------------------------------------------------------------------------------

func accessDoc(dirs, prefixes []string) string {
	d, _ := json.Marshal(dirs)
	p, _ := json.Marshal(prefixes)
	if dirs == nil {
		d = []byte("[]")
	}
	if prefixes == nil {
		p = []byte("[]")
	}
	return fmt.Sprintf(`{"v":1,"access":{"directories":%s,"url_prefixes":%s},"bindings":[]}`, d, p)
}

func TestAnAccessDirectoryHasItsSlashAppendedByTheRunner(t *testing.T) {
	r := newBindRig(t)
	for in, want := range map[string]string{
		"/run/rl/params":  "/run/rl/params/",
		"/run/rl/params/": "/run/rl/params/",
		"/data/x":         "/data/x/",
		// not a prefix of /run/rl/secrets/ once it has its slash, and not refused for being a string prefix of it without one
		"/run/rl/sec":       "/run/rl/sec/",
		"/run/rl/secret":    "/run/rl/secret/",
		"/run/rl/secrets2":  "/run/rl/secrets2/",
		"/run/rl/par":       "/run/rl/par/",
		"/a/ü/b":            "/a/ü/b/",
		"/a/b..c":           "/a/b..c/",
		"/pro":              "/pro/",
		"/run/rl/bindings2": "/run/rl/bindings2/",
	} {
		d, err := loadFrom(r, accessDoc([]string{in}, nil), nil)
		if err != nil || len(d.dirs) != 1 || d.dirs[0] != want {
			t.Errorf("directory %q: got %v err %v, want [%s]", in, d, err, want)
		}
	}
}

func TestAnAccessDirectoryIsHeldToItsGrammar(t *testing.T) {
	r := newBindRig(t)
	for _, in := range []string{
		"", "relative/x", "x", "./x", "/run/rl/../x", "/run/../x", "/..", "/run//x", "//run", "/run/./x", "/run/rl/par\tams", "/run/rl/ params", "/run/rl/params\n", "/run/rl/par\u0000ams",
		"/run/rl/pa\u00a0rams", "/run/rl/pa\u2028rams", "/a/b/..", "/a/b/../", "/a/b/.", "/run/rl/\u007f", "/" + strings.Repeat("a", 1024),
	} {
		if _, err := loadFrom(r, accessDoc([]string{in}, nil), nil); err == nil {
			t.Errorf("directory %q accepted", in)
		}
	}
	// invalid UTF-8 cannot reach here through JSON (the whole file is checked); the function itself refuses it too.
	for _, in := range []string{"/a/\xff", "/a/\xc0\xaf"} {
		if _, err := cleanAccessDir(in); err == nil {
			t.Errorf("cleanAccessDir(%q) accepted", in)
		}
	}
	var many []string
	for i := 0; i < maxAccessEntries; i++ {
		many = append(many, fmt.Sprintf("/data/d%d", i))
	}
	if _, err := loadFrom(r, accessDoc(many, nil), nil); err != nil {
		t.Errorf("sixteen directories refused: %v", err)
	}
	if _, err := loadFrom(r, accessDoc(append(many, "/data/more"), nil), nil); err == nil {
		t.Error("seventeen directories accepted")
	}
}

func TestAnAccessDirectoryThatOverlapsWhatTheRunnerProtectsIsRefused(t *testing.T) {
	r := newBindRig(t)
	tokDir := t.TempDir()
	tok := filepath.Join(tokDir, "catalog_token")
	if err := os.WriteFile(tok, []byte("canary.CATALOG-TOKEN_0123456789"), 0o440); err != nil {
		t.Fatal(err)
	}
	withToken := func(c *config) { c.catalogTokenFile = tok }
	secrets := r.secretsDir()
	for _, tc := range []struct {
		name string
		dir  string
	}{
		{"the secrets directory", secrets},
		{"the secrets directory with a slash", secrets + "/"},
		{"a file in the secrets directory", secrets + "/source_1"},
		{"a directory below the secrets directory", secrets + "/sub/dir"},
		{"the directory holding the descriptor", r.rl},
		{"the directory holding the descriptor, with a slash", r.rl + "/"},
		{"the descriptor itself", r.file()},
		{"below the descriptor", r.file() + "/x"},
		{"the root", "/"},
		{"a parent of the descriptor's directory", filepath.Dir(r.rl)},
		{"the token's directory", tokDir},
		{"the token file", tok},
		{"below the token file", tok + "/x"},
		{"/proc", "/proc"},
		{"below /proc", "/proc/1/environ"},
		{"/sys", "/sys/"},
		{"/dev", "/dev"},
		{"/dev/shm", "/dev/shm"},
		{"/etc", "/etc"},
		{"below /etc", "/etc/ssl/certs"},
	} {
		if _, err := loadFrom(r, accessDoc([]string{tc.dir}, nil), withToken); err == nil {
			t.Errorf("%s (%s): accepted", tc.name, tc.dir)
		}
	}
	// neighbours that only share letters are not the thing and are taken (strings, with a slash)
	for _, dir := range []string{secrets + "2", secrets[:len(secrets)-2], r.rl + "-other", tokDir + "-other", "/procs", "/etcetera", "/pro"} {
		if _, err := loadFrom(r, accessDoc([]string{dir}, nil), withToken); err != nil {
			t.Errorf("a neighbour %s was refused: %v", dir, err)
		}
	}
}

func TestAnAccessDirectoryThatIsASymlinkToWhatTheRunnerProtectsIsRefused(t *testing.T) {
	r := newBindRig(t)
	links := t.TempDir()
	for name, target := range map[string]string{"to_secrets": r.secretsDir(), "to_rl": r.rl, "to_etc": "/etc"} {
		if err := os.Symlink(target, filepath.Join(links, name)); err != nil {
			t.Fatal(err)
		}
		if _, err := loadFrom(r, accessDoc([]string{filepath.Join(links, name)}, nil), nil); err == nil {
			t.Errorf("a symlink to %s was accepted", target)
		}
	}
	// and the protected path spelled the other way round: the real path of a directory whose usual spelling goes through a symlink (/var is /private/var on macOS)
	real, err := filepath.EvalSymlinks(r.secretsDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := loadFrom(r, accessDoc([]string{real}, nil), nil); err == nil {
		t.Errorf("the resolved spelling of the secrets directory %s was accepted", real)
	}
	// a symlink to somewhere harmless is the harmless place
	other := t.TempDir()
	if err := os.Symlink(other, filepath.Join(links, "to_other")); err != nil {
		t.Fatal(err)
	}
	if _, err := loadFrom(r, accessDoc([]string{filepath.Join(links, "to_other")}, nil), nil); err != nil {
		t.Errorf("a symlink to an ordinary directory was refused: %v", err)
	}
}

func TestAnAccessURLPrefixIsAnObjectStorePrefixAndNothingElse(t *testing.T) {
	r := newBindRig(t)
	for _, p := range []string{
		"s3://bkt/runninglake/prj/env/", "gs://bkt/a/", "abfss://container@account.dfs.core.windows.net/p/e/", "s3://bkt/", "s3://my-bucket.v2/a_b/c=d/", "s3://bkt/a.b/c-d/",
	} {
		if _, err := loadFrom(r, accessDoc(nil, []string{p}), nil); err != nil {
			t.Errorf("prefix %q refused: %v", p, err)
		}
	}
	for _, p := range []string{
		"http://example.com/", "https://example.com/", "https://catalog.internal/catalog/", "http://127.0.0.1:8080/", "file:///etc/", "s3://bkt", "s3://bkt/a", "s3://", "s3:///a/",
		"s3://bkt/a/../b/", "s3://bkt/a//b/", "s3://bkt/./a/", "s3://bkt/a b/", "s3://bkt/a?x=1/", "s3://bkt/a#x/", "s3://bkt/a%2e%2e/", "S3://bkt/", "s3://bkt/\u00e9/", "s3://bkt/a\n/",
		"s3://bkt/a'/", "s3://bkt/a\\b/", "az://c/", "hdfs://nn/", "", "/run/rl/", "s3://u:p@bkt/", "s3://bkt:80/", "s3://bkt/" + strings.Repeat("a", 1100) + "/",
	} {
		if _, err := loadFrom(r, accessDoc(nil, []string{p}), nil); err == nil {
			t.Errorf("prefix %q accepted", p)
		}
	}
	var many []string
	for i := 0; i < maxAccessEntries; i++ {
		many = append(many, fmt.Sprintf("s3://bkt/p%d/", i))
	}
	if _, err := loadFrom(r, accessDoc(nil, many), nil); err != nil {
		t.Errorf("sixteen prefixes refused: %v", err)
	}
	if _, err := loadFrom(r, accessDoc(nil, append(many, "s3://bkt/more/")), nil); err == nil {
		t.Error("seventeen prefixes accepted")
	}
}

// The loopback stub's origin is the proof vehicle's and only with the runner's own word; nothing else with a scheme of http or https is ever an access entry.
func TestOnlyTheProofVehicleMayBeGivenTheLoopbackOrigin(t *testing.T) {
	r := newBindRig(t)
	proof := func(c *config) { c.bindingsProof = true }
	if _, err := loadFrom(r, accessDoc(nil, []string{stubScope}), nil); err == nil {
		t.Error("the loopback origin was accepted without RL_BINDINGS_PROOF")
	}
	if _, err := loadFrom(r, accessDoc(nil, []string{stubScope}), proof); err != nil {
		t.Errorf("the loopback origin was refused with RL_BINDINGS_PROOF: %v", err)
	}
	for _, p := range []string{"http://127.0.0.1:99999/", "http://127.0.0.1/", "http://localhost:80/", "https://127.0.0.1:80/", "http://127.0.0.1:80/x/", "http://127.0.0.2:80/", "http://example.com/", "https://catalog.internal/", "http://127.0.0.1:0/"} {
		if _, err := loadFrom(r, accessDoc(nil, []string{p}), proof); err == nil {
			t.Errorf("%s accepted even with RL_BINDINGS_PROOF", p)
		}
	}
}

// ---- the value ------------------------------------------------------------------------------------------------------------------------------------------------

func TestAValueIsReadExactlyAndRefusedByName(t *testing.T) {
	r := newBindRig(t)
	for _, tc := range []struct {
		name  string
		file  []byte
		rule  string // "" is accepted, and the value is the file
		spawn bool
	}{
		{"a password", []byte("pw"), "", false},
		{"a trailing newline is part of the password", []byte("pw\n"), "", false},
		{"a trailing CR LF is part of the password", []byte("pw\r\n"), "", false},
		{"a leading newline is part of the password", []byte("\npw"), "", false},
		{"a leading and trailing space", []byte(" pw "), "", false},
		{"a single space", []byte(" "), "", false},
		{"a tab", []byte("\t"), "", false},
		{"both quotes, a backslash and a newline", []byte("pa'ss\"w\\o\nrd"), "", false},
		{"non-ASCII", []byte("pässwörd-日本語-🔑"), "", false},
		{"exactly 65536 bytes", bytes.Repeat([]byte("a"), 65536), "", false},
		{"65537 bytes", bytes.Repeat([]byte("a"), 65537), ruleValueTooLarge, false},
		{"empty", []byte{}, ruleValueEmpty, false},
		{"a newline only", []byte("\n"), ruleValueEmpty, false},
		{"CR LF only", []byte("\r\n"), ruleValueEmpty, false},
		{"several line terminators only", []byte("\n\r\n\n\r"), ruleValueEmpty, false},
		{"not UTF-8", []byte("pw\xff"), ruleValueNotUTF8, false},
		{"a truncated multi-byte sequence", []byte("pw\xe6\x97"), ruleValueNotUTF8, false},
		{"a NUL inside", []byte("a\x00b"), ruleValueHasNUL, false},
		{"a NUL only", []byte("\x00"), ruleValueHasNUL, false},
		{"a trailing NUL", []byte("pw\x00"), ruleValueHasNUL, false},
	} {
		r.secret("source_1", tc.file)
		got, err := readBindingValue(r.secretsDir(), "source_1")
		if tc.rule == "" {
			if err != nil || !bytes.Equal(got, tc.file) {
				t.Errorf("%s: got %q err %v, want the file's bytes", tc.name, got, err)
			}
			continue
		}
		ref, ok := err.(valueRefusal)
		if !ok || ref.rule != tc.rule || ref.binding != "source_1" {
			t.Errorf("%s: err = %v, want %s", tc.name, err, tc.rule)
		}
		if err != nil && strings.Contains(err.Error(), string(bytes.TrimSpace(tc.file))) && len(bytes.TrimSpace(tc.file)) > 1 {
			t.Errorf("%s: the refusal quotes the value: %v", tc.name, err)
		}
	}
	if _, err := readBindingValue(r.secretsDir(), "absent"); err == nil || err.(valueRefusal).rule != ruleValueMissing {
		t.Errorf("a missing file: %v", err)
	}
	if err := os.Mkdir(filepath.Join(r.secretsDir(), "adir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := readBindingValue(r.secretsDir(), "adir"); err == nil || err.(valueRefusal).rule != ruleValueUnreadable {
		t.Errorf("a directory: %v", err)
	}
	// a projected volume lays a key out as a symlink to ..data/<key>; the read follows it as the token's does
	if err := os.MkdirAll(filepath.Join(r.secretsDir(), "..2026_data"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.secretsDir(), "..2026_data", "linked"), []byte("via-symlink"), 0o440); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..2026_data", filepath.Join(r.secretsDir(), "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..data/linked", filepath.Join(r.secretsDir(), "linked")); err != nil {
		t.Fatal(err)
	}
	if got, err := readBindingValue(r.secretsDir(), "linked"); err != nil || string(got) != "via-symlink" {
		t.Errorf("a projected symlink: %q %v", got, err)
	}
	if os.Getuid() != 0 {
		r.secret("locked", []byte("pw"))
		if err := os.Chmod(filepath.Join(r.secretsDir(), "locked"), 0); err != nil {
			t.Fatal(err)
		}
		if _, err := readBindingValue(r.secretsDir(), "locked"); err == nil || err.(valueRefusal).rule != ruleValueUnreadable {
			t.Errorf("an unreadable file: %v", err)
		}
	}
}

func TestTheValuesOfAllTheBindingsAreCappedInSum(t *testing.T) {
	r := newBindRig(t)
	var bs []string
	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("b%d", i)
		bs = append(bs, fmt.Sprintf(`{"binding":%q,"type":"http","scope":%q,"http_proxy":"direct"}`, name, stubScope))
		r.secret(name, bytes.Repeat([]byte("a"), 60<<10))
	}
	r.write(`{"v":1,"bindings":[` + strings.Join(bs, ",") + `]}`)
	cfg := config{bindingsFile: r.file(), bindingsProof: true, workDir: "/work", resultFile: "/work/result.parquet"}
	if _, class, err := prepareBindings(cfg); class != classBindingRefused || err == nil || !strings.Contains(err.Error(), ruleValueTooLarge) {
		t.Errorf("five values of 60 KiB (300 KiB): class %q err %v, want binding_refused value_too_large", class, err)
	}
	// exactly the cap: four values of 64 KiB
	r.write(`{"v":1,"bindings":[` + strings.Join(bs[:4], ",") + `]}`)
	for i := 0; i < 4; i++ {
		r.secret(fmt.Sprintf("b%d", i), bytes.Repeat([]byte("a"), 64<<10))
	}
	if p, class, err := prepareBindings(cfg); err != nil || class != "" {
		t.Errorf("four values of 64 KiB (exactly 256 KiB): class %q err %v", class, err)
	} else {
		p.wipe()
	}
}

// ---- what the script says --------------------------------------------------------------------------------------------------------------------------------------

func TestTheRenderedSecretIsTheValuesHexAndNothingOfItsBytes(t *testing.T) {
	for name, value := range map[string][]byte{
		"a quote":                 []byte("pa'ss"),
		"both quotes":             []byte(`pa'ss"w`),
		"a backslash":             []byte(`a\b\\c\n\x41A`),
		"a newline":               []byte("line1\nline2\nline3"),
		"a trailing newline":      []byte("pw\n"),
		"a semicolon and a dot":   []byte("x'); DROP SECRET y; --\n.shell id\n"),
		"non-ASCII":               []byte("pässwörd-日本語-🔑"),
		"64 KiB":                  bytes.Repeat([]byte("a'\n\\é"), 13107)[:65536],
		"4 KiB of mixed":          bytes.Repeat([]byte("p'\"\\\n\té日"), 400)[:4096],
		"bytes that look like it": []byte("e'' || chr(0) || ''"),
	} {
		if !validUTF8(value) {
			// slicing a repeated multi-byte pattern can cut a character; cut back to a boundary
			for len(value) > 0 && !validUTF8(value) {
				value = value[:len(value)-1]
			}
		}
		d := bindingDecl{Binding: "source_1", Type: "http", Scope: strp(stubScope), HTTPProxy: strp("direct")}
		var b bytes.Buffer
		renderBinding(&b, d, value)
		got := b.String()
		want := "CREATE SECRET rl_bind_source_1 (TYPE http, BEARER_TOKEN decode(from_hex('" + hex.EncodeToString(value) + "')), SCOPE '" + stubScope + "', HTTP_PROXY '');\n"
		if got != want {
			t.Errorf("%s: the statement is not the hex form (got %d bytes, want %d)", name, len(got), len(want))
			continue
		}
		// between the quotes of the literal only [0-9a-f] appears, so the value can neither close a string nor start a statement
		lit := got[strings.Index(got, "from_hex('")+len("from_hex('"):]
		lit = lit[:strings.Index(lit, "'")]
		for _, c := range lit {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				t.Errorf("%s: the literal holds %q", name, c)
				break
			}
		}
		if len(value) > 8 && strings.Contains(got, string(value)) {
			t.Errorf("%s: the value's own bytes are in the statement", name)
		}
	}
}

func validUTF8(b []byte) bool { return strings.ToValidUTF8(string(b), "\x00") == string(b) }
func strp(s string) *string   { return &s }

func TestTheProxyIsEmptyForDirectAndTheAddressOtherwise(t *testing.T) {
	var b bytes.Buffer
	renderBinding(&b, bindingDecl{Binding: "a", Type: "http", Scope: strp("https://api.example.com/"), HTTPProxy: strp("10.0.0.1:4750")}, []byte("pw"))
	if !strings.Contains(b.String(), "SCOPE 'https://api.example.com/', HTTP_PROXY '10.0.0.1:4750');") {
		t.Errorf("proxy form: %s", b.String())
	}
	b.Reset()
	renderBinding(&b, bindingDecl{Binding: "a", Type: "http", Scope: strp("https://api.example.com/"), HTTPProxy: strp("direct")}, []byte("pw"))
	if !strings.Contains(b.String(), "HTTP_PROXY '');") || strings.Contains(b.String(), "direct") {
		t.Errorf("direct form: %s", b.String())
	}
}

// A descriptor with one http binding, one catalog token, a prelude and a task or a query: the whole script, in order.
func TestTheScriptWithADescriptorRestrictsBeforeTheLockAndMakesTheSecretsAfterIt(t *testing.T) {
	r := newBindRig(t)
	secretFile := []byte("pa'ss\nword-é")
	r.secret("source_1", secretFile)
	data := t.TempDir()
	r.write(httpDesc("source_1", fmt.Sprintf(`{"directories":[%s],"url_prefixes":["s3://bkt/runninglake/p/e/"]}`, jq(data))))
	tok := filepath.Join(t.TempDir(), "catalog_token")
	const tokText = "e" + "yJhbGciOiJSUzI1NiJ9.c2ln-_bmF0dXJl"
	if err := os.WriteFile(tok, []byte(tokText), 0o440); err != nil {
		t.Fatal(err)
	}
	prelude := filepath.Join(t.TempDir(), "prelude.sql")
	if err := os.WriteFile(prelude, []byte("ATTACH 'wh' AS rl_catalog (TYPE iceberg);\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, "SELECT 1 AS a", r.env(map[string]string{"RL_BINDINGS_PROOF": "1", "RL_CATALOG_TOKEN_FILE": tok, "RL_PRELUDE_FILE": prelude}))
	m, code, lines := h.runTask()
	if code != 0 || m.Status != "succeeded" {
		t.Fatalf("metrics = %+v code %d\n%s", m, code, h.errorLog())
	}
	s := h.script()
	at := func(sub string) int {
		i := strings.Index(s, sub)
		if i < 0 {
			t.Errorf("the script has no %q:\n%s", sub, s)
		}
		return i
	}
	allowed := fmt.Sprintf("SET allowed_directories=['%s', '%s/', 's3://bkt/runninglake/p/e/'];\n", h.work, data)
	ord := []int{
		at("SET temp_directory="), at("SET memory_limit="),
		at("SET allow_persistent_secrets=false;\n"), at(allowed), at("SET enable_external_access=false;\n"),
		at("SET lock_configuration=true;\n"), at("SELECT '" + initSentinel + "';"),
		at("CREATE SECRET " + catalogSecret), at("SELECT '" + tokenSentinel + "';"),
		at("CREATE SECRET rl_bind_source_1"), at("SELECT '" + secretsSentinel + "';"),
		at("ATTACH 'wh'"), at("SELECT '" + preludeSentinel + "';"), at("CREATE TEMP TABLE " + resultTable),
	}
	for i := 1; i < len(ord); i++ {
		if ord[i-1] >= ord[i] {
			t.Errorf("statement %d is not after statement %d in the script:\n%s", i, i-1, s)
		}
	}
	if strings.Count(s, "SET allowed_directories") != 1 || strings.Count(s, "enable_external_access") != 1 || strings.Count(s, "allow_persistent_secrets") != 1 {
		t.Errorf("the restriction is not written exactly once:\n%s", s)
	}
	// nothing of the value but its hex is in the script, and the hex is
	if strings.Contains(s, "pa'ss") || strings.Contains(s, "word-é") {
		t.Errorf("the value's bytes are in the script")
	}
	if !strings.Contains(s, "from_hex('"+hex.EncodeToString(secretFile)+"')") {
		t.Errorf("the value's hex is not in the script")
	}
	// the ack is the line directly before the metrics line
	if len(lines) != 2 || lines[0] != `RL_BINDINGS {"v":1,"made":["source_1"],"restricted":true}` || !strings.HasPrefix(lines[1], "RL_METRICS ") {
		t.Errorf("stdout = %q", lines)
	}
}

func TestAnAbsentAccessStillRestrictsToTheWorkAndResultDirectories(t *testing.T) {
	r := newBindRig(t)
	r.write(`{"v":1,"bindings":[]}`)
	resultDir := t.TempDir()
	h := newHarness(t, "SELECT 1", r.env(map[string]string{"RL_RESULT_FILE": filepath.Join(resultDir, "result.parquet")}))
	m, code, lines := h.runTask()
	if code != 0 || m.Status != "succeeded" {
		t.Fatalf("metrics = %+v code %d\n%s", m, code, h.errorLog())
	}
	s := h.script()
	for _, want := range []string{
		"SET allow_persistent_secrets=false;\n", fmt.Sprintf("SET allowed_directories=['%s', '%s'];\n", h.work, resultDir), "SET enable_external_access=false;\n",
		"SET lock_configuration=true;\n", "SELECT '" + secretsSentinel + "';",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("the script lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "CREATE SECRET") {
		t.Errorf("a descriptor with no bindings made a secret:\n%s", s)
	}
	if lines[0] != `RL_BINDINGS {"v":1,"made":[],"restricted":true}` {
		t.Errorf("ack = %q", lines[0])
	}
	// the work directory alone when the result is inside it, written once
	h2 := newHarness(t, "SELECT 1", r.env(nil))
	h2.runTask()
	if want := fmt.Sprintf("SET allowed_directories=['%s'];\n", h2.work); !strings.Contains(h2.script(), want) {
		t.Errorf("the work directory is not alone in the list:\n%s", h2.script())
	}
}

func TestTheBindingsSecretsAreMadeInNameOrder(t *testing.T) {
	r := newBindRig(t)
	var bs []string
	for _, n := range []string{"zeta", "alpha", "mid_1"} {
		bs = append(bs, fmt.Sprintf(`{"binding":%q,"type":"http","scope":%q,"http_proxy":"direct"}`, n, stubScope))
		r.secret(n, []byte("value-of-"+n))
	}
	r.write(`{"v":1,"bindings":[` + strings.Join(bs, ",") + `]}`)
	h := newHarness(t, "SELECT 1", r.env(proofEnv()))
	_, code, lines := h.runTask()
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, h.errorLog())
	}
	s := h.script()
	a, m, z := strings.Index(s, "rl_bind_alpha"), strings.Index(s, "rl_bind_mid_1"), strings.Index(s, "rl_bind_zeta")
	if !(a >= 0 && a < m && m < z) {
		t.Errorf("secrets are not in name order (%d %d %d):\n%s", a, m, z, s)
	}
	if lines[0] != `RL_BINDINGS {"v":1,"made":["alpha","mid_1","zeta"],"restricted":true}` {
		t.Errorf("ack = %q", lines[0])
	}
}

// With a descriptor the token's secret has its own sentinel; without one the script holds neither sentinel, nor any of the restriction.
func TestWithNoDescriptorThereIsNoRestrictionNoSentinelAndNoAckLine(t *testing.T) {
	tok := filepath.Join(t.TempDir(), "catalog_token")
	if err := os.WriteFile(tok, []byte("e"+"yJhbGciOiJSUzI1NiJ9.c2ln-_bmF0dXJl"), 0o440); err != nil {
		t.Fatal(err)
	}
	for name, env := range map[string]map[string]string{
		"a plain query": nil,
		"a token":       {"RL_CATALOG_TOKEN_FILE": tok},
		"measuring":     {"RL_MEASURE_BYTES": "1"},
	} {
		h := newHarness(t, "SELECT 1", env)
		_, code, line := h.exec()
		if code != 0 {
			t.Fatalf("%s: exit %d", name, code)
		}
		s := h.script()
		for _, bad := range []string{"allowed_directories", "enable_external_access", "allow_persistent_secrets", tokenSentinel, secretsSentinel, "rl_bind_", "from_hex"} {
			if strings.Contains(s, bad) {
				t.Errorf("%s: a run with no descriptor wrote %q:\n%s", name, bad, s)
			}
		}
		if strings.Contains(line, "RL_BINDINGS") {
			t.Errorf("%s: a run with no descriptor printed the ack: %q", name, line)
		}
	}
	th := taskHarness(t, "task_ok", nil)
	_, _, lines := th.runTask()
	for _, l := range lines {
		if strings.HasPrefix(l, "RL_BINDINGS") {
			t.Errorf("a task with no descriptor printed the ack: %q", l)
		}
	}
	if s := th.script(); strings.Contains(s, "allowed_directories") || strings.Contains(s, secretsSentinel) {
		t.Errorf("a task with no descriptor was restricted:\n%s", s)
	}
}

// ---- the refusals ---------------------------------------------------------------------------------------------------------------------------------------------

func TestADescriptorTheRunnerCannotUseIsRefusedBeforeTheEngineRunsAndNeverQuoted(t *testing.T) {
	r := newBindRig(t)
	canary := "canary-host.example.com"
	for name, doc := range map[string]string{
		"not JSON":       `{` + canary,
		"a bad version":  `{"v":9,"bindings":[],"x":"` + canary + `"}`,
		"a bad host":     strings.Replace(pgWith("host", jq(canary+"/x")), "x", "x", 1),
		"a bad database": pgWith("database", jq(canary+"?x=1")),
		"a bad user":     pgWith("user", jq("1"+canary)),
		"a bad name":     httpDesc("Bad-"+canary, ""),
		"a bad scope":    strings.Replace(httpDesc("a", ""), stubScope, "http://"+canary+"/", 1),
		"a bad prefix":   accessDoc(nil, []string{"https://" + canary + "/"}),
		"a bad dir":      accessDoc([]string{"relative/" + canary}, nil),
	} {
		r.write(doc)
		h := newHarness(t, "SELECT 1", r.env(proofEnv()))
		m, code, line := h.exec()
		if code != 1 || m.ErrorClass != classInvalidQuery {
			t.Errorf("%s: metrics = %+v code %d, want invalid_query_file", name, m, code)
		}
		if _, err := os.Stat(filepath.Join(h.work, "fake_script.sql")); err == nil {
			t.Errorf("%s: the engine ran with a descriptor it should have refused", name)
		}
		if strings.Contains(h.errorLog(), canary) || strings.Contains(line, canary) {
			t.Errorf("%s: a field of the descriptor is quoted: %q", name, h.errorLog())
		}
		if strings.Contains(line, "RL_BINDINGS") {
			t.Errorf("%s: a refused descriptor printed an ack: %s", name, line)
		}
	}
}

func TestATypeTheRunnerCannotMakeIsRefusedByNameBeforeTheEngineRuns(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		env  map[string]string
	}{
		{"postgres", pg(""), nil},
		{"postgres, with the proof word", pg(""), proofEnv()},
		{"mysql", strings.Replace(pg(""), `"postgres"`, `"mysql"`, 1), nil},
		{"mysql, with the proof word", strings.Replace(pg(""), `"postgres"`, `"mysql"`, 1), proofEnv()},
		{"http, without the proof word", httpDesc("source_1", ""), nil},
	} {
		r := newBindRig(t)
		r.secret("source_1", []byte("a-password-value"))
		r.write(tc.doc)
		h := newHarness(t, "SELECT 1", r.env(tc.env))
		m, code, line := h.exec()
		if code != 1 || m.ErrorClass != classBindingRefused {
			t.Errorf("%s: metrics = %+v code %d, want binding_refused", tc.name, m, code)
		}
		if _, err := os.Stat(filepath.Join(h.work, "fake_script.sql")); err == nil {
			t.Errorf("%s: the engine ran", tc.name)
		}
		log := h.errorLog()
		if !strings.Contains(log, ruleTypeUnavailable) || !strings.Contains(log, "source_1") {
			t.Errorf("%s: the note does not name the rule and the binding: %q", tc.name, log)
		}
		for _, leak := range []string{"a-password-value", "db.example.com", "rl_reader", "127.0.0.1"} {
			if strings.Contains(log, leak) || strings.Contains(line, leak) {
				t.Errorf("%s: %q is in the note or the line", tc.name, leak)
			}
		}
	}
}

func TestAValueTheRunnerWillNotUseIsBindingRefusedBeforeTheEngineRuns(t *testing.T) {
	for _, tc := range []struct {
		name string
		file []byte // nil is no file
		rule string
	}{
		{"missing", nil, ruleValueMissing},
		{"empty", []byte{}, ruleValueEmpty},
		{"only a newline", []byte("\n"), ruleValueEmpty},
		{"only CR LF", []byte("\r\n\r\n"), ruleValueEmpty},
		{"too large", bytes.Repeat([]byte("s3cr3t-"), 10000), ruleValueTooLarge},
		{"not UTF-8", []byte("s3cr3t-value-\xff"), ruleValueNotUTF8},
		{"a NUL", []byte("s3cr3t-val\x00ue"), ruleValueHasNUL},
	} {
		r := newBindRig(t)
		if tc.file != nil {
			r.secret("source_1", tc.file)
		}
		r.write(httpDesc("source_1", ""))
		h := newHarness(t, "SELECT 1", r.env(proofEnv()))
		m, code, line := h.exec()
		if code != 1 || m.ErrorClass != classBindingRefused {
			t.Errorf("%s: metrics = %+v code %d, want binding_refused", tc.name, m, code)
		}
		if _, err := os.Stat(filepath.Join(h.work, "fake_script.sql")); err == nil {
			t.Errorf("%s: the engine ran", tc.name)
		}
		log := h.errorLog()
		if !strings.Contains(log, tc.rule) || !strings.Contains(log, "source_1") {
			t.Errorf("%s: the note does not name %s and the binding: %q", tc.name, tc.rule, log)
		}
		if strings.Contains(log, "s3cr3t") || strings.Contains(line, "s3cr3t") || strings.Contains(log, "127.0.0.1") {
			t.Errorf("%s: the note quotes the value or the scope: %q", tc.name, log)
		}
	}
}

func TestADescriptorIsRefusedInTheModesWhoseBehaviourUnderTheRestrictionIsUnmeasured(t *testing.T) {
	r := newBindRig(t)
	r.secret("source_1", []byte("pw-value-1"))
	r.write(httpDesc("source_1", ""))
	probe := filepath.Join(t.TempDir(), "probe.json")
	if err := os.WriteFile(probe, []byte(`{"name_key_hex":"00112233445566778899aabbccddeeff","inputs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		sql  string
		env  map[string]string
		want bool // accepted
	}{
		{"a probe", "SELECT 1", map[string]string{"RL_PROBE_FILE": probe}, false},
		{"a bare statement", "CREATE TABLE t(a INT)", nil, false},
		{"byte measuring", "SELECT 1", map[string]string{"RL_MEASURE_BYTES": "1"}, false},
		{"a plain query", "SELECT 1", nil, true},
		{"a DESCRIBE", "DESCRIBE SELECT 1", nil, true},
		{"a WITH query", "WITH x AS (SELECT 1) SELECT * FROM x", nil, true},
	} {
		env := r.env(proofEnv())
		for k, v := range tc.env {
			env[k] = v
		}
		h := newHarness(t, tc.sql, env)
		m, code, _ := h.runTask()
		_, ran := os.Stat(filepath.Join(h.work, "fake_script.sql"))
		if tc.want {
			if code != 0 || m.Status != "succeeded" || ran != nil {
				t.Errorf("%s: metrics = %+v code %d ran=%v, want accepted", tc.name, m, code, ran == nil)
			}
			continue
		}
		if code != 1 || m.ErrorClass != classInvalidQuery || ran == nil {
			t.Errorf("%s: metrics = %+v code %d, want invalid_query_file before the engine", tc.name, m, code)
		}
	}
}

func TestATaskAcceptsADescriptorAndPrintsTheAckBeforeItsClaim(t *testing.T) {
	r := newBindRig(t)
	r.secret("source_1", []byte("pw-value-1"))
	r.write(httpDesc("source_1", ""))
	h := taskHarness(t, "task_ok", r.env(proofEnv()))
	m, code, lines := h.runTask()
	if code != 0 || m.Status != "succeeded" {
		t.Fatalf("metrics = %+v code %d\n%s", m, code, h.errorLog())
	}
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "RL_BINDINGS ") || !strings.HasPrefix(lines[1], "RL_TASK ") || !strings.HasPrefix(lines[2], "RL_METRICS ") {
		t.Fatalf("stdout = %q, want RL_BINDINGS, RL_TASK, RL_METRICS", lines)
	}
	if !strings.Contains(h.script(), "SET TimeZone='UTC';") || !strings.Contains(h.script(), "SET allowed_directories=") {
		t.Errorf("a restricted task lost its own settings or its restriction:\n%s", h.script())
	}
}

func TestTheRestrictionIsNotWidenedByTheRunnersOwnDirectories(t *testing.T) {
	r := newBindRig(t)
	r.write(`{"v":1,"bindings":[]}`)
	for name, resultFile := range map[string]string{"the root": "/result.parquet", "the descriptor's directory": filepath.Join(r.rl, "result.parquet"), "the secrets directory": filepath.Join(r.secretsDir(), "r.parquet")} {
		h := newHarness(t, "SELECT 1", r.env(map[string]string{"RL_RESULT_FILE": resultFile}))
		m, code, _ := h.exec()
		if code != 1 || m.ErrorClass != classInvalidQuery {
			t.Errorf("a result file in %s: metrics = %+v code %d, want invalid_query_file", name, m, code)
		}
		if _, err := os.Stat(filepath.Join(h.work, "fake_script.sql")); err == nil {
			t.Errorf("a result file in %s: the engine ran", name)
		}
	}
	// and a work directory that holds the descriptor
	h := newHarness(t, "SELECT 1", nil)
	if err := os.MkdirAll(filepath.Join(h.work, "rl", "secrets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.work, "rl", "bindings.json"), []byte(`{"v":1,"bindings":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	h.cfg.bindingsFile = filepath.Join(h.work, "rl", "bindings.json")
	if m, code, _ := h.exec(); code != 1 || m.ErrorClass != classInvalidQuery {
		t.Errorf("a work directory holding the descriptor: metrics = %+v code %d", m, code)
	}
}

func TestTheProofWordIsOneOrEmptyAndNothingElse(t *testing.T) {
	for v, ok := range map[string]bool{"": true, "1": true, " 1 ": true, "0": false, "true": false, "yes": false, "2": false} {
		c, err := configFromEnv(func(k string) string {
			if k == "RL_BINDINGS_PROOF" {
				return v
			}
			return ""
		})
		if (err == nil) != ok {
			t.Errorf("RL_BINDINGS_PROOF=%q: err = %v, want ok=%v", v, err, ok)
		}
		if err == nil && c.bindingsProof != (strings.TrimSpace(v) == "1") {
			t.Errorf("RL_BINDINGS_PROOF=%q: proof = %v", v, c.bindingsProof)
		}
	}
	c, _ := configFromEnv(func(k string) string {
		if k == "RL_BINDINGS_FILE" {
			return " /run/rl/bindings.json "
		}
		return ""
	})
	if c.bindingsFile != "/run/rl/bindings.json" || c.bindingsProof {
		t.Errorf("config = %+v", c)
	}
}

// ---- the log --------------------------------------------------------------------------------------------------------------------------------------------------

// fakeBindingsMode: the engine stops, or leaks, at a point of the bindings' part of the script. It quotes a statement the way DuckDB's parser and binder errors do: the line, from its start.
func fakeBindingsMode(mode, script string, sentinel func(), bindingSentinels func(string)) (bool, int) {
	quote := func(prefix string) string {
		for _, line := range strings.Split(script, "\n") {
			if strings.HasPrefix(line, prefix) {
				return line
			}
		}
		return ""
	}
	switch mode {
	case "secret_error":
		sentinel()
		bindingSentinels(secretsSentinel)
		fmt.Fprintf(os.Stderr, "Catalog Error: Scalar Function with name decod does not exist!\n\nLINE 1: %s\n                                                         ^\n", quote("CREATE SECRET rl_bind_"))
		return true, 1
	case "token_error":
		sentinel()
		bindingSentinels(tokenSentinel)
		fmt.Fprintf(os.Stderr, "Binder Error: bad token\n\nLINE 1: %s\n", quote("CREATE SECRET "+catalogSecret))
		return true, 1
	case "secret_hang":
		sentinel()
		bindingSentinels(secretsSentinel)
		fmt.Fprintf(os.Stderr, "partial: %s\n", quote("CREATE SECRET rl_bind_"))
		time.Sleep(time.Minute)
		return true, 0
	case "secret_kill":
		sentinel()
		bindingSentinels(secretsSentinel)
		fmt.Fprintf(os.Stderr, "partial: %s\n", quote("CREATE SECRET rl_bind_"))
		_ = killSelf()
		time.Sleep(time.Second)
		return true, 1
	case "leak_after_secrets":
		// a run that succeeds, whose engine wrote the statement into stderr and stdout anyway: the belt's case
		fmt.Fprintf(os.Stderr, "WARNING: %s\n", quote("CREATE SECRET rl_bind_"))
		fmt.Printf("%s\n", quote("CREATE SECRET rl_bind_"))
		return false, 0
	}
	return false, 0
}

func leakyRig(t *testing.T, value string, withToken bool) (*bindRig, map[string]string) {
	t.Helper()
	r := newBindRig(t)
	r.secret("source_1", []byte(value))
	r.write(httpDesc("source_1", ""))
	env := r.env(proofEnv())
	if withToken {
		tok := filepath.Join(t.TempDir(), "catalog_token")
		if err := os.WriteFile(tok, []byte("e"+"yJhbGciOiJSUzI1NiJ9.c2ln-_bmF0dXJl"), 0o440); err != nil {
			t.Fatal(err)
		}
		env["RL_CATALOG_TOKEN_FILE"] = tok
	}
	return r, env
}

func TestAFailedSecretStatementLeavesNeitherTheValueNorItsHexInAnyLogAndIsBindingRefused(t *testing.T) {
	// A value short enough to be in every log (the belt does not look for it) is the case only the truncation can fence; a longer one is both.
	for _, value := range []string{"Sup3r'Secret\nvalue-é-1234", "pw'1\n"} {
		for _, withToken := range []bool{false, true} {
			_, env := leakyRig(t, value, withToken)
			h := newHarness(t, "SELECT 1", env)
			h.mode("secret_error")
			m, code, lines := h.runTask()
			if code != 1 || m.Status != "failed" || m.ErrorClass != classBindingRefused {
				t.Errorf("%q token=%v: metrics = %+v code %d, want failed/binding_refused", value, withToken, m, code)
			}
			hx := hex.EncodeToString([]byte(value))
			everything := h.errorLog() + readAll(filepath.Join(h.work, "duckdb.out")) + strings.Join(lines, "\n")
			if strings.Contains(everything, hx) || strings.Contains(everything, "from_hex") || strings.Contains(everything, "Sup3r") {
				t.Errorf("%q token=%v: the value or its hex is in a log or in stdout:\n%s", value, withToken, everything)
			}
			if log := h.errorLog(); !strings.Contains(log, "binding_refused") || !strings.Contains(log, "source_1") || strings.Contains(log, "Catalog Error") {
				t.Errorf("%q token=%v: error.log is not the runner's own note: %q", value, withToken, log)
			}
			// the ack says the files were restricted and no secret was made
			if lines[0] != `RL_BINDINGS {"v":1,"made":[],"restricted":true}` {
				t.Errorf("%q token=%v: ack = %q", value, withToken, lines[0])
			}
		}
	}
}

func readAll(path string) string { b, _ := os.ReadFile(path); return string(b) }

func TestAFailedCatalogTokenStatementKeepsTheTokensClassAndIsScrubbedToo(t *testing.T) {
	_, env := leakyRig(t, "a-value-for-the-binding", true)
	h := newHarness(t, "SELECT 1", env)
	h.mode("token_error")
	m, code, lines := h.runTask()
	if code != 1 || m.ErrorClass != classRunnerIO {
		t.Errorf("metrics = %+v code %d, want runner_io_error (the class a bad token already has)", m, code)
	}
	if log := h.errorLog(); strings.Contains(log, "yJhbGci") || strings.Contains(log, "LINE 1") {
		t.Errorf("the token's statement is in error.log: %q", log)
	}
	if lines[0] != `RL_BINDINGS {"v":1,"made":[],"restricted":true}` {
		t.Errorf("ack = %q", lines[0])
	}
}

func TestAnEngineThatIsKilledOrTimedOutBetweenTheSentinelsLeavesNoStatementInTheLog(t *testing.T) {
	const value = "Sup3r'Secret-value-1234"
	hx := hex.EncodeToString([]byte(value))
	for _, tc := range []struct {
		mode  string
		class string
		code  int
		env   map[string]string
	}{
		{"secret_hang", classTimeout, 2, map[string]string{"RL_TIMEOUT_SECONDS": "1"}},
		{"secret_kill", classBindingRefused, 1, nil},
	} {
		_, env := leakyRig(t, value, false)
		for k, v := range tc.env {
			env[k] = v
		}
		h := newHarness(t, "SELECT 1", env)
		h.mode(tc.mode)
		m, code, lines := h.runTask()
		if code != tc.code || m.ErrorClass != tc.class {
			t.Errorf("%s: metrics = %+v code %d, want %s/%d", tc.mode, m, code, tc.class, tc.code)
		}
		if strings.Contains(h.errorLog(), hx[:24]) {
			t.Errorf("%s: the hex is in error.log: %q", tc.mode, h.errorLog())
		}
		if len(lines) < 2 || !strings.HasPrefix(lines[0], "RL_BINDINGS ") {
			t.Errorf("%s: stdout = %q", tc.mode, lines)
		}
	}
}

func TestTheBeltReplacesALogThatHoldsAValuesBytesOrHexAfterASuccessfulRun(t *testing.T) {
	const value = "Sup3r'Secret-value-1234"
	_, env := leakyRig(t, value, false)
	h := newHarness(t, "SELECT 1", env)
	h.mode("leak_after_secrets")
	m, code, _ := h.runTask()
	if code != 0 || m.Status != "succeeded" {
		t.Fatalf("metrics = %+v code %d", m, code)
	}
	if strings.Contains(h.errorLog(), "from_hex") || strings.Contains(readAll(filepath.Join(h.work, "duckdb.out")), "from_hex") {
		t.Errorf("a log still holds the statement: error.log %q", h.errorLog())
	}
	// a value's own bytes too, in either file; a log that holds neither is left alone
	for _, f := range []string{"error.log", "duckdb.out"} {
		_, env := leakyRig(t, value, false)
		hh := newHarness(t, "SELECT 1", env)
		p := &bindingsPlan{desc: &bindingsDescriptor{bindings: []bindingDecl{{Binding: "source_1"}}}, values: []bindingValue{{"source_1", []byte(value)}}}
		if err := os.WriteFile(filepath.Join(hh.work, f), []byte("x "+value+" y"), 0o600); err != nil {
			t.Fatal(err)
		}
		other := "duckdb.out"
		if f == other {
			other = "error.log"
		}
		if err := os.WriteFile(filepath.Join(hh.work, other), []byte("an ordinary engine message"), 0o600); err != nil {
			t.Fatal(err)
		}
		p.beltScan(filepath.Join(hh.work, "error.log"), filepath.Join(hh.work, "duckdb.out"))
		if strings.Contains(readAll(filepath.Join(hh.work, f)), "Sup3r") {
			t.Errorf("%s still holds the value after the belt", f)
		}
		if readAll(filepath.Join(hh.work, other)) != "an ordinary engine message" {
			t.Errorf("the belt rewrote %s, which held nothing", other)
		}
	}
	// a short value is in every log and is not looked for
	p := &bindingsPlan{desc: &bindingsDescriptor{bindings: []bindingDecl{{Binding: "source_1"}}}, values: []bindingValue{{"source_1", []byte("a")}}}
	hh := newHarness(t, "SELECT 1", nil)
	if err := os.WriteFile(filepath.Join(hh.work, "error.log"), []byte("a b c"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.beltScan(filepath.Join(hh.work, "error.log"), filepath.Join(hh.work, "duckdb.out"))
	if readAll(filepath.Join(hh.work, "error.log")) != "a b c" {
		t.Error("the belt rewrote a log for a one-byte value")
	}
}

func TestTheNoteNamesTheRuleAndTheBindingAndNothingElse(t *testing.T) {
	p := &bindingsPlan{desc: &bindingsDescriptor{bindings: []bindingDecl{{Binding: "a_1", Host: strp("secret-host"), Database: strp("secret-db"), User: strp("secret-user"), Scope: strp("https://secret-scope/")}, {Binding: "b_2"}}}}
	n := p.scrubNote()
	for _, leak := range []string{"secret-host", "secret-db", "secret-user", "secret-scope"} {
		if strings.Contains(n, leak) {
			t.Errorf("the note holds %q: %s", leak, n)
		}
	}
	if !strings.Contains(n, "binding_refused") || !strings.Contains(n, ruleSecretStatement) || !strings.Contains(n, "a_1,b_2") || strings.Count(n, "\n") != 1 {
		t.Errorf("note = %q", n)
	}
}

// ---- the ack ----------------------------------------------------------------------------------------------------------------------------------------------------

func TestTheAckIsReadFromTheSentinelsAndNotFromTheScript(t *testing.T) {
	// an engine that fails in init: the script carried the restriction, and the ack must not say it passed
	_, env := leakyRig(t, "a-value-for-the-binding", false)
	h := newHarness(t, "SELECT 1", env)
	h.mode("init_error")
	m, code, lines := h.runTask()
	if code != 1 || m.ErrorClass != classRunnerIO {
		t.Errorf("metrics = %+v code %d, want runner_io_error", m, code)
	}
	if lines[0] != `RL_BINDINGS {"v":1,"made":[],"restricted":false}` {
		t.Errorf("an engine that never printed the init sentinel: ack = %q", lines[0])
	}
	// an engine that cannot be started
	_, env = leakyRig(t, "a-value-for-the-binding", false)
	env["RL_DUCKDB_BIN"] = "/nonexistent/duckdb"
	h = newHarness(t, "SELECT 1", env)
	_, _, lines = h.runTask()
	if lines[0] != `RL_BINDINGS {"v":1,"made":[],"restricted":false}` {
		t.Errorf("an engine that did not start: ack = %q", lines[0])
	}
	// a statement that fails after the secrets: restricted and made are true, and the failure is the statement's
	_, env = leakyRig(t, "a-value-for-the-binding", false)
	h = newHarness(t, "SELECT 1", env)
	h.mode("sql_error")
	m, code, lines = h.runTask()
	if code != 1 || m.ErrorClass != classCatalog {
		t.Errorf("metrics = %+v code %d, want catalog_error (the statement's own)", m, code)
	}
	if lines[0] != `RL_BINDINGS {"v":1,"made":["source_1"],"restricted":true}` {
		t.Errorf("ack = %q", lines[0])
	}
}

func TestTheAckLineIsExactlyOneLineWithTheShapeThePlaneParses(t *testing.T) {
	var out bytes.Buffer
	emit(&out, metrics{Status: "succeeded", bindings: &bindingsAck{V: 1, Made: []string{"a", "b"}, Restricted: true}})
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || lines[0] != `RL_BINDINGS {"v":1,"made":["a","b"],"restricted":true}` || !strings.HasPrefix(lines[1], "RL_METRICS ") {
		t.Errorf("lines = %q", lines)
	}
	out.Reset()
	emit(&out, metrics{Status: "succeeded", bindings: &bindingsAck{V: 1}})
	if !strings.HasPrefix(out.String(), `RL_BINDINGS {"v":1,"made":null,`) {
		// Made must never be null: settle() makes it an empty list; a hand-made ack with none is the test's own and is not what the runner prints.
		t.Logf("a hand-made ack with a nil list prints null (settle never makes one): %s", out.String())
	}
	if a := (&bindingsPlan{desc: &bindingsDescriptor{}}).settle(filepath.Join(t.TempDir(), "none")); a.Made == nil {
		t.Error("settle made a nil list, which would print null")
	}
	// the metrics line itself is untouched by the ack
	var m metrics
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "RL_METRICS ")), &m); err != nil {
		t.Errorf("metrics line: %v", err)
	}
	if strings.Contains(lines[1], "bindings") {
		t.Errorf("the ack leaked into the metrics line: %s", lines[1])
	}
}

func TestTheValuesAreZeroedWhenThePlanIsWiped(t *testing.T) {
	v := []byte("a-secret-value")
	p := &bindingsPlan{values: []bindingValue{{"a", v}}}
	p.wipe()
	for _, b := range v {
		if b != 0 {
			t.Fatal("a value byte survived wipe")
		}
	}
}

// ---- the script is built as bytes the engine is fed ---------------------------------------------------------------------------------------------------------

func TestTheComposedBytesAreTheComposedString(t *testing.T) {
	initSQL, err := os.ReadFile(filepath.Join("testdata", "golden", "init.sql"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config{workDir: "/work", resultFile: "/work/result.parquet", threads: "2", catalogToken: "canary.CATALOG-TOKEN_0123456789"}
	a := composeScript(string(initSQL), "", cfg, "/work/tmp", "/work/rowcount.csv", "/work/bytes_read.csv", "SELECT 1", kindQuery)
	b := composeScriptBytes(string(initSQL), "", cfg, "/work/tmp", "/work/rowcount.csv", "/work/bytes_read.csv", "SELECT 1", kindQuery)
	if a != string(b) {
		t.Error("composeScript and composeScriptBytes differ")
	}
	_ = context.Background()
}

// The three settings and the secrets are the runner's, in the order measured, and nothing in the image's init.sql may set them: a second setting after the runner's would be a different order.
func TestTheImagesInitScriptHoldsNoneOfTheRunnersFileSettingsAndLoadsNoDatabaseExtension(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "init.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var statements []string
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "--") {
			statements = append(statements, strings.ToLower(line))
		}
	}
	for _, setting := range []string{"allowed_directories", "enable_external_access", "allow_persistent_secrets", "lock_configuration", "postgres", "mysql"} {
		if strings.Contains(strings.Join(statements, "\n"), setting) {
			t.Errorf("init.sql names %q", setting)
		}
	}
	// and the script of a descriptor loads nothing init.sql did not: the database extensions are 'LOAD'ed only for a type this runner can make, and it makes none
	r := newBindRig(t)
	r.secret("source_1", []byte("pw-value-1"))
	r.write(httpDesc("source_1", ""))
	h := newHarness(t, "SELECT 1", r.env(proofEnv()))
	if _, code, _ := h.runTask(); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, line := range strings.Split(h.script(), "\n") {
		if strings.HasPrefix(strings.ToUpper(line), "LOAD ") && !strings.Contains("LOAD httpfs; LOAD iceberg;", line) {
			t.Errorf("the script loads %q", line)
		}
		if strings.HasPrefix(strings.ToUpper(line), "INSTALL ") {
			t.Errorf("the script installs: %q", line)
		}
	}
}
