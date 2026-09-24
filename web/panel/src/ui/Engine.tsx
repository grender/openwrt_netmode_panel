import type { Lock } from '../state/lock';
import type { Side } from '../state/side';
import { jobOn, type SvcState } from '../state/job';
import type { Job, Mode, ProxiesResponse, SetsResponse, Status } from '../api/types';
import type { T } from '../i18n';
import { Meter, Skel, Spin } from './bits';

export interface EngineProps {
	mode: Mode | 'unknown';
	nikki: Side<ProxiesResponse>;
	sets: Side<SetsResponse>;
	svcNikki: SvcState;
	svcB4: SvcState;
	status: Status;
	lock: Lock;
	locked: boolean;
	/** Идущий джоб: кнопка держит кольцо до конца операции, а не до 202. */
	running: Job | null;
	t: T;
	onPickProxy(name: string): void;
	onToggleSet(id: string, enabled: boolean): void;
	onTest(): void;
	onMode(m: Mode): void;
}

/**
 * Опции запущенного движка.
 *
 * Раздел ПРИНАДЛЕЖИТ режиму: у Nikki это узлы, у b4 — сеты, у выключенного —
 * сводка обоих. Всё, что живёт независимо от движка и меняется редко —
 * подписка, наборы, проброс, — на экране настроек: главная отвечает на «что
 * сейчас и что переключить», а не на «что настроено надолго» (ADR-0042).
 */
export function Engine(p: EngineProps) {
	if (p.mode === 'b4') return <B4 {...p} />;
	if (p.mode === 'off' || p.mode === 'unknown') return <Off {...p} />;
	return <Nikki {...p} />;
}

/** Заголовок раздела зависит от режима — его считает App, чтобы показать в свёрнутом виде. */
export function engineTitle(mode: Mode | 'unknown', t: T): string {
	if (mode === 'b4') return t('sets.title');
	if (mode === 'nikki') return t('srv.title');
	return t('engine.off.title');
}

export function engineSummary(
	mode: Mode | 'unknown',
	status: Status,
	nikki: Side<ProxiesResponse>,
	sets: Side<SetsResponse>,
	t: T,
): string {
	if (mode === 'b4') {
		const n = sets?.enabled_count ?? status.b4.enabled_count;
		if (n === 1) return t('sets.sum.one', { set: sets?.selected || status.b4.set });
		if (n === 0) return t('sets.sum.none');
		return t('sets.sum.many', { n });
	}
	if (mode === 'nikki') {
		const pinned = nikki?.pinned ?? status.nikki.pinned ?? false;
		const cur = nikki?.selected || status.nikki.set || '—';
		const rows = nikki?.members.length;
		const base = pinned ? t('srv.sum.pinned', { node: cur }) : t('srv.sum.auto', { node: cur });
		return rows ? `${base} · ${t('srv.sum.rows', { n: rows })}` : base;
	}
	return t('engine.off.sum');
}

// ─────────── Nikki ───────────

