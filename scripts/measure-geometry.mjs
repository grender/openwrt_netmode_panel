#!/usr/bin/env node
// Оснастка измерения геометрии панели: одна команда, конечное время,
// ненулевой код возврата при нарушении критерия.
//
// ЗАЧЕМ. Утверждение «карточки под баннером не дёргаются» проверяемо только
// числом: скачок в 74px за один кадр глазами пропускается, а протокол из
// web/README.md требовал поднять мок руками, открыть DevTools, нажать кнопку
// режима и успеть снять число, пока бежит полоска. Такой замер невоспроизводим
// (гонка с двадцатисекундным джобом), непроверяем в ревью и не умеет падать.
// Здесь то же самое делает `make geometry`.
//
// ЧЕГО ЗДЕСЬ НЕТ И НЕ БУДЕТ.
//
// 1. Ни одного КЛИКА. Все состояния берутся из фикстур через GET /__scenario
//    плюс Page.navigate. Клик по кнопке режима запускает джоб на 8–20 секунд,
//    и замер превращается в гонку с таймером мока: перезагрузка страницы
//    стирает результат, а промах по фазе даёт число не от того состояния.
//    Ровно на этом висла предыдущая попытка мерить геометрию.
// 2. Ни одного `await` без дедлайна. У каждого вызова CDP — свой (T_CDP),
//    у стабилизации после навигации — свой (T_SETTLE), у прогона целиком —
//    свой (T_TOTAL). Скрипт, который умеет ждать бесконечно, однажды повиснет
//    молча в CI и съест сборку; это единственная защита от такого исхода.
// 3. Ни одного «сначала запустите мок». Мок и Chrome поднимает и гасит сам
//    скрипт — в process.on('exit') и по сигналам. Забытый Chrome с чужим
//    профилем живёт до перезагрузки ноутбука.
//
// ЗАПУСК:  node scripts/measure-geometry.mjs   (или `make geometry`)
// Требует: Node 22+, Google Chrome. npm не нужен.
//
// Почему 22, а не 20. Причин две, и хватило бы любой:
//   • глобальный WebSocket (им говорит клиент CDP ниже) на 20 доступен только
//     под флагом --experimental-websocket; без флага он там просто undefined,
//     и скрипт падает на первом же подключении к Chrome;
//   • скрипт САМ поднимает web/mock-server.mjs, а моку нужен Node 22+
//     (web/README.md). Оснастка не может требовать меньше, чем то, что она
//     запускает, — иначе «требования выполнены» означало бы падение мока.

import { spawn } from 'node:child_process';
import fs from 'node:fs';
import net from 'node:net';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const CHROME = process.env.CHROME_BIN
	|| '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';

// ─────────── бюджеты ожидания ───────────
//
// Один общий дедлайн не годится: у вызова CDP и у прогона целиком разные
// причины затянуться, и общий бюджет скрыл бы, что именно встало.
const T_CDP = 5000;      // ответ на один вызов CDP
const T_SETTLE = 8000;   // от навигации до устоявшихся метрик
const T_MOCK = 15000;    // мок начал отвечать на /api/status
const T_CHROME = 20000;  // Chrome открыл порт отладки
const T_TOTAL = 180000;  // прогон целиком

// Ширины. 320 — узкий телефон, 390 — обычный, 1440 — десктоп: брейкпоинт
// 900px переносит слот джоба в .side и раскладывает .wrap в три колонки,
// то есть за ним живёт другая вёрстка, а не та же в увеличенном виде.
const WIDTHS = [320, 390, 1440];

