package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary is also the fake duckdb. The runner always invokes the engine with
// -batch first, and `go test` never does, so that argument is the switch. The fake takes
// its instructions from files in HOME, which the runner sets to the work directory,
// because the runner hands its child a minimal environment on purpose.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "-batch" {
		os.Exit(fakeDuckDB())
	}
	os.Exit(m.Run())
}

const canary = "rl_canary_customers"

func fakeDuckDB() int {
	home := os.Getenv("HOME")
	script, _ := io.ReadAll(os.Stdin)
	_ = os.WriteFile(filepath.Join(home, "fake_script.sql"), script, 0o644)
	_ = os.WriteFile(filepath.Join(home, "fake_args"), []byte(strings.Join(os.Args[1:], " ")), 0o644)
	_ = os.WriteFile(filepath.Join(home, "fake_env"), []byte(strings.Join(os.Environ(), "\n")), 0o644)
	modeBytes, _ := os.ReadFile(filepath.Join(home, "fake_mode"))
	mode := strings.TrimSpace(string(modeBytes))

	sentinel := func() { fmt.Println(initSentinel) }
	switch mode {
	case "sql_error":
		sentinel()
		fmt.Fprintf(os.Stderr, "Catalog Error: Table with name %s does not exist!\nLINE 1: SELECT secret_column FROM %s\n", canary, canary)
		return 1
	case "init_error":
		fmt.Fprintln(os.Stderr, `IO Error: Extension "/opt/duckdb/extensions/v1.5.5/linux_amd64/httpfs.duckdb_extension" not found.`)
		return 1
	case "oom":
		sentinel()
		fmt.Fprintln(os.Stderr, "Out of Memory Error: could not allocate block of size 256.0 KiB (9.3 MiB/9.5 MiB used)")
		return 1
	case "hang":
		time.Sleep(time.Minute)
		return 0
	case "sigkill":
		sentinel()
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		time.Sleep(time.Second)
		return 1
	}
	// Success: honour the COPY targets the runner wrote into the script.
	sentinel()
	s := string(script)
	rows := "1"
	if b, err := os.ReadFile(filepath.Join(home, "fake_rows")); err == nil {
		rows = strings.TrimSpace(string(b))
	}
	if m := regexp.MustCompile(`COPY __rl_result TO '([^']+)' \(FORMAT PARQUET\);`).FindStringSubmatch(s); m != nil {
		_ = os.WriteFile(m[1], []byte("PAR1 fake parquet bytes PAR1"), 0o644)
	}
	if m := regexp.MustCompile(`COPY \(SELECT count\(\*\) FROM __rl_result\) TO '([^']+)' \(FORMAT CSV, HEADER false\);`).FindStringSubmatch(s); m != nil {
		_ = os.WriteFile(m[1], []byte(rows+"\n"), 0o644)
	} else {
		// A non-query statement may print rows; they must never reach the pod's stdout.
		fmt.Println("42,leaked_row_value")
	}
	return 0
}

type harness struct {
	t    *testing.T
	work string
	cfg  config
	out  bytes.Buffer
}

func newHarness(t *testing.T, sql string, env map[string]string) *harness {
	t.Helper()
	work := t.TempDir()
	queryFile := filepath.Join(work, "query.sql")
	if err := os.WriteFile(queryFile, []byte(sql), 0o644); err != nil {
		t.Fatal(err)
	}
	initFile := filepath.Join(work, "init.sql")
	if err := os.WriteFile(initFile, []byte("SET extension_directory='/opt/duckdb/extensions';\nLOAD httpfs;\nLOAD iceberg;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{
		"RL_QUERY_FILE":   queryFile,
		"RL_WORK_DIR":     work,
		"RL_RESULT_FILE":  filepath.Join(work, "result.parquet"),
		"RL_THREADS":      "2",
		"RL_MEMORY_LIMIT": "256MB",
		"RL_DUCKDB_BIN":   self,
		"RL_INIT_FILE":    initFile,
	}
	for k, v := range env {
		vars[k] = v
	}
	cfg, err := configFromEnv(func(k string) string { return vars[k] })
	if err != nil {
		t.Fatalf("configFromEnv: %v", err)
	}
	return &harness{t: t, work: work, cfg: cfg}
}

func (h *harness) mode(m string) {
	if err := os.WriteFile(filepath.Join(h.work, "fake_mode"), []byte(m), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

// exec runs the runner the way main does and returns the parsed metrics, the exit
// code, and the stdout line.
func (h *harness) exec() (metrics, int, string) {
	h.t.Helper()
	m, code := run(context.Background(), h.cfg)
	emit(&h.out, m)
	line := h.out.String()
	if n := strings.Count(line, "\n"); n != 1 {
		h.t.Fatalf("stdout must be exactly one line, got %d: %q", n, line)
	}
	if !strings.HasPrefix(line, "RL_METRICS {") {
		h.t.Fatalf("stdout line has the wrong shape: %q", line)
	}
	var parsed metrics
	if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(line), "RL_METRICS ")), &parsed); err != nil {
		h.t.Fatalf("metrics JSON: %v in %q", err, line)
	}
	return parsed, code, line
}

