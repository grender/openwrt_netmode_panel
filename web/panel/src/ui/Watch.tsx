import { useEffect, useMemo, useRef, useState } from 'preact/hooks';
import type {
	CustomRule,
	Mode,
	RulesDraft,
	RulesetsResponse,
	WatchHost,
	WatchHosts,
	WatchState,
	WatchSummary,
	WatchTarget,
	WatchVerdict,
} from '../api/types';
import type { Key, Lang, T } from '../i18n';
import type { Side } from '../state/side';
import type { Lock } from '../state/lock';
import { Confirm, Spin } from './bits';
import { draftOf, MAX_COMMENT, MAX_RULES, plural } from './Rulesets';

/**
 * Наблюдатель трафика устройства — третий экран панели (ADR-0043).
 *
 * Отвечает на вопрос, на который не отвечает больше ничто: КАКИЕ имена
 * просит устройство и какие из них не работают. Добавить своё правило легко
 * и без него; трудно узнать, какое именно правило нужно.
 *
 * Экран — чистое представление, как Rulesets: черновик правил и его
 * применение живут в App, и черновик здесь ТОТ ЖЕ, что в настройках. Второй
 * путь записи в mixin.yaml не заводится (ADR-0040, дубли).
 */

// ─── подпись порта ───

/**
 * Подпись порта живёт в коде панели, а не в словаре (ADR-0043).
 *
 * Это не перевод, а факт протокола: QUIC остаётся QUIC на любом языке, и
 * положив его в ru.json, мы получили бы ключ, который переводчику нечего
 * делать, а пропуск которого гасил бы подпись целиком.
 */
const PORT_SIG: Record<string, string> = {
	'443/tcp': 'TLS',
	'443/udp': 'QUIC',
	'80/tcp': 'HTTP',
	'53/tcp': 'DNS',
	'53/udp': 'DNS',
	'853/tcp': 'DoT',
	'5223/tcp': 'APNs',
	'5222/tcp': 'XMPP',
	'5228/tcp': 'XMPP',
	'3478/udp': 'STUN',
	'19302/udp': 'STUN',
	'51820/udp': 'WireGuard',
	'123/udp': 'NTP',
};

function portSig(port: number, net: string): string {
	return PORT_SIG[`${port}/${net}`] ?? `${port}/${net}`;
}

// ─── числа ───

/**
 * Байты словами. Разделитель дробной части берётся из языка: у русского
 * запятая, и «1.9 КБ» в русской панели выглядит опечаткой.
 */
export function fmtBytes(b: number, t: T, lang: Lang): string {
	const dot = (s: string) => (lang === 'ru' ? s.replace('.', ',') : s);
	if (b <= 0) return '0';
	if (b < 1024) return `${b} ${t('watch.unit.b')}`;
	if (b < 1048576) return `${dot((b / 1024).toFixed(1))} ${t('watch.unit.kb')}`;
	if (b < 1073741824) return `${dot((b / 1048576).toFixed(b < 10485760 ? 1 : 0))} ${t('watch.unit.mb')}`;
	return `${dot((b / 1073741824).toFixed(2))} ${t('watch.unit.gb')}`;
}

const fmtRate = (b: number, t: T, lang: Lang) => (b <= 0 ? '' : `${fmtBytes(b, t, lang)}${t('watch.unit.persec')}`);

/** Сколько минут идёт наблюдение. Считается от ЛОКАЛЬНОГО момента. */
function minutesSince(iso: string | undefined): number {
	if (!iso) return 0;
	const at = new Date(iso).getTime();
	if (!Number.isFinite(at)) return 0;
	return Math.max(0, Math.floor((Date.now() - at) / 60000));
}

// ─── свои правила ───

const V4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/;

/** Первый адрес подсети. */
export function net4(addr: string, bits: number): string {
	const m = V4.exec(addr);
	if (!m) return addr;
	const o = m.slice(1).map(Number) as [number, number, number, number];
	const num = ((o[0] << 24) | (o[1] << 16) | (o[2] << 8) | o[3]) >>> 0;
	const mask = bits === 0 ? 0 : (0xffffffff << (32 - bits)) >>> 0;
	const net = (num & mask) >>> 0;
	return [(net >>> 24) & 255, (net >>> 16) & 255, (net >>> 8) & 255, net & 255].join('.');
}

/**
 * Маски, которые имеют смысл владельцу: сам адрес, его /24 и /16.
 * Шире — это уже «полстраны в туннель», и такое правило пишут руками.
 */
const MASKS = [32, 24, 16] as const;

function inCIDR(ip: string, value: string): boolean {
	const cut = value.indexOf('/');
	if (cut < 0) return false;
	const bits = Number(value.slice(cut + 1));
	if (!Number.isInteger(bits) || bits < 0 || bits > 32) return false;
	if (!V4.test(ip)) return false;
	return net4(ip, bits) === net4(value.slice(0, cut), bits);
}

export interface Covered {
	rule: CustomRule;
	/** applied — правило уже работает; draft — лежит в черновике. */
	where: 'applied' | 'draft';
}

