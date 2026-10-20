package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// PROBE MODE: A DRY RUN OF ONE STATEMENT ON A SAMPLE, THAT WRITES NOTHING AND REPORTS NUMBERS.
//
// The control plane's change-set gates ask a plane to run an author's statement against a small sample of its inputs and say whether it ran, how many rows
// it produced, how many of each output column were null, and a digest of the output, WITHOUT a row, a value or a name leaving the pod. The agent sets
// RL_PROBE_FILE to a JSON file of its own ConfigMap (probeSpec below) and mints no upload URL; the control plane composes the prelude so that each sampled
// input is a TEMP TABLE named by an alias it chose (a view over a LIMIT would be cut again at every read, so the count and the statement could see
// different rows). A runner built before this mode does not read the variable, runs the statement as a plain query and prints no RL_PROBE line, and the
// agent then answers that the runner cannot probe: it never reads a pass out of a run that measured nothing.
//
// WHAT THE RUNNER DOES DIFFERENTLY: the statement is materialised into the result table as always, and then there is NO result copy and NO upload. In their place the
// engine writes four small files under the work directory (the columns, the counts, the digest, each input's row count), and this runner reads them
// and prints ONE more line before the RL_METRICS line:
//
//	RL_PROBE {"inputs":[{"alias":"inp_...","rows":N}],"columns":[{"name_hash":"<64 hex>","type_family":"string","nulls":N}],"digest_algorithm":"bag_digest_v1","digest":"<64 hex>"}
//
// A COLUMN'S NAME NEVER LEAVES THE POD: it is hashed here, HMAC-SHA-256 of the lower-cased name under the key the control plane minted for this one probe,
// so the control plane (which holds the statement and the key) can put the author's own names back and nothing else can. A type is reported as one word
// of a closed set, and every number is a count.
//
// THE DIGEST bag_digest_v1 is KEYED, ORDER-INDEPENDENT AND CONSTANT-MEMORY. A row is its cells in order, each LENGTH-PREFIXED so no value can imitate another
// encoding: N for a null, and V, the byte length of the cell's text, a colon and the text for anything else, where the text is DuckDB's own
// CAST(cell AS VARCHAR). Each row hashes to SHA-256 of the key's hex, a colon and the row; the four 64-bit words of those hashes are each SUMMED over the rows
// (exactly, in 128 bits, so the sum does not depend on the order) and the digest is the SHA-256 of the text `count:w0:w1:w2:w3`. Permuting the rows changes
// nothing; a different bag, a duplicated row, a value moved to another column all change it; and no row is ever held at once, so a result that the plain query
// streams to a file is as cheap to probe. THE KEY is the per-probe key of the spec: an unkeyed digest of a small or low-entropy result is a guess-and-check oracle
// for its values, and a keyed one is only that for whoever holds the key (the control plane, which holds the statement anyway). So two digests compare only when
// they were made under the same key (the control plane gives every engine of one comparison the same key) and the same algorithm name. It is DuckDB's text
// form, so it compares one DuckDB run with another and NOT yet one engine with another: a cross-engine digest is a later algorithm name.
const (
	probeAlgorithm = "bag_digest_v1"

	maxProbeInputs      = 16
	maxProbeSpecBytes   = 64 << 10
	minProbeKeyHexChars = 32
	maxProbeKeyHexChars = 64

	probeColsFile   = "probe_cols.csv"
	probeCountsFile = "probe_counts.csv"
	probeDigestFile = "probe_digest.csv"
)

// reProbeAlias is the control plane's opaque id grammar for an alias (a short lower-case prefix and 8 to 64 letters and digits). It is also what makes
// an alias safe to put inside double quotes in a statement: nothing in it can end a quoted identifier.
var reProbeAlias = regexp.MustCompile(`^[a-z]{2,8}_[A-Za-z0-9]{8,64}$`)

