#!/usr/bin/env node
/**
 * Auth reverse proxy in front of a loopback-only `dsh web`.
 *
 * Two independent auth layers live here:
 *
 *   1. THIS proxy's own login — a stable token (DSH_AUTH_TOKEN) exchanged once
 *      for a signed, HttpOnly session cookie. `dsh web` has no username/password
 *      of its own, and browsers do not "remember" HTTP Basic the way people
 *      expect (every XHR/WebSocket 401 can re-prompt), so Basic is NOT the
 *      browser path here — the cookie is. Basic stays available for scripts
 *      (DSH_AUTH_MODE=both|basic).
 *   2. dsh's own per-boot session token. `dsh web` prints
 *      `http://127.0.0.1:<port>/?token=<random>` at boot and answers 401 to
 *      everything else. That token rotates on every restart, so the proxy
 *      captures it (DSH_WEB_TOKEN) and, whenever dsh answers 401 to a document
 *      request, transparently bounces the browser through `/?token=<token>`
 *      once; dsh then sets its own session cookie.
 *
 *     client -> auth-proxy (:8080, cookie|bearer|basic)
 *            -> dsh web (127.0.0.1:3080, per-boot token)
 *
 * Modes (DSH_AUTH_MODE):
 *   both  (default) accept session cookie, `Authorization: Bearer <token>`, and
 *                   Basic (for `curl -u`); never sends a Basic challenge, so
 *                   browsers are never prompted
 *   token           cookie + Bearer only — HTTP auth fully off
 *   basic           legacy: challenge every request with Basic (wget -u needs
 *                   a challenge, hence this mode)
 *   off             no auth at all (only if something else fronts it)
 *
 * Endpoints served by the proxy itself:
 *   GET  /__healthz           200, unauthenticated (probes)
 *   GET  /__login             login form (303 to `next` when already signed in)
 *   POST /__login             form {token, next} -> session cookie + 303
 *   GET  /__logout            clears the cookie, 303 to /__login
 *   GET  <any>?dsh_token=...  one-shot login link -> cookie + 303 without the token
 *
 * Zero dependencies on purpose (node:http + node:net + node:crypto only).
 */

import http from 'node:http';
import net from 'node:net';
import { createHash, createHmac, timingSafeEqual } from 'node:crypto';

const BIND = process.env.DSH_PROXY_BIND ?? '0.0.0.0';
const PORT = Number.parseInt(process.env.DSH_PROXY_PORT ?? '8080', 10);
const UPSTREAM_HOST = process.env.DSH_UPSTREAM_HOST ?? '127.0.0.1';
const UPSTREAM_PORT = Number.parseInt(process.env.DSH_PORT ?? '3080', 10);

const MODE = (process.env.DSH_AUTH_MODE ?? 'both').toLowerCase();
const USER = process.env.DSH_AUTH_USER ?? '';
const PASS = process.env.DSH_AUTH_PASS ?? '';
// The stable, human-held login secret. Falls back to the password so an
// existing deployment keeps working without new configuration.
const LOGIN_TOKEN = process.env.DSH_AUTH_TOKEN || PASS;
const SESSION_TTL = Number.parseInt(process.env.DSH_SESSION_TTL ?? '2592000', 10); // 30d
const COOKIE_NAME = process.env.DSH_AUTH_COOKIE ?? 'dsh_auth';
const CHALLENGE = process.env.DSH_AUTH_CHALLENGE === '1' || MODE === 'basic';

