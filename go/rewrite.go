package main

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// proxyConfig is everything the forwarding half of piko-expose needs. The
// values come from the command line; see parseFlags in main.go.
type proxyConfig struct {
	// target is the local address traffic is forwarded to, e.g.
	// "127.0.0.1:43120".
	target string

	// stripPrefix is stripped from the request path when it matches, and only
	// when it matches. Subdomain mode leaves it empty: the whole origin
	// belongs to the endpoint, so paths arrive exactly as the browser sent
	// them. Path mode sets it to "/<endpoint>".
	stripPrefix string

	// preserveHost keeps the browser's Host header instead of rewriting it to
	// the target. The public server's SUBDOMAIN_PRESERVE_HOST=true (default)
	// forwards the public hostname, and DSH wants to see it so its
	// --trusted-host allowance and its Origin==Host fence both pass.
	//
	// When false, Host *and* Origin/Referer are rewritten to the target: DSH
	// only compares Origin against Host, so rewriting Host alone would just
	// turn one 403 into another.
	preserveHost bool

	// authUser/authPass enable HTTP Basic Auth in front of the tunnel. An
	// empty authPass disables it.
	authUser string
	authPass string
}

// errNoTarget is returned when the proxy is built without a target address.
var errNoTarget = errors.New("proxy: target address is required")

// newHandler builds the http.Handler that serves one piko endpoint: optional
// Basic Auth in front of a reverse proxy that can strip a path prefix and
// rewrite the host.
//
// WebSocket upgrades are passed through by httputil.ReverseProxy, which the
// DSH GUI depends on: its stream lives on /api/remote.mux.
func newHandler(cfg proxyConfig) (http.Handler, error) {
	if cfg.target == "" {
		return nil, errNoTarget
	}

	targetURL, err := url.Parse("http://" + cfg.target)
	if err != nil {
		return nil, fmt.Errorf("parse target %q: %w", cfg.target, err)
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(targetURL)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()

			// ReverseProxy sanitises the outbound query with cleanQueryParams
			// *after* Rewrite runs, and that pass re-encodes any query whose
			// first key is not `k=v`. DSH's client-module bundles are fetched
			// through exactly such a URL — /plugins/??a.js,b.js&rev=… — so the
			// sanitised form requests a different module set and the client
			// fails with "HTML did not preload …". We never interpret query
			// parameters, so the original bytes are both safe and required.
			pr.Out.URL.RawQuery = pr.In.URL.RawQuery

			pr.Out.URL.Path = stripPrefixPath(pr.Out.URL.Path, cfg.stripPrefix)
			if pr.Out.URL.Path == pr.In.URL.Path {
				// Nothing stripped, so the original percent-encoding still
				// describes this path and should be kept.
				pr.Out.URL.RawPath = pr.In.URL.RawPath
			} else {
				// The path changed, which makes the old RawPath stale.
				pr.Out.URL.RawPath = ""
			}

			if !cfg.preserveHost {
				localizeRequest(pr, targetURL)
			}
		},
		// A small flush interval keeps streamed responses (SSE, chunked agent
		// output) prompt without flushing on every single write: over a
		// long-RTT link, one flush per write turns a large streamed body into
		// thousands of tiny writes and throughput collapses.
		FlushInterval: 100 * time.Millisecond,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, fmt.Sprintf("piko-expose: upstream error: %v", err), http.StatusBadGateway)
		},
	}

	var handler http.Handler = proxy
	if cfg.authPass != "" {
		handler = basicAuth(handler, cfg.authUser, cfg.authPass)
	}
	return handler, nil
}

// stripPrefixPath removes prefix from path when, and only when, path is the
// prefix itself or a proper descendant of it.
//
// The boundary matters: "/dsh-a1b2c3" must strip "/dsh-a1b2c3", but
// "/dsh-a1b2c3-extra/x" and "/api/x" must pass through untouched. This mirrors
// gotty-piko's sanitizeSession/opencode-piko middleware behaviour.
func stripPrefixPath(path, prefix string) string {
	if prefix == "" || prefix == "/" {
		return path
	}
	prefix = "/" + strings.Trim(prefix, "/")

	switch {
	case path == prefix:
		return "/"
	case strings.HasPrefix(path, prefix+"/"):
		return strings.TrimPrefix(path, prefix)
	default:
		return path
	}
}

// localizeRequest rewrites the request so a target that only trusts loopback
// sees a loopback request. DSH refuses a request when Origin.host differs from
// Host.host, so both have to move together, and Referer follows so that
// relative redirects and same-origin checks stay coherent.
func localizeRequest(pr *httputil.ProxyRequest, targetURL *url.URL) {
	pr.Out.Host = targetURL.Host

	for _, header := range []string{"Origin", "Referer"} {
		value := pr.In.Header.Get(header)
		if value == "" {
			continue
		}
		u, err := url.Parse(value)
		if err != nil {
			continue
		}
		u.Scheme = targetURL.Scheme
		u.Host = targetURL.Host
		pr.Out.Header.Set(header, u.String())
	}
}

// basicAuth wraps handler with HTTP Basic Auth. A missing or wrong credential
// gets a 401 that does not reveal whether the user or the password was wrong;
// the comparison itself is constant time.
func basicAuth(next http.Handler, user, pass string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, ok := r.BasicAuth()
		userOK := subtle.ConstantTimeCompare([]byte(gotUser), []byte(user)) == 1
		passOK := subtle.ConstantTimeCompare([]byte(gotPass), []byte(pass)) == 1
		if !ok || !userOK || !passOK {
			w.Header().Set("WWW-Authenticate", `Basic realm="piko-remote", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
