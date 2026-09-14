import { useCallback, useEffect, useMemo, useRef, useState } from 'preact/hooks';
import { api, ApiError, T as BUDGET } from '../api/client';
import { describe, errCode, failText, jobText, STALE_CODES, BRIDGE_STALE_CODES, WHY_CODES } from '../api/describe';
import type {
	BridgeState,
	JobAccepted,
	LogsResponse,
	Mode,
	NetworksResponse,
	ProxiesResponse,
	RulesDraft,
	RulesetsCatalog,
	RulesetsResponse,
	SavedNetwork,
	ScanResponse,
	SetsResponse,
	Status,
	SubscriptionURL,
	WatchHosts,
	WatchState,
} from '../api/types';
import { bridgeReason, upstreamReason } from '../api/reasons.gen';
import { fmtTime, loadLang, makeT, saveLang, type Lang } from '../i18n';
import { RETRY_MS } from '../state/consts';
import { resolveJob, useSvc } from '../state/job';
import { useLock } from '../state/lock';
import { usePoll } from '../state/poll';
import { useSide } from '../state/side';
import { useWatchPoll } from '../state/watch';
import { Section, Skel, Spin, useMedia } from './bits';
import { bridgeProblem, bridgeSummary } from './Bridge';
import { Engine, engineSummary, engineTitle } from './Engine';
import { rulesSummary } from './Rulesets';
import { Settings, SETTINGS_ROWS, type SettingsRow } from './Settings';
import { BridgeForm, NetworkForm, type BridgeBody, type NetBody, type NetSeed } from './Sheets';
import { Shelf } from './Shelf';
import { subSummary } from './Subscription';
import { Uplink } from './Uplink';
import { Watch, watchSummary } from './Watch';

const MODES: Mode[] = ['nikki', 'b4', 'off'];

/**
 * Куда ведёт адрес: на главную или на экран настроек, и какой раздел там
 * открыт. #routes / #sub / #bridge — настройки (старый #subscription из
 * закладок — тоже подписка), #settings — настройки с первым разделом; всё
 * остальное — главная, где хеш по-прежнему раскрывает аккордеон (fromHash).
 */
type Route = { screen: 'main' } | { screen: 'settings'; row: SettingsRow } | { screen: 'watch' };

