package ias3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/4q4r/wauploader/internal/auth"
	"github.com/4q4r/wauploader/internal/httpx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestClient returns an ias3.Client pointed at a fresh httptest server.
// Same override pattern as the other packages (auth endpoint vars): tests
// never touch live archive.org; each test runs sequentially, so mutating
// the client and the package-level pacing vars is race-free under -race.
func newTestClient(t *testing.T, h http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	cl := NewClient(httpx.NewClient(nil, "wauploader/test"), auth.Credentials{Access: "AKIAIOSFODNN7EXAMPLE", Secret: "wJalrXUtnFEMI"})
	cl.base = ts.URL
	return cl, ts
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "file.bin")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

// shrinkPacing replaces the 503-loop sleeps with 1ms so tests finish fast.
func shrinkPacing(t *testing.T) {
	t.Helper()
	oldSleep, oldPoll := slowDownWait, checkLimitPoll
	slowDownWait, checkLimitPoll = time.Millisecond, time.Millisecond
	t.Cleanup(func() { slowDownWait, checkLimitPoll = oldSleep, oldPoll })
}

// --- ValidateIdentifier -------------------------------------------------

func TestValidateIdentifierRules(t *testing.T) {
	valid := []string{"abcde", "a-b.c_d1", "12345", strings.Repeat("a", 80)}
	for _, id := range valid {
		assert.NoErrorf(t, ValidateIdentifier(id), "want %q valid", id)
	}

	invalid := []struct {
		id    string
		frag  string // Russian rule text expected in the message
		frag2 string // optional second rule text for multi-violation inputs
	}{
		{"ab", "5", "80"},
		{"-abc1", "первый символ", ""},
		{"ab#de", "A-Z", "a-z"},
		{"ab de", "A-Z", ""},
		{strings.Repeat("a", 81), "80", ""},
		{"test_meta.xml", "_meta.xml", ""},
		{"test_files.xml", "_files.xml", ""},
		// every violated rule at once: too short + bad first char + bad charset
		{"-#.", "первый символ", "символы"},
	}
	for _, tc := range invalid {
		err := ValidateIdentifier(tc.id)
		require.Errorf(t, err, "want %q rejected", tc.id)
		assert.Contains(t, err.Error(), tc.frag, "id %q: message must name the violated rule", tc.id)
		if tc.frag2 != "" {
			assert.Contains(t, err.Error(), tc.frag2, "id %q: message must list every violated rule", tc.id)
		}
	}

	// Multi-violation message: "-#" is too short, bad first char, bad charset.
	err := ValidateIdentifier("-#")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "длина", "must list the length violation")
	assert.Contains(t, err.Error(), "первый символ", "must list the first-char violation")
	assert.Contains(t, err.Error(), "символы", "must list the charset violation")
}

// --- HeaderOpts.headers --------------------------------------------------

func TestHeadersExactNames(t *testing.T) {
	o := HeaderOpts{
		Collection:              "test_collection",
		Mediatype:               "data",
		Title:                   "Тест ☃",
		Description:             "plain desc",
		AutoMakeBucket:          true,
		QueueDerive:             true,
		KeepOldVersion:          true,
		IgnorePreexistingBucket: true,
		// InteractivePriority deliberately false: header must be absent.
		SizeHint: 12345,
		Extra: Meta{
			"language":            {"rus", "eng"}, // multi-value → meta00/meta01 numbering
			"publisher--year":     {"2026"},       // "--" translated to "_"
			"external-identifier": {"urn:x:1"},
		},
	}

	got := o.headers()
	want := http.Header{
		"X-Archive-Auto-Make-Bucket":          {"1"},
		"X-Archive-Queue-Derive":              {"1"},
		"X-Archive-Keep-Old-Version":          {"1"},
		"X-Archive-Size-Hint":                 {"12345"},
		"X-Archive-Meta-Mediatype":            {"data"},
		"X-Archive-Meta01-Collection":         {"test_collection"},
		"X-Archive-Meta-Title":                {"uri(%D0%A2%D0%B5%D1%81%D1%82%20%E2%98%83)"},
		"X-Archive-Meta-Description":          {"plain desc"},
		"X-Archive-Meta00-Language":           {"rus"},
		"X-Archive-Meta01-Language":           {"eng"},
		"X-Archive-Meta-Publisher_year":       {"2026"},
		"X-Archive-Meta-External-Identifier":  {"urn:x:1"},
		"X-Archive-Ignore-Preexisting-Bucket": {"1"},
	}
	assert.Equal(t, want, got)
	assert.Empty(t, got.Values("X-Archive-Interactive-Priority"),
		"InteractivePriority=false must not emit a header")
}

