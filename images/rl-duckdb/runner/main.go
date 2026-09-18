// Command rl-duckdb-runner is the ENTRYPOINT of the rl-duckdb image.
//
// It runs exactly one SQL statement through the DuckDB CLI inside a single-use sandbox
// pod and prints exactly one line to stdout:
//
//	RL_METRICS {"status":"succeeded","rows_out":1,...}
//
// Nothing else reaches stdout or stderr. Everything the engine says goes to
// <RL_WORK_DIR>/error.log, because a DuckDB error message quotes the SQL and can quote
// row values, and the pod's log stream is not a place for either. The contract with the
// agent is in README.md at the repository root; the agent implements the other side.
//
// Exit codes: 0 succeeded, 1 failed, 2 killed at RL_TIMEOUT_SECONDS.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// version is stamped by the Dockerfile with -X main.version.
var version = "dev"

const (
	// maxQueryBytes is the largest query file the runner will read. The agent sends one
	// statement; a megabyte of SQL is not that.
	maxQueryBytes = 1 << 20

	// resultTable is the temp table the statement is materialised into before it is
	// copied to Parquet. Reserved: a statement that names it collides with itself.
	resultTable = "__rl_result"

	// initSentinel is printed by the script after every configuration and LOAD and
	// before the statement. Its absence on failure means the engine, not the SQL, broke.
	initSentinel = "RL_INIT_OK"

	// The closed set of error classes. The agent switches on these; add one only with
	// the agent.
	classSQL          = "sql_error"
	classTimeout      = "query_timeout"
	classOOM          = "out_of_memory"
	classRunnerIO     = "runner_io_error"
	classInvalidQuery = "invalid_query_file"

	// THE FINER CLASSES THE AGENT HAS ALWAYS ACCEPTED AND NOTHING EVER SENT.
	//
	// internal/agent/duckdb's RunnerClasses admits all of these and its own comment says
	// "the rest of the vocabulary a runner may adopt"; cmd/rl/classes.go already carries
	// a sentence for each. Until now this runner reported every engine error as
	// sql_error, so a person who queried a table their project HAD DECLARED was told
	// "the engine reported an error in the statement" — which points them at their SQL,
	// and their SQL was fine. Measured on QA 2026-09-18: `SELECT * FROM loop_probe`
	// against a table in phase ready, ten vCPU-seconds charged, and nothing in the answer
	// saying the sandbox has no catalog to find it in.
	//
	// A class is a word from a list, never a fragment of the engine's message: the
	// message quotes the statement and can quote row values, which is why it stays in
	// error.log. Which WORD is chosen is the only thing that crosses.
	classCatalog    = "catalog_error"
	classBinder     = "binder_error"
	classSyntax     = "syntax_error"
	classIO         = "io_error"
	classHTTP       = "http_error"
	classPermission = "permission_denied"
)

// duckdbErrorClasses maps DuckDB's own error prefixes onto that vocabulary.
//
// ORDER MATTERS AND IS NOT ALPHABETICAL. DuckDB prefixes every error with its kind, but
// some messages mention another kind further in — an "IO Error" while reading a remote
// file also says "HTTP", for instance — so the first prefix wins and the list is ordered
// by how specific the kind is. Anything not here stays sql_error, which is what every
// engine error was before this existed: a token that says "the statement failed" without
// guessing why.
var duckdbErrorClasses = []struct {
	prefix string
	class  string
}{
	{"Catalog Error", classCatalog},
	{"Binder Error", classBinder},
	{"Parser Error", classSyntax},
	{"Permission Error", classPermission},
	{"HTTP Error", classHTTP},
	{"IO Error", classIO},
}

// classifyEngineError reads the kind DuckDB named and returns the matching class.
//
// It reads only the HEAD of the log, the same bounded read the out-of-memory check uses:
// the error log holds the engine's whole complaint, which is customer data, and this
// function must never need more than the first line of it to name a kind.
func classifyEngineError(head string) string {
	for _, c := range duckdbErrorClasses {
		if strings.Contains(head, c.prefix) {
			return c.class
		}
	}
	return classSQL
}