const LOGIN_PATH = '/__login';
const LOGOUT_PATH = '/__logout';
const HEALTH_PATH = '/__healthz';
const REALM = process.env.DSH_AUTH_REALM ?? 'DSH';
const WEB_TOKEN = process.env.DSH_WEB_TOKEN ?? '';
// Per-request + WebSocket lifecycle logging: the only way to tell "the browser
// stopped asking" (client/tab side) from "the channel died" (proxy/edge side)
// when somebody reports the UI freezing.
const ACCESS_LOG = process.env.DSH_PROXY_LOG !== '0';
// Keep long-lived WebSockets warm: an empty ping every N ms makes the browser
// answer with a pong, so idle timeouts on the hops in between cannot drop them.
const WS_PING_MS = process.env.DSH_WS_PING === '0'
	? 0
	: Number.parseInt(process.env.DSH_WS_PING_MS ?? '25000', 10);
// If dsh does not answer the boot-token exchange in time, stop waiting: the
// bootstrap promise gates every request for that authority.
const BOOTSTRAP_TIMEOUT_MS = Number.parseInt(process.env.DSH_BOOTSTRAP_TIMEOUT_MS ?? '10000', 10);
const log = (...args) => console.log(...args);

const AUTH_OFF = MODE === 'off' || LOGIN_TOKEN === '';
const BASIC_ENABLED = !AUTH_OFF && (MODE === 'basic' || MODE === 'both');

const sha256 = (value) => createHash('sha256').update(value, 'utf8').digest();
/** Constant-time string compare that leaks neither length nor prefix. */
function safeEqual(a, b) {
	return timingSafeEqual(sha256(String(a)), sha256(String(b)));
}

// --- session cookie: v1.<expEpoch>.<hmac> ---------------------------------
const sign = (exp) => createHmac('sha256', LOGIN_TOKEN).update(`v1.${exp}`).digest('base64url');

/** @returns {{value: string, exp: number}} */
function mintSession(nowMs = Date.now()) {
	const exp = Math.floor(nowMs / 1000) + SESSION_TTL;
	return { value: `v1.${exp}.${sign(exp)}`, exp };
}

/** @returns {number|null} seconds-until-expiry epoch, or null when invalid. */
function verifySession(value) {
	if (typeof value !== 'string') return null;
	const match = /^v1\.(\d{1,12})\.([A-Za-z0-9_-]{43})$/.exec(value);
	if (!match) return null;
	const exp = Number(match[1]);
	if (!safeEqual(match[2], sign(exp))) return null;
	if (exp * 1000 <= Date.now()) return null;
	return exp;
}

/** @returns {Record<string,string>} */
function parseCookies(header) {
	const out = {};
	if (typeof header !== 'string') return out;
	for (const part of header.split(';')) {
		const eq = part.indexOf('=');
		if (eq < 0) continue;
		const name = part.slice(0, eq).trim();
		let value = part.slice(eq + 1).trim();
		try {
			value = decodeURIComponent(value);
		} catch {
			/* keep raw */
		}
		out[name] = value;
	}
	return out;
}

const isSecureRequest = (req) =>
	req.socket.encrypted === true
	|| String(req.headers['x-forwarded-proto'] ?? '').split(',')[0].trim() === 'https';

function setCookie(req, value, maxAge) {
	const bits = [`${COOKIE_NAME}=${encodeURIComponent(value)}`, 'Path=/', 'HttpOnly', 'SameSite=Lax', `Max-Age=${maxAge}`];
	if (isSecureRequest(req)) bits.push('Secure');
	return bits.join('; ');
}

// --- request classification ------------------------------------------------
const pathOf = (url) => (url ?? '/').split('?')[0];

/** Browser navigation (not XHR/fetch/WS): these get an HTML login redirect. */
function isDocumentRequest(req) {
	if (req.method !== 'GET' && req.method !== 'HEAD') return false;
	if (pathOf(req.url) === '/') return true;
	return String(req.headers.accept ?? '').includes('text/html');
}