// --- Upload ---------------------------------------------------------------

type capturedRequest struct {
	method           string
	path             string
	contentMD5       string
	authorization    string
	expect           string
	userAgent        string
	contentLength    int64
	transferEncoding []string
	body             []byte
	progressLast     int64
}

func TestUploadSuccessWithContentMD5(t *testing.T) {
	content := "hello"
	var capReq capturedRequest
	var progressLast int64
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capReq = capturedRequest{
			method:           r.Method,
			path:             r.URL.Path,
			contentMD5:       r.Header.Get("Content-MD5"),
			authorization:    r.Header.Get("Authorization"),
			expect:           r.Header.Get("Expect"),
			userAgent:        r.Header.Get("User-Agent"),
			contentLength:    r.ContentLength,
			transferEncoding: r.TransferEncoding,
			body:             body,
		}
		w.WriteHeader(http.StatusOK)
	})
	cl, _ := newTestClient(t, h)
	path := writeTempFile(t, content)

	err := cl.Upload(context.Background(), UploadRequest{
		Identifier: "testid1",
		Key:        "file.bin",
		Path:       path,
		Size:       int64(len(content)),
		MD5Hex:     "5d41402abc4b2a76b9719d911017c592",
	}, HeaderOpts{Collection: "test_collection", Mediatype: "data"},
		func(sent int64) { progressLast = sent })

	require.NoError(t, err)
	assert.Equal(t, http.MethodPut, capReq.method)
	assert.Equal(t, "/testid1/file.bin", capReq.path)
	assert.Equal(t, "5d41402abc4b2a76b9719d911017c592", capReq.contentMD5, "Content-MD5 must carry the hex digest verbatim")
	assert.True(t, strings.HasPrefix(capReq.authorization, "LOW AKIAIOSFODNN7EXAMPLE:"), "Authorization must be LOW <access>:<secret>, got %q", capReq.authorization)
	assert.Equal(t, "100-continue", capReq.expect, "Expect: 100-continue must reach the wire")
	assert.Contains(t, capReq.userAgent, "wauploader/", "mandatory descriptive User-Agent")
	assert.Equal(t, int64(len(content)), capReq.contentLength)
	assert.Equal(t, []byte(content), capReq.body, "server must receive the exact file bytes")
	assert.Equal(t, int64(len(content)), progressLast, "progress must report the full size")
}