// Состояния. scenario — имя из SCENARIOS мока (web/mock-server.mjs), rm —
// эмуляция prefers-reduced-motion: reduce.
//
// Пары «покой/джоб» повторяются с rm намеренно: в @media
// (prefers-reduced-motion: reduce) панель гасит анимации (app.css), и если
// правило заодно тронет размеры, слот перестанет держать высоту ровно у тех
// владельцев, которые попросили меньше движения.
const STATES = [
	{ key: 'single', scenario: 'single', rm: false },
	{ key: 'job', scenario: 'job', rm: false },
	{ key: 'job-fail-short', scenario: 'job-fail-short', rm: false },
	{ key: 'job-fail-long', scenario: 'job-fail-long', rm: false },
	{ key: 'upstream-fail-seen', scenario: 'upstream-fail-seen', rm: false },
	{ key: 'upstream-fail-long', scenario: 'upstream-fail-long', rm: false },
	{ key: 'upstream-job-long', scenario: 'upstream-job-long', rm: false },
	{ key: 'upstream-long-list', scenario: 'upstream-long-list', rm: false },
	{ key: 'ambiguous', scenario: 'ambiguous', rm: false },
	// База сравнения для длинных состояний. Само по себе это состояние ничего
	// не проверяет — оно нужно C3/C4 (см. BASE ниже).
	{ key: 'single-long', scenario: 'single-long', rm: false },
	{ key: 'single-rm', scenario: 'single', rm: true },
	{ key: 'job-rm', scenario: 'job', rm: true },
];

const REST = 'single';
const JOB = 'job';

// Состояния, где на экране есть блок провала ПЕРЕКЛЮЧЕНИЯ ВНЕШНЕЙ СЕТИ
// (UpstreamFailNote). Только они годятся под C3/C4, и это не сужение
// критерия, а его единственное осмысленное прочтение.
//
// UpstreamFailNote стоит НИЖЕ карточки Wi-Fi (web/app.js: «Ниже WifiCard,
// а не выше: причина провала обязана стоять рядом с тем местом, где
// нажимали»), поэтому первую карточку он не двигает вовсе — это C4, — а
// последнюю двигает обязательно, и это C3.
//
// job-fail-short/long сюда не входят: там .note.err.full выносится в начало
// .wrap, ВЫШЕ всех карточек, и двигает их все. Это объявленное поведение
// (web/README.md, «Критерий», пункт 2: число больше ровно на высоту блока
// с текстом ошибки), а не регрессия, и требовать от него C4 значило бы
// требовать отмены принятого решения.
const UPSTREAM_FAIL = ['upstream-fail-seen', 'upstream-fail-long'];

// С ЧЕМ сравнивать каждое из них. Не с REST для всех подряд — и это не
// удобство, а условие того, чтобы C3/C4 вообще что-нибудь измеряли.
//
// Критерий вида «А отличается от Б» осмыслен, только когда А и Б различаются
// РОВНО ОДНИМ признаком — тем, который он проверяет (наличием блока провала).
// upstream-fail-long отличается от single двумя: блоком провала И длиной ssid
// в шапке (ap.ssid) и баннере (configured_ssid). Длинный ssid переносится на
// вторую строку и растит то, что стоит НАД карточками, — и C4 падал на этом
// росте, объявляя дефектом вёрстку, которой не касался.
//
// Доказательства, что дело было в длине, а не в блоке провала, два, и они
// в самом отчёте:
//   • у upstream-fail-long Δ card1Top в точности равна Δ wifiTop (52 на 320px,
//     64 на 390px) — сдвинулись обе карточки одинаково, то есть выросло что-то
//     над ними, а блок провала стоит НИЖЕ обеих и так двигать их не умеет;
//   • контроль: upstream-fail-seen — тот же блок провала, но ssid короткий —
//     совпадал с single ТОЧНО на всех трёх ширинах.
// Отсюда и правило: длинные состояния сравниваем с длинной базой, короткие —
// с короткой. single-long — это status-upstream-fail-long.json с last_fail:
// null и тем же списком сетей, то есть та же страница минус ровно блок
// провала.
const BASE = {
	'upstream-fail-seen': REST,
	'upstream-fail-long': 'single-long',
};

