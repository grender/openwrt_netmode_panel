import { useState } from 'preact/hooks';
import type { AutopoolDraft, AutopoolMode, AutopoolResponse, Job } from '../api/types';
import type { Key, T } from '../i18n';
import { jobOn } from '../state/job';
import type { Lock } from '../state/lock';
import type { Side } from '../state/side';
import { Skel, Spin } from './bits';

/**
 * Авто-пул: из каких узлов движок выбирает сам.
 *
 * Экран отвечает на вопрос, которого не было нигде: «Авто» в карточке
 * Nikki показывает, ЧЕРЕЗ КАКОЙ узел работает обход, но не говорит, из
 * чего выбирали. А выбирали раньше из всего списка подписки — и на живом
 * роутере это привело к домашнему выходу для трафика, который гнали в
 * туннель ради обхода.
 *
 * Компонент чистый: черновик живёт в App, как у наборов. null означает
 * «совпадает с применённым», а не «пусто».
 */
export interface AutopoolProps {
	applied: Side<AutopoolResponse>;
	draft: AutopoolDraft | null;
	setDraft(d: AutopoolDraft | null): void;
	lock: Lock;
	locked: boolean;
	/** Идущий джоб: кнопка держит кольцо до конца операции, а не до 202. */
	running: Job | null;
	t: T;
	onApply(d: AutopoolDraft): void;
}

const MODES: AutopoolMode[] = ['deny', 'allow', 'provider'];

/** Развёртка применённого в черновик. */
function draftOf(a: AutopoolResponse): AutopoolDraft {
	return { mode: a.mode, nodes: [...a.nodes] };
}

/** Сколько узлов останется в пуле при таком выборе. */
export function poolLeft(d: AutopoolDraft, available: string[]): number {
	if (d.mode === 'deny') return available.filter((n) => !d.nodes.includes(n)).length;
	return d.nodes.length;
}

/** Есть ли расхождение с применённым — по составу, а не по порядку. */
function dirty(a: AutopoolResponse, d: AutopoolDraft | null): boolean {
	if (!d) return false;
	if (d.mode !== a.mode) return true;
	if (d.nodes.length !== a.nodes.length) return true;
	const was = new Set(a.nodes);
	return d.nodes.some((n) => !was.has(n));
}

/** Сводка для полки и заголовка раздела. */
export function poolSummary(v: Side<AutopoolResponse>, d: AutopoolDraft | null, t: T): string {
	if (v === undefined) return '…';
	if (v === null) return t('pool.sum.unknown');
	if (v.foreign) return t('pool.sum.foreign');
	const eff = d ?? draftOf(v);
	const left = poolLeft(eff, v.available);
	const base = t(`pool.sum.${eff.mode}` as Key, { n: left, all: v.available.length });
	return dirty(v, d) ? `${base} · ${t('pool.sum.draft')}` : base;
}