function Nikki({ nikki, svcNikki, status, lock, locked, t, onPickProxy, onTest }: EngineProps) {
	if (svcNikki === 'starting') {
		return (
			<>
				<Skel n={3} />
				<p class="hint">{t('srv.starting')}</p>
			</>
		);
	}
	if (svcNikki === 'down') return <div class="empty">{t('srv.down')}</div>;
	if (nikki === undefined) return <Skel n={3} />;
	if (!nikki || !nikki.available) return <div class="empty">{t('srv.down')}</div>;

	const members = nikki.members ?? [];
	if (members.length === 0) return <div class="empty">{t('srv.empty')}</div>;

	const pinned = !!nikki.pinned;
	const active = nikki.fixed || nikki.selected;
	const nodes = members.filter((m) => m.kind === 'node');
	const test = nikki.test;

	return (
		<>
			{pinned ? (
				<div class="confirm">
					<b>{t('srv.pinned.note')}</b>
					<div class="buttons">
						<button
							type="button"
							class="go"
							disabled={locked}
							aria-busy={lock.on('proxy', 'AUTO')}
							onClick={() => onPickProxy('AUTO')}
						>
							{lock.on('proxy', 'AUTO') ? <Spin /> : null} {t('srv.auto.back')}
						</button>
					</div>
				</div>
			) : null}

			{/* «Авто» стоит ОТДЕЛЬНО от списка узлов и это не косметика:
			    в списке провайдера это балансировщик, а у нас — снятие
			    закрепления. Одна строка в общем ряду читалась бы как ещё
			    один узел, которым она не является. */}
			<button
				type="button"
				class={`row${pinned ? '' : ' sel'}`}
				disabled={locked}
				aria-busy={lock.on('proxy', 'AUTO')}
				onClick={() => onPickProxy('AUTO')}
			>
				<span class="name">{t('srv.auto.row')}</span>
				<span class="tag">
					{pinned ? t('srv.auto.back') : t('srv.auto.now', { node: nikki.selected || '—' })}
				</span>
				<span class="ms">{lock.on('proxy', 'AUTO') ? <Spin /> : null}</span>
			</button>

			<div class="label">
				{t('srv.group.manual')} · {t('srv.group.count', { rows: members.length, nodes: nodes.length })}
			</div>

			<div class="rows">
				{members.map((m, i) => {
					if (m.kind === 'separator') {
						return (
							<div key={`s${i}`} class="sep">
								<span>{m.name}</span>
								<span>{t('srv.kind.separator')}</span>
							</div>
						);
					}
					if (m.kind === 'auto') return null;
					if (m.kind !== 'node') {
						// Некликабельная строка обязана ВЫГЛЯДЕТЬ некликабельной:
						// пунктир, приглушённое имя, пустая полоска задержки.
						return (
							<div key={`u${i}`} class="row dead">
								<span class="name">{m.name}</span>
								<span class="tag tag-dim">{t('srv.kind.unsupported')}</span>
								<span class="meter" />
								<span class="ms">—</span>
								{m.reason ? <span class="why">{m.reason}</span> : null}
							</div>
						);
					}
					const isActive = active === m.name;
					const busy = lock.on('proxy', m.name);
					return (
						<button
							key={m.name}
							type="button"
							class={`row${isActive ? ' sel' : ''}${m.alive ? '' : ' dim'}`}
							disabled={locked}
							aria-busy={busy}
							onClick={() => onPickProxy(m.name)}
						>
							<span class="name">{m.name}</span>
							{isActive ? (
								<span class={`tag ${pinned ? 'tag-pin' : 'tag-dim'}`}>
									{pinned ? `📌 ${t('srv.tag.pinned')}` : t('srv.tag.auto')}
								</span>
							) : null}
							<Meter ms={m.delay_ms} busy={busy} />
						</button>
					);
				})}
			</div>

			<button
				type="button"
				class="wide"
				disabled={locked}
				aria-busy={lock.on('test')}
				onClick={onTest}
			>
				{lock.on('test') ? (
					<>
						<Spin /> {t('srv.measuring')}
					</>
				) : (
					t('srv.measure')
				)}
			</button>

			{test ? (
				<div class="stats">
					<span>{t('srv.test.total', { n: test.total })}</span>
					<span class="lat-ok">{t('srv.test.measured', { n: test.measured })}</span>
					{/* «Не ответили» и «пропущено» разведены намеренно: про
					    пропущенные неизвестно НИЧЕГО, и записать их в «мёртвые»
					    значило бы оболгать исправный сервер своим таймаутом. */}
					{test.failed ? (
						<span class="lat-bad">{t('srv.test.failed', { n: test.failed })}</span>
					) : null}
					{test.skipped ? (
						<span style={{ color: 'var(--muted-2)' }}>
							{t('srv.test.skipped', { n: test.skipped })}
						</span>
					) : null}
					<span style={{ color: 'var(--muted-2)' }}>
						{(test.elapsed_ms / 1000).toFixed(1)} {t('unit.sec')}
					</span>
				</div>
			) : null}

			<p class="hint">{t('srv.measure.hint')}</p>
			{/* Список — из подписки, а она на экране настроек: сказать это
			    здесь дешевле, чем заставить искать кнопку «Обновить». */}
			{status.subscription ? (
				<p class="hint">
					{t('srv.list.from.sub')} <a href="#sub">{t('settings.link')}</a>
				</p>
			) : null}
		</>
	);
}

