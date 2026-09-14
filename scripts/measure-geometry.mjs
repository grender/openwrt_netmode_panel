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
// 18 состояний на трёх ширинах против прежних 16, плюс раскрытия
// аккордеона. Пошаговые бюджеты не трогаем — именно они называют
// зависшую фазу, а общий лишь ограничивает прогон целиком.
const T_TOTAL = 300000;  // прогон целиком

// Ширины. 320 — узкий телефон, 390 — обычный, 1440 — десктоп: брейкпоинт
// 900px переносит слот джоба в .side и раскладывает .wrap в три колонки,
// то есть за ним живёт другая вёрстка, а не та же в увеличенном виде.
const WIDTHS = [320, 390, 1440];

// Состояния. scenario — имя из SCENARIOS мока (web/mock-server.mjs), rm —
// эмуляция prefers-reduced-motion: reduce, hash — что развёрнуто в аккордеоне
// на узком экране (продуктовая возможность: #uplink разворачивает раздел,
// #all — все).
//
// Вкладок больше нет: панель — один экран, на широком сеткой, на узком
// аккордеоном. Поэтому исчез и признак tab, а вместо него появился hash:
// на узком экране свёрнутая секция не измеряется никак, и ошибки переполнения
// прятались бы в ней от C2 — ровно тот слепой угол, который проект уже
// однажды оплатил.
//
// Пары «покой/занята» повторяются с rm намеренно: в @media
// (prefers-reduced-motion: reduce) панель гасит анимации, и если правило
// заодно тронет размеры, слот перестанет держать высоту ровно у тех
// владельцев, которые попросили меньше движения.
const STATES = [
	{ key: 'calm', scenario: 'single', hash: 'all', rm: false },
	{ key: 'busy', scenario: 'job', hash: 'all', rm: false },
	{ key: 'calm-rm', scenario: 'single', hash: 'all', rm: true },
	{ key: 'busy-rm', scenario: 'job', hash: 'all', rm: true },
	// Провал операции: слот показывает его на месте «занята».
	{ key: 'job-fail-short', scenario: 'job-fail-short', hash: 'all', rm: false },
	{ key: 'job-fail-long', scenario: 'job-fail-long', hash: 'all', rm: false },
	// Блок провала переключения сети — под карточкой аплинка.
	{ key: 'upstream-fail-seen', scenario: 'upstream-fail-seen', hash: 'all', rm: false },
	{ key: 'upstream-fail-long', scenario: 'upstream-fail-long', hash: 'all', rm: false },
	{ key: 'upstream-job-long', scenario: 'upstream-job-long', hash: 'all', rm: false },
	{ key: 'upstream-long-list', scenario: 'upstream-long-list', hash: 'all', rm: false },
	{ key: 'ambiguous', scenario: 'ambiguous', hash: 'all', rm: false },
	{ key: 'single-long', scenario: 'single-long', hash: 'all', rm: false },
	// Полоса аномалии над карточками: включённый проброс с молчащим шлюзом.
	{ key: 'bridge-on', scenario: 'bridge-on', hash: 'all', rm: false },
	{ key: 'bridge-off', scenario: 'bridge-off', hash: 'all', rm: false },
	// Аккордеон: свёрнуто всё и раскрыт один раздел. На широком экране hash
	// не значит ничего — там секции раскрыты всегда, — и это проверяет C8.
	{ key: 'acc-closed', scenario: 'single', hash: '', rm: false },
	{ key: 'acc-uplink', scenario: 'single', hash: 'uplink', rm: false },
	// #bridge с ADR-0042 ведёт на экран настроек, у которого нет баннера; на
	// главной раскрытый раздел проверяется на карточке движка.
	{ key: 'acc-engine', scenario: 'bridge-on', hash: 'engine', rm: false },
	// Наблюдатель (ADR-0043). Третий экран, и баннера у него нет — значит из
	// восьми критериев к нему относятся два: C2 (горизонтальной прокрутки
	// нет) и C7 (пустых областей не бывает). Именно C2 здесь и нужен: это
	// единственная в панели таблица, у которой на широком экране бывает своя
	// прокрутка, и утечка её наружу уносит вбок весь документ.
	{ key: 'watch', scenario: 'watch-session', hash: 'watch', rm: false },
	{ key: 'watch-pick', scenario: 'watch-pick', hash: 'watch', rm: false },
	{ key: 'watch-rm', scenario: 'watch-session', hash: 'watch', rm: true },
];