func TestUploadSlowDownRetryThenSuccess(t *testing.T) {
	shrinkPacing(t)
	puts, checks := 0, 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			puts++
			_, _ = io.Copy(io.Discard, r.Body) // avoid 500ms drain in httptest.Server.Close
			if puts == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>SlowDown</Code><Message>Please slow down.</Message></Error>`))
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			if r.URL.Query().Get("check_limit") == "1" {
				checks++
				_, _ = w.Write([]byte(`{"bucket":"testid1","accesskey":"x","over_limit":0,"detail":{}}`))
			}
		}
	})
	cl, _ := newTestClient(t, h)
	path := writeTempFile(t, "data")

	err := cl.Upload(context.Background(), UploadRequest{
		Identifier: "testid1", Key: "file.bin", Path: path, Size: 4,
	}, HeaderOpts{Mediatype: "data"}, nil)

	require.NoError(t, err)
	assert.Equal(t, 2, puts, "503 SlowDown must be retried with a whole-file re-PUT")
	assert.GreaterOrEqual(t, checks, 1, "CheckLimit must run between attempts")
}

func TestUploadSpamSlowDownTerminal(t *testing.T) {
	shrinkPacing(t)
	puts := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts++
			_, _ = io.Copy(io.Discard, r.Body) // avoid 500ms drain in httptest.Server.Close
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`<Error><Code>SlowDown</Code><Message>Your file appears to be spam and has been blocked.</Message></Error>`))
		}
	})
	cl, _ := newTestClient(t, h)
	path := writeTempFile(t, "data")

	err := cl.Upload(context.Background(), UploadRequest{
		Identifier: "testid1", Key: "file.bin", Path: path, Size: 4,
	}, HeaderOpts{Mediatype: "data"}, nil)

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrSpamSlowDown), "want ErrSpamSlowDown, got: %v", err)
	assert.Equal(t, 1, puts, "spam 503 is terminal — must NEVER be retried")
}

func TestUploadBadCredentials(t *testing.T) {
	shrinkPacing(t)
	puts := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			puts++
			_, _ = io.Copy(io.Discard, r.Body) // avoid 500ms drain in httptest.Server.Close
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`<Error><Code>InvalidAccessKeyId</Code><Message>The AWS Access Key Id you provided does not exist in our records.</Message><Resource /><RequestId>3cb1b592</RequestId></Error>`))
		}
	})
	cl, _ := newTestClient(t, h)
	path := writeTempFile(t, "data")

	err := cl.Upload(context.Background(), UploadRequest{
		Identifier: "testid1", Key: "file.bin", Path: path, Size: 4,
	}, HeaderOpts{Mediatype: "data"}, nil)

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBadCredentials), "want ErrBadCredentials, got: %v", err)
	assert.Equal(t, 1, puts, "credentials errors are terminal")

	// Also the "bad request" malformed-LOW body form.
	puts = 0
	h2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		puts++
		_, _ = io.Copy(io.Discard, r.Body) // avoid 500ms drain in httptest.Server.Close
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`bad request`))
	})
	cl2, _ := newTestClient(t, h2)
	err = cl2.Upload(context.Background(), UploadRequest{
		Identifier: "testid1", Key: "file.bin", Path: path, Size: 4,
	}, HeaderOpts{Mediatype: "data"}, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrBadCredentials), `"bad request" 403 must map to ErrBadCredentials, got: %v`, err)
}

func TestUpload503InvalidXMLBodyRetryable(t *testing.T) {
	shrinkPacing(t)
	puts := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			_, _ = w.Write([]byte(`{"over_limit":0}`))
			return
		}
		puts++
		_, _ = io.Copy(io.Discard, r.Body) // avoid 500ms drain in httptest.Server.Close
		if puts == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`<html>proxy maintenance page, not xml</html>`))
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	cl, _ := newTestClient(t, h)
	path := writeTempFile(t, "data")

	err := cl.Upload(context.Background(), UploadRequest{
		Identifier: "testid1", Key: "file.bin", Path: path, Size: 4,
	}, HeaderOpts{Mediatype: "data"}, nil)

	require.NoError(t, err, "a 503 with an unparseable body is retryable, not terminal")
	assert.Equal(t, 2, puts)
}

func TestNoChunkedEncoding(t *testing.T) {
	content := "content" // 7 bytes
	var capReq capturedRequest
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		capReq = capturedRequest{
			transferEncoding: r.TransferEncoding,
			contentLength:    r.ContentLength,
			body:             body,
		}
		w.WriteHeader(http.StatusOK)
	})
	cl, _ := newTestClient(t, h)
	path := writeTempFile(t, content)

	err := cl.Upload(context.Background(), UploadRequest{
		Identifier: "testid1", Key: "file.bin", Path: path, Size: int64(len(content)),
	}, HeaderOpts{Mediatype: "data"}, nil)

	require.NoError(t, err)
	assert.Empty(t, capReq.transferEncoding, "Transfer-Encoding: chunked is forbidden by IA-S3 — it must never be set")
	assert.Equal(t, int64(len(content)), capReq.contentLength, "Content-Length must be set (known length)")
	assert.Equal(t, content, string(capReq.body))
}

// --- CheckLimit / awaitNotOverLimit ---------------------------------------

func TestCheckLimitOverLimit(t *testing.T) {
	var query []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("check_limit") == "1" {
			q := r.URL.Query()
			query = []string{q.Get("accesskey"), q.Get("bucket")}
			_, _ = w.Write([]byte(`{"bucket":"testid1","accesskey":"AKIAIOSFODNN7EXAMPLE","over_limit":1,"detail":{}}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	})
	cl, _ := newTestClient(t, h)

	over, err := cl.CheckLimit(context.Background(), "testid1")
	require.NoError(t, err)
	assert.True(t, over, "over_limit:1 must report the item as overloaded")
	assert.Equal(t, []string{"AKIAIOSFODNN7EXAMPLE", "testid1"}, query, "query must carry accesskey and bucket")

	// Free path: over_limit:0 → false.
	h2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"bucket":"testid1","accesskey":"k","over_limit":0,"detail":{}}`))
	})
	cl2, _ := newTestClient(t, h2)
	over, err = cl2.CheckLimit(context.Background(), "testid1")
	require.NoError(t, err)
	assert.False(t, over)
}

func TestCheckLimitGarbageIsOverloaded(t *testing.T) {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html>definitely not json</html>`))
	})
	cl, _ := newTestClient(t, h)

	over, err := cl.CheckLimit(context.Background(), "testid1")
	assert.True(t, over, "unparseable answer must fail safe to overloaded=true")
	assert.Error(t, err, "the parse failure must be reported, not swallowed")
}

