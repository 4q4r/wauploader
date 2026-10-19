package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/4q4r/wauploader/internal/config"
	"github.com/4q4r/wauploader/internal/httpx"
)

// overrideEndpoint swaps one package-level endpoint variable for the test
// duration and registers the restore via t.Cleanup. Tests in this package
// never run in parallel, so mutating the unexported vars is race-free.
func overrideEndpoint(t *testing.T, target *string, value string) {
	t.Helper()
	old := *target
	*target = value
	t.Cleanup(func() { *target = old })
}

// overridePause swaps the post-login courtesy pause for the test duration.
func overridePause(t *testing.T, d time.Duration) {
	t.Helper()
	old := postLoginPause
	postLoginPause = d
	t.Cleanup(func() { postLoginPause = old })
}

// newTestClient builds an httpx client for the fakes below; every request
// URL comes from the overridden endpoint vars, never from archive.org.
func newTestClient(t *testing.T) *httpx.Client {
	t.Helper()
	return httpx.NewClient(nil, "wauploader-test")
}

// fakeXauthn starts an httptest server acting as the xauthn login endpoint.
// It records the last request (method, parsed form) and replies with the
// given status and body.
func fakeXauthn(t *testing.T, status int, body string) *lastRequest {
	t.Helper()
	req := &lastRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req.capture(r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	overrideEndpoint(t, &xauthnEndpoint, srv.URL+"/services/xauthn/?op=login")
	return req
}

// lastRequest records method, path, and parsed form of the last request.
type lastRequest struct {
	method string
	path   string
	form   map[string]string
}

func (lr *lastRequest) capture(r *http.Request) {
	lr.method = r.Method
	lr.path = r.URL.RequestURI()
	if err := r.ParseForm(); err == nil {
		lr.form = map[string]string{}
		for k := range r.Form {
			lr.form[k] = r.Form.Get(k)
		}
	}
}

const (
	testEmail    = "user@example.com"
	testPassword = "CORRECT-HORSE-BATTERY"
	testAccess   = "SECRETACCESSKEYVALUE123"
	testSecret   = "TOPSECRETSECRETVALUE456"
	testUser     = "logged-in-user-cookie-value"
	testSig      = "logged-in-sig-cookie-value"
)

func successBody() string {
	return `{"success": true, "values": {"s3": {"access": "` + testAccess +
		`", "secret": "` + testSecret + `"}, "cookies": {"logged-in-user": "` + testUser +
		`", "logged-in-sig": "` + testSig + `"}, "screenname": "myscreen"}}`
}

func TestBootstrapSuccess(t *testing.T) {
	overridePause(t, 0)
	req := fakeXauthn(t, http.StatusOK, successBody())

	creds, err := Bootstrap(context.Background(), newTestClient(t), testEmail, testPassword)
	if err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}
	if creds.Access != testAccess {
		t.Errorf("Access = %q, want %q", creds.Access, testAccess)
	}
	if creds.Secret != testSecret {
		t.Errorf("Secret = %q, want %q", creds.Secret, testSecret)
	}
	if creds.CookieUser != testUser {
		t.Errorf("CookieUser = %q, want %q", creds.CookieUser, testUser)
	}
	if creds.CookieSig != testSig {
		t.Errorf("CookieSig = %q, want %q", creds.CookieSig, testSig)
	}
	if req.method != http.MethodPost {
		t.Errorf("xauthn request method = %q, want POST", req.method)
	}
	if !strings.Contains(req.path, "op=login") {
		t.Errorf("xauthn request path = %q, want it to contain op=login", req.path)
	}
	if got := req.form["email"]; got != testEmail {
		t.Errorf("form email = %q, want %q", got, testEmail)
	}
	if got := req.form["password"]; got != testPassword {
		t.Errorf("form password = %q, want %q", got, testPassword)
	}
}

func TestBootstrapBadPassword(t *testing.T) {
	overridePause(t, 0)
	fakeXauthn(t, http.StatusOK, `{"success": false, "values": {"reason": "account_bad_password"}}`)

	_, err := Bootstrap(context.Background(), newTestClient(t), testEmail, "wrong-password")
	if err == nil {
		t.Fatal("Bootstrap returned nil error for account_bad_password, want Russian error")
	}
	if !strings.Contains(err.Error(), "неверный пароль") {
		t.Errorf("error %q does not contain the Russian bad-password message", err.Error())
	}
	// The password value must never appear in the error text.
	if strings.Contains(err.Error(), "wrong-password") {
		t.Errorf("error leaks the password: %q", err.Error())
	}

	// account_not_found → a different Russian message.
	fakeXauthn(t, http.StatusOK, `{"success": false, "values": {"reason": "account_not_found"}}`)
	_, err = Bootstrap(context.Background(), newTestClient(t), testEmail, testPassword)
	if err == nil {
		t.Fatal("Bootstrap returned nil error for account_not_found, want Russian error")
	}
	if !strings.Contains(err.Error(), "не найден") {
		t.Errorf("error %q does not contain the Russian account-not-found message", err.Error())
	}
}