const REST = 'calm';
const BUSY = 'busy';

// Состояния с блоком провала ПЕРЕКЛЮЧЕНИЯ ВНЕШНЕЙ СЕТИ. Только они годятся
// под C3′/C4′, и это не сужение критерия, а его единственное осмысленное
// прочтение: блок стоит НИЖЕ карточки аплинка и потому не может её двигать.
const UPSTREAM_FAIL = ['upstream-fail-seen', 'upstream-fail-long'];

// С ЧЕМ сравнивать. Таблица сохраняется дословно вместе с доводом — это
// самая ценная мысль файла.
//
// Критерий вида «А отличается от Б» осмыслен, только когда А и Б различаются
// РОВНО ОДНИМ признаком — тем, который он проверяет. upstream-fail-long
// отличался от базы двумя: блоком провала И длиной ssid в шапке. Длинный
// ssid переносил шапку на вторую строку и растил то, что стоит НАД
// карточками, — и критерий падал на росте, которого блок провала не вызывал.
// Отсюда правило: длинные состояния сравниваем с длинной базой, короткие —
// с короткой.
const BASE = {
	'upstream-fail-seen': REST,
	'upstream-fail-long': 'single-long',
};

// Объявленные движения. Пусто — значит ни одно состояние не имеет права
// двигать якоря относительно своей базы.
//
// Список ЗАКРЫТЫЙ и работает в обе стороны, как UNIMPLEMENTED в
// check-routes.sh: запись, движения не вызывающая, тоже отказ. Иначе
// освобождение переживает свою причину и молча гасит критерий — ровно то,
// чем было прежнее «job-fail-* не подпадают под C4».
const MOVES = {};

