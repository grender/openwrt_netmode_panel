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