// probeSpec is the agent's file: how the probe reports. It carries no SQL.
type probeSpec struct {
	// NameKeyHex is the per-probe HMAC key, 16 to 32 bytes as hex.
	NameKeyHex string `json:"name_key_hex"`
	// Inputs are the aliases of the sampled TEMP TABLEs the prelude created, in order.
	Inputs []string `json:"inputs"`

	key []byte
}

// loadProbeSpec reads and holds the spec to its grammar. Any fault is the caller's invalid-query class: a probe the runner cannot understand is refused before the
// engine runs, never run as a query.
func loadProbeSpec(path string) (*probeSpec, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("probe spec: %w", err)
	}
	defer f.Close()
	buf, err := io.ReadAll(io.LimitReader(f, maxProbeSpecBytes+1))
	if err != nil {
		return nil, fmt.Errorf("probe spec: %w", err)
	}
	if len(buf) > maxProbeSpecBytes {
		return nil, fmt.Errorf("probe spec exceeds %d bytes", maxProbeSpecBytes)
	}
	var s probeSpec
	if err := json.Unmarshal(buf, &s); err != nil {
		return nil, fmt.Errorf("probe spec is not JSON: %w", err)
	}
	if l := len(s.NameKeyHex); l < minProbeKeyHexChars || l > maxProbeKeyHexChars || l%2 != 0 {
		return nil, errors.New("probe spec: name_key_hex is not 16 to 32 bytes of hex")
	}
	key, err := hex.DecodeString(s.NameKeyHex)
	if err != nil {
		return nil, errors.New("probe spec: name_key_hex is not hex")
	}
	s.key = key
	if len(s.Inputs) > maxProbeInputs {
		return nil, fmt.Errorf("probe spec: more than %d inputs", maxProbeInputs)
	}
	seen := map[string]bool{}
	for _, a := range s.Inputs {
		if !reProbeAlias.MatchString(a) {
			return nil, errors.New("probe spec: an input alias is not an opaque id")
		}
		if seen[a] {
			return nil, errors.New("probe spec: an input alias twice")
		}
		seen[a] = true
	}
	return &s, nil
}

