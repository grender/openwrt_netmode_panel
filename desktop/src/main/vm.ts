// The app talks to the VM only through scripts/vm/*.sh: ports and names
// live in scripts/vm/common.sh and are never duplicated here (ADR-0047).
import { execFile, execFileSync, spawn, type ChildProcess } from 'node:child_process'
import { resolve } from 'node:path'
import { app } from 'electron'
import { parseKV } from './proxy'

/** Repo root: desktop/.. unless NETMODED_REPO says otherwise. */
export const REPO = process.env.NETMODED_REPO ?? resolve(app.getAppPath(), '..')

const script = (name: string) => resolve(REPO, 'scripts/vm', `${name}.sh`)

/**
 * env.sh before the app is ready: the VM name is needed for a command-line
 * switch, which only works before `ready`. A second of startup at most.
 */
export function envSync(): Record<string, string> {
  try {
    return parseKV(execFileSync('/bin/sh', [script('env')], { cwd: REPO, timeout: 5000 }).toString())
  } catch {
    return {}
  }
}

/** Runs a script quietly and parses its KEY=value output. Secrets stay here. */
export function query(name: 'env' | 'status' | 'proxy-info', timeout = 10_000): Promise<Record<string, string>> {
  return new Promise((ok, fail) => {
    execFile('/bin/sh', [script(name)], { cwd: REPO, timeout }, (err, stdout, stderr) => {
      if (err) fail(new Error(stderr.trim() || err.message))
      else ok(parseKV(stdout))
    })
  })
}

/** Runs a command, streaming every output line to `log`. Resolves with the exit code. */
export function stream(cmd: string, args: string[], log: (line: string) => void): Promise<number> {
  return new Promise((ok) => {
    log(`$ ${[cmd, ...args].join(' ')}`)
    const p = spawn(cmd, args, { cwd: REPO, stdio: ['ignore', 'pipe', 'pipe'] })
    const feed = (buf: Buffer) => {
      for (const l of buf.toString().split(/\r?\n/)) if (l.trim()) log(stripAnsi(l))
    }
    p.stdout.on('data', feed)
    p.stderr.on('data', feed)
    p.on('error', (e) => {
      log(`✗ ${e.message}`)
      ok(-1)
    })
    p.on('close', (code) => ok(code ?? -1))
  })
}

export const startVm = (log: (l: string) => void) => stream('/bin/sh', [script('start')], log)
export const stopVm = (log: (l: string) => void) => stream('/bin/sh', [script('down')], log)
export const createVm = (log: (l: string) => void) => stream('make', ['vm-up'], log)

/**
 * scripts/vm/tunnel.sh in the background: ssh forwards to the b4 UI, the
 * nikki dashboard and LuCI (ports in common.sh). Its output goes to the log.
 */
export function spawnTunnel(log: (l: string) => void): ChildProcess {
  const p = spawn('/bin/sh', [script('tunnel')], { cwd: REPO, stdio: ['ignore', 'pipe', 'pipe'] })
  const feed = (buf: Buffer) => {
    for (const l of buf.toString().split(/\r?\n/)) if (l.trim()) log(`tunnel: ${stripAnsi(l)}`)
  }
  p.stdout?.on('data', feed)
  p.stderr?.on('data', feed)
  return p
}

/** start.sh exit code: the VM has not been created yet. */
export const NOT_CREATED = 3

function stripAnsi(s: string): string {
  // eslint-disable-next-line no-control-regex
  return s.replace(/\x1b\[[0-9;]*m/g, '')
}