// ─────────── что именно меряем ───────────
//
// Один Runtime.evaluate на замер: два вызова подряд разделены сетью и
// событийным циклом страницы, и между ними успевает пройти тик опроса
// (POLL_MS=1000 в app.js) — то есть половина чисел оказалась бы от одной
// разметки, половина от следующей.
//
// СТРУКТУРНЫЕ АССЕРТЫ ИДУТ ДО ЗАМЕРА и возвращают ok:false с текстом. Без них
// скрипт зелен оттого, что померил не то: на первом кадре в .wrap лежат три
// карточки-скелетона (App(), ветка !status), и все числа снялись бы с
// загрузочного экрана.
//
// Адресация карточек — ТОЛЬКО индексом в querySelectorAll. :nth-of-type здесь
// врёт: он считает по типу элемента среди братьев, а перед карточками в .wrap
// стоят div.note (провал джоба) и div.note (SelectionNote) — тоже DIV. Номер
// поехал бы, причём в разных состояниях по-разному.
const MEASURE = `(() => {
	const cards = Array.from(document.querySelectorAll('.wrap .card'));
	if (cards.length < 2) return { ok: false, why: 'карточек в .wrap: ' + cards.length + ', нужно не меньше двух' };
	const head = (c) => { const h = c.querySelector('h2'); return h ? h.textContent.trim() : '(нет h2)'; };
	const wifiH = head(cards[1]);
	if (!/Внешняя сеть|Uplink network/.test(wifiH)) {
		return { ok: false, why: 'карточка [1] не про Wi-Fi, заголовок: ' + JSON.stringify(wifiH) };
	}
	const subH = head(cards[cards.length - 1]);
	if (!/Подписка|Subscription/.test(subH)) {
		return { ok: false, why: 'последняя карточка не про подписку, заголовок: ' + JSON.stringify(subH) };
	}
	const slot = document.querySelector('.slot');
	if (!slot) return { ok: false, why: 'в баннере нет .slot' };

	const top = (el) => el.getBoundingClientRect().top;
	const row = cards[1].querySelector('.row');

	// Узлы, у которых содержимое шире их самих. Это НЕ критерий, а число
	// в отчёт: перенос кнопок в .row объявлен осознанной ценой (app.css),
	// запрещать его нельзя — но и оставлять неизмеренным нельзя, иначе цена
	// растёт молча. Виновника C2 ищет отдельный список ниже: этот набор
	// селекторов фиксирован ради сравнимости и до баннера не достаёт вовсе.
	const SEL = '.note, .note h3, .note p, .hint, .card, .row, .top-id, .sheet';
	const desc = (el) => {
		const sel = el.tagName.toLowerCase() + Array.from(el.classList).map((c) => '.' + c).join('');
		const same = Array.from(document.querySelectorAll(sel));
		const i = same.indexOf(el);
		return sel + (same.length > 1 ? '[' + i + ']' : '');
	};
	const overflowers = Array.from(document.querySelectorAll(SEL))
		.filter((el) => el.scrollWidth > el.clientWidth + 1)
		.map((el) => ({ node: desc(el), scrollW: el.scrollWidth, clientW: el.clientWidth,
			text: (el.textContent || '').trim().slice(0, 48) }));

	// Кто именно уносит документ вправо. Отдельный список, а не расширенный
	// overflowers, и вот почему: overflowers смотрит на фиксированный набор
	// селекторов и даёт ЧИСЛО В ОТЧЁТ, сравнимое от прогона к прогону. А C2
	// про весь документ, и виновник может не иметь к этому набору никакого
	// отношения — так и вышло: первый же красный прогон нашёл переполнение
	// в баннере (.slot/.job), которого ни один из тех селекторов не видит.
	// Критерий, который умеет падать, но не умеет назвать узел, чинить нечем.
	//
	// Признак «уносит» — собственная рамка ЗА краем окна (rect.right), а не
	// scrollWidth: у html и body scrollWidth тоже больше clientWidth, но они
	// лишь наследуют чужую беду. Оставляем самые глубокие: их предки в списке
	// — тот же дефект, пересказанный на уровень выше.
	const win = window.innerWidth;
	const all = Array.from(document.body.querySelectorAll('*'));
	const outside = new Set(all.filter((el) => el.getBoundingClientRect().right > win + 1));
	const shell = (el) => {
		for (let p = el.parentElement; p && p !== document.body; p = p.parentElement) {
			if (p.scrollWidth > p.clientWidth + 1) return desc(p) + ' (' + p.scrollWidth + '>' + p.clientWidth + ')';
		}
		return '—';
	};
	const bleeders = [...outside]
		.filter((el) => !Array.from(el.querySelectorAll('*')).some((c) => outside.has(c)))
		// Сначала узлы С ТЕКСТОМ: «span.blink «Переключаю на Beeline_…»»
		// называет причину, а «div.bar» — только следствие, уехавшее вместе
		// с ним. Дальше — по дальности заезда за край.
		.sort((a, b) => {
			const ta = (a.textContent || '').trim() ? 0 : 1;
			const tb = (b.textContent || '').trim() ? 0 : 1;
			return ta - tb || b.getBoundingClientRect().right - a.getBoundingClientRect().right;
		})
		.map((el) => {
			const r = el.getBoundingClientRect();
			return {
				node: desc(el),
				right: Math.round(r.right),
				width: Math.round(r.width),
				inside: shell(el),
				text: (el.textContent || '').trim().slice(0, 48),
			};
		});

	return {
		ok: true,
		card1Top: top(cards[0]),
		wifiTop: top(cards[1]),
		subTop: top(cards[cards.length - 1]),
		slotH: slot.getBoundingClientRect().height,
		rowH: row ? row.getBoundingClientRect().height : null,
		docW: document.documentElement.scrollWidth,
		winW: win,
		overflowers,
		bleeders,
	};
})()`;

