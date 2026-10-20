package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// A PROBE WRITES NOTHING AND REPORTS NUMBERS (probe.go). These tests run the generated SQL on the REAL engine, as hugeint_test.go does and for its reason: the
// rest of this package drives a fake duckdb that answers whatever it is asked, and a fake cannot have a quoting or a type bug.

const probeKeyHex = "000102030405060708090a0b0c0d0e0f"

func probeName(name string) string {
	key, _ := hex.DecodeString(probeKeyHex)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(strings.ToLower(name)))
	return hex.EncodeToString(mac.Sum(nil))
}

// runProbe runs a whole probe the way main does, on the real engine: a prelude that samples two inputs as TEMP TABLEs, the statement, the probe.
func runProbe(t *testing.T, stmt, prelude string, inputs []string) (metrics, int, string, string) {
	t.Helper()
	return runProbeWith(t, stmt, prelude, inputs, probeKeyHex, "256MB")
}

func runProbeWith(t *testing.T, stmt, prelude string, inputs []string, keyHex, memory string) (metrics, int, string, string) {
	t.Helper()
	bin := duckdbBinary(t)
	work := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(work, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	spec, _ := json.Marshal(map[string]any{"name_key_hex": keyHex, "inputs": inputs})
	vars := map[string]string{
		"RL_QUERY_FILE":   write("query.sql", stmt),
		"RL_WORK_DIR":     work,
		"RL_DUCKDB_BIN":   bin,
		"RL_INIT_FILE":    write("init.sql", ""),
		"RL_PRELUDE_FILE": write("prelude.sql", prelude),
		"RL_PROBE_FILE":   write("probe.json", string(spec)),
		"RL_THREADS":      "2",
		"RL_MEMORY_LIMIT": memory,
	}
	cfg, err := configFromEnv(func(k string) string { return vars[k] })
	if err != nil {
		t.Fatal(err)
	}
	m, code := run(context.Background(), cfg)
	var out bytes.Buffer
	emit(&out, m)
	errLog, _ := os.ReadFile(filepath.Join(work, "error.log"))
	return m, code, out.String(), work + "\n" + string(errLog)
}

const probePrelude = `CREATE TEMP TABLE inp_aaaaaaa1 AS SELECT range AS id, 'x' AS tag FROM range(10);
CREATE TEMP TABLE inp_bbbbbbb2 AS SELECT range AS id FROM range(3);
`

func parseProbeLines(t *testing.T, out string) (probeFacts, metrics) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "RL_PROBE {") || !strings.HasPrefix(lines[1], "RL_METRICS {") {
		t.Fatalf("a probe prints its facts line and then its metrics line, got %q", out)
	}
	var f probeFacts
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[0], "RL_PROBE ")), &f); err != nil {
		t.Fatalf("facts JSON: %v in %q", err, lines[0])
	}
	var m metrics
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "RL_METRICS ")), &m); err != nil {
		t.Fatalf("metrics JSON: %v in %q", err, lines[1])
	}
	return f, m
}

