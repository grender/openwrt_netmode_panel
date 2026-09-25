import { useEffect, useRef, useState } from 'preact/hooks'
import type { Api, AppState } from '../../shared/types'

declare global {
  interface Window {
    vm: Api
  }
}
const vm = window.vm

export function App() {
  const [s, setS] = useState<AppState | null>(null)
  const [log, setLog] = useState<string[]>([])
  const [copied, setCopied] = useState<string | null>(null)
  const logRef = useRef<HTMLPreElement>(null)

  useEffect(() => {
    vm.onState(setS)
    vm.onLog((l) =>
      setLog((prev) =>
        // progress dots of wait_ssh belong to the previous line
        /^\.+$/.test(l) && prev.length > 0 ? [...prev.slice(0, -1), prev[prev.length - 1] + l] : [...prev.slice(-1999), l],
      ),
    )
  }, [])
  useEffect(() => {
    const el = logRef.current
    if (el) el.scrollTop = el.scrollHeight
  }, [log])

  if (!s) return <main class="app"><p class="muted">Loading…</p></main>

  const running = s.vm === 'running'
  const ready = running && s.credsLoaded
  const copy = (id: string) => {
    void vm.copy(id)
    setCopied(id)
    setTimeout(() => setCopied((c) => (c === id ? null : c)), 1200)
  }

  return (
    <main class="app">
      <section class="card">
        <h2>VM</h2>
        <Row label="State" value={s.busy ? `${s.busy}…` : running && !s.credsLoaded ? 'booting…' : s.vm} tone={s.busy || (running && !s.credsLoaded) ? 'warn' : running ? 'ok' : 'muted'} />
        <Row label="Backend" value={s.backend} />
        <Row label="Proxy port" value={running ? (s.proxyReachable ? 'reachable' : 'not reachable') : '—'} tone={s.proxyReachable ? 'ok' : 'muted'} />
        <Row label="Panel mode" value={s.mode ?? '—'} tone={s.mode === 'nikki' ? 'ok' : s.mode ? 'warn' : 'muted'} />
        {ready && s.mode && s.mode !== 'nikki' && (
          <p class="warn">Panel mode is “{s.mode}”: the proxy won't forward traffic. Switch to nikki in the panel.</p>
        )}
        {s.error && <p class="err">{s.error}</p>}
        <div class="buttons">
          <button disabled={!!s.busy || running} onClick={() => void vm.start()}>
            {s.backend === 'none' ? 'Create & start' : 'Start'}
          </button>
          <button disabled={!!s.busy || !running} onClick={() => void vm.stop()}>Stop</button>
          <button disabled={!ready} onClick={() => void vm.openPanel()}>Open panel</button>
        </div>
      </section>

      <section class="card">
        <h2>Proxy settings</h2>
        {s.copies.length === 0 ? (
          <p class="muted">Available when the VM is running.</p>
        ) : (
          <ul class="copies">
            {s.copies.map((c) => (
              <li key={c.id}>
                <span class="label">{c.label}</span>
                <code title={c.preview}>{c.preview}</code>
                <button class="small" onClick={() => copy(c.id)}>{copied === c.id ? 'Copied' : 'Copy'}</button>
              </li>
            ))}
          </ul>
        )}
      </section>

      <section class="card">
        <h2>macOS system proxy</h2>
        <Row
          label="Status"
          value={s.sysProxy === 'vm' ? 'ON — through the VM' : s.sysProxy === 'other' ? 'ON — another proxy' : 'off'}
          tone={s.sysProxy === 'vm' ? 'ok' : s.sysProxy === 'other' ? 'warn' : 'muted'}
        />
        <label class="row">
          <span class="label">Service</span>
          <select value={s.service ?? ''} disabled={s.sysProxy === 'vm'} onChange={(e) => void vm.setService((e.target as HTMLSelectElement).value)}>
            {s.services.map((n) => <option key={n} value={n}>{n}</option>)}
          </select>
        </label>
        <div class="buttons">
          {s.sysProxy === 'vm' ? (
            <button onClick={() => void vm.setSysProxy(false)}>Turn off</button>
          ) : (
            <button disabled={!running || !s.proxyReachable} onClick={() => void vm.setSysProxy(true)}>Use VM as system proxy</button>
          )}
        </div>
        <p class="muted small">
          Host and port only: the proxy still wants the nikki login, and apps that can ask for it will (user and
          password are in the copy list). Turned off automatically when the VM stops or the app quits.
        </p>
      </section>

      <section class="card grow">
        <h2>Log</h2>
        <pre ref={logRef} class="log">{log.join('\n')}</pre>
      </section>
    </main>
  )
}

function Row({ label, value, tone }: { label: string; value: string; tone?: 'ok' | 'warn' | 'muted' }) {
  return (
    <div class="row">
      <span class="label">{label}</span>
      <span class={`value ${tone ?? ''}`}>{value}</span>
    </div>
  )
}
