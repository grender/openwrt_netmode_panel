#!/usr/bin/env node
// Мок-сервер для разработки панели без роутера.
//
// Отдаёт статику из web/ и API из docs/api/examples/ — тех самых golden-фикстур,
// против которых потом будут сверяться Go-обработчики. Значит панель и демон
// разрабатываются от одного контракта, и расхождение видно сразу, а не при
// первом клике на живом железе.
//
// Ноль зависимостей: только стандартная библиотека Node. В проекте нет npm,
// package.json и бандлера, и заводить их ради мока незачем.
//
// Запуск:  node web/mock-server.mjs  [порт]
// Сценарий: curl 'http://localhost:8088/__scenario?name=<имя>' (GET или POST)

import http from 'node:http';
import fs from 'node:fs/promises';
import path from 'node:path';
import url from 'node:url';
import crypto from 'node:crypto';

const HERE = path.dirname(url.fileURLToPath(import.meta.url));
const WEB = HERE;
const EX = path.join(HERE, '..', 'docs', 'api', 'examples');
const PORT = Number(process.argv[2] || process.env.PORT || 8088);

// Сценарии переключают, какой файл статуса отдаётся. Имена совпадают с
// именами фикстур: подглядывать в код, чтобы понять, что показывает мок,
// не нужно.
const SCENARIOS = {
	single: 'status-single.json',
	ambiguous: 'status-ambiguous.json',
	'all-disabled': 'status-all-disabled.json',
	empty: 'status-empty.json',
	'b4-down': 'status-b4-unavailable.json',
	job: 'status-job-running.json',
	// Отказы GET /api/nikki/panel. Статус берётся обычный: ломается не
	// состояние роутера, а возможность открыть веб-морду Nikki.
	'nikki-unconfigured': 'status-single.json',
	'nikki-nopanel': 'status-single.json',
	'nikki-boom': 'status-single.json',
	// Адрес роутера не выводится из Host. Статус обычный и движок отвечает —
	// смотреть надо именно на шапку: кнопки нет, хотя движок жив.
	'host-unknown': 'status-single.json',
	// Медленный старт службы режима (см. PREARM ниже). База — статус,
	// где нужный движок уже лежит: сценарий его потом ПОДНИМАЕТ, а не роняет.
	'b4-slow-start': 'status-b4-unavailable.json',
	'nikki-slow-start': 'status-single.json',
	// Главный: долгий режим nikki, потом переключение на b4. База — рабочий
	// nikki, где b4 молчит СТОЛЬКО, сколько вы просидите в этом режиме.
	'b4-after-nikki': 'status-single.json',
	'b4-never-up': 'status-b4-unavailable.json',
	// Провалившаяся операция. Три сценария на одной фикстуре: два различаются
	// длиной текста (JOB_FAIL ниже), третий убирает джоб через пять секунд.
	'job-fail-short': 'status-job-failed.json',
	'job-fail-long': 'status-job-failed.json',
	'job-fail-vanish': 'status-job-failed.json',
	// Переключение внешней сети (POST /api/upstream). База у всех пяти —
	// status-single.json: mode nikki, configured_ssid == associated_ssid ==
	// John24, wireless_fingerprint sha256:1f0c9a3b7d2e4a58 — то же значение,
	// что fingerprint в wifi-networks.json, иначе If-Match проверить нечем.
	// Исход решает POST-обработчик через overlay, а не отдельная фикстура на
	// сценарий: до нажатия кнопки все пять неотличимы.
	'upstream-ok': 'status-single.json',
	'upstream-fail-stale': 'status-single.json',
	'upstream-fail-nokey': 'status-single.json',
	'upstream-no-ipv4': 'status-single.json',
	'upstream-busy': 'status-single.json',
};

// SLOW_START_SEC — сколько секунд служба режима не слушает свой порт ПОСЛЕ
// того, как демон уже записал mode.
//
// Это не украшение и не «медленный мок»: демон коммитит UCI mode ДО запуска
// службы (internal/httpapi/modehandler.go:77-86, и порядок там намеренный —
// намерение сохраняется первым). Значит на живом роутере есть окно, в котором
// /api/status уже говорит mode:"b4", а порт 7000 ещё молчит, и /api/b4/sets
// отвечает 503. Восемь секунд — середина вилки SPEC §5 (5–15 с).
const SLOW_START_SEC = 8;

