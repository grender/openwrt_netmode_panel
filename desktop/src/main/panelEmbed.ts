import { MSG_HEIGHT, MSG_VIEWPORT } from '../shared/embed'

// Injected into the panel only when it is shown inside the app (ADR-0047).
// The panel itself stays untouched: in a browser it looks as before.
//
// 1. Look: the panel's page chrome (.page padding, the bordered .shell) would
//    be a second frame inside the window. Without it the panel's cards sit on
//    the same 12px gutters and 9px gaps as the app's own sections.
// 2. One scroll: the frame never scrolls. The script reports the content
//    height and the app sizes the frame to it, so the window is the only
//    scroller. The panel's floating toast (.slot-float, position: fixed) would
//    then sit at the bottom of a frame taller than the screen; the app sends
//    where the visible part of the frame ends (--vp-bottom) and the toast is
//    placed just above that line.
export const PANEL_EMBED_CSS = `
html, body { overflow: hidden !important; }
.page { padding: 0 !important; }
.shell {
  max-width: none !important; border: 0 !important; border-radius: 0 !important;
  background: var(--page) !important; overflow: visible !important;
  /* the frame starts at the window's corner; the first card keeps the gutter */
  padding-top: 12px;
}
.top, .foot {
  margin: 0 12px; background: var(--card) !important;
  border: 1px solid var(--line) !important; border-radius: var(--r-lg);
}
.grid { padding: 9px 12px !important; gap: 9px !important; }
.foot { margin-bottom: 0; }
.slot-float {
  bottom: auto !important;
  top: calc(var(--vp-bottom, 100vh) - 16px) !important;
  transform: translate(-50%, -100%) !important;
}
`


export const PANEL_EMBED_JS = `(() => {
  if (document.getElementById('desktop-embed')) return
  const s = document.createElement('style')
  s.id = 'desktop-embed'
  s.textContent = ${JSON.stringify(PANEL_EMBED_CSS)}
  document.head.appendChild(s)
  // body, not documentElement: the latter is never shorter than the frame,
  // so the frame could grow but never shrink.
  let last = 0
  const report = () => {
    const h = Math.ceil(document.body.getBoundingClientRect().height)
    if (h !== last) {
      last = h
      parent.postMessage({ type: ${JSON.stringify(MSG_HEIGHT)}, height: h }, '*')
    }
  }
  new ResizeObserver(report).observe(document.body)
  report()
  addEventListener('message', (e) => {
    if (e.source === parent && e.data && e.data.type === ${JSON.stringify(MSG_VIEWPORT)}) {
      document.documentElement.style.setProperty('--vp-bottom', e.data.bottom + 'px')
    }
  })
})()`
