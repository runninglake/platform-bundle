package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// THE REAL ENGINE, for the bindings. Everything here runs the runner against the pinned DuckDB CLI (v1.5.5 d8cdaa33fd, the version .github/actions/duckdb pins), because the fake cannot say what the
// engine refuses after the three settings, what it accepts inside a CREATE SECRET, what it puts in an error, or what bytes a secret puts on a wire. A test SKIPS, with the reason in its output, when the CLI
// is not on PATH, is another version, or has no cached httpfs: it never INSTALLs, and it never reaches past the loopback.
//
// THE BUILD OF THE CLI is the Homebrew darwin-arm64 one on a laptop, not the pinned linux-amd64 zip, and nothing here ran under gVisor, on a Kubernetes mount or against the image: bindings.go says what
// that leaves unmeasured.

const pinnedEngineVersion = "v1.5.5 (Variegata) d8cdaa33fd"

type engineRig struct{ bin, extDir string }

func pinnedEngine(t *testing.T) engineRig {
	t.Helper()
	path, err := exec.LookPath("duckdb")
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatal("duckdb is not on PATH in CI, so the bindings' real-engine tests are unrun")
		}
		t.Skip("NOT RUN: duckdb is not on PATH (the bindings' real-engine tests need the pinned " + pinnedEngineVersion + ")")
	}
	out, err := exec.Command(path, "--version").Output()
	if err != nil || !strings.Contains(string(out), pinnedEngineVersion) {
		t.Skipf("NOT RUN: %s is %q, not the pinned %s", path, strings.TrimSpace(string(out)), pinnedEngineVersion)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("NOT RUN: no home directory to find the cached extensions in")
	}
	if m, _ := filepath.Glob(filepath.Join(home, ".duckdb", "extensions", "v1.5.5", "*", "httpfs.duckdb_extension")); len(m) == 0 {
		t.Skip("NOT RUN: httpfs is not cached under ~/.duckdb/extensions/v1.5.5 (these tests never INSTALL)")
	}
	return engineRig{bin: path, extDir: filepath.Join(home, ".duckdb", "extensions")}
}

