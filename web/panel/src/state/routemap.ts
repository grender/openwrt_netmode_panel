import type {
	ExitGroup,
	HopsResponse,
	NikkiExitsResponse,
	PathProbe,
	PathTestResponse,
	ProxiesResponse,
	RulesetsResponse,
	WatchTarget,
} from '../api/types';

/**
 * Карта путей для экрана «Детектор отказов» (ADR-0051).
 *
 * Чистые функции: из того, что панель уже знает (наборы, группы выходов,
 * пул хопов, узлы туннеля), собирается граф «клиент → роутер → серверы
 * подписки → свои ноды → выходы → интернет»; замер звеньев
 * (POST /api/nikki/pathtest) красит рёбра, а запросы устройства из
 * наблюдателя ложатся на маршруты.
 *
 * Диагноз ставится ЗДЕСЬ, а не в демоне: демон отдаёт замеры, а вывод
 * «resi не ответил при живом dc той же ноды — значит, отказ за нодой»
 * нужен только этому экрану.
 */

const CHAIN_SEP = ' ⇢ ';
/** Выход с IP самого VPS: его проба и есть проба дороги до ноды. */
const MEASURED_EXIT = 'dc';

export type LinkState = 'ok' | 'fail' | 'unknown';
export type Col = 'client' | 'router' | 'sub' | 'node' | 'exit' | 'net';

export interface GNode {
	id: string;
	col: Col;
	label: string;
	/** Вторая строка: роль узла. Ключ словаря или готовый текст — решает экран. */
	role: 'client' | 'router' | 'hop' | 'tunnel' | 'hop+tunnel' | 'node' | 'isp' | 'vps' | 'subip' | 'home' | 'net';
}

export interface GEdge {
	id: string;
	from: string;
	to: string;
	state: LinkState;
	ms: number | null;
	/** По ребру сейчас идёт трафик какого-то маршрута. */
	used: boolean;
	/** Отказавших запросов устройства, приписанных этому звену. */
	fails: number;
}

export interface Route {
	id: string;
	kind: 'exit' | 'tunnel' | 'direct' | 'other';
	/** Группа движка: EXIT-KZ-RESI, BYPASS; пусто у «напрямую». */
	group: string;
	/** Что сюда направлено: наборы и свои правила. */
	rules: string[];
	/** Хвост правил («всё остальное») ведёт сюда. */
	tail: boolean;
	/** Группа выхода закрыта руками (REJECT) или живого пути нет. */
	closed: boolean;
	/** Рёбра текущего пути по порядку от роутера. Пусто — путь не определён. */
	path: string[];
	/** Первое красное ребро пути — где рвётся связь. */
	broken: string | null;
	ok: WatchTarget[];
	failed: WatchTarget[];
}

export interface RouteMap {
	nodes: GNode[];
	edges: GEdge[];
	routes: Route[];
	/** Замер звеньев был — иначе рёбра серые не потому, что всё плохо. */
	tested: boolean;
}

export interface MapInput {
	rulesets: RulesetsResponse | null | undefined;
	exits: NikkiExitsResponse | null | undefined;
	hops: HopsResponse | null | undefined;
	proxies: ProxiesResponse | null | undefined;
	test: PathTestResponse | null;
	/** Устройство под наблюдением: подпись и его запросы. */
	client: { label: string; targets: WatchTarget[] } | null;
}

const ROUTER = 'router';
const NET = 'net';
const HOME = 'home';
/** Выход туннеля: сайт видит IP самого сервера подписки. */
const TUNEXIT = 'exit:tunnel';
const subID = (name: string) => `sub:${name}`;
const nodeID = (name: string) => `node:${name}`;
const exitID = (node: string, exit: string) => `exit:${node}:${exit}`;
const edgeID = (from: string, to: string) => `${from}>${to}`;

const stateOf = (p: PathProbe | null | undefined): LinkState =>
	p?.state === 'ok' ? 'ok' : p?.state === 'fail' ? 'fail' : 'unknown';

/** «EXIT-KZ-RESI» → { cc: 'kz', exit: 'resi' }; «EXIT-RESI» → { cc: '', exit: 'resi' }. */
function parseExitGroup(name: string): { cc: string; exit: string } {
	const tail = name.replace(/^EXIT-/, '').toLowerCase();
	const i = tail.lastIndexOf('-');
	return i < 0 ? { cc: '', exit: tail } : { cc: tail.slice(0, i), exit: tail.slice(i + 1) };
}

/** Отказ запроса: вердикт не «ок» или были неудавшиеся дозвоны. */
export const isFailed = (t: WatchTarget): boolean => t.verdict !== 'ok' || t.dial_errors > 0;

/** «группа[узел]» → группа и узел; «DIRECT» → группа DIRECT без узла. */
export function parseChain(chain: string): { group: string; node: string } {
	const m = /^(.*)\[(.*)\]$/.exec(chain);
	return m ? { group: m[1] ?? '', node: m[2] ?? '' } : { group: chain, node: '' };
}