// probeSQL is the script's tail in probe mode, in place of the result copy and the row count. Like copyResultSQL it asks duckdb_columns() for the result's
// own schema (and ONLY the temp table's: database_name = 'temp'), writes one generated statement per question to a file, and `.read`s it, so the whole probe
// is ONE engine process and its CPU and memory are the one the runner reports.
//
// WHY THIS IS NOT AN INJECTION SEAM, as copyResultSQL says: every identifier is emitted inside double quotes with `"` doubled, so a column named with a
// newline, a semicolon or a quote stays inside the quoted identifier; the paths are the runner's own, under its work directory; the aliases passed
// reProbeAlias. The customer's statement cannot reach `.read`: it sits inside CREATE TEMP TABLE ... AS, and loadQuery refuses a leading '.'.
func probeSQL(workDir, tmpDir, keyHex string, inputs []string) string {
	var b strings.Builder
	cols := filepath.Join(workDir, probeColsFile)
	counts := filepath.Join(workDir, probeCountsFile)
	digest := filepath.Join(workDir, probeDigestFile)

	// The result's columns, in output order: index, name, type. The runner (not the engine) hashes the names. The name travels as hex, so no byte of it (a
	// carriage return inside a quoted CSV field is rewritten by the reader) can change what is hashed.
	fmt.Fprintf(&b, "COPY (SELECT column_index, hex(column_name), data_type FROM duckdb_columns() WHERE database_name = 'temp' AND table_name = '%s' ORDER BY column_index) TO %s (FORMAT CSV, HEADER false);\n",
		resultTable, sqlString(cols))

	// How many rows, and how many of each column are null: one row, count first, then a column's nulls in output order.
	genCounts := filepath.Join(tmpDir, "rl_probe_counts.sql")
	fmt.Fprintf(&b, `COPY (
  SELECT 'COPY (SELECT count(*)' || string_agg(', count(*) - count("' || replace(column_name, '"', '""') || '")', '' ORDER BY column_index)
    || ' FROM %s) TO %s (FORMAT CSV, HEADER false);'
  FROM duckdb_columns() WHERE database_name = 'temp' AND table_name = '%s'
) TO %s (FORMAT CSV, HEADER false, QUOTE '', DELIMITER E'\x01');
`, resultTable, nestedSQLString(counts), resultTable, sqlString(genCounts))
	fmt.Fprintf(&b, ".read %s\n", genCounts)

	// The digest of the bag (bag_digest_v1, above): a keyed hash per row, its four 64-bit words summed exactly, one SHA-256 of the totals. The cells are joined
	// by one flat concat (a chain of || is one level of expression depth per column and the engine refuses a thousand), and no row is held at once.
	genDigest := filepath.Join(tmpDir, "rl_probe_digest.sql")
	word := func(from int) string {
		return fmt.Sprintf("coalesce(sum((''0x'' || substr(h, %d, 16))::UBIGINT::HUGEINT), 0)::VARCHAR", from)
	}
	fmt.Fprintf(&b, `COPY (
  SELECT 'COPY (SELECT sha256(count(*)::VARCHAR || '':'' || %s || '':'' || %s || '':'' || %s || '':'' || %s) FROM (SELECT sha256(''%s:'' || concat(' || string_agg(
      '(CASE WHEN "' || replace(column_name, '"', '""') || '" IS NULL THEN ''N'' ELSE ''V'' || strlen(CAST("' || replace(column_name, '"', '""')
      || '" AS VARCHAR)) || '':'' || CAST("' || replace(column_name, '"', '""') || '" AS VARCHAR) END)', ', ' ORDER BY column_index)
    || ')) AS h FROM %s)) TO %s (FORMAT CSV, HEADER false);'
  FROM duckdb_columns() WHERE database_name = 'temp' AND table_name = '%s'
) TO %s (FORMAT CSV, HEADER false, QUOTE '', DELIMITER E'\x01');
`, word(1), word(17), word(33), word(49), keyHex, resultTable, nestedSQLString(digest), resultTable, sqlString(genDigest))
	fmt.Fprintf(&b, ".read %s\n", genDigest)

	// Each sampled input's row count, once sampled.
	for i, alias := range inputs {
		fmt.Fprintf(&b, "COPY (SELECT count(*) FROM \"%s\") TO %s (FORMAT CSV, HEADER false);\n", alias, sqlString(probeInputFile(workDir, i)))
	}
	return b.String()
}

func probeInputFile(workDir string, i int) string {
	return filepath.Join(workDir, "probe_in_"+strconv.Itoa(i)+".csv")
}

// probeFacts is the RL_PROBE line.
type probeFacts struct {
	Inputs          []probeInput  `json:"inputs"`
	Columns         []probeColumn `json:"columns"`
	DigestAlgorithm string        `json:"digest_algorithm"`
	Digest          string        `json:"digest"`
}

type probeInput struct {
	Alias string `json:"alias"`
	Rows  int64  `json:"rows"`
}

type probeColumn struct {
	NameHash   string `json:"name_hash"`
	TypeFamily string `json:"type_family"`
	Nulls      int64  `json:"nulls"`
}

var reHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// readProbe turns the engine's four files into the facts and the row count. Anything that does not add up is an error (the caller's runner_io_error): a count
// that is negative, a null count above the row count, a digest that is not 64 hex digits, a file with the wrong number of fields.
func readProbe(workDir string, spec *probeSpec) (*probeFacts, int64, error) {
	rows := func(name string) ([][]string, error) {
		f, err := os.Open(filepath.Join(workDir, name))
		if err != nil {
			return nil, err
		}
		defer f.Close()
		r := csv.NewReader(f)
		r.FieldsPerRecord = -1
		return r.ReadAll()
	}
	colRows, err := rows(probeColsFile)
	if err != nil {
		return nil, 0, fmt.Errorf("probe columns: %w", err)
	}
	countRows, err := rows(probeCountsFile)
	if err != nil {
		return nil, 0, fmt.Errorf("probe counts: %w", err)
	}
	if len(countRows) != 1 || len(countRows[0]) != 1+len(colRows) || len(colRows) == 0 {
		return nil, 0, errors.New("probe counts: not one row of a count and a null count for each column")
	}
	n, err := strconv.ParseInt(countRows[0][0], 10, 64)
	if err != nil || n < 0 {
		return nil, 0, errors.New("probe counts: the row count is not a count")
	}
	facts := &probeFacts{Inputs: []probeInput{}, Columns: []probeColumn{}, DigestAlgorithm: probeAlgorithm}
	for i, c := range colRows {
		if len(c) != 3 {
			return nil, 0, errors.New("probe columns: a column is not an index, a name and a type")
		}
		nulls, err := strconv.ParseInt(countRows[0][1+i], 10, 64)
		if err != nil || nulls < 0 || nulls > n {
			return nil, 0, errors.New("probe counts: a null count is not between 0 and the row count")
		}
		name, err := hex.DecodeString(c[1])
		if err != nil {
			return nil, 0, errors.New("probe columns: a column name is not hex")
		}
		mac := hmac.New(sha256.New, spec.key)
		mac.Write([]byte(strings.ToLower(string(name))))
		facts.Columns = append(facts.Columns, probeColumn{NameHash: hex.EncodeToString(mac.Sum(nil)), TypeFamily: typeFamily(c[2]), Nulls: nulls})
	}
	dRows, err := rows(probeDigestFile)
	if err != nil {
		return nil, 0, fmt.Errorf("probe digest: %w", err)
	}
	if len(dRows) != 1 || len(dRows[0]) != 1 || !reHex64.MatchString(dRows[0][0]) {
		return nil, 0, errors.New("probe digest: not one 64-digit hex digest")
	}
	facts.Digest = dRows[0][0]
	for i, alias := range spec.Inputs {
		iRows, err := rows(filepath.Base(probeInputFile(workDir, i)))
		if err != nil {
			return nil, 0, fmt.Errorf("probe input: %w", err)
		}
		if len(iRows) != 1 || len(iRows[0]) != 1 {
			return nil, 0, errors.New("probe input: not one count")
		}
		r, err := strconv.ParseInt(iRows[0][0], 10, 64)
		if err != nil || r < 0 {
			return nil, 0, errors.New("probe input: not a count")
		}
		facts.Inputs = append(facts.Inputs, probeInput{Alias: alias, Rows: r})
	}
	return facts, n, nil
}

// typeFamily maps DuckDB's own type name to the closed set of families the control plane's dialect service states (boolean, integer, long, float, double,
// decimal, string, date, time, timestamp, timestamptz, binary, uuid), and "other" for anything it has no family for: an unsigned or 128-bit integer, a
// nested type, an interval. An exotic column is a count and a hash and never a refusal.
func typeFamily(dataType string) string {
	t := strings.ToUpper(strings.TrimSpace(dataType))
	switch {
	case t == "BOOLEAN":
		return "boolean"
	case t == "TINYINT" || t == "SMALLINT" || t == "INTEGER":
		return "integer"
	case t == "BIGINT":
		return "long"
	case t == "FLOAT":
		return "float"
	case t == "DOUBLE":
		return "double"
	case t == "DECIMAL" || (strings.HasPrefix(t, "DECIMAL(") && strings.HasSuffix(t, ")")):
		return "decimal"
	case t == "VARCHAR":
		return "string"
	case t == "DATE":
		return "date"
	case t == "TIME":
		return "time"
	case t == "TIMESTAMP" || t == "TIMESTAMP_S" || t == "TIMESTAMP_MS" || t == "TIMESTAMP_NS":
		return "timestamp"
	case t == "TIMESTAMP WITH TIME ZONE":
		return "timestamptz"
	case t == "BLOB":
		return "binary"
	case t == "UUID":
		return "uuid"
	}
	return "other"
}