/**
 * Покрыт ли адресат своим правилом.
 *
 * Суффикс покрывает поддомены, подсеть — адрес: правило 5.61.0.0/16
 * покрывает 5.61.44.9, и второе правило на тот же адрес было бы
 * недостижимо (ADR-0040).
 */
export function coveredBy(
	name: string,
	isIP: boolean,
	rules: CustomRule[],
	applied: CustomRule[],
): Covered | null {
	const tok = (r: CustomRule) => `${r.kind}:${r.value}`;
	const live = new Set(applied.map(tok));
	for (const r of rules) {
		const hit =
			r.kind === 'cidr'
				? isIP && inCIDR(name, r.value)
				: r.kind === 'domain'
					? name === r.value
					: name === r.value || name.endsWith(`.${r.value}`);
		if (hit) return { rule: r, where: live.has(tok(r)) ? 'applied' : 'draft' };
	}
	return null;
}

// ─── таблица ───

type ColId = 'verdict' | 'verdictErr' | 'route' | 'sess' | 'rate' | 'conns' | 'live' | 'times' | 'dst' | 'port' | 'cc';
type ModeId = 'route' | 'load' | 'broken' | 'tech' | 'all';

const COLS: Record<ColId, { w: string; head: Key }> = {
	verdict: { w: '132px', head: 'watch.col.verdict' },
	verdictErr: { w: 'minmax(180px,1.1fr)', head: 'watch.col.verdictErr' },
	route: { w: 'minmax(170px,1.1fr)', head: 'watch.col.route' },
	sess: { w: '148px', head: 'watch.col.sess' },
	rate: { w: '116px', head: 'watch.col.rate' },
	conns: { w: '104px', head: 'watch.col.conns' },
	live: { w: '104px', head: 'watch.col.live' },
	times: { w: '112px', head: 'watch.col.times' },
	dst: { w: '136px', head: 'watch.col.dst' },
	port: { w: '134px', head: 'watch.col.port' },
	cc: { w: '58px', head: 'watch.col.cc' },
};

/**
 * Пять наборов колонок вместо одной таблицы.
 *
 * У активного устройства 50–150 строк, и два сценария владельца смотрят на
 * разные поля: «что сломано» — на вердикт и ошибку, «с кем говорит новое
 * устройство» — на трафик и правило. Одна таблица со всеми колонками сразу
 * не влезает даже на 1440, а на 390 не влезает вовсе.
 *
 * Кнопок правила в таблице нет НИ В ОДНОМ режиме: правило пишется в
 * раскрытой строке, где видно, на что именно оно встанет.
 */
const MODES: { id: ModeId; cols: ColId[]; only?: boolean; wide?: boolean }[] = [
	{ id: 'route', cols: ['verdict', 'route', 'times'] },
	{ id: 'load', cols: ['sess', 'rate', 'conns'] },
	{ id: 'broken', cols: ['verdictErr', 'live', 'times'], only: true },
	{ id: 'tech', cols: ['dst', 'port', 'cc'] },
	{ id: 'all', cols: ['verdictErr', 'port', 'route', 'sess', 'rate', 'conns', 'times', 'dst', 'cc'], wide: true },
];

/**
 * Вердикт различается ЗНАЧКОМ и СЛОВОМ, а не только цветом: рядом «молчит»
 * и «не отвечает» обязаны читаться без цвета.
 */
const V_ICON: Record<WatchVerdict, string> = { ok: '✓', silent: '◌', unreachable: '⊘' };
const V_CLS: Record<WatchVerdict, string> = { ok: 'v-ok', silent: 'v-mute', unreachable: 'v-dead' };

/** Порядок «проблемы первыми»: сломанное, потом ушедшее мимо правил. */
function sortKey(x: WatchTarget): number {
	if (x.verdict === 'unreachable') return 0;
	if (x.verdict === 'silent') return 1;
	return x.chain === 'DIRECT' && x.rule === 'Match' ? 2 : 3;
}

const bytes = (x: WatchTarget) => x.up + x.down;

/** Ушло ли в туннель. У прямой цепочки звено ровно одно и называется DIRECT. */
const toTunnel = (x: WatchTarget) => x.chain !== '' && x.chain !== 'DIRECT';

// ─── сводка для полки главной ───

/**
 * Строка полки называет СОСТОЯНИЕ, а не раздел: «выключен» — это тоже
 * состояние, и оно честнее, чем «перейти к наблюдателю».
 */
export function watchSummary(s: WatchSummary | null, t: T): { text: string; warn: boolean } {
	if (!s) return { text: t('watch.shelf.off'), warn: false };
	const parts = [s.ip, t('watch.shelf.min', { n: minutesSince(s.since) })];
	if (s.engine !== 'running') parts.push(t(`watch.engine.${s.engine}` as Key));
	if (s.problems > 0) parts.push(t('watch.shelf.bad', { n: s.problems }));
	return { text: parts.filter(Boolean).join(' · '), warn: s.problems > 0 || s.engine !== 'running' };
}

// ─── экран ───