// ─────────── b4 ───────────

function B4({ sets, svcB4, lock, locked, t, onToggleSet }: EngineProps) {
	if (svcB4 === 'starting') {
		return (
			<>
				<Skel n={3} cls="pill" wrap="pills" />
				<p class="hint">{t('sets.starting')}</p>
			</>
		);
	}
	if (svcB4 === 'down') return <div class="empty">{t('sets.down')}</div>;
	if (sets === undefined) return <Skel n={3} cls="pill" wrap="pills" />;
	if (!sets || !sets.available) return <div class="empty">{t('sets.down')}</div>;

	const on = sets.sets.filter((x) => x.enabled);

	return (
		<>
			<p class="prose">{t('sets.intro')}</p>
			<div class="pills">
				{sets.sets.map((x) => (
					<button
						key={x.id}
						type="button"
						aria-pressed={x.enabled}
						disabled={locked}
						aria-busy={lock.on('set', x.id)}
						onClick={() => onToggleSet(x.id, !x.enabled)}
					>
						{lock.on('set', x.id) ? <Spin /> : null} {x.name}
					</button>
				))}
			</div>

			{/* Три состояния показаны как НОРМА, а не как ошибка: у b4 нет
			    «текущего сета», включённых может быть сколько угодно, ноль —
			    тоже валидно (ADR-0033). */}
			{on.length === 1 ? <div class="summary">{t('sets.one', { set: on[0]?.name ?? '' })}</div> : null}
			{on.length === 0 ? (
				<div class="confirm">
					<b>{t('sets.none.title')}</b>
					<p>{t('sets.none.text')}</p>
				</div>
			) : null}
			{on.length > 1 ? (
				<div class="confirm">
					<b>{t('sets.many.title', { n: on.length })}</b>
					<p>{t('sets.many.text')}</p>
				</div>
			) : null}

			<p class="hint">{t('sets.hint')}</p>
		</>
	);
}

// ─────────── выключено ───────────

function Off({ status, nikki, sets, lock, locked, running, t, onMode }: EngineProps) {
	const to = (m: Mode) => lock.on('mode', m) || jobOn(running, 'mode', m);
	const nk = status.nikki;
	const nodes = nikki?.members.filter((m) => m.kind === 'node').length;
	const b4on = sets?.sets.filter((x) => x.enabled).map((x) => x.name) ?? [];

	return (
		<>
			<p class="prose">{t('engine.off.text')}</p>
			<div class="diags">
				<div class="diag">
					<span>Nikki</span>
					<span>
						{nk.set
							? nk.pinned
								? `📌 ${nk.set}`
								: t('engine.off.auto', { node: nk.set })
							: '—'}
						{nodes ? ` · ${t('engine.off.nodes', { n: nodes })}` : ''}
					</span>
				</div>
				<div class="diag">
					<span>b4</span>
					<span>{b4on.length ? t('engine.off.sets', { sets: b4on.join(', ') }) : t('sets.sum.none')}</span>
				</div>
			</div>
			<div class="form-row">
				<button type="button" class="wide primary" disabled={locked} aria-busy={to('nikki')} onClick={() => onMode('nikki')}>
					{to('nikki') ? <Spin /> : null} {t('engine.off.on.nikki')}
				</button>
				<button type="button" class="wide" disabled={locked} aria-busy={to('b4')} onClick={() => onMode('b4')}>
					{to('b4') ? <Spin /> : null} {t('engine.off.on.b4')}
				</button>
			</div>
		</>
	);
}