/** @returns {{ok: boolean, via?: string, renew?: string}} */
function authenticate(req) {
	if (AUTH_OFF) return { ok: true, via: 'off' };
	const bearer = String(req.headers.authorization ?? '');
	if (bearer.startsWith('Bearer ') && safeEqual(bearer.slice(7).trim(), LOGIN_TOKEN)) return { ok: true, via: 'bearer' };
	if (BASIC_ENABLED && bearer.startsWith('Basic ')) {
		let decoded = '';
		try {
			decoded = Buffer.from(bearer.slice(6).trim(), 'base64').toString('utf8');
		} catch {
			decoded = '';
		}
		const colon = decoded.indexOf(':');
		if (colon >= 0 && safeEqual(decoded.slice(0, colon), USER) && safeEqual(decoded.slice(colon + 1), PASS)) {
			return { ok: true, via: 'basic' };
		}
	}
	const cookie = parseCookies(req.headers.cookie)[COOKIE_NAME];
	const exp = verifySession(cookie);
	if (exp === null) return { ok: false };
	// Sliding session: refresh once past half the TTL so active users never
	// have to log in again.
	const remaining = exp * 1000 - Date.now();
	const renew = remaining < (SESSION_TTL * 1000) / 2 ? setCookie(req, mintSession().value, SESSION_TTL) : undefined;
	return { ok: true, via: 'cookie', ...(renew ? { renew } : {}) };
}

function deny(req, res) {
	if (BASIC_ENABLED && CHALLENGE) {
		res.writeHead(401, {
			'www-authenticate': `Basic realm="${REALM}", charset="UTF-8"`,
			'content-type': 'text/plain; charset=utf-8',
			'cache-control': 'no-store',
		});
		res.end('401 Unauthorized\n');
		return;
	}
	if (isDocumentRequest(req)) {
		// Never challenge a browser: send it to the login page instead.
		const next = encodeURIComponent(req.url ?? '/');
		res.writeHead(303, { location: `${LOGIN_PATH}?next=${next}`, 'cache-control': 'no-store' });
		res.end();
		return;
	}
	res.writeHead(401, { 'content-type': 'application/json; charset=utf-8', 'cache-control': 'no-store' });
	res.end('{"error":"unauthorized"}\n');
}

// --- login form ------------------------------------------------------------
function loginPage(next, failed) {
	const escaped = String(next).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]);
	return `<!doctype html>
<html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>DSH 登录</title>
<style>
 body{font:15px/1.6 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,sans-serif;
      display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0;background:#0f1115;color:#e6e6e6}
 form{background:#171a21;padding:28px 30px;border-radius:12px;box-shadow:0 8px 30px rgba(0,0,0,.4);min-width:320px}
 h1{font-size:17px;margin:0 0 4px}p{color:#9aa0aa;font-size:13px;margin:0 0 18px}
 input{width:100%;box-sizing:border-box;padding:10px 12px;border-radius:8px;border:1px solid #2c3140;
       background:#0f1115;color:#e6e6e6;font-size:14px}
 button{margin-top:14px;width:100%;padding:10px;border:0;border-radius:8px;background:#3b82f6;color:#fff;
        font-size:14px;cursor:pointer}
 .err{color:#f87171;font-size:13px;margin:10px 0 0}
</style></head><body>
<form method="post" action="${LOGIN_PATH}">
 <h1>DeepSeek Harness</h1>
 <p>粘贴访问 token（登录一次，${Math.round(SESSION_TTL / 86400)} 天内免登录）</p>
 <input type="password" name="token" autocomplete="current-password" autofocus required placeholder="token">
 <input type="hidden" name="next" value="${escaped}">
 <button type="submit">登录</button>
 ${failed ? '<div class="err">token 不对</div>' : ''}
</form></body></html>
`;
}

function readBody(req, limit = 8192) {
	return new Promise((resolve, reject) => {
		let size = 0;
		const chunks = [];
		req.on('data', (chunk) => {
			size += chunk.length;
			if (size > limit) {
				reject(new Error('body too large'));
				req.destroy();
				return;
			}
			chunks.push(chunk);
		});
		req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
		req.on('error', reject);
	});
}

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

