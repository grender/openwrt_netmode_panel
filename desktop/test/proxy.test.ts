import { describe, expect, it } from 'vitest'
import { buildCopies, parseKV, sq } from '../src/main/proxy'

const get = (id: string, pw = 's3cret') =>
  buildCopies({ host: '127.0.0.1', port: '17890', user: 'nikki', password: pw }).find((c) => c.id === id)!

describe('buildCopies', () => {
  it('gives host and port separately (Firefox)', () => {
    expect(get('host').value).toBe('127.0.0.1')
    expect(get('port').value).toBe('17890')
    expect(get('hostport').value).toBe('127.0.0.1:17890')
  })
  it('builds URLs with the login', () => {
    expect(get('http').value).toBe('http://nikki:s3cret@127.0.0.1:17890')
    expect(get('socks').value).toBe('socks5://nikki:s3cret@127.0.0.1:17890')
  })
  it('url-encodes odd passwords', () => {
    expect(get('http', 'a@b:c/d%').value).toBe('http://nikki:a%40b%3Ac%2Fd%25@127.0.0.1:17890')
  })
  it('never puts the password into previews', () => {
    for (const c of buildCopies({ host: 'h', port: '1', user: 'u', password: 'TOPSECRET' }))
      expect(c.preview).not.toContain('TOPSECRET')
  })
  it('quotes the shell export line', () => {
    expect(get('export', "it's").value).toBe(
      "export http_proxy='http://nikki:it's@127.0.0.1:17890'".replace("it's", "it'\\''s") +
        " https_proxy='http://nikki:it'\\''s@127.0.0.1:17890'" +
        " all_proxy='socks5://nikki:it'\\''s@127.0.0.1:17890'",
    )
  })
  it('omits auth when there is no user', () => {
    const c = buildCopies({ host: 'h', port: '1', user: '', password: '' })
    expect(c.find((x) => x.id === 'http')!.value).toBe('http://h:1')
  })
})

describe('helpers', () => {
  it('parseKV keeps "=" inside values', () => {
    expect(parseKV('a=1\ntoken=x=y\n\njunk\n')).toEqual({ a: '1', token: 'x=y' })
  })
  it('sq escapes single quotes', () => {
    expect(sq("a'b")).toBe(`'a'\\''b'`)
  })
})

describe('b4 mode (ADR-0048)', () => {
  const b4 = buildCopies({ host: '127.0.0.1', port: '17890', user: 'nikki', password: 's3cret' }, 'b4')
  const by = (id: string) => b4.find((c) => c.id === id)

  it('keeps host, port and login — same port in both modes', () => {
    expect(by('host')!.value).toBe('127.0.0.1')
    expect(by('port')!.value).toBe('17890')
    expect(by('user')!.value).toBe('nikki')
    expect(by('socks')!.value).toBe('socks5://nikki:s3cret@127.0.0.1:17890')
  })
  it('drops the HTTP URL: b4 serves SOCKS5 only', () => {
    expect(by('http')).toBeUndefined()
  })
  it('points every export variable at socks5h', () => {
    const v = by('export')!.value
    expect(v).toContain("http_proxy='socks5h://nikki:s3cret@127.0.0.1:17890'")
    expect(v).toContain("https_proxy='socks5h://")
    expect(v).toContain("all_proxy='socks5h://")
    expect(v).not.toContain('http://')
  })
  it('nikki and unknown mode keep the HTTP URL', () => {
    for (const m of ['nikki', null]) {
      const c = buildCopies({ host: 'h', port: '1', user: 'u', password: 'p' }, m)
      expect(c.find((x) => x.id === 'http')).toBeDefined()
    }
  })
})