// PREARM — сценарии, где окно молчания заводится уже при переключении
// сценария, а не только по нажатию кнопки режима: их смысл виден без клика,
// и перезагрузка страницы в середине окна обязана показывать то же самое.
//
// Значение — движок, чей порт молчит. У b4-after-nikki здесь записи нет
// намеренно: в нём окно открывает именно нажатие, а до нажатия b4 молчит
// потому, что режим не его (см. инвариант в applyEngines).
const PREARM = {
	'b4-slow-start': 'b4',
	'nikki-slow-start': 'nikki',
};

// DEAD — движок, который в этом сценарии не поднимется вовсе.
//
// b4-never-up проверяет потолок ожидания в панели: скелетон обязан смениться
// на «не отвечает», а не остаться навсегда. b4-down — то же самое состояние,
// но без всякой истории: b4 лежал ещё до того, как страницу открыли.
const DEAD = {
	'b4-down': 'b4',
	'b4-never-up': 'b4',
};

const ENGINES = ['nikki', 'b4'];

// Блок движка в /api/status. Значения «поднятого» согласованы с b4-sets.json
// и nikki-proxies.json: панель показывает set из статуса, а список — из
// побочного запроса, и расхождение между ними выглядело бы как её ошибка.
//
// Порядок ключей — как в httpapi.Service: available, version, set,
// enabled_count. Поле pinned опущено (omitempty, и оно false).
const SERVICE_DOWN = { available: false, version: null, set: '', enabled_count: 0 };
const SERVICE_UP = {
	b4: { available: true, version: '1.74.1', set: 'general', enabled_count: 1 },
	// enabled_count у Nikki остаётся нулём: статус его не считает
	// (internal/httpapi/status.go — заполняются только Set и Pinned).
	nikki: { available: true, version: 'v1.19.27', set: 'NL-01', enabled_count: 0 },
};

// JOB_KEEP_MS — сколько демон показывает завершённую операцию.
//
// Ровно keepFinished из internal/job/job.go. По истечении из статуса исчезает
// весь джоб целиком — и заголовок провала, и текст ошибки. Мок обязан уметь
// то же самое: провал, который висит вечно, — состояние, которого на роутере
// нет, и мерить на нём читаемость значит мерить не то.
const JOB_KEEP_MS = 5000;

// Текст провалившейся операции — под замер геометрии баннера.
//
// Длинный — дословный текст демона (internal/httpapi/modehandler.go:129 плюс
// executor.ErrApplyFirewall), и он же лежит в фикстуре: это худший реальный
// случай, 2–3 строки при ширине карточки на телефоне. Короткий — провал
// записи UCI (modehandler.go:77-84 возвращает ошибку executor как есть),
// одна строка. Между ними баннер обязан менять высоту, а не обрезать текст.
const JOB_FAIL = {
	'job-fail-short': 'uci commit netmode: exit status 1',
	'job-fail-long':
		'режим не включён, обход выключен — трафик идёт напрямую: netmode-apply: firewall не перезапустился',
};

// Что мок отвечает на запрос адреса панели Nikki.
//
// Ломается не статус и не links, а ровно один запрос — тот, что идёт по
// клику. Links демон кладёт всегда, когда из Host выводится хост: поле не
// зависит ни от available, ни от наличия веб-морды (internal/httpapi/
// status.go:104-122). Прежде здесь стояло поле link, гасившее links.nikki
// в двух сценариях из трёх, — оно моделировало поведение, которого у демона
// нет, и заодно прятало кнопку, по которой эти сценарии только и проверяются.
//
// Все три достижимы глазами: кнопка Nikki в шапке рисуется, когда известен
// адрес И движок отвечает (ADR-0024), а база у всех трёх — status-single.json,
// где режим nikki и Clash API жив.
const PANEL_FAIL = {
	'nikki-unconfigured': { status: 503, code: 'nikki_unconfigured', msg: 'Nikki не настроен' },
	'nikki-nopanel': { status: 503, code: 'panel_missing', msg: 'У Nikki нет веб-морды: только Clash API' },
	// Панель обязана закрыть уже открытую пустую вкладку, а не оставить
	// белый прямоугольник без объяснений.
	'nikki-boom': { status: 500, code: 'internal', msg: 'Внутренняя ошибка' },
};

const state = {
	scenario: 'single',
	// Изменения, накопленные POST-ами: мок не переписывает фикстуры, а
	// накладывает поверх — так исходные данные остаются эталоном.
	overlay: {},
	job: null,
	// Окно молчания службы: upEngine — чей порт молчит, upAt — момент (мс),
	// с которого он начинает отвечать. Ноль означает «окна нет»: движок виден
	// таким, каким его показывает фикстура.
	upEngine: '',
	upAt: 0,
	// Докуда показывается провалившийся джоб из фикстуры. Ноль — бессрочно.
	jobUntil: 0,
};

