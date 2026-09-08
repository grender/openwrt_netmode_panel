// Формы ответов демона.
//
// Пишутся руками, а не порождаются из openapi.yaml, и это осознанно.
// Порождённый из схем тип — это ПЕРЕСКАЗ контракта, который выглядит
// проверкой: он совпадает с контрактом по построению и потому не способен
// поймать расхождение между контрактом и тем, что демон реально шлёт.
// Рукописный тип — независимая перекличка, ровно как рукописные словари
// рядом с порождённой таксономией причин.
//
// Порождается из контракта только то, где расхождение не имеет смысла:
// пути (routes.gen.ts) и перечисления причин (reasons.gen.ts).

import type { UpstreamReason, BridgeReason, UNKNOWN_REASON } from './reasons.gen';

export type Mode = 'nikki' | 'b4' | 'off';
export type ModeOrUnknown = Mode | 'unknown';

export type JobState = 'running' | 'done' | 'failed';

export interface Job {
	id: string;
	kind: string;
	arg: string;
	label: string;
	started_at: string;
	/** null, пока джоб идёт. */
	finished_at: string | null;
	eta_sec: number;
	state: JobState;
	error: string | null;
}

export interface Service {
	available: boolean;
	/** null, пока движок не ответил на пробу версии. */
	version: string | null;
	/** Активный узел (Nikki) или единственный включённый сет (b4). */
	set: string;
	/** Только у Nikki; ключ отсутствует, когда false. */
	pinned?: boolean;
	enabled_count: number;
}

export interface Subscription {
	configured: boolean;
	last_update: string | null;
	status: string;
	nodes: number;
	error: string | null;
}

export interface Fail {
	ssid: string;
	reason: UpstreamReason | typeof UNKNOWN_REASON | string;
	at: string;
	detail?: string;
}

export interface BridgeFail {
	action: string;
	reason: BridgeReason | typeof UNKNOWN_REASON | string;
	at: string;
	detail?: string;
}

export interface Links {
	nikki: string | null;
	b4: string | null;
	luci: string | null;
}

export interface Ap {
	ssid?: string;
	band?: string;
	clients?: number;
}

export type SelectionState = 'single' | 'ambiguous' | 'empty' | 'all_disabled';

export interface Status {
	generated_at: string;
	hostname?: string;
	mode: string;
	pending_apply?: boolean;
	selection_state?: SelectionState;
	configured_ssid?: string;
	associated_ssid?: string;
	online?: { ok: boolean; checked_at?: string };
	ap?: Ap;
	links?: Links;
	nikki: Service;
	b4: Service;
	subscription: Subscription | null;
	job: Job | null;
	last_fail: Fail | null;
	bridge_last_fail?: BridgeFail | null;
	missing_executors?: string[];
	wireless_fingerprint?: string;
}

// ─────────── Wi-Fi ───────────

export interface SavedNetwork {
	id: string;
	ssid: string;
	encryption: string;
	enabled: boolean;
	/** Решает СЕРВЕР. Своей копии правила у панели нет — иначе она разойдётся с ADR-0026. */
	switchable: boolean;
	editable: boolean;
	has_key?: boolean;
}

export interface NetworksResponse {
	fingerprint: string;
	networks: SavedNetwork[];
}

export interface ScanNetwork {
	ssid: string;
	encryption: string;
	signal_dbm: number;
}

export interface ScanResponse {
	band?: string;
	networks: ScanNetwork[];
}

// ─────────── Nikki ───────────

export type ProxyKind = 'node' | 'auto' | 'separator' | 'unsupported';

export interface Proxy {
	name: string;
	type: string;
	alive: boolean;
	/** null — пробы не было. Именно null: ноль означал бы «самый быстрый». */
	delay_ms: number | null;
	pinned: boolean;
	selectable: boolean;
	kind: ProxyKind;
	reason?: string;
}

export interface ProbeSummary {
	total: number;
	measured: number;
	failed: number;
	skipped: number;
	elapsed_ms: number;
}