// THE FACTS, ON THE REAL ENGINE: rows, nulls per column in output order, a hash for each name and never the name, a closed type word, each input's rows,
// and no result file in the work directory. Mutants: swap the null count's column order, hash the name unkeyed, drop the lower-casing, report the type name.
func TestAProbeReportsCountsHashesAndTypesAndWritesNoResult(t *testing.T) {
	stmt := `SELECT a.id AS "Id", CASE WHEN a.id % 2 = 0 THEN NULL ELSE a.tag END AS tag, CAST(a.id AS DECIMAL(10,2)) AS amt, 1.5::DOUBLE AS d, now()::TIMESTAMPTZ AS ts, [1,2] AS nested
FROM inp_aaaaaaa1 a CROSS JOIN inp_bbbbbbb2 b`
	m, code, out, work := runProbe(t, stmt, probePrelude, []string{"inp_aaaaaaa1", "inp_bbbbbbb2"})
	if code != 0 || m.Status != "succeeded" {
		t.Fatalf("the probe failed: %+v code %d\n%s", m, code, work)
	}
	f, metrics := parseProbeLines(t, out)
	if metrics.RowsOut != 30 || metrics.ResultUploaded || metrics.ResultBytes != 0 {
		t.Errorf("metrics = %+v, want 30 rows out and nothing uploaded", metrics)
	}
	if len(f.Inputs) != 2 || f.Inputs[0] != (probeInput{"inp_aaaaaaa1", 10}) || f.Inputs[1] != (probeInput{"inp_bbbbbbb2", 3}) {
		t.Errorf("inputs = %+v", f.Inputs)
	}
	want := []probeColumn{
		{probeName("id"), "long", 0}, // a column named "Id" is hashed lower-cased
		{probeName("tag"), "string", 15},
		{probeName("amt"), "decimal", 0},
		{probeName("d"), "double", 0},
		{probeName("ts"), "timestamptz", 0},
		{probeName("nested"), "other", 0},
	}
	if len(f.Columns) != len(want) {
		t.Fatalf("columns = %+v", f.Columns)
	}
	for i := range want {
		if f.Columns[i] != want[i] {
			t.Errorf("column %d = %+v, want %+v", i, f.Columns[i], want[i])
		}
	}
	if f.DigestAlgorithm != "bag_digest_v1" || !reHex64.MatchString(f.Digest) {
		t.Errorf("digest = %q (%q)", f.Digest, f.DigestAlgorithm)
	}
	// NOTHING LEFT BEHIND TO UPLOAD OR DOWNLOAD, and no name or value on stdout.
	workDir := strings.SplitN(work, "\n", 2)[0]
	if _, err := os.Stat(filepath.Join(workDir, "result.parquet")); err == nil {
		t.Error("a probe wrote a result file")
	}
	for _, leak := range []string{"Id", "tag", "amt", "nested"} {
		if strings.Contains(out, `"`+leak+`"`) {
			t.Errorf("stdout carries the column name %q: %s", leak, out)
		}
	}
}

// THE DIGEST IS OF A BAG: the same rows in another order digest alike, a different bag does not, and a null is not the text it could be mistaken for.
// Mutants: sort nothing (the first fails), drop the length prefix (the third fails), treat a null as an empty string (the fourth fails).
func TestTheDigestIsOfTheBagAndCannotBeImitated(t *testing.T) {
	digest := func(stmt string) string {
		t.Helper()
		m, code, out, work := runProbe(t, stmt, "", nil)
		if code != 0 || m.Status != "succeeded" {
			t.Fatalf("the probe failed: %+v\n%s", m, work)
		}
		f, _ := parseProbeLines(t, out)
		return f.Digest
	}
	asc := digest(`SELECT * FROM (VALUES (1,'a'),(2,'b'),(3,'c')) t(x,y) ORDER BY x`)
	desc := digest(`SELECT * FROM (VALUES (1,'a'),(2,'b'),(3,'c')) t(x,y) ORDER BY x DESC`)
	if asc != desc {
		t.Errorf("permuting the rows changed the digest: %s vs %s", asc, desc)
	}
	if other := digest(`SELECT * FROM (VALUES (1,'a'),(2,'b'),(3,'d')) t(x,y)`); other == asc {
		t.Error("a different bag has the same digest")
	}
	// A duplicate row is a different bag (a bag, not a set).
	once := digest(`SELECT * FROM (VALUES (1,'a'),(2,'b')) t(x,y)`)
	twice := digest(`SELECT * FROM (VALUES (1,'a'),(2,'b'),(2,'b')) t(x,y)`)
	if once == twice {
		t.Error("a duplicated row did not change the digest")
	}
	// Two cells that run together the same way are not the same row: ('ab','c') and ('a','bc').
	if digest(`SELECT 'ab' AS x, 'c' AS y`) == digest(`SELECT 'a' AS x, 'bc' AS y`) {
		t.Error("cells that split differently digest alike: the encoding is not length-prefixed")
	}
	// A null is not an empty string, and is not the text a naive encoding would give it.
	if digest(`SELECT NULL::VARCHAR AS x`) == digest(`SELECT '' AS x`) || digest(`SELECT NULL::VARCHAR AS x`) == digest(`SELECT 'N' AS x`) || digest(`SELECT NULL::VARCHAR AS x`) == digest(`SELECT 'NULL' AS x`) {
		t.Error("a null digests like a value")
	}
	// Cells cannot imitate each other across a column boundary: ('V','') is not ('','V'), and (NULL,'x') is not ('x',NULL).
	if digest(`SELECT 'V' AS x, '' AS y`) == digest(`SELECT '' AS x, 'V' AS y`) {
		t.Error("two rows whose cells shift across a column digest alike: the cells are not length-prefixed")
	}
	if digest(`SELECT NULL::VARCHAR AS x, 'x' AS y`) == digest(`SELECT 'x' AS x, NULL::VARCHAR AS y`) {
		t.Error("a null that moves to another column digests alike: a null is encoded as nothing")
	}
	// An empty result is a result.
	if e := digest(`SELECT 1 AS x WHERE false`); !reHex64.MatchString(e) {
		t.Errorf("an empty result has digest %q", e)
	}
}

