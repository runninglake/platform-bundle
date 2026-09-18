package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

type capture struct {
	mu      sync.Mutex
	hits    int
	hdrs    http.Header
	body    []byte
	method  string
	status  int
	cLength int64
}

func (c *capture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.hits++
		c.method = r.Method
		c.hdrs = r.Header.Clone()
		c.cLength = r.ContentLength
		c.body, _ = io.ReadAll(r.Body)
		if c.status == 0 {
			c.status = 200
		}
		if c.status != 200 {
			// What S3 actually sends: XML naming the bucket and the key.
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(`<Error><Code>AccessDenied</Code><BucketName>rl-qa-pl-qa0001-system</BucketName></Error>`))
			return
		}
		w.WriteHeader(200)
	}
}

func uploadEnv(u string) map[string]string {
	return map[string]string{
		"RL_RESULT_UPLOAD_URL": u,
		"RL_RESULT_UPLOAD_HEADERS": `{"X-Amz-Server-Side-Encryption":"aws:kms",` +
			`"X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id":"arn:aws:kms:us-east-2:1:key/k"}`,
	}
}

func TestTheResultIsUploadedAndTheSignedHeadersGoVerbatim(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(c.handler())
	defer srv.Close()

	h := newHarness(t, "SELECT 1 AS n;\n", uploadEnv(srv.URL+"/query-results/q1/result.parquet?sig=abc"))
	m, code, _ := h.exec()

	if code != 0 || m.Status != "succeeded" {
		t.Fatalf("want succeeded/0, got %+v code %d", m, code)
	}
	if !m.ResultUploaded {
		t.Fatal("result_uploaded is false after a successful upload; the agent emits no " +
			"handle on that, so the caller would see a finished query with no rows")
	}
	if c.hits != 1 || c.method != http.MethodPut {
		t.Fatalf("got %d request(s), method %q; want one PUT", c.hits, c.method)
	}
	// The headers are part of what was signed: dropping or altering one fails the
	// signature rather than writing an unencrypted object.
	if got := c.hdrs.Get("X-Amz-Server-Side-Encryption"); got != "aws:kms" {
		t.Errorf("X-Amz-Server-Side-Encryption = %q, want aws:kms", got)
	}
	if got := c.hdrs.Get("X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"); !strings.HasPrefix(got, "arn:aws:kms:") {
		t.Errorf("the key id header did not arrive verbatim: %q", got)
	}
	// S3 refuses a chunked PUT against a presigned URL, so the length must be set.
	if c.cLength <= 0 || c.cLength != m.ResultBytes {
		t.Errorf("Content-Length = %d, result_bytes = %d; they must agree and be positive", c.cLength, m.ResultBytes)
	}
	if len(c.body) == 0 {
		t.Error("the PUT carried no body")
	}
}

// A REFUSED UPLOAD IS A SUCCESSFUL QUERY WITHOUT A LINK, NOT A FAILED QUERY.
//
// The query ran and its numbers are real. Reporting it failed would tell somebody their
// query failed when it did not, and would lose the metrics the cost path settles from.
func TestARefusedUploadLeavesTheQuerySuccessful(t *testing.T) {
	c := &capture{status: 403}
	srv := httptest.NewServer(c.handler())
	defer srv.Close()

	h := newHarness(t, "SELECT 1 AS n;\n", uploadEnv(srv.URL+"/query-results/q1/result.parquet?sig=abc"))
	m, code, _ := h.exec()

	if code != 0 || m.Status != "succeeded" {
		t.Fatalf("a refused upload failed the query: %+v code %d", m, code)
	}
	if m.ResultUploaded {
		t.Fatal("result_uploaded is true after a 403; the agent would emit a handle for " +
			"an object that was never written, and the browser would get an XML error " +
			"naming the customer's bucket")
	}
	if m.RowsOut != 1 || m.ResultBytes <= 0 {
		t.Error("the query's own numbers were lost with the upload")
	}
}

// THE PRESIGNED URL MUST NEVER REACH error.log. Its query string IS its authorisation,
// and error.log is readable to anyone who can read the pod. net/http wraps transport
// failures in *url.Error, which embeds the whole URL — so this is a real leak, not a
// hypothetical one.
func TestAFailedUploadNeverWritesTheUrlToTheLog(t *testing.T) {
	srv := httptest.NewServer(c403())
	addr := srv.URL
	srv.Close() // closed: every request now fails in the transport, inside a *url.Error

	const sig = "SIGNATUREthatMUSTnotAPPEAR"
	h := newHarness(t, "SELECT 1 AS n;\n", uploadEnv(addr+"/query-results/q1/result.parquet?X-Amz-Signature="+sig))
	m, code, _ := h.exec()

	if code != 0 || m.ResultUploaded {
		t.Fatalf("want a successful query with no upload, got %+v code %d", m, code)
	}
	log := h.errorLog()
	if log == "" {
		t.Fatal("nothing was logged at all, so this test would pass for the wrong reason")
	}
	if strings.Contains(log, sig) || strings.Contains(log, "X-Amz-Signature") {
		t.Errorf("the presigned URL's signature reached error.log:\n%s", log)
	}
	if strings.Contains(log, "query-results/q1") {
		t.Errorf("the object key reached error.log:\n%s", log)
	}
}

func c403() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }
}

// Both or neither: a URL without its signed headers fails the signature, so it is treated
// as no upload rather than attempted and lost.
func TestAnUrlWithoutItsHeadersIsNotAnUpload(t *testing.T) {
	c := &capture{}
	srv := httptest.NewServer(c.handler())
	defer srv.Close()

	h := newHarness(t, "SELECT 1 AS n;\n", map[string]string{"RL_RESULT_UPLOAD_URL": srv.URL + "/x?sig=a"})
	m, code, _ := h.exec()
	if code != 0 || m.ResultUploaded {
		t.Fatalf("want a successful query with no upload, got %+v code %d", m, code)
	}
	if c.hits != 0 {
		t.Errorf("a request was made with no signed headers; it would have failed the signature")
	}
}

func TestNoUploadConfiguredMakesNoRequest(t *testing.T) {
	h := newHarness(t, "SELECT 1 AS n;\n", nil)
	m, code, line := h.exec()
	if code != 0 || m.Status != "succeeded" {
		t.Fatalf("want succeeded/0, got %+v", m)
	}
	if m.ResultUploaded {
		t.Error("result_uploaded is true on a plane that configured no upload")
	}
	// An older agent decodes result_uploaded as absent/false, which is correct: it
	// uploads nothing. The field must still be present for a newer one.
	var decoded map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(line), "RL_METRICS ")), &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["result_uploaded"]; !ok {
		t.Error("result_uploaded is missing from the report")
	}
}

func TestRedactUrlRemovesTheWholeUrl(t *testing.T) {
	err := &url.Error{Op: "Put", URL: "https://b.s3.amazonaws.com/k?X-Amz-Signature=SECRET", Err: errors.New("dial tcp: refused")}
	got := redactURL(err).Error()
	if strings.Contains(got, "SECRET") || strings.Contains(got, "s3.amazonaws.com") {
		t.Errorf("redactURL kept the url: %q", got)
	}
	if !strings.Contains(got, "dial tcp: refused") {
		t.Errorf("redactURL dropped the cause, which is the only useful part: %q", got)
	}
}
