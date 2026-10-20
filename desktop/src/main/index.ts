// Menu-bar app for the test VM (ADR-0047): tray icon + one narrow window
// whose page is an accordion of sections, the VM panel being one of them.
import { join } from 'node:path'
import { app, BrowserWindow, clipboard, ipcMain, Menu, nativeImage, screen, session, shell, Tray, type NativeImage, type Rectangle } from 'electron'
import type { AppState } from '../shared/types'
import { Monitor, panelHost } from './monitor'
import { PANEL_EMBED_JS } from './panelEmbed'
import { rewrite } from './links'
import { envSync } from './vm'

// Launched from Finder/Dock the PATH is bare: make, qemu and utmctl's
// helpers live in Homebrew.
process.env.PATH = ['/opt/homebrew/bin', '/usr/local/bin', process.env.PATH].filter(Boolean).join(':')

// The panel is opened as http://<VM name>:<port>, and Chromium resolves that
// name to 127.0.0.1. With a loopback Host the daemon would build no links to
// b4, nikki and LuCI at all (panelHost in internal/httpapi/server.go refuses
// them: through a tunnel they would point at the laptop). Here the app knows
// it is a tunnel and rewrites those links onto its own (links.ts). Must be set
// before `ready`.
const PANEL_HOST = panelHost(envSync())
if (PANEL_HOST !== '127.0.0.1') app.commandLine.appendSwitch('host-resolver-rules', `MAP ${PANEL_HOST} 127.0.0.1`)

let tray: Tray | null = null
let win: BrowserWindow | null = null
/** Window bounds before the panel section grew it to full height. */
let beforePanel: Rectangle | null = null
let quitting = false
const logLines: string[] = []

const monitor = new Monitor(
  (s) => {
    win?.webContents.send('state', s)
    updateTray(s)
  },
  (line) => {
    console.log(line)
    // wait_ssh prints its progress dots one write at a time: glue them on.
    if (/^\.+$/.test(line) && logLines.length > 0) logLines[logLines.length - 1] += line
    else logLines.push(line)
    if (logLines.length > 2000) logLines.splice(0, logLines.length - 2000)
    win?.webContents.send('log', line)
  },
)

if (!app.requestSingleInstanceLock()) app.quit()
app.on('second-instance', showWindow)

app.whenReady().then(async () => {
  app.dock?.hide()
  tray = new Tray(icon('grey', false))
  tray.setToolTip('netmoded VM')
  tray.on('click', () => tray?.popUpContextMenu())
  // The app only ever talks to the VM on this Mac: never through the system
  // proxy — which may point at that very VM, or at a stopped one.
  await session.defaultSession.setProxy({ mode: 'direct' })
  ipc()
  authorizePanel()
  showWindow()
  try {
    await monitor.init()
  } catch (e) {
    monitor.log(`✗ init: ${(e as Error).message}`)
  }
})

// Keep running in the menu bar when windows close.
app.on('window-all-closed', () => {})

app.on('before-quit', (e) => {
  monitor.dispose()
  if (quitting) return
  if (monitor.state.sysProxy !== 'vm') return
  // Never leave the Mac pointing at a proxy nobody manages.
  e.preventDefault()
  quitting = true
  monitor.log('Quitting: turning the system proxy off')
  void monitor.proxyOff().finally(() => app.quit())
})

function showWindow(): void {
  if (win) {
    win.show()
    win.focus()
    return
  }
  win = new BrowserWindow({
    width: 440,
    height: 760,
    minWidth: 400,
    maxWidth: 560,
    title: 'netmoded VM',
    show: false,
    // --page of the panel stylesheet: no white flash before the CSS loads.
    backgroundColor: '#0a0b0c',
    webPreferences: { preload: join(__dirname, '../preload/index.js'), sandbox: true },
  })
  win.once('ready-to-show', () => win?.show())
  win.webContents.on('did-finish-load', () => {
    win?.webContents.send('state', monitor.state)
    for (const l of logLines) win?.webContents.send('log', l)
  })
  win.on('closed', () => {
    win = null
    beforePanel = null
  })
  if (process.env.ELECTRON_RENDERER_URL) void win.loadURL(process.env.ELECTRON_RENDERER_URL)
  else void win.loadFile(join(__dirname, '../renderer/index.html'))
}

