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
// Сценарий: кнопки в шапке страницы либо ?scenario=<имя> либо POST /__scenario

import http from 'node:http';
import fs from 'node:fs/promises';
import path from 'node:path';
import url from 'node:url';

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
};

// Что мок отвечает на запрос адреса панели Nikki.
//
// status и links обязаны быть согласованы: демон не кладёт адрес в links,
// когда открывать нечего, — иначе панель показала бы живую ссылку, которая
// гарантированно не открывается. Отсюда поле link.
//
// Четвёртое состояние, host_unknown, отдельного сценария не требует: оно
// наступает само, когда мок открыт по localhost (см. hostOf ниже) — ровно
// как на роутере, куда пришли через ssh-туннель.
const PANEL_FAIL = {
	'nikki-unconfigured': { status: 503, code: 'nikki_unconfigured', msg: 'Nikki не настроен', link: false },
	'nikki-nopanel': { status: 503, code: 'panel_missing', msg: 'У Nikki нет веб-морды: только Clash API', link: false },
	// Ссылка в шапке живая, а запрос за адресом падает пятисоткой. Сценарий
	// нужен глазами: панель обязана закрыть уже открытую пустую вкладку,
	// а не оставить белый прямоугольник без объяснений.
	'nikki-boom': { status: 500, code: 'internal', msg: 'Внутренняя ошибка', link: true },
};

const state = {
	scenario: 'single',
	// Изменения, накопленные POST-ами: мок не переписывает фикстуры, а
	// накладывает поверх — так исходные данные остаются эталоном.
	overlay: {},
	job: null,
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

// Ссылки на веб-морды строятся от заголовка Host — тем же правилом, что и на
// роутере. Фикстуры отдают links как есть, а мок открывают на localhost:
// без пересборки обе ссылки были бы мертвы всегда, и три состояния шапки
// в разработке посмотреть было бы нечем.
const LOCAL = new Set(['localhost', '127.0.0.1', '[::1]', '::1', '0.0.0.0', '::']);

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

const linksFor = (req) => {
	const host = hostOf(req);
	if (!host) return { nikki: null, b4: null };
	// Адрес веб-морды Nikki — БЕЗ секрета: порт 9090 и путь /ui/
	// (raw/73-nikki-ui-probe.txt). Секрет отдаётся только по явному запросу
	// GET /api/nikki/panel. Порт b4 — 7000 (raw/50-b4-api.txt).
	const fail = PANEL_FAIL[state.scenario];
	const nikki = fail && !fail.link ? null : `http://${host}:9090/ui/`;
	return { nikki, b4: `http://${host}:7000/` };
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

const startJob = (kind, arg, label, sec) => {
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
		if (mine()) { state.job.state = 'done'; state.job.finished_at = new Date().toISOString(); }
		setTimeout(() => { if (mine()) state.job = null; }, 1500);
	}, sec * 1000);
};

async function handleAPI(req, res, u) {
	const p = u.pathname;
	const method = req.method;

	// --- статус ---
	if (p === '/api/status' && method === 'GET') {
		const base = await currentStatus();
		const s = { ...base, ...state.overlay, links: linksFor(req), generated_at: new Date().toISOString() };
		if (state.job) s.job = state.job;
		if (s.online) s.online = { ...s.online, checked_at: s.generated_at };
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
		state.overlay.mode = mode;
		return send(res, 202, { job: state.job });
	}

	// --- upstream: во второй фазе ---
	if (p === '/api/upstream' && method === 'POST') {
		return fail(res, 501, 'not_implemented',
			'Переключение внешней сети появится во второй фазе');
	}

	// --- WiFi ---
	if (p === '/api/wifi/scan' && method === 'GET') {
		await new Promise((r) => setTimeout(r, 1200)); // скан не мгновенный
		return send(res, 200, await readJSON('wifi-scan.json'));
	}
	if (p === '/api/wifi/networks' && method === 'GET') {
		const d = await readJSON('wifi-networks.json');
		// Список сетей обязан согласовываться со сценарием. Мок, у которого
		// баннер говорит «сохранённых сетей нет», а список показывает две, —
		// хуже отсутствующего: он учит неправде о собственном интерфейсе.
		d.selection_state = (await currentStatus()).selection_state;
		if (state.scenario === 'empty') {
			d.networks = [];
		} else if (state.scenario === 'all-disabled') {
			d.networks = d.networks.map((n) => ({ ...n, enabled: false, editable: true }));
		} else if (state.scenario === 'ambiguous') {
			// Обе включены — это и есть конфликт. Править нельзя ни одну.
			d.networks = d.networks.map((n) => ({ ...n, enabled: true, editable: false }));
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
		const host = hostOf(req);
		// Порядок проверок тот же, что задуман на демоне: хост выводится из
		// заголовка Host, и без него собирать нечего — остальные причины
		// проверять уже незачем.
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
		const d = await readJSON('nikki-proxies.json');
		if (state.overlay.nikkiSelected) d.selected = state.overlay.nikkiSelected;
		return send(res, 200, d);
	}
	if (p === '/api/nikki/proxy' && method === 'POST') {
		const { name } = await readBody(req);
		state.overlay.nikkiSelected = name;
		return send(res, 200, { selected: name }); // быстрая операция: без джоба
	}
	if (p === '/api/nikki/test' && method === 'POST') {
		await new Promise((r) => setTimeout(r, 900));
		return send(res, 200, { ok: true });
	}

	// --- b4 ---
	if (p === '/api/b4/sets' && method === 'GET') {
		if (state.scenario === 'b4-down') {
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
		if (state.scenario === 'b4-down') {
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
