import { useMemo } from 'preact/hooks';
import type {
	HopsResponse,
	Mode,
	NikkiExitsResponse,
	PathTestResponse,
	ProxiesResponse,
	RulesetsResponse,
	WatchState,
	WatchTarget,
} from '../api/types';
import { fmtTime, type Key, type Lang, type T } from '../i18n';
import type { Lock } from '../state/lock';
import { buildRouteMap, type Col, type GEdge, type GNode, type Route, type RouteMap } from '../state/routemap';
import type { Side } from '../state/side';
import { Spin } from './bits';

/**
 * Экран «Детектор отказов» (ADR-0051): схема всех путей по хопам и место,
 * где рвётся связь.
 *
 * Экран ничего не переключает. Данные — те же, что у главной и наблюдателя;
 * своё у него одно — замер звеньев по кнопке (POST /api/nikki/pathtest).
 * Кнопкой, а не сам по себе: проба выхода через ISP-прокси — новое
 * соединение, а прокси отказывает на частых соединениях.
 */
export interface FailsProps {
	mode: Mode | 'unknown';
	rulesets: Side<RulesetsResponse>;
	exits: Side<NikkiExitsResponse>;
	hops: Side<HopsResponse>;
	proxies: Side<ProxiesResponse>;
	watch: WatchState | undefined;
	test: { at: string; res: PathTestResponse } | null;
	onTest(): void;
	lock: Lock;
	locked: boolean;
	t: T;
	lang: Lang;
}

// ── геометрия схемы ──
const COLS: Col[] = ['client', 'router', 'sub', 'node', 'exit', 'net'];
const BW = 124;
const BH = 40;
const GAP_X = 36;
const ROW = 54;
const PAD = 12;
const colX = (c: Col) => PAD + COLS.indexOf(c) * (BW + GAP_X);
const WIDTH = PAD * 2 + COLS.length * BW + (COLS.length - 1) * GAP_X;

/** Обрезка подписи под ширину блока: SVG сам текст не переносит и не режет. */
const clip = (s: string, n = 17) => ([...s].length > n ? `${[...s].slice(0, n - 1).join('')}…` : s);

export function Fails(p: FailsProps) {
	const { t, lang } = p;
	const session = p.watch?.active ? p.watch : null;
	const map = useMemo(
		() =>
			buildRouteMap({
				rulesets: p.rulesets,
				exits: p.exits,
				hops: p.hops,
				proxies: p.proxies,
				test: p.test?.res ?? null,
				client: session ? { label: session.ip ?? '', targets: session.targets ?? [] } : null,
			}),
		[p.rulesets, p.exits, p.hops, p.proxies, p.test, session],
	);

	if (p.mode !== 'nikki') {
		return (
			<div class="card full" data-part="fails">
				<div class="note warn">
					<h3>{t('fails.off.title')}</h3>
					<p>{t('fails.off.text')}</p>
				</div>
			</div>
		);
	}

	const busy = p.lock.on('pathtest');
	const label = (n: GNode): { main: string; sub: string } => {
		switch (n.role) {
			case 'router':
				return { main: t('fails.node.router'), sub: '' };
			case 'net':
				return { main: t('fails.node.net'), sub: '' };
			case 'client':
				return { main: n.label, sub: t('fails.node.client') };
			case 'home':
				return { main: t('fails.node.home'), sub: t('fails.node.home.sub') };
			case 'isp':
				return { main: t('fails.node.isp'), sub: n.label };
			case 'vps':
				return { main: t('fails.node.vps'), sub: n.label };
			case 'subip':
				return { main: t('fails.node.subip'), sub: n.label };
			case 'node':
				return { main: n.label, sub: t('fails.node.own') };
			case 'hop+tunnel':
				return { main: n.label, sub: t('fails.node.hoptunnel') };
			case 'tunnel':
				return { main: n.label, sub: t('fails.node.tunnel') };
			default:
				return { main: n.label, sub: t('fails.node.hop') };
		}
	};
	const nameOf = (id: string): string => {
		const n = map.nodes.find((x) => x.id === id);
		if (!n) return id;
		const l = label(n);
		return n.role === 'isp' || n.role === 'vps' || n.role === 'subip' ? `${l.main} (${l.sub})` : l.main;
	};

	const failsTotal = map.routes.reduce((a, r) => a + r.failed.length, 0);

	return (
		<div class="card full fails" data-part="fails">
			<div class="f-bar">
				<button type="button" class="mini" disabled={p.locked} aria-busy={busy} onClick={p.onTest}>
					{busy ? <Spin /> : null}
					{t('fails.test')}
				</button>
				<span class="f-when">
					{busy
						? t('fails.testing')
						: p.test
							? t('fails.tested', { when: fmtTime(p.test.at, lang), sec: (p.test.res.elapsed_ms / 1000).toFixed(1) })
							: t('fails.untested')}
				</span>
			</div>

			<Graph map={map} label={label} t={t} />

			<p class="f-legend">
				<span class="f-key ok">{t('fails.legend.ok')}</span>
				<span class="f-key fail">{t('fails.legend.fail')}</span>
				<span class="f-key unknown">{t('fails.legend.unknown')}</span>
				<span class="f-key used">{t('fails.legend.used')}</span>
			</p>

			<p class="hint">
				{session
					? t('fails.dev.on', { ip: session.ip ?? '', n: failsTotal })
					: t('fails.dev.off')}{' '}
				<a href="#watch">{t(session ? 'fails.dev.open' : 'fails.dev.pick')} ›</a>
			</p>

			<div class="f-routes">
				{map.routes.map((r) => (
					<RouteCard key={r.id} r={r} map={map} nameOf={nameOf} session={!!session} t={t} />
				))}
			</div>

			<p class="hint">{t('fails.limits')}</p>
		</div>
	);
}

