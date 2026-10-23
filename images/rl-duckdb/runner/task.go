package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// TASK MODE: ONE WRITE OF A PIPELINE TASK, COMPOSED HERE FROM A SPEC FILE, AND A CLAIM ABOUT WHAT IT DID.
//
// The agent sets RL_TASK_FILE to a JSON file of its own ConfigMap (taskSpec below): the task's STRUCTURED INTENT, with no SQL in it. query.sql stays exactly one SELECT. The
// agent composes no DML and the control plane splices no value into one (docs/pipeline-runs.md section 10.1): this runner turns the intent into the write, and refuses what it
// cannot compose. A runner built before this mode does not read the variable, runs query.sql as a plain query (which cannot write) and prints no RL_TASK line, and the agent then
// answers task_runner_unsupported: it never reads a pass out of a run that wrote nothing.
//
// THE WRITE. The SELECT is materialised into __rl_result as for any query, and then the runner's own script writes it to rl_catalog."<namespace levels joined with dots>"."<name>"
// (the alias the agent's ATTACH gives the project's warehouse; the quoting rule is E06-BE's measured one, one quoted identifier for all the levels):
//
//	append              INSERT INTO t SELECT * FROM __rl_result                        one statement, auto-committed (measured: m810 P1)
//	overwrite           BEGIN; DELETE FROM t; INSERT INTO t SELECT * ...; COMMIT       one commit, two snapshots, delete then append (m810 P2c)
//	partition_overwrite BEGIN; DELETE FROM t WHERE <the bounds>; INSERT ...; COMMIT    the same shape for one partition (m810 P4b)
//	merge               MERGE INTO t USING (...) AS s USING (<key>) WHEN MATCHED THEN UPDATE WHEN NOT MATCHED THEN INSERT    one statement (m810 P3d)
//
// NOTHING IS WRITTEN WHEN THERE IS NOTHING TO WRITE: an append or a merge of an empty result runs no statement and claims no_change (an engine's answer to an empty INSERT was not
// measured, and a claim of no_change over a snapshot it made, or of committed over none, is one the agent cannot reconcile). An overwrite of an empty result still deletes.
//
// THE CHECKS THAT RUN BEFORE ANYTHING IS WRITTEN, because the engine enforces none of them: a merge's source holds each key once and none of its keys is NULL (the measured engine refuses
// a duplicate only for a key already in the table, and lands a duplicate NEW key twice, m810 P3), and a partition overwrite's result holds no row outside the partition it replaces (a row
// outside would not be deleted by a retry, and the retry would write it twice). A refusal is an ordinary engine error before the first write: not_committed.
//
// THE CLAIM, and only what the runner knows. Directly before the RL_METRICS line it prints
//
//	RL_TASK {"v":1,"attempt":"<attempt id>","outcome":"committed","base_snapshot_id":"123","committed_snapshot_id":"0","files_written":0,"bytes_written":0}
//
// The outcome is the word of the script's progress: committed (the COMMIT, or the one statement, returned), no_change (nothing was to be written and nothing was), not_committed (the
// engine failed before any write statement ran, or inside the transaction before COMMIT was sent) or unknown (it failed after a write statement or after COMMIT was sent, or it was
// killed, which loses the sentinels that say how far it got). THE RUNNER DOES NOT NAME THE SNAPSHOT IT COMMITTED OR COUNT FILES AND BYTES: DuckDB gives it neither in a form it
// can trust (docs/measurements/2026-09-29-duckdb-1.5.5-iceberg-writes.md, Findings 5 and 7), and the agent, which reads the table after the Job ends, decides what was written
// from the catalog and not from this line. "0" is none. The base is the spec's, echoed. The runner does not compare the table's current snapshot with the base before it writes: it
// has no way to read one that was measured, and the agent refuses a moved table at acceptance and proves the outcome after the Job (docs/pipeline-runs.md section 10.3).
//
// THE CATALOG TOKEN. When RL_CATALOG_TOKEN_FILE names a file, the runner creates the iceberg secret from it AFTER the lock and the init sentinel and BEFORE the prelude, which (the agent's
// ATTACH, with SUPPORT_NESTED_NAMESPACES true) then finds the one iceberg secret by default. The token is checked against a conservative alphabet and quoted; it is on the engine's stdin
// and nowhere else: not the prelude, not a file, not an environment variable, not a line of the runner's output.
const (
	taskSpecVersion  = 2
	maxTaskSpecBytes = 64 << 10
	maxTokenBytes    = 8 << 10
	minTokenBytes    = 8

	// catalogAlias is the name the agent's ATTACH gives the project's warehouse (conformance.CatalogAlias): the contract is the alias and the dotted-schema form.
	catalogAlias = "rl_catalog"
	// catalogSecret is the iceberg secret the token makes. One iceberg secret exists, so the ATTACH picks it without naming it (the measured form).
	catalogSecret = "rl_catalog_token"

	// The sentinels the write script prints, so the claim says how far it got.
	writeBeginSentinel     = "RL_WRITE_BEGIN"
	writeStagedSentinel    = "RL_WRITE_STAGED"
	writeCommittedSentinel = "RL_WRITE_COMMITTED"
	writeSkippedSentinel   = "RL_WRITE_SKIPPED"

	// The generated files, under the work directory's tmp.
	writeApplyScript  = "rl_write_apply.sql"
	writeSkipScript   = "rl_write_skip.sql"
	writeChooseScript = "rl_write_choose.sql"

	maxTaskNameBytes = 255
	maxTaskTextBytes = 1024
)