const MIME = {
	'.html': 'text/html; charset=utf-8',
	'.js': 'text/javascript; charset=utf-8',
	'.css': 'text/css; charset=utf-8',
	'.json': 'application/json; charset=utf-8',
	'.svg': 'image/svg+xml',
};

const readJSON = async (name) => JSON.parse(await fs.readFile(path.join(EX, name), 'utf8'));

// Текущий статус сценария. Вынесен отдельно, потому что от него зависят
// и /api/status, и согласованность побочных списков.
const currentStatus = () => readJSON(SCENARIOS[state.scenario] || SCENARIOS.single);

// arm открывает окно молчания движка: столько-то секунд порт не отвечает,
// потом движок поднимается сам. Пустой engine закрывает окно вовсе.
const arm = (engine, sec = SLOW_START_SEC) => {
	state.upEngine = engine || '';
	state.upAt = engine ? Date.now() + sec * 1000 : 0;
};

// engineDown — порт движка не слушает: либо ещё не открылся, либо в этом
// сценарии не откроется никогда.
//
// Про инвариант «движок не тот, что в mode» здесь ничего нет намеренно: это
// знание про статус, и живёт оно в одном месте — applyEngines.
const engineDown = (engine) =>
	DEAD[state.scenario] === engine || (state.upEngine === engine && Date.now() < state.upAt);

// applyEngines приводит блоки движков в статусе к тому, что бывает на роутере.
//
// Инвариант: одновременно работает НЕ БОЛЬШЕ одного движка, и это тот, что
// записан в mode. netmode-apply гасит второй безусловно и до всего остального
// (files/usr/local/bin/netmode-apply:128-129 — `svc nikki stop`, `svc b4 stop`
// перед firewall restart), поэтому «mode: nikki при b4.available: true» —
// состояние, которого не бывает.
//
// Держать инвариант обязан мок, а не только фикстуры: mode меняется POST-ом
// на лету, и без этой сборки статус после нажатия расходился бы с роутером
// на ровном месте. Цена расхождения высокая с обеих сторон: в шапке рисуется
// кнопка чужого движка (ADR-0024 требует не больше одной, а в режиме off —
// ни одной), а главное — панель НИКОГДА не видит долго молчащий движок, и
// дефекты, которые из этого молчания растут, на ноуте невоспроизводимы.
//
// Движок режима поднятым здесь не назначается: фикстура вправе показывать
// mode: "b4" при лежащем b4 (status-b4-unavailable.json, status-job-failed.json
// — намерение записано, служба не поднялась). Мок вмешивается, только когда
// сам открыл окно молчания.
const applyEngines = (s) => {
	for (const e of ENGINES) {
		if (s.mode !== e || engineDown(e)) { s[e] = SERVICE_DOWN; continue; }
		if (state.upEngine === e) s[e] = SERVICE_UP[e];
	}
};

// engineUp — отвечает ли движок прямо сейчас, тем же расчётом, что и статус.
//
// Побочные списки обязаны соглашаться со статусом: «b4 лежит» в /api/status и
// работающий /api/b4/sets в соседнем ответе — это две правды об одном роутере,
// и та из них, что удобнее, всегда достаётся панели случайно. Отдельная
// проверка «а не идёт ли окно молчания» этого не давала: она молчала про
// фикстуры, где движок лежит сам по себе (status-job-failed.json), и про
// чужой режим, где движок погашен инвариантом.
const engineUp = async (engine) => {
	const s = { ...(await currentStatus()), ...state.overlay };
	applyEngines(s);
	return s[engine].available;
};

// Ссылки на веб-морды строятся от заголовка Host — тем же правилом, что и на
// роутере. Фикстуры отдают links как есть, а мок открывают на localhost:
// без пересборки все три ссылки были бы мертвы всегда, и в шапке не появилось
// бы ни одной кнопки — ни движков, ни LuCI.
const LOCAL = new Set(['localhost', '127.0.0.1', '[::1]', '::1', '0.0.0.0', '::']);

// LINK_HOST — чем подменяется невыводимый хост.
//
// Мок открывают по localhost (web/README.md), а из петли адрес роутера не
// выводится — правило hostOf ниже дословно повторяет демон. Повторив его и
// на выходе, мок отдавал бы links: null ВСЕГДА: ни одной кнопки движка в
// шапке, а вместе с ними ни одного пути к GET /api/nikki/panel — то есть три
// сценария его отказа проверить было бы нечем.
//
// Поэтому для петли подставляется LAN-адрес из фикстур. Случай «адрес не
// выводится» не потерян: он вынесен в сценарий host-unknown, где отдаётся
// ровно то, что отдал бы демон за ssh-туннелем.
const LINK_HOST = process.env.MOCK_LINK_HOST || '192.168.9.1';