function Graph({ map, label, t }: { map: RouteMap; label: (n: GNode) => { main: string; sub: string }; t: T }) {
	// Раскладка: столбец на роль, внутри столбца — по порядку, столбцы
	// выровнены по середине самого высокого.
	const byCol = new Map<Col, GNode[]>();
	for (const c of COLS) byCol.set(c, []);
	for (const n of map.nodes) byCol.get(n.col)?.push(n);
	// Столбец серверов подписки прижат к верху и оставляет нижний ряд
	// свободным, столбец выходов растянут: выход туннеля — в верхнем ряду,
	// «домашний IP» — в нижнем. Так путь туннеля идёт по верхнему краю, а
	// прямой — по нижнему, и ни один не пересекает чужие блоки.
	const subs = byCol.get('sub') ?? [];
	const exits = byCol.get('exit') ?? [];
	const tallest = Math.max(3, subs.length + 1, exits.length, ...[...byCol.values()].map((a) => a.length));
	const height = PAD * 2 + tallest * ROW - (ROW - BH);
	const pos = new Map<string, { x: number; y: number }>();
	const put = (n: GNode, row: number) => pos.set(n.id, { x: colX(n.col), y: PAD + row * ROW });
	for (const [c, list] of byCol) {
		if (c === 'sub') {
			list.forEach((n, i) => put(n, i));
		} else if (c === 'exit') {
			const top = list.filter((n) => n.role === 'subip');
			const bottom = list.filter((n) => n.role === 'home');
			const mid = list.filter((n) => n.role !== 'subip' && n.role !== 'home');
			top.forEach((n) => put(n, 0));
			bottom.forEach((n) => put(n, tallest - 1));
			const free = tallest - top.length - bottom.length;
			mid.forEach((n, i) => put(n, top.length + (free - mid.length) / 2 + i));
		} else {
			list.forEach((n, i) => put(n, (tallest - list.length) / 2 + i));
		}
	}
	// Блок «красный», когда до него не дошли ни одним путём, а пробовали.
	const inbound = new Map<string, GEdge[]>();
	for (const e of map.edges) inbound.set(e.to, [...(inbound.get(e.to) ?? []), e]);
	const dead = (id: string) => {
		const es = inbound.get(id) ?? [];
		return es.length > 0 && es.some((e) => e.state === 'fail') && !es.some((e) => e.state === 'ok');
	};

	return (
		<div class="f-scroll">
			<svg
				class="f-graph"
				viewBox={`0 0 ${WIDTH} ${height}`}
				width={WIDTH}
				height={height}
				role="img"
				aria-label={t('fails.graph.aria')}
			>
				{map.edges.map((e) => {
					const a = pos.get(e.from);
					const b = pos.get(e.to);
					if (!a || !b) return null;
					const x1 = a.x + BW;
					const y1 = a.y + BH / 2;
					const x2 = b.x;
					const y2 = b.y + BH / 2;
					const dx = (x2 - x1) / 2;
					return (
						<g key={e.id} class={`f-edge ${e.state}${e.used ? ' used' : ''}`}>
							<path d={`M${x1},${y1} C${x1 + dx},${y1} ${x2 - dx},${y2} ${x2},${y2}`}>
								<title>
									{t(`fails.state.${e.state}` as Key)}
									{e.ms != null ? ` · ${e.ms} ${t('fails.ms')}` : ''}
								</title>
							</path>
							{e.fails > 0 ? (
								<g class="f-badge" transform={`translate(${(x1 + x2) / 2},${(y1 + y2) / 2})`}>
									<circle r="10" />
									<text text-anchor="middle" dy="3.5">
										{e.fails > 99 ? '99+' : e.fails}
									</text>
								</g>
							) : null}
						</g>
					);
				})}
				{map.nodes.map((n) => {
					const at = pos.get(n.id);
					if (!at) return null;
					const l = label(n);
					return (
						<g key={n.id} class={`f-node ${n.role.replace('+', '-')}${dead(n.id) ? ' dead' : ''}`} transform={`translate(${at.x},${at.y})`}>
							<title>{l.sub ? `${l.main} · ${l.sub}` : l.main}</title>
							<rect width={BW} height={BH} rx="7" />
							<text class="f-main" x={BW / 2} y={l.sub ? 16 : 24} text-anchor="middle">
								{clip(l.main)}
							</text>
							{l.sub ? (
								<text class="f-sub" x={BW / 2} y={31} text-anchor="middle">
									{clip(l.sub, 21)}
								</text>
							) : null}
						</g>
					);
				})}
			</svg>
		</div>
	);
}