// taskModes is the closed set of writes this runner composes.
var taskModes = map[string]bool{"append": true, "overwrite": true, "partition_overwrite": true, "merge": true}

var (
	reTaskAttempt  = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)
	reTaskBase     = regexp.MustCompile(`^[0-9]{1,19}$`)
	reTaskInteger  = regexp.MustCompile(`^-?[0-9]{1,18}$`)
	reTaskDate     = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
	reCatalogToken = regexp.MustCompile(`^[A-Za-z0-9._~+/=-]+$`)
)

// taskSpec is the agent's file (internal/agent/duckdb's taskSpec, version 2). It names the customer's table and values: it is held to the statement's rule and never put in a log
// line, an error or the output.
type taskSpec struct {
	V       int    `json:"v"`
	Attempt string `json:"attempt"`
	Mode    string `json:"mode"`
	Target  struct {
		Warehouse string   `json:"warehouse"`
		Namespace []string `json:"namespace"`
		Name      string   `json:"name"`
	} `json:"target"`
	UniqueKey      []string       `json:"unique_key,omitempty"`
	Partition      *taskPartition `json:"partition,omitempty"`
	BaseSnapshotID string         `json:"base_snapshot_id"`

	base int64
}

// taskPartition is a partition overwrite's slice: the column and its bounds. With an end the slice is [start, end); with none it is the static partition equal to start.
type taskPartition struct {
	Column string     `json:"column"`
	Start  *taskValue `json:"start"`
	End    *taskValue `json:"end,omitempty"`
}