/** Only allow same-origin redirect targets. */
function safeNext(raw) {
	if (typeof raw !== 'string' || raw === '' || !raw.startsWith('/') || raw.startsWith('//')) return '/';
	return raw;
}

function sendLoginForm(res, next, failed) {
	const html = loginPage(next, failed);
	res.writeHead(failed ? 401 : 200, {
		'content-type': 'text/html; charset=utf-8',
		'content-length': Buffer.byteLength(html),
		'cache-control': 'no-store',
	});
	res.end(html);
}

async function handleLogin(req, res, url) {
	const params = url.searchParams;
	if (req.method === 'GET' || req.method === 'HEAD') {
		const next = safeNext(params.get('next'));
		if (authenticate(req).ok) {
			res.writeHead(303, { location: next, 'cache-control': 'no-store' });
			res.end();
			return;
		}
		sendLoginForm(res, next, false);
		return;
	}
	if (req.method !== 'POST') {
		res.writeHead(405, { allow: 'GET, POST' });
		res.end();
		return;
	}
	let fields;
	try {
		fields = new URLSearchParams(await readBody(req));
	} catch {
		res.writeHead(413);
		res.end();
		return;
	}
	const next = safeNext(fields.get('next'));
	const candidate = (fields.get('token') ?? '').trim();
	if (!AUTH_OFF && safeEqual(candidate, LOGIN_TOKEN)) {
		const session = mintSession();
		res.writeHead(303, {
			location: next,
			'set-cookie': setCookie(req, session.value, SESSION_TTL),
			'cache-control': 'no-store',
		});
		res.end();
		return;
	}
	await sleep(600); // slow brute force down a little; no lockout state to abuse
	sendLoginForm(res, next, true);
}

// --- proxying --------------------------------------------------------------
function badGateway(res, error) {
	if (res.headersSent) {
		res.destroy();
		return;
	}
	res.writeHead(502, { 'content-type': 'text/plain; charset=utf-8' });
	res.end(`502 Bad Gateway: ${error instanceof Error ? error.message : String(error)}\n`);
}

function forwardHeaders(req) {
	const headers = { ...req.headers };
	const peer = req.socket.remoteAddress;
	headers['x-forwarded-for'] = req.headers['x-forwarded-for'] ? `${req.headers['x-forwarded-for']}, ${peer}` : peer;
	headers['x-forwarded-proto'] ??= 'http';
	headers['x-forwarded-host'] ??= req.headers.host ?? '';
	return headers;
}

/**
 * dsh's own per-boot session, obtained server-side from `/?token=<DSH_WEB_TOKEN>`
 * and injected into upstream requests. Doing the bootstrap here (instead of
 * bouncing the browser) means the client never sees `?token=` in its URL bar,
 * and Bearer-authenticated scripts work on the first request.
 *
 * dsh signs the session with the *authority* it was redeemed for (its payload
 * literally carries `"authority":"<host>"`), so sessions are cached per Host
 * header — bootstrapping with the wrong Host yields a cookie that 401s.
 * @type {Map<string, string>} authority -> "name=value; other=1"
 */
const dshSessions = new Map();
/** @type {Map<string, Promise<string>>} in-flight bootstraps, per authority */
const dshBootstraps = new Map();

const sessionFor = (authority) => dshSessions.get(authority) ?? '';

