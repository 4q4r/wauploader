// Package auth resolves, validates, and bootstraps archive.org credentials
// for wauploader: IA-S3 keys (Authorization: LOW) and SPN2 login cookies.
//
// Wire contract (docs/research/r2 §1.3): email+password is exchanged once
// against the xauthn login endpoint, which returns the S3 keys and both
// login cookies in a single response; there is no cookies→keys endpoint.
// Key validity is checked without side effects via the whoami endpoint.
//
// Secret discipline: key, cookie, and password values never appear in logs,
// console output, or error messages. Anything user-visible goes through
// Mask (first 4 characters + length).
package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/4q4r/wauploader/internal/config"
	"github.com/4q4r/wauploader/internal/httpx"
	toml "github.com/pelletier/go-toml/v2"
)

// Endpoint variables are package-level so httptest fakes can override them
// (tests never touch live archive.org). Tests in this package run
// sequentially, making the mutation race-free under -race.
var (
	xauthnEndpoint = "https://archive.org/services/xauthn/?op=login"
	whoamiEndpoint = "https://archive.org/services/user.php?op=whoami"
	// postLoginPause is the reference-implementation courtesy sleep after
	// every completed xauthn exchange (rate politeness toward IA).
	postLoginPause = 2 * time.Second
)

// maxLoginTimeout bounds a single xauthn exchange; the reference client
// uses 10s, doubled here because IA services can be slow (plan T5). It is
// a package-level var so tests can shrink it (same pattern as
// postLoginPause); tests run sequentially, making the mutation race-free.
var maxLoginTimeout = 30 * time.Second

// maxBodyRead caps how many response bytes are read before parsing.
const maxBodyRead = 1 << 20

// maxErrorBody caps the raw response fragment embedded in an error.
const maxErrorBody = 200

// ErrCookiesNotReturned is returned by Bootstrap together with usable
// key-only credentials: archive.org confirmed the login and issued the
// IA-S3 keys, but sent no (or a partial) login cookie pair. Keys work for
// both SPN2 (auto/keys modes) and IA-S3, so the caller may persist the
// keys and surface the sentinel's Russian text as a warning instead of
// failing. It never wraps other errors.
var ErrCookiesNotReturned = errors.New("куки не получены — SPN2 будет работать по ключам")

// Credentials holds one archive.org identity: IA-S3 keys and the SPN2 login
// cookies. Any subset may be populated; Resolve guarantees at least one
// usable form for the selected auth mode.
type Credentials struct {
	Access     string
	Secret     string
	CookieUser string
	CookieSig  string
}

// String renders every field through Mask so that an accidental %v of
// credentials (a log line, an error wrap) can never print a full secret.
func (c Credentials) String() string {
	return fmt.Sprintf("Credentials{s3_access: %s, s3_secret: %s, cookie_user: %s, cookie_sig: %s}",
		Mask(c.Access), Mask(c.Secret), Mask(c.CookieUser), Mask(c.CookieSig))
}

// xauthnResponse mirrors the xauthn login JSON envelope.
type xauthnResponse struct {
	Success bool   `json:"success"`
	Error   string `json:"error"`
	Values  struct {
		Reason string `json:"reason"`
		S3     struct {
			Access string `json:"access"`
			Secret string `json:"secret"`
		} `json:"s3"`
		Cookies struct {
			LoggedInUser string `json:"logged-in-user"`
			LoggedInSig  string `json:"logged-in-sig"`
		} `json:"cookies"`
	} `json:"values"`
}

// Mask renders a secret for user-facing output: the first 4 characters plus
// the length, or length only when the value is too short to safely show a
// prefix. Empty values get an explicit placeholder.
func Mask(s string) string {
	if s == "" {
		return "(пусто)"
	}

	n := len(s)
	if n <= 8 {
		return "*** (длина " + strconv.Itoa(n) + ")"
	}

	return s[:4] + "*** (длина " + strconv.Itoa(n) + ")"
}