// initFile is init.sql for these runs: the image's own shape (extension directory, no autoinstall, no autoload, httpfs loaded) and no network settings.
func (e engineRig) initFile(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "init.sql")
	sql := fmt.Sprintf("SET extension_directory=%s;\nSET autoinstall_known_extensions=false;\nSET autoload_known_extensions=false;\nSET allow_unsigned_extensions=false;\nLOAD httpfs;\nSET threads=2;\n", sqlString(e.extDir))
	if err := os.WriteFile(f, []byte(sql), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// run is the runner, against the real engine, for a descriptor in r (or none).
func (e engineRig) run(t *testing.T, r *bindRig, sql string, env map[string]string) (metrics, int, []string, *harness) {
	t.Helper()
	vars := map[string]string{"RL_DUCKDB_BIN": e.bin, "RL_INIT_FILE": e.initFile(t)}
	if r != nil {
		vars["RL_BINDINGS_FILE"] = r.file()
	}
	for k, v := range env {
		vars[k] = v
	}
	h := newHarness(t, sql, vars)
	m, code, lines := h.runTask()
	return m, code, lines, h
}

// resultOf reads the Parquet the run wrote, through the CLI.
func (e engineRig) resultOf(t *testing.T, h *harness, expr string) string {
	t.Helper()
	return duckSQL(t, e.bin, "SELECT "+expr+" FROM read_parquet("+sqlString(filepath.Join(h.work, "result.parquet"))+")")
}

const canaryText = "CANARY-file-text.0123456789.abcdef"

// ---- what the engine can no longer read ----------------------------------------------------------------------------------------------------------------------

func TestTheRealEngineOpensOnlyWhatItIsNamedOnceADescriptorIsPresent(t *testing.T) {
	e := pinnedEngine(t)
	r := newBindRig(t)
	r.write(`{"v":1,"bindings":[]}`) // no access at all: the descriptor's presence is the trigger
	canaryFile := filepath.Join(r.secretsDir(), "canary_file")
	if err := os.WriteFile(canaryFile, []byte(canaryText), 0o440); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "data.txt"), []byte("outside-data"), 0o644); err != nil {
		t.Fatal(err)
	}
	probe := newHarness(t, "SELECT 1", nil) // only to learn a work directory's shape for the '..' spelling
	rel, err := filepath.Rel(probe.work, canaryFile)
	if err != nil {
		t.Fatal(err)
	}
	for name, sql := range map[string]string{
		"read_text":                  "SELECT content FROM read_text(" + sqlString(canaryFile) + ")",
		"read_blob":                  "SELECT decode(content) FROM read_blob(" + sqlString(canaryFile) + ")",
		"read_csv":                   "SELECT * FROM read_csv(" + sqlString(canaryFile) + ", header=false, columns={'c':'VARCHAR'}, delim=E'\\x01')",
		"a glob of the directory":    "SELECT * FROM glob(" + sqlString(r.secretsDir()+"/*") + ")",
		"read_text with a glob":      "SELECT content FROM read_text(" + sqlString(r.secretsDir()+"/*") + ")",
		"the descriptor":             "SELECT content FROM read_text(" + sqlString(r.file()) + ")",
		"a path through ..":          "SELECT content FROM read_text(" + sqlString(probe.work+"/"+rel) + ")",
		"a .. from the work dir":     "SELECT content FROM read_text(" + sqlString(probe.work+"/tmp/../../"+filepath.Base(outside)+"/data.txt") + ")",
		"a file outside every entry": "SELECT content FROM read_text(" + sqlString(filepath.Join(outside, "data.txt")) + ")",
		"a relative path":            "SELECT content FROM read_text('testdata/golden/init.sql')",
		"a system file":              "SELECT content FROM read_text('/etc/hosts')",
	} {
		m, code, lines, h := e.run(t, r, sql, nil)
		if code != 1 || m.Status != "failed" || m.ErrorClass != classPermission {
			t.Errorf("%s: metrics = %+v code %d, want failed/permission_denied\n%s", name, m, code, h.errorLog())
		}
		if _, err := os.Stat(filepath.Join(h.work, "result.parquet")); err == nil {
			t.Errorf("%s: a result file was written", name)
		}
		everything := h.errorLog() + readAll(filepath.Join(h.work, "duckdb.out")) + strings.Join(lines, "\n")
		if strings.Contains(everything, canaryText) || strings.Contains(everything, "outside-data") {
			t.Errorf("%s: the canary reached a log or stdout", name)
		}
		if lines[0] != `RL_BINDINGS {"v":1,"made":[],"restricted":true}` {
			t.Errorf("%s: ack = %q", name, lines[0])
		}
	}
	// THE CONTROL: the same statement shapes with no descriptor run as they always did, so the refusals above are the restriction's and not the statements'. The fixtures are in a
	// directory of their own.
	fixtures := t.TempDir()
	plainFile := filepath.Join(fixtures, "plain_file")
	if err := os.WriteFile(plainFile, []byte(canaryText), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ sql, want string }{
		"a fixture":      {"SELECT content FROM read_text(" + sqlString(plainFile) + ")", canaryText},
		"outside a list": {"SELECT content FROM read_text(" + sqlString(filepath.Join(outside, "data.txt")) + ")", "outside-data"},
	} {
		m, code, _, h := e.run(t, nil, tc.sql, nil)
		if code != 0 || m.Status != "succeeded" {
			t.Fatalf("control %s: metrics = %+v code %d\n%s", name, m, code, h.errorLog())
		}
		if got := e.resultOf(t, h, "content"); !strings.Contains(got, tc.want) {
			t.Errorf("control %s: result = %q, want it to hold %q", name, got, tc.want)
		}
	}
}