/** @returns {Promise<string>} cookie pairs dsh issued for this authority. */
function bootstrapDshSession(authority) {
	if (WEB_TOKEN === '') return Promise.resolve('');
	const cached = dshSessions.get(authority);
	if (cached !== undefined) return Promise.resolve(cached);
	const inflight = dshBootstraps.get(authority);
	if (inflight !== undefined) return inflight;
	const promise = new Promise((resolve) => {
		let settled = false;
		const done = (cookies) => {
			if (settled) return;
			settled = true;
			clearTimeout(watchdog);
			resolve(cookies);
		};
		// Never let a silent dsh hang every request behind this promise.
		const watchdog = setTimeout(() => {
			if (ACCESS_LOG) log(`[dsh] bootstrap for host=${authority} timed out after ${BOOTSTRAP_TIMEOUT_MS}ms — falling back to the ?token= redirect`);
			done('');
		}, BOOTSTRAP_TIMEOUT_MS);
		const request = http.request(
			{
				host: UPSTREAM_HOST,
				port: UPSTREAM_PORT,
				method: 'GET',
				path: `/?token=${encodeURIComponent(WEB_TOKEN)}`,
				headers: { host: authority },
			},
			(upstreamRes) => {
				const raw = upstreamRes.headers['set-cookie'];
				upstreamRes.resume();
				const list = Array.isArray(raw) ? raw : raw ? [raw] : [];
				const cookies = list.map((c) => c.split(';')[0]).join('; ');
				if (cookies !== '') dshSessions.set(authority, cookies);
				if (ACCESS_LOG) {
					const name = cookies.split('=')[0] || 'none';
					log(`[dsh] session for host=${authority}: ${cookies === '' ? 'NO COOKIE (dsh rejected the boot token?)' : `cookie ${name}`}`);
				}
				done(cookies);
			},
		);
		request.on('error', () => done(''));
		request.end();
	}).finally(() => {
		dshBootstraps.delete(authority);
	});
	dshBootstraps.set(authority, promise);
	return promise;
}

/** Join two `Cookie:` header values, de-duplicating by cookie name. */
function mergeCookies(existing, extra) {
	if (extra === '') return existing;
	const present = new Set(existing.split(';').map((p) => p.split('=')[0].trim()).filter(Boolean));
	const missing = extra.split('; ').filter((p) => !present.has(p.split('=')[0].trim()));
	if (missing.length === 0) return existing;
	return existing === '' ? missing.join('; ') : `${existing}; ${missing.join('; ')}`;
}

/** Merge dsh's session cookies into the outgoing headers (client cookies win). */
function withDshCookies(headers) {
	const cookies = sessionFor(headers.host ?? '');
	if (cookies === '') return headers;
	const merged = mergeCookies(headers.cookie ?? '', cookies);
	return merged === (headers.cookie ?? '') ? headers : { ...headers, cookie: merged };
}

/**
 * Fallback only: if we could not obtain dsh's session, bounce the browser
 * through the token URL the old way (never for XHR — that would break the SPA).
 * @returns {boolean}
 */
function needsDshToken(req, statusCode) {
	if (statusCode !== 401 || WEB_TOKEN === '') return false;
	if (sessionFor(req.headers.host ?? '') !== '') return false;
	if (req.method !== 'GET' && req.method !== 'HEAD') return false;
	if (pathOf(req.url).startsWith('/api/')) return false;
	return isDocumentRequest(req);
}

/** Proxy one attempt; retries once with a fresh dsh session on a stale 401. */
function proxyOnce(req, res, session, retried) {
	const bodyless = req.method === 'GET' || req.method === 'HEAD';
	const authority = req.headers.host ?? '';
	const upstream = http.request(
		{
			host: UPSTREAM_HOST,
			port: UPSTREAM_PORT,
			method: req.method,
			path: req.url,
			headers: withDshCookies(forwardHeaders(req)),
		},
		(upstreamRes) => {
			const status = upstreamRes.statusCode ?? 0;
			if (status === 401 && bodyless && !retried && sessionFor(authority) !== '') {
				// dsh restarted (or dropped the session): re-bootstrap and retry once.
				upstreamRes.resume();
				dshSessions.delete(authority);
				if (ACCESS_LOG) log(`[dsh] session for host=${authority} went stale (401) — re-bootstrapping and retrying once`);
				bootstrapDshSession(authority).then(() => proxyOnce(req, res, session, true));
				return;
			}
			if (needsDshToken(req, status)) {
				upstreamRes.resume(); // drain dsh's 401 body
				res.writeHead(303, {
					location: `/?token=${encodeURIComponent(WEB_TOKEN)}`,
					'cache-control': 'no-store',
				});
				res.end();
				return;
			}
			const headers = { ...upstreamRes.headers };
			if (session.renew) {
				// Keep dsh's own Set-Cookie(s) and append our renewal.
				const existing = headers['set-cookie'];
				headers['set-cookie'] = Array.isArray(existing)
					? [...existing, session.renew]
					: existing ? [existing, session.renew] : [session.renew];
			}
			res.writeHead(status || 502, headers);
			upstreamRes.pipe(res);
		},
	);
	upstream.on('error', (error) => badGateway(res, error));
	req.on('error', () => upstream.destroy());
	// Bodyless requests must not be piped twice (the retry reuses this stream).
	if (bodyless) upstream.end();
	else req.pipe(upstream);
}