// ─────────── сторож: дети и глобальный дедлайн ───────────

// Где мы сейчас. Печатается по глобальному дедлайну: «висим» без указания
// состояния и ширины не чинится вообще ничем, кроме повторного зависания.
const phase = { what: 'старт', state: '—', width: '—' };
const at = () => `ждали «${phase.what}» в состоянии ${phase.state} на ширине ${phase.width}`;

const kids = [];
let profileDir = '';
let cleaned = false;

// Гасим детей и стираем профиль ровно один раз, откуда бы ни пришли.
// SIGKILL, а не SIGTERM: Chrome по TERM иногда уходит в «сохранение сессии»
// и переживает родителя, а брошенный headless держит порт отладки.
const cleanup = () => {
	if (cleaned) return;
	cleaned = true;
	for (const k of kids) { try { k.kill('SIGKILL'); } catch { /* уже мёртв */ } };
	if (profileDir) { try { fs.rmSync(profileDir, { recursive: true, force: true }); } catch { /* и ладно */ } }
};
process.on('exit', cleanup);
for (const sig of ['SIGINT', 'SIGTERM', 'SIGHUP']) {
	process.on(sig, () => { cleanup(); process.exit(1); });
}

const started = Date.now();
const guard = setTimeout(() => {
	console.error(`\nmeasure-geometry: глобальный дедлайн ${T_TOTAL / 1000} с — ${at()}`);
	cleanup();
	process.exit(1);
}, T_TOTAL);

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Хвост чужого stderr в одну строку. Целиком его печатать нельзя: стек Node
// или простыня предупреждений Chrome топят собственное сообщение скрипта,
// а именно оно объясняет, на какой фазе всё встало.
const tail = (chunks) => {
	const s = chunks.join('').trim().replace(/\s+/g, ' ');
	return s.length > 300 ? '…' + s.slice(-300) : s;
};

// ─────────── мелкая инфраструктура ───────────

// Свободный порт спрашиваем у ядра, а не берём фиксированный: мок владельца
// на 8088 может быть уже поднят, и прогон молча мерил бы чужой сервер
// с чужим сценарием.
const freePort = () => new Promise((resolve, reject) => {
	const s = net.createServer();
	s.on('error', reject);
	s.listen(0, '127.0.0.1', () => {
		const { port } = s.address();
		s.close(() => resolve(port));
	});
});

const getJSON = async (url, ms = T_CDP) => {
	const r = await fetch(url, { signal: AbortSignal.timeout(ms) });
	const text = await r.text();
	// Разбор намеренно не защищён: если по адресу отвечает не JSON, это и есть
	// ошибка, которую надо увидеть. Так, /json/new в свежем Chrome отвечает
	// строкой «Using unsafe HTTP verb GET…» — молчаливый разбор такого ответа
	// дал бы undefined вместо цели и падение через три шага не по адресу.
	return JSON.parse(text);
};

