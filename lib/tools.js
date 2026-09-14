/**
 * The three model-facing tools: remote_expose, remote_status, remote_close.
 *
 * Descriptions carry the security story on purpose. The model decides whether
 * to expose a port, so the warning about what exposing the DSH GUI actually
 * hands over has to live where the model reads it — not only in the README.
 *
 * @module piko-remote/tools
 */

import { defineTool } from '@deepseek-ai/dsh-tools'

/** Shared warning text, kept in one place so no tool drifts out of sync. */
const EXPOSURE_WARNING =
  'Security: the returned URL is reachable by anyone who has it. Exposing the DSH Web UI ' +
  'itself hands control of this machine’s agent (files, shell, credentials) to whoever opens ' +
  'the link, so it stays behind the allowDshUiExpose switch and should always keep Basic Auth on.'

/**
 * Register the plugin's tools on the host context.
 *
 * @param {import('@deepseek-ai/cordis').Context} ctx - plugin context with `tools`.
 * @param {object} deps - wiring.
 * @param {import('./supervisor.js').TunnelSupervisor} deps.supervisor - tunnel owner.
 * @param {object} deps.config - resolved plugin config.
 * @param {() => (number|undefined)} deps.getWebPort - reads this run's DSH web port; a function
 *   because the web server service may come up after this plugin loads.
 */
export function registerTools(ctx, { supervisor, config, getWebPort }) {
  ctx.tools.register(exposeTool(supervisor, config, getWebPort))
  ctx.tools.register(statusTool(supervisor))
  ctx.tools.register(closeTool(supervisor))
}

/**
 * Build the `remote_expose` tool.
 *
 * @param {import('./supervisor.js').TunnelSupervisor} supervisor - tunnel owner.
 * @param {object} config - resolved plugin config.
 * @param {() => (number|undefined)} getWebPort - reads the DSH web port.
 * @returns {object} a tool definition.
 */
function exposeTool(supervisor, config, getWebPort) {
  return defineTool({
    name: 'remote_expose',
    description:
      'Expose a local port through the piko tunnel and return the public https URL. ' +
      'Omit `port` to expose this DSH Web UI’s own port. Basic Auth is on by default with ' +
      'randomly generated credentials, and the tunnel expires after the configured TTL. ' +
      EXPOSURE_WARNING,
    parameters: {
      port: {
        type: 'number',
        description: 'Local port to expose. Defaults to the DSH Web port of this run.',
      },
      name: {
        type: 'string',
        description:
          'Endpoint name, which becomes the first DNS label of the public URL. Lowercase letters, ' +
          'digits and inner dashes only. Defaults to a random name such as dsh-k3f9qz.',
      },
      ttlMinutes: {
        type: 'number',
        description: 'Minutes until the tunnel closes itself. 0 means it stays until closed explicitly.',
      },
      auth: {
        type: 'boolean',
        description: 'Whether to require HTTP Basic Auth on this tunnel. Defaults to the plugin setting.',
      },
    },
    output: {
      schema: {
        type: 'object',
        additionalProperties: false,
        properties: {
          endpoint: { type: 'string', required: true },
          remoteUrl: { type: 'string', required: true },
          localPort: { type: 'integer', required: true },
          state: { type: 'string', required: true },
          authUser: { type: 'string' },
          authPass: { type: 'string' },
          expiresAt: { type: 'string' },
        },
      },
      render: (_args, value) => [
        {
          type: 'text',
          text:
            `Exposed port ${value.localPort} at ${value.remoteUrl}` +
            (value.authUser === undefined
              ? ' (no Basic Auth — anyone with the URL can use it)'
              : ` (Basic Auth ${value.authUser} / ${value.authPass})`) +
            (value.expiresAt === undefined ? '' : `; expires ${value.expiresAt}`),
        },
      ],
    },
    async execute(args) {
      const webPort = getWebPort()
      const requested = args.port === undefined ? webPort : Number(args.port)

      if (webPort !== undefined && requested === webPort && !config.allowDshUiExpose) {
        throw new Error(
          'remote_expose: refusing to expose the DSH Web UI because allowDshUiExpose is false. ' +
            'Exposing it lets anyone with the URL drive this machine’s agent. Ask the user to confirm, ' +
            'then set allowDshUiExpose: true (and keep basicAuth on) before retrying.',
        )
      }

      return supervisor.expose({
        port: args.port,
        name: args.name,
        ttlMinutes: args.ttlMinutes,
        auth: args.auth,
      })
    },
  })
}

/**
 * Build the `remote_status` tool.
 *
 * @param {import('./supervisor.js').TunnelSupervisor} supervisor - tunnel owner.
 * @returns {object} a tool definition.
 */
function statusTool(supervisor) {
  return defineTool({
    name: 'remote_status',
    description:
      'List the tunnels this DSH run currently has open, including their public URLs, local ports, ' +
      'state and expiry. Credentials are included so they can be handed to the user.',
    parameters: {},
    output: {
      schema: {
        type: 'object',
        additionalProperties: false,
        properties: {
          tunnels: {
            type: 'array',
            required: true,
            items: {
              type: 'object',
              additionalProperties: false,
              properties: {
                endpoint: { type: 'string', required: true },
                remoteUrl: { type: 'string', required: true },
                localPort: { type: 'integer', required: true },
                state: { type: 'string', required: true },
                startedAt: { type: 'string', required: true },
                expiresAt: { type: 'string' },
                authUser: { type: 'string' },
                error: { type: 'string' },
              },
            },
          },
        },
      },
      render: (_args, value) => [
        {
          type: 'text',
          text:
            value.tunnels.length === 0
              ? 'No tunnels are open.'
              : value.tunnels
                  .map(
                    (tunnel) =>
                      `${tunnel.endpoint} -> 127.0.0.1:${tunnel.localPort} [${tunnel.state}] ${tunnel.remoteUrl}` +
                      (tunnel.expiresAt === undefined ? '' : ` (expires ${tunnel.expiresAt})`) +
                      (tunnel.error === undefined ? '' : ` (error: ${tunnel.error})`),
                  )
                  .join('\n'),
        },
      ],
    },
    async execute() {
      return { tunnels: supervisor.list() }
    },
  })
}

/**
 * Build the `remote_close` tool.
 *
 * @param {import('./supervisor.js').TunnelSupervisor} supervisor - tunnel owner.
 * @returns {object} a tool definition.
 */
function closeTool(supervisor) {
  return defineTool({
    name: 'remote_close',
    description:
      'Close one tunnel by endpoint, or every open tunnel when no endpoint is given. ' +
      'Closing is immediate: the public URL stops routing and the local helper process exits.',
    parameters: {
      endpoint: {
        type: 'string',
        description: 'Endpoint to close, e.g. dsh-k3f9qz. Omit to close all tunnels.',
      },
    },
    output: {
      schema: {
        type: 'object',
        additionalProperties: false,
        properties: {
          closed: {
            type: 'array',
            required: true,
            items: { type: 'string' },
          },
        },
      },
      render: (_args, value) => [
        {
          type: 'text',
          text: value.closed.length === 0 ? 'Nothing to close.' : `Closed: ${value.closed.join(', ')}`,
        },
      ],
    },
    async execute(args) {
      return { closed: await supervisor.close(args.endpoint) }
    },
  })
}
