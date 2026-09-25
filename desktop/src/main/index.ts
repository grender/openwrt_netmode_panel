// Menu-bar app for the test VM (ADR-0047): tray icon + control window +
// the VM panel in its own window.
import { join } from 'node:path'
import { app, BrowserWindow, clipboard, ipcMain, Menu, nativeImage, shell, Tray, type NativeImage } from 'electron'
import type { AppState } from '../shared/types'
import { Monitor } from './monitor'

// Launched from Finder/Dock the PATH is bare: make, qemu and utmctl's
// helpers live in Homebrew.
process.env.PATH = ['/opt/homebrew/bin', '/usr/local/bin', process.env.PATH].filter(Boolean).join(':')

let tray: Tray | null = null
let win: BrowserWindow | null = null
let panel: BrowserWindow | null = null
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
  ipc()
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
    width: 460,
    height: 720,
    title: 'netmoded VM',
    show: false,
    webPreferences: { preload: join(__dirname, '../preload/index.js'), sandbox: true },
  })
  win.once('ready-to-show', () => win?.show())
  win.webContents.on('did-finish-load', () => {
    win?.webContents.send('state', monitor.state)
    for (const l of logLines) win?.webContents.send('log', l)
  })
  win.on('closed', () => (win = null))
  if (process.env.ELECTRON_RENDERER_URL) void win.loadURL(process.env.ELECTRON_RENDERER_URL)
  else void win.loadFile(join(__dirname, '../renderer/index.html'))
}

function openPanel(): void {
  const url = monitor.panelUrl
  if (!url) {
    monitor.log('Panel is not available yet (VM down or token not read)')
    return
  }
  if (panel) {
    panel.show()
    panel.focus()
    return
  }
  const origin = new URL(url).origin
  panel = new BrowserWindow({
    width: 1100,
    height: 800,
    title: 'netmoded panel (VM)',
    webPreferences: { sandbox: true, contextIsolation: true, nodeIntegration: false },
  })
  // Only the panel lives here; anything else goes to the browser.
  panel.webContents.on('will-navigate', (e, to) => {
    if (new URL(to).origin !== origin) {
      e.preventDefault()
      void shell.openExternal(to)
    }
  })
  panel.webContents.setWindowOpenHandler(({ url: to }) => {
    void shell.openExternal(to)
    return { action: 'deny' }
  })
  panel.on('closed', () => (panel = null))
  void panel.loadURL(url)
}

function copy(id: string): void {
  const v = monitor.copyValue(id)
  if (v !== null) clipboard.writeText(v)
}

function ipc(): void {
  ipcMain.handle('start', () => monitor.start())
  ipcMain.handle('stop', () => monitor.stop())
  ipcMain.handle('openPanel', () => openPanel())
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
    { label: 'Open panel', enabled: running && s.credsLoaded, click: openPanel },
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