export interface ProxiesResponse {
	available: boolean;
	version: string;
	group: string;
	type: string;
	/** Через кого группа работает СЕЙЧАС. */
	selected: string;
	/** Закреплённый вручную. Пусто — автовыбор. */
	fixed: string;
	pinned: boolean;
	selectable: boolean;
	members: Proxy[];
	/** Приходит ТОЛЬКО в ответе на замер. */
	test?: ProbeSummary;
}

export interface NikkiPanel {
	url: string;
}

// ─────────── b4 ───────────

export interface B4Set {
	id: string;
	name: string;
	enabled: boolean;
}

export interface SetsResponse {
	available: boolean;
	version: string;
	/** Имя, если включён ровно один. Иначе пусто — и при нуле, и при нескольких. */
	selected: string;
	enabled_count: number;
	sets: B4Set[];
}

// ─────────── проброс ───────────

export interface BridgeState {
	fingerprint: string;
	enabled: boolean;
	leg_ip: string | null;
	pc_ip: string | null;
	ap_access: boolean;
	relayd: { installed: boolean; running: boolean } | null;
	port: {
		name: string;
		carrier: boolean;
		speed_mbps: number | null;
		carrier_changes: number | null;
	} | null;
	uplink: {
		up: boolean;
		address: string | null;
		mask: number | null;
		gateway: string | null;
		device?: string;
	} | null;
	probes?: {
		pc: { answered: boolean } | null;
		gateway: { answered: boolean } | null;
	} | null;
	last_fail: BridgeFail | null;
}

// ─────────── подписка и журнал ───────────

export interface SubscriptionURL {
	configured: boolean;
	/** Схема, хост, путь и имена параметров. Ни одного символа секрета. */
	masked: string;
}

export interface LogLine {
	ts: string;
	nodes: number;
	status: 'ok' | 'fail';
	err: string;
}

export interface LogsResponse {
	path: string;
	n: number;
	lines: LogLine[];
}

export interface JobAccepted {
	job: Job;
}

// ─────────── наборы geosite ───────────

export type RulesetPolicy = 'profile' | 'only' | 'except';
export type RulesetDownload = 'direct' | 'tunnel';

export interface RulesetSet {
	name: string;
	ip: boolean;
	/**
	 * Три значения, и они РАЗНЫЕ: true — набор загружен, false — движок
	 * говорит «не загружен», null — движок не ответил вовсе. Слить null с
	 * false значит показать «не загрузился» там, где панель не знает
	 * ничего, и отправить владельца чинить исправное.
	 */
	loaded: boolean | null;
	rules: number | null;
	updated_at: string | null;
}

export type RuleKind = 'suffix' | 'domain' | 'cidr';
export type RuleAction = 'tunnel' | 'direct';

/**
 * Своё правило владельца: домен с поддоменами, точный хост или подсеть —
 * в туннель или напрямую. В mixin.yaml стоит раньше наборов: у mihomo
 * побеждает первое совпадение, и правило владельца перебивает набор.
 */
export interface CustomRule {
	kind: RuleKind;
	value: string;
	action: RuleAction;
	/** Пометка для человека, до 80 знаков; в файле — строка «#» над правилом. */
	comment: string;
}

export interface RulesetsResponse {
	fingerprint: string;
	policy: RulesetPolicy;
	download: RulesetDownload;
	tunnel_group: string;
	sets: RulesetSet[];
	/** В порядке файла — он же порядок применения. */
	rules: CustomRule[];
	/** Отвечал ли движок: при false про загрузку наборов не известно ничего. */
	live: boolean;
	/** Файл mixin.yaml написан не панелью — применять отсюда нельзя. */
	foreign: boolean;
}

export interface RulesetsCatalog {
	commit: string;
	fetched_at: string;
	/** Список из памяти демона старее суток: имена показываются, но с датой. */
	stale: boolean;
	names: string[];
	ip: string[];
	packs: { id: string; sets: string[] }[];
}

/** Черновик вкладки. null означает «совпадает с применённым». */
export interface RulesDraft {
	policy: RulesetPolicy;
	download: RulesetDownload;
	sets: string[];
	rules: CustomRule[];
}

export type EngineTab = 'nodes' | 'rules';