// metrics is the one line of stdout. Field order is the contract's order.
type metrics struct {
	Status          string  `json:"status"`
	RowsOut         int64   `json:"rows_out"`
	BytesScanned    int64   `json:"bytes_scanned"`
	CPUCoreSeconds  float64 `json:"cpu_core_seconds"`
	PeakMemoryBytes int64   `json:"peak_memory_bytes"`
	WallSeconds     float64 `json:"wall_seconds"`
	ResultBytes     int64   `json:"result_bytes"`
	// ResultUploaded says the result reached the object store the agent named. The
	// agent emits a ResultHandle only when it is true: a handle for an object that was
	// never written sends a browser to the storage endpoint for an XML error that
	// quotes the customer's bucket, which is a worse answer than no download link.
	// Absent on an older runner, which decodes as false — correctly, since an older
	// runner uploads nothing.
	ResultUploaded bool   `json:"result_uploaded"`
	ErrorClass     string `json:"error_class,omitempty"`
}

type config struct {
	// uploadURL is the presigned PUT the result is sent to, empty when the agent minted
	// none. uploadHeaders are the headers its signature covers and must be sent verbatim.
	uploadURL     string
	uploadHeaders map[string]string

	queryFile   string
	workDir     string
	resultFile  string
	threads     string
	memoryLimit string
	timeout     time.Duration
	duckdb      string
	initFile    string
}

var (
	threadsRE = regexp.MustCompile(`^[1-9][0-9]{0,3}$`)
	// What DuckDB's memory_limit accepts: a number, an optional unit, or a percentage.
	// Validated because the value is interpolated into SQL.
	memoryLimitRE = regexp.MustCompile(`(?i)^[0-9]+(\.[0-9]+)?\s*([KMGTP]i?B|[KMGTP]|B|%)?$`)
	timeoutRE     = regexp.MustCompile(`^[0-9]{1,7}$`)
)

func configFromEnv(getenv func(string) string) (config, error) {
	or := func(v, def string) string {
		if v == "" {
			return def
		}
		return v
	}
	c := config{
		queryFile: or(getenv("RL_QUERY_FILE"), "/run/rl/query.sql"),
		workDir:   or(getenv("RL_WORK_DIR"), "/work"),
		duckdb:    or(getenv("RL_DUCKDB_BIN"), "/usr/local/bin/duckdb"),
		initFile:  or(getenv("RL_INIT_FILE"), "/opt/duckdb/init.sql"),
	}
	c.resultFile = or(getenv("RL_RESULT_FILE"), filepath.Join(c.workDir, "result.parquet"))
	// THE UPLOAD, WHEN THE AGENT MINTED ONE. A presigned PUT and the headers its
	// signature covers. No credential reaches this pod — automountServiceAccountToken is
	// false and the admission policy refuses it otherwise — so a signed URL is the only
	// way the result leaves, and it is deliberately all this process can do with it: one
	// method, one key, one deadline.
	//
	// BOTH OR NEITHER. The headers are part of what was signed, so sending the URL
	// without them fails the signature rather than writing an unencrypted object. A URL
	// with no headers is treated as no upload at all rather than attempted and lost.
	c.uploadURL = strings.TrimSpace(getenv("RL_RESULT_UPLOAD_URL"))
	if h := strings.TrimSpace(getenv("RL_RESULT_UPLOAD_HEADERS")); h != "" && c.uploadURL != "" {
		if err := json.Unmarshal([]byte(h), &c.uploadHeaders); err != nil {
			return c, fmt.Errorf("RL_RESULT_UPLOAD_HEADERS is not a JSON object: %w", err)
		}
	}
	if len(c.uploadHeaders) == 0 {
		c.uploadURL = ""
	}
	if v := getenv("RL_THREADS"); v != "" {
		if !threadsRE.MatchString(v) {
			return c, fmt.Errorf("RL_THREADS must be a positive integer, got %q", v)
		}
		c.threads = v
	}
	if v := strings.TrimSpace(getenv("RL_MEMORY_LIMIT")); v != "" {
		if !memoryLimitRE.MatchString(v) {
			return c, fmt.Errorf("RL_MEMORY_LIMIT must look like 512MB, got %q", v)
		}
		c.memoryLimit = v
	}
	if v := getenv("RL_TIMEOUT_SECONDS"); v != "" {
		if !timeoutRE.MatchString(v) {
			return c, fmt.Errorf("RL_TIMEOUT_SECONDS must be a non-negative integer, got %q", v)
		}
		n, _ := strconv.Atoi(v)
		c.timeout = time.Duration(n) * time.Second
	}
	return c, nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Println("rl-duckdb-runner", version)
		return
	}
	cfg, err := configFromEnv(os.Getenv)
	if err != nil {
		// No work directory is trusted yet, so the reason is lost; the class is not.
		m := metrics{Status: "failed", ErrorClass: classRunnerIO}
		emit(os.Stdout, m)
		os.Exit(1)
	}
	m, code := run(context.Background(), cfg)
	emit(os.Stdout, m)
	os.Exit(code)
}