// Host — это ввод, а не факт: в заголовок кладут что угодно, и мусор вроде
// `foo"onmouseover=` уехал бы прямо в href. Пропускаем только то, что вообще
// может быть именем хоста или IP-литералом; всё остальное — «адрес
// неизвестен», то есть тот же null, что отдал бы демон.
const HOST_OK = /^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?)*$|^\[[0-9A-Fa-f:.]+\]$/;

// Хост запроса или пустая строка, если из него адрес роутера не выводится.
const hostOf = (req) => {
	// Порт срезаем регуляркой, а не split(':'): у IPv6 хост сам полон
	// двоеточий и приходит как [::1]:8088.
	const host = String(req.headers.host || '').replace(/:\d+$/, '').toLowerCase();
	if (!host || !HOST_OK.test(host)) return '';
	// Из localhost адрес роутера не выводится: панель открыта через
	// ssh-туннель, и :7000 на этой машине — не b4. Суффикс .localhost сюда же
	// (RFC 6761: весь домен зарезервирован под петлю, и браузеры его так и
	// разрешают), как и вся сеть 127.0.0.0/8, а не один 127.0.0.1.
	if (LOCAL.has(host) || host.endsWith('.localhost') || /^127\./.test(host)) return '';
	return host;
};

// linkHost — хост для ссылок: из запроса, а для петли — LINK_HOST.
// Пустая строка означает «адрес не выводится», и это ровно один сценарий.
const linkHost = (req) => (state.scenario === 'host-unknown' ? '' : hostOf(req) || LINK_HOST);

// Все три поля собираются ОДИНАКОВО и не зависят ни от available, ни от того,
// ответит ли что-нибудь по адресу (internal/httpapi/status.go:104-122).
// Показывать ли кнопку — решает панель, и решает по available (ADR-0024);
// смешивать эти два предмета в моке значило бы проверять не тот код.
//
// Порядок полей значим и повторяет Links: nikki, b4, luci.
const linksFor = (req) => {
	const host = linkHost(req);
	if (!host) return { nikki: null, b4: null, luci: null };
	// Адрес веб-морды Nikki — БЕЗ секрета: порт 9090 и путь /ui/
	// (raw/73-nikki-ui-probe.txt). Секрет отдаётся только по явному запросу
	// GET /api/nikki/panel. Порт b4 — 7000 (raw/50-b4-api.txt).
	//
	// LuCI: схема http и порт по умолчанию, поэтому порта в адресе НЕТ.
	// uhttpd слушает и 80, и 443, но redirect_https='0' — редиректа нет, а
	// сертификат самоподписанный, и https увёл бы владельца на страницу об
	// угрозе безопасности вместо роутера. Путь — точка входа LuCI, а не «/»:
	// по «/» отдаётся статический /www/index.html. Всё — raw/72-luci-probe.txt.
	//
	// Живостью НЕ гейтится, и это не забывчивость. Правило «прятать кнопку
	// недоступной морды» действует на движки, которыми управляет netmode-apply
	// (ADR-0024, «Границы правила»): они гаснут по нашей же команде. uhttpd мы
	// не трогаем, available для него не считаем, и кнопка LuCI обязана быть
	// видна во всех сценариях, где выводится хост, — включая режим off, где
	// кнопок движков нет ни одной, и b4-down, где как раз и надо уйти чинить.
	return {
		nikki: `http://${host}:9090/ui/`,
		b4: `http://${host}:7000/`,
		luci: `http://${host}/cgi-bin/luci`,
	};
};

const send = (res, code, body, type = MIME['.json']) => {
	const b = typeof body === 'string' || Buffer.isBuffer(body) ? body : JSON.stringify(body, null, 2);
	res.writeHead(code, { 'Content-Type': type, 'Cache-Control': 'no-store' });
	res.end(b);
};

const fail = (res, code, errCode, msg) =>
	send(res, code, { code: errCode, error: msg });

const readBody = (req) => new Promise((resolve) => {
	let s = '';
	req.on('data', (c) => { s += c; });
	req.on('end', () => { try { resolve(s ? JSON.parse(s) : {}); } catch { resolve({}); } });
});

