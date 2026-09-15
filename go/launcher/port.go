package main

// DSH's browser cookie is bound to the authority it was issued for, and with
// host rewriting that authority is `127.0.0.1:<port>` — the port included. An
// OS-assigned port therefore changes on every boot, which invalidates every
// existing cookie: the operator's bookmark and open tabs suddenly answer
// `401 dsh web authentication required` and they have to reopen the printed
// token URL. Reusing the port the profile last ran on keeps them signed in.

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
)

// portFromURL extracts the port from a URL such as
// `http://127.0.0.1:38235/?token=…`, or 0 when there is none.
//
// @param raw - the recorded local URL.
// @returns the port number, or 0.
func portFromURL(raw string) int {
	parsed, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil || port < 1 || port > 65535 {
		return 0
	}
	return port
}

// portFree reports whether a loopback port can still be bound.
//
// @param port - candidate port.
// @returns true when the port is available on 127.0.0.1.
func portFree(port int) bool {
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = listener.Close()
	return true
}

// resolveWebPort picks the port this run should use.
//
// An explicit `--port` always wins. Otherwise the port recorded by the previous
// run of this profile is reused when it is still free, which is what keeps a
// browser's signed session cookie valid across restarts.
//
// @param log - operator-facing logger.
// @param requested - the `--port` value (0 = let the OS choose).
// @param previousLocalURL - the local URL recorded by the last run, if any.
// @returns the port to pass to dsh (0 lets the OS choose).
func resolveWebPort(log *logger, requested int, previousLocalURL string) int {
	if requested != 0 {
		return requested
	}
	port := portFromURL(previousLocalURL)
	if port == 0 {
		return 0
	}
	if !portFree(port) {
		log.warn("port %d from the previous run is busy; the OS will pick a new one and existing browser sessions will need the new token URL", port)
		return 0
	}
	log.info("reusing port %d from the previous run so existing browser sessions stay signed in", port)
	return port
}