func (h *harness) script() string {
	b, err := os.ReadFile(filepath.Join(h.work, "fake_script.sql"))
	if err != nil {
		h.t.Fatalf("the engine was never run: %v", err)
	}
	return string(b)
}

func (h *harness) errorLog() string {
	b, _ := os.ReadFile(filepath.Join(h.work, "error.log"))
	return string(b)
}

func TestSuccessfulQuery(t *testing.T) {
	h := newHarness(t, "SELECT count(*) AS n, sum(i) AS s FROM range(10) t(i);\n", nil)
	m, code, line := h.exec()
	if code != 0 || m.Status != "succeeded" || m.ErrorClass != "" {
		t.Fatalf("want succeeded/0, got %+v code %d", m, code)
	}
	if m.RowsOut != 1 {
		t.Errorf("rows_out = %d, want 1", m.RowsOut)
	}
	if m.ResultBytes <= 0 {
		t.Errorf("result_bytes = %d, want > 0", m.ResultBytes)
	}
	if m.WallSeconds <= 0 || m.PeakMemoryBytes <= 0 {
		t.Errorf("wall_seconds %v and peak_memory_bytes %d should both be positive", m.WallSeconds, m.PeakMemoryBytes)
	}
	if m.BytesScanned != 0 {
		t.Errorf("bytes_scanned is documented as 0 in this version, got %d", m.BytesScanned)
	}
	if strings.Contains(line, "error_class") {
		t.Errorf("error_class must be absent on success: %s", line)
	}
	want := `{"status":"succeeded","rows_out":1,"bytes_scanned":0,"cpu_core_seconds":`
	if !strings.HasPrefix(line, "RL_METRICS "+want) {
		t.Errorf("field order or compactness is off: %s", line)
	}

	s := h.script()
	for _, frag := range []string{
		"LOAD httpfs;",
		"SET temp_directory='" + filepath.Join(h.work, "tmp") + "';",
		"SET threads=2;",
		"SET memory_limit='256MB';",
		"SET lock_configuration=true;",
		"SELECT 'RL_INIT_OK';",
		"CREATE TEMP TABLE __rl_result AS\nSELECT count(*) AS n, sum(i) AS s FROM range(10) t(i)\n;",
		"COPY __rl_result TO '" + h.cfg.resultFile + "' (FORMAT PARQUET);",
		"COPY (SELECT count(*) FROM __rl_result) TO '" + filepath.Join(h.work, "rowcount.csv") + "' (FORMAT CSV, HEADER false);",
	} {
		if !strings.Contains(s, frag) {
			t.Errorf("script lacks %q:\n%s", frag, s)
		}
	}
	if idx := strings.Index(s, "SET lock_configuration=true;"); idx < strings.Index(s, "SET memory_limit") || idx > strings.Index(s, "CREATE TEMP TABLE") {
		t.Errorf("the lock must follow the settings and precede the statement:\n%s", s)
	}
	if fi, err := os.Stat(filepath.Join(h.work, "tmp")); err != nil || !fi.IsDir() {
		t.Errorf("work/tmp was not created: %v", err)
	}
	args, _ := os.ReadFile(filepath.Join(h.work, "fake_args"))
	if got := string(args); got != "-batch -bail -no-init -csv -noheader" {
		t.Errorf("engine args = %q", got)
	}
}