// Ждать до дедлайна, а не «пока не получится».
const waitFor = async (what, ms, fn) => {
	phase.what = what;
	const till = Date.now() + ms;
	let last = null;
	while (Date.now() < till) {
		try {
			const v = await fn();
			if (v) return v;
		} catch (e) { last = e; }
		await sleep(150);
	}
	throw new Error(`${what}: не дождались за ${ms} мс${last ? ' (последняя ошибка: ' + last.message + ')' : ''}`);
};

// ─────────── клиент CDP ───────────

const connectCDP = (wsUrl) => new Promise((resolve, reject) => {
	const ws = new WebSocket(wsUrl);
	const calls = new Map();
	const waiters = [];
	let nextId = 0;

	const openTimer = setTimeout(() => reject(new Error(`CDP: сокет не открылся за ${T_CDP} мс`)), T_CDP);

	// Закрытый сокет обязан развалить ВСЕ висящие обещания. Иначе падение
	// Chrome посреди прогона выглядит как зависание: вызов уже никогда не
	// ответит, а ждать его будут до глобального дедлайна.
	const breakAll = (msg) => {
		for (const c of calls.values()) c.reject(new Error(msg));
		calls.clear();
		for (const w of waiters.splice(0)) w.reject(new Error(msg));
	};

	ws.addEventListener('open', () => { clearTimeout(openTimer); resolve(api); });
	ws.addEventListener('error', () => { clearTimeout(openTimer); reject(new Error('CDP: ошибка сокета')); });
	ws.addEventListener('close', () => breakAll('CDP: сокет закрыт (Chrome умер?)'));
	ws.addEventListener('message', (ev) => {
		let m;
		try { m = JSON.parse(ev.data); } catch { return; }
		if (m.id != null) {
			const c = calls.get(m.id);
			if (!c) return;
			calls.delete(m.id);
			if (m.error) c.reject(new Error(`CDP ${c.method}: ${m.error.message}`));
			else c.resolve(m.result);
			return;
		}
		for (let i = waiters.length - 1; i >= 0; i--) {
			if (waiters[i].method === m.method) waiters.splice(i, 1)[0].resolve(m.params);
		}
	});

	const api = {
		send(method, params = {}) {
			return new Promise((res, rej) => {
				const id = ++nextId;
				const t = setTimeout(() => {
					calls.delete(id);
					rej(new Error(`CDP ${method}: нет ответа за ${T_CDP} мс`));
				}, T_CDP);
				calls.set(id, {
					method,
					resolve: (v) => { clearTimeout(t); res(v); },
					reject: (e) => { clearTimeout(t); rej(e); },
				});
				ws.send(JSON.stringify({ id, method, params }));
			});
		},
		// Ожидание события заводится ДО действия, которое его вызывает:
		// Page.loadEventFired приходит быстрее, чем разрешается промис
		// Page.navigate, и подписка после навигации ловила бы уже пустоту.
		wait(method, ms) {
			return new Promise((res, rej) => {
				const w = { method };
				const t = setTimeout(() => {
					const i = waiters.indexOf(w);
					if (i >= 0) waiters.splice(i, 1);
					rej(new Error(`CDP: события ${method} не было ${ms} мс`));
				}, ms);
				w.resolve = (v) => { clearTimeout(t); res(v); };
				w.reject = (e) => { clearTimeout(t); rej(e); };
				waiters.push(w);
			});
		},
		close() { try { ws.close(); } catch { /* всё равно убиваем процесс */ } },
	};
});

const evalJS = async (cdp, expr) => {
	const r = await cdp.send('Runtime.evaluate', { expression: expr, returnByValue: true });
	if (r.exceptionDetails) {
		const d = r.exceptionDetails;
		throw new Error('замер упал в браузере: ' + ((d.exception && d.exception.description) || d.text));
	}
	return r.result.value;
};

// Стабилизация: не setTimeout наугад, а два совпавших замера подряд.
//
// Наугад выбранная пауза — это ставка на скорость машины: на быстрой она
// лишняя, на медленной коротка, и во втором случае числа снимаются с
// недорисованной страницы, а прогон при этом зелёный. Панель к тому же
// дорисовывается волнами: сперва статус (карточки режима), потом
// /api/wifi/networks и /api/logs — между волнами разметка выглядит
// законченной, но числа ещё поедут.
const settle = async (cdp) => {
	phase.what = 'стабилизация метрик';
	const till = Date.now() + T_SETTLE;
	let prevJSON = '';
	let last = null;
	while (Date.now() < till) {
		const m = await evalJS(cdp, MEASURE);
		const j = JSON.stringify(m);
		if (m.ok && j === prevJSON) return m;
		prevJSON = j;
		last = m;
		await sleep(250);
	}
	throw new Error(last && !last.ok
		? `разметка не сошлась за ${T_SETTLE} мс: ${last.why}`
		: `метрики не устоялись за ${T_SETTLE} мс`);
};

