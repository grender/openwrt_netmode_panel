// macOS system proxy: networksetup to change it, scutil --proxy to see
// what is actually in effect.
import { execFile } from 'node:child_process'
import { promisify } from 'node:util'
import { sq } from './proxy'
import type { SysProxyStatus } from '../shared/types'

const run = promisify(execFile)

export interface Service {
  name: string
  device: string | null
}

/** `networksetup -listnetworkserviceorder`, disabled services skipped. */
export function parseServiceOrder(text: string): Service[] {
  const out: Service[] = []
  const lines = text.split('\n')
  for (let i = 0; i < lines.length; i++) {
    const m = /^\((\d+)\)\s+(.+)$/.exec(lines[i]!.trim())
    if (!m) continue
    const dev = /Device:\s*([^)\s]+)/.exec(lines[i + 1] ?? '')
    out.push({ name: m[2]!, device: dev ? dev[1]! : null })
  }
  return out
}

/** Top-level `Key : value` pairs of `scutil --proxy`. */
export function parseScutil(text: string): Record<string, string> {
  const out: Record<string, string> = {}
  let depth = 0
  for (const line of text.split('\n')) {
    if (depth === 1) {
      const m = /^\s*(\w+)\s*:\s*(.*?)\s*$/.exec(line)
      if (m && !m[2]!.startsWith('<')) out[m[1]!] = m[2]!
    }
    depth += (line.match(/\{/g)?.length ?? 0) - (line.match(/\}/g)?.length ?? 0)
  }
  return out
}

export function classify(p: Record<string, string>, host: string, port: string): SysProxyStatus {
  const kinds = ['HTTP', 'HTTPS', 'SOCKS'] as const
  const on = kinds.filter((k) => p[`${k}Enable`] === '1')
  if (on.length === 0) return 'off'
  return on.some((k) => p[`${k}Proxy`] === host && p[`${k}Port`] === port) ? 'vm' : 'other'
}

export async function status(host: string, port: string): Promise<SysProxyStatus> {
  const { stdout } = await run('scutil', ['--proxy'])
  return classify(parseScutil(stdout), host, port)
}

export async function services(): Promise<Service[]> {
  const { stdout } = await run('networksetup', ['-listnetworkserviceorder'])
  return parseServiceOrder(stdout)
}

/** The service owning the default route, else the first one with a device. */
export async function defaultService(list: Service[]): Promise<string | null> {
  try {
    const { stdout } = await run('route', ['-n', 'get', 'default'])
    const ifc = /interface:\s*(\S+)/.exec(stdout)?.[1]
    const hit = list.find((s) => s.device === ifc)
    if (hit) return hit.name
  } catch {
    // no default route (offline) — fall through
  }
  return list.find((s) => s.device)?.name ?? null
}

const KINDS = ['webproxy', 'securewebproxy', 'socksfirewallproxy'] as const

/** Which proxy kinds point at the VM: all three (nikki), or SOCKS only (b4, ADR-0048). */
export type Kinds = 'all' | 'socks'

/**
 * Host and port only, without the nikki login. networksetup run as the user
 * does store a login (login-keychain items), but never sets the
 * "Authenticated Proxy" flag, so CFNetwork clients ignore it and get 407
 * anyway (checked with URLSession). Apps that can ask for a proxy login
 * will; the login is on the Copy list. Items left by earlier versions are
 * removed.
 *
 * `socks`: b4 has no HTTP proxy on the port, so the web and secure-web
 * proxies are turned off — pointing them at a SOCKS-only port would break
 * every HTTP client that honours them.
 */
export async function enable(svc: string, host: string, port: string, kinds: Kinds = 'all'): Promise<void> {
  await forgetLogin(host, port)
  if (kinds === 'socks') {
    await networksetup([
      ['-setsocksfirewallproxy', svc, host, port],
      ['-setwebproxystate', svc, 'off'],
      ['-setsecurewebproxystate', svc, 'off'],
    ])
    return
  }
  await networksetup(KINDS.map((k) => [`-set${k}`, svc, host, port]))
}

export async function disable(svc: string, host: string, port: string): Promise<void> {
  await networksetup(KINDS.map((k) => [`-set${k}state`, svc, 'off']))
  await forgetLogin(host, port)
}

/** Keychain protocol codes networksetup uses for web, secure web and SOCKS proxies. */
const KEYCHAIN_PROTOCOLS = ['htpx', 'htsx', 'sox ']

async function forgetLogin(host: string, port: string): Promise<void> {
  for (const r of KEYCHAIN_PROTOCOLS) {
    await run('security', ['delete-internet-password', '-s', host, '-P', port, '-r', r]).catch(() => {})
  }
}

/**
 * Runs the commands as the user; if macOS refuses, repeats them once through
 * the admin-password dialog. networksetup exits 0 on some refusals, so the
 * output is checked too.
 */
async function networksetup(cmds: string[][]): Promise<void> {
  try {
    for (const c of cmds) {
      const { stdout, stderr } = await run('networksetup', c)
      if (/error|requires admin/i.test(stdout + stderr)) throw new Error((stdout + stderr).trim())
    }
  } catch {
    const sh = cmds.map((c) => ['/usr/sbin/networksetup', ...c].map(sq).join(' ')).join(' && ')
    const as = `do shell script "${sh.replace(/\\/g, '\\\\').replace(/"/g, '\\"')}" with administrator privileges`
    await run('osascript', ['-e', as])
  }
}
