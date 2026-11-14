import type { ExitMember, NikkiExitsResponse } from '../api/types';
import type { T } from '../i18n';
import type { Lock } from '../state/lock';
import type { Side } from '../state/side';
import { Meter } from './bits';

/** Разделитель в имени цепочки — как exits.ChainSep демона. */
const CHAIN_SEP = ' ⇢ ';

export interface ExitsBlockProps {
	exits: Side<NikkiExitsResponse>;
	lock: Lock;
	locked: boolean;
	t: T;
	onPick(group: string, member: string): void;
}

function memberLabel(m: ExitMember, t: T): string {
	switch (m.kind) {
		case 'chains': {
			// Через какой хоп группа работает сейчас — хвост имени цепочки.
			const i = m.now ? m.now.indexOf(CHAIN_SEP) : -1;
			return i >= 0 && m.now ? t('exits.kind.chains.now', { via: m.now.slice(i + CHAIN_SEP.length) }) : t('exits.kind.chains');
		}
		case 'reject':
			return t('exits.kind.reject');
		case 'chain':
			return t('exits.kind.chain');
		default:
			return t('exits.kind.direct');
	}
}

/**
 * Свои выходы в карточке Nikki: по строке на группу EXIT-*, участники —
 * «через хоп / напрямую / закрыто» с задержкой (ADR-0050). Нет групп или
 * движок молчит — блока нет вовсе: свои выходы необязательны.
 */
export function ExitsBlock(p: ExitsBlockProps) {
	const { t } = p;
	const groups = p.exits?.groups ?? [];
	if (groups.length === 0) return null;

	return (
		<>
			<div class="label">{t('exits.label')}</div>
			{groups.map((g) => (
				<div key={g.name} class="rows">
					<div class="sep">
						<span>{g.name}</span>
					</div>
					{g.members.map((m) => {
						const isNow = g.now === m.name;
						const busy = p.lock.on('exit', `${g.name}|${m.name}`);
						return (
							<button
								key={m.name}
								type="button"
								class={`row${isNow ? ' sel' : ''}${m.kind !== 'reject' && !m.alive ? ' dim' : ''}`}
								disabled={p.locked}
								aria-busy={busy}
								aria-pressed={isNow}
								onClick={() => p.onPick(g.name, m.name)}
							>
								<span class="name">{memberLabel(m, t)}</span>
								{isNow ? <span class="tag tag-pin">{t('exits.now')}</span> : null}
								{m.kind === 'reject' ? <span class="ms">—</span> : <Meter ms={m.delay_ms} busy={busy} />}
							</button>
						);
					})}
				</div>
			))}
			<p class="hint">{t('exits.hint')}</p>
		</>
	);
}