// Resolve picks credentials for SPN2 per cfg.SPN2.AuthMode and fails fast
// in Russian when the selected form is missing, naming the exact TOML keys
// and the "wauploader init" escape hatch.
//
// auth_mode semantics (decided here, not in SPN2Request, because only
// Resolve sees the config):
//   - "auto": keys when present, else cookies, else error;
//   - "keys": keys required;
//   - "cookies": cookies required and returned exclusively — keys are
//     deliberately dropped, so SPN2Request cannot silently prefer them.
//
// Resolve is the SPN2-oriented resolver; upload commands must require the
// key fields themselves because IA-S3 never accepts cookies.
func Resolve(cfg *config.Config) (Credentials, error) {
	if cfg == nil {
		return Credentials{}, errors.New("конфигурация не загружена")
	}

	c := Credentials{
		Access:     cfg.Auth.S3Access,
		Secret:     cfg.Auth.S3Secret,
		CookieUser: cfg.Auth.CookieUser,
		CookieSig:  cfg.Auth.CookieSig,
	}
	if (c.Access == "") != (c.Secret == "") {
		return Credentials{}, errors.New("s3_access и s3_secret должны быть заданы вместе (оба ключа или ни одного)")
	}

	if (c.CookieUser == "") != (c.CookieSig == "") {
		return Credentials{}, errors.New("cookie_user и cookie_sig должны быть заданы вместе (оба куки или ни одного)")
	}

	haveKeys := c.Access != "" && c.Secret != ""
	haveCookies := c.CookieUser != "" && c.CookieSig != ""

	switch cfg.SPN2.AuthMode {
	case "keys":
		if !haveKeys {
			return Credentials{}, errors.New("spn2.auth_mode = \"keys\", но ключи не заданы: заполните s3_access и s3_secret в файле конфигурации (получить: https://archive.org/account/s3.php) или выполните wauploader init")
		}
	case "cookies":
		if !haveCookies {
			return Credentials{}, errors.New("spn2.auth_mode = \"cookies\", но куки не заданы: заполните cookie_user и cookie_sig в файле конфигурации или выполните wauploader init")
		}

		return Credentials{CookieUser: c.CookieUser, CookieSig: c.CookieSig}, nil
	default: // "auto"
		if !haveKeys && !haveCookies {
			return Credentials{}, errors.New("не заданы учётные данные archive.org: заполните в файле конфигурации ключи s3_access и s3_secret (получить: https://archive.org/account/s3.php) или куки cookie_user и cookie_sig, либо выполните wauploader init")
		}
	}

	return c, nil
}

// ValidateKeys checks the IA-S3 key pair against the whoami endpoint
// (success:true means valid). The GET is idempotent and uses the full
// retry ladder. Errors never quote the key values.
func (c Credentials) ValidateKeys(ctx context.Context, cl *httpx.Client) error {
	if c.Access == "" || c.Secret == "" {
		return errors.New("проверка ключей невозможна: s3_access или s3_secret не заданы")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, whoamiEndpoint, nil)
	if err != nil {
		return fmt.Errorf("проверка ключей: построить запрос whoami: %w", err)
	}

	req.Header.Set("Authorization", "LOW "+c.Access+":"+c.Secret)

	resp, err := cl.DoWithRetry(ctx, req, httpx.RetryOpts{Mode: httpx.RetryIdempotent})
	if err != nil {
		return fmt.Errorf("проверка ключей (whoami) не выполнена: %w", err)
	}
	defer drainAndClose(resp.Body)

	var payload struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyRead)).Decode(&payload); err != nil {
		return fmt.Errorf("проверка ключей: неожиданный ответ archive.org (HTTP %d): %w", resp.StatusCode, err)
	}

	if !payload.Success {
		return errors.New("ключи s3_access/s3_secret отклонены archive.org (whoami вернул success:false): сгенерируйте новые на https://archive.org/account/s3.php или выполните wauploader init")
	}

	return nil
}

// SPN2Request attaches SPN2 credentials to a request: the IA-S3 keys as an
// Authorization: LOW header when present (preferred), otherwise the login
// cookies. With both forms empty it leaves the request untouched — Resolve
// is the fail-fast gate upstream; this method cannot return an error by
// contract. Cookies attach to the request host (web.archive.org), matching
// the .archive.org cookie scope.
func (c Credentials) SPN2Request(req *http.Request) {
	if c.Access != "" && c.Secret != "" {
		req.Header.Set("Authorization", "LOW "+c.Access+":"+c.Secret)
		return
	}

	if c.CookieUser != "" {
		req.AddCookie(&http.Cookie{Name: "logged-in-user", Value: c.CookieUser})
	}

	if c.CookieSig != "" {
		req.AddCookie(&http.Cookie{Name: "logged-in-sig", Value: c.CookieSig})
	}
}