// ─────────── печать ───────────

const num = (v) => (v == null ? '—' : String(Number(v.toFixed(2))));
const pad = (s, n) => String(s).padEnd(n);
const padL = (s, n) => String(s).padStart(n);

const printTable = (results) => {
	const cols = [
		['card1Top', 9], ['wifiTop', 9], ['subTop', 9], ['slotH', 7],
		['rowH', 7], ['docW', 6], ['winW', 6], ['overflow', 8],
	];
	for (const w of WIDTHS) {
		console.log(`\n── ширина ${w}px ` + '─'.repeat(58));
		console.log(pad('состояние', 20) + cols.map(([c, n]) => padL(c, n)).join(''));
		for (const st of STATES) {
			const m = results[w][st.key];
			const cells = [
				num(m.card1Top), num(m.wifiTop), num(m.subTop), num(m.slotH),
				num(m.rowH), String(m.docW), String(m.winW), String(m.overflowers.length),
			];
			console.log(pad(st.key, 20) + cells.map((v, i) => padL(v, cols[i][1])).join(''));
		}
	}
};

const printOverflowers = (results) => {
	console.log('\n── узлы, где содержимое шире своей коробки (не критерий, число в отчёт) ──');
	let any = false;
	for (const w of WIDTHS) {
		for (const st of STATES) {
			for (const o of results[w][st.key].overflowers) {
				any = true;
				console.log(`  ${w}px  ${pad(st.key, 20)} ${pad(o.node, 26)} ${o.scrollW} > ${o.clientW}  «${o.text}»`);
			}
		}
	}
	if (!any) console.log('  нет');
};

const printBleeders = (results) => {
	console.log('\n── узлы, вылезающие за правый край окна (виновники C2) ──');
	let any = false;
	for (const w of WIDTHS) {
		for (const st of STATES) {
			for (const b of results[w][st.key].bleeders) {
				any = true;
				console.log(`  ${w}px  ${pad(st.key, 20)} ${pad(b.node, 22)} правый край ${b.right} при окне ${w}`
					+ `, внутри ${b.inside}  «${b.text}»`);
			}
		}
	}
	if (!any) console.log('  нет');
};

// ─────────── критерии ───────────