// taskValue is one typed bound: a kind and its text. An integer is text because an int64 does not survive a reader that goes through a float.
type taskValue struct {
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// loadTaskSpec reads the spec and holds it to its grammar. Any fault is the caller's invalid-query class: a task the runner cannot understand is refused before the engine runs,
// never run as a plain query. The error never carries a value from the file.
func loadTaskSpec(path string) (*taskSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("task spec: %w", err)
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, maxTaskSpecBytes+1))
	if err != nil {
		return nil, fmt.Errorf("task spec: %w", err)
	}
	if len(buf) > maxTaskSpecBytes {
		return nil, fmt.Errorf("task spec exceeds %d bytes", maxTaskSpecBytes)
	}
	var s taskSpec
	dec := json.NewDecoder(bytes.NewReader(buf))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return nil, errors.New("task spec is not JSON of the shape this runner reads")
	}
	if dec.More() {
		return nil, errors.New("task spec holds more than one document")
	}
	if err := s.validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *taskSpec) validate() error {
	if s.V != taskSpecVersion {
		return fmt.Errorf("task spec: version %d is not the %d this runner reads", s.V, taskSpecVersion)
	}
	if !reTaskAttempt.MatchString(s.Attempt) {
		return errors.New("task spec: attempt is not an opaque id")
	}
	if !taskModes[s.Mode] {
		return errors.New("task spec: mode is not one this runner composes")
	}
	if !reTaskBase.MatchString(s.BaseSnapshotID) {
		return errors.New("task spec: base_snapshot_id is not a decimal snapshot id")
	}
	base, err := strconv.ParseInt(s.BaseSnapshotID, 10, 64)
	if err != nil {
		return errors.New("task spec: base_snapshot_id is not a snapshot id")
	}
	s.base = base
	if len(s.Target.Namespace) == 0 {
		return errors.New("task spec: the target has no namespace")
	}
	for _, level := range s.Target.Namespace {
		// A level holding a dot would be read as two levels by the form the catalog's schema is named in.
		if !okName(level) || strings.Contains(level, ".") {
			return errors.New("task spec: a namespace level is not a name")
		}
	}
	if !okName(s.Target.Name) {
		return errors.New("task spec: the target's name is not a name")
	}
	switch s.Mode {
	case "merge":
		if len(s.UniqueKey) == 0 {
			return errors.New("task spec: a merge names the key it matches on")
		}
		seen := map[string]bool{}
		for _, k := range s.UniqueKey {
			if !okName(k) {
				return errors.New("task spec: a key column is not a name")
			}
			// DuckDB matches identifiers case-insensitively, so two spellings are one column.
			if seen[strings.ToLower(k)] {
				return errors.New("task spec: a key column is named twice")
			}
			seen[strings.ToLower(k)] = true
		}
	}
	if s.Mode == "partition_overwrite" {
		if err := s.Partition.validate(); err != nil {
			return err
		}
	} else if s.Partition != nil {
		return errors.New("task spec: only a partition overwrite names a partition")
	}
	return nil
}

func (p *taskPartition) validate() error {
	if p == nil || p.Start == nil {
		return errors.New("task spec: a partition overwrite names its partition")
	}
	if !okName(p.Column) {
		return errors.New("task spec: the partition column is not a name")
	}
	if err := p.Start.validate(); err != nil {
		return err
	}
	if p.End != nil {
		if err := p.End.validate(); err != nil {
			return err
		}
		if p.End.Kind != p.Start.Kind || p.Start.Kind == "text" {
			return errors.New("task spec: a partition's bounds are of one ordered kind")
		}
	}
	return nil
}

func (v *taskValue) validate() error {
	switch v.Kind {
	case "integer":
		if !reTaskInteger.MatchString(v.Value) {
			return errors.New("task spec: an integer bound is not an integer")
		}
	case "date":
		if _, err := time.Parse("2006-01-02", v.Value); err != nil || !reTaskDate.MatchString(v.Value) {
			return errors.New("task spec: a date bound is not a date")
		}
	case "instant":
		if _, err := time.Parse(time.RFC3339Nano, v.Value); err != nil {
			return errors.New("task spec: an instant bound is not an RFC 3339 instant")
		}
	case "text":
		if !okText(v.Value, maxTaskTextBytes) {
			return errors.New("task spec: a text bound is not text this runner accepts")
		}
	default:
		return errors.New("task spec: a bound is of a kind this runner does not know")
	}
	return nil
}

// okName is a name the runner puts inside double quotes: valid UTF-8, no control character (a newline could not end the identifier, but nothing should ride one), not empty.
func okName(s string) bool { return s != "" && okText(s, maxTaskNameBytes) }

