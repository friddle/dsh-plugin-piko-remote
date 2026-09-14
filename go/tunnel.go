package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/andydunstall/piko/client"
)

// A client.Listener is a net.Listener, which is exactly what lets net/http
// serve an endpoint as if it were a socket.
var _ net.Listener = client.Listener(nil)

// dialUpstream opens the outbound-only connection to the piko server and
// listens on the endpoint.
//
// Nothing here binds a public port: piko multiplexes browser connections back
// over this single outbound WebSocket, which is why the tunnel works from
// behind NAT with no port forwarding.
func dialUpstream(ctx context.Context, opts options) (client.Listener, error) {
	remoteURL, err := url.Parse(opts.remote)
	if err != nil {
		return nil, fmt.Errorf("parse remote %q: %w", opts.remote, err)
	}

	upstream := &client.Upstream{
		URL:   remoteURL,
		Token: opts.upstreamKey,
	}
	if opts.insecure {
		upstream.TLSConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // explicit debug flag
	}

	ln, err := upstream.Listen(ctx, opts.endpoint)
	if err != nil {
		return nil, fmt.Errorf("listen on endpoint %q via %s: %w", opts.endpoint, opts.remote, err)
	}
	return ln, nil
}

// newHTTPServer wraps the tunnel handler. ReadHeaderTimeout bounds a stalled
// client without capping long-lived streams.
func newHTTPServer(handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
	}
}

// serve runs srv on ln and reports the first meaningful Serve error on the
// returned channel. A clean shutdown reports nil.
func serve(srv *http.Server, ln net.Listener) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		err := srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			err = nil
		}
		errCh <- err
	}()
	return errCh
}