const checkCriteria = (results) => {
	const bad = [];
	const R = (w, s) => results[w][s];

	// C1. Существующий критерий проекта, сохраняется дословно: слот держит
	// высоту призраком из той же разметки, поэтому равенство обязано быть
	// ТОЧНЫМ. Расхождение в 1px означает, что призрак собран не из того, что
	// замещает, — то есть прыжок вернётся, просто уменьшенный.
	for (const w of WIDTHS) {
		for (const [a, b] of [[REST, JOB], [REST + '-rm', JOB + '-rm']]) {
			if (R(w, a).card1Top !== R(w, b).card1Top) {
				bad.push(`C1: ${w}px — card1Top ${a}=${num(R(w, a).card1Top)} ≠ ${b}=${num(R(w, b).card1Top)}`);
			}
		}
	}

	// C2. Горизонтальной прокрутки быть не должно ни в одном состоянии:
	// на телефоне она уносит вбок весь документ вместе с баннером и шапкой.
	for (const w of WIDTHS) {
		for (const st of STATES) {
			const m = R(w, st.key);
			if (m.docW > m.winW) {
				// Три узла, не все: остальные — тот же дефект, пересказанный
				// соседями по коробке. Полный список печатается таблицей выше.
				const top3 = m.bleeders.slice(0, 3)
					.map((b) => `${b.node} до ${b.right} внутри ${b.inside}${b.text ? ' «' + b.text + '»' : ''}`);
				const rest = m.bleeders.length - top3.length;
				const who = top3.length
					? top3.join('; ') + (rest > 0 ? `; и ещё ${rest}` : '')
					: 'виновника найти не удалось — переполнение вне body?';
				bad.push(`C2: ${w}px, ${st.key} — docW ${m.docW} > winW ${m.winW}; ${who}`);
			}
		}
	}

	// C3. Доказательство, что замер физически видит появление блока провала.
	// Старая точка (card1Top) этого не могла в принципе: UpstreamFailNote стоит
	// ниже неё, и при любой поломке блока число не дрогнуло бы.
	for (const w of WIDTHS) {
		for (const s of UPSTREAM_FAIL) {
			const b = BASE[s];
			if (!(R(w, s).subTop > R(w, b).subTop)) {
				bad.push(`C3: ${w}px — subTop ${s}=${num(R(w, s).subTop)} не больше ${b}=${num(R(w, b).subTop)}`);
			}
		}
	}

	// C4. И при этом первую карточку блок провала не двигает.
	//
	// Сравнение идёт с BASE[s], а не с REST: база обязана совпадать
	// с состоянием по ДЛИНЕ ssid, иначе разность считает две вещи сразу
	// и не измеряет ни одной — разбор в комментарии к BASE выше.
	for (const w of WIDTHS) {
		for (const s of UPSTREAM_FAIL) {
			const base = BASE[s];
			if (R(w, s).card1Top !== R(w, base).card1Top) {
				const d1 = R(w, s).card1Top - R(w, base).card1Top;
				const d2 = R(w, s).wifiTop - R(w, base).wifiTop;
				// Разложение сдвига, без которого сообщение не чинится. Равные
				// дельты у первой и второй карточки означают, что вырос кто-то
				// НАД .wrap — шапка или баннер, — и блок провала тут ни при чём:
				// он стоит ниже обеих. Разные дельты — это уже он.
				const where = d1 === d2
					? 'сдвиг пришёл сверху (шапка/баннер), блок провала карточки не двигал'
					: 'сдвиг между карточками — виноват блок провала';
				bad.push(`C4: ${w}px — card1Top ${s}=${num(R(w, s).card1Top)} ≠ ${base}=${num(R(w, base).card1Top)}`
					+ ` (Δ${num(d1)}, у wifiTop Δ${num(d2)}: ${where})`);
			}
		}
	}

	// C5. Высота слота — одно число на всю панель. Разойдись она хоть где-то,
	// C1 стал бы проверять совпадение двух одинаково съехавших чисел.
	const seen = new Map();
	for (const w of WIDTHS) {
		for (const st of STATES) {
			const h = R(w, st.key).slotH;
			if (!seen.has(h)) seen.set(h, []);
			seen.get(h).push(`${w}px/${st.key}`);
		}
	}
	if (seen.size > 1) {
		const parts = [...seen.entries()].map(([h, where]) => `${num(h)} (${where.join(', ')})`);
		bad.push(`C5: высота .slot различается — ${parts.join(' | ')}`);
	}

	return bad;
};

// ─────────── прогон ───────────

