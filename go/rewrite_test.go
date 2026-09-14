package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestStripPrefixPath(t *testing.T) {
	cases := []struct {
		name   string
		path   string
		prefix string
		want   string
	}{
		{"no prefix configured keeps path", "/api/x", "", "/api/x"},
		{"root prefix keeps path", "/api/x", "/", "/api/x"},
		{"exact prefix becomes root", "/dsh-a1b2c3", "/dsh-a1b2c3", "/"},
		{"descendant strips", "/dsh-a1b2c3/api/remote.mux", "/dsh-a1b2c3", "/api/remote.mux"},
		{"prefix without leading slash is normalised", "/ep/x", "ep", "/x"},
		{"prefix with trailing slash is normalised", "/ep/x", "/ep/", "/x"},
		{"sibling endpoint untouched", "/dsh-a1b2c3-extra/x", "/dsh-a1b2c3", "/dsh-a1b2c3-extra/x"},
		{"unrelated first segment untouched", "/api/x", "/dsh-a1b2c3", "/api/x"},
		{"subdomain root untouched", "/", "/dsh-a1b2c3", "/"},
		{"longer similar segment untouched", "/epx", "/ep", "/epx"},
		{"prefix as substring of deeper segment untouched", "/x/ep", "/ep", "/x/ep"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := stripPrefixPath(tc.path, tc.prefix); got != tc.want {
				t.Fatalf("stripPrefixPath(%q, %q) = %q, want %q", tc.path, tc.prefix, got, tc.want)
			}
		})
	}
}

// seenRequest is what the target server observed, which is how the tests check
// the rewriting rather than trusting the proxy's own bookkeeping.
type seenRequest struct {
	Path     string
	Host     string
	Origin   string
	Referer  string
	FwdHost  string
	RawQuery string
}

// newRecordingTarget starts a local target that records the last request.
func newRecordingTarget(t *testing.T) (*httptest.Server, *seenRequest) {
	t.Helper()
	seen := &seenRequest{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Path = r.URL.Path
		seen.Host = r.Host
		seen.Origin = r.Header.Get("Origin")
		seen.Referer = r.Header.Get("Referer")
		seen.FwdHost = r.Header.Get("X-Forwarded-Host")
		seen.RawQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("target:" + r.URL.Path))
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

func targetAddr(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestHandlerForwardsAndStripsPrefix(t *testing.T) {
	srv, seen := newRecordingTarget(t)
	handler, err := newHandler(proxyConfig{
		target:       targetAddr(t, srv),
		stripPrefix:  "/dsh-a1b2c3",
		preserveHost: true,
	})
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://dsh-a1b2c3.clauded.friddle.me/dsh-a1b2c3/api/remote.mux?x=1", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if seen.Path != "/api/remote.mux" {
		t.Fatalf("target saw path %q, want /api/remote.mux", seen.Path)
	}
	if seen.RawQuery != "x=1" {
		t.Fatalf("target saw query %q, want x=1", seen.RawQuery)
	}
}

func TestHandlerPreservesHostByDefault(t *testing.T) {
	srv, seen := newRecordingTarget(t)
	handler, err := newHandler(proxyConfig{
		target:       targetAddr(t, srv),
		preserveHost: true,
	})
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://dsh-a1b2c3.clauded.friddle.me/", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	// DSH's browser fence compares Origin.host against Host.host, so the public
	// hostname has to survive the hop.
	if seen.Host != "dsh-a1b2c3.clauded.friddle.me" {
		t.Fatalf("target saw Host %q, want the public hostname", seen.Host)
	}
	if seen.FwdHost != "dsh-a1b2c3.clauded.friddle.me" {
		t.Fatalf("X-Forwarded-Host = %q, want the public hostname", seen.FwdHost)
	}
}

func TestHandlerLocalizesHostOriginAndReferer(t *testing.T) {
	srv, seen := newRecordingTarget(t)
	addr := targetAddr(t, srv)
	handler, err := newHandler(proxyConfig{
		target:       addr,
		preserveHost: false,
	})
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "https://dsh-a1b2c3.clauded.friddle.me/api/x", nil)
	req.Header.Set("Origin", "https://dsh-a1b2c3.clauded.friddle.me")
	req.Header.Set("Referer", "https://dsh-a1b2c3.clauded.friddle.me/some/page")
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if seen.Host != addr {
		t.Fatalf("Host = %q, want %q", seen.Host, addr)
	}
	// Rewriting Host alone would leave Origin != Host, which DSH rejects too.
	if seen.Origin != "http://"+addr {
		t.Fatalf("Origin = %q, want http://%s", seen.Origin, addr)
	}
	if !strings.HasPrefix(seen.Referer, "http://"+addr+"/some/page") {
		t.Fatalf("Referer = %q, want it rebased onto http://%s", seen.Referer, addr)
	}
}

