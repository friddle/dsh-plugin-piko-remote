package main

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseRange(t *testing.T) {
	cases := []struct {
		header    string
		size      int64
		wantStart int64
		wantEnd   int64
		wantOK    bool
	}{
		{"", 100, 0, 0, false},
		{"bytes=0-9", 100, 0, 9, true},
		{"bytes=90-", 100, 90, 99, true},
		{"bytes=-10", 100, 90, 99, true},
		{"bytes=90-200", 100, 90, 99, true},
		{"bytes=100-", 100, 0, 0, false},
		{"bytes=50-10", 100, 0, 0, false},
		{"bytes=abc-def", 100, 0, 0, false},
		{"bytes=0-9,20-29", 100, 0, 0, false},
		{"items=0-9", 100, 0, 0, false},
		{"bytes=-5", 3, 0, 2, true},
	}
	for _, tc := range cases {
		start, end, ok := parseRange(tc.header, tc.size)
		if ok != tc.wantOK || (ok && (start != tc.wantStart || end != tc.wantEnd)) {
			t.Errorf("parseRange(%q, %d) = (%d, %d, %v), want (%d, %d, %v)",
				tc.header, tc.size, start, end, ok, tc.wantStart, tc.wantEnd, tc.wantOK)
		}
	}
}

func TestCacheable(t *testing.T) {
	cases := []struct {
		name   string
		status int
		header http.Header
		want   bool
	}{
		{"immutable bundle", 200, http.Header{"Cache-Control": {"public, max-age=31536000, immutable"}}, true},
		{"no-cache", 200, http.Header{"Cache-Control": {"no-cache"}}, false},
		{"no-store", 200, http.Header{"Cache-Control": {"no-store"}}, false},
		{"private", 200, http.Header{"Cache-Control": {"private, max-age=60"}}, false},
		{"max-age=0", 200, http.Header{"Cache-Control": {"public, max-age=0"}}, false},
		{"sets a cookie", 200, http.Header{"Cache-Control": {"public, max-age=60"}, "Set-Cookie": {"a=b"}}, false},
		{"event stream", 200, http.Header{"Cache-Control": {"public, max-age=60"}, "Content-Type": {"text/event-stream"}}, false},
		{"error", 500, http.Header{"Cache-Control": {"public, max-age=60"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, ok := cacheable(tc.status, tc.header); ok != tc.want {
				t.Fatalf("cacheable = %v, want %v", ok, tc.want)
			}
		})
	}
}

// cacheFixture is an upstream that counts hits and serves a fixed body.
func cacheFixture(t *testing.T, body string, header http.Header) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		for name, values := range header {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(upstream.Close)
	return upstream, &hits
}

func cacheHandler(t *testing.T, upstream *httptest.Server, maxEntry int64) http.Handler {
	t.Helper()
	handler, err := newHandler(proxyConfig{
		target:       strings.TrimPrefix(upstream.URL, "http://"),
		preserveHost: true,
		cache:        newResponseCache(maxEntry, maxEntry*4),
	})
	if err != nil {
		t.Fatalf("newHandler: %v", err)
	}
	return handler
}

func TestCacheAddsContentLengthAndReusesTheBody(t *testing.T) {
	body := strings.Repeat("client-module-source;", 4096)
	upstream, hits := cacheFixture(t, body, http.Header{
		"Cache-Control": {"public, max-age=31536000, immutable"},
		"Content-Type":  {"text/javascript"},
	})
	handler := cacheHandler(t, upstream, 1<<20)

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "https://ep.example/plugins/??a.js,b.js&rev=1", nil))

	if first.Code != http.StatusOK {
		t.Fatalf("status = %d", first.Code)
	}
	if got := first.Header().Get("Content-Length"); got != strconv.Itoa(len(body)) {
		// Without this the client cannot tell a truncated body from a short one.
		t.Fatalf("Content-Length = %q, want %d", got, len(body))
	}
	if got := first.Body.String(); got != body {
		t.Fatalf("body mismatch: %d bytes vs %d", len(got), len(body))
	}
	if got := first.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q", got)
	}

	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "https://ep.example/plugins/??a.js,b.js&rev=1", nil))
	if second.Body.String() != body {
		t.Fatal("cached body mismatch")
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Fatalf("upstream hits = %d, want 1 (the second request should be served from cache)", got)
	}
}

