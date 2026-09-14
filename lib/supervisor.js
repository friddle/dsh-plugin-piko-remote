/**
 * Tunnel lifecycle: one supervised `piko-expose` child per endpoint.
 *
 * The helper speaks a line-delimited JSON protocol on stdout (see go/main.go).
 * This module turns that stream into a small state machine plus a promise that
 * settles when the tunnel is actually reachable — "spawned" is not "ready", and
 * handing the model a URL before the upstream has connected would produce a
 * link that 404s for the first few hundred milliseconds.
 *
 * Everything external is injected (`spawn`, `now`, `logger`), so the whole
 * state machine is testable without a piko server or a real process.
 *
 * @module piko-remote/supervisor
 */

import { randomEndpoint, remoteUrlFor, sanitizeEndpoint } from './endpoint.js'

/** Default bound on "spawned but never reported ready". */
export const DEFAULT_READY_TIMEOUT_MS = 20000

/** Grace given to a terminating helper before we stop waiting for it. */
export const TERMINATION_GRACE_MS = 5000

/**
 * Build the helper's argv.
 *
 * Kept separate from spawning so the flag mapping (which is the part that can
 * silently be wrong) is unit-tested directly.
 *
 * @param {object} options - tunnel parameters.
 * @param {string} options.helperPath - path to the helper binary.
 * @param {string} options.remote - piko server URL.
 * @param {string} options.endpoint - endpoint ID.
 * @param {number} options.port - local port to expose.
 * @param {'subdomain'|'path'} options.urlMode - routing mode.
 * @param {boolean} options.basicAuth - whether to enable Basic Auth.
 * @param {string} [options.authUser] - fixed Basic Auth user; '' means random.
 * @param {string} [options.authPass] - fixed Basic Auth password; '' means random.
 * @param {boolean} options.preserveHost - whether to forward the browser Host.
 * @param {string} options.upstreamKey - piko upstream key, '' for none.
 * @param {number} options.ttlMinutes - auto-exit after this many minutes, 0 never.
 * @returns {string[]} argv, with the binary first.
 */
export function buildHelperArgv({
  helperPath,
  remote,
  endpoint,
  port,
  urlMode,
  basicAuth,
  authUser,
  authPass,
  preserveHost,
  upstreamKey,
  ttlMinutes,
}) {
  const argv = [
    helperPath,
    '--remote',
    remote,
    '--endpoint',
    endpoint,
    '--target',
    `127.0.0.1:${port}`,
    '--url-mode',
    urlMode,
    '--json',
  ]

  if (basicAuth === false) argv.push('--auth=false')
  // Fixed credentials are passed only when configured: the helper's own random
  // generation is the default, so an empty value must not become an empty flag.
  if (typeof authUser === 'string' && authUser !== '' && basicAuth !== false) argv.push('--auth-user', authUser)
  if (typeof authPass === 'string' && authPass !== '' && basicAuth !== false) argv.push('--auth-pass', authPass)
  if (preserveHost === false) argv.push('--preserve-host=false')
  if (typeof upstreamKey === 'string' && upstreamKey !== '') argv.push('--upstream-key', upstreamKey)
  if (Number.isFinite(ttlMinutes) && ttlMinutes > 0) argv.push('--auto-exit', String(ttlMinutes))

  return argv
}

/**
 * Incremental newline splitter.
 *
 * stdout arrives in arbitrary chunks, so a JSON event is routinely cut in half.
 * Buffering until the newline is not optional: parsing each chunk separately
 * loses exactly the events that matter most — the ones emitted while the
 * process is still starting up.
 *
 * @param {(line: string) => void} onLine - called once per complete non-empty line.
 * @returns {(chunk: string) => void} the chunk feeder.
 */
export function createLineSplitter(onLine) {
  let buffer = ''
  return (chunk) => {
    buffer += chunk
    let index = buffer.indexOf('\n')
    while (index >= 0) {
      const line = buffer.slice(0, index).trim()
      buffer = buffer.slice(index + 1)
      if (line !== '') onLine(line)
      index = buffer.indexOf('\n')
    }
  }
}