// emit writes the one permitted line of stdout.
func emit(w io.Writer, m metrics) {
	b, err := json.Marshal(m)
	if err != nil {
		// A struct of numbers and short strings cannot fail to marshal; keep the line
		// shape anyway so the agent never sees a bare prompt.
		b = []byte(`{"status":"failed","error_class":"runner_io_error"}`)
	}
	fmt.Fprintf(w, "RL_METRICS %s\n", b)
}

// run executes the query file described by cfg and returns the metrics and the
// process exit code. It never writes to stdout or stderr.
func run(ctx context.Context, cfg config) (metrics, int) {
	start := time.Now()
	m := metrics{Status: "failed"}
	finish := func(class string, code int) (metrics, int) {
		m.WallSeconds = round6(time.Since(start).Seconds())
		if class == "" {
			m.Status = "succeeded"
		} else {
			m.Status = "failed"
			m.ErrorClass = class
		}
		return m, code
	}

	tmpDir := filepath.Join(cfg.workDir, "tmp")
	if err := os.MkdirAll(tmpDir, 0o700); err != nil {
		return finish(classRunnerIO, 1)
	}
	errorLog := filepath.Join(cfg.workDir, "error.log")
	logf, err := os.OpenFile(errorLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return finish(classRunnerIO, 1)
	}
	defer logf.Close()
	// note records a runner-level diagnostic beside the engine's own stderr. It must
	// never be handed statement text.
	note := func(format string, args ...any) {
		fmt.Fprintf(logf, "rl-duckdb-runner: "+format+"\n", args...)
	}

	stmt, kind, err := loadQuery(cfg.queryFile)
	if err != nil {
		note("%v", err)
		return finish(classInvalidQuery, 1)
	}
	initSQL, err := os.ReadFile(cfg.initFile)
	if err != nil {
		note("init file: %v", err)
		return finish(classRunnerIO, 1)
	}
	countFile := filepath.Join(cfg.workDir, "rowcount.csv")
	script := composeScript(string(initSQL), cfg, tmpDir, countFile, stmt, kind)

	// The engine's stdout is captured to a file and never forwarded: a non-query
	// statement can print rows, and the sentinel is read back from here.
	outPath := filepath.Join(cfg.workDir, "duckdb.out")
	outf, err := os.Create(outPath)
	if err != nil {
		note("engine stdout capture: %v", err)
		return finish(classRunnerIO, 1)
	}

	if cfg.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.timeout)
		defer cancel()
	}
	// -bail: stop at the first error so a failed CREATE never reaches the COPY.
	// -no-init: never read ~/.duckdbrc; HOME is the writable emptyDir.
	// -csv -noheader: the only stdout we read back is the sentinel and it is one token.
	cmd := exec.CommandContext(ctx, cfg.duckdb, "-batch", "-bail", "-no-init", "-csv", "-noheader")
	cmd.Stdin = strings.NewReader(script)
	cmd.Stdout = outf
	cmd.Stderr = logf
	// The engine gets a fresh, minimal environment. The pod carries no credential by
	// contract, and this is the layer that makes that true even when the pod is wrong.
	cmd.Env = []string{"HOME=" + cfg.workDir, "TMPDIR=" + tmpDir}
	cmd.Cancel = func() error { return cmd.Process.Kill() }
	cmd.WaitDelay = 5 * time.Second
	runErr := cmd.Run()
	outf.Close()

	if ps := cmd.ProcessState; ps != nil {
		m.CPUCoreSeconds = round6((ps.UserTime() + ps.SystemTime()).Seconds())
		if ru, ok := ps.SysUsage().(*syscall.Rusage); ok && ru != nil {
			m.PeakMemoryBytes = maxRSSBytes(int64(ru.Maxrss))
		}
	}

	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		note("engine killed after %s (RL_TIMEOUT_SECONDS)", cfg.timeout)
		return finish(classTimeout, 2)
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			note("could not run %s: %v", cfg.duckdb, runErr)
			return finish(classRunnerIO, 1)
		}
		if !initRan(outPath) {
			note("the engine failed before the statement ran (configuration or extension load); its stderr is above")
			return finish(classRunnerIO, 1)
		}
		head := readHead(errorLog, 64<<10)
		if strings.Contains(head, "Out of Memory Error") {
			return finish(classOOM, 1)
		}
		// The engine named a kind; say which. Previously every one of these was
		// sql_error, and "the engine reported an error in the statement" is the least
		// useful true thing that can be said about a missing table.
		if c := classifyEngineError(head); c != classSQL {
			return finish(c, 1)
		}
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
			note("engine was killed by SIGKILL outside the runner's timeout; presumed the pod memory limit")
			return finish(classOOM, 1)
		}
		return finish(classSQL, 1)
	}

	if kind == kindStatement {
		return finish("", 0)
	}
	rows, err := readCount(countFile)
	if err != nil {
		note("row count: %v", err)
		return finish(classRunnerIO, 1)
	}
	fi, err := os.Stat(cfg.resultFile)
	if err != nil {
		note("result file: %v", err)
		return finish(classRunnerIO, 1)
	}
	m.RowsOut = rows
	m.ResultBytes = fi.Size()

	// THE UPLOAD IS THE LAST THING, AND IT DOES NOT DECIDE THE QUERY.
	//
	// The query ran and its numbers are real whether or not the result reaches the
	// bucket, so a failed upload is reported as a successful query with
	// result_uploaded false — the caller sees their metrics and no download link, and
	// can re-run. Failing the run instead would tell somebody their query failed when
	// it did not, and would lose the metrics the cost path needs.
	if cfg.uploadURL != "" {
		if err := upload(ctx, cfg, fi.Size()); err != nil {
			// The URL is NEVER in the note: it carries its own authorisation in its
			// query string, and error.log is readable to anyone who reads the pod.
			note("result upload failed: %v", redactURL(err))
		} else {
			m.ResultUploaded = true
		}
	}
	return finish("", 0)
}