async function main() {
	// 1. Мок.
	const mockPort = await freePort();
	const base = `http://127.0.0.1:${mockPort}`;
	phase.what = 'запуск мока';
	const mockErr = [];
	const mock = spawn(process.execPath, [path.join(ROOT, 'web', 'mock-server.mjs'), String(mockPort)],
		{ cwd: ROOT, stdio: ['ignore', 'ignore', 'pipe'] });
	kids.push(mock);
	mock.stderr.on('data', (c) => mockErr.push(String(c)));
	// Готовность — не «процесс запустился», а «отвечает по контракту».
	await waitFor('мок отвечает на /api/status', T_MOCK, async () => {
		if (mock.exitCode != null) throw new Error(`мок умер, код ${mock.exitCode}: ${tail(mockErr)}`);
		const s = await getJSON(`${base}/api/status`, 1000);
		return s && typeof s.mode === 'string';
	});

	// 2. Chrome.
	const cdpPort = await freePort();
	profileDir = fs.mkdtempSync(path.join(os.tmpdir(), 'netmode-geometry-'));
	phase.what = 'запуск Chrome';
	const chromeErr = [];
	const chrome = spawn(CHROME, [
		'--headless=new',
		'--disable-gpu',
		`--remote-debugging-port=${cdpPort}`,
		'--no-first-run',
		`--user-data-dir=${profileDir}`,
		'about:blank',
	], { stdio: ['ignore', 'ignore', 'pipe'] });
	kids.push(chrome);
	chrome.stderr.on('data', (c) => chromeErr.push(String(c)));
	chrome.on('error', (e) => chromeErr.push('spawn: ' + e.message));
	await waitFor('Chrome открыл порт отладки', T_CHROME, async () => {
		if (chrome.exitCode != null) throw new Error(`Chrome умер, код ${chrome.exitCode}: ${tail(chromeErr)}`);
		const v = await getJSON(`http://127.0.0.1:${cdpPort}/json/version`, 1000);
		return v && v.webSocketDebuggerUrl;
	});

	// Цель берём из /json/list и ищем type === 'page'. НЕ /json/new: он в
	// свежем Chrome отвечает текстом «Using unsafe HTTP verb GET…» вместо JSON
	// и валит разбор.
	const target = await waitFor('вкладка в /json/list', T_CHROME, async () => {
		const list = await getJSON(`http://127.0.0.1:${cdpPort}/json/list`, 1000);
		return Array.isArray(list) && list.find((x) => x.type === 'page' && x.webSocketDebuggerUrl);
	});

	phase.what = 'подключение к CDP';
	const cdp = await connectCDP(target.webSocketDebuggerUrl);
	await cdp.send('Page.enable');
	// Полосы прокрутки прячем намеренно. Классическая полоса забирает ~15px
	// ширины у layout viewport, и docW <= winW проходил бы при переполнении
	// вплоть до этих пятнадцати пикселей — то есть критерий C2 был бы слепым
	// ровно в самом частом диапазоне. Заодно это ближе к телефону, ради
	// которого 320px и мерится: там полосы наложенные и места не занимают.
	await cdp.send('Emulation.setScrollbarsHidden', { hidden: true });

	// 3. Прогон.
	const results = {};
	let nonce = 0;
	for (const w of WIDTHS) {
		phase.width = `${w}px`;
		results[w] = {};
		await cdp.send('Emulation.setDeviceMetricsOverride',
			{ width: w, height: 900, deviceScaleFactor: 1, mobile: false });
		for (const st of STATES) {
			phase.state = st.key;
			phase.what = 'переключение сценария';
			await getJSON(`${base}/__scenario?name=${encodeURIComponent(st.scenario)}`);
			phase.what = 'эмуляция prefers-reduced-motion';
			await cdp.send('Emulation.setEmulatedMedia', {
				features: st.rm ? [{ name: 'prefers-reduced-motion', value: 'reduce' }] : [],
			});
			phase.what = 'навигация';
			// Подписка ДО навигации — см. cdp.wait. Нонс в адресе гарантирует
			// настоящую навигацию, а не «мы уже здесь».
			const loaded = cdp.wait('Page.loadEventFired', T_SETTLE);
			await cdp.send('Page.navigate', { url: `${base}/?geom=${++nonce}` });
			await loaded;
			results[w][st.key] = await settle(cdp);
		}
	}

	cdp.close();
	clearTimeout(guard);
	cleanup();

	// 4. Отчёт.
	printTable(results);
	printOverflowers(results);
	printBleeders(results);

	const bad = checkCriteria(results);
	const secs = ((Date.now() - started) / 1000).toFixed(1);
	console.log('');
	if (bad.length === 0) {
		console.log(`measure-geometry: все критерии выполнены (C1–C5), ${secs} с`);
		return 0;
	}
	console.log(`measure-geometry: НАРУШЕНО критериев — ${bad.length}`);
	for (const b of bad) console.log('  ✗ ' + b);
	console.log(`\nпрогон занял ${secs} с`);
	return 1;
}

main().then((code) => {
	clearTimeout(guard);
	cleanup();
	process.exit(code);
}).catch((e) => {
	clearTimeout(guard);
	console.error(`\nmeasure-geometry: ${e && e.message} — ${at()}`);
	cleanup();
	process.exit(1);
});
