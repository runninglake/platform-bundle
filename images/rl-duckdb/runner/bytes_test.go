package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// bytes_scanned is the sum of the bytes of the engine's own file-system READs for the statement, and it is OFF unless the pod says RL_MEASURE_BYTES=1 (bytesReadSQL
// says where the figure comes from, what it is not, and why it is not on for every query). These tests hold three things: that off is exactly the script the runner
// always wrote; that on reports the figure and never turns one it cannot read into a failed query; and, on the real engine, that on does not change what a query
// returns, the kind of its failure, or what survives a hostile file name.

func measuring() map[string]string { return map[string]string{"RL_MEASURE_BYTES": "1"} }

func TestMeasuringIsOffByDefaultAndTheScriptIsUnchanged(t *testing.T) {
	h := newHarness(t, "SELECT 1 AS one", nil)
	h.bytes("4096")
	m, code, _ := h.exec()
	if code != 0 || m.Status != "succeeded" || m.BytesScanned != 0 {
		t.Fatalf("off: want succeeded with bytes_scanned 0, got %+v code %d", m, code)
	}
	s := h.script()
	for _, frag := range []string{"enable_logging", "truncate_duckdb_logs", "duckdb_logs", bytesReadFile} {
		if strings.Contains(s, frag) {
			t.Errorf("off: the script must not mention %q:\n%s", frag, s)
		}
	}
	if strings.Contains(h.errorLog(), "bytes read") {
		t.Errorf("off: nothing is read back, so nothing is noted: %q", h.errorLog())
	}
}

func TestABytesFigureIsReportedAndAnUnreadableOneIsNotAFailure(t *testing.T) {
	for _, c := range []struct {
		name, file string
		want       int64
		note       bool
	}{
		{"a number", "123456789012", 123456789012, false},
		{"zero reads", "0", 0, false},
		{"no file", "absent", 0, true},
		{"a line that is not a number", "garbage", 0, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, "SELECT 1 AS one", measuring())
			h.bytes(c.file)
			m, code, _ := h.exec()
			if code != 0 || m.Status != "succeeded" || m.ErrorClass != "" {
				t.Fatalf("a bytes figure that cannot be read must not fail the query: %+v code %d", m, code)
			}
			if m.BytesScanned != c.want {
				t.Errorf("bytes_scanned = %d, want %d", m.BytesScanned, c.want)
			}
			if got := strings.Contains(h.errorLog(), "bytes read:"); got != c.note {
				t.Errorf("error.log mentions the unreadable figure = %v, want %v: %q", got, c.note, h.errorLog())
			}
		})
	}
}

// THE REGRESSION THE REVIEW FOUND: with logging on, the CLI's coloured WARNING put an escape sequence on the front of the init sentinel, the runner read "the engine
// failed before the statement ran", and EVERY failed query was reported runner_io_error in place of the engine's own kind. The fake now prints that warning.
func TestAFailedStatementKeepsItsKindWhileMeasuring(t *testing.T) {
	h := newHarness(t, "SELECT secret_column FROM "+canary, measuring())
	h.mode("sql_error")
	h.bytes("999")
	m, code, _ := h.exec()
	if code != 1 || m.Status != "failed" || m.ErrorClass != classCatalog {
		t.Fatalf("want failed/catalog_error/1, got %+v code %d", m, code)
	}
	if m.BytesScanned != 0 {
		t.Errorf("a statement that failed read no figure: %d", m.BytesScanned)
	}
}