/** Tray "Open panel" / "Hide panel": the page toggles the panel, which resizes the window. */
function togglePanel(): void {
  if (beforePanel) {
    win?.webContents.send('closePanel')
    return
  }
  if (!monitor.state.panelReady) return
  showWindow()
  win?.webContents.send('openPanel')
}

/**
 * The panel is an iframe in a file:// page, i.e. cross-site: its
 * SameSite=Strict cookie is never sent there. Every daemon route also
 * accepts a Bearer token, so the main process adds it to requests for the
 * panel origin. The token never reaches the renderer.
 */
function authorizePanel(): void {
  session.defaultSession.webRequest.onBeforeSendHeaders((details, cb) => {
    const origin = monitor.state.panelOrigin
    const token = monitor.creds?.token
    if (origin && token && details.url.startsWith(`${origin}/`)) {
      cb({ requestHeaders: { ...details.requestHeaders, Authorization: `Bearer ${token}` } })
    } else {
      cb({ requestHeaders: details.requestHeaders })
    }
  })
  app.on('web-contents-created', (_e, wc) => {
    // Links inside the panel that open new windows go to the browser.
    wc.setWindowOpenHandler(({ url }) => {
      // The nikki link opens an empty window first and loads the address
      // with the secret into it after an API call (web/panel App.tsx
      // onNikkiPanel). Denying the empty window loses the click; allow it
      // hidden and hand its first navigation to the browser instead.
      if (!url || url === 'about:blank') {
        return { action: 'allow', overrideBrowserWindowOptions: { show: false, webPreferences: { sandbox: true } } }
      }
      openInBrowser(url)
      return { action: 'deny' }
    })
    wc.on('did-create-window', (child) => {
      const hand = (e: { preventDefault(): void }, url: string) => {
        e.preventDefault()
        openInBrowser(url)
        child.destroy()
      }
      child.webContents.on('will-navigate', hand)
      child.webContents.on('will-redirect', hand)
      // The panel closes it itself on an API error; this is the backstop.
      setTimeout(() => child.isDestroyed() || child.destroy(), 20_000)
    })
    // The panel frame drops its own page chrome to look native (panelEmbed.ts).
    wc.on('frame-created', (_ev, { frame }) => {
      frame?.on('dom-ready', () => {
        const origin = monitor.state.panelOrigin
        if (origin && frame.url.startsWith(`${origin}/`)) void frame.executeJavaScript(PANEL_EMBED_JS).catch(() => {})
      })
    })
  })
}

/**
 * Opens a link in the default browser. Links to b4, the nikki dashboard and
 * LuCI point at VM ports the Mac does not have; they go through the ssh
 * tunnel (links.ts, scripts/vm/tunnel.sh).
 */
function openInBrowser(url: string): void {
  const via = rewrite(url, monitor.links, PANEL_HOST)
  if (via) monitor.log(`Opening ${new URL(via).origin} in the browser (tunnel)`)
  void shell.openExternal(via ?? url)
}

/** Panel section open: full screen height; closed: back to the old bounds. */
function setPanelOpen(open: boolean): void {
  if (!win) return
  if (open) {
    if (beforePanel) return
    const b = win.getBounds()
    const area = screen.getDisplayMatching(b).workArea
    beforePanel = b
    win.setBounds({ x: b.x, y: area.y, width: b.width, height: area.height }, true)
  } else if (beforePanel) {
    const { x, y, height } = beforePanel
    win.setBounds({ x, y, height, width: win.getBounds().width }, true)
    beforePanel = null
  }
  updateTray(monitor.state)
}

function copy(id: string): void {
  const v = monitor.copyValue(id)
  if (v !== null) clipboard.writeText(v)
}

function ipc(): void {
  ipcMain.handle('start', () => monitor.start())
  ipcMain.handle('stop', () => monitor.stop())
  ipcMain.handle('setPanelOpen', (_e, open: boolean) => setPanelOpen(open))
  ipcMain.handle('copy', (_e, id: string) => copy(id))
  ipcMain.handle('setSysProxy', async (_e, on: boolean) => {
    try {
      await monitor.setSysProxy(on)
    } catch (e) {
      monitor.log(`✗ system proxy: ${(e as Error).message}`)
    }
  })
  ipcMain.handle('setService', (_e, name: string) => monitor.setService(name))
}

