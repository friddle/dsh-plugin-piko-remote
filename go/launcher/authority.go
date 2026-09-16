package main

// Which authority DSH issues its browser cookie for decides whether a session
// survives a restart.
//
// With host rewriting (`preserveHost: false`) the upstream sees `127.0.0.1:<port>`,
// so that is the authority: the cookie *name* is a hash of it and the payload
// binds it. A new port — or any restart that picks one — invalidates every
// existing cookie and the browser answers a bare `401 dsh web authentication
// required` even though the tunnel and DSH are both healthy.
//
// Keeping the browser's Host instead (`preserveHost: true`) makes the authority
// the pinned public name, which only changes if the endpoint does. DSH then needs
// that authority in its browser-trust fence, which is what `--trusted-host` adds.

import "net/url"

// publicAuthority is the authority a browser uses for a pinned endpoint:
// `<endpoint>.<host>` with the remote's port when it has one.
//
// @param endpoint - the pinned endpoint name, or "" for a random one.
// @param remote - the piko server URL.
// @returns the authority, or "" when it cannot be derived (random endpoint or
//
//	unparseable remote), which is the signal to keep rewriting the Host.
func publicAuthority(endpoint, remote string) string {
	if endpoint == "" {
		return ""
	}
	parsed, err := url.Parse(remote)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return endpoint + "." + parsed.Host
}