// A COLUMN NAME CANNOT REACH THE GENERATED STATEMENTS: every identifier is quoted with the quote doubled, so a name with a quote, a semicolon, a newline or
// a dot-command stays a name. And a column's name is hashed from what it is, newline and all.
func TestAHostileColumnNameStaysAName(t *testing.T) {
	hostile := []string{`odd "name`, "semi;colon", "line\nbreak", ".read /etc/passwd", `x" ; DROP TABLE t; --`}
	var cols []string
	for i, h := range hostile {
		cols = append(cols, `'v`+string(rune('0'+i))+`' AS "`+strings.ReplaceAll(h, `"`, `""`)+`"`)
	}
	m, code, out, work := runProbe(t, `SELECT `+strings.Join(cols, ", "), "", nil)
	if code != 0 || m.Status != "succeeded" {
		t.Fatalf("a hostile column name broke the probe: %+v\n%s", m, work)
	}
	f, _ := parseProbeLines(t, out)
	if len(f.Columns) != len(hostile) {
		t.Fatalf("columns = %+v", f.Columns)
	}
	for i, h := range hostile {
		if f.Columns[i].NameHash != probeName(h) {
			t.Errorf("column %d (%q) hash = %s", i, h, f.Columns[i].NameHash)
		}
	}
}