/**
 * One supervised tunnel plus its child process.
 *
 * @param {object} options - supervisor wiring.
 * @param {(spec: object) => object} options.spawn - subprocess spawner.
 * @param {object} options.config - resolved plugin config.
 * @param {number|(() => number|undefined)} [options.defaultPort] - port used when a call omits
 *   one; a function is accepted because the DSH web port becomes known only once the web server
 *   service is up.
 * @param {string} [options.helperPath] - helper binary path.
 * @param {string} [options.cwd] - child working directory.
 * @param {(message: string) => void} [options.logger] - diagnostic sink.
 * @param {number} [options.readyTimeoutMs] - ready bound, defaults to config.
 */
export class TunnelSupervisor {
  #spawn
  #config
  #defaultPort
  #cwd
  #log
  #tunnels = new Map()
  #shuttingDown = false

  constructor({ spawn, config, defaultPort, helperPath = '', cwd = process.cwd(), logger, readyTimeoutMs }) {
    if (typeof spawn !== 'function') {
      throw new TypeError('TunnelSupervisor requires a spawn function')
    }
    this.#spawn = spawn
    this.#config = config
    this.#defaultPort = typeof defaultPort === 'function' ? defaultPort : () => defaultPort
    this.helperPath = helperPath
    this.#cwd = cwd
    this.#log = typeof logger === 'function' ? logger : () => {}
    this.readyTimeoutMs = readyTimeoutMs ?? config.connectTimeoutMs ?? DEFAULT_READY_TIMEOUT_MS
  }

  /** Number of tunnels currently tracked. */
  get size() {
    return this.#tunnels.size
  }

