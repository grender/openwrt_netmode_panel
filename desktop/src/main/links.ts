// Links the panel builds to the engines' own UIs (b4, nikki dashboard,
// LuCI) point at the host the panel was opened on — in the app that is
// 127.0.0.1 — and at the VM's ports, which the Mac does not have. They are
// reachable through scripts/vm/tunnel.sh on other local ports; this module
// maps one onto the other. Pure: unit-tested.

/** guest port → local tunnel port, both as strings. */
export type PortMap = Record<string, string>

export function portMap(env: Record<string, string>): PortMap {
  const m: PortMap = {}
  const pairs: [string, string][] = [
    ['VM_GUEST_B4_UI', 'VM_B4_UI_PORT'],
    ['VM_GUEST_NIKKI_UI', 'VM_NIKKI_UI_PORT'],
    ['VM_GUEST_LUCI', 'VM_LUCI_PORT'],
  ]
  for (const [guest, local] of pairs) {
    if (env[guest] && env[local]) m[env[guest]!] = env[local]!
  }
  return m
}

/**
 * Rewrites a link to an engine UI onto the tunnel. `panelHost` is the name the
 * app opens the panel under (the VM name, see index.ts): the daemon builds
 * links from the request's Host and refuses loopback ones, so in the app
 * they arrive as http://<VM name>:<VM port>/… Anything else — another
 * host, a port with no tunnel, not http — comes back unchanged (null).
 *
 * The nikki dashboard also carries the Clash API address in its query
 * (`host`, `hostname`, `port` — internal/nikki/panel.go PanelURL) and calls
 * the API there from the browser, so `port` is rewritten too.
 */
export function rewrite(raw: string, map: PortMap, panelHost = ''): string | null {
  let u: URL
  try {
    u = new URL(raw)
  } catch {
    return null
  }
  const ours = (h: string) => isLoopback(h) || (panelHost !== '' && h === panelHost)
  if (u.protocol !== 'http:' || !ours(u.hostname)) return null
  const guest = u.port || '80'
  const local = map[guest]
  if (!local) return null
  u.hostname = '127.0.0.1'
  u.port = local
  if (u.searchParams.get('port') === guest) u.searchParams.set('port', local)
  for (const k of ['host', 'hostname']) {
    const v = u.searchParams.get(k)
    if (v !== null && ours(v)) u.searchParams.set(k, '127.0.0.1')
  }
  return u.toString()
}

function isLoopback(h: string): boolean {
  return h === '127.0.0.1' || h === 'localhost' || h === '[::1]' || h === '::1'
}
