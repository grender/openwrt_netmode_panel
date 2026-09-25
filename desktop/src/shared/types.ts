// Types shared by the main process, the preload bridge and the renderer.

export type VmState = 'running' | 'stopped' | 'absent' | 'unknown'
export type Busy = null | 'starting' | 'creating' | 'stopping'

/** What the effective macOS proxy (scutil --proxy) points to. */
export type SysProxyStatus = 'vm' | 'other' | 'off'

/** One clipboard entry. `preview` never contains the password. */
export interface CopyItem {
  id: string
  label: string
  preview: string
}

export interface AppState {
  backend: 'utm' | 'qemu' | 'none' | 'unknown'
  vm: VmState
  busy: Busy
  /** ssh answered and proxy-info.sh returned the login. */
  credsLoaded: boolean
  proxyReachable: boolean
  /** Panel mode from /api/status: nikki | b4 | off | unknown; null = panel unreachable. */
  mode: string | null
  sysProxy: SysProxyStatus
  service: string | null
  services: string[]
  copies: CopyItem[]
  /** http://127.0.0.1:<port> of the panel once the token is known; the token itself stays in main. */
  panelOrigin: string | null
  error: string | null
}

export interface Api {
  onState(cb: (s: AppState) => void): void
  onLog(cb: (line: string) => void): void
  start(): Promise<void>
  stop(): Promise<void>
  onOpenPanel(cb: () => void): void
  setPanelOpen(open: boolean): Promise<void>
  copy(id: string): Promise<void>
  setSysProxy(on: boolean): Promise<void>
  setService(name: string): Promise<void>
}