func TestSQLErrorNeverReachesStdout(t *testing.T) {
	h := newHarness(t, "SELECT secret_column FROM "+canary, nil)
	h.mode("sql_error")
	m, code, line := h.exec()
	// classCatalog, not classSQL: the fake's message leads with "Catalog Error", and the
	// runner now names the kind DuckDB named instead of reporting every engine error as
	// sql_error. This assertion is ALSO the only coverage that run() actually calls the
	// classifier — the unit tests exercise classifyEngineError directly, so deleting the
	// call site leaves them green and this red.
	if code != 1 || m.Status != "failed" || m.ErrorClass != classCatalog {
		t.Fatalf("want failed/catalog_error/1, got %+v code %d", m, code)
	}
	// THE CLASS IS A TOKEN FROM OUR LIST, NEVER A SLICE OF THE ENGINE'S SENTENCE. The
	// check below would pass a class of "Catalog Error" only by accident of case, so it
	// is worth being explicit: what crosses is a word we chose.
	if strings.Contains(m.ErrorClass, " ") || strings.ToLower(m.ErrorClass) != m.ErrorClass {
		t.Errorf("error_class %q looks like engine prose rather than a token", m.ErrorClass)
	}
	for _, leak := range []string{canary, "secret_column", "Catalog Error", "SELECT"} {
		if strings.Contains(line, leak) {
			t.Errorf("stdout leaks %q: %s", leak, line)
		}
	}
	if !strings.Contains(h.errorLog(), canary) {
		t.Errorf("the engine's stderr should be in error.log, got %q", h.errorLog())
	}
	if m.RowsOut != 0 || m.ResultBytes != 0 {
		t.Errorf("a failed query reports no rows and no result: %+v", m)
	}
}

func TestTimeoutKillsTheEngineAndExits2(t *testing.T) {
	h := newHarness(t, "SELECT 1", map[string]string{"RL_TIMEOUT_SECONDS": "1"})
	h.mode("hang")
	start := time.Now()
	m, code, _ := h.exec()
	if code != 2 || m.ErrorClass != classTimeout || m.Status != "failed" {
		t.Fatalf("want failed/query_timeout/2, got %+v code %d", m, code)
	}
	if took := time.Since(start); took < time.Second || took > 15*time.Second {
		t.Errorf("timeout enforcement took %s", took)
	}
	if !strings.Contains(h.errorLog(), "RL_TIMEOUT_SECONDS") {
		t.Errorf("error.log should record the kill: %q", h.errorLog())
	}
}

func TestOversizedQueryFileIsRefusedBeforeTheEngineRuns(t *testing.T) {
	h := newHarness(t, "SELECT '"+strings.Repeat("x", maxQueryBytes)+"'", nil)
	m, code, _ := h.exec()
	if code != 1 || m.ErrorClass != classInvalidQuery {
		t.Fatalf("want invalid_query_file/1, got %+v code %d", m, code)
	}
	if _, err := os.Stat(filepath.Join(h.work, "fake_script.sql")); err == nil {
		t.Error("the engine ran on an oversized file")
	}
	if !strings.Contains(h.errorLog(), "exceeds") {
		t.Errorf("error.log should say why: %q", h.errorLog())
	}
}

func TestNonQueryStatementRunsAsIsAndReportsNoRows(t *testing.T) {
	stmt := "CREATE TEMP TABLE scratch AS SELECT 1 AS one"
	h := newHarness(t, "  -- a leading comment\n"+stmt+" ;\n", nil)
	m, code, line := h.exec()
	if code != 0 || m.Status != "succeeded" {
		t.Fatalf("want succeeded/0, got %+v code %d", m, code)
	}
	if m.RowsOut != 0 || m.ResultBytes != 0 {
		t.Errorf("non-query statements report rows_out 0 and result_bytes 0: %+v", m)
	}
	if strings.Contains(line, "leaked_row_value") {
		t.Errorf("engine stdout leaked to the pod's stdout: %s", line)
	}
	s := h.script()
	if !strings.Contains(s, "SELECT 'RL_INIT_OK';\n"+stmt+"\n;\n") {
		t.Errorf("statement should run as-is, stripped of the leading comment and trailing terminator:\n%s", s)
	}
	if strings.Contains(s, resultTable) || strings.Contains(s, "COPY") {
		t.Errorf("non-query path must not materialise or copy:\n%s", s)
	}
	if _, err := os.Stat(h.cfg.resultFile); err == nil {
		t.Error("no result file is written for a non-query statement")
	}
}

