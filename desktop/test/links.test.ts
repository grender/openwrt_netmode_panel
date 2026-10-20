import { describe, expect, it } from 'vitest'
import { portMap, rewrite } from '../src/main/links'

const map = portMap({
  VM_GUEST_B4_UI: '7000', VM_B4_UI_PORT: '17000',
  VM_GUEST_NIKKI_UI: '9090', VM_NIKKI_UI_PORT: '19090',
  VM_GUEST_LUCI: '80', VM_LUCI_PORT: '18080',
})

describe('rewrite', () => {
  it('moves the b4 UI onto the tunnel port', () => {
    expect(rewrite('http://127.0.0.1:7000/', map)).toBe('http://127.0.0.1:17000/')
  })
  it('moves the nikki dashboard and its API port in the query', () => {
    const got = rewrite('http://127.0.0.1:9090/ui/?host=127.0.0.1&hostname=127.0.0.1&port=9090&secret=a%26b', map)!
    const u = new URL(got)
    expect(u.port).toBe('19090')
    expect(u.pathname).toBe('/ui/')
    expect(u.searchParams.get('port')).toBe('19090')
    expect(u.searchParams.get('host')).toBe('127.0.0.1')
    expect(u.searchParams.get('secret')).toBe('a&b')
  })
  it('moves LuCI (implicit port 80)', () => {
    expect(rewrite('http://127.0.0.1/cgi-bin/luci', map)).toBe('http://127.0.0.1:18080/cgi-bin/luci')
  })
  it('leaves other links alone', () => {
    expect(rewrite('https://github.com/x', map)).toBeNull()
    expect(rewrite('http://192.168.9.1:7000/', map)).toBeNull()
    expect(rewrite('http://127.0.0.1:18088/', map)).toBeNull()
    expect(rewrite('', map)).toBeNull()
  })
})

describe('rewrite with the panel host (VM name)', () => {
  it('moves links built from Host: netmoded-vm', () => {
    expect(rewrite('http://netmoded-vm:7000/', map, 'netmoded-vm')).toBe('http://127.0.0.1:17000/')
    expect(rewrite('http://netmoded-vm/cgi-bin/luci', map, 'netmoded-vm')).toBe('http://127.0.0.1:18080/cgi-bin/luci')
    const n = new URL(rewrite('http://netmoded-vm:9090/ui/?host=netmoded-vm&hostname=netmoded-vm&port=9090&secret=s', map, 'netmoded-vm')!)
    expect([n.host, n.searchParams.get('host'), n.searchParams.get('hostname'), n.searchParams.get('port')])
      .toEqual(['127.0.0.1:19090', '127.0.0.1', '127.0.0.1', '19090'])
  })
  it('does not touch other hosts', () => {
    expect(rewrite('http://router.lan:7000/', map, 'netmoded-vm')).toBeNull()
  })
})
