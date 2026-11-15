package main

import (
	"os"
	"path/filepath"
	"testing"
)

// THE SCRIPT A RUN WITH NO BINDINGS DESCRIPTOR WRITES, FROZEN.
//
// Every file in testdata/golden is the output of composeScript at cbcb7ae6 (the commit before the bindings descriptor existed), written by the unmodified function from the
// inputs below and the image's init.sql of that commit (testdata/golden/init.sql, a copy). A run with no RL_BINDINGS_FILE must compose these bytes exactly, for every mode
// the runner has: the first five are the ones docs/measurements/2026-11-14-runner-file-access/golden holds (the control plane's measurement of the same function), the
// rest are the task, probe and byte-measuring shapes that measurement did not reproduce.
//
// To regenerate (ONLY from a commit whose composeScript is the one being frozen): RL_GOLDEN_WRITE=1 go test -run TestTheScriptWithNoDescriptorIsTheFrozenOne.
func goldenCases() []struct {
	name    string
	cfg     config
	prelude string
	stmt    string
	kind    stmtKind
} {
	base := config{workDir: "/work", resultFile: "/work/result.parquet", threads: "2", memoryLimit: "1GB", catalogToken: "canary.CATALOG-TOKEN_0123456789"}
	noToken := base
	noToken.catalogToken = ""
	noSettings := base
	noSettings.threads, noSettings.memoryLimit = "", ""
	measured := base
	measured.measureBytes = true
	task := base
	task.task = &taskSpec{}
	task.taskTail = "COPY (SELECT count(*) FROM __rl_result) TO '/work/rowcount.csv' (FORMAT CSV, HEADER false);\n.read /work/tmp/rl_write_apply.sql\n"
	taskNoToken := task
	taskNoToken.catalogToken = ""
	probe := noToken
	probe.probe = &probeSpec{NameKeyHex: "00112233445566778899aabbccddeeff", Inputs: []string{"inp_abcdefgh12", "inp_zyxwvuts98"}}
	probeMeasured := probe
	probeMeasured.measureBytes = true
	return []struct {
		name    string
		cfg     config
		prelude string
		stmt    string
		kind    stmtKind
	}{
		{"query_with_token", base, "", "SELECT 1 AS a", kindQuery},
		{"query_no_token", noToken, "", "SELECT 1 AS a", kindQuery},
		{"describe_with_token", base, "", "DESCRIBE SELECT 1 AS a", kindDescribe},
		{"statement_with_token", base, "", "CREATE TABLE t(a INT)", kindStatement},
		{"query_prelude_token", noSettings, "ATTACH 'x' AS y;\n", "SELECT 1 AS a", kindQuery},
		{"query_prelude_no_trailing_newline", noToken, "SET VARIABLE n = 1;", "SELECT getvariable('n') AS n", kindQuery},
		{"query_measured_with_prelude", measured, "ATTACH 'x' AS y;\n", "SELECT 1 AS a", kindQuery},
		{"statement_measured", measured, "", "CREATE TABLE t(a INT)", kindStatement},
		{"task_with_token", task, "ATTACH 'wh' AS rl_catalog (TYPE iceberg);\n", "SELECT 1 AS id", kindQuery},
		{"task_no_token", taskNoToken, "", "SELECT 1 AS id", kindQuery},
		{"probe_no_token", probe, "CREATE TEMP TABLE inp_abcdefgh12 AS SELECT 1;\n", "SELECT 1 AS a", kindQuery},
		{"probe_measured", probeMeasured, "", "SELECT 1 AS a", kindQuery},
		{"query_no_settings_no_token", config{workDir: "/work", resultFile: "/work/result.parquet"}, "", "SELECT 1 AS a", kindQuery},
	}
}

func TestTheScriptWithNoDescriptorIsTheFrozenOne(t *testing.T) {
	initSQL, err := os.ReadFile(filepath.Join("testdata", "golden", "init.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range goldenCases() {
		got := composeScript(string(initSQL), c.prelude, c.cfg, "/work/tmp", "/work/rowcount.csv", "/work/bytes_read.csv", c.stmt, c.kind)
		file := filepath.Join("testdata", "golden", c.name+".sql")
		if os.Getenv("RL_GOLDEN_WRITE") == "1" {
			if err := os.WriteFile(file, []byte(got), 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		want, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got != string(want) {
			t.Errorf("%s: a run with no bindings descriptor no longer composes the script it always did.\n--- got ---\n%s\n--- want ---\n%s", c.name, got, want)
		}
	}
}