func okText(s string, limit int) bool {
	if len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// quoteIdent is the one rule for an identifier: double-quoted, with the quote doubled. It is the only thing that can end a quoted identifier.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// targetSQL is the table as the agent's ATTACH names it: the alias, the namespace levels joined with dots as ONE schema, the table.
func (s *taskSpec) targetSQL() string {
	return catalogAlias + "." + quoteIdent(strings.Join(s.Target.Namespace, ".")) + "." + quoteIdent(s.Target.Name)
}

// literal is a bound as a SQL literal of its kind. Every kind was held to its grammar, so nothing but a text value needs quoting.
func (v *taskValue) literal() string {
	switch v.Kind {
	case "integer":
		return v.Value
	case "date":
		return "DATE " + sqlString(v.Value)
	case "instant":
		t, _ := time.Parse(time.RFC3339Nano, v.Value)
		// The session's time zone is UTC (composeScript), so a TIMESTAMP column compares at the instant.
		return "TIMESTAMPTZ " + sqlString(t.UTC().Format("2006-01-02 15:04:05.999999999+00"))
	}
	return sqlString(v.Value)
}

// predicate is the partition's own condition on the column: [start, end) with an end, equality without one.
func (p *taskPartition) predicate() string {
	col := quoteIdent(p.Column)
	if p.End == nil {
		return col + " = " + p.Start.literal()
	}
	return col + " >= " + p.Start.literal() + " AND " + col + " < " + p.End.literal()
}

// transactional says the write is an explicit transaction, so a failure before COMMIT was sent is a write that did not happen.
func (s *taskSpec) transactional() bool {
	return s.Mode == "overwrite" || s.Mode == "partition_overwrite"
}

// skippable says an empty result writes nothing: an append or a merge of no rows. An overwrite of none still deletes.
func (s *taskSpec) skippable() bool { return s.Mode == "append" || s.Mode == "merge" }

// writeSQL is the script `.read` runs when there is something to write: the checks, then the sentinels around the write itself.
func (s *taskSpec) writeSQL() string {
	var b strings.Builder
	t := s.targetSQL()
	switch s.Mode {
	case "merge":
		keys := make([]string, len(s.UniqueKey))
		for i, k := range s.UniqueKey {
			keys[i] = quoteIdent(k)
		}
		list := strings.Join(keys, ", ")
		var nulls []string
		for _, k := range keys {
			nulls = append(nulls, k+" IS NULL")
		}
		// error() runs only for a row that passes the WHERE: no row, no error.
		fmt.Fprintf(&b, "SELECT error('the merge source holds a NULL key') WHERE EXISTS (SELECT 1 FROM %s WHERE %s);\n", resultTable, strings.Join(nulls, " OR "))
		fmt.Fprintf(&b, "SELECT error('the merge source holds a key twice') WHERE EXISTS (SELECT 1 FROM %s GROUP BY %s HAVING count(*) > 1);\n", resultTable, list)
		fmt.Fprintf(&b, "SELECT '%s';\n", writeBeginSentinel)
		fmt.Fprintf(&b, "MERGE INTO %s USING (SELECT * FROM %s) AS s USING (%s) WHEN MATCHED THEN UPDATE WHEN NOT MATCHED THEN INSERT;\n", t, resultTable, list)
	case "append":
		fmt.Fprintf(&b, "SELECT '%s';\n", writeBeginSentinel)
		fmt.Fprintf(&b, "INSERT INTO %s SELECT * FROM %s;\n", t, resultTable)
	case "overwrite":
		fmt.Fprintf(&b, "SELECT '%s';\nBEGIN;\nDELETE FROM %s;\nINSERT INTO %s SELECT * FROM %s;\nSELECT '%s';\nCOMMIT;\n", writeBeginSentinel, t, t, resultTable, writeStagedSentinel)
	case "partition_overwrite":
		pred := s.Partition.predicate()
		fmt.Fprintf(&b, "SELECT error('the result holds a row outside the partition') WHERE EXISTS (SELECT 1 FROM %s WHERE (%s) IS NOT TRUE);\n", resultTable, pred)
		fmt.Fprintf(&b, "SELECT '%s';\nBEGIN;\nDELETE FROM %s WHERE %s;\nINSERT INTO %s SELECT * FROM %s;\nSELECT '%s';\nCOMMIT;\n", writeBeginSentinel, t, pred, t, resultTable, writeStagedSentinel)
	}
	fmt.Fprintf(&b, "SELECT '%s';\n", writeCommittedSentinel)
	return b.String()
}

// writeFiles writes the two scripts the engine `.read`s and returns the tail of the main script: the row count, then the choice between them. The files are the runner's own, under
// the work directory, and the main script names them by path.
func (s *taskSpec) writeFiles(tmpDir, countFile string) (tail string, err error) {
	apply := filepath.Join(tmpDir, writeApplyScript)
	if err := os.WriteFile(apply, []byte(s.writeSQL()), 0o600); err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "COPY (SELECT count(*) FROM %s) TO %s (FORMAT CSV, HEADER false);\n", resultTable, sqlString(countFile))
	if !s.skippable() {
		fmt.Fprintf(&b, ".read %s\n", apply)
		return b.String(), nil
	}
	skip := filepath.Join(tmpDir, writeSkipScript)
	if err := os.WriteFile(skip, []byte(fmt.Sprintf("SELECT '%s';\n", writeSkippedSentinel)), 0o600); err != nil {
		return "", err
	}
	// THE ENGINE CHOOSES, because only it knows the count: it writes the one line the CLI then reads. A dot-command is read only when the statement buffer is empty, which it is here.
	choose := filepath.Join(tmpDir, writeChooseScript)
	fmt.Fprintf(&b, "COPY (SELECT CASE WHEN count(*) > 0 THEN %s ELSE %s END FROM %s) TO %s (FORMAT CSV, HEADER false, QUOTE '', DELIMITER E'\\x01');\n",
		sqlString(".read "+apply), sqlString(".read "+skip), resultTable, sqlString(choose))
	fmt.Fprintf(&b, ".read %s\n", choose)
	return b.String(), nil
}