export interface WatchProps {
	state: WatchState | undefined;
	stale: boolean;
	hosts: Side<WatchHosts>;
	mode: Mode | 'unknown';
	applied: Side<RulesetsResponse>;
	draft: RulesDraft | null;
	setDraft: (d: RulesDraft) => void;
	onStart: (ip: string) => void;
	onStop: () => void;
	onApplyRules: (d: RulesDraft) => void;
	lock: Lock;
	locked: boolean;
	t: T;
	lang: Lang;
}

type Leave = 'stop' | 'change' | null;

export function Watch(p: WatchProps) {
	const { t, lang, state, applied } = p;
	const [mode, setMode] = useState<ModeId>('route');
	const [sort, setSort] = useState<'problems' | 'traffic'>('problems');
	const [query, setQuery] = useState('');
	const [open, setOpen] = useState('');
	const [form, setForm] = useState<{ key: string; to: 'tunnel' | 'direct'; cut: number | null; mask: number; comment: string } | null>(null);
	const [draftOpen, setDraftOpen] = useState(false);
	const [leave, setLeave] = useState<Leave>(null);
	/**
	 * Порядок ЗАМОРОЖЕН: строки не переставляются под пальцем, пока владелец
	 * целится в кнопку. Живой сортировкой список переупорядочивался бы
	 * каждую секунду — ровно в тот момент, когда по нему нажимают.
	 */
	const [order, setOrder] = useState<string[] | null>(null);
	const [freshOpen, setFreshOpen] = useState(false);

	const eff = p.draft ?? draftOf(applied);
	const appliedRules = applied?.rules ?? [];
	const targets = useMemo(() => state?.targets ?? [], [state]);
	const engine = state?.engine ?? 'running';
	const restarting = engine === 'restarting';

	// Новая сессия — новый порядок: строки прежнего устройства к нему
	// отношения не имеют.
	const ip = state?.active ? state.ip : undefined;
	const prevIP = useRef<string | undefined>(undefined);
	useEffect(() => {
		if (prevIP.current !== ip) {
			prevIP.current = ip;
			setOrder(null);
			setOpen('');
			setForm(null);
		}
	}, [ip]);

	const M = MODES.find((m) => m.id === mode) ?? MODES[0]!;
	const byKey = useMemo(() => new Map(targets.map((x) => [x.key, x])), [targets]);

	const sorted = (list: WatchTarget[]) =>
		list.slice().sort((a, b) => {
			if (sort === 'traffic') return bytes(b) - bytes(a);
			const k = sortKey(a) - sortKey(b);
			return k !== 0 ? k : bytes(b) - bytes(a);
		});

	// Порядок берётся из замороженного списка; всё, что появилось позже,
	// ждёт в своей полосе и общий список не двигает.
	const frozen = order ?? sorted(targets).map((x) => x.key);
	if (order === null && targets.length > 0) {
		// Первый кадр задаёт порядок один раз, дальше он живёт в состоянии.
		queueMicrotask(() => setOrder((o) => o ?? sorted(targets).map((x) => x.key)));
	}
	const inOrder = new Set(frozen);
	const pending = targets.filter((x) => !inOrder.has(x.key));

	const q = query.trim().toLowerCase();
	const visible = (list: WatchTarget[]) =>
		list.filter((x) => (!M.only || x.verdict !== 'ok') && (!q || x.name.toLowerCase().includes(q)));

	const rows = visible(frozen.map((k) => byKey.get(k)).filter((x): x is WatchTarget => !!x));
	const fresh = visible(sorted(pending));
	const shown = rows.length + fresh.length;
	const total = targets.filter((x) => !M.only || x.verdict !== 'ok').length;

	const merge = () => {
		setOrder(sorted(targets).map((x) => x.key));
		setFreshOpen(false);
	};
	const resort = (s: 'problems' | 'traffic') => {
		// Смена порядка — явное действие владельца: тут переставлять можно.
		setSort(s);
		setOrder(
			targets
				.slice()
				.sort((a, b) => {
					if (s === 'traffic') return bytes(b) - bytes(a);
					const k = sortKey(a) - sortKey(b);
					return k !== 0 ? k : bytes(b) - bytes(a);
				})
				.map((x) => x.key),
		);
		setOpen('');
		setForm(null);
	};

	const dirtyRules = eff.rules.length - appliedRules.length;
	const pendingRules = eff.rules.filter(
		(r) => !appliedRules.some((a) => a.kind === r.kind && a.value === r.value && a.action === r.action),
	);

	const stopOrAsk = (what: Exclude<Leave, null>) => {
		if (pendingRules.length > 0) {
			setLeave(what);
			return;
		}
		p.onStop();
	};

	// ── наблюдать нечего ──
	if (p.mode !== 'nikki') {
		return (
			<div class="watch" data-part="watch">
				<div class="note warn full" data-part="watch-off">
					<h3>{t('watch.off.title')}</h3>
					<p>{t('watch.off.text')}</p>
				</div>
				<a class="linkbtn" href="#engine">
					{t('watch.off.cta')}
				</a>
			</div>
		);
	}

	// ── выбор устройства ──
	if (!state?.active) {
		return (
			<div class="watch" data-part="watch">
				<p class="hint">{t('watch.pick.hint')}</p>
				<HostList hosts={p.hosts} onStart={p.onStart} lock={p.lock} locked={p.locked} t={t} />
				<p class="hint">{t('watch.pick.note')}</p>
			</div>
		);
	}

	const grid = `minmax(150px,1fr) ${M.cols.map((c) => COLS[c].w).join(' ')} 28px`;

	return (
		<div class="watch" data-part="watch">
			<div class="w-sess" data-part="watch-session" data-anchor="watch-session">
				<span class="w-ip">{state.ip}</span>
				<span class="w-meta">{t('watch.sess.age', { at: hhmm(state.since ?? ''), n: minutesSince(state.since) })}</span>
				<span class={`w-eng w-eng-${engine}`}>
					<i class="dot" aria-hidden="true" />
					{t(`watch.engine.${engine}` as Key)}
				</span>
				<span class="w-acts">
					<button type="button" class="mini" onClick={() => stopOrAsk('change')}>
						{t('watch.sess.change')}
					</button>
					<button type="button" class="mini" onClick={() => stopOrAsk('stop')}>
						{t('watch.sess.stop')}
					</button>
				</span>
			</div>

			{restarting ? (
				<div class="note warn full" data-part="watch-restart">
					<h3>
						<Spin /> {t('watch.restart.title')}
					</h3>
					<p>{t('watch.restart.text')}</p>
				</div>
			) : null}

			{engine === 'off' ? (
				<div class="note warn full" data-part="watch-engine-off">
					<h3>{t('watch.gone.title')}</h3>
					<p>{t('watch.gone.text')}</p>
				</div>
			) : null}

			{state.unparsed > 0 ? (
				<div class="note warn full" data-part="watch-unparsed">
					<h3>{t('watch.unparsed.title')}</h3>
					<p>{t('watch.unparsed.text', { n: state.unparsed })}</p>
				</div>
			) : null}

			{p.stale ? <p class="hint bad">{t('watch.stale')}</p> : null}

			<div class="w-ctl" data-part="watch-controls">
				<div class="seg w-modes">
					{MODES.map((m) => (
						<button
							key={m.id}
							type="button"
							class="accent"
							aria-pressed={m.id === mode}
							onClick={() => {
								setMode(m.id);
								setOpen('');
								setForm(null);
							}}
						>
							{t(`watch.mode.${m.id}` as Key)}
						</button>
					))}
				</div>
				<div class="seg w-sort">
					{(['problems', 'traffic'] as const).map((s) => (
						<button key={s} type="button" aria-pressed={s === sort} onClick={() => resort(s)}>
							{t(`watch.sort.${s}` as Key)}
						</button>
					))}
				</div>
				<input
					class="w-find"
					value={query}
					placeholder={t('watch.find')}
					aria-label={t('watch.find')}
					autocomplete="off"
					spellcheck={false}
					onInput={(e) => setQuery((e.target as HTMLInputElement).value)}
				/>
			</div>

			{/* Полоса новых стоит ВСЕГДА, даже пустая: так таблица под ней не
			    прыгает в момент, когда адресат появляется. */}
			<div class={`w-fresh${pending.length ? ' has' : ''}`} data-part="watch-fresh">
				<button
					type="button"
					class="w-fresh-head"
					aria-expanded={freshOpen}
					disabled={pending.length === 0}
					onClick={() => setFreshOpen(!freshOpen)}
				>
					<i class="dot" aria-hidden="true" />
					<span>
						{pending.length
							? t('watch.fresh.some', {
									n: pending.length,
									w: plural(pending.length, lang, 'watch.fresh.one', 'watch.fresh.few', 'watch.fresh.many', t),
								})
							: t('watch.fresh.none')}
					</span>
					{pending.length ? <span class="chev">{freshOpen ? t('watch.fresh.hide') : t('watch.fresh.show')}</span> : null}
				</button>
				{pending.length ? (
					<button type="button" class="mini" onClick={merge}>
						{t('watch.fresh.merge')}
					</button>
				) : null}
			</div>

			{freshOpen && fresh.length > 0 ? (
				<div class="w-table fresh" style={{ '--w-grid': grid }}>
					{fresh.map((x) => (
						<Row key={x.key} x={x} {...rowProps()} />
					))}
				</div>
			) : null}

			<div class={`w-table${M.wide ? ' wide' : ''}`} style={{ '--w-grid': grid }} data-part="watch-table">
				<div class="w-head" aria-hidden="true">
					<span>{t('watch.col.name')}</span>
					{M.cols.map((c) => (
						<span key={c}>{t(COLS[c].head)}</span>
					))}
					<span />
				</div>
				{rows.map((x) => (
					<Row key={x.key} x={x} {...rowProps()} />
				))}
			</div>

			{shown === 0 ? (
				<div class="empty" data-part="watch-empty">
					<b>{quietTitle()}</b>
					<p>{quietText()}</p>
				</div>
			) : null}

			<p class="hint">
				{t('watch.count', { n: shown, m: total })}
				{q ? ` · ${t('watch.count.find', { q: query })}` : ''}
			</p>

			{pendingRules.length > 0 ? (
				<DraftBar
					rules={pendingRules}
					open={draftOpen}
					onToggle={() => setDraftOpen(!draftOpen)}
					onApply={() => p.onApplyRules(eff)}
					onDrop={() => p.setDraft({ ...eff, rules: appliedRules.map((r) => ({ ...r })) })}
					onRemove={(r) => p.setDraft({ ...eff, rules: eff.rules.filter((x) => !(x.kind === r.kind && x.value === r.value)) })}
					busy={p.lock.on('rulesets')}
					locked={p.locked}
					t={t}
					lang={lang}
				/>
			) : null}

			{leave ? (
				<Confirm
					title={t(leave === 'change' ? 'watch.leave.change.title' : 'watch.leave.stop.title')}
					text={t(leave === 'change' ? 'watch.leave.change.text' : 'watch.leave.stop.text', { n: pendingRules.length })}
					go={t('watch.leave.apply')}
					cancel={t('watch.leave.stay')}
					danger
					forAction="watch-leave"
					onGo={() => {
						p.onApplyRules(eff);
						setLeave(null);
						p.onStop();
					}}
					onCancel={() => setLeave(null)}
				/>
			) : null}
		</div>
	);

	function quietTitle(): string {
		if (q) return t('watch.empty.find.title', { q: query });
		if (M.only) return t('watch.empty.broken.title');
		return t('watch.empty.quiet.title');
	}
	function quietText(): string {
		if (q) return t('watch.empty.find.text');
		if (M.only) return t('watch.empty.broken.text');
		return t('watch.empty.quiet.text');
	}

	function rowProps() {
		return {
			cols: M.cols,
			narrowCols: (M.only
				? (['verdictErr', 'times'] as ColId[])
				: M.cols.filter((c) => c !== 'verdict' && c !== 'verdictErr')
			).slice(0, 2),
			open,
			onToggle: (k: string) => {
				setOpen(open === k ? '' : k);
				setForm(null);
			},
			form,
			setForm,
			eff,
			appliedRules,
			setDraft: p.setDraft,
			t,
			lang,
		};
	}
}