// Bootstrap exchanges email+password for credentials via one xauthn login
// POST. The POST uses RetryConnectOnly: a resent login request is
// ambiguous (the first attempt may have been processed), so only
// connect-phase failures — where nothing was written — are retried.
// Following the reference implementation, every completed exchange is
// followed by a courtesy pause. The password is used for the request only
// and never appears in returned errors.
func Bootstrap(ctx context.Context, cl *httpx.Client, email, password string) (creds Credentials, err error) {
	if email == "" {
		return Credentials{}, errors.New("не указан адрес электронной почты archive.org")
	}

	if password == "" {
		return Credentials{}, errors.New("не указан пароль archive.org")
	}
	// The login deadline bounds only the exchange itself; the courtesy
	// pause below runs on the caller's ctx, so a near-deadline success is
	// never overwritten by a pause error.
	loginCtx, cancelLogin := context.WithTimeout(ctx, maxLoginTimeout)
	defer cancelLogin()

	form := url.Values{"email": {email}, "password": {password}}

	req, err := http.NewRequestWithContext(loginCtx, http.MethodPost, xauthnEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return Credentials{}, fmt.Errorf("вход archive.org: построить запрос xauthn: %w", err)
	}

	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := cl.DoWithRetry(loginCtx, req, httpx.RetryOpts{Mode: httpx.RetryConnectOnly})
	if err != nil {
		return Credentials{}, fmt.Errorf("вход archive.org (xauthn) не выполнен: %w", err)
	}
	// Reference behavior: sleep after the call, on every answered path.
	// The pause uses the PARENT context (the login deadline may be nearly
	// spent when the exchange completes) and its outcome never overwrites
	// the exchange result: a pause interrupted by context cancellation
	// must not replace fresh credentials with a pause error.
	defer func() {
		_ = awaitRateCourtesy(ctx)
	}()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))

	_ = resp.Body.Close()
	if err != nil {
		return Credentials{}, fmt.Errorf("вход archive.org: чтение ответа (HTTP %d): %w", resp.StatusCode, err)
	}

	var payload xauthnResponse
	if err := json.Unmarshal(raw, &payload); err != nil {
		return Credentials{}, fmt.Errorf("вход archive.org: неожиданный ответ (HTTP %d): %w", resp.StatusCode, err)
	}

	if !payload.Success {
		// The refusal envelope is only trusted on 2xx: a cached or proxy
		// error page (e.g. 502) that happens to carry success:false must
		// not be read as a credential failure.
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return Credentials{}, refusalError(&payload, raw, resp.StatusCode, email)
		}

		detail := truncate(strings.TrimSpace(string(raw)), maxErrorBody)
		if detail == "" {
			detail = http.StatusText(resp.StatusCode)
		}

		return Credentials{}, fmt.Errorf("вход archive.org не выполнен (HTTP %d): %s", resp.StatusCode, detail)
	}

	values := payload.Values
	if values.S3.Access == "" || values.S3.Secret == "" {
		return Credentials{}, errors.New("вход выполнен, но archive.org не вернул S3-ключи: сгенерируйте их вручную на https://archive.org/account/s3.php")
	}

	creds = Credentials{Access: values.S3.Access, Secret: values.S3.Secret}
	if values.Cookies.LoggedInUser == "" || values.Cookies.LoggedInSig == "" {
		// Keys without cookies are a usable state: Resolve accepts
		// key-only credentials in auto/keys modes, so return the keys and
		// let the caller decide (persist + warn), never lose them.
		return creds, ErrCookiesNotReturned
	}

	creds.CookieUser = values.Cookies.LoggedInUser
	creds.CookieSig = values.Cookies.LoggedInSig

	return creds, nil
}

