#!/usr/bin/env node
/**
 * Basic Auth reverse proxy in front of a loopback-only `dsh web`.
 *
 * Two auth layers, deliberately:
 *
 *   1. HTTP Basic Auth (this file) — the stable, human-managed account.
 *      `dsh web` has no username/password of its own.
 *   2. dsh's own session token — `dsh web` prints
 *      `http://127.0.0.1:<port>/?token=<random>` at boot and answers **401** to
 *      everything else, including `/`. The token is per-boot and not something a
 *      human can hold, so this proxy captures it (DSH_WEB_TOKEN) and, whenever
 *      dsh answers 401 to a document request, transparently bounces the browser
 *      through `/?token=<token>` once. dsh then sets its session cookie and the
 *      UI loads normally.
 *
 *     ingress -> auth-proxy (:8080, Basic Auth + token bootstrap)
 *             -> dsh web (127.0.0.1:3080, token-gated)
 *
 * Zero dependencies on purpose (node:http + node:net only): HTTP requests and
 * WebSocket upgrades are both proxied, the original Host header is preserved
 * (so dsh's browser-trust fence sees the public authority, which must also be
 * listed in DSH_TRUSTED_HOST), and credentials are compared in constant time.
 *
 * Env:
 *   DSH_PROXY_BIND    bind address            (default 0.0.0.0)
 *   DSH_PROXY_PORT    listen port             (default 8080)
 *   DSH_UPSTREAM_HOST dsh web host            (default 127.0.0.1)
 *   DSH_PORT          dsh web port            (default 3080)
 *   DSH_AUTH_USER     Basic Auth user         (empty + empty = auth disabled)
 *   DSH_AUTH_PASS     Basic Auth password
 *   DSH_AUTH          set to "false" to disable Basic Auth entirely
 *   DSH_AUTH_REALM    WWW-Authenticate realm  (default DSH)
 *   DSH_WEB_TOKEN     dsh's boot token; empty disables the 401 bootstrap
 */

import http from 'node:http';
import net from 'node:net';
import { createHash, timingSafeEqual } from 'node:crypto';

const BIND = process.env.DSH_PROXY_BIND ?? '0.0.0.0';
const PORT = Number.parseInt(process.env.DSH_PROXY_PORT ?? '8080', 10);
const UPSTREAM_HOST = process.env.DSH_UPSTREAM_HOST ?? '127.0.0.1';
const UPSTREAM_PORT = Number.parseInt(process.env.DSH_PORT ?? '3080', 10);
const USER = process.env.DSH_AUTH_USER ?? '';
const PASS = process.env.DSH_AUTH_PASS ?? '';
const REALM = process.env.DSH_AUTH_REALM ?? 'DSH';
const WEB_TOKEN = process.env.DSH_WEB_TOKEN ?? '';
const HEALTH_PATH = '/__healthz';
const AUTH_ENABLED = process.env.DSH_AUTH !== 'false' && (USER !== '' || PASS !== '');

const sha256 = (value) => createHash('sha256').update(value, 'utf8').digest();
const USER_HASH = sha256(USER);
const PASS_HASH = sha256(PASS);

/** Constant-time Basic Auth check. @returns {boolean} */
function authorized(header) {
	if (!AUTH_ENABLED) return true;
	if (typeof header !== 'string' || !header.startsWith('Basic ')) return false;
	let decoded;
	try {
		decoded = Buffer.from(header.slice('Basic '.length).trim(), 'base64').toString('utf8');
	} catch {
		return false;
	}
	const separator = decoded.indexOf(':');
	if (separator < 0) return false;
	// Hash first: timingSafeEqual requires equal lengths, and this also keeps the
	// comparison constant-time for variable-length credentials.
	const userOk = timingSafeEqual(sha256(decoded.slice(0, separator)), USER_HASH);
	const passOk = timingSafeEqual(sha256(decoded.slice(separator + 1)), PASS_HASH);
	return userOk && passOk;
}

/**
 * Should a 401 from dsh be turned into a trip through `/?token=`?
 * Only for document navigations: never for XHR/WS under /api, which must keep
 * their real status codes for the SPA to react to.
 * @returns {boolean}
 */
function shouldBootstrap(req, statusCode) {
	if (statusCode !== 401 || WEB_TOKEN === '') return false;
	if (req.method !== 'GET' && req.method !== 'HEAD') return false;
	const path = (req.url ?? '/').split('?')[0];
	if (path.startsWith('/api/')) return false;
	if (path === '/') return true;
	return (req.headers.accept ?? '').includes('text/html');
}