// ─── список устройств ───

function HostList({
	hosts,
	onStart,
	lock,
	locked,
	t,
}: {
	hosts: Side<WatchHosts>;
	onStart: (ip: string) => void;
	lock: Lock;
	locked: boolean;
	t: T;
}) {
	if (hosts === undefined) return <div class="rows" aria-hidden="true" data-part="skeleton" />;
	if (hosts === null) return <p class="hint bad">{t('watch.pick.down')}</p>;
	if (hosts.hosts.length === 0) return <p class="hint">{t('watch.pick.none')}</p>;
	return (
		<div class="w-devs" data-part="watch-devices">
			{hosts.hosts.map((h) => (
				<Device key={h.ip} h={h} onStart={onStart} busy={lock.on('watch', h.ip)} locked={locked} t={t} />
			))}
		</div>
	);
}

function Device({
	h,
	onStart,
	busy,
	locked,
	t,
}: {
	h: WatchHost;
	onStart: (ip: string) => void;
	busy: boolean;
	locked: boolean;
	t: T;
}) {
	return (
		<div class="w-dev">
			<span class="w-dev-name">
				{h.name || <i class="w-noname">{t('watch.dev.noname')}</i>}
				<span class="w-dev-sub">
					{h.ip} · {t(`watch.dev.${h.kind}` as Key)}
				</span>
			</span>
			<span class="w-dev-ip">{h.ip}</span>
			<span class="w-dev-kind">{t(`watch.dev.${h.kind}` as Key)}</span>
			<span class={`w-dev-talk${h.active ? ' on' : ''}`}>
				{t(h.active ? 'watch.dev.talking' : 'watch.dev.quiet')}
			</span>
			<button type="button" class="wide primary" disabled={locked} aria-busy={busy} onClick={() => onStart(h.ip)}>
				{busy ? <Spin /> : null}
				{t('watch.dev.start')}
			</button>
		</div>
	);
}