func TestTheRealEngineStillWritesTheResultCountsTheRowsAndReadsWhatTheDescriptorNames(t *testing.T) {
	e := pinnedEngine(t)
	r := newBindRig(t)
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "rows.csv"), []byte("a,b\n1,x\n2,y\n3,z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	neighbour := data + "-neighbour"
	if err := os.MkdirAll(neighbour, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(neighbour) })
	if err := os.WriteFile(filepath.Join(neighbour, "rows.csv"), []byte("a,b\n9,n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// a projected volume's layout: the file is a symlink to ..data/<key>, and ..data to a timestamped directory, all inside the mount
	params := filepath.Join(t.TempDir(), "params")
	if err := os.MkdirAll(filepath.Join(params, "..2026_11_15"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(params, "..2026_11_15", "params.json"), []byte(`[{"name":"n","value":"7"}]`), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..2026_11_15", filepath.Join(params, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..data/params.json", filepath.Join(params, "params.json")); err != nil {
		t.Fatal(err)
	}
	r.write(fmt.Sprintf(`{"v":1,"access":{"directories":[%s,%s]},"bindings":[]}`, jq(data), jq(params)))

	// the result COPY and the row count, with a result that spills no file of the customer's
	m, code, lines, h := e.run(t, r, "SELECT i, 'v' || i AS s FROM range(7) t(i)", nil)
	if code != 0 || m.Status != "succeeded" || m.RowsOut != 7 || m.ResultBytes <= 0 {
		t.Fatalf("metrics = %+v code %d\n%s", m, code, h.errorLog())
	}
	if got := e.resultOf(t, h, "sum(i)"); got != "21" {
		t.Errorf("the result's sum = %q, want 21", got)
	}
	if lines[0] != `RL_BINDINGS {"v":1,"made":[],"restricted":true}` {
		t.Errorf("ack = %q", lines[0])
	}
	// a directory the descriptor names is read; one that only shares its letters is not
	m, code, _, h = e.run(t, r, "SELECT sum(a) AS s FROM read_csv("+sqlString(filepath.Join(data, "rows.csv"))+")", nil)
	if code != 0 || m.RowsOut != 1 || e.resultOf(t, h, "s") != "6" {
		t.Errorf("a named directory: metrics = %+v code %d\n%s", m, code, h.errorLog())
	}
	m, code, _, h = e.run(t, r, "SELECT sum(a) AS s FROM read_csv("+sqlString(filepath.Join(neighbour, "rows.csv"))+")", nil)
	if code != 1 || m.ErrorClass != classPermission {
		t.Errorf("a neighbour that shares the name's letters: metrics = %+v code %d\n%s", m, code, h.errorLog())
	}
	// the symlinked layout of a projected volume reads through the allowed directory
	m, code, _, h = e.run(t, r, "SELECT n FROM read_json("+sqlString(filepath.Join(params, "params.json"))+", columns={'name':'VARCHAR','value':'VARCHAR'}) t(n, v) ", nil)
	if code != 0 || m.RowsOut != 1 {
		t.Errorf("a projected-volume layout: metrics = %+v code %d\n%s", m, code, h.errorLog())
	}
	// a task's temp directory (spill) is the work directory's tmp: a sort over the memory limit still runs
	m, code, _, h = e.run(t, r, "SELECT i, md5(i::VARCHAR) AS h FROM range(4000000) t(i) ORDER BY h", map[string]string{"RL_MEMORY_LIMIT": "128MB"})
	if code != 0 || m.RowsOut != 4000000 {
		t.Errorf("a spilling sort under the restriction: metrics = %+v code %d\n%s", m, code, h.errorLog())
	}
}

// A work or result directory that is a link to a protected directory is refused before the engine runs, and one that is a link to an ordinary directory runs and writes its result: the
// engine resolves the entry and the path it opens, and the list carries the resolved directory.
func TestTheRealEngineRunsWithALinkedWorkDirectoryOnlyWhenItLeadsNowhereProtected(t *testing.T) {
	e := pinnedEngine(t)
	r := newBindRig(t)
	r.write(`{"v":1,"bindings":[]}`)
	links := t.TempDir()
	toRL := filepath.Join(links, "to_rl")
	if err := os.Symlink(r.rl, toRL); err != nil {
		t.Fatal(err)
	}
	m, code, _, h := e.run(t, r, "SELECT 1", map[string]string{"RL_WORK_DIR": toRL})
	if code != 1 || m.ErrorClass != classInvalidQuery {
		t.Errorf("a work directory that is a link to the descriptor's directory: metrics = %+v code %d", m, code)
	}
	if _, err := os.Stat(filepath.Join(h.work, "duckdb.out")); err == nil {
		t.Error("the engine was started")
	}
	realWork := t.TempDir()
	linkWork := filepath.Join(links, "work")
	if err := os.Symlink(realWork, linkWork); err != nil {
		t.Fatal(err)
	}
	m, code, _, h = e.run(t, r, "SELECT i FROM range(3) t(i)", map[string]string{"RL_WORK_DIR": linkWork, "RL_RESULT_FILE": filepath.Join(linkWork, "result.parquet")})
	if code != 0 || m.Status != "succeeded" || m.RowsOut != 3 {
		t.Fatalf("a work directory that is a link to an ordinary directory: metrics = %+v code %d\n%s", m, code, h.errorLog())
	}
	if got := duckSQL(t, e.bin, "SELECT count(*) FROM read_parquet("+sqlString(filepath.Join(realWork, "result.parquet"))+")"); got != "3" {
		t.Errorf("rows in the result = %q, want 3", got)
	}
}

// A single quote in a directory the descriptor names is an SQL string literal's quote, doubled: the directory is read, and an entry that looks like the end of the list is only an
// unusual directory name.
func TestTheRealEngineTakesAQuoteInAnAccessDirectoryAsPartOfItsName(t *testing.T) {
	e := pinnedEngine(t)
	r := newBindRig(t)
	quoted := filepath.Join(t.TempDir(), "it's")
	if err := os.MkdirAll(quoted, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(quoted, "rows.csv"), []byte("a\n4\n5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.write(fmt.Sprintf(`{"v":1,"access":{"directories":[%s,"/no/such/q'];SELECT"]},"bindings":[]}`, jq(quoted)))
	m, code, lines, h := e.run(t, r, "SELECT sum(a) AS s FROM read_csv("+sqlString(filepath.Join(quoted, "rows.csv"))+")", nil)
	if code != 0 || m.Status != "succeeded" || e.resultOf(t, h, "s") != "9" {
		t.Fatalf("metrics = %+v code %d\n%s", m, code, h.errorLog())
	}
	if lines[0] != `RL_BINDINGS {"v":1,"made":[],"restricted":true}` {
		t.Errorf("ack = %q", lines[0])
	}
}

// The composed script, exactly as the runner feeds it, and then more statements of a customer: none can undo it.
func TestTheRealEngineKeepsTheRestrictionAndTheSecretsAfterTheLock(t *testing.T) {
	e := pinnedEngine(t)
	r := newBindRig(t)
	r.secret("source_1", []byte("Sup3r'Secret value\nx"))
	r.write(httpDesc("source_1", ""))
	work := t.TempDir()
	cfg := config{bindingsFile: r.file(), bindingsProof: true, workDir: work, resultFile: filepath.Join(work, "result.parquet")}
	plan, class, err := prepareBindings(cfg)
	if err != nil {
		t.Fatalf("%s: %v", class, err)
	}
	cfg.bindings = plan
	initSQL, _ := os.ReadFile(e.initFile(t))
	base := composeScriptBytes(string(initSQL), "", cfg, filepath.Join(work, "tmp"), filepath.Join(work, "rowcount.csv"), filepath.Join(work, "bytes.csv"), "SELECT 1", kindQuery)
	// cut the result's own statements: only the settings, the lock and the secrets are being tested
	cut := bytes.Index(base, []byte("CREATE TEMP TABLE"))
	if cut < 0 {
		t.Fatal("no statement in the script")
	}
	prefix := string(base[:cut])
	for name, tail := range map[string]string{
		"external access back on":       "SET enable_external_access=true;",
		"the directories widened":       "SET allowed_directories=['/'];",
		"persistent secrets back on":    "SET allow_persistent_secrets=true;",
		"the lock released":             "SET lock_configuration=false;",
		"a persistent secret":           "CREATE PERSISTENT SECRET p (TYPE http, BEARER_TOKEN 'x');",
		"unredacted secrets":            "SELECT * FROM duckdb_secrets(redact := false);",
		"unredacted secrets by name":    "SET allow_unredacted_secrets=true;",
		"an extension from the network": "INSTALL httpfs;",
		"a file":                        "SELECT * FROM read_text('" + filepath.Join(r.secretsDir(), "source_1") + "');",
	} {
		out, code := e.cli(t, prefix+tail+"\n")
		if code == 0 || strings.Contains(out, "Sup3r") {
			t.Errorf("%s: exit %d, the engine accepted it or printed the value:\n%s", name, code, out)
		}
	}
	// the secret exists and its token is redacted
	out, code := e.cli(t, prefix+"SELECT name, type, secret_string FROM duckdb_secrets() WHERE name = 'rl_bind_source_1';\n")
	if code != 0 || !strings.Contains(out, "rl_bind_source_1") || strings.Contains(out, "Sup3r") || !strings.Contains(out, "redacted") {
		t.Errorf("the secret is not listed, or is not redacted (exit %d):\n%s", code, out)
	}
	// and the persistent form left no file
	if m, _ := filepath.Glob(filepath.Join(work, ".duckdb", "*", "*")); len(m) != 0 {
		t.Errorf("a persistent secret was written: %v", m)
	}
	plan.wipe()
}

// cli feeds stdin to the CLI exactly as the runner does (-batch -bail -no-init, a minimal environment) and returns stdout and stderr together with the exit code.
func (e engineRig) cli(t *testing.T, stdin string) (string, int) {
	t.Helper()
	cmd := exec.Command(e.bin, "-batch", "-bail", "-no-init", "-csv", "-noheader")
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = []string{"HOME=" + t.TempDir()}
	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		code = 1
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
	}
	return string(out), code
}

// ---- the bytes on a wire ----------------------------------------------------------------------------------------------------------------------------------------

// stub is a loopback HTTP server that records every byte it is sent, whatever they are: a header with a newline in it is not parsed away.
type stub struct {
	ln   net.Listener
	mu   sync.Mutex
	raw  []byte
	port int
}

func startStub(t *testing.T) *stub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("NOT RUN: no loopback listener: %v", err)
	}
	s := &stub{ln: ln, port: ln.Addr().(*net.TCPAddr).Port}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go s.serve(c)
		}
	}()
	return s
}

