package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"
)

// THE TASK'S WRITE, ON THE REAL ENGINE, AGAINST A NATIVE DuckDB DATABASE STANDING IN FOR THE CATALOG.
//
// WHAT THIS PROVES AND WHAT IT DOES NOT. The scripts are real SQL run by the pinned CLI, so the composition, the quoting, the checks, the sentinels and the `.read` chain are exercised and
// their syntax and semantics are the engine's. The attached database is a native one, not an Iceberg REST table: what DuckDB's iceberg extension does with the same statements (two
// snapshots for an overwrite, merge-on-read for a MERGE) rests on docs/measurements/2026-09-29-duckdb-1.5.5-iceberg-writes.md, from which every statement here is taken, and not on this
// file. A write into a NESTED namespace through the dotted-schema form was not measured on Iceberg either; here "a.b" is a native schema of that name.

type taskRig struct {
	t    *testing.T
	bin  string
	work string
	db   string
}

func newTaskRig(t *testing.T) *taskRig {
	t.Helper()
	work := t.TempDir()
	r := &taskRig{t: t, bin: duckdbBinary(t), work: work, db: filepath.Join(work, "cat.duckdb")}
	r.seed(`CREATE SCHEMA "a.b";
CREATE TABLE "a.b".t (id INTEGER, d DATE, ts TIMESTAMP, v VARCHAR);`)
	return r
}

// cli runs the real CLI on the database file, outside the runner, to seed it and to read it back.
func (r *taskRig) cli(sql string) string {
	r.t.Helper()
	out, err := exec.Command(r.bin, "-batch", "-csv", "-noheader", r.db, "-c", sql).CombinedOutput()
	if err != nil {
		r.t.Fatalf("duckdb %q: %v\n%s", sql, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (r *taskRig) seed(sql string) { r.t.Helper(); r.cli(sql) }

// rows reads the target back, one comma-joined line per row in id order.
func (r *taskRig) rows() string {
	r.t.Helper()
	// The CLI writes a NULL as the word; the expectations below leave it empty.
	return strings.ReplaceAll(strings.ReplaceAll(r.cli(`SELECT id, d, ts, v FROM "a.b".t ORDER BY id, d, v`), "NULL", ""), "\n", " | ")
}

// run is the runner, as main runs it, over this rig: the spec, the statement and the prelude that attaches the stand-in catalog. It returns the metrics, the exit code and the lines of
// stdout.
func (r *taskRig) run(spec, query string, env map[string]string) (metrics, int, []string) {
	r.t.Helper()
	write := func(name, content string) string {
		p := filepath.Join(r.work, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			r.t.Fatal(err)
		}
		return p
	}
	vars := map[string]string{
		"RL_QUERY_FILE":   write("query.sql", query),
		"RL_TASK_FILE":    write("task.json", spec),
		"RL_PRELUDE_FILE": write("prelude.sql", fmt.Sprintf("ATTACH %s AS rl_catalog;\n", sqlString(r.db))),
		"RL_INIT_FILE":    write("init.sql", ""),
		"RL_WORK_DIR":     filepath.Join(r.work, "w"),
		"RL_DUCKDB_BIN":   r.bin,
		"RL_THREADS":      "2",
		"RL_MEMORY_LIMIT": "256MB",
	}
	for k, v := range env {
		vars[k] = v
	}
	if err := os.MkdirAll(vars["RL_WORK_DIR"], 0o700); err != nil {
		r.t.Fatal(err)
	}
	cfg, err := configFromEnv(func(k string) string { return vars[k] })
	if err != nil {
		r.t.Fatalf("configFromEnv: %v", err)
	}
	m, code := run(context.Background(), cfg)
	var out bytes.Buffer
	emit(&out, m)
	return m, code, strings.Split(strings.TrimSpace(out.String()), "\n")
}

func taskSpecJSON(mode, extra string) string {
	if extra != "" {
		extra = "," + extra
	}
	return fmt.Sprintf(`{"v":2,"attempt":"att-1","mode":%q,"target":{"warehouse":"wh","namespace":["a","b"],"name":"t"},"base_snapshot_id":"4711"%s}`, mode, extra)
}

// claimOf reads the RL_TASK line out of the output: it must be exactly one, directly before the RL_METRICS line.
func claimOf(t *testing.T, lines []string) taskLine {
	t.Helper()
	n := 0
	for _, l := range lines {
		if strings.HasPrefix(l, "RL_TASK ") {
			n++
		}
	}
	if n != 1 || len(lines) < 2 || !strings.HasPrefix(lines[len(lines)-2], "RL_TASK ") || !strings.HasPrefix(lines[len(lines)-1], "RL_METRICS ") {
		t.Fatalf("the output is not one RL_TASK line directly before the RL_METRICS line: %q", lines)
	}
	var c taskLine
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[len(lines)-2], "RL_TASK ")), &c); err != nil {
		t.Fatalf("RL_TASK is not JSON: %v", err)
	}
	return c
}

