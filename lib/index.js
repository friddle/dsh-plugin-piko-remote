/**
 * dsh-plugin-piko-remote — host half.
 *
 * Exposes a local port through a remote piko server and hands back the public
 * URL, the same way `opencode-piko-remote` does it: an outbound-only piko
 * upstream connection per endpoint, with a reverse proxy in front of the local
 * target.
 *
 * This file currently carries only the load-time smoke check. That is
 * deliberate: the install path (`dsh plugin add` → profile manifest → layer
 * stack → module resolution) is the part most likely to fail silently, so it
 * gets verified before any tunnel code exists. PLAN.md holds the rest.
 *
 * @module dsh-plugin-piko-remote
 */

/** Loader row identity. Must match the `name` in cordis.patch.yml. */
export const name = 'piko-remote'

/**
 * Register the plugin's host-side contributions.
 *
 * @param {import('@deepseek-ai/cordis').Context} ctx - the plugin's context.
 */
export function apply(ctx) {
  const log = (message) => {
    if (typeof ctx.logger?.info === 'function') ctx.logger.info(message)
    else console.log(message)
  }

  // `webServer` is the local HTTP server the browser talks to, and its port is
  // what `remote_expose` publishes when the caller does not name a port. It is
  // resolved with ctx.get rather than injected: a DSH run without a web
  // surface (`dsh --profile web --help`, or a plain CLI run) has no such
  // service, and the plugin must still load there.
  const webServer = ctx.get('webServer')
  if (webServer === undefined) {
    log('[piko-remote] loaded, but this run has no webServer service to expose')
    return
  }

  log(`[piko-remote] loaded; local web server is on port ${webServer.port}`)
}
