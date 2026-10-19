// Package httpx provides the hardened HTTP client shared by all
// wauploader API layers (auth, SPN2, IA-S3). It pins HTTP/1.1
// transport, applies the mandatory descriptive User-Agent to every
// request (archive.org Bots policy, r3 F3.9), keeps Authorization
// alive across redirects to archive.org hosts only (IA-S3 issues
// frequent 307s; mirrors internetarchive's rebuild_auth, r3 F1.5),
// and offers a context-aware retry ladder (see retry.go).
package httpx

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/4q4r/wauploader/internal/version"
)

// maxRedirects mirrors net/http's default redirect budget; the custom
// CheckRedirect on Client must reproduce it because setting the field
// disables the built-in default.
const maxRedirects = 10

// Client wraps a standard http.Client configured for archive.org
// services. The zero value is not usable; construct with NewClient.
// transport keeps the inner *http.Transport reachable past the
// User-Agent wrapper (same-package white-box access).
type Client struct {
	HTTP      *http.Client
	transport *http.Transport
}

// NewClient builds a client with an HTTP/1.1-only transport, the given
// cookie jar (may be nil for cookie-less API use), and userAgent as
// the default User-Agent header. An empty userAgent falls back to
// version.UserAgent() so a misconfigured caller can never send Go's
// default identifier to archive.org; explicit per-request User-Agent
// headers are never clobbered.
//
// The client-wide Timeout stays zero on purpose: SPN2 and IA-S3 are
// slow, and deadline ownership belongs to callers through per-request
// contexts.
func NewClient(jar http.CookieJar, userAgent string) *Client {
	if userAgent == "" {
		userAgent = version.UserAgent()
	}
	tr := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     false, // IA-S3 documents "only the HTTP 1.1 REST interface"
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second, // IA-S3 needs good 100-Continue support (r2)
	}
	// Pin HTTP/1.1. Go 1.27 made Transport.Protocols a *Protocols with
	// a nil meaning "library default", so allocate before pinning.
	if tr.Protocols == nil {
		tr.Protocols = &http.Protocols{}
	}
	tr.Protocols.SetHTTP1(true)
	tr.Protocols.SetHTTP2(false)

	return &Client{
		HTTP: &http.Client{
			Transport:     &uaTransport{base: tr, userAgent: userAgent},
			Jar:           jar,
			CheckRedirect: checkRedirect,
		},
		transport: tr,
	}
}

// uaTransport injects the client-wide User-Agent into requests that do
// not carry one. RoundTrippers see a fresh request per attempt, so the
// header must be applied here rather than once on the caller's request.
type uaTransport struct {
	base      http.RoundTripper
	userAgent string
}

func (t *uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", t.userAgent)
	}
	return t.base.RoundTrip(req)
}

// checkRedirect follows up to maxRedirects hops and enforces the
// archive.org Authorization policy: the original Authorization header
// is re-attached only while the redirect target is archive.org or a
// .archive.org subdomain (the frequent IA-S3 307 chain), and stripped
// for every other target — including same-hostname hops that net/http
// would normally keep.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if len(via) == 0 {
		return nil
	}
	origAuth := via[0].Header.Get("Authorization")
	if redirectAuthAllowed(req.URL.Hostname()) {
		if origAuth != "" {
			req.Header.Set("Authorization", origAuth)
		}
		return nil
	}
	req.Header.Del("Authorization")
	return nil
}

// redirectAuthAllowed reports whether host may carry credentials on a
// redirect hop: archive.org itself or any subdomain of it.
func redirectAuthAllowed(host string) bool {
	host = strings.ToLower(host)
	return host == "archive.org" || strings.HasSuffix(host, ".archive.org")
}
