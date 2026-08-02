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
const startJob = (kind, label, sec) => {
	state.job = {
		id: 'j-' + Math.random().toString(16).slice(2, 8),
		kind, label,
		started_at: new Date().toISOString(),
		finished_at: null,
		eta_sec: sec,
		state: 'running',
		error: null,
	};
	setTimeout(() => {
		if (state.job) { state.job.state = 'done'; state.job.finished_at = new Date().toISOString(); }
		setTimeout(() => { state.job = null; }, 1500);
	}, sec * 1000);
};

async function handleAPI(req, res, u) {
	const p = u.pathname;
	const method = req.method;

	// --- статус ---
	if (p === '/api/status' && method === 'GET') {
		const base = await currentStatus();
		const s = { ...base, ...state.overlay, generated_at: new Date().toISOString() };
		if (state.job) s.job = state.job;
		if (s.online) s.online = { ...s.online, checked_at: s.generated_at };
		return send(res, 200, s);
	}

	// --- медленные операции: джоб ---
	if (p === '/api/mode' && method === 'POST') {
		if (state.job) return fail(res, 409, 'job_busy', 'Уже идёт другая операция');
		const { mode } = await readBody(req);
		if (!['nikki', 'b4', 'off'].includes(mode)) {
			return fail(res, 400, 'bad_request', 'Неизвестный режим');
		}
		startJob('mode', `Переключение режима на ${mode}`, 8);
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
		if (state.job) return fail(res, 409, 'job_busy', 'Уже идёт другая операция');
		startJob('subscription', 'Обновление подписки', 4);
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
