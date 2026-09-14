/**
 * Plugin configuration.
 *
 * The values arrive as the cordis row's `config` — the layer a profile's own
 * cordis.patch.yml targets by row id:
 *
 *   - id: piko-remote
 *     config:
 *       remote: https://clauded.friddle.me
 *       autoExpose: true
 *
 * `DEFAULTS` is duplicated as plain data on purpose. The schema below is what
 * DSH validates and renders; the plain object is what `resolveConfig` merges, so
 * a plugin loaded without the loader's schema pass still behaves identically.
 *
 * @module piko-remote/config
 */

import z from '@deepseek-ai/schemastery'

/** Public gotty-piko server, matching opencode-piko-remote's default. */
export const DEFAULT_REMOTE = 'https://clauded.friddle.me'

/** Every knob, with the value used when nothing overrides it. */
export const DEFAULTS = Object.freeze({
  remote: DEFAULT_REMOTE,
  upstreamKey: '',
  endpointPrefix: 'dsh',
  defaultTtlMinutes: 120,
  basicAuth: true,
  urlMode: 'subdomain',
  preserveHost: true,
  allowDshUiExpose: false,
  autoExpose: false,
  helperPath: '',
  connectTimeoutMs: 20000,
})

/** Routing modes the piko server supports. */
export const URL_MODES = Object.freeze(['subdomain', 'path'])

/** Schemastery schema for the `piko-remote` row. */
export const Config = z.object({
  remote: z
    .string()
    .default(DEFAULTS.remote)
    .description('piko 服务器地址，隧道上游连到这里。'),
  upstreamKey: z
    .string()
    .role('secret')
    .default(DEFAULTS.upstreamKey)
    .description('piko 上游鉴权 key；公共服务器不需要，自建且配置了 UPSTREAM_KEY 时填。'),
  endpointPrefix: z
    .string()
    .default(DEFAULTS.endpointPrefix)
    .description('随机 endpoint 的前缀，例如 dsh 会生成 dsh-k3f9qz。'),
  defaultTtlMinutes: z
    .natural()
    .default(DEFAULTS.defaultTtlMinutes)
    .description('remote_expose 未指定 ttlMinutes 时的默认存活分钟数；0 表示不过期。'),
  basicAuth: z
    .boolean()
    .default(DEFAULTS.basicAuth)
    .description('默认是否给隧道加 HTTP Basic Auth（账号密码随机生成）。'),
  urlMode: z
    .string()
    .default(DEFAULTS.urlMode)
    .description('subdomain：https://<endpoint>.<base>/（DSH Web 必须用这个）；path：https://<base>/<endpoint>/。'),
  preserveHost: z
    .boolean()
    .default(DEFAULTS.preserveHost)
    .description('是否把浏览器的 Host 原样转发给本地服务；关掉会把 Host/Origin/Referer 改写成目标地址。'),
  allowDshUiExpose: z
    .boolean()
    .default(DEFAULTS.allowDshUiExpose)
    .description('是否允许暴露 DSH 自己的 Web 界面。默认关：暴露它等于把本机 Agent 的控制权交给任何拿到 URL 的人。'),
  autoExpose: z
    .boolean()
    .default(DEFAULTS.autoExpose)
    .description('插件加载时自动暴露 DSH Web 端口（仍需 allowDshUiExpose 打开）。'),
  helperPath: z
    .string()
    .default(DEFAULTS.helperPath)
    .description('piko-expose 二进制的绝对路径；留空则用包内 bin/piko-expose-<os>-<arch>。'),
  connectTimeoutMs: z
    .natural()
    .default(DEFAULTS.connectTimeoutMs)
    .description('等待 helper 上报 ready 的超时毫秒数。'),
})

/**
 * Merge raw row config over the defaults and drop values that cannot work.
 *
 * Normalising here — rather than trusting the caller — keeps one bad value
 * (say `urlMode: 'banana'`) from turning into a tunnel that connects and then
 * prints a URL nobody can open.
 *
 * @param {object} [raw] - the row's config, possibly partial or untrusted.
 * @returns {typeof DEFAULTS} the effective configuration.
 */
export function resolveConfig(raw = {}) {
  const merged = { ...DEFAULTS, ...(raw ?? {}) }

  const cleaned = {
    remote: typeof merged.remote === 'string' && merged.remote !== '' ? merged.remote : DEFAULTS.remote,
    upstreamKey: typeof merged.upstreamKey === 'string' ? merged.upstreamKey : DEFAULTS.upstreamKey,
    endpointPrefix: typeof merged.endpointPrefix === 'string' ? merged.endpointPrefix : DEFAULTS.endpointPrefix,
    defaultTtlMinutes: toNonNegativeNumber(merged.defaultTtlMinutes, DEFAULTS.defaultTtlMinutes),
    basicAuth: merged.basicAuth !== false,
    urlMode: URL_MODES.includes(merged.urlMode) ? merged.urlMode : DEFAULTS.urlMode,
    preserveHost: merged.preserveHost !== false,
    allowDshUiExpose: merged.allowDshUiExpose === true,
    autoExpose: merged.autoExpose === true,
    helperPath: typeof merged.helperPath === 'string' ? merged.helperPath : DEFAULTS.helperPath,
    connectTimeoutMs: toNonNegativeNumber(merged.connectTimeoutMs, DEFAULTS.connectTimeoutMs),
  }

  return Object.freeze(cleaned)
}

function toNonNegativeNumber(value, fallback) {
  const num = Number(value)
  if (!Number.isFinite(num) || num < 0) return fallback
  return Math.floor(num)
}