const server = http.createServer(async (req, res) => {
	const url = new URL(req.url ?? '/', 'http://placeholder');
	const path = pathOf(req.url);

	// One line per request: with these, a frozen UI is diagnosable from the pod
	// log alone (did the browser stop asking, or did something answer slowly?).
	const startedAt = Date.now();
	let via = 'none';
	if (ACCESS_LOG && path !== HEALTH_PATH) {
		res.on('finish', () => {
			log(`[access] ${req.method} ${path} -> ${res.statusCode} ${Date.now() - startedAt}ms host=${req.headers.host ?? '-'} via=${via}`);
		});
	}

	if (path === HEALTH_PATH) {
		res.writeHead(200, { 'content-type': 'text/plain; charset=utf-8' });
		res.end('ok\n');
		return;
	}

	// One-shot login link: /any/path?dsh_token=<token>
	const linkToken = url.searchParams.get('dsh_token');
	if (linkToken !== null && !AUTH_OFF) {
		if (safeEqual(linkToken, LOGIN_TOKEN)) {
			url.searchParams.delete('dsh_token');
			const rest = url.searchParams.toString();
			res.writeHead(303, {
				location: `${url.pathname}${rest ? `?${rest}` : ''}`,
				'set-cookie': setCookie(req, mintSession().value, SESSION_TTL),
				'cache-control': 'no-store',
			});
			res.end();
			return;
		}
		await sleep(600);
	}

	if (path === LOGIN_PATH) {
		await handleLogin(req, res, url);
		return;
	}
	if (path === LOGOUT_PATH) {
		res.writeHead(303, {
			location: LOGIN_PATH,
			'set-cookie': setCookie(req, '', 0),
			'cache-control': 'no-store',
		});
		res.end();
		return;
	}

	const session = authenticate(req);
	via = session.via ?? 'none';
	if (!session.ok) {
		deny(req, res);
		return;
	}

	// Make sure we hold dsh's session for THIS authority before proxying (no
	// redirect for the client; see bootstrapDshSession).
	if (WEB_TOKEN !== '') await bootstrapDshSession(req.headers.host ?? '');
	proxyOnce(req, res, session, false);
});