// --- budget: the 503 loop gives up after maxUploadAttempts ----------------

func TestUpload503BudgetExhausted(t *testing.T) {
	shrinkPacing(t)
	puts := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			_, _ = w.Write([]byte(`{"over_limit":0}`))
			return
		}
		puts++
		_, _ = io.Copy(io.Discard, r.Body) // avoid 500ms drain in httptest.Server.Close
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`<Error><Code>SlowDown</Code><Message>still busy</Message></Error>`))
	})
	cl, _ := newTestClient(t, h)
	path := writeTempFile(t, "data")

	err := cl.Upload(context.Background(), UploadRequest{
		Identifier: "testid1", Key: "file.bin", Path: path, Size: 4,
	}, HeaderOpts{Mediatype: "data"}, nil)

	require.Error(t, err)
	assert.Equal(t, maxUploadAttempts, puts, "loop budget must cap the number of whole-file PUTs")
	var s3Err *S3Error
	assert.True(t, errors.As(err, &s3Err), "budget exhaustion must surface as a typed S3Error, got: %v", err)
	assert.Equal(t, http.StatusServiceUnavailable, s3Err.StatusCode)
}

// --- awaitNotOverLimit poll loop ------------------------------------------

func TestAwaitNotOverLimitPolls(t *testing.T) {
	shrinkPacing(t)
	checks := 0
	overNow := true
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checks++
		if overNow && checks < 3 {
			_, _ = w.Write([]byte(`{"over_limit":1}`))
			return
		}
		_, _ = w.Write([]byte(`{"over_limit":0}`))
	})
	cl, _ := newTestClient(t, h)

	err := awaitNotOverLimit(context.Background(), cl, "testid1")
	require.NoError(t, err)
	assert.GreaterOrEqual(t, checks, 3, "must poll until the limit clears")

	// Context cancellation must end the loop, not spin forever.
	h2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"over_limit":1}`))
	})
	cl2, _ := newTestClient(t, h2)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err = awaitNotOverLimit(ctx, cl2, "testid1")
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "ctx deadline must surface, got: %v", err)
}

// --- upload preconditions ---------------------------------------------------

func TestUploadSizeMismatchRejected(t *testing.T) {
	cl, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	path := writeTempFile(t, "hello") // 5 bytes on disk
	err := cl.Upload(context.Background(), UploadRequest{
		Identifier: "testid1", Key: "file.bin", Path: path, Size: 99,
	}, HeaderOpts{Mediatype: "data"}, nil)
	require.Error(t, err, "declared size differing from the file must be rejected before any PUT")
	assert.Contains(t, err.Error(), "размер", "Russian message must explain the size mismatch")
}

func TestUploadInvalidExtraMetaName(t *testing.T) {
	cl, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	path := writeTempFile(t, "x")
	err := cl.Upload(context.Background(), UploadRequest{
		Identifier: "testid1", Key: "f", Path: path, Size: 1,
	}, HeaderOpts{Mediatype: "data", Extra: Meta{"1bad": {"v"}}}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1bad")
}