// Медленная операция: мок держит джоб те же секунды, что и роутер, иначе
// состояние «идёт применение» невозможно посмотреть.
// arg — машинное уточнение вида операции (для mode это nikki|b4|off).
// Панель подписывает джоб сама по kind и arg, а label читает только человек
// в syslog, поэтому мок обязан отдавать оба поля, иначе подпись в панели
// проверить нечем.
// 409 job_busy отбивает только ИДУЩУЮ операцию, а не любую присутствующую
// в статусе. Демон устроен именно так (job.Manager.Start проверяет
// State == Running), и держит завершённый джоб ещё несколько секунд, чтобы
// панель успела показать исход. Мок раньше проверял просто «есть ли джоб»
// и в это окно отказывал в новой операции — расхождение с контрактом,
// из-за которого поведение панели в самый интересный момент не проверялось.
const busyJob = () => !!state.job && state.job.state === 'running';

// finish — необязательный колбэк, которому решать, как джоб завершится:
// молча проставить state:"done" (по умолчанию, как у mode и subscription)
// либо самому дописать state:"failed"/error и подвинуть что-то в overlay
// (upstream: успех меняет ssid и отпечаток, провал взводит last_fail).
// Общая для всех операций механика — секунды ожидания, потом ещё немного
// показа исхода, — не дублируется: upstream отличается только тем, ЧТО
// происходит по истечении sec, а не КОГДА.
const startJob = (kind, arg, label, sec, finish) => {
	state.job = {
		id: 'j-' + Math.random().toString(16).slice(2, 8),
		kind, arg, label,
		started_at: new Date().toISOString(),
		finished_at: null,
		eta_sec: sec,
		state: 'running',
		error: null,
	};
	// Таймеры привязаны к своему джобу по id. Без этого таймер завершившейся
	// операции догонял уже следующую и гасил её: доиграл первый джоб — и через
	// полторы секунды из статуса пропадал второй, только что запущенный.
	const id = state.job.id;
	const mine = () => state.job && state.job.id === id;
	setTimeout(() => {
		if (mine()) {
			if (finish) finish(); else state.job.state = 'done';
			state.job.finished_at = new Date().toISOString();
		}
		setTimeout(() => { if (mine()) state.job = null; }, 1500);
	}, sec * 1000);
};

// Причина неудачи переключения по имени сценария. Отсутствие в таблице —
// успех. Строки те же, что в закрытом наборе LastFail.reason
// (docs/api/openapi.yaml) и в internal/httpapi/upstreamhandler.go.
const UPSTREAM_REASON = {
	'upstream-fail-stale': 'stayed_on_previous',
	'upstream-fail-nokey': 'not_associated',
	'upstream-no-ipv4': 'no_ipv4',
};

// Текст job.error — дословно из internal/httpapi/upstreamhandler.go
// (associationError и verifySwitch): панель и мок обязаны показывать
// человеку одну и ту же историю про один и тот же код.
const upstreamFailText = (reason, ssid, prevSsid) => {
	if (reason === 'stayed_on_previous') {
		return `станция осталась на прежней сети «${prevSsid}» — переключение на ` +
			`«${ssid}» не состоялось, хотя применение прошло без ошибки`;
	}
	if (reason === 'no_ipv4') {
		return `станция подключилась к «${ssid}», но внешний канал так и не получил ` +
			'адрес IPv4 — сеть подключена, интернета нет';
	}
	return `станция не подключилась к «${ssid}» за отведённое время`;
};

// Отпечаток /etc/config/wireless меняется вместе с содержимым: успешное
// переключение переписало disabled у двух секций, и старое значение стало
// неправдой. Демон считает sha256 от вывода uci show; мок этого не читает,
// но обязан отдать значение той же формы — панель сверяет строку, а не
// пересчитывает hash сама.
const nextFingerprint = () => 'sha256:' + crypto.randomBytes(8).toString('hex');