// A PROBE NEVER UPLOADS: even when the environment hands the runner an upload URL, the probe's run makes no request. The agent mints none, and this is the
// second fence. Mutant: leave the upload after the probe's return.
func TestAProbeNeverUploadsEvenWhenGivenAURL(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	bin := duckdbBinary(t)
	work := t.TempDir()
	// A result file already in place, so that an upload attempted after the probe would have something to send and would make its request.
	files := map[string]string{"query.sql": "SELECT 1 AS x", "init.sql": "", "probe.json": `{"name_key_hex":"` + probeKeyHex + `","inputs":[]}`, "result.parquet": "PAR1"}
	for n, b := range files {
		if err := os.WriteFile(filepath.Join(work, n), []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	vars := map[string]string{
		"RL_QUERY_FILE": filepath.Join(work, "query.sql"), "RL_WORK_DIR": work, "RL_DUCKDB_BIN": bin, "RL_INIT_FILE": filepath.Join(work, "init.sql"),
		"RL_PROBE_FILE": filepath.Join(work, "probe.json"), "RL_RESULT_UPLOAD_URL": srv.URL + "/put", "RL_RESULT_UPLOAD_HEADERS": `{"X-A":"b"}`,
	}
	cfg, err := configFromEnv(func(k string) string { return vars[k] })
	if err != nil {
		t.Fatal(err)
	}
	if m, code := run(context.Background(), cfg); code != 0 || m.ResultUploaded {
		t.Fatalf("the probe failed or claims an upload: %+v code %d", m, code)
	}
	if hits.Load() != 0 {
		t.Errorf("a probe made %d upload requests", hits.Load())
	}
}

// A SPEC THE RUNNER CANNOT HOLD, OR A STATEMENT WITH NO ROWS, IS REFUSED BEFORE THE ENGINE RUNS: the invalid-query class, nothing materialised, and the
// statement is not run as a plain query. Mutants: ignore the spec's fault, accept a DDL statement in a probe.
func TestAProbeThatCannotBeUnderstoodIsRefusedBeforeTheEngineRuns(t *testing.T) {
	good := `{"name_key_hex":"` + probeKeyHex + `","inputs":["inp_aaaaaaa1"]}`
	cases := map[string]struct{ spec, sql string }{
		"not JSON":        {`nope`, "SELECT 1;\n"},
		"a short key":     {`{"name_key_hex":"0011","inputs":[]}`, "SELECT 1;\n"},
		"a key not hex":   {`{"name_key_hex":"` + strings.Repeat("zz", 16) + `","inputs":[]}`, "SELECT 1;\n"},
		"a table name":    {`{"name_key_hex":"` + probeKeyHex + `","inputs":["analytics.public.customers"]}`, "SELECT 1;\n"},
		"an alias twice":  {`{"name_key_hex":"` + probeKeyHex + `","inputs":["inp_aaaaaaa1","inp_aaaaaaa1"]}`, "SELECT 1;\n"},
		"too many inputs": {`{"name_key_hex":"` + probeKeyHex + `","inputs":[` + strings.TrimSuffix(strings.Repeat(`"inp_aaaaaaa1",`, 17), ",") + `]}`, "SELECT 1;\n"},
		"a DDL statement": {good, "CREATE TABLE t (x INT);\n"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, c.sql, nil)
			spec := filepath.Join(h.work, "probe.json")
			if err := os.WriteFile(spec, []byte(c.spec), 0o644); err != nil {
				t.Fatal(err)
			}
			h.cfg.probeFile = spec
			m, code, _ := h.exec()
			if code != 1 || m.ErrorClass != classInvalidQuery {
				t.Fatalf("want invalid_query_file/1, got %+v code %d", m, code)
			}
			if _, err := os.Stat(filepath.Join(h.work, "fake_script.sql")); err == nil {
				t.Error("the engine ran for a probe that could not be understood")
			}
		})
	}
}

// A PLAIN QUERY IS UNTOUCHED: no RL_PROBE_FILE, no probe line, one line of stdout, the result copy in the script. Mutant: print the facts line for any run.
func TestAPlainQueryHasNoProbeLineAndKeepsItsCopy(t *testing.T) {
	h := newHarness(t, "SELECT 1;\n", nil)
	m, code, line := h.exec()
	if code != 0 || m.ErrorClass != "" || strings.Contains(line, "RL_PROBE") {
		t.Fatalf("a plain query: %+v code %d line %q", m, code, line)
	}
	if s := h.script(); !strings.Contains(s, "FORMAT PARQUET") || strings.Contains(s, "probe_cols") {
		t.Errorf("a plain query's script lost its result copy or gained the probe's:\n%s", s)
	}
}

func TestTheTypeFamiliesAreTheDialectServicesWordsAndOtherForTheRest(t *testing.T) {
	for in, want := range map[string]string{
		"BOOLEAN": "boolean", "TINYINT": "integer", "SMALLINT": "integer", "INTEGER": "integer", "BIGINT": "long", "FLOAT": "float", "DOUBLE": "double",
		"DECIMAL(18,3)": "decimal", "VARCHAR": "string", "DATE": "date", "TIME": "time", "TIMESTAMP": "timestamp", "TIMESTAMP_NS": "timestamp",
		"TIMESTAMP WITH TIME ZONE": "timestamptz", "BLOB": "binary", "UUID": "uuid",
		"HUGEINT": "other", "UBIGINT": "other", "INTEGER[]": "other", "STRUCT(a INTEGER)": "other", "INTERVAL": "other", "": "other",
	} {
		if got := typeFamily(in); got != want {
			t.Errorf("typeFamily(%q) = %q, want %q", in, got, want)
		}
	}
}

