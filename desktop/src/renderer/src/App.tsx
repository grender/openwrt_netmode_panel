import { useEffect, useRef, useState } from 'preact/hooks'
import type { Api, AppState } from '../../shared/types'
import { MSG_HEIGHT, MSG_VIEWPORT } from '../../shared/embed'
import { Acc } from './Acc'

declare global {
  interface Window {
    vm: Api
  }
}
const vm = window.vm

type Section = 'vm' | 'proxy' | 'sys' | 'panel' | 'log'
const DEFAULT_OPEN: Section[] = ['vm', 'proxy', 'sys']

export function App() {
  const [s, setS] = useState<AppState | null>(null)
  const [log, setLog] = useState<string[]>([])
  const [open, setOpen] = useState<Set<Section>>(new Set(DEFAULT_OPEN))
  // Sections that were open before the panel took the window over.
  const before = useRef<Set<Section> | null>(null)

  const openPanel = () => {
    setOpen((cur) => {
      if (cur.has('panel')) return cur
      before.current = cur
      return new Set<Section>(['panel'])
    })
    void vm.setPanelOpen(true)
  }
  const closePanel = () => {
    setOpen((cur) => {
      if (!cur.has('panel')) return cur
      const prev = before.current ?? new Set(DEFAULT_OPEN)
      before.current = null
      return new Set([...cur, ...prev].filter((x) => x !== 'panel'))
    })
    void vm.setPanelOpen(false)
  }
  const toggle = (id: Section) => {
    if (id === 'panel') return open.has('panel') ? closePanel() : openPanel()
    setOpen((cur) => {
      const next = new Set(cur)
      if (next.has(id)) next.delete(id)
      else next.add(id)
      return next
    })
  }

  useEffect(() => {
    vm.onState(setS)
    vm.onLog((l) =>
      setLog((prev) =>
        // progress dots of wait_ssh belong to the previous line
        /^\.+$/.test(l) && prev.length > 0 ? [...prev.slice(0, -1), prev[prev.length - 1] + l] : [...prev.slice(-1999), l],
      ),
    )
    vm.onOpenPanel(openPanel)
    // A reloaded page starts with the panel closed: let main shrink the window to match.
    void vm.setPanelOpen(false)
  }, [])

  // The panel went away (VM stopped): give the window back.
  const panelOrigin = s?.panelOrigin ?? null
  useEffect(() => {
    if (!panelOrigin && open.has('panel')) closePanel()
  }, [panelOrigin])

  if (!s) return <main class="app"><p class="hint">Loading…</p></main>

  const running = s.vm === 'running'
  const booting = running && !s.credsLoaded
  const panelOpen = open.has('panel')

  return (
    <main class={`app${panelOpen ? ' panel-open' : ''}`}>
      <Acc
        id="panel"
        title="Panel"
        summary={s.panelOrigin ? (panelOpen ? s.panelOrigin.replace('http://', '') : 'open in this window') : 'VM not running'}
        open={panelOpen}
        onToggle={() => toggle('panel')}
      >
        {s.panelOrigin ? null : <p class="hint">The panel is available once the VM is running.</p>}
      </Acc>
      {/* First on the page. Outside any card and full window width: the panel brings its own
          cards (their gutters are aligned by panelEmbed.ts in main). */}
      {panelOpen && s.panelOrigin ? <PanelFrame origin={s.panelOrigin} /> : null}

      <Acc id="vm" title="VM" summary={vmSummary(s)} warn={running && !!s.mode && s.mode !== 'nikki'} open={open.has('vm')} onToggle={() => toggle('vm')}>
        <VmBody s={s} running={running} booting={booting} />
      </Acc>

      <Acc
        id="proxy"
        title="Proxy settings"
        summary={s.copies.length ? `127.0.0.1:${s.copies.find((c) => c.id === 'port')?.preview ?? ''} · ${s.copies.find((c) => c.id === 'user')?.preview ?? ''}` : 'VM not running'}
        open={open.has('proxy')}
        onToggle={() => toggle('proxy')}
      >
        <ProxyBody s={s} />
      </Acc>

      <Acc id="sys" title="System proxy" summary={<SysSummary s={s} />} warn={s.sysProxy === 'other'} open={open.has('sys')} onToggle={() => toggle('sys')}>
        <SysBody s={s} running={running} />
      </Acc>

      <Acc id="log" title="Log" summary={log[log.length - 1] ?? 'empty'} open={open.has('log')} onToggle={() => toggle('log')}>
        <LogBody lines={log} />
      </Acc>
    </main>
  )
}

function vmSummary(s: AppState): string {
  if (s.busy) return `${s.busy}…`
  if (s.vm === 'running') return s.credsLoaded ? `running · ${s.mode ?? '?'}` : 'booting…'
  return s.vm === 'absent' ? 'not created' : s.vm
}