// composeScript is the whole conversation with the engine, in order: the image's
// init.sql, the per-run settings, the lock, the sentinel, then the statement.
func composeScript(initSQL string, cfg config, tmpDir, countFile, stmt string, kind stmtKind) string {
	var b strings.Builder
	b.WriteString(initSQL)
	if !strings.HasSuffix(initSQL, "\n") {
		b.WriteByte('\n')
	}
	fmt.Fprintf(&b, "SET temp_directory=%s;\n", sqlString(tmpDir))
	if cfg.threads != "" {
		fmt.Fprintf(&b, "SET threads=%s;\n", cfg.threads)
	}
	if cfg.memoryLimit != "" {
		fmt.Fprintf(&b, "SET memory_limit=%s;\n", sqlString(cfg.memoryLimit))
	}
	// From here the statement cannot re-enable autoinstall, move the extension
	// directory, or change any other setting: DuckDB refuses with an error.
	b.WriteString("SET lock_configuration=true;\n")
	fmt.Fprintf(&b, "SELECT '%s';\n", initSentinel)
	switch kind {
	case kindQuery:
		fmt.Fprintf(&b, "CREATE TEMP TABLE %s AS\n%s\n;\n", resultTable, stmt)
	case kindDescribe:
		// DESCRIBE, SHOW and SUMMARIZE are not accepted directly after AS; DuckDB
		// takes them as a subquery.
		fmt.Fprintf(&b, "CREATE TEMP TABLE %s AS SELECT * FROM (\n%s\n);\n", resultTable, stmt)
	case kindStatement:
		fmt.Fprintf(&b, "%s\n;\n", stmt)
	}
	if kind != kindStatement {
		fmt.Fprintf(&b, "COPY %s TO %s (FORMAT PARQUET);\n", resultTable, sqlString(cfg.resultFile))
		fmt.Fprintf(&b, "COPY (SELECT count(*) FROM %s) TO %s (FORMAT CSV, HEADER false);\n", resultTable, sqlString(countFile))
	}
	return b.String()
}

