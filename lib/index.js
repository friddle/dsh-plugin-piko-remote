/**
 * dsh-plugin-piko-remote — host half.
 *
 * Exposes a local port through a remote piko server and hands back the public
 * URL, the same way `opencode-piko-remote` does it: an outbound-only piko
 * upstream connection per endpoint, with a reverse proxy in front of the local
 * target. Three pieces do the work:
 *
 *   - `supervisor.js` owns one `piko-expose` child per endpoint (the Go helper
 *     in `go/`, built into `bin/`), and turns its JSON event stream into state.
 *   - `endpoint.js` names endpoints so they are valid DNS labels, because in
 *     subdomain mode the endpoint *is* the leftmost label of the public URL.
 *   - `tools.js` exposes remote_expose / remote_status / remote_close.
 *
 * @module dsh-plugin-piko-remote
 */

import path from 'node:path'
import { fileURLToPath } from 'node:url'

import { Config, resolveConfig } from './config.js'
import { resolveHelperPath } from './helper.js'
import { TunnelSupervisor } from './supervisor.js'
import { registerTools } from './tools.js'

/** Loader row identity. Must match the `name` in cordis.patch.yml. */
export const name = 'piko-remote'

/**
 * Host services this plugin needs before it can work. `tools` is required —
 * without the tool registry there is nothing to register.
 *
 * `subprocess` and `webServer` are deliberately NOT injected: they only exist
 * in some profiles (headless runs have no web server), and inject would then
 * keep this plugin — and its tools — from loading at all. They are picked up
 * through `ctx.inject` below, which also handles the case where the service is
 * provided *after* this plugin loads. Reading them once with `ctx.get` would
 * latch `undefined` forever in that ordering.
 */
export const inject = ['tools']

/** Schemastery config schema, surfaced in DSH's configuration UI. */
export { Config }

/** Package root: the working directory for the helper child process. */
const PACKAGE_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

/**
 * Register the plugin's host-side contributions.
 *
 * @param {import('@deepseek-ai/cordis').Context} ctx - the plugin's context.
 * @param {object} [config] - this row's config; see config.js for the shape.
 */
export function apply(ctx, config) {
  const settings = resolveConfig(config)

  const log = (message) => {
    if (typeof ctx.logger?.info === 'function') ctx.logger.info(message)
    else console.log(message)
  }
  const warn = (message) => {
    if (typeof ctx.logger?.warn === 'function') ctx.logger.warn(message)
    else console.warn(message)
  }

  // `webServer` is the local HTTP server the browser talks to, and its port is
  // what `remote_expose` publishes when the caller does not name a port.
  let webPort
  // The subprocess service is how the helper gets started.
  let subprocess

  // A missing helper is reported now, at load, with the path that was checked:
  // finding out at the first remote_expose call is a much worse moment.
  let helperPath = ''
  try {
    helperPath = resolveHelperPath({ override: settings.helperPath })
  } catch (error) {
    warn(`[piko-remote] ${error.message}`)
  }

  const supervisor = new TunnelSupervisor({
    spawn: (spec) => {
      if (subprocess === undefined) {
        throw new Error('piko-remote: this run has no subprocess service, so piko-expose cannot be started')
      }
      return subprocess.spawn(spec)
    },
    config: settings,
    defaultPort: () => webPort,
    helperPath,
    cwd: PACKAGE_ROOT,
    logger: log,
  })

  registerTools(ctx, { supervisor, config: settings, getWebPort: () => webPort })

  ctx.inject(['webServer'], (webCtx) => {
    webPort = webCtx.webServer.port
    log(`[piko-remote] local DSH web server on port ${webPort}`)
    maybeAutoExpose()
    return () => {
      webPort = undefined
    }
  })

  ctx.inject(['subprocess'], (subCtx) => {
    subprocess = subCtx.subprocess
    return () => {
      subprocess = undefined
    }
  })

  // Tunnels must not outlive the plugin: an orphaned helper keeps a public
  // endpoint routed to a port that no longer serves anything.
  ctx.effect(() => () => {
    void supervisor.dispose()
  })

  log(
    `[piko-remote] loaded; remote=${settings.remote} urlMode=${settings.urlMode} ` +
      `helper=${helperPath === '' ? 'MISSING' : helperPath} ` +
      `autoExpose=${settings.autoExpose} allowDshUiExpose=${settings.allowDshUiExpose}`,
  )

  if (settings.autoExpose && !settings.allowDshUiExpose) {
    warn('[piko-remote] autoExpose ignored: allowDshUiExpose is false')
  }

  /** Expose the DSH web port once, as soon as everything needed is available. */
  let autoExposeStarted = false
  function maybeAutoExpose() {
    if (autoExposeStarted || !settings.autoExpose || !settings.allowDshUiExpose) return
    if (webPort === undefined || helperPath === '') return
    autoExposeStarted = true

    // Deliberately not logging the URL or the credentials: DSH logs are files
    // on disk, and this URL is the only thing guarding the port.
    void supervisor.expose({ port: webPort, ttlMinutes: settings.defaultTtlMinutes }).then(
      (tunnel) => log(`[piko-remote] auto-exposed the DSH Web UI on endpoint ${tunnel.endpoint}`),
      (error) => warn(`[piko-remote] autoExpose failed: ${error.message}`),
    )
  }
}