func TestDescribeFamilyIsWrappedAsASubquery(t *testing.T) {
	for _, stmt := range []string{"DESCRIBE SELECT 1 AS a", "show tables", "SUMMARIZE SELECT 1"} {
		h := newHarness(t, stmt, nil)
		if m, code, _ := h.exec(); code != 0 || m.RowsOut != 1 {
			t.Fatalf("%q: got %+v code %d", stmt, m, code)
		}
		if s := h.script(); !strings.Contains(s, "CREATE TEMP TABLE __rl_result AS SELECT * FROM (\n"+stmt+"\n);") {
			t.Errorf("%q should be wrapped as a subquery:\n%s", stmt, s)
		}
	}
}

func TestQueryKeywordsAreMaterialisedDirectly(t *testing.T) {
	for _, stmt := range []string{"with t as (select 1) select * from t", "FROM range(3)", "VALUES (1),(2)", "PIVOT x ON k USING sum(v)", "UNPIVOT x ON a, b", "(SELECT 1) UNION (SELECT 2)"} {
		h := newHarness(t, stmt, nil)
		if m, code, _ := h.exec(); code != 0 || m.RowsOut != 1 {
			t.Fatalf("%q: got %+v code %d", stmt, m, code)
		}
		if s := h.script(); !strings.Contains(s, "CREATE TEMP TABLE __rl_result AS\n"+stmt+"\n;") {
			t.Errorf("%q should be materialised directly:\n%s", stmt, s)
		}
	}
}