func TestCacheServesRanges(t *testing.T) {
	body := "0123456789"
	upstream, _ := cacheFixture(t, body, http.Header{
		"Cache-Control": {"public, max-age=3600"},
		"Content-Type":  {"text/javascript"},
	})
	handler := cacheHandler(t, upstream, 1<<20)

	// Prime the cache.
	handler.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "https://ep.example/plugins/??a.js,b.js&rev=1", nil))

	req := httptest.NewRequest(http.MethodGet, "https://ep.example/plugins/??a.js,b.js&rev=1", nil)
	req.Header.Set("Range", "bytes=4-7")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("status = %d, want 206", rec.Code)
	}
	if got := rec.Body.String(); got != "4567" {
		t.Fatalf("body = %q, want 4567", got)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes 4-7/10" {
		t.Fatalf("Content-Range = %q", got)
	}
}

func TestCacheNegotiatesGzipWithCorrectLength(t *testing.T) {
	body := strings.Repeat("const x = 1;\n", 500)
	upstream, _ := cacheFixture(t, body, http.Header{
		"Cache-Control": {"public, max-age=3600"},
		"Content-Type":  {"text/javascript"},
	})
	handler := cacheHandler(t, upstream, 1<<20)

	req := httptest.NewRequest(http.MethodGet, "https://ep.example/bundle.js", nil)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if got := rec.Header().Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q", got)
	}
	compressed := rec.Body.Bytes()
	if got := rec.Header().Get("Content-Length"); got != strconv.Itoa(len(compressed)) {
		t.Fatalf("Content-Length = %q, want the compressed length %d", got, len(compressed))
	}
	reader, err := gzip.NewReader(strings.NewReader(string(compressed)))
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	decoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(decoded) != body {
		t.Fatalf("decoded body mismatch: %d vs %d bytes", len(decoded), len(body))
	}
}

func TestCacheSkipsUncacheableResponses(t *testing.T) {
	upstream, hits := cacheFixture(t, "dynamic", http.Header{"Cache-Control": {"no-store"}})
	handler := cacheHandler(t, upstream, 1<<20)

	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://ep.example/api/state", nil))
		if rec.Body.String() != "dynamic" {
			t.Fatalf("body = %q", rec.Body.String())
		}
	}
	if got := atomic.LoadInt32(hits); got != 2 {
		t.Fatalf("upstream hits = %d, want 2 (nothing should have been cached)", got)
	}
}

func TestCacheStreamsResponsesLargerThanTheLimit(t *testing.T) {
	body := strings.Repeat("x", 5000)
	upstream, _ := cacheFixture(t, body, http.Header{"Cache-Control": {"public, max-age=3600"}})
	handler := cacheHandler(t, upstream, 1024) // limit below the body size

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "https://ep.example/big.js", nil))

	if len(rec.Body.String()) != len(body) {
		t.Fatalf("streamed body = %d bytes, want %d (overflow must still deliver everything)", len(rec.Body.String()), len(body))
	}
}

func TestCacheEvictsWhenOverBudget(t *testing.T) {
	cache := newResponseCache(1<<20, 10)
	entry := func(body string, expires time.Time) *cacheEntry {
		return &cacheEntry{status: 200, header: http.Header{}, body: []byte(body), expires: expires}
	}
	future := time.Now().Add(time.Hour)

	cache.put("a", entry("1234567", future))
	cache.put("b", entry("8901234", future))

	if cache.get("a") != nil {
		t.Fatal("the oldest entry should have been evicted to stay within budget")
	}
	if cache.get("b") == nil {
		t.Fatal("the newest entry should still be present")
	}
}

func TestCacheIgnoresExpiredEntries(t *testing.T) {
	cache := newResponseCache(1<<20, 1<<20)
	cache.put("a", &cacheEntry{status: 200, header: http.Header{}, body: []byte("x"), expires: time.Now().Add(-time.Second)})
	if cache.get("a") != nil {
		t.Fatal("an expired entry must not be served")
	}
}