// ─── строка ───

interface RowProps {
	x: WatchTarget;
	cols: ColId[];
	narrowCols: ColId[];
	open: string;
	onToggle: (k: string) => void;
	form: { key: string; to: 'tunnel' | 'direct'; cut: number | null; mask: number; comment: string } | null;
	setForm: (f: RowProps['form']) => void;
	eff: RulesDraft;
	appliedRules: CustomRule[];
	setDraft: (d: RulesDraft) => void;
	t: T;
	lang: Lang;
}

function Row(p: RowProps) {
	const { x, t, lang } = p;
	const isOpen = p.open === x.key;
	const cov = coveredBy(x.name, x.is_ip, p.eff.rules, p.appliedRules);
	const cell = (c: ColId) => renderCell(c, x, t, lang, p.appliedRules);

	return (
		<div class={`w-row${isOpen ? ' open' : ''}${x.new ? ' fresh' : ''}`} data-part="watch-row">
			<button type="button" class="w-name" onClick={() => p.onToggle(x.key)} aria-expanded={isOpen} title={x.name}>
				<span class="w-name-text">{x.name}</span>
				{x.new ? <span class="tag">{t('watch.tag.new')}</span> : null}
				{cov ? (
					<span class={`tag ${cov.where === 'draft' ? 'tag-draft' : 'tag-live'}`}>
						{t(cov.where === 'draft' ? 'watch.tag.draft' : 'watch.tag.rule')}
					</span>
				) : null}
				<span class={`w-vsmall ${V_CLS[x.verdict]}`}>
					{V_ICON[x.verdict]} {t(`watch.verdict.${x.verdict}` as Key)}
				</span>
			</button>

			{p.cols.map((c) => {
				const v = cell(c);
				return (
					<span key={c} class={`w-cell ${v.cls}`}>
						<span class="w-cell-main">{v.text}</span>
						{v.sub ? <span class="w-cell-sub">{v.sub}</span> : null}
					</span>
				);
			})}

			<button type="button" class="w-chev" onClick={() => p.onToggle(x.key)} aria-label={t('watch.row.details')}>
				<span aria-hidden="true">›</span>
			</button>

			<span class="w-narrow">
				{p.narrowCols.map((c) => {
					const v = cell(c);
					return (
						<span key={c} class={v.cls}>
							{v.text}
							{v.sub ? ` · ${v.sub}` : ''}
						</span>
					);
				})}
			</span>

			{isOpen ? <Detail {...p} cov={cov} /> : null}
		</div>
	);
}