/** @param {http.ServerResponse} res */
function deny(res) {
	res.writeHead(401, {
		'www-authenticate': `Basic realm="${REALM}", charset="UTF-8"`,
		'content-type': 'text/plain; charset=utf-8',
		'cache-control': 'no-store',
	});
	res.end('401 Unauthorized\n');
}

/** @param {http.ServerResponse} res */
function badGateway(res, error) {
	if (res.headersSent) {
		res.destroy();
		return;
	}
	res.writeHead(502, { 'content-type': 'text/plain; charset=utf-8' });
	res.end(`502 Bad Gateway: ${error instanceof Error ? error.message : String(error)}\n`);
}

/** Headers to hand upstream: keep the original Host, add forwarding metadata. */
function forwardHeaders(req) {
	const headers = { ...req.headers };
	const peer = req.socket.remoteAddress;
	headers['x-forwarded-for'] = req.headers['x-forwarded-for']
		? `${req.headers['x-forwarded-for']}, ${peer}`
		: peer;
	headers['x-forwarded-proto'] ??= 'http';
	headers['x-forwarded-host'] ??= req.headers.host ?? '';
	return headers;
}

const server = http.createServer((req, res) => {
	if (req.url?.split('?')[0] === HEALTH_PATH) {
		res.writeHead(200, { 'content-type': 'text/plain; charset=utf-8' });
		res.end('ok\n');
		return;
	}
	if (!authorized(req.headers.authorization)) {
		deny(res);
		return;
	}
	const upstream = http.request(
		{
			host: UPSTREAM_HOST,
			port: UPSTREAM_PORT,
			method: req.method,
			path: req.url,
			headers: forwardHeaders(req),
		},
		(upstreamRes) => {
			if (shouldBootstrap(req, upstreamRes.statusCode ?? 0)) {
				upstreamRes.resume(); // drain dsh's 401 body
				res.writeHead(303, {
					location: `/?token=${encodeURIComponent(WEB_TOKEN)}`,
					'cache-control': 'no-store',
				});
				res.end();
				return;
			}
			res.writeHead(upstreamRes.statusCode ?? 502, upstreamRes.headers);
			upstreamRes.pipe(res);
		},
	);
	upstream.on('error', (error) => badGateway(res, error));
	req.on('error', () => upstream.destroy());
	req.pipe(upstream);
});

// WebSocket (and any other Upgrade) — pipe raw sockets once authenticated.
// dsh's browser UI keeps a long-lived /api/remote.mux WebSocket open; by then
// the browser already holds dsh's session cookie from the token bootstrap.
server.on('upgrade', (req, socket, head) => {
	if (!authorized(req.headers.authorization)) {
		socket.write(
			'HTTP/1.1 401 Unauthorized\r\n'
			+ `WWW-Authenticate: Basic realm="${REALM}", charset="UTF-8"\r\n`
			+ 'Content-Length: 0\r\n'
			+ 'Connection: close\r\n\r\n',
		);
		socket.destroy();
		return;
	}
	const upstream = net.connect(UPSTREAM_PORT, UPSTREAM_HOST, () => {
		// Relay the handshake verbatim (rawHeaders keeps the original Host,
		// Cookie, Upgrade and Sec-WebSocket-* headers the client sent).
		const lines = [`${req.method} ${req.url} HTTP/1.1`];
		for (let i = 0; i < req.rawHeaders.length; i += 2) {
			lines.push(`${req.rawHeaders[i]}: ${req.rawHeaders[i + 1]}`);
		}
		upstream.write(`${lines.join('\r\n')}\r\n\r\n`);
		if (head?.length) upstream.write(head);
		upstream.pipe(socket);
		socket.pipe(upstream);
	});
	upstream.on('error', () => socket.destroy());
	socket.on('error', () => upstream.destroy());
	socket.setNoDelay?.(true);
});

server.on('clientError', (_error, socket) => {
	socket.destroy();
});

// Long-lived streams (SSE, WebSockets) must not be cut by Node's own timeouts.
server.requestTimeout = 0;
server.headersTimeout = 60_000;
server.keepAliveTimeout = 65_000;

server.listen(PORT, BIND, () => {
	const basic = AUTH_ENABLED ? `Basic Auth (user "${USER}")` : 'NO Basic Auth';
	const token = WEB_TOKEN === '' ? 'no dsh token' : 'dsh token bootstrap on';
	console.log(`[auth-proxy] listening on ${BIND}:${PORT} -> ${UPSTREAM_HOST}:${UPSTREAM_PORT} [${basic}, ${token}]`);
});
