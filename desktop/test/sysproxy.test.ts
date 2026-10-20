import { describe, expect, it, vi } from 'vitest'

vi.mock('electron', () => ({}))
const { classify, parseScutil, parseServiceOrder } = await import('../src/main/sysproxy')

const SCUTIL_ON = `<dictionary> {
  ExceptionsList : <array> {
    0 : *.local
    1 : 169.254/16
  }
  FTPPassive : 1
  HTTPEnable : 1
  HTTPPort : 17890
  HTTPProxy : 127.0.0.1
  HTTPSEnable : 1
  HTTPSPort : 17890
  HTTPSProxy : 127.0.0.1
  SOCKSEnable : 1
  SOCKSPort : 17890
  SOCKSProxy : 127.0.0.1
}
`
const SCUTIL_OFF = `<dictionary> {
  ExceptionsList : <array> {
    0 : *.local
  }
  FTPPassive : 1
  HTTPEnable : 0
  HTTPSEnable : 0
}
`

describe('scutil --proxy', () => {
  it('parses top-level keys and skips nested arrays', () => {
    const p = parseScutil(SCUTIL_ON)
    expect(p.HTTPProxy).toBe('127.0.0.1')
    expect(p.SOCKSPort).toBe('17890')
    expect(p['0']).toBeUndefined()
  })
  it('classifies vm / other / off', () => {
    expect(classify(parseScutil(SCUTIL_ON), '127.0.0.1', '17890')).toBe('vm')
    expect(classify(parseScutil(SCUTIL_ON), '127.0.0.1', '9999')).toBe('other')
    expect(classify(parseScutil(SCUTIL_OFF), '127.0.0.1', '17890')).toBe('off')
  })
})

describe('networksetup -listnetworkserviceorder', () => {
  it('maps services to devices and skips disabled ones', () => {
    const text = `An asterisk (*) denotes that a network service is disabled.
(1) Wi-Fi
(Hardware Port: Wi-Fi, Device: en0)

(2) Thunderbolt Bridge
(Hardware Port: Thunderbolt Bridge, Device: bridge0)

(*) Bluetooth PAN
(Hardware Port: Bluetooth PAN, Device: en6)

(3) Tailscale
(Hardware Port: Tailscale, Device: )
`
    expect(parseServiceOrder(text)).toEqual([
      { name: 'Wi-Fi', device: 'en0' },
      { name: 'Thunderbolt Bridge', device: 'bridge0' },
      { name: 'Tailscale', device: null },
    ])
  })
})