func (s *stub) serve(c net.Conn) {
	defer c.Close()
	var buf []byte
	tmp := make([]byte, 64<<10)
	for {
		n, err := c.Read(tmp)
		if n > 0 {
			s.mu.Lock()
			s.raw = append(s.raw, tmp[:n]...)
			s.mu.Unlock()
			buf = append(buf, tmp[:n]...)
			for {
				i := bytes.Index(buf, []byte("\r\n\r\n"))
				if i < 0 {
					break
				}
				head := buf[:i]
				buf = buf[i+4:]
				body := "ok\n"
				if bytes.HasPrefix(head, []byte("HEAD ")) {
					body = ""
				}
				fmt.Fprintf(c, "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nContent-Type: text/plain\r\nAccept-Ranges: none\r\n\r\n%s", body)
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *stub) received() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.raw...)
}

func (s *stub) origin() string { return fmt.Sprintf("http://127.0.0.1:%d/", s.port) }

func TestTheRealEngineSendsAnHTTPBearerSecretToALoopbackStubAsTheFilesExactBytes(t *testing.T) {
	e := pinnedEngine(t)
	// A value of 64 KiB is a 128 KiB line of hex on the engine's stdin, so the long line and decode(from_hex()) inside CREATE SECRET are measured by the same cases.
	mixed := func(n int) []byte {
		unit := []byte("p'\"\\;--/*é日🔑\t#.x ")
		return bytes.Repeat(unit, n/len(unit)+1)[:n]
	}
	trim := func(b []byte) []byte {
		for len(b) > 0 && !validUTF8(b) {
			b = b[:len(b)-1]
		}
		return b
	}
	for _, tc := range []struct {
		name  string
		value []byte
	}{
		{"a quote, both quotes, a backslash, a newline and non-ASCII", []byte("pa'ss\"w\\o\nrd-é")},
		{"a trailing newline", []byte("pw-with-a-trailing-newline\n")},
		{"a trailing CR LF", []byte("pw-with-crlf\r\n")},
		{"a leading and a trailing space", []byte(" pw ")},
		{"a statement and a dot-command inside", []byte("x'); DROP SECRET y; --\n.shell id\n")},
		{"2-, 3- and 4-byte UTF-8", []byte("pässwörd-日本語-🔑")},
		{"4 KiB of mixed punctuation", trim(mixed(4096))},
		{"exactly 65536 bytes", trim(mixed(65536))},
	} {
		st := startStub(t)
		r := newBindRig(t)
		r.secret("source_1", tc.value)
		r.write(fmt.Sprintf(`{"v":1,"access":{"url_prefixes":[%q]},"bindings":[{"binding":"source_1","type":"http","scope":%q,"http_proxy":"direct"}]}`, st.origin(), st.origin()))
		m, code, lines, h := e.run(t, r, "SELECT content FROM read_text('"+st.origin()+"x')", proofEnv())
		if code != 0 || m.Status != "succeeded" {
			t.Errorf("%s: metrics = %+v code %d\n%s", tc.name, m, code, h.errorLog())
			continue
		}
		if lines[0] != `RL_BINDINGS {"v":1,"made":["source_1"],"restricted":true}` {
			t.Errorf("%s: ack = %q", tc.name, lines[0])
		}
		want := append([]byte("Authorization: Bearer "), tc.value...)
		want = append(want, "\r\n"...)
		got := st.received()
		if !bytes.Contains(got, want) {
			i := bytes.Index(got, []byte("Authorization: Bearer "))
			shown := got
			if i >= 0 && len(got[i:]) > 200 {
				shown = got[i : i+200]
			} else if i >= 0 {
				shown = got[i:]
			}
			t.Errorf("%s: the stub did not receive the file's exact bytes after 'Authorization: Bearer ' (received %d bytes; from the header: %q)", tc.name, len(got), shown)
		}
		if tc.name == "exactly 65536 bytes" {
			t.Logf("MEASURED: a 65536-byte value is a %d-byte line of hex in CREATE SECRET ... decode(from_hex('...')); the CLI took it and the stub received all %d bytes", 2*65536, len(tc.value))
		}
		// the value is in no log and not in stdout
		everything := h.errorLog() + readAll(filepath.Join(h.work, "duckdb.out")) + strings.Join(lines, "\n")
		if len(tc.value) > 8 && (strings.Contains(everything, string(tc.value)) || strings.Contains(everything, hex.EncodeToString(tc.value))) {
			t.Errorf("%s: the value or its hex is in a log", tc.name)
		}
	}
}

// The statement a customer writes cannot make the secret go anywhere else: a request to another origin carries no bearer (the scope is a prefix of the origin) and, with the origin not allowed, is not made at all.
func TestTheRealEngineSendsTheBearerOnlyWhereTheScopeSaysAndDoesNotReachAnOriginTheDescriptorDoesNotName(t *testing.T) {
	e := pinnedEngine(t)
	scoped, other := startStub(t), startStub(t)
	r := newBindRig(t)
	r.secret("source_1", []byte("scoped-only-secret-value"))
	r.write(fmt.Sprintf(`{"v":1,"access":{"url_prefixes":[%q,%q]},"bindings":[{"binding":"source_1","type":"http","scope":%q,"http_proxy":"direct"}]}`, scoped.origin(), other.origin(), scoped.origin()))
	m, code, _, h := e.run(t, r, "SELECT content FROM read_text('"+other.origin()+"x')", proofEnv())
	if code != 0 || m.Status != "succeeded" {
		t.Fatalf("metrics = %+v code %d\n%s", m, code, h.errorLog())
	}
	if len(other.received()) == 0 {
		t.Fatal("the other origin was never reached, so the test shows nothing")
	}
	if bytes.Contains(other.received(), []byte("scoped-only-secret-value")) || bytes.Contains(other.received(), []byte("Authorization")) {
		t.Errorf("the bearer went to an origin outside its scope:\n%q", other.received())
	}
	// an origin the descriptor does not name is not read at all
	third := startStub(t)
	m, code, _, h = e.run(t, r, "SELECT content FROM read_text('"+third.origin()+"x')", proofEnv())
	if code != 1 || m.ErrorClass != classPermission || len(third.received()) != 0 {
		t.Errorf("an unnamed origin: metrics = %+v code %d, received %d bytes\n%s", m, code, len(third.received()), h.errorLog())
	}
}

// ---- a failing secret statement ---------------------------------------------------------------------------------------------------------------------------------

// breakingEngine is a stand-in for the CLI that misspells the one function of the bindings' statement, so the REAL engine fails it the way it fails any bad statement: with an error that quotes the line.
func (e engineRig) breakingEngine(t *testing.T) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "duckdb-breaking")
	script := "#!/bin/sh\nsed 's/BEARER_TOKEN decode(/BEARER_TOKEN decod(/' | exec " + e.bin + " \"$@\"\n"
	if err := os.WriteFile(f, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestTheRealEnginesErrorQuotesTheHexAndTheRunnerLeavesNoneOfItAnywhere(t *testing.T) {
	e := pinnedEngine(t)
	for _, tc := range []struct {
		name  string
		value []byte
	}{
		{"a short value", []byte("Sup3r'Secret\nvalue-é")},
		{"a 64 KiB value", bytes.Repeat([]byte("Sup3rSecret-"), 65536/12+1)[:65536]},
	} {
		hx := hex.EncodeToString(tc.value)
		r := newBindRig(t)
		r.secret("source_1", tc.value)
		r.write(httpDesc("source_1", ""))

		// WITHOUT THE RUNNER'S SCRUB the engine's own error holds the hex: this is what the scrub is for, and the proof that the test below could fail.
		work := t.TempDir()
		cfg := config{bindingsFile: r.file(), bindingsProof: true, workDir: work, resultFile: filepath.Join(work, "result.parquet")}
		plan, _, err := prepareBindings(cfg)
		if err != nil {
			t.Fatal(err)
		}
		cfg.bindings = plan
		initSQL, _ := os.ReadFile(e.initFile(t))
		script := composeScriptBytes(string(initSQL), "", cfg, filepath.Join(work, "tmp"), filepath.Join(work, "rc.csv"), filepath.Join(work, "b.csv"), "SELECT 1", kindQuery)
		raw := exec.Command(e.breakingEngine(t), "-batch", "-bail", "-no-init", "-csv", "-noheader")
		raw.Stdin = bytes.NewReader(script)
		raw.Env = []string{"HOME=" + work}
		rawOut, _ := raw.CombinedOutput()
		quoted := 0
		for quoted < len(hx) && bytes.Contains(rawOut, []byte(hx[:quoted+1])) {
			quoted++
		}
		if quoted < 16 {
			t.Fatalf("%s: the engine's own error does not quote the hex, so the scrub test below proves nothing:\n%.600s", tc.name, rawOut)
		}
		t.Logf("MEASURED (%s): the engine's error for a failed CREATE SECRET quotes the first %d of the literal's %d hex characters (it cuts the quoted line, so a long value is quoted by its head only)", tc.name, quoted, len(hx))
		plan.wipe()

		// THROUGH THE RUNNER, the same failure leaves nothing.
		m, code, lines, h := e.run(t, r, "SELECT 1", map[string]string{"RL_BINDINGS_PROOF": "1", "RL_DUCKDB_BIN": e.breakingEngine(t)})
		if code != 1 || m.Status != "failed" || m.ErrorClass != classBindingRefused {
			t.Errorf("%s: metrics = %+v code %d, want failed/binding_refused\n%s", tc.name, m, code, h.errorLog())
		}
		var all strings.Builder
		_ = filepath.Walk(h.work, func(p string, info os.FileInfo, err error) error {
			if err == nil && !info.IsDir() {
				b, _ := os.ReadFile(p)
				all.Write(b)
			}
			return nil
		})
		all.WriteString(strings.Join(lines, "\n"))
		for _, probe := range []string{hx[:16], string(tc.value[:12])} {
			if strings.Contains(all.String(), probe) {
				t.Errorf("%s: %q is in the work directory or on stdout", tc.name, probe)
			}
		}
		if log := h.errorLog(); !strings.Contains(log, "binding_refused") || !strings.Contains(log, "source_1") {
			t.Errorf("%s: error.log = %q, want the runner's note", tc.name, log)
		}
		if lines[0] != `RL_BINDINGS {"v":1,"made":[],"restricted":true}` {
			t.Errorf("%s: ack = %q", tc.name, lines[0])
		}
	}
}

func TestTheRealEngineRefusesAMissingOrEmptyValueFileBeforeItRuns(t *testing.T) {
	e := pinnedEngine(t)
	for name, file := range map[string][]byte{"missing": nil, "empty": {}, "only a newline": []byte("\n")} {
		r := newBindRig(t)
		if file != nil {
			r.secret("source_1", file)
		}
		r.write(httpDesc("source_1", ""))
		m, code, lines, h := e.run(t, r, "SELECT 1", proofEnv())
		if code != 1 || m.ErrorClass != classBindingRefused {
			t.Errorf("%s: metrics = %+v code %d, want binding_refused", name, m, code)
		}
		if _, err := os.Stat(filepath.Join(h.work, "duckdb.out")); err == nil {
			t.Errorf("%s: the engine was started", name)
		}
		if len(lines) != 1 || !strings.HasPrefix(lines[0], "RL_METRICS ") {
			t.Errorf("%s: stdout = %q", name, lines)
		}
	}
}

// A run with no descriptor keeps the script this runner always wrote: the same plain query runs and writes its result.
func TestTheRealEngineKeepsTheOldScriptForARunWithNoDescriptor(t *testing.T) {
	e := pinnedEngine(t)
	data := t.TempDir()
	if err := os.WriteFile(filepath.Join(data, "f.csv"), []byte("a\n5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, code, lines, h := e.run(t, nil, "SELECT sum(a) AS s FROM read_csv("+sqlString(filepath.Join(data, "f.csv"))+")", nil)
	if code != 0 || m.Status != "succeeded" || e.resultOf(t, h, "s") != "5" {
		t.Errorf("metrics = %+v code %d\n%s", m, code, h.errorLog())
	}
	if len(lines) != 1 {
		t.Errorf("stdout = %q", lines)
	}
}