function renderCell(c: ColId, x: WatchTarget, t: T, lang: Lang, applied: CustomRule[]): { text: string; sub: string; cls: string } {
	const none = { text: '', sub: '', cls: '' };
	const verdict = `${V_ICON[x.verdict]} ${t(`watch.verdict.${x.verdict}` as Key)}`;
	switch (c) {
		case 'verdict':
			return { text: verdict, sub: '', cls: V_CLS[x.verdict] };
		case 'verdictErr':
			return { text: verdict, sub: x.last_error, cls: V_CLS[x.verdict] };
		case 'route':
			return {
				text: ruleLabel(x, t, applied),
				sub: toTunnel(x) ? `→ ${t('watch.to.tunnel')} · ${nodeOf(x.chain)}` : `→ ${t('watch.to.direct')}`,
				cls: '',
			};
		case 'sess':
			return { text: `↑ ${fmtBytes(x.up, t, lang)}  ↓ ${fmtBytes(x.down, t, lang)}`, sub: '', cls: 'mono' };
		case 'rate': {
			const up = fmtRate(x.rate_up, t, lang);
			const dn = fmtRate(x.rate_down, t, lang);
			if (!up && !dn) return { text: '—', sub: '', cls: 'mono muted' };
			return { text: dn ? `↓ ${dn}` : `↑ ${up}`, sub: dn && up ? `↑ ${up}` : '', cls: 'mono' };
		}
		case 'conns':
			return {
				text: `${x.live} / ${x.count}`,
				sub: t(x.live ? 'watch.conns.live' : 'watch.conns.none'),
				cls: x.live ? 'mono' : 'mono muted',
			};
		case 'live':
			return { text: `${x.live} / ${x.count}`, sub: '', cls: x.live ? 'mono' : 'mono muted' };
		case 'times':
			return { text: `${hhmm(x.first)} → ${hhmm(x.last)}`, sub: '', cls: 'mono' };
		case 'dst':
			return { text: x.addr, sub: '', cls: 'mono' };
		case 'port':
			return { text: portsOf(x), sub: `${x.net.toUpperCase()} · ${sigOf(x)}`, cls: 'mono' };
		case 'cc':
			// Страны нет почти нигде, и это НЕ «страна неизвестна»: поля просто
			// нет, и пустая клетка — верное его изображение.
			return { text: (x.geo ?? []).join(' '), sub: '', cls: 'mono muted' };
		default:
			return none;
	}
}

const hhmm = (iso: string) => {
	const d = new Date(iso);
	return Number.isFinite(d.getTime()) ? `${pad(d.getHours())}:${pad(d.getMinutes())}` : '—';
};
const pad = (n: number) => String(n).padStart(2, '0');
const portsOf = (x: WatchTarget) => (x.ports ?? []).join(', ') || '—';
const sigOf = (x: WatchTarget) => {
	const p = (x.ports ?? [])[0];
	return p ? portSig(p, x.net) : x.net.toUpperCase();
};
/** Узел из цепочки «группа[узел]». */
const nodeOf = (chain: string) => {
	const a = chain.indexOf('[');
	return a > 0 && chain.endsWith(']') ? chain.slice(a + 1, -1) : chain;
};

/**
 * Что сработало, словами владельца.
 *
 * «Своё правило» называется своим именем, только когда оно ДЕЙСТВИТЕЛЬНО
 * есть среди применённых: у mihomo DomainSuffix бывает и в профиле, и
 * называть чужое правило своим значило бы отправить владельца искать его во
 * вкладке, где его нет.
 */
