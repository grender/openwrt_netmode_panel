import type { ComponentChildren } from 'preact';
import { useEffect, useState } from 'preact/hooks';
import { LAT_FULL, LAT_OK, LAT_WARN, SIG_OK, SIG_WARN } from '../state/consts';
import type { T } from '../i18n';

/**
 * Кольцо ожидания.
 *
 * Компонент, а не готовая константа-vnode: preact мутирует vnode при
 * вставке, и один объект, отрисованный в двух местах, ломается.
 *
 * aria-hidden: озвучивать нечего, за состояние отвечает aria-busy на самой
 * кнопке. Болтливая заглушка хуже молчаливой.
 */
export const Spin = () => <i class="spin" aria-hidden="true" />;

/**
 * Скелет. Обёртка задаётся параметром, а не зашита: у пилюль b4 своя
 * раскладка, и захардкоженная колонка разложила бы их вертикально, а при
 * подстановке данных они прыгнули бы в строку — то самое дёрганье, ради
 * которого скелет и рисуют.
 */
export function Skel({ n = 3, cls = '', wrap = 'rows' }: { n?: number; cls?: string; wrap?: string }) {
	return (
		<div class={wrap} aria-hidden="true" data-part="skeleton">
			{Array.from({ length: n }, (_, i) => (
				<div key={i} class={`skel ${cls}`} />
			))}
		</div>
	);
}

/** Цвет задержки. Пороги — контракт из макета, а не украшение. */
export const latClass = (ms: number | null): string =>
	ms == null ? 'lat-bad' : ms < LAT_OK ? 'lat-ok' : ms < LAT_WARN ? 'lat-warn' : 'lat-bad';

export const latBg = (ms: number | null): string =>
	ms == null ? 'bg-bad' : ms < LAT_OK ? 'bg-ok' : ms < LAT_WARN ? 'bg-warn' : 'bg-bad';

/**
 * Ширина полоски задержки.
 *
 * У мёртвого узла полоска ПУСТАЯ, а не полная: полная читалась бы как
 * «задержка максимальная», то есть узел живой и очень медленный, — прямо
 * противоположно тому, что произошло.
 */
export const latPct = (ms: number | null): string =>
	ms == null ? '0%' : `${Math.min(100, Math.round((ms / LAT_FULL) * 100))}%`;

export const sigClass = (dbm: number): string =>
	dbm >= SIG_OK ? 'lat-ok' : dbm >= SIG_WARN ? 'lat-warn' : 'lat-bad';

/** Метр задержки: полоска плюс число. */
export function Meter({ ms, busy }: { ms: number | null; busy: boolean }) {
	return (
		<>
			<span class="meter">
				<i class={latBg(ms)} style={{ width: latPct(ms) }} />
			</span>
			<span class={`ms ${latClass(ms)}`}>{busy ? <Spin /> : ms == null ? '—' : `${ms} ms`}</span>
		</>
	);
}

/**
 * Встроенное подтверждение вместо браузерного confirm().
 *
 * confirm() блокирует поток, не переводится, не стилизуется и — главное —
 * не может показать ПОСЛЕДСТВИЯ отдельной строкой. А именно они здесь и
 * решают: смена аплинка рвёт связь и не откатывается сама (ADR-0006).
 */
export function Confirm({
	title,
	text,
	go,
	cancel,
	danger,
	onGo,
	onCancel,
	forAction,
}: {
	title: string;
	text: string;
	go: string;
	cancel: string;
	danger?: boolean;
	onGo: () => void;
	onCancel: () => void;
	forAction: string;
}) {
	return (
		<div class={`confirm${danger ? ' danger' : ''}`} data-part="confirm" data-confirm-for={forAction}>
			<b>{title}</b>
			<p>{text}</p>
			<div class="buttons">
				<button type="button" class="go" onClick={onGo}>
					{go}
				</button>
				<button type="button" class="no" onClick={onCancel}>
					{cancel}
				</button>
			</div>
		</div>
	);
}

/**
 * Секция: заголовок и содержимое.
 *
 * На узком экране — аккордеон: в свёрнутом виде заголовок отдаёт СВОЁ
 * состояние, чтобы разворачивать приходилось только то, что заинтересовало.
 * На широком раскрыты все, и кнопки нет вовсе — aria-expanded, которое
 * нельзя изменить, это ложь скринридеру, а не украшение.
 */
export function Section({
	id,
	title,
	summary,
	wide,
	open,
	onToggle,
	children,
	full,
	t,
}: {
	id: string;
	title: string;
	summary?: string;
	wide: boolean;
	open: boolean;
	onToggle: () => void;
	children: ComponentChildren;
	full?: boolean;
	t: T;
}) {
	if (wide) {
		return (
			<section class={`card${full ? ' full' : ''}`} data-part="card" data-card={id}>
				<h2 data-anchor={`sec-${id}`}>
					{title}
					{summary ? <span class="summary">{summary}</span> : null}
				</h2>
				{children}
			</section>
		);
	}
	const panel = `sec-${id}`;
	return (
		<section
			class={`card${full ? ' full' : ''}`}
			data-part="card"
			data-card={id}
		>
			<button
				type="button"
				class="acc-head"
				aria-expanded={open}
				aria-controls={panel}
				onClick={onToggle}
				data-part="accordion-item"
				data-anchor={`sec-${id}`}
			>
				<h2>{title}</h2>
				{summary ? <span class="acc-sum">{summary}</span> : null}
				<span class="chev" aria-hidden="true">
					›
				</span>
				<span class="sr-only">{open ? t('ui.collapse') : t('ui.expand')}</span>
			</button>
			{open ? (
				<div id={panel} class="acc-panel" data-part="accordion-panel">
					{children}
				</div>
			) : null}
		</section>
	);
}

/** Совпадает ли ширина окна с медиазапросом. */
export function useMedia(query: string): boolean {
	const [match, setMatch] = useState(() =>
		typeof matchMedia === 'function' ? matchMedia(query).matches : false,
	);
	useEffect(() => {
		if (typeof matchMedia !== 'function') return;
		const mq = matchMedia(query);
		const on = () => setMatch(mq.matches);
		on();
		mq.addEventListener('change', on);
		return () => mq.removeEventListener('change', on);
	}, [query]);
	return match;
}