func TestHandlerBasicAuth(t *testing.T) {
	srv, _ := newRecordingTarget(t)
	handler, err := newHandler(proxyConfig{
		target:       targetAddr(t, srv),
		preserveHost: true,
		authUser:     "k3f9qz",
		authPass:     "s3cret-passphrase",
	})
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}

	t.Run("missing credentials are rejected", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://ep.example/", nil))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
		if got := rec.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic ") {
			t.Fatalf("WWW-Authenticate = %q, want a Basic challenge", got)
		}
	})

	t.Run("wrong password is rejected", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "https://ep.example/", nil)
		req.SetBasicAuth("k3f9qz", "nope")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})

	t.Run("right credentials pass", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "https://ep.example/", nil)
		req.SetBasicAuth("k3f9qz", "s3cret-passphrase")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
	})
}

func TestHandlerAuthDisabledWithoutPassword(t *testing.T) {
	srv, _ := newRecordingTarget(t)
	handler, err := newHandler(proxyConfig{target: targetAddr(t, srv), preserveHost: true})
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://ep.example/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 when no password is configured", rec.Code)
	}
}

func TestNewHandlerRequiresTarget(t *testing.T) {
	if _, err := newHandler(proxyConfig{}); err == nil {
		t.Fatal("newHandler with no target should fail")
	}
}

func TestParseFlags(t *testing.T) {
	t.Run("defaults produce a random username and password", func(t *testing.T) {
		opts, err := parseFlags([]string{"--endpoint", "dsh-a1b2c3", "--target", "43120"})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if opts.target != "127.0.0.1:43120" {
			t.Fatalf("target = %q, want 127.0.0.1:43120", opts.target)
		}
		if opts.authUser == "" || opts.authPass == "" {
			t.Fatalf("expected generated credentials, got user=%q pass=%q", opts.authUser, opts.authPass)
		}
		if len(opts.authPass) != 20 {
			t.Fatalf("password length = %d, want 20", len(opts.authPass))
		}
		if opts.stripPrefix != "" {
			t.Fatalf("subdomain mode should not strip, got %q", opts.stripPrefix)
		}
		url, err := opts.remoteURL()
		if err != nil {
			t.Fatalf("remoteURL: %v", err)
		}
		want := "https://dsh-a1b2c3." + strings.TrimPrefix(defaultRemote, "https://") + "/"
		if url != want {
			t.Fatalf("remoteURL = %q, want %q", url, want)
		}
	})

	t.Run("path mode derives the prefix and the URL", func(t *testing.T) {
		opts, err := parseFlags([]string{
			"--endpoint", "dsh-a1b2c3", "--target", "127.0.0.1:43120", "--url-mode", "path",
		})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if opts.stripPrefix != "/dsh-a1b2c3" {
			t.Fatalf("stripPrefix = %q, want /dsh-a1b2c3", opts.stripPrefix)
		}
		url, err := opts.remoteURL()
		if err != nil {
			t.Fatalf("remoteURL: %v", err)
		}
		if url != "https://clauded.friddle.me/dsh-a1b2c3/" {
			t.Fatalf("remoteURL = %q", url)
		}
	})

	t.Run("uppercase endpoints are normalised and bad ones rejected", func(t *testing.T) {
		opts, err := parseFlags([]string{"--endpoint", "DSH-A1B2C3", "--target", "43120"})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if opts.endpoint != "dsh-a1b2c3" {
			t.Fatalf("endpoint = %q, want dsh-a1b2c3", opts.endpoint)
		}
		for _, bad := range []string{"dsh_a1b2c3", "-leading", "trailing-", "has space", "dot.name"} {
			if _, err := parseFlags([]string{"--endpoint", bad, "--target", "43120"}); err == nil {
				t.Fatalf("endpoint %q should have been rejected", bad)
			}
		}
	})

	t.Run("endpoint and target are required", func(t *testing.T) {
		if _, err := parseFlags([]string{"--target", "43120"}); err == nil {
			t.Fatal("missing --endpoint should fail")
		}
		if _, err := parseFlags([]string{"--endpoint", "dsh-a1b2c3"}); err == nil {
			t.Fatal("missing --target should fail")
		}
	})

	t.Run("remote without a scheme gets https", func(t *testing.T) {
		opts, err := parseFlags([]string{
			"--endpoint", "ep", "--target", "43120", "--remote", "clauded.friddle.me",
		})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if opts.remote != "https://clauded.friddle.me" {
			t.Fatalf("remote = %q", opts.remote)
		}
	})

	t.Run("auth can be disabled", func(t *testing.T) {
		opts, err := parseFlags([]string{"--endpoint", "ep", "--target", "43120", "--auth=false"})
		if err != nil {
			t.Fatalf("parseFlags: %v", err)
		}
		if opts.authUser != "" || opts.authPass != "" {
			t.Fatalf("expected no credentials, got user=%q pass=%q", opts.authUser, opts.authPass)
		}
	})
}

func TestRandomStringIsUnbiasedAndBounded(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		got, err := randomString(8, userAlphabet)
		if err != nil {
			t.Fatalf("randomString: %v", err)
		}
		if len(got) != 8 {
			t.Fatalf("length = %d, want 8", len(got))
		}
		for _, r := range got {
			if !strings.ContainsRune(userAlphabet, r) {
				t.Fatalf("character %q is outside the alphabet", r)
			}
		}
		seen[got] = true
	}
	if len(seen) < 40 {
		t.Fatalf("only %d distinct values out of 50 draws", len(seen))
	}
}