function ruleLabel(x: WatchTarget, t: T, applied: CustomRule[]): string {
	if (x.rule === 'RuleSet' && x.payload) return t('watch.rule.set', { name: x.payload.replace(/^nm-geosite-/, '') });
	if (x.rule === 'Match' || x.rule === '') return t('watch.rule.rest');
	if (x.payload && applied.some((r) => r.value === x.payload)) return t('watch.rule.own', { v: x.payload });
	return x.payload ? `${x.rule} ${x.payload}` : x.rule;
}

// ─── раскрытие строки ───

function Detail(p: RowProps & { cov: Covered | null }) {
	const { x, t, lang, cov } = p;
	const deadTunnel = x.verdict === 'unreachable' && toTunnel(x);
	const formOn = p.form?.key === x.key;

	const facts: [string, string][] = [
		[t('watch.det.port'), `${portsOf(x)} · ${x.net.toUpperCase()} · ${sigOf(x)}`],
		[t('watch.det.addr'), x.addr],
		[t('watch.det.rule'), ruleLabel(x, t, p.appliedRules)],
		[t('watch.det.to'), toTunnel(x) ? `${t('watch.to.tunnel')} · ${nodeOf(x.chain)}` : t('watch.to.direct')],
		[t('watch.det.conns'), t('watch.det.conns.val', { n: x.live, m: x.count })],
		[t('watch.det.traffic'), `↑ ${fmtBytes(x.up, t, lang)} ↓ ${fmtBytes(x.down, t, lang)}`],
		[t('watch.det.now'), x.rate_up || x.rate_down ? `↑ ${fmtRate(x.rate_up, t, lang) || '0'} ↓ ${fmtRate(x.rate_down, t, lang) || '0'}` : t('watch.det.now.quiet')],
		[t('watch.det.first'), `${hhmm(x.first)} → ${hhmm(x.last)}`],
	];
	// Страна добавляется, только если она есть: пустая строка «страна: —»
	// утверждала бы, что её не смогли определить.
	if ((x.geo ?? []).length > 0) facts.push([t('watch.det.cc'), (x.geo ?? []).join(' ')]);

	return (
		<div class="w-det" data-part="watch-detail">
			<div class="diags">
				{facts.map(([k, v]) => (
					<div key={k} class="diag">
						<span>{k}</span>
						<b>{v}</b>
					</div>
				))}
			</div>

			{x.last_error ? <p class="w-err">{x.last_error}</p> : null}

			{deadTunnel ? (
				<div class="note warn">
					<h3>{t('watch.dead.title')}</h3>
					<p>{t('watch.dead.text')}</p>
					<p>{t('watch.dead.hint', { node: nodeOf(x.error_chain || x.chain) })}</p>
					<a class="linkbtn" href="#engine">
						{t('watch.dead.cta')}
					</a>
				</div>
			) : null}

			{cov ? (
				<p class="w-cov">
					{t(cov.where === 'draft' ? 'watch.cov.draft' : 'watch.cov.live', {
						v: cov.rule.value,
						a: t(cov.rule.action === 'tunnel' ? 'watch.to.tunnel' : 'watch.to.direct'),
					})}{' '}
					<a href="#routes">{t('watch.cov.link')}</a>
				</p>
			) : null}

			{!formOn ? (
				<button
					type="button"
					class="linkbtn"
					disabled={p.eff.rules.length >= MAX_RULES}
					onClick={() =>
						p.setForm({
							key: x.key,
							// Предвыбор — ПРОТИВОПОЛОЖНОЕ тому, куда идёт сейчас:
							// правило заводят, чтобы изменить решение, а не
							// закрепить его.
							to: toTunnel(x) ? 'direct' : 'tunnel',
							cut: null,
							mask: 32,
							comment: '',
						})
					}
				>
					{t(cov ? 'watch.form.open.replace' : 'watch.form.open')}
				</button>
			) : (
				<RuleForm {...p} />
			)}
		</div>
	);
}

// ─── форма правила ───