// ─────────── что именно меряем ───────────
//
// Один Runtime.evaluate на замер: два вызова подряд разделены сетью и
// событийным циклом страницы, и между ними успевает пройти тик опроса
// (1 Гц) — половина чисел оказалась бы от одной разметки, половина от
// следующей.
//
// СТРУКТУРНЫЕ АССЕРТЫ ИДУТ ДО ЗАМЕРА и возвращают ok:false с текстом. Без них
// скрипт зелен оттого, что померил не то: на первом кадре в сетке лежит
// карточка-скелетон, и все числа снялись бы с загрузочного экрана.
//
// ОБРАЩЕНИЕ К DOM — ТОЛЬКО через data-part, data-anchor и состояния ARIA.
// Ни классов, ни регекспов по заголовкам, и оба запрета не про вкус:
//   • классы под бандлером принадлежат сборке, а не разметке;
//   • регексп по заголовку («Внешняя сеть|Uplink network») — это структурное
//     утверждение, привязанное к ЛОКАЛИ: оно ломается в день, когда кто-то
//     улучшит русский текст, и падение будет означать не то, что написано.
// data-part — тождество («это баннер»), data-anchor — участие в критерии
// («это не должно двигаться»). Два атрибута, а не один: иначе новый якорь
// требовал бы переименования части.
const MEASURE = `(() => {
	const q = (s) => document.querySelector(s);
	const qa = (s) => Array.from(document.querySelectorAll(s));

	// Экран наблюдателя (ADR-0043) баннера не имеет, и это не дефект: он
	// отвечает не на «что сейчас с роутером», а на «с кем говорит
	// устройство». Поэтому структурные утверждения главной к нему не
	// применяются, а из критериев остаются те два, что от баннера не
	// зависят, — C2 и C7. Признак — сама разметка, а не поле в STATES:
	// состояние, забывшее объявить свой экран, молча мерило бы не то.
	const watch = q('[data-part="watch"]');

	const hero = q('[data-part="hero"]');
	if (!watch && !hero) return { ok: false, why: 'нет баннера [data-part=hero]' };

	const slot = q('[data-part="hero-status"]');
	if (!watch && !slot) return { ok: false, why: 'нет слота [data-part=hero-status]' };
	const st = slot ? slot.dataset.status : 'calm';
	if (!watch && !['busy', 'result', 'calm'].includes(st)) {
		return { ok: false, why: 'у слота неизвестное состояние: ' + JSON.stringify(st) };
	}

	const grid = q('[data-part="grid"]');
	if (!watch && !grid) return { ok: false, why: 'нет сетки [data-part=grid]' };

	const cards = qa('[data-part="card"]');
	if (!watch && cards.length < 1) return { ok: false, why: 'в сетке нет ни одной карточки' };

	// Разложено, но не оформлено — новая опасность, которую внесла сборка.
	// CSS теперь отдельным файлом, и кадр может быть структурно готов, а
	// стилей ещё нет: settle() с его двумя одинаковыми снимками вернул бы
	// такой кадр не поморщившись, а числа с неоформленной страницы — мусор.
	const painted = watch || hero;
	const heroBg = getComputedStyle(painted).backgroundColor;
	if (!watch && (!heroBg || heroBg === 'rgba(0, 0, 0, 0)' || heroBg === 'transparent')) {
		return { ok: false, why: 'стили ещё не применились (фон баннера прозрачен)' };
	}
	// У наблюдателя фон красит не он сам, а оболочка; доказательством
	// оформления служит собственный отступ экрана — он задан только в CSS.
	if (watch && parseFloat(getComputedStyle(watch).paddingTop) === 0) {
		return { ok: false, why: 'стили ещё не применились (у экрана наблюдателя нет отступа)' };
	}

	// C7 — пустых областей не бывает.
	//
	// Это перевод задокументированной ловушки из протокола в гейт.
	// Совпадающие числа НЕ ловят пустую дыру: призрак держит высоту, число
	// не шелохнётся, а место под переключателем окажется белым. Раньше на
	// это был ответ «посмотрите глазами». Теперь — проверка.
	const filled = (el) => {
		if (!el) return false;
		if ((el.textContent || '').trim().length === 0) return false;
		return Array.from(el.children).some((c) => {
			const r = c.getBoundingClientRect();
			return r.width > 0 && r.height > 0;
		}) || (el.getBoundingClientRect().height > 0);
	};
	const empties = [];
	if (slot && !filled(slot.querySelector(':scope > *:not(.ghost)'))) empties.push('hero-status');
	// У наблюдателя пустой обязана не быть каждая из его собственных
	// областей: таблица, полоса новых и шапка сессии. Пустая таблица здесь —
	// не «устройство молчит», а несработавший экран: молчание показывается
	// отдельным блоком с текстом.
	if (watch) {
		for (const part of ['watch-session', 'watch-table', 'watch-fresh', 'watch-devices']) {
			const el = q('[data-part="' + part + '"]');
			if (el && !filled(el)) empties.push(part);
		}
		if (!filled(watch)) empties.push('watch');
	}
	// Список проблем живёт за колокольчиком (ADR-0042) и раскрыт только по
	// нажатию; раскрытый обязан быть заполнен так же, как карточка.
	const alerts = q('[data-part="alerts"]');
	if (alerts && !filled(alerts)) empties.push('alerts');
	for (const c of cards) if (!filled(c)) empties.push('card:' + (c.dataset.card || '?'));

	// Аккордеон: aria-expanded обязано согласовываться с видимостью панели.
	// Открытая по ARIA, но нулевой высоты — настоящая тихая ошибка, которую
	// не видно ни глазами (панель просто пуста), ни по числам.
	const accHeads = qa('[data-part="accordion-item"]');
	const accLies = [];
	for (const h of accHeads) {
		const open = h.getAttribute('aria-expanded') === 'true';
		const id = h.getAttribute('aria-controls');
		const panel = id ? document.getElementById(id) : null;
		const shown = !!panel && panel.getBoundingClientRect().height > 0;
		if (open !== shown) accLies.push((h.textContent || '').trim().slice(0, 24) + ': aria=' + open + ', видно=' + shown);
	}

	const top = (el) => el.getBoundingClientRect().top;

	// Якоря. Обобщение прежней «первой карточки»: тот замер видел сдвиг
	// только у card[0], и прыжок чего угодно ниже был ему невидим.
	const anchors = qa('[data-anchor]').map((el) => ({
		id: el.getAttribute('data-anchor'),
		top: top(el),
	}));
	if (!watch && anchors.length < 2) {
		return { ok: false, why: 'якорей [data-anchor] нашлось ' + anchors.length + ', ожидалось не меньше двух' };
	}

	const desc = (el) => {
		const part = el.getAttribute('data-part');
		if (part) return '[data-part=' + part + ']';
		const sel = el.tagName.toLowerCase() + Array.from(el.classList).map((c) => '.' + c).join('');
		const same = qa(sel);
		const i = same.indexOf(el);
		return sel + (same.length > 1 ? '[' + i + ']' : '');
	};

	// Узлы, у которых содержимое шире их самих. Это НЕ критерий, а число
	// в отчёт: перенос кнопок в строке объявлен осознанной ценой, запрещать
	// его нельзя — но и оставлять неизмеренным нельзя, иначе цена растёт
	// молча. Набор фиксирован ради сравнимости от прогона к прогону и
	// выражен через data-part, а не через классы оформления.
	const SEL = '[data-part=card], [data-part=hero], [data-part=problem], [data-part=confirm], .row, .note, .hint';
	const overflowers = qa(SEL)
		.filter((el) => el.scrollWidth > el.clientWidth + 1)
		.map((el) => ({ node: desc(el), scrollW: el.scrollWidth, clientW: el.clientWidth,
			text: (el.textContent || '').trim().slice(0, 48) }));

	// Кто именно уносит документ вправо. Механика сохранена дословно: это
	// лучшая часть прежнего файла. Признак «уносит» — собственная рамка за
	// краем окна, а не scrollWidth: у html и body он тоже больше clientWidth,
	// но они лишь наследуют чужую беду. Оставляем самые глубокие: их предки
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
		// Сначала узлы С ТЕКСТОМ: они называют причину, а пустая коробка —
		// только следствие, уехавшее вместе с ней.
		.sort((a, b) => {
			const ta = (a.textContent || '').trim() ? 0 : 1;
			const tb = (b.textContent || '').trim() ? 0 : 1;
			return ta - tb || b.getBoundingClientRect().right - a.getBoundingClientRect().right;
		})
		.map((el) => {
			const r = el.getBoundingClientRect();
			return {
				node: desc(el), right: Math.round(r.right), width: Math.round(r.width),
				inside: shell(el), text: (el.textContent || '').trim().slice(0, 48),
			};
		});

	return {
		ok: true,
		screen: watch ? 'watch' : 'main',
		slotStatus: st,
		anchors,
		card1Top: cards[0] ? top(cards[0]) : null,
		gridBottom: grid ? grid.getBoundingClientRect().bottom : 0,
		// Высота БАННЕРА, а не слота: резервируется теперь именно она, и
		// только под «занята» (C5′).
		heroH: hero ? hero.getBoundingClientRect().height : 0,
		slotH: slot ? slot.getBoundingClientRect().height : 0,
		scrollH: document.documentElement.scrollHeight,
		// Сколько дорожек в сетке — так проверяется, что перелом раскладки
		// вообще что-то делает (C8). Прежде этого не доказывало ничто.
		gridTracks: grid ? getComputedStyle(grid).gridTemplateColumns.split(' ').filter(Boolean).length : 1,
		accordions: accHeads.length,
		accLies,
		empties,
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
		['card1Top', 9], ['gridBot', 9], ['heroH', 8], ['slotH', 7],
		['tracks', 7], ['acc', 5], ['docW', 6], ['winW', 6], ['overflow', 9],
	];
	for (const w of WIDTHS) {
		console.log(`\n── ширина ${w}px ` + '─'.repeat(58));
		console.log(pad('состояние', 20) + cols.map(([c, n]) => padL(c, n)).join(''));
		for (const st of STATES) {
			const m = results[w][st.key];
			const cells = [
				num(m.card1Top), num(m.gridBottom), num(m.heroH), num(m.slotH),
				String(m.gridTracks), String(m.accordions),
				String(m.docW), String(m.winW), String(m.overflowers.length),
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
	const NARROW = WIDTHS.filter((w) => w < 900);
	const WIDE = WIDTHS.filter((w) => w >= 900);

	// Сравнение якорей: одинаковы ли позиции ВСЕХ якорей между двумя
	// состояниями. Возвращает список расхождений.
	const anchorDiff = (a, b) => {
		const ma = new Map(a.anchors.map((x) => [x.id, x.top]));
		const mb = new Map(b.anchors.map((x) => [x.id, x.top]));
		const out = [];
		for (const [id, ta] of ma) {
			if (!mb.has(id)) { out.push(`${id}: есть в первом, нет во втором`); continue; }
			const tb = mb.get(id);
			if (ta !== tb) out.push(`${id}: ${num(ta)} ≠ ${num(tb)} (Δ${num(tb - ta)})`);
		}
		for (const id of mb.keys()) if (!ma.has(id)) out.push(`${id}: нет в первом, есть во втором`);
		return out;
	};

	// C1′ — анкерная устойчивость. Обобщение прежнего C1 и строго сильнее
	// него: тот смотрел на верх ПЕРВОЙ карточки, и прыжок чего угодно ниже
	// был ему невидим. Точное равенство, а не «примерно»: расхождение в
	// пиксель означает, что призрак собран не из того, что замещает, — то
	// есть прыжок вернётся, просто уменьшенный.
	//
	// СРАВНИВАЮТСЯ НЕ ВСЕ ЯКОРЯ, и это не послабление. Пара «покой/занята» —
	// это два РАЗНЫХ сценария мока, и различаются они не только идущей
	// операцией: в одном движок отвечает списком из тридцати трёх узлов,
	// в другом лежит и рисует скелетон. Позиции разделов НИЖЕ первого
	// зависят от этих данных, и требовать от них совпадения значит мерить
	// фикстуру, а не вёрстку — та самая ошибка «А отличается от Б двумя
	// признаками», которая однажды уже заставила завести таблицу BASE.
	//
	// Поэтому здесь сравнивается ровно то, до чего изменение состояния
	// панели физически достаёт: кнопки режима (они НАД слотом, и двигать их
	// не может ничто) и заголовок первого раздела (он сразу под баннером,
	// и двигается тогда и только тогда, когда баннер вырос). Всё, что ниже,
	// проверяет C5′ через высоту баннера — единственным числом и без
	// зависимости от данных.
	const STABLE = (a) => /^mode-|^sec-uplink$/.test(a);
	const PAIRS = [
		[REST, BUSY],
		[REST + '-rm', BUSY + '-rm'],
	];
	for (const w of WIDTHS) {
		for (const [a, b] of PAIRS) {
			const key = `${a}→${b}`;
			const diff = anchorDiff(R(w, a), R(w, b)).filter((d) => STABLE(d.split(':')[0]));
			if (diff.length && !MOVES[key]) {
				bad.push(`C1′: ${w}px, ${key} — якоря сдвинулись: ${diff.join('; ')}`);
			}
			if (!diff.length && MOVES[key]) {
				// Освобождение, пережившее свою причину, — это молча
				// погашенный критерий. Та же растяжка, что у UNIMPLEMENTED.
				bad.push(`MOVES: ${w}px, ${key} объявлено движущимся, но якоря не сдвинулись — уберите запись`);
			}
		}
	}

	// C2. Горизонтальной прокрутки быть не должно ни в одном состоянии:
	// на телефоне она уносит вбок весь документ вместе с баннером и шапкой.
	for (const w of WIDTHS) {
		for (const st of STATES) {
			const m = R(w, st.key);
			if (m.docW > m.winW) {
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

	// C3′. Доказательство, что замер физически ВИДИТ появление блока провала.
	// Точка — низ сетки: блок стоит последним, карточек ниже нет, и расти от
	// его появления умеет только он.
	for (const w of WIDTHS) {
		for (const s of UPSTREAM_FAIL) {
			const b = BASE[s];
			if (!(R(w, s).gridBottom > R(w, b).gridBottom)) {
				bad.push(`C3′: ${w}px — gridBottom ${s}=${num(R(w, s).gridBottom)} не больше ${b}=${num(R(w, b).gridBottom)}`);
			}
		}
	}

	// C4′. Измерять вместо освобождения от проверки.
	//
	// Прежний C4 требовал «первая карточка не сдвинулась» и выводил из-под
	// себя целый класс состояний (job-fail-*), потому что там она сдвигается
	// законно. Освобождение — слепое пятно: оно не отличает «сдвинулось
	// объявленное» от «сдвинулось объявленное И заодно что-то выросло».
	//
	// Блок провала переключения встаёт МЕЖДУ разделом аплинка и остальными.
	// Значит правильное утверждение не «ничто не сдвинулось» и не «всё
	// сдвинулось одинаково», а:
	//
	//   • якоря ВЫШЕ блока не сдвинулись вовсе;
	//   • якоря НИЖЕ сдвинулись все на одно и то же;
	//   • и ровно на столько же вырос низ сетки.
	//
	// Последнее условие и есть то, чего освобождение доказать не может: оно
	// отличает «появился блок» от «появился блок И что-то ещё выросло».
	for (const w of WIDTHS) {
		for (const s of UPSTREAM_FAIL) {
			const base = BASE[s];
			const a = R(w, base), b = R(w, s);
			const ma = new Map(a.anchors.map((x) => [x.id, x.top]));
			const deltas = new Map();
			for (const x of b.anchors) if (ma.has(x.id)) deltas.set(x.id, x.top - ma.get(x.id));

			const moved = [...deltas.entries()].filter(([, d]) => d !== 0);
			const uniq = [...new Set(moved.map(([, d]) => d))];
			if (uniq.length > 1) {
				const parts = moved.map(([id, d]) => `${id}:Δ${num(d)}`);
				bad.push(`C4′: ${w}px, ${s} — сдвинувшиеся якоря разъехались (${parts.join(', ')});`
					+ ' значит внутри сетки выросло что-то ещё, кроме блока провала');
				continue;
			}
			if (moved.length === 0) {
				bad.push(`C4′: ${w}px, ${s} — блок провала не сдвинул НИ ОДНОГО якоря:`
					+ ' либо он не появился, либо якорей под ним нет и критерий ничего не мерит');
				continue;
			}
			const shift = uniq[0];
			const grew = b.gridBottom - a.gridBottom;
			// Допуск в полпикселя: обе величины считаются из
			// getBoundingClientRect, и субпиксельное округление у сдвига
			// и у роста происходит в разных местах.
			if (Math.abs(shift - grew) > 0.5) {
				bad.push(`C4′: ${w}px, ${s} — якоря сдвинулись на ${num(shift)}, а низ сетки вырос на ${num(grew)};`
					+ ' разница означает, что вместе с блоком выросло что-то ещё');
			}
		}
	}

	// C5′ — адресная резервация. Заменяет снятый C5 («высота слота — одно
	// число на всю панель»).
	//
	// Инвариантом никогда не была постоянная высота слота: им было «ничто,
	// к чему пользователь тянется, не уезжает из-под пальца». Держать
	// призрак под максимум из трёх состояний значит платить мёртвой полосой
	// постоянно — во всех состояниях, на всех ширинах, — ради тоста, который
	// живёт пять секунд. Поэтому резервируется ТОЛЬКО «занята»: она наступает
	// по клику владельца, длится 5–20 секунд, и именно тогда он тянется
	// к следующему контролу.
	for (const w of WIDTHS) {
		for (const [a, b] of [[REST, BUSY], [REST + '-rm', BUSY + '-rm']]) {
			if (R(w, a).heroH !== R(w, b).heroH) {
				bad.push(`C5′: ${w}px — высота баннера ${a}=${num(R(w, a).heroH)} ≠ ${b}=${num(R(w, b).heroH)};`
					+ ' призрак слота не держит «занята»');
			}
		}
	}

	// C6 — состояния аккордеона измеряются, и ARIA не врёт.
	//
	// Ошибки переполнения прячутся в свёрнутых панелях: прежняя оснастка их
	// не открывала вовсе и не увидела бы никогда.
	for (const w of NARROW) {
		for (const key of ['acc-closed', 'acc-uplink', 'acc-engine']) {
			const m = R(w, key);
			if (m.accordions === 0) {
				bad.push(`C6: ${w}px, ${key} — аккордеона нет вовсе, а на узком экране он обязан быть`);
			}
			if (m.accLies.length) {
				bad.push(`C6: ${w}px, ${key} — aria-expanded расходится с видимостью: ${m.accLies.join('; ')}`);
			}
		}
	}

	// C7 — пустых областей не бывает. Ловит ровно то, чего не ловят числа:
	// призрак держит высоту, замер доволен, а место пустое.
	for (const w of WIDTHS) {
		for (const st of STATES) {
			const m = R(w, st.key);
			if (m.empties.length) {
				bad.push(`C7: ${w}px, ${st.key} — пустые области: ${m.empties.join(', ')}`);
			}
		}
	}

	// C8 — обе раскладки существуют. До сих пор ничто не доказывало, что
	// перелом на 900px вообще что-то делает: числа просто различались, и
	// различаться они могли по любой причине.
	for (const w of WIDE) {
		if (R(w, REST).gridTracks < 2) {
			bad.push(`C8: ${w}px — в сетке ${R(w, REST).gridTracks} дорожка, на широком экране их обязано быть больше`);
		}
		if (R(w, REST).accordions !== 0) {
			bad.push(`C8: ${w}px — на широком экране есть кнопки аккордеона (${R(w, REST).accordions}),`
				+ ' а секции там раскрыты всегда: aria-expanded, которое нельзя изменить, — ложь скринридеру');
		}
	}
	for (const w of NARROW) {
		if (R(w, REST).gridTracks !== 1) {
			bad.push(`C8: ${w}px — в сетке ${R(w, REST).gridTracks} дорожек, на узком экране обязана быть одна`);
		}
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
	// Мок отдаёт СОБРАННУЮ панель (умолчание MOCK_STATIC — web/panel/dist).
	//
	// До сборки исходник и отгружаемое совпадали, и вопроса не было. Теперь
	// у дев-сервера нет ни минификации, ни вынесенного CSS, ни того же
	// разбиения на чанки: мерить на нём — значит мерить бандл, который никто
	// не загрузит. Отсюда же зависимость цели geometry от panel в Makefile.
	const dist = path.join(ROOT, 'web', 'panel', 'dist', 'index.html');
	if (!fs.existsSync(dist)) {
		console.error('measure-geometry: нет собранной панели (web/panel/dist)');
		console.error('  соберите её: make panel');
		process.exit(1);
	}
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
			// настоящую навигацию, а не «мы уже здесь»: смена одного хеша
			// навигацией не считается, и без нонса состояние с той же вкладкой
			// осталось бы на старой странице со старым сценарием.
			const loaded = cdp.wait('Page.loadEventFired', T_SETTLE);
			await cdp.send('Page.navigate', { url: `${base}/?geom=${++nonce}${st.hash ? '#' + st.hash : ''}` });
			await loaded;
			results[w][st.key] = await settle(cdp);
		}
	}

	cdp.close();
	clearTimeout(guard);
	cleanup();

	// 4. Отчёт.
	// GEOM_DUMP=<файл> кладёт все замеры как есть. Не украшение: когда
	// критерий падает на числе, вопрос всегда один — из чего это число
	// сложилось, — и отвечать на него, дописывая временный вывод в скрипт,
	// значит каждый раз заново.
	if (process.env.GEOM_DUMP) fs.writeFileSync(process.env.GEOM_DUMP, JSON.stringify(results, null, 1));
	printTable(results);
	printOverflowers(results);
	printBleeders(results);

	const bad = checkCriteria(results);
	const secs = ((Date.now() - started) / 1000).toFixed(1);
	console.log('');
	if (bad.length === 0) {
		console.log(`measure-geometry: все критерии выполнены (C1′–C8), ${secs} с`);
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