func TestASentinelWithAColourCodeOnItsFrontIsStillTheSentinel(t *testing.T) {
	out := filepath.Join(t.TempDir(), "duckdb.out")
	write := func(s string) {
		if err := os.WriteFile(out, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("\x1b[90mWARNING:\nThe logging settings have been changed.\n\x1b[00m" + initSentinel + "\n")
	if !sentinelRan(out, initSentinel) {
		t.Error("the sentinel behind the CLI's colour code was not read")
	}
	write("\x1b[00mx" + initSentinel + "\n" + initSentinel + "x\n")
	if sentinelRan(out, initSentinel) {
		t.Error("a line that merely ends or starts with the sentinel is not it")
	}
}

// EVERY KIND OF STATEMENT carries the same three steps in the same order: logging on before the lock, the log emptied after the init sentinel, the sum written after
// the statement and before the result is copied, and the probe's or the task's tail after that. A kind that skipped one would report 0 and look like a query that read
// nothing.
func TestEveryKindOfStatementCarriesTheMeasurement(t *testing.T) {
	for name, sql := range map[string]string{
		"a query":     "SELECT 1 AS one",
		"a describe":  "DESCRIBE SELECT 1 AS a",
		"a statement": "CREATE TEMP TABLE scratch AS SELECT 1 AS one",
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, sql, measuring())
			if _, code, _ := h.exec(); code != 0 {
				t.Fatalf("code %d", code)
			}
			s := h.script()
			bytesCopy := bytesReadSQL(h.work, filepath.Join(h.work, bytesReadFile))
			if strings.Count(s, enableReadLogSQL) != 1 || strings.Count(s, truncateReadLogSQL) != 1 || strings.Count(s, bytesCopy) != 1 {
				t.Fatalf("the script must hold each step once:\n%s", s)
			}
			on, lock, trunc, sum := strings.Index(s, enableReadLogSQL), strings.Index(s, "SET lock_configuration=true;"), strings.Index(s, truncateReadLogSQL), strings.Index(s, bytesCopy)
			if !(on < lock && lock < trunc && trunc < sum) {
				t.Errorf("logging on (%d) < lock (%d) < log emptied (%d) < sum written (%d) is the order:\n%s", on, lock, trunc, sum, s)
			}
		})
	}
}

func TestTheProbeAndTheTaskTailsComeAfterTheSum(t *testing.T) {
	cfg := config{workDir: "/work", measureBytes: true, probe: &probeSpec{NameKeyHex: probeKeyHex, Inputs: []string{"inp_aaaaaaa1"}}}
	s := composeScript("", "", cfg, "/work/tmp", "/work/rowcount.csv", "/work/bytes_read.csv", "SELECT 1", kindQuery)
	if i, j := strings.Index(s, "FROM system.main.duckdb_logs"), strings.Index(s, "rl_probe"); i < 0 || j < 0 || i > j {
		t.Errorf("a probe's tail must come after the sum (sum at %d, probe at %d):\n%s", i, j, s)
	}
	cfg = config{workDir: "/work", measureBytes: true, task: &taskSpec{}, taskTail: "-- TASK TAIL\n"}
	s = composeScript("", "", cfg, "/work/tmp", "/work/rowcount.csv", "/work/bytes_read.csv", "SELECT 1", kindQuery)
	if i, j := strings.Index(s, "FROM system.main.duckdb_logs"), strings.Index(s, "-- TASK TAIL"); i < 0 || j < 0 || i > j {
		t.Errorf("a task's write must come after the sum, so a write is never in it (sum at %d, tail at %d):\n%s", i, j, s)
	}
}

// The path the sum leaves out is the runner's own work directory, with its trailing slash, so a data file in a sibling directory whose name only STARTS like it is counted.
func TestTheSumLeavesOutTheWorkDirectoryAndNotItsNeighbours(t *testing.T) {
	sql := bytesReadSQL("/work", "/work/bytes_read.csv")
	if !strings.Contains(sql, `NOT starts_with(regexp_extract(message, '"path":"(.*)","op":"', 1), '/work/')`) {
		t.Errorf("the exclusion is the work directory and its slash: %s", sql)
	}
	if !strings.Contains(sql, "FROM system.main.duckdb_logs") {
		t.Errorf("the view is named in full, so a temp view of the same name cannot stand in for it: %s", sql)
	}
	if strings.Contains(sql, "json_extract") {
		t.Errorf("a path is written into the message unescaped, so the message is not JSON: %s", sql)
	}
	if got := bytesReadSQL("/work/", "/work/bytes_read.csv"); got != sql {
		t.Errorf("a trailing slash on the work directory must not change the statement:\n%s\n%s", got, sql)
	}
	if !strings.Contains(bytesReadSQL("/w'ork", "/w'ork/b.csv"), "'/w''ork/'") {
		t.Errorf("a quote in the path must be doubled")
	}
}

// ---- the real engine ------------------------------------------------------------------------------------------------------------------------------------------
// Everything below runs the runner against the real pinned CLI (duckdbBinary: on PATH, or skipped on a laptop and a failure in CI), because the fake cannot say what the
// engine's log holds or what its warning does to a line.

