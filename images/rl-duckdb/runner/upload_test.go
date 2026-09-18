package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func resultFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "result.parquet")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const signed = "?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Signature=deadbeef"

var sseHeaders = map[string]string{
	"X-Amz-Server-Side-Encryption":                "aws:kms",
	"X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id": "arn:aws:kms:us-east-2:1:key/k",
}

// THE KNOWN POSITIVE: a 200 sink receives one PUT carrying the file, its length and the
// two signed headers verbatim. Without this the negatives below could pass against an
// upload that never sends anything.
func TestTheResultIsPutWithTheSignedHeadersAndTheWholeFile(t *testing.T) {
	var seen *http.Request
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		seen = r
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := config{
		resultFile:    resultFile(t, "PAR1 these are the bytes PAR1"),
		uploadURL:     srv.URL + "/query-results/q1/result.parquet" + signed,
		uploadHeaders: sseHeaders,
	}
	if err := uploadResult(context.Background(), cfg, int64(len("PAR1 these are the bytes PAR1"))); err != nil {
		t.Fatalf("a 200 sink was reported as a failure: %v", err)
	}
	if seen == nil {
		t.Fatal("nothing was sent")
	}
	if seen.Method != http.MethodPut {
		t.Errorf("method %s, want PUT", seen.Method)
	}
	if string(body) != "PAR1 these are the bytes PAR1" {
		t.Errorf("body %q was not the result file", body)
	}
	if seen.ContentLength != int64(len(body)) {
		t.Errorf("Content-Length %d, want %d; S3 refuses a chunked PUT to a presigned URL", seen.ContentLength, len(body))
	}
	for k, v := range sseHeaders {
		if got := seen.Header.Get(k); got != v {
			t.Errorf("header %s = %q, want %q; it is signed into the URL and dropping it is a 403", k, got, v)
		}
	}
	if seen.URL.RawQuery == "" {
		t.Error("the signature (query string) did not reach the sink")
	}
}

// THE KNOWN NEGATIVE, AND THE ONE PROPERTY OF THE ERROR THAT MATTERS: a refusal fails
// the upload with the status and the storage endpoint's own words, and the presigned
// URL is in neither the error nor anything derived from it.
func TestARefusedPutFailsAndNeverLeaksTheURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, "<Error><Code>SignatureDoesNotMatch</Code></Error>")
	}))
	defer srv.Close()

	cfg := config{resultFile: resultFile(t, "x"), uploadURL: srv.URL + "/r.parquet" + signed, uploadHeaders: sseHeaders}
	err := uploadResult(context.Background(), cfg, 1)
	if err == nil {
		t.Fatal("a 403 was reported as success; the caller would be told succeeded about rows nobody can reach")
	}
	if !strings.Contains(err.Error(), "403") || !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Errorf("error %q does not carry the status and the endpoint's reason", err)
	}
	if strings.Contains(err.Error(), "X-Amz-Signature") || strings.Contains(err.Error(), srv.URL) {
		t.Fatalf("the error carries the presigned URL: %q", err)
	}
}

func TestAnUnreachableEndpointFailsWithoutTheURL(t *testing.T) {
	// A port nothing listens on.
	cfg := config{resultFile: resultFile(t, "x"), uploadURL: "https://127.0.0.1:9" + "/r" + signed, uploadHeaders: sseHeaders}
	err := uploadResult(context.Background(), cfg, 1)
	if err == nil {
		t.Fatal("an unreachable endpoint was reported as success")
	}
	if strings.Contains(err.Error(), "X-Amz-Signature") || strings.Contains(err.Error(), "127.0.0.1:9") {
		t.Fatalf("the error carries the URL: %q", err)
	}
}

// The pair is both-or-neither, https only, and the headers are a JSON object — refused
// at start, before a query runs and is charged for.
func TestTheUploadPairIsValidatedAtStart(t *testing.T) {
	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	hdr := `{"X-Amz-Server-Side-Encryption":"aws:kms"}`
	cases := []struct {
		name    string
		kv      map[string]string
		wantErr string
	}{
		{"neither is a supported plane", map[string]string{}, ""},
		{"both, valid", map[string]string{"RL_RESULT_UPLOAD_URL": "https://b.s3.amazonaws.com/k" + signed, "RL_RESULT_UPLOAD_HEADERS": hdr}, ""},
		{"url without headers", map[string]string{"RL_RESULT_UPLOAD_URL": "https://b/k"}, "set together"},
		{"headers without url", map[string]string{"RL_RESULT_UPLOAD_HEADERS": hdr}, "set together"},
		{"http, not https", map[string]string{"RL_RESULT_UPLOAD_URL": "http://b/k", "RL_RESULT_UPLOAD_HEADERS": hdr}, "not an https URL"},
		{"headers not an object", map[string]string{"RL_RESULT_UPLOAD_URL": "https://b/k", "RL_RESULT_UPLOAD_HEADERS": `["a"]`}, "JSON object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := configFromEnv(env(tc.kv))
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if tc.kv["RL_RESULT_UPLOAD_URL"] != "" && (c.uploadURL == "" || c.uploadHeaders["X-Amz-Server-Side-Encryption"] != "aws:kms") {
					t.Fatalf("the pair was accepted but not carried: url set=%t headers=%v", c.uploadURL != "", c.uploadHeaders)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to say %q", err, tc.wantErr)
			}
			// The URL is a credential and must not be echoed by a refusal either.
			if strings.Contains(err.Error(), "X-Amz-Signature") {
				t.Fatalf("the refusal echoes the URL: %v", err)
			}
		})
	}
}