// taskLine is the RL_TASK line the runner prints: the claim, in the agent's shape (internal/agent/duckdb's taskJSON).
type taskLine struct {
	V                   int    `json:"v"`
	Attempt             string `json:"attempt"`
	Outcome             string `json:"outcome"`
	BaseSnapshotID      string `json:"base_snapshot_id"`
	CommittedSnapshotID string `json:"committed_snapshot_id"`
	FilesWritten        uint64 `json:"files_written"`
	BytesWritten        uint64 `json:"bytes_written"`
}

// taskOutcome is the word for how far the write script got, from the sentinels the engine printed and how it ended. exited says the engine returned normally (zero or not), and so
// flushed its stdout; a process that was killed or timed out may have lost the sentinels it printed, so it is unknown whatever they say.
func taskOutcome(spec *taskSpec, outPath string, exitedNormally, failed bool) string {
	if !exitedNormally {
		return "unknown"
	}
	switch {
	case sentinelRan(outPath, writeCommittedSentinel):
		return "committed"
	case sentinelRan(outPath, writeSkippedSentinel):
		return "no_change"
	case !failed:
		// The engine returned success and printed no word of the write: not a state this script produces.
		return "unknown"
	case !sentinelRan(outPath, writeBeginSentinel):
		return "not_committed"
	case spec.transactional() && !sentinelRan(outPath, writeStagedSentinel):
		return "not_committed"
	}
	return "unknown"
}

// readCatalogToken reads the token file and holds it to its alphabet: a JWT is base64url and dots, and nothing else is a token this runner will put in a statement. The error never
// carries the token.
func readCatalogToken(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("catalog token file: %w", err)
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, maxTokenBytes+1))
	if err != nil {
		return "", fmt.Errorf("catalog token file: %w", err)
	}
	if len(buf) > maxTokenBytes {
		return "", fmt.Errorf("catalog token file exceeds %d bytes", maxTokenBytes)
	}
	tok := strings.TrimSpace(string(buf))
	if len(tok) < minTokenBytes || !reCatalogToken.MatchString(tok) {
		return "", errors.New("catalog token file does not hold a token of the shape this runner accepts")
	}
	return tok, nil
}

// secretSQL is the statement that gives the engine the token: the iceberg secret the agent's ATTACH finds by default.
func secretSQL(token string) string {
	return fmt.Sprintf("CREATE SECRET %s (TYPE iceberg, TOKEN %s);\n", catalogSecret, sqlString(token))
}