export function Autopool(p: AutopoolProps) {
	const { t } = p;
	const applied = p.applied;
	const [ask, setAsk] = useState(false);

	if (applied === undefined) return <Skel n={4} />;
	if (applied === null) return <div class="empty">{t('pool.unknown')}</div>;

	// Чужой файл замораживает раздел целиком: кнопка «Применить» молча
	// затёрла бы чужую работу.
	const frozen = applied.foreign;
	const off = p.locked || frozen;
	const eff = p.draft ?? draftOf(applied);
	const busy = p.lock.on('autopool') || jobOn(p.running, 'autopool');
	const left = poolLeft(eff, applied.available);
	const changed = dirty(applied, p.draft);
	// Режим «как в подписке» предлагается, только если провайдер прислал
	// свой балансировщик. Иначе кнопка вела бы к отказу no_provider_pool.
	const hasProvider = applied.provider_pool.length > 0;

	const toggle = (name: string) => {
		const has = eff.nodes.includes(name);
		p.setDraft({
			...eff,
			nodes: has ? eff.nodes.filter((n) => n !== name) : [...eff.nodes, name],
		});
	};

	return (
		<>
			{frozen ? <p class="hint warn">{t('pool.foreign')}</p> : null}

			<p class="hint">{t('pool.text')}</p>

			<div class="seg">
				{MODES.map((m) => (
					<button
						key={m}
						type="button"
						class="accent"
						aria-pressed={eff.mode === m}
						disabled={off || (m === 'provider' && !hasProvider)}
						// Смена режима список НЕ чистит: владелец переключает
						// «кроме этих» ↔ «только эти» на одних и тех же
						// отметках, и потерять их при переключении значило бы
						// заставить отмечать заново.
						onClick={() => p.setDraft({ ...eff, mode: m })}
					>
						{t(`pool.mode.${m}` as Key)}
					</button>
				))}
			</div>
			<p class="hint">{t(`pool.mode.hint.${eff.mode}` as Key)}</p>
			{!hasProvider ? <p class="hint">{t('pool.no.provider')}</p> : null}

			{/* Пропавшие имена — единственный способ заметить, что провайдер
			    переименовал узел: в режиме «только отмеченные» такое имя молча
			    уменьшает пул, в «все, кроме» — молча увеличивает. */}
			{applied.missing.length > 0 ? (
				<p class="hint warn">{t('pool.missing', { names: applied.missing.join(', ') })}</p>
			) : null}

			{eff.mode === 'provider' ? (
				<>
					<div class="label">{t('pool.provider.label', { n: applied.provider_pool.length })}</div>
					<div class="chips">
						{applied.provider_pool.map((n) => (
							<span key={n} class="chip">
								{n}
							</span>
						))}
					</div>
				</>
			) : (
				<>
					<div class="label">
						{t(`pool.list.${eff.mode}` as Key)} · {t('pool.left', { n: left, all: applied.available.length })}
					</div>
					<div class="rows">
						{applied.available.map((n) => {
							const marked = eff.nodes.includes(n);
							// В пуле ли узел — зависит от режима, и показывать
							// надо именно это, а не «отмечен». «Отмечен» в
							// режиме «кроме этих» означает ровно обратное.
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
									<span class={`tag ${inPool ? 'tag-pin' : 'tag-dim'}`}>
										{t(inPool ? 'pool.in' : 'pool.out')}
									</span>
								</button>
							);
						})}
					</div>
				</>
			)}

			{/* Живой размер группы у движка. Он может не совпасть с расчётом
			    по манифесту: фильтр применяет mihomo, и расхождение — это
			    сигнал, а не помеха. null значит «движок молчит». */}
			{applied.pool_size !== null ? (
				<p class="hint">{t('pool.live', { n: applied.pool_size })}</p>
			) : (
				<p class="hint">{t('pool.live.unknown')}</p>
			)}

			{changed ? (
				<div class="confirm" data-part="confirm" data-confirm-for="autopool">
					<b>{t('pool.dirty.title')}</b>
					<p>{left === 0 ? t('pool.dirty.empty') : t('pool.dirty.text')}</p>
					<div class="buttons">
						<button
							type="button"
							class="go"
							// Пустой пул гасит кнопку, а не ждёт отказа демона:
							// тот ответит empty_pool и тем же, но уже после
							// нажатия и перезапуска движка.
							disabled={off || left === 0}
							aria-busy={busy}
							onClick={() => (eff.mode === 'provider' ? setAsk(true) : p.onApply(eff))}
						>
							{busy ? (
								<>
									<Spin /> {t('pool.applying')}
								</>
							) : (
								t('pool.apply')
							)}
						</button>
						<button type="button" class="no" onClick={() => p.setDraft(null)}>
							{t('pool.reset')}
						</button>
					</div>
					{/* У «как в подписке» список дальше ведёт провайдер, и
					    ручные отметки перестают действовать. Сказать это надо
					    до применения, а не после. */}
					{ask ? (
						<div class="confirm" data-confirm-for="autopool-provider">
							<b>{t('pool.provider.ask')}</b>
							<p>{t('pool.provider.ask.text')}</p>
							<div class="buttons">
								<button
									type="button"
									class="go"
									onClick={() => {
										setAsk(false);
										p.onApply({ mode: 'provider', nodes: [] });
									}}
								>
									{t('pool.apply')}
								</button>
								<button type="button" class="no" onClick={() => setAsk(false)}>
									{t('wifi.cancel')}
								</button>
							</div>
						</div>
					) : null}
				</div>
			) : null}
		</>
	);
}