func sqlString(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// loadQuery reads the query file and returns the single statement it holds, with
// leading whitespace and comments and any trailing terminator removed, and its kind.
func loadQuery(path string) (string, stmtKind, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, fmt.Errorf("query file: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxQueryBytes+1))
	if err != nil {
		return "", 0, fmt.Errorf("query file: %w", err)
	}
	if len(data) > maxQueryBytes {
		return "", 0, fmt.Errorf("query file exceeds %d bytes", maxQueryBytes)
	}
	if strings.IndexByte(string(data), 0) >= 0 {
		return "", 0, errors.New("query file contains a NUL byte")
	}
	stmt, err := singleStatement(string(data))
	if err != nil {
		return "", 0, err
	}
	kind, err := classify(stmt)
	if err != nil {
		return "", 0, err
	}
	return stmt, kind, nil
}

type stmtKind int

const (
	// kindQuery is materialised with CREATE TEMP TABLE ... AS <stmt>.
	kindQuery stmtKind = iota + 1
	// kindDescribe is materialised through a subquery; see composeScript.
	kindDescribe
	// kindStatement is executed as-is and reports rows_out 0.
	kindStatement
)

// classify decides how a statement is run from its first token, per the contract's
// list, and refuses the two things the DuckDB CLI would take as a command rather than
// SQL when they begin a line: a dot-command and a '#' comment.
func classify(stmt string) (stmtKind, error) {
	switch stmt[0] {
	case '.', '#':
		return 0, errors.New("statement begins with a CLI dot-command or '#', which is not SQL")
	case '(':
		return kindQuery, nil
	}
	end := 0
	for end < len(stmt) && (isIdentByte(stmt[end])) {
		end++
	}
	switch strings.ToUpper(stmt[:end]) {
	case "SELECT", "WITH", "FROM", "VALUES", "PIVOT", "UNPIVOT":
		return kindQuery, nil
	case "DESCRIBE", "SHOW", "SUMMARIZE":
		return kindDescribe, nil
	}
	return kindStatement, nil
}

// singleStatement returns the one statement in sql, or an error if sql holds none or
// more than one. It walks the lexical forms DuckDB's parser knows, because a semicolon
// inside a string literal, a quoted identifier, a dollar-quoted string or a comment is
// not a terminator, and one outside them is.
func singleStatement(sql string) (string, error) {
	start := skipBlank(sql, 0)
	if start >= len(sql) {
		return "", errors.New("query file holds no statement")
	}
	end := -1
	i := start
scan:
	for i < len(sql) {
		c := sql[i]
		switch {
		case c == '-' && i+1 < len(sql) && sql[i+1] == '-':
			i = skipLineComment(sql, i)
		case c == '/' && i+1 < len(sql) && sql[i+1] == '*':
			i = skipBlockComment(sql, i)
		case c == '\'':
			i = skipQuoted(sql, i, '\'', isEscapeString(sql, i))
		case c == '"':
			i = skipQuoted(sql, i, '"', false)
		case c == '$' && (i == 0 || !isIdentByte(sql[i-1])):
			if j, ok := skipDollarQuoted(sql, i); ok {
				i = j
			} else {
				i++
			}
		case c == ';':
			end = i
			break scan
		default:
			i++
		}
	}
	var stmt string
	if end < 0 {
		stmt = sql[start:]
	} else {
		rest := end + 1
		for {
			rest = skipBlank(sql, rest)
			if rest < len(sql) && sql[rest] == ';' {
				rest++
				continue
			}
			break
		}
		if rest < len(sql) {
			return "", errors.New("query file holds more than one statement")
		}
		stmt = sql[start:end]
	}
	return strings.TrimRight(stmt, " \t\r\n\v\f"), nil
}

// skipBlank advances past whitespace and comments.
func skipBlank(s string, i int) int {
	for i < len(s) {
		switch {
		case s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n' || s[i] == '\v' || s[i] == '\f':
			i++
		case s[i] == '-' && i+1 < len(s) && s[i+1] == '-':
			i = skipLineComment(s, i)
		case s[i] == '/' && i+1 < len(s) && s[i+1] == '*':
			i = skipBlockComment(s, i)
		default:
			return i
		}
	}
	return i
}

func skipLineComment(s string, i int) int {
	for i < len(s) && s[i] != '\n' {
		i++
	}
	return i
}

// skipBlockComment handles nesting, as the parser does.
func skipBlockComment(s string, i int) int {
	depth := 0
	for i < len(s) {
		switch {
		case s[i] == '/' && i+1 < len(s) && s[i+1] == '*':
			depth++
			i += 2
		case s[i] == '*' && i+1 < len(s) && s[i+1] == '/':
			depth--
			i += 2
			if depth == 0 {
				return i
			}
		default:
			i++
		}
	}
	return i
}

// skipQuoted starts at the opening quote and returns the index after the closing one.
// A doubled quote is an escape; with backslash set (E'...' strings) so is a backslash.
func skipQuoted(s string, i int, q byte, backslash bool) int {
	j := i + 1
	for j < len(s) {
		switch {
		case s[j] == q:
			if j+1 < len(s) && s[j+1] == q {
				j += 2
				continue
			}
			return j + 1
		case backslash && s[j] == '\\':
			j += 2
		default:
			j++
		}
	}
	return j
}

func isEscapeString(s string, i int) bool {
	if i == 0 || (s[i-1] != 'E' && s[i-1] != 'e') {
		return false
	}
	return i-1 == 0 || !isIdentByte(s[i-2])
}

// skipDollarQuoted recognises $$...$$ and $tag$...$tag$ at i. It reports false when
// the '$' does not open a tag, e.g. a $1 parameter.
func skipDollarQuoted(s string, i int) (int, bool) {
	j := i + 1
	for j < len(s) && isIdentByte(s[j]) {
		j++
	}
	if j >= len(s) || s[j] != '$' {
		return 0, false
	}
	if j > i+1 && s[i+1] >= '0' && s[i+1] <= '9' {
		return 0, false
	}
	tag := s[i : j+1]
	k := strings.Index(s[j+1:], tag)
	if k < 0 {
		return len(s), true
	}
	return j + 1 + k + len(tag), true
}

func isIdentByte(c byte) bool {
	return c == '_' || (c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}

// initRan reports whether the sentinel reached the engine's stdout.
func initRan(outPath string) bool {
	for _, line := range strings.Split(readHead(outPath, 64<<10), "\n") {
		line = strings.Trim(strings.TrimSpace(line), `"`)
		if line == initSentinel {
			return true
		}
	}
	return false
}

func readHead(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, int64(n)))
	return string(b)
}

func readCount(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unparseable row count")
	}
	return n, nil
}

