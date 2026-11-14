import type { HopsDraft, HopsMode, HopsResponse, Job } from '../api/types';
import type { Key, T } from '../i18n';
import { jobOn } from '../state/job';
import type { Lock } from '../state/lock';
import type { Side } from '../state/side';
import { Skel, Spin } from './bits';

/**
 * Цепочки до своих нод: свои ноды из экспортов и пул хопов (ADR-0050).
 *
 * Узор — авто-пула: компонент чистый, черновик живёт в App, null значит
 * «совпадает с применённым». Отличие в умолчании: «без цепочек» — это ноль
 * хопов, а не весь список, — и в потолке пула (max_pool).
 */
export interface HopsProps {
	applied: Side<HopsResponse>;
	draft: HopsDraft | null;
	setDraft(d: HopsDraft | null): void;
	lock: Lock;
	locked: boolean;
	running: Job | null;
	t: T;
	onApply(d: HopsDraft): void;
	onReload(): void;
}

const MODES: HopsMode[] = ['off', 'allow', 'deny', 'provider'];

function draftOf(a: HopsResponse): HopsDraft {
	return { mode: a.mode, nodes: [...a.nodes] };
}

/** Сколько хопов будет при таком выборе — тем же расчётом, что у демона. */
export function hopsLeft(d: HopsDraft, a: HopsResponse): number {
	switch (d.mode) {
		case 'off':
			return 0;
		case 'provider':
			return a.provider_pool.length;
		case 'deny':
			return a.available.filter((n) => !d.nodes.includes(n)).length;
		default:
			return a.available.filter((n) => d.nodes.includes(n)).length;
	}
}

function dirty(a: HopsResponse, d: HopsDraft | null): boolean {
	if (!d) return false;
	if (d.mode !== a.mode) return true;
	if (d.mode === 'off' || d.mode === 'provider') return false;
	if (d.nodes.length !== a.nodes.length) return true;
	const was = new Set(a.nodes);
	return d.nodes.some((n) => !was.has(n));
}

export function hopsSummary(v: Side<HopsResponse>, d: HopsDraft | null, t: T): string {
	if (v === undefined) return '…';
	if (v === null) return t('hops.sum.unknown');
	if (v.foreign) return t('hops.sum.foreign');
	if (v.exits_error) return t('hops.sum.broken');
	const eff = d ?? draftOf(v);
	const nodes = v.exits.length;
	const base =
		eff.mode === 'off' ? t('hops.sum.off', { nodes }) : t('hops.sum.on', { n: hopsLeft(eff, v), nodes });
	return dirty(v, d) ? `${base} · ${t('hops.sum.draft')}` : base;
}