func TestBootstrapMissingReasonField(t *testing.T) {
	overridePause(t, 0)
	fakeXauthn(t, http.StatusOK, `{"success": false}`)

	_, err := Bootstrap(context.Background(), newTestClient(t), testEmail, testPassword)
	if err == nil {
		t.Fatal("Bootstrap returned nil error for a refusal without reason, want raw error surfaced")
	}
	// The raw refusal body must be surfaced, not swallowed.
	if !strings.Contains(err.Error(), `"success": false`) {
		t.Errorf("error %q does not surface the raw refusal body", err.Error())
	}
}

func TestBootstrapRateCourtesyPause(t *testing.T) {
	overridePause(t, 30*time.Millisecond)
	fakeXauthn(t, http.StatusOK, successBody())

	start := time.Now()
	_, err := Bootstrap(context.Background(), newTestClient(t), testEmail, testPassword)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Bootstrap returned error: %v", err)
	}
	if elapsed < 25*time.Millisecond {
		t.Errorf("Bootstrap returned in %v, want >= 25ms courtesy pause after the call", elapsed)
	}
}

func TestSaveToConfigPerms0600(t *testing.T) {
	// Nested directory that does not exist yet must be created.
	path := filepath.Join(t.TempDir(), "wauploader", "wauploader.toml")
	creds := Credentials{Access: testAccess, Secret: testSecret, CookieUser: testUser, CookieSig: testSig}

	if err := SaveToConfig(path, creds, "myscreen"); err != nil {
		t.Fatalf("SaveToConfig returned error: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("config perms = %o, want 600", got)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "password") {
		t.Error("saved config mentions password; init must never store the password")
	}
	if !strings.Contains(string(data), "myscreen") {
		t.Error("saved config lost the informational screenname comment")
	}
}

func TestSaveToConfigMergePreservesUnrelatedSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wauploader.toml")
	existing := `# user comment

[spn2]
capture_all = true
auth_mode = "cookies"

[limits]
save_per_minute = 3
`
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatalf("write existing config: %v", err)
	}
	creds := Credentials{Access: testAccess, Secret: testSecret, CookieUser: testUser, CookieSig: testSig}
	if err := SaveToConfig(path, creds, ""); err != nil {
		t.Fatalf("SaveToConfig returned error: %v", err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("config.Load after merge: %v", err)
	}
	if !cfg.SPN2.CaptureAll {
		t.Error("merge lost spn2.capture_all = true")
	}
	if cfg.SPN2.AuthMode != "cookies" {
		t.Errorf("merge lost spn2.auth_mode = %q", cfg.SPN2.AuthMode)
	}
	if cfg.Limits.SavePerMinute != 3 {
		t.Errorf("merge lost limits.save_per_minute = 3, got %d", cfg.Limits.SavePerMinute)
	}
	if cfg.Auth.S3Access != testAccess || cfg.Auth.S3Secret != testSecret {
		t.Error("merge did not write the [auth] keys")
	}
	if cfg.Auth.CookieUser != testUser || cfg.Auth.CookieSig != testSig {
		t.Error("merge did not write the [auth] cookies")
	}
	// Overwrite semantics: a second save replaces the previous [auth] values.
	other := Credentials{Access: "AAAA", Secret: "BBBB", CookieUser: "CCCC", CookieSig: "DDDD"}
	if err := SaveToConfig(path, other, ""); err != nil {
		t.Fatalf("second SaveToConfig: %v", err)
	}
	cfg, err = config.Load(path)
	if err != nil {
		t.Fatalf("config.Load after overwrite: %v", err)
	}
	if cfg.Auth.S3Access != "AAAA" {
		t.Errorf("second save did not replace s3_access, got %q", cfg.Auth.S3Access)
	}
}