export function buildRouteMap(inp: MapInput): RouteMap {
	const nodes: GNode[] = [];
	const edges = new Map<string, GEdge>();
	const test = inp.test;
	const hopsInfo = inp.hops ?? null;
	const pool = hopsInfo?.pool ?? [];
	const exitNodes = hopsInfo?.exits ?? [];
	const follow = hopsInfo?.follow ?? {};
	const tunnelNow = tunnelNodeOf(inp.proxies);

	const edge = (from: string, to: string, state: LinkState, ms: number | null = null): GEdge => {
		const id = edgeID(from, to);
		let e = edges.get(id);
		if (!e) {
			e = { id, from, to, state, ms, used: false, fails: 0 };
			edges.set(id, e);
		}
		return e;
	};

	// ── клиент и роутер ──
	nodes.push({ id: ROUTER, col: 'router', label: '', role: 'router' });
	if (inp.client) {
		nodes.push({ id: 'client', col: 'client', label: inp.client.label, role: 'client' });
		// Устройство видно наблюдателю — значит, до роутера оно дошло.
		edge('client', ROUTER, 'ok').used = true;
	}
	nodes.push({ id: NET, col: 'net', label: '', role: 'net' });

	// ── серверы подписки: хопы пула и узел туннеля ──
	// Узел туннеля — первым: его путь идёт по верхнему краю схемы, мимо своих нод.
	const subs: string[] = tunnelNow ? [tunnelNow, ...pool.filter((h) => h !== tunnelNow)] : [...pool];
	const passive = new Map((inp.proxies?.members ?? []).map((m) => [m.name, m]));
	const subState = new Map<string, LinkState>();
	for (const name of subs) {
		const isHop = pool.includes(name);
		const isTun = name === tunnelNow;
		nodes.push({ id: subID(name), col: 'sub', label: name, role: isHop && isTun ? 'hop+tunnel' : isHop ? 'hop' : 'tunnel' });
		const probe = test?.hops.find((h) => h.name === name);
		const pm = passive.get(name);
		// До замера — то, что движок намерил сам (health-check провайдера).
		const st: LinkState = probe ? stateOf(probe) : pm ? (pm.alive ? 'ok' : 'fail') : 'unknown';
		const ms = probe ? probe.delay_ms : (pm?.delay_ms ?? null);
		subState.set(name, st);
		edge(ROUTER, subID(name), st, ms);
		if (isTun) {
			nodes.push({ id: TUNEXIT, col: 'exit', label: name, role: 'subip' });
			edge(subID(name), TUNEXIT, st === 'ok' ? 'ok' : 'unknown');
			edge(TUNEXIT, NET, st === 'ok' ? 'ok' : 'unknown');
		}
	}

	// ── свои ноды и их выходы ──
	for (const n of exitNodes) {
		const res = test?.nodes.find((x) => x.name === n.name);
		nodes.push({ id: nodeID(n.name), col: 'node', label: n.name, role: 'node' });

		const direct = stateOf(res?.direct);
		edge(ROUTER, nodeID(n.name), direct, res?.direct?.delay_ms ?? null);

		let reachable = direct === 'ok';
		for (const h of pool) {
			const c = res?.chains.find((x) => x.hop === h);
			// Цепочка не ответила при мёртвом хопе — про звено «хоп → нода»
			// мы ничего не узнали: виноват хоп, и он уже красный.
			let st = stateOf(c);
			if (st === 'fail' && subState.get(h) !== 'ok') st = 'unknown';
			if (st === 'ok') reachable = true;
			edge(subID(h), nodeID(n.name), st, c?.delay_ms ?? null);
		}

		for (const ex of n.exits) {
			const id = exitID(n.name, ex);
			nodes.push({ id, col: 'exit', label: n.name, role: ex === MEASURED_EXIT ? 'vps' : 'isp' });
			let st: LinkState = 'unknown';
			let ms: number | null = null;
			if (ex === MEASURED_EXIT) {
				// Выход dc — сама нода: дошли до неё — вышли с её IP.
				st = reachable ? 'ok' : 'unknown';
			} else {
				const p = res?.exits.find((x) => x.exit === ex);
				st = stateOf(p);
				ms = p?.delay_ms ?? null;
				// Выход не ответил, а до ноды и не дошли — про звено за нодой
				// неизвестно ничего.
				if (st === 'fail' && !reachable) st = 'unknown';
			}
			edge(nodeID(n.name), id, st, ms);
			edge(id, NET, st === 'ok' ? 'ok' : 'unknown');
		}
	}

	// ── напрямую, с домашнего IP ──
	nodes.push({ id: HOME, col: 'exit', label: '', role: 'home' });
	edge(ROUTER, HOME, 'unknown');
	edge(HOME, NET, 'unknown');

	// ── маршруты ──
	const routes: Route[] = [];
	const rs = inp.rulesets ?? null;
	const tunnelGroup = rs?.tunnel_group || 'BYPASS';
	const byAction = (action: string, group?: string): string[] => {
		if (!rs) return [];
		const out: string[] = [];
		for (const r of rs.rules) if (r.action === action && (!group || r.group === group)) out.push(r.value);
		for (const s of rs.sets) if (s.action === action && (!group || s.group === group)) out.push(s.name);
		return out;
	};

	const groupNames = new Set<string>();
	for (const g of inp.exits?.groups ?? []) groupNames.add(g.name);
	for (const g of rs?.exit_groups ?? []) if (isTopExitGroup(g, rs?.exit_groups ?? [])) groupNames.add(g.name);

	for (const name of [...groupNames].sort()) {
		const g = inp.exits?.groups.find((x) => x.name === name);
		const { cc, exit } = parseExitGroup(name);
		const mine = exitNodes.filter((n) => (!cc || n.country === cc) && n.exits.includes(exit));
		const route: Route = {
			id: name, kind: 'exit', group: name, rules: byAction('exit', name), tail: false,
			closed: false, path: [], broken: null, ok: [], failed: [],
		};
		const now = g?.members.find((m) => m.name === g.now);
		if (!now || now.kind === 'reject') {
			route.closed = !!g;
		} else if (mine.length === 1 && mine[0]) {
			// Несколько нод под одной группой: какая выбрана, панель не знает —
			// путь не рисуем, чем рисовать догадку.
			const n = mine[0];
			let hop = '';
			if (now.kind === 'chains') {
				const i = now.now ? now.now.indexOf(CHAIN_SEP) : -1;
				hop = i >= 0 && now.now ? now.now.slice(i + CHAIN_SEP.length) : '';
			} else if (now.kind === 'chain') {
				hop = follow[n.name] ?? '';
			}
			if (now.kind !== 'direct' && (!hop || hop === 'REJECT' || !pool.includes(hop))) {
				route.closed = true;
			} else {
				const first = hop
					? [edgeID(ROUTER, subID(hop)), edgeID(subID(hop), nodeID(n.name))]
					: [edgeID(ROUTER, nodeID(n.name))];
				route.path = [...first, edgeID(nodeID(n.name), exitID(n.name, exit)), edgeID(exitID(n.name, exit), NET)];
			}
		}
		routes.push(route);
	}

	const tunnel: Route = {
		id: 'tunnel', kind: 'tunnel', group: tunnelGroup, rules: byAction('tunnel'), tail: rs?.policy === 'tunnel',
		closed: !tunnelNow,
		path: tunnelNow ? [edgeID(ROUTER, subID(tunnelNow)), edgeID(subID(tunnelNow), TUNEXIT), edgeID(TUNEXIT, NET)] : [],
		broken: null, ok: [], failed: [],
	};
	const direct: Route = {
		id: 'direct', kind: 'direct', group: '', rules: byAction('direct'), tail: rs?.policy === 'direct',
		closed: false, path: [edgeID(ROUTER, HOME), edgeID(HOME, NET)], broken: null, ok: [], failed: [],
	};
	const other: Route = {
		id: 'other', kind: 'other', group: '', rules: [], tail: false, closed: false, path: [], broken: null, ok: [], failed: [],
	};
	routes.push(tunnel, direct);

	// ── запросы устройства → маршруты ──
	for (const t of inp.client?.targets ?? []) {
		const bad = isFailed(t);
		// У отказавшего дозвона цепочка — та, на которой он отказал.
		const { group } = parseChain((bad && t.error_chain) || t.chain || t.error_chain);
		const r =
			routes.find((x) => x.kind === 'exit' && x.group === group) ??
			(group === 'DIRECT' ? direct : group === tunnelGroup || group === 'PROXY' || group === 'AUTO' ? tunnel : other);
		(bad ? r.failed : r.ok).push(t);
	}
	if (other.ok.length + other.failed.length > 0) routes.push(other);

	// ── где рвётся и что занято ──
	for (const r of routes) {
		for (const id of r.path) {
			const e = edges.get(id);
			if (!e) continue;
			e.used = true;
			if (!r.broken && e.state === 'fail') r.broken = id;
		}
		if (r.failed.length > 0) {
			// Отказы ложатся на красное звено; нет красного — на последнее
			// непроверенное: дальше него про путь ничего не известно.
			const at = r.broken ?? r.path.find((id) => edges.get(id)?.state === 'unknown') ?? r.path[r.path.length - 1];
			const e = at ? edges.get(at) : undefined;
			if (e) e.fails += r.failed.length;
		}
	}

	return { nodes, edges: [...edges.values()], routes, tested: !!test };
}

/** Верхняя группа выхода: не «…-DIRECT» и не «…-HOP» при живой родительской. */
function isTopExitGroup(g: ExitGroup, all: ExitGroup[]): boolean {
	const m = /^(.*)-(DIRECT|HOP)$/.exec(g.name);
	return !m || !all.some((x) => x.name === m[1]);
}

/** Узел подписки, через который сейчас идёт туннель. Пусто — туннель не на узле. */
function tunnelNodeOf(p: ProxiesResponse | null | undefined): string {
	if (!p || !p.available) return '';
	const m = p.members.find((x) => x.name === p.selected);
	return m && m.kind === 'node' ? m.name : '';
}