function RuleForm(p: RowProps & { cov: Covered | null }) {
	const { x, t } = p;
	const f = p.form!;
	const labels = x.name.split('.');
	const cut = f.cut ?? Math.max(0, labels.length - 2);
	const domain = labels.slice(cut).join('.');
	const exact = cut === 0 && labels.length <= 1;
	const kind: CustomRule['kind'] = x.is_ip ? 'cidr' : cut === labels.length - 1 || exact ? 'domain' : 'suffix';
	const value = x.is_ip ? `${net4(x.name, f.mask)}/${f.mask}` : domain;
	const shown = x.is_ip ? value : kind === 'suffix' ? `*.${value}` : value;

	const add = () => {
		const rule: CustomRule = { kind, value, action: f.to, comment: f.comment.trim() };
		// Второе правило на ту же цель недостижимо — заменяем, а не копим.
		p.setDraft({ ...p.eff, rules: [...p.eff.rules.filter((r) => !(r.kind === rule.kind && r.value === rule.value)), rule] });
		p.setForm(null);
	};

	return (
		<div class="w-form" data-part="watch-form">
			<b>{t('watch.form.title')}</b>
			{x.verdict !== 'ok' && !(x.verdict === 'unreachable' && toTunnel(x)) ? (
				<p class="hint">{t(x.verdict === 'unreachable' ? 'watch.form.why.dead' : 'watch.form.why.silent')}</p>
			) : null}

			<div class="seg">
				{(['tunnel', 'direct'] as const).map((a) => (
					<button key={a} type="button" class="accent" aria-pressed={f.to === a} onClick={() => p.setForm({ ...f, to: a })}>
						{t(a === 'tunnel' ? 'watch.to.tunnel.act' : 'watch.to.direct.act')}
					</button>
				))}
			</div>
			<p class="hint">
				{f.to === (toTunnel(x) ? 'tunnel' : 'direct')
					? t('watch.form.same', { a: t(toTunnel(x) ? 'watch.to.tunnel' : 'watch.to.direct'), r: ruleLabel(x, t, p.appliedRules) })
					: t(f.to === 'tunnel' ? 'watch.form.toTunnel' : 'watch.form.toDirect')}
			</p>

			<b>{t(x.is_ip ? 'watch.form.mask' : 'watch.form.level')}</b>
			<p class="hint">{t(x.is_ip ? 'watch.form.mask.help' : 'watch.form.level.help')}</p>

			{x.is_ip ? (
				<div class="pills w-masks">
					{MASKS.map((m) => (
						<button key={m} type="button" aria-pressed={f.mask === m} onClick={() => p.setForm({ ...f, mask: m })}>
							{net4(x.name, m)}/{m} · {t(`watch.mask.${m}` as Key)}
						</button>
					))}
				</div>
			) : (
				<div class="w-parts">
					{labels.map((l, i) => (
						<button
							key={i}
							type="button"
							class={i >= cut ? 'on' : ''}
							aria-pressed={i >= cut}
							onClick={() => p.setForm({ ...f, cut: i })}
						>
							{l}
							{i < labels.length - 1 ? '.' : ''}
						</button>
					))}
				</div>
			)}

			<div class="w-result">
				{shown} → {t(f.to === 'tunnel' ? 'watch.to.tunnel.act' : 'watch.to.direct.act')}
			</div>

			<input
				class="note"
				value={f.comment}
				placeholder={t('watch.form.comment')}
				maxLength={MAX_COMMENT}
				autocomplete="off"
				onInput={(e) => p.setForm({ ...f, comment: (e.target as HTMLInputElement).value.replace(/[\r\n]+/g, ' ') })}
			/>

			<div class="w-form-acts">
				<button type="button" class="wide primary" onClick={add}>
					{t(p.cov ? 'watch.form.replace' : 'watch.form.add')}
				</button>
				<button type="button" class="wide" onClick={() => p.setForm(null)}>
					{t('watch.form.cancel')}
				</button>
			</div>
		</div>
	);
}

// ─── полоса черновика ───

function DraftBar({
	rules,
	open,
	onToggle,
	onApply,
	onDrop,
	onRemove,
	busy,
	locked,
	t,
	lang,
}: {
	rules: CustomRule[];
	open: boolean;
	onToggle: () => void;
	onApply: () => void;
	onDrop: () => void;
	onRemove: (r: CustomRule) => void;
	busy: boolean;
	locked: boolean;
	t: T;
	lang: Lang;
}) {
	return (
		<div class="confirm w-draft" data-part="watch-draft" data-confirm-for="watch-rules">
			<button type="button" class="w-draft-head" aria-expanded={open} onClick={onToggle}>
				<b>
					{t('watch.draft.title', {
						n: rules.length,
						w: plural(rules.length, lang, 'rules.custom.count.one', 'rules.custom.count.few', 'rules.custom.count.many', t),
					})}
				</b>
				<span class="w-draft-list">
					{rules.map((r) => `${r.value} → ${t(r.action === 'tunnel' ? 'watch.to.tunnel.act' : 'watch.to.direct.act')}`).join(' · ')}
				</span>
				<span class="chev" aria-hidden="true">
					›
				</span>
			</button>
			{open ? (
				<div class="w-draft-rows">
					{rules.map((r) => (
						<div key={`${r.kind}:${r.value}`} class="w-draft-row">
							<span>
								<b>{r.value}</b> {t(r.action === 'tunnel' ? 'watch.to.tunnel.act' : 'watch.to.direct.act')}
								{r.comment ? <i>{r.comment}</i> : null}
							</span>
							<button type="button" class="mini danger" onClick={() => onRemove(r)}>
								{t('watch.draft.remove')}
							</button>
						</div>
					))}
					<p class="hint">{t('watch.draft.note')}</p>
				</div>
			) : null}
			<div class="buttons">
				<button type="button" class="go" disabled={locked} aria-busy={busy} onClick={onApply}>
					{busy ? <Spin /> : null}
					{t(busy ? 'watch.draft.applying' : 'watch.draft.apply')}
				</button>
				<button type="button" class="no" onClick={onDrop}>
					{t('watch.draft.drop')}
				</button>
			</div>
		</div>
	);
}