  /**
   * Snapshots of every tracked tunnel, newest state included.
   *
   * @returns {object[]} JSON-safe tunnel records.
   */
  list() {
    return [...this.#tunnels.values()].map(snapshot)
  }

  /**
   * Expose a local port, resolving once the tunnel reports ready.
   *
   * @param {object} [request] - expose request.
   * @param {number} [request.port] - local port; defaults to the DSH web port.
   * @param {string} [request.name] - requested endpoint. Falls back to the
   *   configured `endpoint` (a launcher-pinned name), then to a random one.
   * @param {number} [request.ttlMinutes] - lifetime in minutes.
   * @param {boolean} [request.auth] - enable Basic Auth for this tunnel.
   * @returns {Promise<object>} the running tunnel snapshot.
   */
  async expose({ port, name, ttlMinutes, auth } = {}) {
    if (this.#shuttingDown) {
      throw new Error('piko-remote: shutting down, not starting new tunnels')
    }

    const localPort = normalisePort(port === undefined ? this.#defaultPort() : port)
    // A configured endpoint is the operator's pinned name: auto-expose never
    // passes one, so without this fallback `--endpoint` would only ever apply to
    // an explicit remote_expose call and every boot would publish a new URL.
    const requested = name === undefined || name === '' ? this.#config.endpoint : name
    const endpoint =
      requested === undefined || requested === ''
        ? randomEndpoint(this.#config.endpointPrefix)
        : sanitizeEndpoint(requested)

    const existing = this.#tunnels.get(endpoint)
    if (existing !== undefined && (existing.state === 'running' || existing.state === 'starting')) {
      return snapshot(existing)
    }
    if (existing !== undefined) this.#tunnels.delete(endpoint)

    const ttl = ttlMinutes === undefined ? this.#config.defaultTtlMinutes : toTtl(ttlMinutes)
    const useAuth = auth === undefined ? this.#config.basicAuth : auth !== false

    const record = {
      endpoint,
      localPort,
      state: 'starting',
      startedAt: new Date().toISOString(),
      remoteUrl: remoteUrlFor({ endpoint, remote: this.#config.remote, mode: this.#config.urlMode }),
      authUser: undefined,
      authPass: undefined,
      expiresAt: ttl > 0 ? new Date(Date.now() + ttl * 60_000).toISOString() : undefined,
      error: undefined,
      handle: undefined,
    }
    this.#tunnels.set(endpoint, record)

    const argv = buildHelperArgv({
      helperPath: this.helperPath,
      remote: this.#config.remote,
      endpoint,
      port: localPort,
      urlMode: this.#config.urlMode,
      basicAuth: useAuth,
      authUser: this.#config.basicAuthUser,
      authPass: this.#config.basicAuthPass,
      preserveHost: this.#config.preserveHost,
      upstreamKey: this.#config.upstreamKey,
      ttlMinutes: ttl,
    })

    const ready = deferred()
    try {
      record.handle = this.#spawn({
        argv,
        cwd: this.#cwd,
        stdio: { stdin: 'ignore', stdout: 'pipe', stderr: { maxBytes: 64 * 1024 } },
        graceMs: TERMINATION_GRACE_MS,
      })
    } catch (error) {
      this.#tunnels.delete(endpoint)
      throw new Error(`piko-remote: could not start piko-expose: ${error.message}`, { cause: error })
    }

    this.#wire(record, ready)

    try {
      await withTimeout(
        ready.promise,
        this.readyTimeoutMs,
        `piko-remote: piko-expose did not report ready within ${this.readyTimeoutMs}ms`,
      )
    } catch (error) {
      const detail = this.#stderrTail(record)
      record.state = 'failed'
      record.error = error.message
      await this.#stop(record)
      throw new Error(detail === '' ? error.message : `${error.message} (stderr: ${detail})`, { cause: error })
    }

    record.state = 'running'
    return snapshot(record)
  }

  /**
   * Close one tunnel, or all of them.
   *
   * @param {string} [endpoint] - endpoint to close; omitted closes everything.
   * @returns {Promise<string[]>} the endpoints that were closed.
   */
  async close(endpoint) {
    const targets =
      endpoint === undefined || endpoint === ''
        ? [...this.#tunnels.values()]
        : [this.#tunnels.get(sanitizeEndpoint(endpoint))].filter((record) => record !== undefined)

    const closed = []
    for (const record of targets) {
      await this.#stop(record)
      closed.push(record.endpoint)
    }
    return closed
  }

  /**
   * Terminate everything. Called from the plugin's dispose hook, so a reload or
   * shutdown never leaves an orphaned process holding a public endpoint.
   *
   * @returns {Promise<void>} resolves once all children are terminated.
   */
  async dispose() {
    this.#shuttingDown = true
    await this.close()
  }

  /**
   * Attach the helper's stdout/stderr and the child's outcome to a record.
   *
   * @param {object} record - the tunnel record being started.
   * @param {{promise: Promise<void>, resolve: Function, reject: Function}} ready - readiness latch.
   */
  #wire(record, ready) {
    const feed = createLineSplitter((line) => {
      let event
      try {
        event = JSON.parse(line)
      } catch {
        // The helper emits human text only in non-JSON mode; anything
        // unparseable here is diagnostic, not protocol.
        this.#log(`[piko-remote] helper: ${line}`)
        return
      }

      switch (event.event) {
        case 'ready':
          if (typeof event.remoteUrl === 'string' && event.remoteUrl !== '') record.remoteUrl = event.remoteUrl
          ready.resolve()
          break
        case 'auth':
          record.authUser = typeof event.user === 'string' ? event.user : undefined
          record.authPass = typeof event.pass === 'string' ? event.pass : undefined
          break
        case 'error':
          record.error = typeof event.message === 'string' ? event.message : 'unknown helper error'
          ready.reject(new Error(`piko-expose: ${record.error}`))
          break
        case 'closed':
          record.state = 'stopped'
          break
        default:
          this.#log(`[piko-remote] helper event: ${line}`)
      }
    })

    record.handle.stdout?.on('data', (chunk) => feed(String(chunk)))
    record.handle.stdout?.on('end', () => feed('\n'))

    const settled = record.handle.done
    if (settled !== undefined && typeof settled.then === 'function') {
      settled
        .then((outcome) => {
          if (record.state !== 'failed') record.state = 'stopped'
          ready.reject(
            new Error(
              `piko-remote: piko-expose exited (code ${outcome?.exitCode ?? 'null'}` +
                `${outcome?.signal ? `, signal ${outcome.signal}` : ''})`,
            ),
          )
        })
        .catch((error) => {
          record.state = 'failed'
          record.error = error?.message ?? String(error)
          ready.reject(error instanceof Error ? error : new Error(String(error)))
        })
    }
  }

  /**
   * Terminate a record's child and forget the tunnel.
   *
   * @param {object} record - tunnel to stop.
   * @returns {Promise<void>} resolves after the child's range is gone.
   */
  async #stop(record) {
    const handle = record.handle
    record.handle = undefined
    this.#tunnels.delete(record.endpoint)
    if (handle === undefined) return

    try {
      handle.terminate()
    } catch (error) {
      this.#log(`[piko-remote] terminate failed for ${record.endpoint}: ${error.message}`)
    }
    try {
      await handle.waitForExit?.(AbortSignal.timeout(TERMINATION_GRACE_MS + 2000))
    } catch (error) {
      this.#log(`[piko-remote] waitForExit failed for ${record.endpoint}: ${error.message}`)
    }
  }

  /**
   * Last collected stderr, used to explain a helper that died during startup.
   *
   * @param {object} record - the tunnel being started.
   * @returns {string} trimmed stderr tail, or '' when nothing was collected.
   */
  #stderrTail(record) {
    try {
      const text = record.handle?.collected?.stderr?.readFrom(0)?.text
      return typeof text === 'string' ? text.trim().split('\n').slice(-3).join(' ') : ''
    } catch {
      return ''
    }
  }
}

/**
 * Public projection of a tunnel record: no process handles, no live streams.
 *
 * @param {object} record - internal record.
 * @returns {object} JSON-safe snapshot.
 */
function snapshot(record) {
  return {
    endpoint: record.endpoint,
    remoteUrl: record.remoteUrl,
    localPort: record.localPort,
    state: record.state,
    startedAt: record.startedAt,
    ...(record.expiresAt === undefined ? {} : { expiresAt: record.expiresAt }),
    ...(record.authUser === undefined ? {} : { authUser: record.authUser }),
    ...(record.authPass === undefined ? {} : { authPass: record.authPass }),
    ...(record.error === undefined ? {} : { error: record.error }),
  }
}

/**
 * Validate a port, defaulting to the DSH web port when the caller gave none.
 *
 * @param {unknown} value - requested port.
 * @returns {number} a usable TCP port.
 * @throws {Error} when there is no port to use, or it is out of range.
 */
function normalisePort(value) {
  if (value === undefined || value === null) {
    throw new Error(
      'piko-remote: no port to expose — pass one, or run this inside a DSH web profile so the web port is known',
    )
  }
  const port = Number(value)
  if (!Number.isInteger(port) || port < 1 || port > 65535) {
    throw new Error(`piko-remote: invalid port ${String(value)}`)
  }
  return port
}

/**
 * Coerce a TTL to whole non-negative minutes.
 *
 * @param {unknown} value - requested TTL.
 * @returns {number} minutes, 0 meaning no expiry.
 */
function toTtl(value) {
  const ttl = Number(value)
  if (!Number.isFinite(ttl) || ttl < 0) return 0
  return Math.floor(ttl)
}

/**
 * A promise with its settle functions exposed, for event-driven handshakes.
 *
 * @returns {{promise: Promise<void>, resolve: () => void, reject: (error: Error) => void}} the latch.
 */
function deferred() {
  let resolve = () => {}
  let reject = () => {}
  const promise = new Promise((res, rej) => {
    resolve = res
    reject = rej
  })
  // A rejected latch is always awaited by expose(), but Node still reports an
  // unhandled rejection if the caller attaches a handler a tick later.
  promise.catch(() => {})
  return { promise, resolve, reject }
}

/**
 * Bound a promise with a timeout.
 *
 * @param {Promise<unknown>} promise - the operation.
 * @param {number} ms - timeout in milliseconds.
 * @param {string} message - error message used on timeout.
 * @returns {Promise<unknown>} the original result, or a rejection.
 */
function withTimeout(promise, ms, message) {
  if (!Number.isFinite(ms) || ms <= 0) return promise
  let timer
  const timeout = new Promise((_, reject) => {
    timer = setTimeout(() => reject(new Error(message)), ms)
    timer.unref?.()
  })
  return Promise.race([promise, timeout]).finally(() => clearTimeout(timer))
}