func wantClaim(t *testing.T, lines []string, outcome string) {
	t.Helper()
	wantClaimBase(t, lines, outcome, "4711")
}

func wantClaimBase(t *testing.T, lines []string, outcome, base string) {
	t.Helper()
	c := claimOf(t, lines)
	if c.V != 1 || c.Attempt != "att-1" || c.Outcome != outcome || c.BaseSnapshotID != base || c.CommittedSnapshotID != "0" || c.FilesWritten != 0 || c.BytesWritten != 0 {
		t.Errorf("claim = %+v, want outcome %q for attempt att-1, base 4711, no snapshot, no counts", c, outcome)
	}
}

func TestAnAppendWritesTheResultToTheTargetAndClaimsCommitted(t *testing.T) {
	r := newTaskRig(t)
	r.seed(`INSERT INTO "a.b".t VALUES (1, DATE '2026-09-01', TIMESTAMP '2026-09-01 10:00:00', 'one')`)
	m, code, lines := r.run(taskSpecJSON("append", ""), "SELECT 2 AS id, DATE '2026-09-02' AS d, TIMESTAMP '2026-09-02 10:00:00' AS ts, 'two' AS v", nil)
	if code != 0 || m.Status != "succeeded" || m.RowsOut != 1 || m.ResultUploaded || m.ResultBytes != 0 {
		t.Fatalf("metrics = %+v code %d", m, code)
	}
	wantClaim(t, lines, "committed")
	if got, want := r.rows(), "1,2026-09-01,2026-09-01 10:00:00,one | 2,2026-09-02,2026-09-02 10:00:00,two"; got != want {
		t.Errorf("table = %q, want %q", got, want)
	}
	// Nothing the statement made is left to upload: a task's result is the table.
	if _, err := os.Stat(filepath.Join(r.work, "w", "result.parquet")); err == nil {
		t.Error("a task wrote a result file")
	}
}

func TestAnEmptyAppendOrMergeWritesNothingAndClaimsNoChange(t *testing.T) {
	for mode, extra := range map[string]string{"append": "", "merge": `"unique_key":["id"]`} {
		r := newTaskRig(t)
		r.seed(`INSERT INTO "a.b".t VALUES (1, NULL, NULL, 'one')`)
		m, code, lines := r.run(taskSpecJSON(mode, extra), "SELECT 2 AS id, NULL::DATE AS d, NULL::TIMESTAMP AS ts, 'two' AS v WHERE false", nil)
		if code != 0 || m.Status != "succeeded" || m.RowsOut != 0 {
			t.Fatalf("%s: metrics = %+v code %d", mode, m, code)
		}
		wantClaim(t, lines, "no_change")
		if got := r.rows(); got != "1,,,one" {
			t.Errorf("%s: table = %q, want it untouched", mode, got)
		}
	}
}

func TestAnOverwriteReplacesEverythingAndAnEmptyOneEmptiesTheTable(t *testing.T) {
	r := newTaskRig(t)
	r.seed(`INSERT INTO "a.b".t VALUES (1, NULL, NULL, 'old'), (2, NULL, NULL, 'old')`)
	_, code, lines := r.run(taskSpecJSON("overwrite", ""), "SELECT 9 AS id, NULL::DATE AS d, NULL::TIMESTAMP AS ts, 'new' AS v", nil)
	if code != 0 {
		t.Fatalf("exit %d: %q", code, lines)
	}
	wantClaim(t, lines, "committed")
	if got := r.rows(); got != "9,,,new" {
		t.Errorf("table = %q", got)
	}
	// An overwrite of nothing is still an overwrite: the old rows go.
	_, code, lines = r.run(taskSpecJSON("overwrite", ""), "SELECT 9 AS id, NULL::DATE AS d, NULL::TIMESTAMP AS ts, 'new' AS v WHERE false", nil)
	if code != 0 {
		t.Fatalf("exit %d: %q", code, lines)
	}
	wantClaim(t, lines, "committed")
	if got := r.rows(); got != "" {
		t.Errorf("table = %q, want it emptied", got)
	}
}

