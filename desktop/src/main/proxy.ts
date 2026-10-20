// Clipboard strings for the nikki proxy. Pure: values come from
// scripts/vm/env.sh (port) and scripts/vm/proxy-info.sh (login).

export interface ProxyCreds {
  host: string
  port: string
  user: string
  password: string
}

export interface Copy {
  id: string
  label: string
  value: string
  /** Shown in the UI instead of value; value holds the password. */
  preview: string
}

const MASK = '••••••'

/**
 * In b4 mode the same port is b4's SOCKS5 server (ADR-0048): SOCKS5 only,
 * no HTTP proxy. The http:// URL is dropped and the export line points
 * every variable at socks5h:// (curl and most CLIs accept it there).
 */
export function socksOnly(mode: string | null): boolean {
  return mode === 'b4'
}

export function buildCopies(c: ProxyCreds, mode: string | null = null): Copy[] {
  const auth = c.user ? `${encodeURIComponent(c.user)}:${encodeURIComponent(c.password)}@` : ''
  const masked = c.user ? `${c.user}:${MASK}@` : ''
  const hp = `${c.host}:${c.port}`
  const http = `http://${auth}${hp}`
  const socks = `socks5://${auth}${hp}`
  const httpM = `http://${masked}${hp}`
  const socksM = `socks5://${masked}${hp}`
  const exp = (h: string, s: string) =>
    `export http_proxy=${sq(h)} https_proxy=${sq(h)} all_proxy=${sq(s)}`
  const all: Copy[] = [
    { id: 'host', label: 'Host', value: c.host, preview: c.host },
    { id: 'port', label: 'Port', value: c.port, preview: c.port },
    { id: 'user', label: 'User', value: c.user, preview: c.user },
    { id: 'password', label: 'Password', value: c.password, preview: MASK },
    { id: 'hostport', label: 'host:port', value: hp, preview: hp },
    { id: 'http', label: 'HTTP URL', value: http, preview: httpM },
    { id: 'socks', label: 'SOCKS5 URL', value: socks, preview: socksM },
    { id: 'export', label: 'Shell export', value: exp(http, socks), preview: exp(httpM, socksM) },
  ]
  if (!socksOnly(mode)) return all
  const sh = `socks5h://${auth}${hp}`
  const shM = `socks5h://${masked}${hp}`
  return all
    .filter((x) => x.id !== 'http')
    .map((x) => (x.id === 'export' ? { ...x, value: exp(sh, sh), preview: exp(shM, shM) } : x))
}

/** Single-quote for sh. */
export function sq(s: string): string {
  return `'${s.replace(/'/g, `'\\''`)}'`
}

/** KEY=value lines (env.sh, status.sh, proxy-info.sh). */
export function parseKV(text: string): Record<string, string> {
  const out: Record<string, string> = {}
  for (const line of text.split('\n')) {
    const i = line.indexOf('=')
    if (i > 0) out[line.slice(0, i)] = line.slice(i + 1)
  }
  return out
}