func TestResolveFailsFastWithHint(t *testing.T) {
	cfg := config.Default()
	_, err := Resolve(cfg)
	if err == nil {
		t.Fatal("Resolve returned nil error on empty credentials, want Russian fail-fast error")
	}
	for _, want := range []string{"s3_access", "s3_secret", "cookie_user", "cookie_sig", "wauploader init", "s3.php"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err.Error(), want)
		}
	}

	// auth_mode = "keys" demands the S3 keys.
	cfg = config.Default()
	cfg.SPN2.AuthMode = "keys"
	_, err = Resolve(cfg)
	if err == nil || !strings.Contains(err.Error(), "s3_access") {
		t.Errorf("Resolve(auth_mode=keys, no keys) = %v, want error naming s3_access", err)
	}

	// auth_mode = "cookies" demands the cookies.
	cfg = config.Default()
	cfg.SPN2.AuthMode = "cookies"
	_, err = Resolve(cfg)
	if err == nil || !strings.Contains(err.Error(), "cookie_user") {
		t.Errorf("Resolve(auth_mode=cookies, no cookies) = %v, want error naming cookie_user", err)
	}

	// auto: keys win when present.
	cfg = config.Default()
	cfg.Auth.S3Access = testAccess
	cfg.Auth.S3Secret = testSecret
	creds, err := Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve(auto, keys) returned error: %v", err)
	}
	if creds.Access != testAccess || creds.Secret != testSecret {
		t.Error("Resolve(auto, keys) did not return the configured keys")
	}

	// auto: cookie-only credentials are a valid SPN2 state.
	cfg = config.Default()
	cfg.Auth.CookieUser = testUser
	cfg.Auth.CookieSig = testSig
	creds, err = Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve(auto, cookies) returned error: %v", err)
	}
	if creds.CookieUser != testUser || creds.CookieSig != testSig {
		t.Error("Resolve(auto, cookies) did not return the configured cookies")
	}

	// auth_mode = "cookies" with both present: SPN2 must use cookies only.
	cfg = config.Default()
	cfg.SPN2.AuthMode = "cookies"
	cfg.Auth.S3Access = testAccess
	cfg.Auth.S3Secret = testSecret
	cfg.Auth.CookieUser = testUser
	cfg.Auth.CookieSig = testSig
	creds, err = Resolve(cfg)
	if err != nil {
		t.Fatalf("Resolve(cookies, both) returned error: %v", err)
	}
	if creds.Access != "" || creds.Secret != "" {
		t.Error("Resolve(cookies, both) leaked keys into the credentials; explicit cookies mode must not use keys")
	}

	// Half-set key pair fails fast.
	cfg = config.Default()
	cfg.Auth.S3Access = testAccess
	_, err = Resolve(cfg)
	if err == nil || !strings.Contains(err.Error(), "s3_secret") {
		t.Errorf("Resolve(half key pair) = %v, want error naming s3_secret", err)
	}
}

func TestValidateKeysWhoami(t *testing.T) {
	requests := 0
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success": true, "values": {"screenname": "myscreen"}}`))
	}))
	t.Cleanup(srv.Close)
	overrideEndpoint(t, &whoamiEndpoint, srv.URL+"/services/user.php?op=whoami")

	creds := Credentials{Access: testAccess, Secret: testSecret}
	if err := creds.ValidateKeys(context.Background(), newTestClient(t)); err != nil {
		t.Fatalf("ValidateKeys returned error for a valid pair: %v", err)
	}
	if requests != 1 {
		t.Errorf("whoami requests = %d, want 1", requests)
	}
	if !strings.HasPrefix(gotAuth, "LOW ") {
		t.Errorf("Authorization = %q, want it to start with \"LOW \"", gotAuth)
	}

	// Invalid keys: success:false → error, and the secret values never surface.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"success": false}`))
	}))
	t.Cleanup(srv2.Close)
	overrideEndpoint(t, &whoamiEndpoint, srv2.URL+"/services/user.php?op=whoami")

	err := creds.ValidateKeys(context.Background(), newTestClient(t))
	if err == nil {
		t.Fatal("ValidateKeys returned nil error for success:false, want invalid-keys error")
	}
	if strings.Contains(err.Error(), testAccess) || strings.Contains(err.Error(), testSecret) {
		t.Errorf("invalid-keys error leaks the key values: %q", err.Error())
	}

	// Empty credentials fail fast without any HTTP call.
	requests = 0
	empty := Credentials{}
	if err := empty.ValidateKeys(context.Background(), newTestClient(t)); err == nil {
		t.Fatal("ValidateKeys returned nil error on empty credentials")
	}
	if requests != 0 {
		t.Errorf("empty credentials still hit whoami %d time(s); want fail-fast without HTTP", requests)
	}
}