function routeOf(hash: string): Route {
	const h = hash.replace(/^#/, '');
	// Наблюдатель — третий экран, и хеш у него свой (ADR-0043). Стоит РАНЬШЕ
	// разбора настроек: иначе #watch уехал бы в ветку «всё остальное», то
	// есть на главную, где раскрыл бы несуществующий раздел аккордеона.
	if (h === 'watch') return { screen: 'watch' };
	if (h === 'settings') return { screen: 'settings', row: 'routes' };
	if (h === 'subscription') return { screen: 'settings', row: 'sub' };
	if ((SETTINGS_ROWS as string[]).includes(h)) return { screen: 'settings', row: h as SettingsRow };
	return { screen: 'main' };
}

export function App() {
	const [lang, setLang] = useState<Lang>(loadLang);
	const t = useMemo(() => makeT(lang), [lang]);
	useEffect(() => saveLang(lang), [lang]);

	const wide = useMedia('(min-width: 900px)');

	const poll = usePoll();
	const lock = useLock(t('err.busy'), (e) => describe(e, t));
	const svc = useSvc();

	const nets = useSide<NetworksResponse>('wifiNetworks');
	const scan = useSide<ScanResponse>('wifiScan');
	const nikki = useSide<ProxiesResponse>('nikkiProxies');
	const sets = useSide<SetsResponse>('b4Sets');
	const logs = useSide<LogsResponse>('logs');
	const bridge = useSide<BridgeState>('bridge');
	const sub = useSide<SubscriptionURL>('subscription');
	const rulesets = useSide<RulesetsResponse>('nikkiRulesets');
	const catalog = useSide<RulesetsCatalog>('nikkiRulesetsCatalog');

	const [netForm, setNetForm] = useState<NetSeed | null>(null);
	// Экран — из адреса, и только из него: кнопка «назад» браузера обязана
	// возвращать с настроек на главную без нашего участия.
	const [route, setRoute] = useState<Route>(() => routeOf(location.hash));
	// Колокольчик: список проблем свёрнут, красная точка — пока не открыли.
	// Открытие гасит точку: список увиден, а не решён.
	const [bellOpen, setBellOpen] = useState(false);
	const [seen, setSeen] = useState<string[]>([]);
	// Черновик наборов живёт в памяти вкладки и теряется при F5 — осознанно:
	// сохранённый черновик пережил бы и смену режима, и правку по ssh, то
	// есть однажды применил бы выбор поверх файла, которого владелец уже
	// не помнит.
	const [rulesDraft, setRulesDraft] = useState<RulesDraft | null>(null);
	const [bridgeForm, setBridgeForm] = useState(false);
	// Наблюдатель опрашивается СВОИМ циклом и только пока его экран открыт:
	// состояние сессии велико, а нужно оно ровно здесь. И только этот опрос
	// продлевает TTL сессии — иначе «закрыл вкладку, через минуту погасло»
	// перестало бы работать у всякого, кто просто ушёл на главную.
	const watch = useWatchPoll(route.screen === 'watch');
	const watchHosts = useSide<WatchHosts>('watchHosts');
	// Скрытые провалы помнятся ПО СОБЫТИЮ, а не флагом «больше не показывать»:
	// крестик значит «я прочитал это», а не «отключить сообщения».
	const [failHidden, setFailHidden] = useState('');
	const [bridgeFailHidden, setBridgeFailHidden] = useState('');
	const [open, setOpen] = useState<Record<string, boolean>>(() => fromHash());

	// Единственный слушатель хеша. Настройки пишут свой раздел через
	// replaceState — оно события не порождает, и состояние React остаётся
	// источником правды; сюда приходят только переходы по ссылкам и «назад».
	useEffect(() => {
		const on = () => {
			const r = routeOf(location.hash);
			setRoute(r);
			if (r.screen === 'main') setOpen(fromHash());
		};
		addEventListener('hashchange', on);
		return () => removeEventListener('hashchange', on);
	}, []);

	const status = poll.status;
	const { running, failed, locked } = resolveJob(status, poll.seed, lock.busy, poll.skewMs);

	// ─── побочные списки ───

	const retryAt = useRef<Record<string, number>>({});
	const seenFp = useRef('');
	const wfp = status?.wireless_fingerprint;

	useEffect(() => {
		if (!wfp) return;
		if (nets.value && nets.value.fingerprint === wfp) {
			seenFp.current = wfp;
			return;
		}
		if (seenFp.current === wfp) return;
		// Отметка ставится по факту ЗАПИСИ ответа, а не перед отправкой:
		// иначе разовый отказ запирал бы карточку насмерть — конфигурация
		// больше не менялась, отпечаток тот же, повтора нет ни одного.
		const now = Date.now();
		if (now < (retryAt.current['nets'] ?? 0)) return;
		retryAt.current['nets'] = now + RETRY_MS;
		void nets.load().then(() => {
			seenFp.current = wfp;
		});
	}, [wfp, nets]);

	const mode: Mode | 'unknown' = MODES.includes(status?.mode as Mode)
		? (status?.mode as Mode)
		: 'unknown';

	const svcNikki = svc('nikki', !!status?.nikki.available, running ?? failed, poll.stale);
	const svcB4 = svc('b4', !!status?.b4.available, running ?? failed, poll.stale);

	// Списки движков грузятся, только когда движок отвечает: запрос к
	// лежащему движку — это гарантированный отказ, который панель тут же
	// покажет как «не отвечает», хотя и так знает это из статуса.
	useEffect(() => {
		if (svcNikki === 'up' && nikki.value === undefined) void nikki.load();
	}, [svcNikki, nikki]);
	useEffect(() => {
		if (svcB4 === 'up' && sets.value === undefined) void sets.load();
	}, [svcB4, sets]);
	useEffect(() => {
		if (status && logs.value === undefined) void logs.load({ query: { n: '5' } });
	}, [status, logs]);
	useEffect(() => {
		if (status && sub.value === undefined) void sub.load();
	}, [status, sub]);
	useEffect(() => {
		if (status && bridge.value === undefined) void bridge.load();
	}, [status, bridge]);

	// Наборы — исключение из правила «список движка ждёт живого движка»:
	// выбор лежит в файле, и демон отдаёт его с live:false, когда Nikki
	// молчит. Грузятся при любом режиме: полка на главной сводит их всегда,
	// а ждать svcNikki значило бы прятать раздел ровно в том случае, ради
	// которого его и открывают.
	useEffect(() => {
		if (status && rulesets.value === undefined) void rulesets.load();
	}, [status, rulesets]);

	// Список устройств грузится при заходе на экран и после остановки
	// наблюдения: между этими моментами он не меняется настолько, чтобы
	// платить за него запросом каждую секунду.
	useEffect(() => {
		if (route.screen !== 'watch') return;
		if (watchHosts.value === undefined) void watchHosts.load();
	}, [route.screen, watchHosts]);

	// Уход из режима nikki уносит черновик: раздел принадлежит режиму, и
	// вернувшийся владелец не должен обнаружить непринятые правки для
	// движка, который всё это время был выключен.
	useEffect(() => {
		if (mode === 'nikki') return;
		setRulesDraft(null);
	}, [mode]);

	// Список движка перечитывается, когда движок только что поднялся: до
	// этого момента его ответом был отказ, и без перечитывания панель
	// осталась бы с «не отвечает» до F5.
	const wasUp = useRef({ nikki: false, b4: false });
	useEffect(() => {
		if (svcNikki === 'up' && !wasUp.current.nikki) {
			void nikki.load();
			// Наборы перечитываются вместе с узлами: до подъёма движка ответ
			// нёс live:false и null вместо загрузки, и без перечитывания
			// панель до F5 утверждала бы «неизвестно» про уже загруженное.
			void rulesets.load();
		}
		wasUp.current.nikki = svcNikki === 'up';
	}, [svcNikki, nikki, rulesets]);
	useEffect(() => {
		if (svcB4 === 'up' && !wasUp.current.b4) void sets.load();
		wasUp.current.b4 = svcB4 === 'up';
	}, [svcB4, sets]);

	// Результат операции вместо тишины.
	//
	// Джоб доводит демон, а не панель: act снимает свой замок сразу после 202,
	// и без этого наблюдателя нажатие «Nikki» заканчивалось молчанием —
	// баннер просто когда-нибудь менялся. Отслеживается ПЕРЕХОД в done, а не
	// само состояние: демон держит завершённый джоб пять секунд, и без
	// запоминания id тост показывался бы все пять.
	const seenDone = useRef('');
	useEffect(() => {
		const j = status?.job;
		if (!j || j.state !== 'done') return;
		if (seenDone.current === j.id) return;
		seenDone.current = j.id;
		// У наборов свой тост: после применения важно не «готово», а какие
		// наборы не загрузились. Общее «готово» это скрыло бы, и владелец
		// узнал бы о неработающем правиле только по неработающему сайту.
		if (j.kind === 'rulesets') {
			void (async () => {
				try {
					const r = await api<RulesetsResponse>('nikkiRulesets', { timeoutMs: BUDGET.SIDE });
					rulesets.put(r);
					const bad = r.sets.filter((x) => x.loaded === false);
					if (bad.length === 0) {
						lock.flash(t('rules.done', { n: r.sets.length }), 'ok');
						return;
					}
					lock.flash(
						t('rules.done.partial', {
							bad: bad.length,
							n: r.sets.length,
							names: bad.map((x) => x.name).join(', '),
						}),
						'warn',
					);
				} catch {
					// Перечитать не вышло — сказать про завершение всё равно
					// надо: молчание после нажатия читается как зависание.
					lock.flash(t('job.done', { what: jobText(j, t) }), 'ok');
				}
			})();
			return;
		}
		const secs =
			j.finished_at && j.started_at
				? (new Date(j.finished_at).getTime() - new Date(j.started_at).getTime()) / 1000
				: null;
		lock.flash(
			secs != null && secs >= 0
				? t('job.done.timed', { what: jobText(j, t), sec: secs.toFixed(1) })
				: t('job.done', { what: jobText(j, t) }),
			'ok',
		);
	}, [status?.job, lock, t, rulesets]);

	// ─── действия ───

	const loadNets = useCallback(() => nets.load(), [nets]);

	const post = useCallback(
		<R,>(route: Parameters<typeof api>[0], body: unknown, timeoutMs?: number, ifMatch?: string) =>
			api<R>(route, {
				method: 'POST',
				body: JSON.stringify(body ?? {}),
				...(ifMatch ? { headers: { 'If-Match': ifMatch } } : {}),
				...(timeoutMs ? { timeoutMs } : {}),
			}),
		[],
	);

	const onMode = (m: Mode) =>
		void lock.act(`mode:${m}`, async () => {
			poll.sow(await post<JobAccepted>('mode', { mode: m }, BUDGET.MODE));
		});

	const onPickProxy = (name: string) =>
		void lock.act(
			`proxy:${name}`,
			() => post<ProxiesResponse>('nikkiProxy', { name }),
			() => nikki.load(),
		);

	const onToggleSet = (id: string, enabled: boolean) =>
		void lock.act(
			`set:${id}`,
			async () => sets.put(await post<SetsResponse>('b4Set', { id, enabled })),
			async () => {},
		);

	const onTest = () =>
		void lock.act(
			'test',
			async () => {
				const r = await post<ProxiesResponse>('nikkiTest', {}, BUDGET.TEST);
				nikki.put(r);
				const s = r.test;
				if (!s || typeof s.total !== 'number') return;
				if (s.total === 0) lock.flash(t('srv.test.empty'), 'warn');
				else if (s.measured === 0) lock.flash(t('srv.test.none', { n: s.total }), 'err');
				else if (s.skipped) lock.flash(t('srv.test.cut', { ok: s.measured, n: s.total }), 'warn');
				else if (s.failed)
					lock.flash(t('srv.test.part', { ok: s.measured, n: s.total, bad: s.failed }), 'warn');
				else lock.flash(t('srv.test.ok', { n: s.total }), 'ok');
			},
			async () => {},
		);

	const onScan = () =>
		void lock.act('scan', async () => {
			scan.put(await api<ScanResponse>('wifiScan', { timeoutMs: BUDGET.SCAN }));
		});

	const onConnect = (n: SavedNetwork) =>
		void lock.act(`switch:${n.id}`, async () => {
			try {
				poll.sow(await post<JobAccepted>('upstream', { id: n.id }, BUDGET.MODE, nets.value?.fingerprint));
			} catch (e) {
				// Устаревший список лечится перечитыванием, а не повтором.
				if (STALE_CODES.has(errCode(e))) await loadNets();
				throw e;
			}
		});

	const onDeleteNet = (n: SavedNetwork) =>
		void lock.act(
			`del:${n.id}`,
			() =>
				api('wifiNetworksById', {
					method: 'DELETE',
					params: { id: n.id },
					headers: { 'If-Match': nets.value?.fingerprint ?? '' },
				}),
			loadNets,
		);

	const saveNetwork = async (body: NetBody, setErr: (s: string) => void) => {
		try {
			return await api<NetworksResponse>('wifiNetworks', {
				method: 'POST',
				body: JSON.stringify(body),
				headers: { 'If-Match': nets.value?.fingerprint ?? '' },
			});
		} catch (e) {
			// Ошибка показывается РОВНО ОДИН раз — здесь, в форме. Пробросить
			// её выше значило бы получить второй тост другими словами, и общий
			// текст перебил бы точный.
			if (STALE_CODES.has(errCode(e))) await loadNets();
			setErr(describe(e, t));
			return null;
		}
	};

	const onSaveNet = (body: NetBody, setErr: (s: string) => void) =>
		void lock.act(
			'save',
			async () => {
				const r = await saveNetwork(body, setErr);
				if (r) {
					nets.put(r);
					setNetForm(null);
				}
			},
			async () => {},
		);

	const onSaveConnect = (body: NetBody, setErr: (s: string) => void) =>
		void lock.act(
			'save',
			async () => {
				// Снимок id ДО записи: свою запись мы находим диффом по id, а
				// не поиском по ssid — одинаковые ssid штатны (ADR-0005), и
				// поиск по имени на втором профиле находил бы две записи.
				const before = new Set((nets.value?.networks ?? []).map((n) => n.id));
				const r = await saveNetwork(body, setErr);
				if (!r) return;
				setNetForm(null);
				nets.put(r);
				const fresh = r.networks.filter((n) => n.id && !before.has(n.id));
				if (fresh.length !== 1) {
					lock.flash(t('wifi.saved.pick', { ssid: body.ssid }), 'warn');
					return;
				}
				// Ключ занятости переезжает на строку сети ПЕРЕД вторым
				// вызовом: пока он был 'save', форма уже закрыта, а признака
				// работы нет — до восьми секунд молчания после нажатия.
				const target = fresh[0];
				if (!target) return;
				lock.rekey(`switch:${target.id}`);
				try {
					poll.sow(await post<JobAccepted>('upstream', { id: target.id }, BUDGET.MODE, r.fingerprint));
				} catch (e) {
					// Своя ветка: сеть СОХРАНЕНА, и общее «не удалось» после
					// закрытой формы читается как «ничего не сохранилось» —
					// владелец заводит сеть второй раз, и у него два профиля
					// одной сети с разными паролями.
					lock.flash(t('wifi.saved.nolink', { ssid: body.ssid, why: describe(e, t) }), 'warn');
				}
			},
			async () => {},
		);

	const postBridge = async (route: 'bridgeEnable' | 'bridgeDisable' | 'bridgeAccess', body: unknown) => {
		try {
			const r = await post<JobAccepted>(route, body, BUDGET.MODE, bridge.value?.fingerprint);
			poll.sow(r);
			return r;
		} catch (e) {
			if (BRIDGE_STALE_CODES.has(errCode(e))) await bridge.load();
			throw e;
		}
	};

	const onBridgeProbe = () =>
		void lock.act('bridge:probe', () => bridge.load({ query: { probe: '1' } }));

	const onBridgeAccess = (on: boolean) =>
		void lock.act('bridge:access', () => postBridge('bridgeAccess', { enabled: on }), () => bridge.load());

	const onBridgeDisable = () =>
		void lock.act('bridge:disable', () => postBridge('bridgeDisable', null), () => bridge.load());

	const onBridgeEnable = (body: BridgeBody, setErr: (s: string) => void) =>
		void lock.act(
			'bridge:enable',
			async () => {
				try {
					await postBridge('bridgeEnable', body);
					setBridgeForm(false);
				} catch (e) {
					// Единственный отказ, который лечится согласием, а не
					// правкой формы.
					if (errCode(e) !== 'relayd_missing') {
						setErr(describe(e, t));
						return;
					}
					if (!confirm(t('bridge.confirm.install'))) return;
					try {
						await postBridge('bridgeEnable', { ...body, install: true });
						setBridgeForm(false);
					} catch (e2) {
						setErr(describe(e2, t));
					}
				}
			},
			() => bridge.load(),
		);

	const onUpdateSub = () =>
		void lock.act(
			'sub',
			async () => {
				poll.sow(await post<JobAccepted>('subscriptionUpdate', {}));
			},
			async () => {
				await logs.load({ query: { n: '5' } });
				await nikki.load();
			},
		);

	const onWatchStart = (ip: string) =>
		void lock.act(
			`watch:${ip}`,
			async () => {
				watch.put(
					await api<WatchState>('watch', {
						method: 'POST',
						body: JSON.stringify({ ip }),
						timeoutMs: BUDGET.SIDE,
					}),
				);
			},
			async () => {},
		);

	const onWatchStop = () =>
		void lock.act(
			'watch',
			async () => {
				await api<void>('watch', { method: 'DELETE', timeoutMs: BUDGET.SIDE });
				watch.put({ active: false, unparsed: 0, dropped: 0 });
			},
			// Список устройств перечитывается ПОСЛЕ остановки: «говорит
			// сейчас» за время сессии успевает измениться, а выбирать
			// владелец будет по нему же.
			async () => {
				await watchHosts.load();
			},
		);

	const onApplyRules = (d: RulesDraft) =>
		void lock.act(
			'rulesets',
			async () => {
				try {
					poll.sow(
						await api<JobAccepted>('nikkiRulesets', {
							method: 'PUT',
							body: JSON.stringify(d),
							headers: { 'If-Match': rulesets.value?.fingerprint ?? '' },
							timeoutMs: BUDGET.MODE,
						}),
					);
				} catch (e) {
					// Устаревший отпечаток лечится перечитыванием, а не
					// повтором: файл наборов изменился, и повтор затёр бы
					// чужую правку тем же черновиком.
					if (errCode(e) === 'stale_rulesets') await rulesets.load();
					throw e;
				}
				setRulesDraft(null);
			},
			async () => {},
		);

	// Каталог тянется лениво и ровно один раз: 60 КБ имён ради вкладки, куда
	// заходят редко. «Повторить» проходит сюда же — после отказа значение
	// null, а не объект, и запрет на повтор его бы и заблокировал.
	const onLoadCatalog = useCallback(() => {
		if (catalog.value) return;
		void catalog.load();
	}, [catalog]);

	const onSaveURL = (url: string) =>
		void lock.act(
			'suburl',
			async () => {
				sub.put(
					await api<SubscriptionURL>('subscription', {
						method: 'PUT',
						body: JSON.stringify({ url }),
					}),
				);
				lock.flash(t('sub.url.saved'), 'ok');
			},
			async () => {},
		);

	const onNikkiPanel = useCallback(
		async (e: MouseEvent) => {
			if (e.metaKey || e.ctrlKey || e.shiftKey || e.altKey || e.button !== 0) return;
			e.preventDefault();
			// Окно открывается ДО await: после него браузер уже не считает
			// это жестом владельца и блокирует вкладку.
			const w = window.open('', '_blank');
			if (w) w.opener = null;
			lock.flash(t('links.opening'), 'info');
			try {
				const r = await api<{ url: string }>('nikkiPanel', { timeoutMs: BUDGET.SIDE });
				if (w) w.location.replace(r.url);
				else lock.flash(t('links.blocked'), 'warn', { href: r.url, cta: t('links.blocked.cta') });
				if (w) lock.dropToast();
			} catch (err) {
				if (w) w.close();
				lock.flash(describe(err, t, 'links.err'), WHY_CODES.has(errCode(err)) ? 'warn' : 'err');
			}
		},
		[lock, t],
	);

	// ─── экран ───

	if (!status) {
		return (
			<div class="page">
				<div class="shell">
					<div class="grid">
						<div class="card full">
							<Skel n={3} />
						</div>
					</div>
				</div>
			</div>
		);
	}

	const toggle = (id: string) => setOpen((o) => ({ ...o, [id]: !o[id] }));
	const isOpen = (id: string) => wide || !!open[id];

	const problem = bridgeProblem(bridge.value);
	// Факты, о которых владелец должен узнать, даже когда смотрит на другое.
	// Не полоса над карточками, а список за колокольчиком: количество видно
	// всегда, точка говорит только о новизне (ADR-0042).
	const unsupported = nikki.value?.members.filter((m) => m.kind === 'unsupported') ?? [];
	const alerts: { id: string; title: string; text: string; act?: () => void; actLabel?: string; busy?: boolean }[] = [];
	if (problem) {
		alerts.push({
			id: `bridge:${problem}`,
			title: t(`problem.${problem}.title`),
			text: t(`problem.${problem}.text`, {
				pc: bridge.value?.pc_ip ?? '—',
				gw: bridge.value?.uplink?.gateway ?? '—',
			}),
			act: onBridgeProbe,
			actLabel: lock.on('bridge', 'probe') ? t('bridge.probing') : t('bridge.probe'),
			busy: lock.on('bridge', 'probe'),
		});
	}
	if (unsupported.length > 0) {
		alerts.push({
			id: `unsupported:${unsupported.length}`,
			title: t('sub.unsupported.title', { n: unsupported.length, names: unsupported.map((m) => m.name).join(', ') }),
			text: unsupported[0]?.reason ?? t('sub.unsupported.text'),
		});
	}
	const unseen = alerts.filter((a) => !seen.includes(a.id)).length;
	const openRow = (id: SettingsRow) => {
		// На широком экране раскрыты все — переключать нечего.
		const next = !wide && route.screen === 'settings' && route.row === id ? '' : id;
		setRoute({ screen: 'settings', row: (next || 'routes') as SettingsRow });
		history.replaceState(null, '', `#${next || 'settings'}`);
	};
	const w = watchSummary(status.watch, t);
	const watchRow = { id: 'watch' as const, title: t('shelf.watch'), summary: w.text, warn: w.warn };
	const upFail = status.last_fail;
	const upFailKey = upFail ? `${upFail.reason}:${upFail.at}` : '';
	const brFail = bridge.value?.last_fail ?? status.bridge_last_fail ?? null;
	const brFailKey = brFail ? `${brFail.reason}:${brFail.at}` : '';

	const ap = status.ap ?? {};
	const apMeta = [
		ap.ssid ? t('ap.broadcasts', { ssid: ap.ssid }) : '',
		ap.band ? ap.band.toUpperCase() : '',
		ap.clients != null ? t('ap.clients', { n: ap.clients }) : '',
	]
		.filter(Boolean)
		.join(' · ');

	const links = status.links ?? { nikki: null, b4: null, luci: null };
	const dotColor = running ? 'var(--warn)' : mode === 'off' ? 'var(--muted-2)' : 'var(--ok)';
	const online = status.online?.ok;
	const ssid = status.configured_ssid || status.associated_ssid || '';

	const foot = (
		<footer class="foot">
			<span class={poll.stale ? 'stale' : running ? 'busy' : ''}>
				{poll.stale
					? t('foot.stale')
					: running
						? t('foot.busy', { what: jobText(running, t) })
						: t('foot.free')}
			</span>
			<span>{t('foot.updated', { when: fmtTime(status.generated_at, lang) })}</span>
		</footer>
	);

	const langSwitch = (
		<div class="lang">
			{(['ru', 'en'] as const).map((l) => (
				<button key={l} type="button" aria-pressed={lang === l} onClick={() => setLang(l)}>
					{l.toUpperCase()}
				</button>
			))}
		</div>
	);

	const brFailNote =
		brFail && brFailKey !== bridgeFailHidden ? (
			<div class="note err full dismissable">
				<button type="button" class="x" onClick={() => setBridgeFailHidden(brFailKey)} title={t('ui.dismiss')} aria-label={t('ui.dismiss')}>
					✕
				</button>
				<h3>{t(`bridge.fail.${reasonKey(brFail.reason, 'bridge')}.title` as never)}</h3>
				<p>{t(`bridge.fail.${reasonKey(brFail.reason, 'bridge')}.text` as never)}</p>
				{brFail.detail ? <p class="detail">{brFail.detail}</p> : null}
			</div>
		) : null;

	// ─── экран наблюдателя ───
	//
	// Шапка своя, как у настроек: ссылка назад и имя экрана. Кнопка «назад»
	// браузера работает сама — экран берётся из хеша и только из него.
	if (route.screen === 'watch') {
		return (
			<div class="page">
				<div class="shell">
					<header class="top settings-top" data-part="top">
						<div class="top-id">
							<a class="toplink" href="#">
								‹ {t('watch.back')}
							</a>
							<span class="host">{t('watch.title')}</span>
							<span class="ap-meta">{t('watch.sub')}</span>
						</div>
						<div class="top-links">{langSwitch}</div>
					</header>
					<Watch
						state={watch.value}
						stale={watch.stale}
						hosts={watchHosts.value}
						mode={mode}
						applied={rulesets.value}
						draft={rulesDraft}
						setDraft={setRulesDraft}
						onStart={onWatchStart}
						onStop={onWatchStop}
						onApplyRules={onApplyRules}
						lock={lock}
						locked={locked}
						t={t}
						lang={lang}
					/>
					{foot}
				</div>
			</div>
		);
	}

	// ─── экран настроек ───
	if (route.screen === 'settings') {
		return (
			<div class="page">
				<div class="shell">
					<header class="top settings-top" data-part="top">
						<div class="top-id">
							<a class="toplink" href="#">
								‹ {t('settings.back')}
							</a>
							<span class="host">{t('settings.title')}</span>
							<span class="ap-meta">{t('settings.sub')}</span>
						</div>
						<div class="top-links">{langSwitch}</div>
					</header>
					<Settings
						row={route.row}
						onRow={openRow}
						wide={wide}
						t={t}
						lang={lang}
						lock={lock}
						locked={locked}
						status={status}
						rulesets={rulesets.value}
						catalog={catalog.value}
						draft={rulesDraft}
						setDraft={setRulesDraft}
						onApplyRules={onApplyRules}
						onLoadCatalog={onLoadCatalog}
						sub={sub.value}
						logs={logs.value}
						nikki={nikki.value}
						onUpdateSub={onUpdateSub}
						onSaveURL={onSaveURL}
						bridge={bridge.value}
						onBridgeProbe={onBridgeProbe}
						onBridgeAccess={onBridgeAccess}
						onBridgeDisable={onBridgeDisable}
						onOpenBridgeForm={() => setBridgeForm(true)}
						bridgeFail={brFailNote}
					/>
					{foot}
				</div>
				{bridgeForm && bridge.value ? (
					<BridgeForm
						state={bridge.value}
						lock={lock}
						locked={locked}
						t={t}
						onSubmit={onBridgeEnable}
						onClose={() => setBridgeForm(false)}
					/>
				) : null}
			</div>
		);
	}

	return (
		<div class="page">
			<div class="shell">
				{/* ─── шапка ─── */}
				<header class="top" data-part="top">
					<div class="top-id">
						<span class="dot" style={{ background: dotColor }} aria-hidden="true" />
						<span class="host">{status.hostname || 'netmoded'}</span>
						{apMeta ? <span class="ap-meta">{apMeta}</span> : null}
					</div>
					<div class="top-links">
						{/* Кнопка морды движка рисуется, ТОЛЬКО когда движок
						    отвечает (ADR-0024): серая заглушка отвечала бы на
						    вопрос «где панель Nikki» тем же молчанием, но при
						    этом ловила бы палец. */}
						{links.nikki && status.nikki.available ? (
							<a
								class="toplink"
								href={links.nikki}
								target="_blank"
								rel="noreferrer"
								title={t('links.nikki')}
								aria-label={t('links.nikki')}
								onClick={(e) => void onNikkiPanel(e as unknown as MouseEvent)}
							>
								{t('links.nikki.short')} <span aria-hidden="true">↗</span>
							</a>
						) : null}
						{links.b4 && status.b4.available ? (
							<a class="toplink" href={links.b4} target="_blank" rel="noreferrer" title={t('links.b4')} aria-label={t('links.b4')}>
								b4 <span aria-hidden="true">↗</span>
							</a>
						) : null}
						{mode === 'off' ? <span class="toplink dead">{t('links.engines.off')}</span> : null}
						{/* LuCI виден ВСЕГДА, когда адрес роутера выводится:
						    uhttpd мы не гасим, и его живость не считаем — это
						    стоило бы пробы к чужой службе на каждом тике. */}
						{links.luci ? (
							<a class="toplink" href={links.luci} target="_blank" rel="noreferrer" title={t('links.luci')} aria-label={t('links.luci')}>
								LuCI <span aria-hidden="true">↗</span>
							</a>
						) : null}
						<button
							type="button"
							class={`toplink bell${alerts.length ? ' has' : ''}${bellOpen ? ' open' : ''}`}
							aria-expanded={bellOpen}
							aria-controls="alerts"
							aria-label={
								alerts.length
									? unseen
										? t('bell.label.new', { n: alerts.length, m: unseen })
										: t('bell.label', { n: alerts.length })
									: t('bell.label.none')
							}
							onClick={() => {
								setBellOpen(!bellOpen);
								if (!bellOpen) setSeen(alerts.map((a) => a.id));
							}}
						>
							<svg aria-hidden="true" width="14" height="14" viewBox="0 0 16 16" fill="none" stroke="currentColor" stroke-width="1.5" stroke-linejoin="round">
								<path d="M4 6.5a4 4 0 0 1 8 0c0 2.2.5 3.4 1.2 4.2.3.3.1.8-.3.8H3.1c-.4 0-.6-.5-.3-.8C3.5 9.9 4 8.7 4 6.5Z" />
								<path d="M6.4 13.2a1.8 1.8 0 0 0 3.2 0" />
							</svg>
							{alerts.length}
							{unseen ? <span class="bell-dot" aria-hidden="true" /> : null}
						</button>
						<a class="toplink" href="#routes" title={t('settings.title')}>
							<span aria-hidden="true">⚙</span> {t('settings.link')}
						</a>
						{langSwitch}
					</div>
				</header>

				<div class="grid" data-part="grid">
					{/* ─── баннер режима ─── */}
					<section class={`hero m-${mode}`} data-part="hero">
						<div class="hero-head">
							<div class="kicker">{t('hero.kicker')}</div>
							<h1>{running ? t('hero.switching') : t(`title.${mode}`)}</h1>
							<div class="facts">
								<div class="fact">
									<span>{t('hero.uplink')}</span>
									<span>{ssid || t('net.nossid')}</span>
								</div>
								<div class="fact">
									<span>{t('hero.route')}</span>
									<span>{routeText(status, mode, nikki.value, sets.value, t)}</span>
								</div>
								<div class="fact">
									<span>{t('hero.internet')}</span>
									<span
										class={`pill ${running ? 'neutral' : online ? 'ok' : 'bad'}`}
										title={
											ssid
												? online
													? t('net.tip.online', { ssid })
													: t('net.tip.offline', { ssid })
												: t('net.nossid')
										}
									>
										{running ? t('net.checking') : online ? t('net.yes') : t('net.no')}
									</span>
								</div>
							</div>
						</div>

						<div class="hero-side">
							<div class="modes">
								{MODES.map((m) => (
									<button
										key={m}
										type="button"
										data-anchor={`mode-${m}`}
										aria-pressed={status.mode === m}
										disabled={locked || status.mode === m}
										aria-busy={lock.on('mode', m)}
										onClick={() => onMode(m)}
									>
										{lock.on('mode', m) ? <Spin /> : null} {t(`mode.${m}`)}
									</button>
								))}
							</div>

							{/* Слот: занята / результат / покой — три состояния
							    одного места. Высоту держит призрак, и держит
							    только под «занята»: тост перекрывает. */}
							<div class="slot" data-part="hero-status" data-status={running ? 'busy' : lock.toast ? 'result' : 'calm'}>
								{/* Призрак собран из ТОЙ ЖЕ разметки и с ТЕМ ЖЕ текстом,
								    что «занята». Пробел вместо подписи давал одну строку
								    там, где настоящая на 320px переносится на две:
								    четырнадцать пикселей прыжка ровно в тот момент, когда
								    владелец смотрит, сработало ли нажатие. Число сюда
								    вписать нельзя: оно сложилось бы из пяти констант и
								    разошлось бы МОЛЧА с первой же правкой шрифта. */}
								<div class="job ghost" aria-hidden="true">
									<div class="job-row">
										<span>{' '}</span>
										<span>{' '}</span>
									</div>
									<div class="bar" />
									<div class="job-note">{t('job.locked.note')}</div>
								</div>

								{running ? (
									<JobBar job={running} skewMs={poll.skewMs} t={t} />
								) : failed ? (
									<div class="result err">
										<span aria-hidden="true">✕</span>
										<span>{failText(failed, t)}</span>
									</div>
								) : lock.toast ? (
									<div class={`result ${lock.toast.kind === 'ok' ? '' : lock.toast.kind}`}>
										<span aria-hidden="true">{lock.toast.kind === 'ok' ? '✓' : '·'}</span>
										<span>{lock.toast.msg}</span>
										{lock.toast.href ? (
											<a class="cta" href={lock.toast.href} target="_blank" rel="noreferrer" onClick={() => setTimeout(lock.dropToast, 0)}>
												{lock.toast.cta}
											</a>
										) : null}
										<button type="button" class="x" onClick={lock.dropToast} title={t('ui.dismiss')} aria-label={t('ui.dismiss')}>
											✕
										</button>
									</div>
								) : (
									<div class="calm">{locked ? t('mode.hint.busy') : t('mode.hint')}</div>
								)}
							</div>
						</div>
					</section>

					{/* Живая область висит ПОСТОЯННО и вне слота: область,
					    вставленная вместе со своим содержимым, не озвучивается. */}
					<div class="live" role="status" aria-live="polite" style={{ display: 'none' }}>
						{lock.toast ? lock.toast.msg : ''}
					</div>

					{/* ─── проблемы за колокольчиком ─── */}
					{bellOpen ? (
						<div id="alerts" class="alerts full" data-part="alerts">
							{alerts.map((a) => (
								<div key={a.id} class="alert">
									<span class="dot" aria-hidden="true" />
									<div class="alert-text">
										<b>{a.title}</b> <span>{a.text}</span>
									</div>
									{a.act ? (
										<button type="button" disabled={locked} aria-busy={a.busy} onClick={a.act}>
											{a.busy ? <Spin /> : null} {a.actLabel}
										</button>
									) : null}
								</div>
							))}
							{alerts.length === 0 ? <div class="alert-none">{t('bell.none')}</div> : null}
						</div>
					) : null}

					{/* ─── исполнители ─── */}
					{status.missing_executors && status.missing_executors.length > 0 ? (
						<div class="note err full">
							<h3>{t('exec.missing.title')}</h3>
							<p>{t('exec.missing.text')}</p>
							{status.missing_executors.map((p) => (
								<code key={p}>{p}</code>
							))}
							<p>{t('exec.missing.fix')}</p>
						</div>
					) : null}

					{/* ─── неоднозначный выбор ─── */}
					{status.selection_state === 'ambiguous' ? (
						<div class="note err full">
							<h3>{t('sel.ambiguous.title')}</h3>
							<p>{t('sel.ambiguous.text')}</p>
							<p>{t('sel.ambiguous.switch')}</p>
							<code>{t('sel.ambiguous.cmd', { host: status.hostname || 'router' })}</code>
						</div>
					) : status.selection_state === 'all_disabled' || status.selection_state === 'empty' ? (
						<div class="note warn full">
							<h3>{t(status.selection_state === 'empty' ? 'sel.empty.title' : 'sel.alldisabled.title')}</h3>
							<p>{t(status.selection_state === 'empty' ? 'sel.empty.text' : 'sel.alldisabled.text')}</p>
						</div>
					) : null}

					{/* ─── разделы ─── */}
					<Section id="uplink" title={t('wifi.title')} summary={ssid || t('net.nossid')} wide={wide} open={isOpen('uplink')} onToggle={() => toggle('uplink')} t={t}>
						<Uplink
							nets={nets.value}
							scan={scan.value}
							status={status}
							lock={lock}
							locked={locked}
							t={t}
							onScan={onScan}
							onConnect={onConnect}
							onDelete={onDeleteNet}
							onOpenForm={setNetForm}
						/>
					</Section>

					{upFail && upFailKey !== failHidden ? (
						<div class="note err full dismissable">
							<button type="button" class="x" onClick={() => setFailHidden(upFailKey)} title={t('ui.dismiss')} aria-label={t('ui.dismiss')}>
								✕
							</button>
							<h3>{t(`wifi.fail.${reasonKey(upFail.reason, 'wifi')}.title` as never, { ssid: upFail.ssid })}</h3>
							<p>{t(`wifi.fail.${reasonKey(upFail.reason, 'wifi')}.text` as never, { ssid: upFail.ssid })}</p>
							{upFail.detail || failed?.error ? <p class="detail">{failed?.error || upFail.detail}</p> : null}
						</div>
					) : null}

					<Section id="engine" title={engineTitle(mode, t)} summary={engineSummary(mode, status, nikki.value, sets.value, t)} wide={wide} open={isOpen('engine')} onToggle={() => toggle('engine')} t={t}>
						<Engine
							mode={mode}
							nikki={nikki.value}
							sets={sets.value}
							svcNikki={svcNikki}
							svcB4={svcB4}
							status={status}
							lock={lock}
							locked={locked}
							t={t}
							onPickProxy={onPickProxy}
							onToggleSet={onToggleSet}
							onTest={onTest}
							onMode={onMode}
						/>
					</Section>

					{/* Полка сводок вместо трёх карточек: то, что настраивают раз и
					    надолго, живёт на экране настроек, а здесь — ответ на
					    вопрос, надо ли туда идти. */}
					<Shelf
						t={t}
						rows={[
							{ id: 'routes', title: t('shelf.rules'), summary: rulesSummary(rulesets.value, rulesDraft, t) },
							{ id: 'sub', title: t('shelf.sub'), summary: subSummary(status, lang, t) },
							{ id: 'bridge', title: t('shelf.bridge'), summary: bridgeSummary(bridge.value, t), warn: !!problem },
							// Сводка наблюдателя называет СОСТОЯНИЕ: «выключен» — это
							// тоже состояние, и оно честнее, чем «перейти к
							// наблюдателю». Строки нет в режимах, где наблюдать
							// нечего, — но она остаётся, пока жива сессия: режим
							// могли сменить прямо при открытом наблюдателе, и
							// накопленное ещё можно дочитать.
							...(mode === 'nikki' || status.watch ? [watchRow] : []),
						]}
					/>

				</div>

				{foot}
			</div>

			{netForm ? (
				<NetworkForm
					seed={netForm}
					savedSsids={new Set((nets.value?.networks ?? []).map((n) => n.ssid))}
					lock={lock}
					locked={locked}
					t={t}
					onSave={onSaveNet}
					onSaveConnect={onSaveConnect}
					onClose={() => setNetForm(null)}
				/>
			) : null}

			{bridgeForm && bridge.value ? (
				<BridgeForm
					state={bridge.value}
					lock={lock}
					locked={locked}
					t={t}
					onSubmit={onBridgeEnable}
					onClose={() => setBridgeForm(false)}
				/>
			) : null}
		</div>
	);
}

/** Полоса идущей операции с обратным отсчётом. */
function JobBar({ job, skewMs, t }: { job: { started_at: string; eta_sec: number }; skewMs: number; t: ReturnType<typeof makeT> }) {
	const now = Date.now() + skewMs;
	const gone = (now - new Date(job.started_at).getTime()) / 1000;
	const pct = job.eta_sec ? Math.min(99, Math.max(0, Math.round((gone / job.eta_sec) * 100))) : 0;
	const left = job.eta_sec ? Math.max(0, job.eta_sec - Math.round(gone)) : null;
	return (
		<div class="job">
			<div class="job-row">
				<span class="blink">{jobText(job as never, t)}</span>
				<span>{left != null ? t('job.left', { sec: left }) : t('job.blocked')}</span>
			</div>
			<div class="bar">
				<i style={{ width: `${pct}%` }} />
			</div>
			<div class="job-note">{t('job.locked.note')}</div>
		</div>
	);
}

function routeText(
	status: Status,
	mode: Mode | 'unknown',
	nikki: ProxiesResponse | null | undefined,
	sets: SetsResponse | null | undefined,
	t: ReturnType<typeof makeT>,
): string {
	if (mode === 'off') return t('route.off');
	if (mode === 'unknown') return t('mode.unknown');
	if (mode === 'b4') {
		const n = sets?.enabled_count ?? status.b4.enabled_count;
		if (n === 1) return t('route.b4.one', { set: sets?.selected || status.b4.set });
		if (n === 0) return t('route.b4.none');
		return t('route.b4.many', { n });
	}
	const pinned = nikki?.pinned ?? status.nikki.pinned ?? false;
	const cur = nikki?.selected || status.nikki.set || '—';
	return pinned ? t('route.nikki.pinned', { node: cur }) : t('route.nikki.auto', { node: cur });
}

/**
 * Незнакомая причина — не ошибка панели: демон и панель обновляются порознь,
 * и новый код приедет раньше своего перевода.
 *
 * Списки берутся из ПОРОЖДЁННОГО файла, а не переписываются сюда. Копия
 * здесь была бы шестым источником той же таксономии, который гейт
 * check-fail-reasons.sh не проверяет, — то есть ровно тем местом, где она
 * разъедется молча.
 */
function reasonKey(reason: string, kind: 'wifi' | 'bridge'): string {
	return kind === 'wifi' ? upstreamReason(reason) : bridgeReason(reason);
}

/** Аккордеон открывается из адреса: #uplink разворачивает раздел, #all — все. */
function fromHash(): Record<string, boolean> {
	const h = location.hash.replace(/^#/, '');
	if (!h) return {};
	if (h === 'all') return { uplink: true, engine: true, subscription: true, bridge: true };
	return { [h]: true };
}
