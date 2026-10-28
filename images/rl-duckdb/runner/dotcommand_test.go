package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A STATEMENT MAY NOT REACH THE DUCKDB CLI'S DOT-COMMANDS, ON THE REAL ENGINE (tracker 8.2.40; found 2026-10-28 from E04-Backend's measurement that `SET lock_configuration=true`
// does not stop `.shell`). The CLI takes a line that begins with `.` as a command when the statement before it is complete, and `.shell` runs a program in the runner's pod with the
// uid that reads /run/rl/secrets. The runner feeds the engine one script on stdin, so the only thing between a statement and a dot-command is that the runner and the CLI agree
// where the statement ENDS. They agree on strings, identifiers, dollar quoting and nested comments (measured on 1.5.5), and they did NOT agree on a backslash in an E string:
// DuckDB's parser reads E'\'' as one string with an escaped quote, and the CLI's own scanner, which has no backslash escape, closes it at the backslash and sees the `;` after it as
// the end of the statement, so a `.shell` line on the next line ran. The runner's lexer is DuckDB's, and the CLI's is not, so the rule here is one the CLI cannot read differently:
// no line of a statement may begin with a dot. It refuses some statements that would have been data (a line inside a multi-line string that starts with a dot), which is the cost.
//
// These run the pinned CLI, as hugeint_test.go does, and fail in CI when it is missing.

// dotShape is one hostile statement; {M} is the path a shell command would create.
type dotShape struct{ name, sql string }

func dotShapes() []dotShape {
	return []dotShape{
		{"E string with an escaped quote, then a dot line", "SELECT E'\\'; \n.shell touch {M}\n' AS a"},
		{"E string with an escaped quote, a comment closes it", "SELECT E'\\';\n.shell touch {M}\n--' AS a"},
		{"E string, the escape before a semicolon at the end of a line", "SELECT E'\\'\n;\n.shell touch {M}\n' AS a"},
		{"a dot line inside a block comment", "SELECT 1 /* a\n.shell touch {M}\n*/"},
		{"a dot line inside a nested block comment", "SELECT 1 /* a /* b */ ;\n.shell touch {M}\n*/"},
		{"a dot line inside a string", "SELECT 'a\n.shell touch {M}\n' AS a"},
		{"a dot line inside a dollar-quoted string", "SELECT $$a;\n.shell touch {M}\n$$ AS a"},
		{"a dot line inside a quoted identifier", "SELECT 1 AS \"a;\n.shell touch {M}\n\""},
		{"a dot line after a line comment", "SELECT 1 -- c\n.shell touch {M}"},
		{"the .system alias", "SELECT E'\\';\n.system touch {M}\n' AS a"},
		{"the .sh alias", "SELECT E'\\';\n.sh touch {M}\n' AS a"},
		{"a CRLF line end", "SELECT E'\\';\r\n.shell touch {M}\r\n' AS a"},
		{"a dot line in a DESCRIBE", "DESCRIBE SELECT E'\\';\n.shell touch {M}\n' AS a"},
		{"a dot line in a statement that is run as it is", "SET VARIABLE v = E'\\';\n.shell touch {M}\n'"},
		// The four disagreements E02-Backend's reviewer found between the CLI's scanner and DuckDB's lexer (2026-10-28): the first is the E string above. The CLI cuts the
		// statement at a `;` inside an unclosed parenthesis and closes a block comment at the first `*/` however deeply it nests; a line comment ended by a bare CR is the fourth.
		{"a semicolon inside an unclosed parenthesis", "SELECT (1;\n.shell touch {M}\n)"},
		{"a block comment nested in a block comment, closed at the first */", "SELECT 1 /* a /* b */ ;\n.shell touch {M}\n*/"},
		{"a line comment ended by a bare CR, then a semicolon", "SELECT 1 -- c\r;\n.shell touch {M}\n"},
		{"a line comment ended by a bare CR, then the dot line", "SELECT 1 -- c\r.shell touch {M}\n"},
	}
}

func TestNoShapeOfStatementRunsADotCommandOnTheRealEngine(t *testing.T) {
	bin := duckdbBinary(t)
	for _, s := range dotShapes() {
		t.Run(s.name, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "marker")
			init := filepath.Join(t.TempDir(), "init.sql")
			if err := os.WriteFile(init, []byte("SET autoinstall_known_extensions=false;\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			h := newHarness(t, strings.ReplaceAll(s.sql, "{M}", marker), map[string]string{"RL_DUCKDB_BIN": bin, "RL_INIT_FILE": init})
			m, code, _ := h.exec()
			t.Logf("status %s class %q code %d", m.Status, m.ErrorClass, code)
			if _, err := os.Stat(marker); err == nil {
				t.Fatalf("a dot-command ran: the shell command created %s (status %s class %q code %d)\nscript:\n%s", marker, m.Status, m.ErrorClass, code, h.errorLog())
			}
		})
	}
}

// THE RULE ITSELF, WITHOUT THE ENGINE: every shape is refused by refuseDotLines whatever the engine would have done with it. The test above holds the outcome (nothing ran);
// this one holds the reason it is not an accident of -bail, so removing the rule fails here and removing -bail does not make the first test the only guard.
func TestEveryShapeWithADotLineIsRefusedBeforeTheEngineStarts(t *testing.T) {
	for _, s := range dotShapes() {
		t.Run(s.name, func(t *testing.T) {
			sql := strings.ReplaceAll(s.sql, "{M}", "/tmp/x")
			stmt, err := singleStatement(sql)
			if err != nil {
				return // refused earlier still: more than one statement, or none
			}
			if _, _, err := loadQuery(writeQuery(t, sql)); err == nil {
				t.Fatalf("loadQuery accepted a statement with a dot line:\n%s\n(singleStatement kept %q)", sql, stmt)
			}
		})
	}
}