func TestRowCountComesFromTheEngine(t *testing.T) {
	h := newHarness(t, "SELECT * FROM range(1234)", nil)
	if err := os.WriteFile(filepath.Join(h.work, "fake_rows"), []byte("1234"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, _, _ := h.exec(); m.RowsOut != 1234 {
		t.Errorf("rows_out = %d, want 1234", m.RowsOut)
	}
}

func TestOutOfMemoryIsItsOwnClass(t *testing.T) {
	h := newHarness(t, "SELECT * FROM range(1e12)", nil)
	h.mode("oom")
	if m, code, _ := h.exec(); code != 1 || m.ErrorClass != classOOM {
		t.Fatalf("want out_of_memory/1, got %+v code %d", m, code)
	}
}

func TestSIGKILLOutsideTheTimeoutIsOutOfMemory(t *testing.T) {
	h := newHarness(t, "SELECT 1", nil)
	h.mode("sigkill")
	if m, code, _ := h.exec(); code != 1 || m.ErrorClass != classOOM {
		t.Fatalf("want out_of_memory/1, got %+v code %d", m, code)
	}
}

func TestEngineFailureBeforeTheStatementIsARunnerError(t *testing.T) {
	h := newHarness(t, "SELECT 1", nil)
	h.mode("init_error")
	m, code, _ := h.exec()
	if code != 1 || m.ErrorClass != classRunnerIO {
		t.Fatalf("a LOAD failure is the image's fault, not the SQL's: got %+v code %d", m, code)
	}
	if !strings.Contains(h.errorLog(), "before the statement ran") {
		t.Errorf("error.log should explain: %q", h.errorLog())
	}
}

func TestMissingQueryFile(t *testing.T) {
	h := newHarness(t, "SELECT 1", map[string]string{"RL_QUERY_FILE": filepath.Join(t.TempDir(), "absent.sql")})
	if m, code, _ := h.exec(); code != 1 || m.ErrorClass != classInvalidQuery {
		t.Fatalf("want invalid_query_file/1, got %+v code %d", m, code)
	}
}

func TestMissingEngineBinaryIsARunnerError(t *testing.T) {
	h := newHarness(t, "SELECT 1", map[string]string{"RL_DUCKDB_BIN": filepath.Join(t.TempDir(), "no-such-duckdb")})
	if m, code, _ := h.exec(); code != 1 || m.ErrorClass != classRunnerIO {
		t.Fatalf("want runner_io_error/1, got %+v code %d", m, code)
	}
}

func TestInvalidQueryFiles(t *testing.T) {
	cases := map[string]string{
		"empty":                     "",
		"only comments":             "-- nothing\n/* here */\n",
		"two statements":            "SELECT 1; DROP TABLE t",
		"two statements, comment":   "SELECT 1; -- x\n/* y */ SELECT 2",
		"dot command":               ".shell id",
		"dot command after blank":   "\n\n.shell id",
		"dot command after comment": "-- c\n.shell id",
		"hash line":                 "#\n.shell id",
		"nul byte":                  "\x00\n.shell id",
	}
	for name, sql := range cases {
		h := newHarness(t, sql, nil)
		m, code, _ := h.exec()
		if code != 1 || m.ErrorClass != classInvalidQuery {
			t.Errorf("%s: want invalid_query_file/1, got %+v code %d", name, m, code)
		}
		if _, err := os.Stat(filepath.Join(h.work, "fake_script.sql")); err == nil {
			t.Errorf("%s: the engine ran", name)
		}
	}
}

func TestSingleStatementLexing(t *testing.T) {
	cases := []struct {
		in   string
		want string
		err  bool
	}{
		{"SELECT 1", "SELECT 1", false},
		{"SELECT 1;", "SELECT 1", false},
		{"SELECT 1;;  \n-- done\n", "SELECT 1", false},
		{"  \n SELECT 1 \n ", "SELECT 1", false},
		{"SELECT ';' -- ;\n/* ; */ FROM t", "SELECT ';' -- ;\n/* ; */ FROM t", false},
		{"SELECT 'it''s; fine'", "SELECT 'it''s; fine'", false},
		{`SELECT "a;b" FROM t`, `SELECT "a;b" FROM t`, false},
		{`SELECT E'\'; DROP'`, `SELECT E'\'; DROP'`, false},
		{"SELECT $$a;b$$, $tag$c;d$tag$", "SELECT $$a;b$$, $tag$c;d$tag$", false},
		{"SELECT $1; SELECT 2", "", true},
		{"SELECT /* a /* nested; */ b */ 1", "SELECT /* a /* nested; */ b */ 1", false},
		{"SELECT 1; SELECT 2", "", true},
		{"SELECT 1 -- trailing\n", "SELECT 1 -- trailing", false},
		{"", "", true},
		{"/* only */", "", true},
	}
	for _, c := range cases {
		got, err := singleStatement(c.in)
		if (err != nil) != c.err {
			t.Errorf("%q: err = %v, want error %v", c.in, err, c.err)
			continue
		}
		if got != c.want {
			t.Errorf("%q: got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestChildEnvironmentIsMinimal(t *testing.T) {
	t.Setenv("AWS_SECRET_ACCESS_KEY", "must-not-cross")
	t.Setenv("RL_THREADS", "8")
	h := newHarness(t, "SELECT 1", nil)
	h.exec()
	env, err := os.ReadFile(filepath.Join(h.work, "fake_env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(env), "\n") {
		if strings.HasPrefix(line, "AWS_") || strings.HasPrefix(line, "RL_") {
			t.Errorf("engine inherited %q", line)
		}
	}
	if !strings.Contains(string(env), "HOME="+h.work) {
		t.Errorf("engine should get HOME=work dir, got:\n%s", env)
	}
}

func TestConfigValidation(t *testing.T) {
	bad := []map[string]string{
		{"RL_THREADS": "0"},
		{"RL_THREADS": "4; DROP"},
		{"RL_MEMORY_LIMIT": "512MB'; SET x"},
		{"RL_TIMEOUT_SECONDS": "-1"},
		{"RL_TIMEOUT_SECONDS": "ten"},
	}
	for _, env := range bad {
		if _, err := configFromEnv(func(k string) string { return env[k] }); err == nil {
			t.Errorf("%v should be rejected", env)
		}
	}
	good := map[string]string{"RL_MEMORY_LIMIT": "1.5 GiB", "RL_THREADS": "16", "RL_TIMEOUT_SECONDS": "0"}
	cfg, err := configFromEnv(func(k string) string { return good[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.timeout != 0 || cfg.memoryLimit != "1.5 GiB" || cfg.threads != "16" {
		t.Errorf("config = %+v", cfg)
	}
	if cfg.queryFile != "/run/rl/query.sql" || cfg.workDir != "/work" || cfg.resultFile != "/work/result.parquet" {
		t.Errorf("defaults = %+v", cfg)
	}
}