// refusalError builds the Russian failure message for a success:false
// envelope: a translated message for the documented reasons, otherwise the
// raw reason or the truncated raw body — a refusal without any reason field
// must still surface its raw payload for diagnosis. The email address is
// quoted for not-found (it is not a secret); the password never is.
func refusalError(payload *xauthnResponse, raw []byte, status int, email string) error {
	switch payload.Values.Reason {
	case "account_not_found":
		return fmt.Errorf("аккаунт archive.org с адресом %s не найден", email)
	case "account_bad_password":
		return errors.New("неверный пароль учётной записи archive.org")
	case "":
		detail := payload.Error
		if detail == "" {
			detail = truncate(strings.TrimSpace(string(raw)), maxErrorBody)
		}

		if detail == "" {
			detail = fmt.Sprintf("HTTP %d без тела ответа", status)
		}

		return fmt.Errorf("вход не выполнен, archive.org не указал причину: %s", detail)
	default:
		return fmt.Errorf("вход не выполнен, archive.org вернул причину: %s", payload.Values.Reason)
	}
}

// awaitRateCourtesy sleeps for postLoginPause, returning early (with an
// error) when the context ends first.
func awaitRateCourtesy(ctx context.Context) error {
	if postLoginPause <= 0 {
		return nil
	}

	t := time.NewTimer(postLoginPause)
	defer t.Stop()

	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("пауза после входа archive.org прервана: %w", ctx.Err())
	}
}

// truncate caps s to max runes so error messages stay bounded.
func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}

	cut := 0
	for i := range s {
		if cut == limit {
			return s[:i] + "…"
		}

		cut++
	}

	return s
}

// sanitizeComment strips C0 control characters (including \n, \r, \t) and
// DEL from screenname before it is embedded in a TOML comment, so a hostile
// or malformed value cannot forge config lines. Empty result yields no comment.
func sanitizeComment(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}

		return r
	}, s)
}

// SaveToConfig merges the credentials into the [auth] section of the TOML
// file at path (creating it and its directory when missing), leaving every
// unrelated section byte-compatible in value. The write is atomic
// (temp file + rename) and the file ends with permission 0600. The password
// is not a parameter and is never stored; screenname — a reserved parameter
// for a future caller that has it (e.g. T12 wiring) — is kept as a
// sanitized informational comment when non-empty, because the config schema
// has no field for it (writing a schema key would trip unknown-key detection).
func SaveToConfig(path string, c Credentials, screenname string) error {
	base := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := toml.Unmarshal(data, &base); err != nil {
			return fmt.Errorf("не удалось разобрать файл конфигурации %s: %w", path, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("не удалось прочитать файл конфигурации %s: %w", path, err)
	}

	authSection, ok := base["auth"].(map[string]any)
	if !ok || authSection == nil {
		authSection = map[string]any{}
	}

	authSection["s3_access"] = c.Access
	authSection["s3_secret"] = c.Secret
	authSection["cookie_user"] = c.CookieUser
	authSection["cookie_sig"] = c.CookieSig
	base["auth"] = authSection

	var buf bytes.Buffer
	if screenname != "" {
		fmt.Fprintf(&buf, "# Аккаунт archive.org: %s\n", sanitizeComment(screenname))
	}

	if err := toml.NewEncoder(&buf).Encode(base); err != nil {
		return fmt.Errorf("не удалось сформировать файл конфигурации: %w", err)
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("не удалось создать каталог конфигурации %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".wauploader-*.tmp")
	if err != nil {
		return fmt.Errorf("не удалось создать временный файл конфигурации в %s: %w", dir, err)
	}

	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("не удалось установить права на временный файл конфигурации: %w", err)
	}

	if _, err := tmp.WriteString(buf.String()); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("не удалось записать файл конфигурации %s: %w", path, err)
	}

	if err := tmp.Close(); err != nil {
		return fmt.Errorf("не удалось записать файл конфигурации %s: %w", path, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("не удалось сохранить файл конфигурации %s: %w", path, err)
	}

	tmpName = "" // renamed into place; nothing left to clean up

	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("не удалось установить права 0600 на файл конфигурации %s: %w", path, err)
	}

	return nil
}

// drainAndClose discards the remaining response body so the connection
// returns to the idle pool, then closes it.
func drainAndClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(rc, maxBodyRead))
	_ = rc.Close()
}