// THE ENGINE'S FILES ARE CHECKED BEFORE THEY ARE BELIEVED: a null count above the row count, a negative count, a digest that is not 64 hex digits, and
// a counts row that does not fit the columns are the runner's own failure (runner_io_error), never facts. Mutants: drop the nulls relation, the digest shape.
func TestTheEnginesFilesAreCheckedBeforeTheyAreBelieved(t *testing.T) {
	spec := &probeSpec{key: []byte("0123456789abcdef"), Inputs: []string{"inp_aaaaaaa1"}}
	write := func(dir, name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files := func(cols, counts, digest, in string) string {
		dir := t.TempDir()
		write(dir, probeColsFile, cols)
		write(dir, probeCountsFile, counts)
		write(dir, probeDigestFile, digest)
		write(dir, "probe_in_0.csv", in)
		return dir
	}
	const cols = "0,6964,BIGINT\n1,746167,VARCHAR\n" // hex of "id" and "tag"
	good := strings.Repeat("ab", 32) + "\n"
	if _, n, err := readProbe(files(cols, "5,0,5\n", good, "7\n"), spec); err != nil || n != 5 {
		t.Fatalf("well-formed files were refused: %v (%d)", err, n)
	}
	for name, dir := range map[string]string{
		"more nulls than rows":     files(cols, "5,0,6\n", good, "7\n"),
		"a negative null count":    files(cols, "5,-1,0\n", good, "7\n"),
		"a negative row count":     files(cols, "-1,0,0\n", good, "7\n"),
		"counts for a column less": files(cols, "5,0\n", good, "7\n"),
		"counts for a column more": files(cols, "5,0,0,0\n", good, "7\n"),
		"no columns":               files("", "5\n", good, "7\n"),
		"a digest not hex":         files(cols, "5,0,0\n", strings.Repeat("zz", 32)+"\n", "7\n"),
		"a short digest":           files(cols, "5,0,0\n", strings.Repeat("a", 63)+"\n", "7\n"),
		"an upper-case digest":     files(cols, "5,0,0\n", strings.Repeat("AB", 32)+"\n", "7\n"),
		"a column that is two":     files("0,6964\n", "5,0\n", good, "7\n"),
		"a name that is not hex":   files("0,id,BIGINT\n", "5,0\n", good, "7\n"),
		"an input that is not one": files(cols, "5,0,0\n", good, "x\n"),
		"a negative input":         files(cols, "5,0,0\n", good, "-3\n"),
	} {
		if _, _, err := readProbe(dir, spec); err == nil {
			t.Errorf("%s was believed", name)
		}
	}
}

// A DECIMAL INSIDE A LIST IS NOT A DECIMAL, a CRLF in a column name reaches the hash intact, and a wide result and a long one still probe (the plain query
// handles both): the first generation of this mode chained || per column (the engine refuses a thousand levels of expression and the author was told their
// SQL had a syntax error), summed a FILTER aggregate per column (6 GB at 4,096 columns) and held every row hash at once (64 bytes a row, not spillable).
// Mutants: the old prefix test for DECIMAL; a || chain; hold every hash.
func TestAWideOrLongResultStillProbes(t *testing.T) {
	var cols []string
	for i := 0; i < 1500; i++ {
		cols = append(cols, "1 AS c"+strings.Repeat("0", 0)+itoa(i))
	}
	m, code, out, work := runProbe(t, "SELECT "+strings.Join(cols, ", "), "", nil)
	if code != 0 || m.Status != "succeeded" {
		t.Fatalf("a 1,500-column result failed to probe: %+v\n%s", m, work)
	}
	if f, _ := parseProbeLines(t, out); len(f.Columns) != 1500 {
		t.Errorf("columns = %d", len(f.Columns))
	}
	// Four million rows under a 96 MB limit: a held-at-once digest needs 256 MB of row hashes and is killed by the limit; a summed one holds none.
	m, code, out, work = runProbeWith(t, "SELECT range AS x FROM range(4000000)", "", nil, probeKeyHex, "96MB")
	if code != 0 || m.Status != "succeeded" || m.RowsOut != 4000000 {
		t.Fatalf("a four-million-row result failed to probe under a small limit: %+v code %d\n%s", m, code, work)
	}
}

func itoa(i int) string { return strconv.Itoa(i) }

func TestANestedDecimalIsOtherAndACarriageReturnNameIsHashedIntact(t *testing.T) {
	for in, want := range map[string]string{"DECIMAL(10,2)[]": "other", "DECIMAL(10,2)[1]": "other", "DECIMAL": "decimal", "DECIMAL(38,0)": "decimal"} {
		if got := typeFamily(in); got != want {
			t.Errorf("typeFamily(%q) = %q, want %q", in, got, want)
		}
	}
	// A lone carriage return survives to the hash. (A CR LF does not reach the engine at all: the DuckDB CLI reads its script from stdin with line endings
	// normalised, so the alias is `c\nd` there and in the plain query's result file too; the hash is of what the engine has.)
	for name, engineName := range map[string]string{"c\rd": "c\rd", "c\r\nd": "c\nd"} {
		m, code, out, work := runProbe(t, `SELECT 1 AS "`+name+`"`, "", nil)
		if code != 0 || m.Status != "succeeded" {
			t.Fatalf("%+v\n%s", m, work)
		}
		f, _ := parseProbeLines(t, out)
		if len(f.Columns) != 1 || f.Columns[0].NameHash != probeName(engineName) {
			t.Errorf("the name %q was hashed as another name than the engine's %q: %+v", name, engineName, f.Columns)
		}
	}
}

// THE DIGEST IS KEYED: the same statement under two keys digests differently (an unkeyed digest of a low-entropy result is a guess-and-check oracle for its
// values), and under one key it is stable. Mutant: leave the key out of the row hash.
func TestTheDigestIsKeyed(t *testing.T) {
	digest := func(key string) string {
		t.Helper()
		m, code, out, work := runProbeWith(t, `SELECT 'alice' AS x`, "", nil, key, "256MB")
		if code != 0 || m.Status != "succeeded" {
			t.Fatalf("%+v\n%s", m, work)
		}
		f, _ := parseProbeLines(t, out)
		return f.Digest
	}
	k1, k2 := probeKeyHex, "ffffffffffffffffffffffffffffffff"
	if digest(k1) == digest(k2) {
		t.Error("two keys give one digest: the digest is not keyed")
	}
	if digest(k1) != digest(k1) {
		t.Error("one key gives two digests")
	}
}

// ONLY THE RESULT'S OWN TABLE IS ASKED ABOUT: a table of the same name in another attached database must not join the result (duckdb_columns() lists every
// database's), and in probe mode it would have named columns the result does not have. Mutant: drop the database_name filter from any of the three statements.
func TestASameNamedTableElsewhereDoesNotJoinAProbe(t *testing.T) {
	prelude := "ATTACH ':memory:' AS other;\nCREATE TABLE other." + resultTable + "(zz INT, yy VARCHAR);\n"
	m, code, out, work := runProbe(t, `SELECT 1 AS a, 'x' AS b`, prelude, nil)
	if code != 0 || m.Status != "succeeded" {
		t.Fatalf("a same-named table elsewhere broke the probe: %+v\n%s", m, work)
	}
	if f, _ := parseProbeLines(t, out); len(f.Columns) != 2 {
		t.Errorf("columns = %+v, want the result's two", f.Columns)
	}
}

// AN ALIAS IS ANCHORED AT BOTH ENDS, because it is the one value that is put inside a double-quoted identifier: nothing that can end the identifier may pass.
// Mutants: drop the ^, the $, or end the grammar in .*
func TestAnAliasThatCouldEndTheIdentifierIsRefused(t *testing.T) {
	for _, alias := range []string{"inp_aaaaaaaa\" or \"x", "inp_aaaaaaaa\"", "x inp_aaaaaaaa", "inp_aaaaaaaa x", "inp_aaaaaaaa\n", "\ninp_aaaaaaaa", "inp_aaaaaaaa;--", "inp_aaaaaaaa.b"} {
		spec := filepath.Join(t.TempDir(), "probe.json")
		body, _ := json.Marshal(map[string]any{"name_key_hex": probeKeyHex, "inputs": []string{alias}})
		if err := os.WriteFile(spec, body, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadProbeSpec(spec); err == nil {
			t.Errorf("the alias %q was accepted", alias)
		}
	}
}