function VmBody({ s, running, booting }: { s: AppState; running: boolean; booting: boolean }) {
  const tone = (v: boolean | null) => (v === null ? '' : v ? 'lat-ok' : 'lat-warn')
  return (
    <>
      <div class="diags">
        <Diag label="state" value={s.busy ? `${s.busy}…` : booting ? 'booting…' : s.vm} tone={s.busy || booting ? 'lat-warn' : running ? 'lat-ok' : ''} />
        <Diag label="backend" value={s.backend} />
        <Diag label="proxy port" value={running ? (s.proxyReachable ? 'reachable' : 'not reachable') : '—'} tone={running ? tone(s.proxyReachable) : ''} />
        <Diag label="panel mode" value={s.mode ?? '—'} tone={s.mode ? tone(s.mode === 'nikki') : ''} />
      </div>
      {running && s.credsLoaded && s.mode && s.mode !== 'nikki' ? (
        <div class="note warn">
          <p>Panel mode is “{s.mode}”: the proxy won't forward traffic. Switch to Nikki in the panel.</p>
        </div>
      ) : null}
      {s.error ? (
        <div class="note err">
          <p>{s.error}</p>
        </div>
      ) : null}
      {running ? (
        <button class="wide danger" disabled={!!s.busy} aria-busy={s.busy === 'stopping'} onClick={() => void vm.stop()}>
          {s.busy === 'stopping' ? 'Stopping…' : 'Stop VM'}
        </button>
      ) : (
        <button class="wide primary" disabled={!!s.busy} aria-busy={!!s.busy} onClick={() => void vm.start()}>
          {s.busy ? `${cap(s.busy)}…` : s.backend === 'none' ? 'Create & start VM' : 'Start VM'}
        </button>
      )}
    </>
  )
}

function ProxyBody({ s }: { s: AppState }) {
  const [copied, setCopied] = useState<string | null>(null)
  if (s.copies.length === 0) return <p class="hint">Available when the VM is running.</p>
  const copy = (id: string) => {
    void vm.copy(id)
    setCopied(id)
    setTimeout(() => setCopied((c) => (c === id ? null : c)), 1200)
  }
  return (
    <div class="copies">
      {s.copies.map((c) => (
        <div class="copy" key={c.id}>
          <span class="copy-label">{c.label}</span>
          <code title={c.preview}>{c.preview}</code>
          <button class="mini" onClick={() => copy(c.id)}>{copied === c.id ? 'Copied' : 'Copy'}</button>
        </div>
      ))}
    </div>
  )
}

function SysSummary({ s }: { s: AppState }) {
  const [cls, text] = s.sysProxy === 'vm' ? ['bg-ok', 'on · VM'] : s.sysProxy === 'other' ? ['bg-warn', 'other proxy'] : ['', 'off']
  return (
    <span class="sys-sum">
      <span class={`dot ${cls}`} style={cls ? undefined : { background: 'var(--muted-3)' }} />
      {text}
      {s.service ? ` · ${s.service}` : ''}
    </span>
  )
}

function SysBody({ s, running }: { s: AppState; running: boolean }) {
  return (
    <>
      <label class="field">
        <span>Network service</span>
        <select value={s.service ?? ''} disabled={s.sysProxy === 'vm'} onChange={(e) => void vm.setService((e.target as HTMLSelectElement).value)}>
          {s.services.map((n) => <option key={n} value={n}>{n}</option>)}
        </select>
      </label>
      {s.sysProxy === 'vm' ? (
        <button class="wide" onClick={() => void vm.setSysProxy(false)}>Turn system proxy off</button>
      ) : (
        <button class="wide primary" disabled={!running || !s.proxyReachable} onClick={() => void vm.setSysProxy(true)}>
          Use VM as system proxy
        </button>
      )}
      <p class="hint">
        Host and port only: the proxy still wants the nikki login, and apps that can ask for it will (user and password
        are under Proxy settings). Turned off automatically when the VM stops or the app quits.
      </p>
    </>
  )
}

/**
 * The panel, as tall as its content: the window is the only scroller. The
 * height comes from the script main injects into the panel (panelEmbed.ts);
 * in return the panel learns where the visible part of the frame ends, for
 * its floating toast.
 */
function PanelFrame({ origin }: { origin: string }) {
  const ref = useRef<HTMLIFrameElement>(null)
  const [height, setHeight] = useState<number | null>(null)

  useEffect(() => {
    const frame = ref.current
    if (!frame) return
    const scroller = frame.closest('.app')!
    const sendViewport = () => {
      const top = frame.getBoundingClientRect().top
      const bottom = Math.max(0, Math.min(frame.clientHeight, window.innerHeight - top))
      frame.contentWindow?.postMessage({ type: MSG_VIEWPORT, bottom }, origin)
    }
    const onMessage = (e: MessageEvent) => {
      if (e.origin !== origin || e.source !== frame.contentWindow) return
      if (e.data?.type === MSG_HEIGHT && typeof e.data.height === 'number') {
        setHeight(e.data.height)
        requestAnimationFrame(sendViewport)
      }
    }
    addEventListener('message', onMessage)
    scroller.addEventListener('scroll', sendViewport, { passive: true })
    addEventListener('resize', sendViewport)
    return () => {
      removeEventListener('message', onMessage)
      scroller.removeEventListener('scroll', sendViewport)
      removeEventListener('resize', sendViewport)
    }
  }, [origin])

  return (
    <iframe
      ref={ref}
      id="sec-panel-body"
      class="panel-frame"
      src={`${origin}/`}
      title="netmoded panel"
      scrolling="no"
      style={height ? { height: `${height}px` } : undefined}
    />
  )
}

function LogBody({ lines }: { lines: string[] }) {
  const ref = useRef<HTMLPreElement>(null)
  useEffect(() => {
    if (ref.current) ref.current.scrollTop = ref.current.scrollHeight
  }, [lines])
  return <pre ref={ref} class="logpre">{lines.join('\n')}</pre>
}

function Diag({ label, value, tone }: { label: string; value: string; tone?: string }) {
  return (
    <div class="diag">
      <span>{label}</span>
      <span class={tone ?? ''}>{value}</span>
    </div>
  )
}

const cap = (x: string) => x[0]!.toUpperCase() + x.slice(1)