function RouteCard({
	r,
	map,
	nameOf,
	session,
	t,
}: {
	r: Route;
	map: RouteMap;
	nameOf: (id: string) => string;
	session: boolean;
	t: T;
}) {
	const title =
		r.kind === 'exit'
			? r.group
			: r.kind === 'tunnel'
				? t('fails.route.tunnel', { group: r.group })
				: r.kind === 'direct'
					? t('fails.route.direct')
					: t('fails.route.other');
	const edges = r.path.map((id) => map.edges.find((e) => e.id === id)).filter((e): e is GEdge => !!e);
	const rules = [...r.rules.slice(0, 8), ...(r.rules.length > 8 ? [`+${r.rules.length - 8}`] : [])];

	let verdict: { text: string; cls: string };
	if (r.closed) verdict = { text: t('fails.route.closed'), cls: 'warn' };
	else if (r.broken) {
		const e = edges.find((x) => x.id === r.broken);
		verdict = { text: t('fails.route.broken', { from: e ? nameOf(e.from) : '', to: e ? nameOf(e.to) : '' }), cls: 'bad' };
	} else if (edges.length === 0) verdict = { text: t('fails.route.nopath'), cls: '' };
	else if (r.kind === 'direct') verdict = { text: t('fails.route.home'), cls: '' };
	else if (edges.every((e) => e.state === 'ok')) verdict = { text: t('fails.route.fine'), cls: 'ok' };
	else verdict = { text: t('fails.route.unknown'), cls: '' };

	// Отказы есть, а красного звена нет: что это значит, зависит от того,
	// проверялся ли путь вообще.
	let why: Key | null = null;
	if (r.failed.length > 0 && !r.broken && !r.closed) {
		if (r.kind === 'direct') why = 'fails.req.direct';
		else if (edges.length > 0 && edges.every((e) => e.state === 'ok')) why = 'fails.req.site';
		else why = 'fails.req.nolink';
	}

	return (
		<div class={`f-route${r.failed.length > 0 || r.broken ? ' bad' : ''}`} data-route={r.id}>
			<div class="f-rhead">
				<span class="f-rname">{title}</span>
				<span class={`f-verdict ${verdict.cls}`}>{verdict.text}</span>
			</div>
			{edges.length > 0 ? (
				<div class="f-path">
					<span>{edges[0] ? nameOf(edges[0].from) : ''}</span>
					{edges.map((e) => (
						<span key={e.id} class="f-seg">
							<i class={`f-link ${e.state}`} title={t(`fails.state.${e.state}` as Key)} aria-label={t(`fails.state.${e.state}` as Key)} />
							<span>{nameOf(e.to)}</span>
						</span>
					))}
				</div>
			) : null}
			{rules.length > 0 || r.tail ? (
				<div class="f-rules">
					{rules.map((x) => (
						<code key={x}>{x}</code>
					))}
					{r.tail ? <code class="tail">{t('fails.route.tail')}</code> : null}
				</div>
			) : null}
			{session ? (
				<div class="f-req">
					{t('fails.req', { ok: r.ok.length, bad: r.failed.length })}
					{why ? <span class="f-why"> · {t(why)}</span> : null}
				</div>
			) : null}
			{r.failed.length > 0 ? (
				<ul class="f-fails">
					{r.failed.slice(0, 12).map((x) => (
						<FailRow key={x.key} x={x} t={t} />
					))}
					{r.failed.length > 12 ? <li class="more">{t('fails.req.more', { n: r.failed.length - 12 })}</li> : null}
				</ul>
			) : null}
		</div>
	);
}

function FailRow({ x, t }: { x: WatchTarget; t: T }) {
	return (
		<li>
			<span class="f-host">{x.name}</span>
			<span class="f-net">{x.net}</span>
			<span class="f-cnt">{x.dial_errors > 0 ? t('fails.req.dials', { n: x.dial_errors }) : t(`fails.verdict.${x.verdict}` as Key)}</span>
			{x.last_error ? <span class="f-err">{x.last_error}</span> : null}
		</li>
	);
}
