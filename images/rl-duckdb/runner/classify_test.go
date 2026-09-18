package main

import "testing"

// THE CLASS IS THE ONLY PART OF AN ENGINE ERROR THAT CROSSES, SO IT HAD BETTER SAY
// SOMETHING.
//
// Until this existed every engine error was sql_error, and a person who queried a table
// their project had DECLARED was told "the engine reported an error in the statement".
// Their SQL was fine; the sandbox simply has no catalog to find the table in. Measured on
// QA 2026-09-18: `SELECT * FROM loop_probe` against a table in phase ready — failed,
// sql_error, ten vCPU-seconds charged, and nothing in the answer pointing at the cause.
//
// internal/agent/duckdb has accepted catalog_error, binder_error, syntax_error, io_error,
// http_error and permission_denied all along, and cmd/rl carries a sentence for each. The
// vocabulary was built and nothing ever sent a word from it.
func TestTheEngineErrorKindBecomesAClass(t *testing.T) {
	for _, tc := range []struct {
		name string
		head string
		want string
	}{
		// The case that prompted this, in DuckDB's own words.
		{"a table the sandbox cannot see",
			"Catalog Error: Table with name loop_probe does not exist!\nDid you mean \"pg_proc\"?", classCatalog},
		{"a column that is not there",
			"Binder Error: Referenced column \"nope\" not found in FROM clause!", classBinder},
		{"a statement that does not parse",
			"Parser Error: syntax error at or near \"SELCT\"", classSyntax},
		{"a remote file the sandbox may not read",
			"HTTP Error: HTTP GET error on 'https://example/x.parquet' (HTTP 403)", classHTTP},
		{"a file the engine could not read",
			"IO Error: No files found that match the pattern", classIO},
		{"a path the sandbox is refused",
			"Permission Error: Cannot access file \"/etc/shadow\"", classPermission},

		// Anything unrecognised stays what it has always been. Guessing a kind is worse
		// than declining to: a wrong class is a person sent to look in the wrong place.
		{"a kind this does not know", "Conversion Error: Could not convert string to INT32", classSQL},
		{"no message at all", "", classSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyEngineError(tc.head); got != tc.want {
				t.Errorf("classifyEngineError = %q, want %q\n  head: %q", got, tc.want, tc.head)
			}
		})
	}
}

// ORDER IS PART OF THE CONTRACT. An IO error while fetching a remote file mentions HTTP
// further into its message, so a map or an alphabetical list would classify it by
// whichever kind it happened to see first.
func TestTheFirstKindNamedWins(t *testing.T) {
	// DuckDB leads with the kind; the rest of the sentence may mention another.
	const head = "HTTP Error: HTTP GET error ... while performing IO Error recovery"
	if got := classifyEngineError(head); got != classHTTP {
		t.Errorf("got %q, want %q — the kind DuckDB LEADS with is the kind, not whichever appears in the prose", got, classHTTP)
	}
}

// Every class this can emit must be one internal/agent/duckdb's RunnerClasses admits, or
// the agent folds it to query_failed and the finer word is lost between here and a person.
// Listed literally rather than imported: the agent is a different module, and a copy that
// drifts is caught by this being a closed list somebody must edit deliberately.
func TestEveryClassIsOneTheAgentAccepts(t *testing.T) {
	accepted := map[string]bool{
		"sql_error": true, "query_timeout": true, "out_of_memory": true,
		"runner_io_error": true, "invalid_query_file": true, "query_failed": true,
		"syntax_error": true, "binder_error": true, "catalog_error": true,
		"io_error": true, "http_error": true,
		"permission_denied": true, "result_too_large": true, "runner_error": true,
	}
	for _, c := range duckdbErrorClasses {
		if !accepted[c.class] {
			t.Errorf("%q is not in the agent's RunnerClasses, so it is folded to query_failed and this whole change is invisible", c.class)
		}
	}
	if !accepted[classSQL] {
		t.Errorf("the fallback %q is not accepted either", classSQL)
	}
}
