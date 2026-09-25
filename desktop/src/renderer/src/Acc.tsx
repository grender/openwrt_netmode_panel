import type { ComponentChildren } from 'preact'

/**
 * One section of the page: the panel's card + accordion header
 * (web/panel/src/ui/bits.tsx), so both look and read the same.
 */
export function Acc(props: {
  id: string
  title: string
  summary?: ComponentChildren
  warn?: boolean
  open: boolean
  onToggle: () => void
  class?: string
  children?: ComponentChildren
}) {
  const body = `sec-${props.id}-body`
  return (
    <section class={`card ${props.class ?? ''}`} data-section={props.id}>
      <button type="button" class="acc-head" aria-expanded={props.open} aria-controls={body} onClick={props.onToggle}>
        <h2>{props.title}</h2>
        {props.summary ? <span class={`acc-sum${props.warn ? ' warn' : ''}`}>{props.summary}</span> : null}
        <span class="chev" aria-hidden="true">›</span>
      </button>
      {/* No children: the section's content lives outside the card (the panel frame). */}
      {props.open && props.children ? (
        <div id={body} class="acc-body">
          {props.children}
        </div>
      ) : null}
    </section>
  )
}