export function Hops(p: HopsProps) {
	const { t } = p;
	const a = p.applied;
	if (a === undefined) return <Skel n={4} />;
	if (a === null) return <div class="empty">{t('hops.unknown')}</div>;

	const frozen = a.foreign;
	const off = p.locked || frozen;
	const eff = p.draft ?? draftOf(a);
	const busy = p.lock.on('hops') || jobOn(p.running, 'hops');
	const reloading = p.lock.on('exits') || jobOn(p.running, 'exits');
	const left = hopsLeft(eff, a);
	const over = left > a.max_pool;
	const empty = eff.mode !== 'off' && left === 0;
	const changed = dirty(a, p.draft);
	const hasProvider = a.provider_pool.length > 0;

	const toggle = (name: string) => {
		const has = eff.nodes.includes(name);
		p.setDraft({ ...eff, nodes: has ? eff.nodes.filter((n) => n !== name) : [...eff.nodes, name] });
	};

	return (
		<>
			{frozen ? <p class="hint warn">{t('hops.foreign')}</p> : null}
			<p class="hint">{t('hops.text')}</p>

			{/* Свои ноды — первыми: без них цепочкам не к чему вести, и
			    пустой пул тогда ничего не значит. Секретов тут нет — демон их
			    не отдаёт. */}
			<div class="label">{t('hops.nodes.label', { n: a.exits.length })}</div>
			{a.exits_error ? <p class="hint warn">{t('hops.nodes.broken', { err: a.exits_error })}</p> : null}
			{a.exits.length === 0 && !a.exits_error ? <p class="hint">{t('hops.nodes.none')}</p> : null}
			{a.exits.length > 0 ? (
				<div class="rows">
					{a.exits.map((n) => (
						<div key={n.name} class="row">
							<span class="name">
								{n.name} · {n.country.toUpperCase()}
							</span>
							<span class="tag tag-dim">{t('hops.node.exits', { exits: n.exits.join(', ') })}</span>
							<span class="why">
								{n.address}:{n.port} · SNI {n.sni}
							</span>
						</div>
					))}
				</div>
			) : null}
			{a.skip_sni.length > 0 ? <p class="hint">{t('hops.skip', { sni: a.skip_sni.join(', ') })}</p> : null}
			<button type="button" class="wide" disabled={off} aria-busy={reloading} onClick={p.onReload}>
				{reloading ? (
					<>
						<Spin /> {t('hops.reloading')}
					</>
				) : (
					t('hops.reload')
				)}
			</button>

			<div class="seg">
				{MODES.map((m) => (
					<button
						key={m}
						type="button"
						class="accent"
						aria-pressed={eff.mode === m}
						disabled={off || (m === 'provider' && !hasProvider)}
						// Смена режима отметки не чистит — как у авто-пула.
						onClick={() => p.setDraft({ ...eff, mode: m })}
					>
						{t(`hops.mode.${m}` as Key)}
					</button>
				))}
			</div>
			<p class="hint">{t(`hops.mode.hint.${eff.mode}` as Key)}</p>

			{a.missing.length > 0 ? (
				<p class="hint warn">{t('pool.missing', { names: a.missing.join(', ') })}</p>
			) : null}

			{eff.mode === 'allow' || eff.mode === 'deny' ? (
				<>
					<div class={`label${over ? ' warn' : ''}`}>{t('hops.left', { n: left, max: a.max_pool })}</div>
					{over ? <p class="hint warn">{t('hops.over', { max: a.max_pool })}</p> : null}
					<div class="rows">
						{a.available.map((n) => {
							const marked = eff.nodes.includes(n);
							const inPool = eff.mode === 'deny' ? !marked : marked;
							return (
								<button
									key={n}
									type="button"
									class={`row${inPool ? ' sel' : ''}`}
									aria-pressed={marked}
									disabled={off}
									onClick={() => toggle(n)}
								>
									<span class="name">{n}</span>
									<span class={`tag ${inPool ? 'tag-pin' : 'tag-dim'}`}>{t(inPool ? 'hops.in' : 'hops.out')}</span>
								</button>
							);
						})}
					</div>
				</>
			) : null}
			{eff.mode === 'provider' ? (
				<div class="chips">
					{a.provider_pool.map((n) => (
						<span key={n} class="chip">
							{n}
						</span>
					))}
				</div>
			) : null}

			{changed ? (
				<div class="confirm" data-part="confirm" data-confirm-for="hops">
					<b>{t('hops.dirty.title')}</b>
					<p>{over ? t('hops.over', { max: a.max_pool }) : t('hops.dirty.text')}</p>
					<div class="buttons">
						<button
							type="button"
							class="go"
							// Негодный выбор гасит кнопку, а не ждёт bad_hops от
							// демона после нажатия.
							disabled={off || over || empty}
							aria-busy={busy}
							onClick={() => p.onApply(eff)}
						>
							{busy ? (
								<>
									<Spin /> {t('hops.applying')}
								</>
							) : (
								t('hops.apply')
							)}
						</button>
						<button type="button" class="no" onClick={() => p.setDraft(null)}>
							{t('hops.reset')}
						</button>
					</div>
				</div>
			) : null}
		</>
	);
}