func TestAPartitionOverwriteReplacesOnlyItsPartitionByEveryKindOfBound(t *testing.T) {
	seed := `INSERT INTO "a.b".t VALUES
	  (1, DATE '2026-09-01', TIMESTAMP '2026-09-01 12:00:00', 'a'), (2, DATE '2026-09-02', TIMESTAMP '2026-09-02 12:00:00', 'b'), (3, DATE '2026-09-03', TIMESTAMP '2026-09-03 12:00:00', 'c')`
	for _, tc := range []struct {
		name      string
		partition string
		query     string
		want      string
	}{
		{"a date range, half open", `"partition":{"column":"d","start":{"kind":"date","value":"2026-09-02"},"end":{"kind":"date","value":"2026-09-03"}}`,
			`SELECT 20 AS id, DATE '2026-09-02' AS d, TIMESTAMP '2026-09-02 01:00:00' AS ts, 'B' AS v`,
			"1,2026-09-01,2026-09-01 12:00:00,a | 3,2026-09-03,2026-09-03 12:00:00,c | 20,2026-09-02,2026-09-02 01:00:00,B"},
		{"an instant range over a TIMESTAMP column", `"partition":{"column":"ts","start":{"kind":"instant","value":"2026-09-02T00:00:00Z"},"end":{"kind":"instant","value":"2026-09-03T00:00:00Z"}}`,
			`SELECT 20 AS id, DATE '2026-09-02' AS d, TIMESTAMP '2026-09-02 23:59:59' AS ts, 'B' AS v`,
			"1,2026-09-01,2026-09-01 12:00:00,a | 3,2026-09-03,2026-09-03 12:00:00,c | 20,2026-09-02,2026-09-02 23:59:59,B"},
		{"an integer range", `"partition":{"column":"id","start":{"kind":"integer","value":"2"},"end":{"kind":"integer","value":"4"}}`,
			`SELECT 2 AS id, DATE '2026-09-02' AS d, NULL::TIMESTAMP AS ts, 'B' AS v UNION ALL SELECT 3, DATE '2026-09-03', NULL, 'C'`,
			"1,2026-09-01,2026-09-01 12:00:00,a | 2,2026-09-02,,B | 3,2026-09-03,,C"},
		{"a static text partition", `"partition":{"column":"v","start":{"kind":"text","value":"b"}}`,
			`SELECT 22 AS id, DATE '2026-09-22' AS d, NULL::TIMESTAMP AS ts, 'b' AS v`,
			"1,2026-09-01,2026-09-01 12:00:00,a | 3,2026-09-03,2026-09-03 12:00:00,c | 22,2026-09-22,,b"},
		{"a static date partition with an empty result", `"partition":{"column":"d","start":{"kind":"date","value":"2026-09-02"}}`,
			`SELECT 22 AS id, DATE '2026-09-02' AS d, NULL::TIMESTAMP AS ts, 'b' AS v WHERE false`,
			"1,2026-09-01,2026-09-01 12:00:00,a | 3,2026-09-03,2026-09-03 12:00:00,c"},
	} {
		r := newTaskRig(t)
		r.seed(seed)
		_, code, lines := r.run(taskSpecJSON("partition_overwrite", tc.partition), tc.query, nil)
		if code != 0 {
			t.Errorf("%s: exit %d: %q", tc.name, code, lines)
			continue
		}
		wantClaim(t, lines, "committed")
		if got := r.rows(); got != tc.want {
			t.Errorf("%s: table = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestAPartitionOverwriteRefusesAResultOutsideItsPartitionBeforeWritingAnything(t *testing.T) {
	r := newTaskRig(t)
	r.seed(`INSERT INTO "a.b".t VALUES (1, DATE '2026-09-02', NULL, 'keep')`)
	part := `"partition":{"column":"d","start":{"kind":"date","value":"2026-09-02"},"end":{"kind":"date","value":"2026-09-03"}}`
	for name, q := range map[string]string{
		"a row in another partition": `SELECT 2 AS id, DATE '2026-09-02' AS d, NULL::TIMESTAMP AS ts, 'in' AS v UNION ALL SELECT 3, DATE '2026-09-09', NULL, 'out'`,
		"a NULL partition value":     `SELECT 2 AS id, NULL::DATE AS d, NULL::TIMESTAMP AS ts, 'null' AS v`,
	} {
		m, code, lines := r.run(taskSpecJSON("partition_overwrite", part), q, nil)
		if code == 0 || m.Status != "failed" || m.ErrorClass == "" {
			t.Errorf("%s: metrics = %+v code %d, want a failure", name, m, code)
		}
		wantClaim(t, lines, "not_committed")
		if got := r.rows(); got != "1,2026-09-02,,keep" {
			t.Errorf("%s: table = %q, want it untouched", name, got)
		}
	}
}

func TestAMergeUpdatesMatchedRowsInsertsTheRestAndHonoursACompositeKey(t *testing.T) {
	r := newTaskRig(t)
	r.seed(`INSERT INTO "a.b".t VALUES (1, DATE '2026-09-01', NULL, 'a'), (2, DATE '2026-09-01', NULL, 'b'), (2, DATE '2026-09-02', NULL, 'c')`)
	_, code, lines := r.run(taskSpecJSON("merge", `"unique_key":["id","d"]`),
		`SELECT 2 AS id, DATE '2026-09-02' AS d, NULL::TIMESTAMP AS ts, 'C' AS v UNION ALL SELECT 4, DATE '2026-09-04', NULL, 'D'`, nil)
	if code != 0 {
		t.Fatalf("exit %d: %q", code, lines)
	}
	wantClaim(t, lines, "committed")
	if got, want := r.rows(), "1,2026-09-01,,a | 2,2026-09-01,,b | 2,2026-09-02,,C | 4,2026-09-04,,D"; got != want {
		t.Errorf("table = %q, want %q", got, want)
	}
	// The same merge again is a retry: the content is unchanged.
	_, code, lines = r.run(taskSpecJSON("merge", `"unique_key":["id","d"]`),
		`SELECT 2 AS id, DATE '2026-09-02' AS d, NULL::TIMESTAMP AS ts, 'C' AS v UNION ALL SELECT 4, DATE '2026-09-04', NULL, 'D'`, nil)
	if code != 0 {
		t.Fatalf("retry exit %d: %q", code, lines)
	}
	if got, want := r.rows(), "1,2026-09-01,,a | 2,2026-09-01,,b | 2,2026-09-02,,C | 4,2026-09-04,,D"; got != want {
		t.Errorf("table after the retry = %q, want %q", got, want)
	}
}

func TestAMergeRefusesADuplicateOrNullKeyInItsSourceBeforeWritingAnything(t *testing.T) {
	for name, q := range map[string]string{
		"a duplicate key that is already in the table": `SELECT 1 AS id, NULL::DATE AS d, NULL::TIMESTAMP AS ts, 'x' AS v UNION ALL SELECT 1, NULL, NULL, 'y'`,
		// The engine lands a duplicate NEW key twice (m810 P3); the runner refuses it.
		"a duplicate key that is new": `SELECT 9 AS id, NULL::DATE AS d, NULL::TIMESTAMP AS ts, 'x' AS v UNION ALL SELECT 9, NULL, NULL, 'y'`,
		"a NULL key":                  `SELECT NULL::INTEGER AS id, NULL::DATE AS d, NULL::TIMESTAMP AS ts, 'x' AS v`,
	} {
		r := newTaskRig(t)
		r.seed(`INSERT INTO "a.b".t VALUES (1, NULL, NULL, 'a')`)
		m, code, lines := r.run(taskSpecJSON("merge", `"unique_key":["id"]`), q, nil)
		if code == 0 || m.Status != "failed" {
			t.Errorf("%s: metrics = %+v code %d, want a failure", name, m, code)
		}
		wantClaim(t, lines, "not_committed")
		if got := r.rows(); got != "1,,,a" {
			t.Errorf("%s: table = %q, want it untouched", name, got)
		}
	}
}

// A name is one quoted identifier, whatever it holds: a keyword, a quote, a space, a dot in the table (not in a level).
func TestTheTargetAndItsColumnsAreQuotedAsIdentifiersWhateverTheyHold(t *testing.T) {
	r := newTaskRig(t)
	r.seed(`CREATE SCHEMA "we""ird.sch ema"; CREATE TABLE "we""ird.sch ema"."order" ("select" INTEGER, "a b" VARCHAR)`)
	spec := `{"v":2,"attempt":"att-1","mode":"merge","target":{"warehouse":"wh","namespace":["we\"ird","sch ema"],"name":"order"},"unique_key":["select"],"base_snapshot_id":"0"}`
	_, code, lines := r.run(spec, `SELECT 1 AS "select", 'x' AS "a b"`, nil)
	if code != 0 {
		t.Fatalf("exit %d: %q", code, lines)
	}
	wantClaimBase(t, lines, "committed", "0")
	if got := r.cli(`SELECT "select", "a b" FROM "we""ird.sch ema"."order"`); got != "1,x" {
		t.Errorf("table = %q", got)
	}
}

func TestATaskThatIsNotATaskTheRunnerCanComposeIsRefusedBeforeTheEngineRuns(t *testing.T) {
	r := newTaskRig(t)
	for name, tc := range map[string]struct{ spec, query string }{
		"a statement that is not a query": {taskSpecJSON("append", ""), "INSERT INTO t VALUES (1)"},
		"a describe":                      {taskSpecJSON("append", ""), "DESCRIBE SELECT 1"},
		"a spec that is not JSON":         {"not json", "SELECT 1"},
		"a version it does not know":      {strings.Replace(taskSpecJSON("append", ""), `"v":2`, `"v":3`, 1), "SELECT 1"},
		"a field it does not know":        {taskSpecJSON("append", `"sql":"DROP TABLE t"`), "SELECT 1"},
		"a mode it does not compose":      {taskSpecJSON("truncate", ""), "SELECT 1"},
	} {
		m, code, lines := r.run(tc.spec, tc.query, nil)
		if code != 1 || m.Status != "failed" || m.ErrorClass != classInvalidQuery {
			t.Errorf("%s: metrics = %+v code %d, want invalid_query_file", name, m, code)
		}
		for _, l := range lines {
			if strings.HasPrefix(l, "RL_TASK ") {
				t.Errorf("%s: a refused task printed a claim: %s", name, l)
			}
		}
	}
	// A task is not a probe, even when the probe's own spec is a good one.
	probe := filepath.Join(r.work, "probe.json")
	if err := os.WriteFile(probe, []byte(`{"name_key_hex":"00112233445566778899aabbccddeeff","inputs":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, _, _ := r.run(taskSpecJSON("append", ""), "SELECT 1 AS id, NULL::DATE AS d, NULL::TIMESTAMP AS ts, 'x' AS v", map[string]string{"RL_PROBE_FILE": probe})
	if m.ErrorClass != classInvalidQuery {
		t.Errorf("a task and a probe together: %+v", m)
	}
	if got := r.rows(); got != "" {
		t.Errorf("a refused task wrote %q", got)
	}
}

func TestTheSpecIsHeldToItsGrammarAndNeverQuotedBack(t *testing.T) {
	good := func() taskSpec {
		var s taskSpec
		s.V, s.Attempt, s.Mode, s.BaseSnapshotID = 2, "att-1", "append", "0"
		s.Target.Namespace, s.Target.Name = []string{"a"}, "t"
		return s
	}
	merge := func(keys ...string) taskSpec { s := good(); s.Mode, s.UniqueKey = "merge", keys; return s }
	part := func(p *taskPartition) taskSpec { s := good(); s.Mode, s.Partition = "partition_overwrite", p; return s }
	date := func(v string) *taskValue { return &taskValue{Kind: "date", Value: v} }
	for name, tc := range map[string]struct {
		spec taskSpec
		ok   bool
	}{
		"a plain append":                  {good(), true},
		"an attempt with a quote":         {func() taskSpec { s := good(); s.Attempt = `a"b`; return s }(), false},
		"no namespace":                    {func() taskSpec { s := good(); s.Target.Namespace = nil; return s }(), false},
		"a level with a dot":              {func() taskSpec { s := good(); s.Target.Namespace = []string{"a.b"}; return s }(), false},
		"a name with a newline":           {func() taskSpec { s := good(); s.Target.Name = "t\n.shell id"; return s }(), false},
		"a name with a control character": {func() taskSpec { s := good(); s.Target.Name = "t\x00"; return s }(), false},
		"a name that is not UTF-8":        {func() taskSpec { s := good(); s.Target.Name = "t\xff"; return s }(), false},
		"a base that is not a number":     {func() taskSpec { s := good(); s.BaseSnapshotID = "12a"; return s }(), false},
		"a base past int64":               {func() taskSpec { s := good(); s.BaseSnapshotID = "99999999999999999999"; return s }(), false},
		"a negative base":                 {func() taskSpec { s := good(); s.BaseSnapshotID = "-1"; return s }(), false},
		"a base with a plus":              {func() taskSpec { s := good(); s.BaseSnapshotID = "+5"; return s }(), false},
		"a merge with a key":              {merge("id"), true},
		"a merge with no key":             {merge(), false},
		"a merge with a key twice":        {merge("id", "ID"), false},
		"a partition on an append": {func() taskSpec {
			s := good()
			s.Partition = &taskPartition{Column: "d", Start: date("2026-09-01")}
			return s
		}(), false},
		"a partition overwrite with none":       {part(nil), false},
		"a partition with no column":            {part(&taskPartition{Start: date("2026-09-01")}), false},
		"a partition with a range":              {part(&taskPartition{Column: "d", Start: date("2026-09-01"), End: date("2026-09-02")}), true},
		"a date that is not one":                {part(&taskPartition{Column: "d", Start: date("2026-13-01")}), false},
		"a date written another way":            {part(&taskPartition{Column: "d", Start: date("2026-9-1")}), false},
		"bounds of two kinds":                   {part(&taskPartition{Column: "d", Start: date("2026-09-01"), End: &taskValue{Kind: "integer", Value: "5"}}), false},
		"a range of text":                       {part(&taskPartition{Column: "d", Start: &taskValue{Kind: "text", Value: "a"}, End: &taskValue{Kind: "text", Value: "b"}}), false},
		"a static text partition":               {part(&taskPartition{Column: "d", Start: &taskValue{Kind: "text", Value: "it's"}}), true},
		"an integer that is a SQL fragment":     {part(&taskPartition{Column: "d", Start: &taskValue{Kind: "integer", Value: "1; DROP"}}), false},
		"an instant that is not one":            {part(&taskPartition{Column: "d", Start: &taskValue{Kind: "instant", Value: "2026-09-01"}}), false},
		"an instant":                            {part(&taskPartition{Column: "d", Start: &taskValue{Kind: "instant", Value: "2026-09-01T10:00:00.5+02:00"}}), true},
		"a kind nobody named":                   {part(&taskPartition{Column: "d", Start: &taskValue{Kind: "bytes", Value: "x"}}), false},
		"a text bound with a control character": {part(&taskPartition{Column: "d", Start: &taskValue{Kind: "text", Value: "a\nb"}}), false},
	} {
		err := tc.spec.validate()
		if (err == nil) != tc.ok {
			t.Errorf("%s: validate = %v, want ok=%v", name, err, tc.ok)
		}
		if err != nil {
			for _, secret := range []string{"2026-13-01", "12a", "DROP", "shell"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("%s: the error quotes the spec: %v", name, err)
				}
			}
		}
	}
}

func TestAnInstantBoundIsAnInstantInUTC(t *testing.T) {
	v := &taskValue{Kind: "instant", Value: "2026-09-01T10:00:00.5+02:00"}
	if got, want := v.literal(), `TIMESTAMPTZ '2026-09-01 08:00:00.5+00'`; got != want {
		t.Errorf("literal = %s, want %s", got, want)
	}
	if got, want := (&taskValue{Kind: "text", Value: "it's"}).literal(), `'it''s'`; got != want {
		t.Errorf("literal = %s, want %s", got, want)
	}
}

// THE CLAIM'S OUTCOME FROM HOW FAR THE SCRIPT GOT, on the fake engine, which prints the sentinels a mode says (task_fake_test.go's modes). The unit under test is taskOutcome.
func TestTheOutcomeIsTheWordOfHowFarTheWriteGot(t *testing.T) {
	merge := &taskSpec{Mode: "merge"}
	overwrite := &taskSpec{Mode: "overwrite"}
	partition := &taskSpec{Mode: "partition_overwrite"}
	for _, tc := range []struct {
		name     string
		spec     *taskSpec
		printed  []string
		normally bool
		failed   bool
		want     string
	}{
		{"the write returned", overwrite, []string{writeBeginSentinel, writeStagedSentinel, writeCommittedSentinel}, true, false, "committed"},
		{"nothing was to be written", merge, []string{writeSkippedSentinel}, true, false, "no_change"},
		{"it failed before the write began", overwrite, nil, true, true, "not_committed"},
		{"a transaction failed before COMMIT was sent", overwrite, []string{writeBeginSentinel}, true, true, "not_committed"},
		{"a partition's transaction failed before COMMIT was sent", partition, []string{writeBeginSentinel}, true, true, "not_committed"},
		{"a partition's transaction failed after COMMIT was sent", partition, []string{writeBeginSentinel, writeStagedSentinel}, true, true, "unknown"},
		{"a transaction failed after COMMIT was sent", overwrite, []string{writeBeginSentinel, writeStagedSentinel}, true, true, "unknown"},
		{"a single statement failed", merge, []string{writeBeginSentinel}, true, true, "unknown"},
		{"it was killed before the write began, so what it printed may be lost", overwrite, nil, false, true, "unknown"},
		{"it was killed after the write began", overwrite, []string{writeBeginSentinel}, false, true, "unknown"},
		{"it returned success and said nothing of the write", overwrite, nil, true, false, "unknown"},
	} {
		out := filepath.Join(t.TempDir(), "duckdb.out")
		if err := os.WriteFile(out, []byte(strings.Join(tc.printed, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := taskOutcome(tc.spec, out, tc.normally, tc.failed); got != tc.want {
			t.Errorf("%s: outcome = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// THE FAKE ENGINE'S TASK MODES: how the runner reads an engine that stops somewhere.
func fakeTaskEngine(mode string, script string, home string) int {
	fmt.Println(initSentinel)
	if strings.Contains(script, "SELECT '"+preludeSentinel+"';") {
		fmt.Println(preludeSentinel)
	}
	if m := regexpCount.FindStringSubmatch(script); m != nil {
		_ = os.WriteFile(m[1], []byte("3\n"), 0o644)
	}
	switch mode {
	case "task_ok":
		fmt.Println(writeBeginSentinel)
		fmt.Println(writeStagedSentinel)
		fmt.Println(writeCommittedSentinel)
		return 0
	case "task_skipped":
		fmt.Println(writeSkippedSentinel)
		return 0
	case "task_fail_before_write":
		fmt.Fprintln(os.Stderr, "Binder Error: Referenced column \"secret_col\" not found")
		return 1
	case "task_fail_in_transaction":
		fmt.Println(writeBeginSentinel)
		fmt.Fprintln(os.Stderr, "Constraint Error: a value of customer data")
		return 1
	case "task_fail_at_commit":
		fmt.Println(writeBeginSentinel)
		fmt.Println(writeStagedSentinel)
		fmt.Fprintln(os.Stderr, "HTTP Error: the commit's answer was lost")
		return 1
	case "task_killed_in_write":
		fmt.Println(writeBeginSentinel)
		_ = killSelf()
		time.Sleep(time.Second)
		return 1
	case "task_hang_in_write":
		fmt.Println(writeBeginSentinel)
		time.Sleep(time.Minute)
		return 0
	}
	return 0
}

var regexpCount = regexp.MustCompile(`COPY \(SELECT count\(\*\) FROM __rl_result\) TO '([^']+)' \(FORMAT CSV, HEADER false\);`)

func killSelf() error { return syscall.Kill(os.Getpid(), syscall.SIGKILL) }

// taskHarness is the fake-engine harness with a task spec and a catalog token file in place.
func taskHarness(t *testing.T, mode string, env map[string]string) *harness {
	t.Helper()
	work := t.TempDir()
	spec := filepath.Join(work, "task.json")
	if err := os.WriteFile(spec, []byte(taskSpecJSON(mode2spec(mode), `"unique_key":["id"]`)), 0o600); err != nil {
		t.Fatal(err)
	}
	vars := map[string]string{"RL_TASK_FILE": spec}
	for k, v := range env {
		vars[k] = v
	}
	h := newHarness(t, "SELECT 1 AS id", vars)
	h.mode(mode)
	return h
}

// mode2spec: the fake modes that stand for a transaction use an overwrite, the others a merge (a single statement).
func mode2spec(mode string) string {
	switch mode {
	case "task_fail_in_transaction", "task_fail_at_commit", "task_ok":
		return "overwrite"
	}
	return "merge"
}

// runTask runs the runner the way main does and returns the metrics, the exit code and every line of stdout.
func (h *harness) runTask() (metrics, int, []string) {
	h.t.Helper()
	m, code := run(context.Background(), h.cfg)
	emit(&h.out, m)
	return m, code, strings.Split(strings.TrimSpace(h.out.String()), "\n")
}

func TestTheClaimIsWhatTheEngineGotToAndTheStatusIsTheRunnersOwn(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		outcome string
		status  string
		class   string
		code    int
		env     map[string]string
	}{
		{"task_ok", "committed", "succeeded", "", 0, nil},
		{"task_skipped", "no_change", "succeeded", "", 0, nil},
		{"task_fail_before_write", "not_committed", "failed", classBinder, 1, nil},
		{"task_fail_in_transaction", "not_committed", "failed", classSQL, 1, nil},
		{"task_fail_at_commit", "unknown", "failed", classHTTP, 1, nil},
		{"task_killed_in_write", "unknown", "failed", classOOM, 1, nil},
		{"task_hang_in_write", "unknown", "failed", classTimeout, 2, map[string]string{"RL_TIMEOUT_SECONDS": "1"}},
	} {
		h := taskHarness(t, tc.mode, tc.env)
		m, code, lines := h.runTask()
		if code != tc.code || m.Status != tc.status || m.ErrorClass != tc.class {
			t.Errorf("%s: metrics = %+v code %d, want %s %q exit %d", tc.mode, m, code, tc.status, tc.class, tc.code)
		}
		wantClaim(t, lines, tc.outcome)
		// Nothing of the customer's reaches stdout: the claim and the metrics are words and numbers.
		for _, l := range lines {
			for _, leak := range []string{"secret_col", "customer data", "att-1x"} {
				if strings.Contains(l, leak) {
					t.Errorf("%s: stdout quotes the engine: %s", tc.mode, l)
				}
			}
		}
	}
}

func TestATaskWhoseEngineCannotBeRunClaimsNothingWasWritten(t *testing.T) {
	h := taskHarness(t, "task_ok", map[string]string{"RL_DUCKDB_BIN": "/nonexistent/duckdb"})
	m, code, lines := h.runTask()
	if code != 1 || m.ErrorClass != classRunnerIO {
		t.Fatalf("metrics = %+v code %d", m, code)
	}
	wantClaim(t, lines, "not_committed")
}

func TestTheCatalogTokenIsMadeASecretAfterTheLockAndBeforeThePreludeAndLeaksNowhere(t *testing.T) {
	const tok = "e" + "yJhbGciOiJSUzI1NiJ9.e" + "yJzdWIiOiJwaXBlbGluZSJ9.c2ln-_bmF0dXJl"
	work := t.TempDir()
	tokFile := filepath.Join(work, "catalog_token")
	if err := os.WriteFile(tokFile, []byte(tok+"\n"), 0o440); err != nil {
		t.Fatal(err)
	}
	prelude := filepath.Join(work, "prelude.sql")
	if err := os.WriteFile(prelude, []byte("ATTACH 'wh' AS rl_catalog (TYPE iceberg, ENDPOINT 'https://c/catalog', SUPPORT_NESTED_NAMESPACES true);\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := taskHarness(t, "task_ok", map[string]string{"RL_CATALOG_TOKEN_FILE": tokFile, "RL_PRELUDE_FILE": prelude})
	m, code, lines := h.runTask()
	if code != 0 || m.Status != "succeeded" {
		t.Fatalf("metrics = %+v code %d", m, code)
	}
	s := h.script()
	want := "CREATE SECRET " + catalogSecret + " (TYPE iceberg, TOKEN '" + tok + "');"
	lock, sentinel, secret, attach := strings.Index(s, "SET lock_configuration=true;"), strings.Index(s, "SELECT '"+initSentinel+"';"), strings.Index(s, want), strings.Index(s, "ATTACH 'wh'")
	if !(lock >= 0 && lock < sentinel && sentinel < secret && secret < attach) {
		t.Errorf("the secret is not between the init sentinel and the prelude (lock %d, sentinel %d, secret %d, attach %d):\n%s", lock, sentinel, secret, attach, s)
	}
	for _, l := range lines {
		if strings.Contains(l, tok) {
			t.Errorf("the token reached stdout: %s", l)
		}
	}
	if strings.Contains(h.errorLog(), tok) {
		t.Error("the token reached error.log")
	}
	// The token is on the engine's stdin and in no file the runner wrote.
	err := filepath.Walk(h.work, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || filepath.Base(path) == "fake_script.sql" || filepath.Base(path) == "fake_env" {
			return nil
		}
		if b, _ := os.ReadFile(path); strings.Contains(string(b), tok) {
			t.Errorf("the token is in %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if env, _ := os.ReadFile(filepath.Join(h.work, "fake_env")); strings.Contains(string(env), tok) {
		t.Error("the token is in the engine's environment")
	}
}

func TestACatalogTokenThatIsNotATokenIsRefusedAndNeverQuoted(t *testing.T) {
	for name, content := range map[string]string{
		"a quote":     "abc'; DROP SECRET x; --abcdef",
		"a newline":   "abcdefgh\nijklmnop",
		"empty":       "",
		"too short":   "abc",
		"a space":     "abcdefgh ijklmnop",
		"a backslash": `abcdefgh\ijklmnop`,
	} {
		work := t.TempDir()
		tokFile := filepath.Join(work, "catalog_token")
		if err := os.WriteFile(tokFile, []byte(content), 0o440); err != nil {
			t.Fatal(err)
		}
		h := taskHarness(t, "task_ok", map[string]string{"RL_CATALOG_TOKEN_FILE": tokFile})
		m, code, lines := h.runTask()
		if code != 1 || m.ErrorClass != classRunnerIO {
			t.Errorf("%s: metrics = %+v code %d, want runner_io_error", name, m, code)
		}
		if _, err := os.Stat(filepath.Join(h.work, "fake_script.sql")); err == nil {
			t.Errorf("%s: the engine ran with a token it should have refused", name)
		}
		if strings.Contains(h.errorLog(), "DROP SECRET") {
			t.Errorf("%s: error.log quotes the token", name)
		}
		for _, l := range lines {
			if strings.HasPrefix(l, "RL_TASK ") {
				t.Errorf("%s: a run that never reached the engine printed a claim", name)
			}
		}
	}
}

// A run with no token and no task is the plain query it always was: nothing in this file changes it.
func TestAPlainQueryIsUntouchedByTaskMode(t *testing.T) {
	h := newHarness(t, "SELECT 1", nil)
	m, code, line := h.exec()
	if code != 0 || m.Status != "succeeded" || strings.Contains(line, "RL_TASK") {
		t.Fatalf("metrics = %+v code %d line %q", m, code, line)
	}
	s := h.script()
	if strings.Contains(s, "CREATE SECRET") || strings.Contains(s, "TimeZone") || strings.Contains(s, writeBeginSentinel) {
		t.Errorf("a plain query's script carries task mode:\n%s", s)
	}
}