// WebSocket (and any other Upgrade). Browsers send our session cookie on the
// handshake; dsh's own session is injected by the proxy.
server.on('upgrade', async (req, socket) => {
	const path = pathOf(req.url);
	const session = authenticate(req);
	if (!session.ok) {
		const challenge = BASIC_ENABLED && CHALLENGE
			? `WWW-Authenticate: Basic realm="${REALM}", charset="UTF-8"\r\n`
			: '';
		if (ACCESS_LOG) log(`[ws] rejected ${path} host=${req.headers.host ?? '-'} (no session)`);
		socket.write(`HTTP/1.1 401 Unauthorized\r\n${challenge}Content-Length: 0\r\nConnection: close\r\n\r\n`);
		socket.destroy();
		return;
	}
	const authority = req.headers.host ?? '';
	if (WEB_TOKEN !== '') await bootstrapDshSession(authority);

	const pairs = [];
	for (let i = 0; i < req.rawHeaders.length; i += 2) pairs.push([req.rawHeaders[i], req.rawHeaders[i + 1]]);
	const clientCookie = pairs.filter(([name]) => name.toLowerCase() === 'cookie').map(([, value]) => value).join('; ');

	const wsStarted = Date.now();
	let toUpstream = 0;
	let toClient = 0;
	let pings = 0;
	let pingTimer = null;
	let handshake = 'pending';

	const finish = (why) => {
		if (pingTimer !== null) clearInterval(pingTimer);
		pingTimer = null;
		if (ACCESS_LOG) {
			log(`[ws] closed ${path} after ${Date.now() - wsStarted}ms (${why}) client->dsh=${toUpstream}B dsh->client=${toClient}B pings=${pings} handshake="${handshake}" via=${session.via}`);
		}
	};

	socket.on('data', (chunk) => { toUpstream += chunk.length; });
	socket.on('close', () => finish('client close'));
	socket.on('error', (error) => finish(`client error: ${error.message}`));

	const upstream = net.connect(UPSTREAM_PORT, UPSTREAM_HOST, () => {
		const lines = [`${req.method} ${req.url} HTTP/1.1`];
		for (const [name, value] of pairs) {
			if (name.toLowerCase() !== 'cookie') lines.push(`${name}: ${value}`);
		}
		const cookie = mergeCookies(clientCookie, sessionFor(authority));
		if (cookie !== '') lines.push(`Cookie: ${cookie}`);
		upstream.write(`${lines.join('\r\n')}\r\n\r\n`);
	});

	// Watch dsh's handshake reply so we only keepalive a real 101. An empty WS
	// ping (0x89 0x00) makes the browser answer with a pong, which keeps bytes
	// flowing on every hop and defeats idle timeouts there.
	let headBuf = Buffer.alloc(0);
	upstream.on('data', (chunk) => {
		toClient += chunk.length;
		if (handshake !== 'pending') return;
		headBuf = Buffer.concat([headBuf, chunk]);
		const end = headBuf.indexOf('\r\n\r\n');
		if (end < 0) return;
		const status = headBuf.subarray(0, end).toString('latin1').split('\r\n')[0] ?? '';
		handshake = status;
		headBuf = Buffer.alloc(0);
		if (ACCESS_LOG) log(`[ws] open ${path} host=${authority} via=${session.via} "${status}"`);
		if (WS_PING_MS > 0 && status.startsWith('HTTP/1.1 101')) {
			pingTimer = setInterval(() => {
				try {
					socket.write(Buffer.from([0x89, 0x00]));
					pings++;
				} catch { /* socket going away */ }
			}, WS_PING_MS);
		}
	});
	upstream.on('close', () => finish('dsh close'));
	upstream.on('error', (error) => {
		finish(`dsh error: ${error.message}`);
		socket.destroy();
	});
	socket.setNoDelay?.(true);
	upstream.pipe(socket);
	socket.pipe(upstream);
});

server.on('clientError', (_error, socket) => socket.destroy());

// Long-lived streams (SSE, WebSockets) must not be cut by Node's own timeouts.
server.requestTimeout = 0;
server.headersTimeout = 60_000;
server.keepAliveTimeout = 65_000;

server.listen(PORT, BIND, () => {
	const auth = AUTH_OFF
		? 'NO AUTH'
		: `mode=${MODE}${BASIC_ENABLED ? ' (basic accepted)' : ''}${CHALLENGE ? ' challenge=on' : ' challenge=off'}, ttl=${SESSION_TTL}s`;
	const token = WEB_TOKEN === '' ? 'no dsh token' : 'dsh token bootstrap on';
	console.log(`[auth-proxy] listening on ${BIND}:${PORT} -> ${UPSTREAM_HOST}:${UPSTREAM_PORT} [${auth}, ${token}]`);
});
