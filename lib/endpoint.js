/**
 * Endpoint naming for piko-remote.
 *
 * An endpoint ID is not just a label: in subdomain mode it becomes the leftmost
 * DNS label of the public URL (`<endpoint>.clauded.friddle.me`), so it has to
 * satisfy the same rule a hostname label does — lowercase alphanumeric with
 * inner dashes, at most 63 characters. The public server enforces this with the
 * same shape (gotty-piko `server/subdomain`), and the Go helper validates it
 * again before it dials out, so a bad name fails fast instead of producing a
 * URL that cannot resolve.
 *
 * @module piko-remote/endpoint
 */

import { randomInt } from 'node:crypto'

/** Characters an endpoint label may contain, in generation order. */
const ALPHABET = 'abcdefghijklmnopqrstuvwxyz0123456789'

/** Maximum length of a DNS label, and therefore of an endpoint ID. */
export const MAX_ENDPOINT_LENGTH = 63

/** The rule the public server applies to an endpoint that becomes a subdomain. */
export const ENDPOINT_PATTERN = /^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$/

/** Used when sanitising leaves nothing usable. */
export const DEFAULT_PREFIX = 'dsh'

/**
 * True when `value` can be published as an endpoint.
 *
 * @param {unknown} value - candidate endpoint ID.
 * @returns {boolean} whether the server would accept it.
 */
export function isValidEndpoint(value) {
  return (
    typeof value === 'string' &&
    value.length > 0 &&
    value.length <= MAX_ENDPOINT_LENGTH &&
    ENDPOINT_PATTERN.test(value)
  )
}

/**
 * Coerce arbitrary text into a usable endpoint ID.
 *
 * Anything outside the alphabet becomes a dash, runs collapse, and the edges
 * are trimmed: a stray trailing dash is not a cosmetic problem, it is an
 * invalid DNS label that fails to resolve. The result is truncated to 63
 * characters and trimmed again, since the cut can expose a new edge dash.
 *
 * @param {unknown} raw - user- or model-supplied name.
 * @param {object} [options] - naming options.
 * @param {string} [options.fallback] - used when nothing usable survives.
 * @returns {string} a valid endpoint ID.
 */
export function sanitizeEndpoint(raw, { fallback = DEFAULT_PREFIX } = {}) {
  const cleaned = String(raw ?? '')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/-{2,}/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, MAX_ENDPOINT_LENGTH)
    .replace(/-+$/g, '')

  return cleaned === '' ? fallback : cleaned
}

/**
 * Build a random endpoint such as `dsh-k3f9qz`.
 *
 * Randomness is not decoration: two people exposing the same app would
 * otherwise collide, and piko load-balances connections across upstreams that
 * share an endpoint, which silently mixes their traffic. Random names also keep
 * a public URL from being guessable when Basic Auth is off.
 *
 * @param {string} [prefix] - stable, recognisable part of the name.
 * @param {number} [length] - number of random characters.
 * @returns {string} a valid endpoint ID.
 */
export function randomEndpoint(prefix = DEFAULT_PREFIX, length = 6) {
  let suffix = ''
  for (let i = 0; i < length; i += 1) {
    suffix += ALPHABET[randomInt(0, ALPHABET.length)]
  }
  return sanitizeEndpoint(`${prefix}-${suffix}`)
}

/**
 * Compute the public URL for an endpoint, mirroring the helper's `--url-mode`.
 *
 * Subdomain mode gives the endpoint the entire origin, so paths reach the
 * target untouched; path mode prefixes them with `/<endpoint>`. DSH's own GUI
 * needs subdomain mode, because its client resolves its API base from
 * `location.origin` and would ask for `/api/...` on the bare host.
 *
 * @param {object} options - URL inputs.
 * @param {string} options.endpoint - the endpoint ID.
 * @param {string} options.remote - piko server URL, e.g. `https://clauded.friddle.me`.
 * @param {'subdomain'|'path'} [options.mode] - routing mode.
 * @returns {string} the public URL, with a trailing slash.
 * @throws {TypeError} when the remote is not an absolute URL with a host.
 */
export function remoteUrlFor({ endpoint, remote, mode = 'subdomain' }) {
  let parsed
  try {
    parsed = new URL(remote)
  } catch {
    throw new TypeError(`invalid remote URL: ${remote}`)
  }
  if (parsed.host === '') {
    throw new TypeError(`remote URL has no host: ${remote}`)
  }
  if (mode === 'path') {
    return `${parsed.protocol}//${parsed.host}/${endpoint}/`
  }
  return `${parsed.protocol}//${endpoint}.${parsed.host}/`
}
