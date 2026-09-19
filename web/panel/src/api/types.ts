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
	/** Сводка наблюдения за устройством; null — сессии нет (ADR-0043). */
	watch: WatchSummary | null;
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

/**
 * Куда идёт ОСТАЛЬНОЙ трафик — не совпавший ни со своим правилом, ни с
 * набором (хвост MATCH, ADR-0041). Направление набора здесь не решается:
 * оно у каждого набора своё.
 */
export type RulesetPolicy = 'profile' | 'direct' | 'tunnel';
/** Направление набора или своего правила. */
export type SetAction = 'tunnel' | 'direct';
export type RulesetDownload = 'direct' | 'tunnel';

export interface RulesetSet {
	name: string;
	ip: boolean;
	/** В туннель или напрямую — своё у каждого набора. */
	action: SetAction;
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
export type RuleAction = SetAction;

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
	/** Имя и направление; признак подсетей проставляет демон. */
	sets: { name: string; action: SetAction }[];
	rules: CustomRule[];
}


// ─── авто-пул: чем наполняется группа AUTO ───

/**
 * Чем наполняется авто-пул.
 *
 * `deny` — все узлы, кроме отмеченных; с пустым списком это весь список,
 * то есть поведение до появления пула. `allow` — только отмеченные.
 * `provider` — состав берёт демон из балансировщика подписки и
 * пересобирает при каждом её обновлении.
 */
export type AutopoolMode = 'deny' | 'allow' | 'provider';

export interface AutopoolResponse {
	fingerprint: string;
	mode: AutopoolMode;
	/** Отмеченные имена в том смысле, который задаёт mode. */
	nodes: string[];
	/** Файл mixin.yaml написан не панелью — применять отсюда нельзя. */
	foreign: boolean;
	/** Узлы подписки в авторском порядке провайдера: из них и отмечают. */
	available: string[];
	/** Состав балансировщика подписки. Пуст — режим provider недоступен. */
	provider_pool: string[];
	/** Отмеченные имена, которых в подписке больше нет. */
	missing: string[];
	/**
	 * Сколько узлов у движка в группе AUTO сейчас. null — «спросить не
	 * смогли», а не «пул пуст»: ноль соврал бы про погашенный движок.
	 */
	pool_size: number | null;
}

/** Черновик экрана. null означает «совпадает с применённым». */
export interface AutopoolDraft {
	mode: AutopoolMode;
	nodes: string[];
}

// ─── наблюдатель трафика устройства (ADR-0043) ───

/** Вид подключения устройства. */
export type WatchHostKind = 'wireless' | 'wired' | 'unknown';

/**
 * Устройство в списке выбора.
 *
 * Имени нет у большинства: роутер знает только аренду DHCP, и клиент имя
 * присылает не всегда. Такое устройство узнают по адресу и по тому, что оно
 * «говорит сейчас».
 */
export interface WatchHost {
	ip: string;
	/** Пуст у устройства без аренды — статики или виртуалки за relayd. */
	mac: string;
	/** Пусто, если клиент имени не прислал. */
	name: string;
	/**
	 * `unknown` — аренды нет ЛИБО точка доступа не ответила. Это не третий
	 * вид связи, а отсутствие знания о ней.
	 */
	kind: WatchHostKind;
	/** Говорит прямо сейчас — есть в снимке соединений. */
	active: boolean;
}

/**
 * Вердикт строки — главное, что есть на экране, и это факт, а не догадка.
 *
 * `unreachable` — движок пытался соединиться и не смог. Ушло напрямую —
 * вероятно, адрес заблокирован, лечится правилом «в туннель». Ушло в
 * туннель — правило НЕ поможет, и подсказка там другая.
 * `silent` — соединение есть, запрос отправлен, ответа нет: типичная
 * блокировка по имени сайта. Порог бывает ложным у долгого запроса, поэтому
 * это подсказка, а не приговор.
 */
export type WatchVerdict = 'ok' | 'silent' | 'unreachable';

/** Состояние движка глазами сессии наблюдения. */
export type WatchEngine = 'running' | 'restarting' | 'off';

/**
 * Строка экрана — АДРЕСАТ, а не соединение: одно приложение открывает
 * десятки соединений к одному имени.
 */
export interface WatchTarget {
	/** Имя и протокол: 443/tcp и 443/udp — это TLS и QUIC, разные вещи. */
	key: string;
	name: string;
	/** Имени нет; своё правило для такого адресата будет `cidr`. */
	is_ip: boolean;
	net: 'tcp' | 'udp';
	/** До трёх разных, по частоте. */
	ports?: number[];
	/** Реальный адрес, куда ушло соединение. Есть всегда. */
	addr: string;
	/**
	 * Метка страны, если движок её поставил, — примерно у трёх строк из ста.
	 * Ключа нет вовсе, и это НЕ «страна неизвестна»: поля просто нет.
	 */
	geo?: string[];
	rule: string;
	payload: string;
	/** Куда ушло: `DIRECT` либо `группа[узел]`. */
	chain: string;
	first: string;
	last: string;
	count: number;
	live: number;
	up: number;
	down: number;
	rate_up: number;
	rate_down: number;
	dial_errors: number;
	last_error: string;
	/**
	 * Цепочка, через которую НЕ дозвонились. Отдельным полем: у неудачи
	 * через туннель подсказка другая, и искать скобки в chain значило бы
	 * программировать на форме имени узла.
	 */
	error_chain: string;
	verdict: WatchVerdict;
	/** Появился после старта наблюдения; гаснет сам через 30 с. */
	new: boolean;
}

/** Ответ GET /api/watch. */
export interface WatchState {
	active: boolean;
	ip?: string;
	since?: string;
	engine?: WatchEngine;
	/**
	 * Строки журнала про соединение, которые демон не разобрал. Больше нуля
	 * значит, что движок сменил формат: снимки при этом работают, а у части
	 * адресатов нет правила и вердикта.
	 */
	unparsed: number;
	/** Адресаты, вытесненные сверх потолка. */
	dropped: number;
	targets?: WatchTarget[];
}

/**
 * Сводка наблюдения в статусе — для строки полки на главной.
 *
 * Именно в статусе, а не побочным списком: строка полки обязана называть
 * состояние и быть верной каждую секунду. Демон собирает её чтением, которое
 * не продлевает TTL сессии, — иначе открытая главная держала бы наблюдение
 * вечно.
 */
export interface WatchSummary {
	ip: string;
	since: string;
	engine: WatchEngine;
	/** Сколько адресатов не в порядке: «молчит» плюс «не отвечает». */
	problems: number;
}

/** Ответ GET /api/watch/hosts. */
export interface WatchHosts {
	hosts: WatchHost[];
}