async function handleAPI(req, res, u) {
	const p = u.pathname;
	const method = req.method;

	// --- статус ---
	if (p === '/api/status' && method === 'GET') {
		const base = await currentStatus();
		const s = { ...base, ...state.overlay, links: linksFor(req), generated_at: new Date().toISOString() };
		if (state.job) s.job = state.job;
		if (s.online) s.online = { ...s.online, checked_at: s.generated_at };
		// Движки: инвариант роутера плюс окно молчания. mode здесь уже новый
		// (его положил overlay при POST либо сама фикстура), а служба ещё
		// лежит — ровно то расхождение, которое даёт роутер между коммитом
		// UCI и стартом службы.
		applyEngines(s);
		// Демон убирает завершённый джоб через keepFinished, и провал уходит
		// с экрана вместе с текстом ошибки. Ноль — «сценарий этого не
		// показывает», джоб из фикстуры висит бессрочно.
		if (state.jobUntil && Date.now() > state.jobUntil) s.job = null;
		// Длину текста провала задаёт сценарий, а не фикстура: под замер
		// баннера нужны оба края. Идущий джоб не трогаем — у него ошибки нет.
		const failText = JOB_FAIL[state.scenario];
		if (failText && s.job && s.job.state === 'failed') s.job = { ...s.job, error: failText };
		return send(res, 200, s);
	}

	// --- медленные операции: джоб ---
	if (p === '/api/mode' && method === 'POST') {
		if (busyJob()) return fail(res, 409, 'job_busy', 'Уже идёт другая операция');
		const { mode } = await readBody(req);
		if (!['nikki', 'b4', 'off'].includes(mode)) {
			return fail(res, 400, 'bad_request', 'Неизвестный режим');
		}
		startJob('mode', mode, mode === 'off' ? 'Выключение обхода' : `Переключение режима на ${mode}`, 8);
		// Режим виден в статусе СРАЗУ, до конца джоба: демон коммитит UCI
		// первым действием applyMode, и панель узнаёт новый mode задолго до
		// того, как служба начнёт отвечать.
		state.overlay.mode = mode;
		// Целевая служба молчит те же секунды, что на роутере. Для off движка
		// нет — окно закрывается: обход выключен, и молчать нечему.
		arm(mode === 'off' ? '' : mode);
		// Провалившийся джоб из фикстуры замещён идущим — срок его показа
		// больше ни при чём, иначе он погасил бы и новый.
		state.jobUntil = 0;
		return send(res, 202, { job: state.job });
	}

	// --- upstream: переключение внешней сети (джоб) ---
	//
	// Порядок проверок повторяет internal/httpapi/upstreamhandler.go: тело
	// разбирается первым (400), отпечаток — вторым (409), затем секция
	// ищется по id (404) и проверяется на switchable (409 already_selected —
	// единственный код из этой ветки, для которого в фикстурах есть данные),
	// и только тогда, уже беря джоб, отбивается job_busy. busyJob() стоит не
	// первым: занятый джоб в реальности проверяется при попытке его СТАРТА,
	// то есть после того, как остальное уже сошлось.
	if (p === '/api/upstream' && method === 'POST') {
		const body = await readBody(req);
		const keys = Object.keys(body);
		if (keys.some((k) => k !== 'id')) {
			return fail(res, 400, 'unsupported_field', 'В теле разрешено только поле id');
		}
		if (!body.id || typeof body.id !== 'string') {
			return fail(res, 400, 'bad_request', 'Нужно указать id сохранённой сети');
		}

		const ifMatch = req.headers['if-match'];
		const fp = state.overlay.wireless_fingerprint || (await currentStatus()).wireless_fingerprint;
		if (!ifMatch) {
			return fail(res, 409, 'fingerprint_required',
				'Нужен заголовок If-Match с текущим отпечатком wireless');
		}
		if (ifMatch !== fp) {
			return fail(res, 409, 'fingerprint_mismatch',
				'Список сетей изменился с тех пор, как вы его открыли — перечитайте и повторите');
		}

		const netsFile = state.scenario === 'ambiguous' ? 'wifi-networks-ambiguous.json' : 'wifi-networks.json';
		const target = (await readJSON(netsFile)).networks.find((n) => n.id === body.id);
		if (!target) return fail(res, 404, 'not_found', 'Сеть с таким id не найдена');
		if (!target.switchable) {
			return fail(res, 409, 'already_selected', 'Эта сеть уже единственная включённая');
		}

		if (busyJob()) return fail(res, 409, 'job_busy', 'Уже идёт другая операция. Дождитесь её завершения.');

		const ssid = target.ssid;
		const st = await currentStatus();
		const prevSsid = state.overlay.associated_ssid || st.associated_ssid;
		const reason = UPSTREAM_REASON[state.scenario];

		startJob('upstream', ssid, `Переключение внешней сети на ${ssid}`, 20, () => {
			if (reason) {
				// Провал приходит уже ПОСЛЕ коммита UCI (шаги 6–8 демона):
				// конфигурация записана, а станция не подтвердила её делом.
				// Оба канала доклада — job.error для того, кто смотрит
				// сейчас, last_fail для того, кто вернётся позже.
				state.job.state = 'failed';
				state.job.error = upstreamFailText(reason, ssid, prevSsid);
				state.overlay.last_fail = { ssid, reason, at: new Date().toISOString() };
			} else {
				state.job.state = 'done';
				state.overlay.configured_ssid = ssid;
				state.overlay.associated_ssid = ssid;
				state.overlay.wireless_fingerprint = nextFingerprint();
				// Успех стирает прошлую неудачу (verifySwitch делает то же
				// самое через s.fails.Clear()).
				state.overlay.last_fail = null;
			}
		});
		return send(res, 202, { job: state.job });
	}

	// --- WiFi ---
	if (p === '/api/wifi/scan' && method === 'GET') {
		await new Promise((r) => setTimeout(r, 1200)); // скан не мгновенный
		return send(res, 200, await readJSON('wifi-scan.json'));
	}
	if (p === '/api/wifi/networks' && method === 'GET') {
		if (state.scenario === 'ambiguous') {
			// Обе включены — это и есть конфликт: править и добавлять нельзя
			// ни одну (editable: false у обеих), а переключить — можно
			// (switchable: true у обеих, ADR-0026) — выход из неоднозначности
			// и есть единственная разрешённая на ней операция.
			return send(res, 200, await readJSON('wifi-networks-ambiguous.json'));
		}
		const d = await readJSON('wifi-networks.json');
		// Список сетей обязан согласовываться со сценарием. Мок, у которого
		// баннер говорит «сохранённых сетей нет», а список показывает две, —
		// хуже отсутствующего: он учит неправде о собственном интерфейсе.
		d.selection_state = (await currentStatus()).selection_state;
		if (state.scenario === 'empty') {
			d.networks = [];
		} else if (state.scenario === 'all-disabled') {
			d.networks = d.networks.map((n) => ({ ...n, enabled: false, editable: true }));
		} else if (state.overlay.configured_ssid) {
			// Успешное переключение upstream поменяло, какая секция включена.
			// Список обязан согласоваться со status.configured_ssid и с новым
			// отпечатком — иначе панель, перечитавшая список по расхождению
			// fingerprint, увидела бы список, который сам себе противоречит.
			d.fingerprint = state.overlay.wireless_fingerprint;
			d.networks = d.networks.map((n) => {
				const enabled = n.ssid === state.overlay.configured_ssid;
				return { ...n, enabled, editable: !enabled, switchable: !enabled };
			});
		}
		return send(res, 200, d);
	}
	if (p === '/api/wifi/networks' && method === 'POST') {
		if (state.scenario === 'ambiguous') {
			return fail(res, 409, 'ambiguous_selection',
				'В конфигурации включено несколько сетей — запись запрещена');
		}
		const body = await readBody(req);
		if (body.id === 'wifinet0') {
			return fail(res, 409, 'enabled_network_readonly',
				'Активную сеть в этой фазе менять нельзя');
		}
		return send(res, 200, { ok: true });
	}

	// --- Nikki: адрес веб-морды ---
	//
	// Отдельный эндпоинт, а не поле в /api/status, потому что в адресе лежит
	// api_secret — он же пароль веб-морды. В статусе, который панель тянет
	// раз в секунду, ему делать нечего.
	if (p === '/api/nikki/panel' && method === 'GET') {
		// Тот же хост, что уехал в links: иначе кнопка вела бы на один адрес,
		// а её собственный запрос отвечал бы про другой.
		const host = linkHost(req);
		// Порядок проверок тот же, что задуман на демоне: хост выводится из
		// заголовка Host, и без него собирать нечего — остальные причины
		// проверять уже незачем.
		//
		// Кликом сюда теперь не попасть: кнопки нет ровно тогда, когда нет
		// адреса (ADR-0024 убрал серую заглушку), поэтому ветка проверяется
		// сценарием host-unknown и curl-ом, а не пальцем.
		if (!host) {
			return fail(res, 503, 'host_unknown',
				'Адрес роутера не выводится из заголовка Host');
		}
		const bad = PANEL_FAIL[state.scenario];
		if (bad) return fail(res, bad.status, bad.code, bad.msg);
		// Набор параметров и порядок — как у LuCI (raw/71-luci-nikki-open-dashboard.txt):
		// дашборд читает их из query и сам кладёт секрет в заголовок к API.
		const q = new URLSearchParams({
			host, hostname: host, port: '9090', secret: 'mock-secret-not-a-real-one',
		});
		return send(res, 200, { url: `http://${host}:9090/ui/?${q}` });
	}

	// --- Nikki ---
	if (p === '/api/nikki/proxies' && method === 'GET') {
		if (!(await engineUp('nikki'))) {
			return fail(res, 503, 'nikki_unavailable', 'Nikki не отвечает');
		}
		const d = await readJSON('nikki-proxies.json');
		if (state.overlay.nikkiSelected) d.selected = state.overlay.nikkiSelected;
		return send(res, 200, d);
	}
	if (p === '/api/nikki/proxy' && method === 'POST') {
		if (!(await engineUp('nikki'))) {
			return fail(res, 503, 'nikki_unavailable', 'Nikki не отвечает');
		}
		const { name } = await readBody(req);
		state.overlay.nikkiSelected = name;
		return send(res, 200, { selected: name }); // быстрая операция: без джоба
	}
	if (p === '/api/nikki/test' && method === 'POST') {
		if (!(await engineUp('nikki'))) {
			return fail(res, 503, 'nikki_unavailable', 'Nikki не отвечает');
		}
		await new Promise((r) => setTimeout(r, 900));
		return send(res, 200, { ok: true });
	}

	// --- b4 ---
	if (p === '/api/b4/sets' && method === 'GET') {
		if (!(await engineUp('b4'))) {
			return fail(res, 503, 'b4_unavailable', 'Панель b4 не отвечает');
		}
		const d = await readJSON('b4-sets.json');
		if (state.overlay.b4Set) {
			d.selected = state.overlay.b4Set;
			d.sets = d.sets.map((x) => ({ ...x, enabled: x.id === state.overlay.b4SetId }));
		}
		return send(res, 200, d);
	}
	if (p === '/api/b4/set' && method === 'POST') {
		if (!(await engineUp('b4'))) {
			return fail(res, 503, 'b4_unavailable', 'Панель b4 не отвечает');
		}
		const { id } = await readBody(req);
		const d = await readJSON('b4-sets.json');
		const hit = d.sets.find((x) => x.id === id);
		if (!hit) return fail(res, 404, 'not_found', 'Сет не найден');
		state.overlay.b4SetId = id;
		state.overlay.b4Set = hit.name;
		return send(res, 200, { selected: hit.name }); // быстрая операция
	}

	// --- подписка и логи ---
	if (p === '/api/subscription/update' && method === 'POST') {
		if (busyJob()) return fail(res, 409, 'job_busy', 'Уже идёт другая операция');
		startJob('subscription', '', 'Обновление подписки', 4);
		return send(res, 202, { job: state.job });
	}
	if (p === '/api/logs' && method === 'GET') {
		const d = await readJSON('logs.json');
		// Свежая установка не может иметь истории обновлений.
		if (state.scenario === 'empty') { d.lines = []; d.n = 0; }
		return send(res, 200, d);
	}

	return fail(res, 404, 'not_found', `Нет обработчика для ${method} ${p}`);
}