func realRun(t *testing.T, bin, sql string, env map[string]string) (metrics, int, string, *harness) {
	t.Helper()
	initFile := filepath.Join(t.TempDir(), "init.sql")
	if err := os.WriteFile(initFile, []byte("SET threads=2;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"RL_DUCKDB_BIN": bin, "RL_INIT_FILE": initFile}
	for k, v := range env {
		vars[k] = v
	}
	h := newHarness(t, sql, vars)
	m, code, line := h.exec()
	return m, code, line, h
}

func duckSQL(t *testing.T, bin, sql string) string {
	t.Helper()
	home := t.TempDir()
	cmd := exec.Command(bin, "-batch", "-bail", "-no-init", "-csv", "-noheader", "-cmd", "SET threads=2;", "-c", sql)
	cmd.Env = []string{"HOME=" + home}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", sql, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestTheRealEngineCountsTheCompressedBytesItRead(t *testing.T) {
	bin := duckdbBinary(t)
	data := t.TempDir()
	file := filepath.Join(data, "wide.parquet")
	duckSQL(t, bin, "COPY (SELECT i AS id, i % 97 AS k, 'x' || (i % 1000) AS s, random() AS r FROM range(2000000) t(i)) TO '"+file+"' (FORMAT parquet);")
	read, err := strconv.ParseInt(duckSQL(t, bin, "SELECT sum(total_compressed_size) FROM parquet_metadata('"+file+"') WHERE path_in_schema IN ('k', 'r');"), 10, 64)
	if err != nil || read <= 0 {
		t.Fatalf("compressed size of the columns read: %v", err)
	}
	whole, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	query := "SELECT k, count(*) AS n, sum(r) AS s FROM '" + file + "' GROUP BY k ORDER BY k"

	m, code, line, h := realRun(t, bin, query, measuring())
	if code != 0 || m.Status != "succeeded" {
		t.Fatalf("the real engine failed: %s\n%s", line, h.errorLog())
	}
	if m.BytesScanned < read || m.BytesScanned > read+read/100+(256<<10) || m.BytesScanned >= whole.Size() {
		t.Errorf("bytes_scanned = %d; the two columns read are %d compressed, the file %d: want at least the first, a little over it, and under the file", m.BytesScanned, read, whole.Size())
	}
	if m.RowsOut != 97 {
		t.Errorf("measuring must not change what the query returns: rows_out = %d, want 97", m.RowsOut)
	}

	off, _, offLine, _ := realRun(t, bin, query, nil)
	if off.Status != "succeeded" || off.BytesScanned != 0 || off.RowsOut != 97 {
		t.Errorf("off, the same query reports 0 bytes and the same rows: %s", offLine)
	}
	if zm, zcode, zline, z := realRun(t, bin, "SELECT 1 AS one", measuring()); zcode != 0 || zm.BytesScanned != 0 {
		t.Errorf("a query that reads no file reports zero bytes: %s\n%s", zline, z.errorLog())
	}
	// A WRITE IS NOT A READ: a statement that writes a file of its own outside the work directory read nothing, whatever the engine's log says it wrote.
	if wm, wcode, wline, w := realRun(t, bin, "COPY (SELECT i, random() AS r FROM range(300000) t(i)) TO '"+filepath.Join(data, "out.parquet")+"' (FORMAT parquet)", measuring()); wcode != 0 || wm.BytesScanned != 0 {
		t.Errorf("a statement that only writes a file reports zero bytes read: %s\n%s", wline, w.errorLog())
	}
}

// THE REGRESSION THE REVIEW FOUND, on the real engine: every one of these became runner_io_error while measuring.
func TestTheRealEngineKeepsItsErrorKindsWhileMeasuring(t *testing.T) {
	bin := duckdbBinary(t)
	for _, c := range []struct{ name, sql, class string }{
		{"a missing table", "SELECT * FROM a_table_nobody_made", classCatalog},
		{"a syntax error", "SELEC 1", classSyntax},
		{"an unknown function", "SELECT no_such_function(1)", classCatalog},
	} {
		for _, on := range []bool{false, true} {
			env := map[string]string(nil)
			if on {
				env = measuring()
			}
			m, code, line, h := realRun(t, bin, c.sql, env)
			if code != 1 || m.Status != "failed" || m.ErrorClass != c.class {
				t.Errorf("%s, measuring=%v: want failed/%s/1, got %s\n%s", c.name, on, c.class, line, h.errorLog())
			}
		}
	}
}

// THE SECOND REGRESSION: the log writes a path into its message unescaped, so a name with a quote, a backslash or a tab is not JSON, and a sum that parsed it failed a query
// that had succeeded.
func TestAFileNameThatIsNotJSONDoesNotFailTheQuery(t *testing.T) {
	bin := duckdbBinary(t)
	data := t.TempDir()
	var sources []string
	for _, name := range []string{`q"uote.parquet`, `back\slash.parquet`, "tab\tname.parquet", "it's.parquet", `x","op":"READ","bytes":"1.parquet`} {
		f := filepath.Join(data, name)
		duckSQL(t, bin, "COPY (SELECT i, random() AS r FROM range(50000) t(i)) TO '"+strings.ReplaceAll(f, "'", "''")+"' (FORMAT parquet);")
		sources = append(sources, "SELECT count(*) AS n FROM read_parquet('"+strings.ReplaceAll(f, "'", "''")+"')")
	}
	m, code, line, h := realRun(t, bin, strings.Join(sources, " UNION ALL "), measuring())
	if code != 0 || m.Status != "succeeded" || m.BytesScanned <= 0 {
		t.Fatalf("a hostile file name must not fail the query or hide its bytes: %s\n%s", line, h.errorLog())
	}
}

// A statement can reconfigure the log (enable_logging is a table function the configuration lock does not forbid) and can shadow the view the sum reads by name. The sum
// names the system view in full, so a temp view of the same name does not stand in for it.
func TestATempViewNamedLikeTheLogDoesNotStandInForIt(t *testing.T) {
	bin := duckdbBinary(t)
	m, code, line, h := realRun(t, bin, `CREATE TEMP VIEW duckdb_logs AS SELECT 'FileSystem' AS type, '{"op":"READ","bytes":"123456789012","path":"/x"}' AS message`, measuring())
	if code != 0 || m.Status != "succeeded" || m.BytesScanned != 0 {
		t.Errorf("a temp view must not set the figure: %s\n%s", line, h.errorLog())
	}
}

// The work directory is the runner's own: a file read from inside it is not the statement's data and is not counted, and a sibling directory that only starts with the same
// letters is.
func TestTheRealEngineLeavesOutTheWorkDirectoryAndNotItsNeighbour(t *testing.T) {
	bin := duckdbBinary(t)
	probe := newHarness(t, "SELECT 1", nil)
	inside := filepath.Join(probe.work, "inside.parquet")
	sibling := probe.work + "-sibling"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sibling) })
	outside := filepath.Join(sibling, "outside.parquet")
	for _, f := range []string{inside, outside} {
		duckSQL(t, bin, "COPY (SELECT i, random() AS r FROM range(200000) t(i)) TO '"+f+"' (FORMAT parquet);")
	}
	initFile := filepath.Join(t.TempDir(), "init.sql")
	if err := os.WriteFile(initFile, []byte("SET threads=2;\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	measure := func(f string) int64 {
		vars := map[string]string{"RL_DUCKDB_BIN": bin, "RL_INIT_FILE": initFile, "RL_MEASURE_BYTES": "1", "RL_WORK_DIR": probe.work, "RL_QUERY_FILE": filepath.Join(probe.work, "q.sql"),
			"RL_RESULT_FILE": filepath.Join(probe.work, "result.parquet")}
		if err := os.WriteFile(vars["RL_QUERY_FILE"], []byte("SELECT sum(r) AS s FROM '"+f+"'"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := configFromEnv(func(k string) string { return vars[k] })
		if err != nil {
			t.Fatal(err)
		}
		m, code := run(context.Background(), cfg)
		if code != 0 || m.Status != "succeeded" {
			t.Fatalf("run of %s: %+v\n%s", f, m, probe.errorLog())
		}
		return m.BytesScanned
	}
	if n := measure(inside); n != 0 {
		t.Errorf("a read inside the work directory is the runner's own and is not counted: %d", n)
	}
	if n := measure(outside); n <= 0 {
		t.Errorf("a read in a sibling directory that only starts with the work directory's name is counted: %d", n)
	}
}
