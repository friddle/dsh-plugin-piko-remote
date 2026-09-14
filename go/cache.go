package main

// A small response cache for the tunnel.
//
// Why this exists: DSH's Web UI pulls one concatenated client-module bundle that
// is ~11MB of JavaScript. It streams from the app with no Content-Length, so
// when the upstream piko connection drops mid-body — which it does, regularly,
// over a long-haul link — the truncation is invisible: the client sees a
// "complete" response and imports half a script. The app then reports
// "Failed to load plugins".
//
// Buffering cacheable responses here fixes the shape of that failure:
//
//   - the response gets an accurate Content-Length, so a cut stream is a failed
//     request the client retries instead of a silently corrupt script;
//   - the retry is served from memory, so retrying costs the client bandwidth
//     but not another trip through the app;
//   - Range requests are honoured, so a client that resumes can.
//
// It is deliberately not a general HTTP cache: only GET, only 200, only
// responses that explicitly allow caching, and never anything that sets a
// cookie.

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// hopByHopHeaders are connection-scoped and must not be replayed.
var hopByHopHeaders = []string{
	"Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// cacheEntry is one buffered upstream response.
type cacheEntry struct {
	status  int
	header  http.Header
	body    []byte
	expires time.Time
	// gzipped is filled lazily, once, under the cache mutex.
	gzipped []byte
}

// responseCache stores a bounded number of buffered responses.
type responseCache struct {
	mu       sync.Mutex
	entries  map[string]*cacheEntry
	order    []string
	total    int64
	maxEntry int64
	maxTotal int64
}

// newResponseCache returns a cache holding at most maxEntry per response and
// maxTotal bytes overall, evicting least-recently-stored entries first.
func newResponseCache(maxEntry, maxTotal int64) *responseCache {
	return &responseCache{
		entries:  make(map[string]*cacheEntry),
		maxEntry: maxEntry,
		maxTotal: maxTotal,
	}
}

// get returns a live entry for key.
func (c *responseCache) get(key string) *cacheEntry {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	if !ok {
		return nil
	}
	if !entry.expires.IsZero() && time.Now().After(entry.expires) {
		c.removeLocked(key)
		return nil
	}
	return entry
}

// put stores an entry, evicting older ones to stay within budget.
func (c *responseCache) put(key string, entry *cacheEntry) {
	if c == nil || int64(len(entry.body)) > c.maxEntry {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.entries[key]; ok {
		c.total -= int64(len(existing.body))
		delete(c.entries, key)
	}
	c.entries[key] = entry
	c.order = append(c.order, key)
	c.total += int64(len(entry.body))

	for c.total > c.maxTotal && len(c.order) > 0 {
		c.removeLocked(c.order[0])
	}
}

func (c *responseCache) removeLocked(key string) {
	entry, ok := c.entries[key]
	if ok {
		c.total -= int64(len(entry.body))
		delete(c.entries, key)
	}
	for i, k := range c.order {
		if k == key {
			c.order = append(c.order[:i], c.order[i+1:]...)
			break
		}
	}
}

// serve writes the cached response, honouring Range and gzip negotiation.
//
// Ranges are computed over whichever representation is served, which is what
// RFC 9110 requires: with Content-Encoding present, byte ranges refer to the
// encoded body.
func (e *cacheEntry) serve(w http.ResponseWriter, r *http.Request) {
	body, encoding := e.body, ""
	if acceptsGzip(r) && len(e.body) >= gzipThreshold {
		body, encoding = e.gzipBody(), "gzip"
	}

	header := w.Header()
	for name, values := range e.header {
		for _, value := range values {
			header.Add(name, value)
		}
	}
	if encoding != "" {
		header.Set("Content-Encoding", encoding)
	} else {
		header.Del("Content-Encoding")
	}
	header.Set("Accept-Ranges", "bytes")
	header.Del("Transfer-Encoding")

	if start, end, ok := parseRange(r.Header.Get("Range"), int64(len(body))); ok {
		slice := body[start : end+1]
		header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
		header.Set("Content-Length", strconv.Itoa(len(slice)))
		w.WriteHeader(http.StatusPartialContent)
		if r.Method != http.MethodHead {
			_, _ = w.Write(slice)
		}
		return
	}

	if r.Header.Get("Range") != "" {
		// Unsatisfiable range: the client asked for bytes this representation
		// does not have.
		header.Set("Content-Range", fmt.Sprintf("bytes */%d", len(body)))
		header.Set("Content-Length", "0")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}

	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(e.status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

// gzipBody compresses once and remembers the result.
func (e *cacheEntry) gzipBody() []byte {
	if e.gzipped != nil {
		return e.gzipped
	}
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	_, _ = writer.Write(e.body)
	_ = writer.Close()
	e.gzipped = buffer.Bytes()
	return e.gzipped
}

// gzipThreshold keeps tiny responses uncompressed, where gzip can grow them.
const gzipThreshold = 1024

// parseRange interprets a single byte range against a body of the given size.
func parseRange(header string, size int64) (start, end int64, ok bool) {
	if header == "" || !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(header, "bytes=")
	if strings.Contains(spec, ",") {
		// Multi-range would need multipart/byteranges; serving the whole body is
		// always a valid answer, so fall back to it by reporting "no range".
		return 0, 0, false
	}
	dash := strings.Index(spec, "-")
	if dash < 0 {
		return 0, 0, false
	}
	first, last := strings.TrimSpace(spec[:dash]), strings.TrimSpace(spec[dash+1:])

	switch {
	case first == "":
		// Suffix range: the last N bytes.
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, size > 0
	case last == "":
		from, err := strconv.ParseInt(first, 10, 64)
		if err != nil || from < 0 || from >= size {
			return 0, 0, false
		}
		return from, size - 1, true
	default:
		from, err1 := strconv.ParseInt(first, 10, 64)
		to, err2 := strconv.ParseInt(last, 10, 64)
		if err1 != nil || err2 != nil || from < 0 || from > to {
			return 0, 0, false
		}
		if from >= size {
			return 0, 0, false
		}
		if to >= size {
			to = size - 1
		}
		return from, to, true
	}
}

// isUpgrade reports whether the request asks to switch protocols.
func isUpgrade(r *http.Request) bool {
	if r.Header.Get("Upgrade") != "" {
		return true
	}
	for _, value := range r.Header.Values("Connection") {
		if strings.Contains(strings.ToLower(value), "upgrade") {
			return true
		}
	}
	return false
}

// acceptsGzip reports whether the client asked for gzip.
func acceptsGzip(r *http.Request) bool {
	for _, value := range r.Header.Values("Accept-Encoding") {
		for _, part := range strings.Split(value, ",") {
			encoding := strings.TrimSpace(part)
			if index := strings.Index(encoding, ";"); index >= 0 {
				encoding = strings.TrimSpace(encoding[:index])
			}
			if strings.EqualFold(encoding, "gzip") || encoding == "*" {
				return true
			}
		}
	}
	return false
}

// cacheDirective reports whether a response may be stored, and until when.
func cacheable(status int, header http.Header) (time.Time, bool) {
	if status != http.StatusOK {
		return time.Time{}, false
	}
	if header.Get("Set-Cookie") != "" {
		return time.Time{}, false
	}
	if strings.HasPrefix(header.Get("Content-Type"), "text/event-stream") {
		return time.Time{}, false
	}
	directives := strings.ToLower(header.Get("Cache-Control"))
	if strings.Contains(directives, "no-store") || strings.Contains(directives, "private") {
		return time.Time{}, false
	}
	for _, directive := range strings.Split(directives, ",") {
		directive = strings.TrimSpace(directive)
		if !strings.HasPrefix(directive, "max-age=") {
			continue
		}
		seconds, err := strconv.Atoi(strings.TrimPrefix(directive, "max-age="))
		if err != nil || seconds <= 0 {
			return time.Time{}, false
		}
		return time.Now().Add(time.Duration(seconds) * time.Second), true
	}
	return time.Time{}, false
}

// bufferingWriter collects a response up to a limit, then falls back to
// streaming it straight through.
//
// The fallback matters: a response larger than the limit (or one that never
// ends, like a stream) must still be delivered, and by the time we know that, we
// have already buffered its first bytes.
type bufferingWriter struct {
	dst      http.ResponseWriter
	limit    int64
	buffer   bytes.Buffer
	status   int
	header   http.Header
	overflow bool
	wrote    bool
}

func newBufferingWriter(dst http.ResponseWriter, limit int64) *bufferingWriter {
	return &bufferingWriter{dst: dst, limit: limit, header: make(http.Header)}
}

func (b *bufferingWriter) Header() http.Header { return b.header }

func (b *bufferingWriter) WriteHeader(status int) {
	if b.overflow {
		b.dst.WriteHeader(status)
		return
	}
	if b.wrote {
		return
	}
	b.status = status
	b.wrote = true
}

func (b *bufferingWriter) Write(chunk []byte) (int, error) {
	if b.overflow {
		return b.dst.Write(chunk)
	}
	if int64(b.buffer.Len()+len(chunk)) > b.limit {
		b.overflow = true
		if !b.wrote {
			b.status = http.StatusOK
		}
		b.replayHeaders()
		b.dst.WriteHeader(b.status)
		if _, err := b.dst.Write(b.buffer.Bytes()); err != nil {
			return 0, err
		}
		b.buffer.Reset()
		return b.dst.Write(chunk)
	}
	if !b.wrote {
		b.status = http.StatusOK
		b.wrote = true
	}
	return b.buffer.Write(chunk)
}

// Flush is a no-op while buffering: nothing may reach the client until the
// response is known to be storable, or we would have to send it twice.
func (b *bufferingWriter) Flush() {
	if b.overflow {
		if flusher, ok := b.dst.(http.Flusher); ok {
			flusher.Flush()
		}
	}
}

func (b *bufferingWriter) replayHeaders() {
	for name, values := range b.header {
		for _, value := range values {
			b.dst.Header().Add(name, value)
		}
	}
}

// finish completes the response when the whole body was buffered but could not
// be cached. The length is set explicitly: without it, Go would fall back to
// chunked framing and the client would again be unable to tell a truncated body
// from a complete one.
func (b *bufferingWriter) finish() {
	if b.overflow {
		return
	}
	status := b.status
	if status == 0 {
		status = http.StatusOK
	}
	body := b.buffer.Bytes()
	b.replayHeaders()
	b.dst.Header().Set("Content-Length", strconv.Itoa(len(body)))
	b.dst.WriteHeader(status)
	if _, err := b.dst.Write(body); err != nil {
		return
	}
}

// entry builds a cache entry from a fully buffered response, or reports why it
// cannot be cached.
func (b *bufferingWriter) entry() (*cacheEntry, bool) {
	if b.overflow {
		return nil, false
	}
	expires, ok := cacheable(b.status, b.header)
	if !ok {
		return nil, false
	}
	header := make(http.Header, len(b.header))
	for name, values := range b.header {
		if containsFold(hopByHopHeaders, name) || strings.EqualFold(name, "Content-Length") {
			continue
		}
		header[name] = append([]string(nil), values...)
	}
	return &cacheEntry{
		status:  b.status,
		header:  header,
		body:    append([]byte(nil), b.buffer.Bytes()...),
		expires: expires,
	}, true
}

func containsFold(haystack []string, needle string) bool {
	for _, item := range haystack {
		if strings.EqualFold(item, needle) {
			return true
		}
	}
	return false
}

// withResponseCache wraps a handler with the cache.
func withResponseCache(next http.Handler, cache *responseCache) http.Handler {
	if cache == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		// Protocol upgrades and event streams must never be buffered. An upgrade
		// needs the ResponseWriter's Hijacker to take over the connection, which
		// a buffering writer cannot provide: catching it here would break the
		// DSH client's live stream.
		if isUpgrade(r) || strings.HasPrefix(r.Header.Get("Accept"), "text/event-stream") {
			next.ServeHTTP(w, r)
			return
		}

		key := r.Method + " " + r.URL.RequestURI()
		if entry := cache.get(key); entry != nil {
			entry.serve(w, r)
			return
		}

		// Ask upstream for the identity representation so what we store can be
		// re-encoded per client later.
		upstream := r.Clone(r.Context())
		upstream.Header.Set("Accept-Encoding", "identity")

		recorder := newBufferingWriter(w, cache.maxEntry)
		next.ServeHTTP(recorder, upstream)

		if entry, ok := recorder.entry(); ok {
			cache.put(key, entry)
			// Serve through the cache path so this very response gets the same
			// framing, compression and range handling as a cache hit.
			if stored := cache.get(key); stored != nil {
				stored.serve(w, r)
				return
			}
		}
		recorder.finish()
	})
}