// --- tray -----------------------------------------------------------------

type Color = 'grey' | 'amber' | 'green' | 'red'

function stateLine(s: AppState): string {
  if (s.busy) return `VM: ${s.busy}…`
  if (s.vm === 'running') {
    if (!s.credsLoaded) return 'VM: booting…'
    return `VM: running · mode ${s.mode ?? '?'}`
  }
  return `VM: ${s.vm}`
}

function updateTray(s: AppState): void {
  if (!tray) return
  const color: Color = s.busy || (s.vm === 'running' && !s.credsLoaded) ? 'amber'
    : s.vm === 'running' ? (s.mode === 'nikki' ? 'green' : 'amber')
    : s.error ? 'red' : 'grey'
  tray.setImage(icon(color, s.sysProxy === 'vm'))
  const sysLabel = s.sysProxy === 'vm' ? 'System proxy: ON (VM)' : s.sysProxy === 'other' ? 'System proxy: other proxy' : 'System proxy: off'
  const running = s.vm === 'running'
  tray.setContextMenu(Menu.buildFromTemplate([
    { label: stateLine(s), enabled: false },
    { label: sysLabel, enabled: false },
    ...(running && s.mode && s.mode !== 'nikki' ? [{ label: `⚠ panel mode is "${s.mode}" — proxy won't forward`, enabled: false }] : []),
    { type: 'separator' },
    { label: 'Start VM', enabled: !s.busy && !running, click: () => void monitor.start() },
    { label: 'Stop VM', enabled: !s.busy && running, click: () => void monitor.stop() },
    { label: beforePanel ? 'Hide panel' : 'Open panel', enabled: s.panelReady, click: togglePanel },
    {
      label: 'Copy',
      enabled: s.copies.length > 0,
      submenu: s.copies.map((c) => ({ label: `${c.label}: ${c.preview}`, click: () => copy(c.id) })),
    },
    {
      label: `System proxy via VM${s.service ? ` (${s.service})` : ''}`,
      type: 'checkbox',
      checked: s.sysProxy === 'vm',
      enabled: s.sysProxy === 'vm' || s.proxyReachable,
      click: () => void monitor.setSysProxy(s.sysProxy !== 'vm').catch((e: Error) => monitor.log(`✗ ${e.message}`)),
    },
    { type: 'separator' },
    { label: 'Show window', click: showWindow },
    { label: 'Quit', role: 'quit' },
  ]))
}

const RGB: Record<Color, [number, number, number]> = {
  grey: [142, 142, 147],
  amber: [255, 159, 10],
  green: [48, 209, 88],
  red: [255, 69, 58],
}
const icons = new Map<string, NativeImage>()

/** A filled dot; with the system proxy on, a ring around it. Drawn in code, no asset files. */
function icon(color: Color, ring: boolean): NativeImage {
  const key = `${color}${ring}`
  const hit = icons.get(key)
  if (hit) return hit
  // One @2x bitmap: 18×18 pt in the menu bar.
  const scale = 2
  const n = 18 * scale
  const buf = Buffer.alloc(n * n * 4)
  const c = n / 2
  const [r, g, b] = RGB[color]
  for (let y = 0; y < n; y++) {
    for (let x = 0; x < n; x++) {
      const d = Math.hypot(x + 0.5 - c, y + 0.5 - c) / scale
      // dot radius 4.5; ring between 6.5 and 8
      const a = ring
        ? Math.max(cover(4.5 - d), Math.min(cover(d - 6.5), cover(8 - d)))
        : cover(5.5 - d)
      const i = (y * n + x) * 4
      // BGRA, premultiplied
      buf[i] = Math.round(b * a)
      buf[i + 1] = Math.round(g * a)
      buf[i + 2] = Math.round(r * a)
      buf[i + 3] = Math.round(255 * a)
    }
  }
  const img = nativeImage.createFromBitmap(buf, { width: n, height: n, scaleFactor: scale })
  icons.set(key, img)
  return img
}

/** Antialiased edge: 0 outside, 1 inside, linear over one point. */
function cover(v: number): number {
  return Math.max(0, Math.min(1, v + 0.5))
}