const server = http.createServer(async (req, res) => {
	const u = new URL(req.url, 'http://localhost');

	// Переключение сценария — служебный маршрут мока, вне контракта.
	if (u.pathname === '/__scenario') {
		const name = u.searchParams.get('name');
		if (name && SCENARIOS[name]) {
			state.scenario = name;
			state.overlay = {};
			state.job = null;
			// Окно молчания и срок показа провала отсчитываются от переключения
			// сценария: оба состояния кратковременны, и наблюдать их надо
			// с самого начала. Пересмотреть — переключить сценарий заново.
			arm(PREARM[name] || '');
			state.jobUntil = name === 'job-fail-vanish' ? Date.now() + JOB_KEEP_MS : 0;
		}
		return send(res, 200, { scenario: state.scenario, available: Object.keys(SCENARIOS) });
	}

	if (u.pathname.startsWith('/api/')) {
		try {
			return await handleAPI(req, res, u);
		} catch (e) {
			return fail(res, 500, 'internal', String(e && e.message));
		}
	}

	// Статика.
	let rel = u.pathname === '/' ? '/index.html' : u.pathname;
	const file = path.join(WEB, path.normalize(rel).replace(/^(\.\.[/\\])+/, ''));
	if (!file.startsWith(WEB)) return send(res, 403, 'forbidden', 'text/plain');
	try {
		const body = await fs.readFile(file);
		return send(res, 200, body, MIME[path.extname(file)] || 'application/octet-stream');
	} catch {
		return send(res, 404, 'not found', 'text/plain; charset=utf-8');
	}
});

server.listen(PORT, () => {
	console.log(`netmoded mock  →  http://localhost:${PORT}`);
	console.log(`сценарии: ${Object.keys(SCENARIOS).join(', ')}`);
	console.log(`переключить: curl -s 'http://localhost:${PORT}/__scenario?name=ambiguous'`);
});