// maxRSSBytes converts ru_maxrss to bytes. Linux reports kilobytes; Darwin, where the
// tests also run, reports bytes.
func maxRSSBytes(maxrss int64) int64 {
	if runtime.GOOS == "darwin" {
		return maxrss
	}
	return maxrss * 1024
}

func round6(f float64) float64 {
	return math.Round(f*1e6) / 1e6
}

// uploadTimeout bounds the PUT. Generous, because a large result over a NAT gateway is
// not fast, and bounded because a hung upload would otherwise outlive the query and be
// killed by the pod's deadline with no report at all — the one outcome worse than a
// failed upload, since it loses the metrics too.
const uploadTimeout = 10 * time.Minute

// upload sends the result file to the presigned PUT the agent minted.
//
// It streams the file rather than reading it into memory: a result is bounded by the
// sandbox's disk, not by its RAM, and the memory limit here is the engine's.
// Content-Length is set from the size already stat'd, because S3 refuses a chunked PUT
// against a presigned URL.
//
// EVERY HEADER IS SENT VERBATIM. They are part of what was signed — the SSE-KMS pair —
// so dropping one or altering it fails the signature rather than writing an unencrypted
// object. That is the property which lets a pod holding no credential write to an
// encrypted bucket without being able to choose how it is encrypted.
func upload(ctx context.Context, cfg config, size int64) error {
	f, err := os.Open(cfg.resultFile)
	if err != nil {
		return fmt.Errorf("opening the result: %w", err)
	}
	defer f.Close()

	ctx, cancel := context.WithTimeout(ctx, uploadTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, cfg.uploadURL, f)
	if err != nil {
		return fmt.Errorf("building the request: %w", err)
	}
	req.ContentLength = size
	for k, v := range cfg.uploadHeaders {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// The body of an S3 error is XML naming the bucket and key. It is drained so the
	// connection can be reused and then dropped unread: the status is the whole signal,
	// and 403 on a presigned PUT means the signature or the headers, which is ours.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("the object store refused the upload with status %d", resp.StatusCode)
	}
	return nil
}

// redactURL removes the presigned URL from an error before it reaches error.log.
//
// This is not defensive tidiness. net/http wraps transport failures in *url.Error, which
// embeds the FULL request URL — query string included — and a presigned URL's query
// string IS its authorisation. An unredacted DNS or TLS failure would therefore write a
// working credential for the customer's own object into a log file that anybody who can
// read the pod can read, for as long as the URL lives.
func redactURL(err error) error {
	if err == nil {
		return nil
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		// Keep the operation and the cause, drop the URL entirely rather than trying to
		// strip its query: a truncated URL still names the bucket and the key.
		return fmt.Errorf("%s: %v", ue.Op, ue.Err)
	}
	return err
}
