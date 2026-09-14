/**
 * Locating the piko-expose binary.
 *
 * The helper is shipped per platform as `bin/piko-expose-<os>-<arch>` (see
 * scripts/build-helper.sh). Nothing here downloads anything: a plugin that
 * silently fetches an executable at load time is a supply-chain hole, so a
 * missing helper produces an error that says exactly how to build one.
 *
 * @module piko-remote/helper
 */

import { accessSync, constants } from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

/** Package root, used to find the bundled bin/ directory. */
const PACKAGE_ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..')

/** Node platform names mapped to the Go GOOS values used in the file name. */
const OS_NAMES = Object.freeze({ darwin: 'darwin', linux: 'linux', win32: 'windows' })

/** Node arch names mapped to the Go GOARCH values used in the file name. */
const ARCH_NAMES = Object.freeze({ arm64: 'arm64', x64: 'amd64' })

/**
 * The bundled file name for a platform, e.g. `piko-expose-linux-amd64`.
 *
 * @param {NodeJS.Platform} [platform] - target platform, defaults to this host.
 * @param {string} [arch] - target arch, defaults to this host.
 * @returns {string} the file name, including `.exe` on Windows.
 * @throws {Error} when the platform/arch pair has no build.
 */
export function helperFileName(platform = process.platform, arch = process.arch) {
  const os = OS_NAMES[platform]
  const cpu = ARCH_NAMES[arch]
  if (os === undefined || cpu === undefined) {
    throw new Error(`piko-remote: no piko-expose build is defined for ${platform}/${arch}`)
  }
  return `piko-expose-${os}-${cpu}${os === 'windows' ? '.exe' : ''}`
}

/**
 * Path the package would carry the helper at.
 *
 * @param {NodeJS.Platform} [platform] - target platform.
 * @param {string} [arch] - target arch.
 * @returns {string} absolute path inside this package's bin/.
 */
export function bundledHelperPath(platform = process.platform, arch = process.arch) {
  return path.join(PACKAGE_ROOT, 'bin', helperFileName(platform, arch))
}

/** Raised when no usable helper binary could be found. */
export class HelperMissingError extends Error {
  /**
   * @param {string} attempted - the path that was checked.
   */
  constructor(attempted) {
    super(
      `piko-remote: piko-expose helper not found at ${attempted}. ` +
        'Build it with `scripts/build-helper.sh` (needs a Go toolchain), install a ' +
        'release binary there, or point the `helperPath` config at an existing one.',
    )
    this.name = 'HelperMissingError'
    this.attempted = attempted
  }
}

/**
 * Resolve the helper to execute.
 *
 * An explicit `helperPath` wins and is checked as given; otherwise the bundled
 * per-platform binary is used. Both are checked for executability up front so
 * the failure names the searched path instead of surfacing later as an opaque
 * spawn error.
 *
 * @param {object} [options] - resolution options.
 * @param {string} [options.override] - configured absolute path, or ''.
 * @param {NodeJS.Platform} [options.platform] - target platform.
 * @param {string} [options.arch] - target arch.
 * @param {(candidate: string) => boolean} [options.isExecutable] - test hook.
 * @returns {string} the path to spawn.
 * @throws {HelperMissingError} when nothing executable was found.
 */
export function resolveHelperPath({
  override = '',
  platform = process.platform,
  arch = process.arch,
  isExecutable = defaultIsExecutable,
} = {}) {
  const candidate = override !== '' ? override : bundledHelperPath(platform, arch)
  if (isExecutable(candidate)) return candidate
  throw new HelperMissingError(candidate)
}

/**
 * True when `candidate` is a regular file this process may execute.
 *
 * @param {string} candidate - path to test.
 * @returns {boolean} whether it can be executed.
 */
function defaultIsExecutable(candidate) {
  try {
    accessSync(candidate, constants.X_OK)
    return true
  } catch {
    return false
  }
}