func TestSPN2RequestKeysPreferred(t *testing.T) {
	creds := Credentials{Access: testAccess, Secret: testSecret, CookieUser: testUser, CookieSig: testSig}
	req, _ := http.NewRequest(http.MethodPost, "https://web.archive.org/save", nil)
	creds.SPN2Request(req)

	if got, want := req.Header.Get("Authorization"), "LOW "+testAccess+":"+testSecret; got != want {
		t.Errorf("Authorization = %q, want %q", got, want)
	}
	if cookies := req.Header.Get("Cookie"); cookies != "" {
		t.Errorf("keys present but Cookie header set to %q; keys take precedence", cookies)
	}

	// Cookie-only credentials: cookies on the request, no Authorization.
	cookieOnly := Credentials{CookieUser: testUser, CookieSig: testSig}
	req2, _ := http.NewRequest(http.MethodPost, "https://web.archive.org/save", nil)
	cookieOnly.SPN2Request(req2)
	if got := req2.Header.Get("Authorization"); got != "" {
		t.Errorf("Authorization = %q, want empty for cookie-only credentials", got)
	}
	cookies := req2.Header.Get("Cookie")
	if !strings.Contains(cookies, "logged-in-user="+testUser) || !strings.Contains(cookies, "logged-in-sig="+testSig) {
		t.Errorf("Cookie header = %q, want both logged-in cookies", cookies)
	}
}

func TestMaskFormat(t *testing.T) {
	long := "ABCDEFGHIJKLMNOP"
	masked := Mask(long)
	if masked != "ABCD*** (длина 16)" {
		t.Errorf("Mask(long) = %q, want %q", masked, "ABCD*** (длина 16)")
	}
	if strings.Contains(masked, long) {
		t.Errorf("Mask leaked the full value: %q", masked)
	}
	short := "abc"
	maskedShort := Mask(short)
	if strings.Contains(maskedShort, short) {
		t.Errorf("Mask leaked a short value: %q", maskedShort)
	}
	if Mask("") == "" {
		t.Error("Mask(\"\") returned an empty string; want an explicit placeholder")
	}
}

func TestCredentialsStringMasked(t *testing.T) {
	creds := Credentials{Access: testAccess, Secret: testSecret, CookieUser: testUser, CookieSig: testSig}
	got := creds.String()
	for _, secret := range []string{testAccess, testSecret, testUser, testSig} {
		if strings.Contains(got, secret) {
			t.Errorf("Credentials.String() leaks a full secret value %q: %s", secret, got)
		}
	}
	// Masked prefixes must still be present so the output stays useful.
	for _, want := range []string{"SECR", "TOPS", "logg"} {
		if !strings.Contains(got, want) {
			t.Errorf("Credentials.String() lacks the masked prefix %q: %s", want, got)
		}
	}
	for _, want := range []string{"s3_access", "s3_secret", "cookie_user", "cookie_sig"} {
		if !strings.Contains(got, want) {
			t.Errorf("Credentials.String() lacks the field name %q: %s", want, got)
		}
	}
}

func TestSaveToConfigSanitizesScreenname(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wauploader.toml")
	dirty := "my\nscreen\rname\twith\x00ctrl"
	creds := Credentials{Access: "A12345678", Secret: "B87654321", CookieUser: "C12345678", CookieSig: "D12345678"}
	if err := SaveToConfig(path, creds, dirty); err != nil {
		t.Fatalf("SaveToConfig returned error: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	var commentLine string
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "# Аккаунт archive.org: ") {
			commentLine = line
			break
		}
	}
	want := "# Аккаунт archive.org: myscreennamewithctrl"
	if commentLine != want {
		t.Errorf("screenname comment = %q, want sanitized %q", commentLine, want)
	}
	if _, err := config.Load(path); err != nil {
		t.Errorf("config.Load after sanitized comment: %v", err)
	}
}

func TestBootstrapRefusalNon2xxNotInterpreted(t *testing.T) {
	overridePause(t, 0)
	// A cached 502 page that happens to carry a refusal-shaped body must
	// never be read as "неверный пароль": the envelope is only trusted on 2xx.
	fakeXauthn(t, http.StatusBadGateway, `{"success": false, "values": {"reason": "account_bad_password"}}`)

	_, err := Bootstrap(context.Background(), newTestClient(t), testEmail, testPassword)
	if err == nil {
		t.Fatal("Bootstrap returned nil error for a 502 refusal-shaped body, want error")
	}
	if strings.Contains(err.Error(), "неверный пароль") {
		t.Errorf("502 body was interpreted as a credential refusal: %q", err.Error())
	}
	if !strings.Contains(err.Error(), "502") {
		t.Errorf("error %q does not mention the HTTP status 502", err.Error())
	}
	if !strings.Contains(err.Error(), `"success": false`) {
		t.Errorf("error %q does not surface the raw body", err.Error())
	}
}