func writeQuery(t *testing.T, sql string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "query.sql")
	if err := os.WriteFile(p, []byte(sql), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// WHAT IS STILL ACCEPTED: a number that begins with a dot, a statement over many lines, and a dot that is not at the start of a line.
func TestADotThatIsNotACommandIsStillAccepted(t *testing.T) {
	for name, sql := range map[string]string{
		"a number .5 at the start of a line": "SELECT 1 +\n.5 AS x",
		"a number .25 after CRLF":            "SELECT 1 +\r\n.25 AS x",
		"a dot inside a line":                "SELECT 1 AS x,\n  s.a, t.b\nFROM s JOIN t ON s.id = t.id",
		"an indented dot-like line":          "SELECT 'a\n  .shell id\n' AS x",
		"a dotted name":                      "SELECT a.b FROM c.d.e",
		"a float":                            "SELECT 1.5, .5e3",
		"a # at a line start is a positional reference, not a command (measured: the CLI takes no such line as one)": "SELECT\n#1 FROM (SELECT 1 AS x)",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := loadQuery(writeQuery(t, sql)); err != nil {
				t.Errorf("refused a statement with no dot command in it: %v\n%s", err, sql)
			}
		})
	}
}

// THE CONTROL, WHICH SHOWS THE FIRST TEST CAN FAIL AND WHERE THE DISAGREEMENT IS: the same E-string statement fed to the bare CLI with no -bail. DuckDB reads it as one string; the
// CLI's scanner closes the string at the backslash, runs the cut-off statement (a syntax error), carries on, and runs the `.shell` line that follows. If this stops creating the file,
// a new engine has changed what the CLI scans and the rule above should be looked at again.
func TestTheBareCLIRunsTheDotLineAfterAnEStringTheRunnerReadsAsOneStatement(t *testing.T) {
	bin := duckdbBinary(t)
	work := t.TempDir()
	marker := filepath.Join(work, "marker")
	script := "CREATE TEMP TABLE r AS\nSELECT E'\\'; \n.shell touch " + marker + "\n' AS a\n;\n"
	if _, err := singleStatement("SELECT E'\\'; \n.shell touch " + marker + "\n' AS a"); err != nil {
		t.Fatalf("the runner's lexer should read this as one statement: %v", err)
	}
	cmd := exec.Command(bin, "-batch", "-no-init", "-csv", "-noheader")
	cmd.Dir = work
	cmd.Env = []string{"HOME=" + work, "TMPDIR=" + work}
	cmd.Stdin = strings.NewReader(script)
	_, _ = cmd.CombinedOutput() // the cut-off statement fails, and that is expected
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the bare CLI no longer runs the dot line after the cut-off statement (%v): the differential is gone or the control is wrong", err)
	}
}

// WHICH OF THE SHAPES THE BARE CLI RUNS, AND THAT `-c` RUNS NONE (measured 2026-10-28 on 1.5.5, as depth and not as the runner's way). The runner feeds the engine one script on stdin;
// `duckdb -c "<sql>"` hands the whole string to the SQL parser, where a line starting with a dot is a syntax error and never a command, even after a real statement boundary.
// The runner cannot become one `-c` without a redesign (its own steps are dot-commands: `.bail`, `.read`), but a statement could be its own `-c` argument. This test records
// both facts so a new engine that changes either is seen: the shapes that run on stdin without -bail, and none that runs as `-c`.
func TestOnlyStdinRunsADotLineAndDashCNeverDoes(t *testing.T) {
	bin := duckdbBinary(t)
	run := func(args []string, stdin, work string) {
		cmd := exec.Command(bin, append([]string{"-batch", "-no-init", "-csv", "-noheader"}, args...)...)
		cmd.Dir = work
		cmd.Env = []string{"HOME=" + work, "TMPDIR=" + work}
		if stdin != "" {
			cmd.Stdin = strings.NewReader(stdin)
		}
		_, _ = cmd.CombinedOutput() // a statement the CLI cut off fails, which is expected
	}
	// The ones that ran on stdin when this was measured. A4's two shapes did not (a bare CR does not end a comment for the CLI on 1.5.5); the runner refuses them anyway.
	runsOnStdin := map[string]bool{
		"E string with an escaped quote, then a dot line":                   true,
		"a semicolon inside an unclosed parenthesis":                        true,
		"a block comment nested in a block comment, closed at the first */": true,
	}
	for _, s := range dotShapes() {
		t.Run(s.name, func(t *testing.T) {
			work := t.TempDir()
			sql := strings.ReplaceAll(s.sql, "{M}", filepath.Join(work, "marker"))
			run(nil, "CREATE TEMP TABLE r AS\n"+sql+"\n;\n", work)
			_, ran := os.Stat(filepath.Join(work, "marker"))
			if want := runsOnStdin[s.name]; want && ran != nil {
				t.Errorf("stdin, no -bail: the dot line no longer runs for this shape (%v): the differential moved, look at the rule again", ran)
			}
			work2 := t.TempDir()
			sql2 := strings.ReplaceAll(s.sql, "{M}", filepath.Join(work2, "marker"))
			run([]string{"-c", "CREATE TEMP TABLE r AS\n" + sql2 + "\n;\n"}, "", work2)
			if _, err := os.Stat(filepath.Join(work2, "marker")); err == nil {
				t.Errorf("-c ran a dot line: the premise of using it as depth is gone")
			}
		})
	}
}
