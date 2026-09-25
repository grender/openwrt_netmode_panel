// App state: polls the VM, the proxy port, the panel and the macOS proxy,
// runs start/stop, and turns the system proxy off whenever it would point
// at a dead VM.
import { connect } from 'node:net'
import { readFileSync, writeFileSync, rmSync } from 'node:fs'
import { join } from 'node:path'
import { app } from 'electron'
import type { AppState } from '../shared/types'
import { buildCopies, type Copy } from './proxy'
import * as sys from './sysproxy'
import { query, startVm, stopVm, createVm, NOT_CREATED } from './vm'

const HOST = '127.0.0.1'
const POLL_MS = 2000

interface Creds {
  user: string
  password: string
  token: string
}

export class Monitor {
  env: Record<string, string> = {}
  creds: Creds | null = null
  copies: Copy[] = []
  state: AppState = {
    backend: 'unknown',
    vm: 'unknown',
    busy: null,
    credsLoaded: false,
    proxyReachable: false,
    mode: null,
    sysProxy: 'off',
    service: null,
    services: [],
    copies: [],
    error: null,
  }
  private timer: NodeJS.Timeout | null = null
  private polling = false
  private stateFile = join(app.getPath('userData'), 'state.json')

  constructor(
    private emit: (s: AppState) => void,
    readonly log: (line: string) => void,
  ) {}

  get port(): string {
    return this.env.VM_PROXY_PORT ?? ''
  }
  get panelUrl(): string | null {
    if (!this.creds?.token || !this.env.VM_HTTP_PORT) return null
    return `http://${HOST}:${this.env.VM_HTTP_PORT}/?token=${encodeURIComponent(this.creds.token)}`
  }

  async init(): Promise<void> {
    this.env = await query('env')
    const list = await sys.services()
    this.state.services = list.map((s) => s.name)
    this.state.service = (await sys.defaultService(list)) ?? this.state.services[0] ?? null
    await this.poll()
    this.timer = setInterval(() => void this.poll(), POLL_MS)
  }

  private set(patch: Partial<AppState>): void {
    this.state = { ...this.state, ...patch }
    this.emit(this.state)
  }

  async poll(): Promise<void> {
    if (this.polling) return
    this.polling = true
    try {
      const st = await query('status').catch(() => ({}) as Record<string, string>)
      const vm = (st.state as AppState['vm']) ?? 'unknown'
      const backend = (st.backend as AppState['backend']) ?? 'unknown'
      const reachable = vm === 'running' && (await probe(HOST, Number(this.port)))
      if (vm !== 'running') this.forgetCreds()
      else if (!this.creds && reachable) await this.loadCreds()
      const mode = this.creds && reachable ? await this.panelMode() : null
      const sysProxy = await sys.status(HOST, this.port).catch(() => this.state.sysProxy)
      this.set({ vm, backend, proxyReachable: reachable, mode, sysProxy })

      // A system proxy pointing at a VM that is down leaves the Mac offline.
      if (vm !== 'running' && !this.state.busy && (sysProxy === 'vm' || this.leftOn())) {
        this.log('VM is down: turning the system proxy off')
        await this.proxyOff()
      }
    } finally {
      this.polling = false
    }
  }

  private async loadCreds(): Promise<void> {
    try {
      const kv = await query('proxy-info')
      this.creds = { user: kv.user ?? '', password: kv.password ?? '', token: kv.token ?? '' }
      this.copies = buildCopies({ host: HOST, port: this.port, user: this.creds.user, password: this.creds.password })
      this.set({ credsLoaded: true, copies: this.copies.map(({ id, label, preview }) => ({ id, label, preview })), error: null })
    } catch (e) {
      // ssh not up yet while the VM boots — next poll retries.
      this.set({ error: `proxy-info: ${(e as Error).message}` })
    }
  }

  private forgetCreds(): void {
    if (!this.creds && this.copies.length === 0) return
    this.creds = null
    this.copies = []
    this.set({ credsLoaded: false, copies: [], mode: null })
  }

  private async panelMode(): Promise<string | null> {
    try {
      const r = await fetch(`http://${HOST}:${this.env.VM_HTTP_PORT}/api/status`, {
        headers: { Authorization: `Bearer ${this.creds!.token}` },
        signal: AbortSignal.timeout(1500),
      })
      if (!r.ok) return null
      return ((await r.json()) as { mode?: string }).mode ?? null
    } catch {
      return null
    }
  }

  copyValue(id: string): string | null {
    return this.copies.find((c) => c.id === id)?.value ?? null
  }

  // --- actions -----------------------------------------------------------

  async start(): Promise<void> {
    if (this.state.busy) return
    this.set({ busy: 'starting', error: null })
    try {
      let code = await startVm(this.log)
      if (code === NOT_CREATED) {
        this.set({ busy: 'creating' })
        this.log('VM not created yet: running make vm-up (several minutes)')
        code = await createVm(this.log)
      }
      this.log(code === 0 ? '✓ VM is up' : `✗ exit code ${code}`)
      if (code !== 0) this.set({ error: `start failed (exit ${code}), see log` })
    } finally {
      this.set({ busy: null })
      await this.poll()
    }
  }

  async stop(): Promise<void> {
    if (this.state.busy) return
    this.set({ busy: 'stopping', error: null })
    try {
      if (this.state.sysProxy === 'vm' || this.leftOn()) await this.proxyOff()
      const code = await stopVm(this.log)
      this.log(code === 0 ? '✓ VM stopped' : `✗ exit code ${code}`)
    } finally {
      this.set({ busy: null })
      await this.poll()
    }
  }

  async setSysProxy(on: boolean): Promise<void> {
    const svc = this.state.service
    if (!svc) throw new Error('no network service')
    if (on) {
      if (!this.state.proxyReachable) throw new Error('proxy is not reachable yet')
      this.log(`System proxy → ${HOST}:${this.port} on "${svc}" (apps ask for the nikki login themselves)`)
      await sys.enable(svc, HOST, this.port)
      writeFileSync(this.stateFile, JSON.stringify({ service: svc }))
    } else {
      await this.proxyOff()
    }
    this.set({ sysProxy: await sys.status(HOST, this.port) })
  }

  setService(name: string): void {
    if (this.state.services.includes(name)) this.set({ service: name })
  }

  /** Turns the proxy off on the service we turned it on (or the current one). */
  async proxyOff(): Promise<void> {
    const svc = this.leftOn() ?? this.state.service
    if (!svc) return
    try {
      await sys.disable(svc, HOST, this.port)
      rmSync(this.stateFile, { force: true })
      this.log(`System proxy off on "${svc}"`)
    } catch (e) {
      this.log(`✗ system proxy off: ${(e as Error).message}`)
    }
    this.set({ sysProxy: await sys.status(HOST, this.port).catch(() => this.state.sysProxy) })
  }

  /** Service recorded when we enabled the proxy (survives crashes). */
  private leftOn(): string | null {
    try {
      return (JSON.parse(readFileSync(this.stateFile, 'utf8')) as { service?: string }).service ?? null
    } catch {
      return null
    }
  }

  dispose(): void {
    if (this.timer) clearInterval(this.timer)
  }
}

function probe(host: string, port: number): Promise<boolean> {
  if (!port) return Promise.resolve(false)
  return new Promise((ok) => {
    const s = connect({ host, port })
    const done = (v: boolean) => {
      s.destroy()
      ok(v)
    }
    s.setTimeout(800, () => done(false))
    s.once('connect', () => done(true))
    s.once('error', () => done(false))
  })
}
