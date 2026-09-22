package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE DIGIT THIS FILE IS ABOUT: 2^53 + 1.
//
// DuckDB widens SUM over any integer column to HUGEINT, Parquet has no 128-bit integer,
// and DuckDB's writer maps HUGEINT to DOUBLE — so a sum past 2^53 is rounded on the way
// into the file a customer downloads, with nothing downstream able to tell. The runner
// now casts HUGEINT and UHUGEINT result columns to DECIMAL(38,0), which Parquet carries
// exactly.
const pastTwoTo53 = "9007199254740993"

func TestTheGeneratedCopyCastsOnlyTheWideIntegers(t *testing.T) {
	got := copyResultSQL("/work/result.parquet", "/work/tmp/rl_copy_result.sql")

	for _, want := range []string{
		"FROM duckdb_columns() WHERE table_name = '__rl_result'",
		"'HUGEINT', 'UHUGEINT'",
		"::DECIMAL(38,0) AS ",
		"ORDER BY column_index",
		"(FORMAT PARQUET);",
		".read /work/tmp/rl_copy_result.sql",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated script lacks %q:\n%s", want, got)
		}
	}
	// Every identifier it emits is double-quoted with `"` doubled. A double quote is the
	// only thing that ends a quoted identifier, so a newline or a semicolon in a column
	// name cannot reach the statement the `.read` executes.
	if !strings.Contains(got, `replace(column_name, '"', '""')`) {
		t.Errorf("column names are not quote-escaped:\n%s", got)
	}
	// The CSV writer must not quote or split the one statement it writes.
	if !strings.Contains(got, "QUOTE ''") || !strings.Contains(got, `DELIMITER E'\x01'`) {
		t.Errorf("the generated statement could be mangled by the CSV writer:\n%s", got)
	}
}

// duckdbBinary finds the real CLI and REFUSES TO QUIETLY NOT RUN in CI, for the reason
// this whole file exists: the rest of this package's tests drive a FAKE duckdb that
// answers whatever they ask, and a fake cannot have this bug.
func duckdbBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("duckdb")
	if err == nil {
		return path
	}
	if os.Getenv("CI") != "" {
		t.Fatal("duckdb is not on PATH in CI, so the round-trip below is unchecked. " +
			"Install it before `go test`, or prove this in images/rl-duckdb/smoke instead — " +
			"but do not let it skip: every other test here uses a fake engine.")
	}
	t.Skip("duckdb not on PATH; skipping the round-trip (brew install duckdb). The smoke suite proves it against the shipped image.")
	return ""
}

// THE ROUND TRIP, ON THE REAL ENGINE. Before the fix this returns 9007199254740992.
func TestAWideIntegerSumSurvivesTheParquetRoundTrip(t *testing.T) {
	bin := duckdbBinary(t)
	work := t.TempDir()
	tmp := filepath.Join(work, "tmp")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		t.Fatal(err)
	}
	result := filepath.Join(work, "result.parquet")

	// The same shape composeScript builds, minus the parts that need a plane.
	script := "CREATE TEMP TABLE " + resultTable + " AS\n" +
		"SELECT SUM(x) AS units, 'keep me' AS \"odd \"\"name\", 7::INTEGER AS small\n" +
		"  FROM (SELECT " + pastTwoTo53 + "::BIGINT AS x) t\n;\n" +
		copyResultSQL(result, filepath.Join(tmp, copyScript))

	cmd := exec.Command(bin, "-batch", "-bail", "-no-init", "-csv", "-noheader")
	cmd.Dir = work
	cmd.Env = []string{"HOME=" + work, "TMPDIR=" + tmp}
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("composing the result failed: %v\n%s\nscript:\n%s", err, out, script)
	}

	read := exec.Command(bin, "-batch", "-bail", "-no-init", "-csv", "-noheader", "-c",
		"SELECT units::VARCHAR, \"odd \"\"name\", small::VARCHAR FROM read_parquet('"+result+"');")
	read.Env = []string{"HOME=" + work, "TMPDIR=" + tmp}
	out, err := read.CombinedOutput()
	if err != nil {
		t.Fatalf("reading the result back: %v\n%s", err, out)
	}
	got := strings.TrimSpace(string(out))
	if !strings.Contains(got, pastTwoTo53) {
		t.Errorf("the sum came back as %q, want it to contain %s — the digit was lost in the file, which is the whole defect", got, pastTwoTo53)
	}
	// The other columns must be untouched: this casts wide integers, not everything.
	if !strings.Contains(got, "keep me") || !strings.Contains(got, "7") {
		t.Errorf("a non-integer column did not survive: %q", got)
	}

	// And the parquet type is a decimal, not a double.
	schema := exec.Command(bin, "-batch", "-bail", "-no-init", "-csv", "-noheader", "-c",
		"SELECT name || '=' || type FROM parquet_schema('"+result+"') WHERE name = 'units';")
	schema.Env = []string{"HOME=" + work, "TMPDIR=" + tmp}
	sout, err := schema.CombinedOutput()
	if err != nil {
		t.Fatalf("reading the parquet schema: %v\n%s", err, sout)
	}
	if strings.Contains(string(sout), "DOUBLE") {
		t.Errorf("units is still written as a DOUBLE: %s", strings.TrimSpace(string(sout)))
	}
}
