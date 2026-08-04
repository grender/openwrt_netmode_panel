// Панель netmoded. Preact + htm, без сборки: файл грузится браузером как
// ESM напрямую и зашивается в бинарь через go:embed.
//
// Состояние тянется опросом /api/status раз в секунду — так требует SPEC §7.
// Ни WebSocket, ни SSE вводить нельзя: на роутере они стоят памяти и ничего
// не покупают при одном пользователе в LAN.

import { html, render, useState, useEffect, useRef } from './vendor/htm-preact-standalone.module.js';
import { makeT } from './i18n.js';

const POLL_MS = 1000;

// Бюджеты ожидания. Один общий не годится: скан эфира штатно идёт секунды,
// а опрос статуса обязан уложиться в интервал опроса, иначе запросы копятся.
const T_STATUS = 4000;
const T_SIDE = 8000;
const T_SCAN = 20000;
const T_MODE = 8000; // 202 приходит сразу, ждать завершения джоба тут нечего
const T_DEFAULT = 8000;

// Запас поверх eta_sec, после которого засеянный джоб считается протухшим.
// Десять секунд: демон обещает eta приблизительно, а смена режима штатно
// рвёт туннель — пара пропущенных опросов тут норма, а не авария.
const SEED_SLACK = 10000;

// Сколько ждём слушателя службы, прежде чем сказать «не отвечает». Демон
// коммитит UCI mode ДО того, как служба поднялась (internal/httpapi/
// modehandler.go:77-86), поэтому первые секунды нового режима — штатный старт,
// а не отказ. Двадцать секунд: firewall restart плюс запуск b4/nikki на живом
// роутере укладываются в единицы секунд, остальное — запас на холодный старт
// и медленную флешку. Меньше — и панель начнёт врать про исправный роутер,
// больше — и настоящая поломка будет полминуты притворяться загрузкой.
const START_GRACE = 20000;

// Как часто досылать запрос списка, если служба отвечает, а список не доехал.
// Пять секунд, а не каждый тик: это починка редкой гонки, и она не стоит
// секундного долбления роутера, которое на медленном канале само себе мешает.
const RETRY_MS = 5000;

// ─────────── утилиты ───────────

// Таймаут обязателен на каждом запросе: зависший (именно зависший, а не
// отклонённый) fetch навсегда оставил бы busy взведённым, а он блокирует все
// кнопки панели до перезагрузки страницы.
//
// AbortController + setTimeout, а не AbortSignal.timeout(): второй новее и
// может отсутствовать в браузере владельца, а полифиллов у панели нет по
// определению — она грузится в браузер как есть, без сборки.
const api = async (path, opts, timeoutMs = T_DEFAULT) => {
	const ctl = new AbortController();
	const timer = setTimeout(() => ctl.abort(), timeoutMs);
	try {
		// signal стоит ПОСЛЕ ...opts намеренно: вызывающий со своим signal
		// иначе перезаписал бы наш, и таймаут молча исчез бы. Таких
		// вызывающих сейчас нет, но порядок здесь значим.
		const r = await fetch(path, {
			headers: { 'Content-Type': 'application/json' },
			...opts,
			signal: ctl.signal,
		});
		const text = await r.text();
		let body = null;
		try { body = text ? JSON.parse(text) : null; } catch { /* не-JSON оставляем как null */ }
		if (!r.ok) {
			const err = new Error((body && body.error) || r.statusText);
			err.status = r.status;
			err.code = body && body.code;
			throw err;
		}
		return body;
	} finally {
		// Снимаем в finally: иначе после быстрого ответа таймер ещё секунды
		// живёт и дёргает abort уже завершённого запроса.
		clearTimeout(timer);
	}
};

const fmtTime = (iso, lang) => {
	if (!iso) return '—';
	const d = new Date(iso);
	if (isNaN(d)) return '—';
	return d.toLocaleString(lang === 'en' ? 'en-GB' : 'ru-RU', {
		day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit',
	});
};

// Пороги задержки — контракт из макета, а не украшение.
const latClass = (ms) => (ms == null ? 'lat-bad' : ms < 55 ? 'lat-ok' : ms < 110 ? 'lat-warn' : 'lat-bad');
const latBg = (ms) => (ms == null ? 'bg-bad' : ms < 55 ? 'bg-ok' : ms < 110 ? 'bg-warn' : 'bg-bad');
// Мёртвый узел — пустая полоска, а не полная. Полная читалась бы как
// «задержка максимальная», то есть узел живой и очень медленный; это
// прямо противоположно тому, что произошло.
const latPct = (ms) => (ms == null ? '0%' : Math.min(100, Math.round((ms / 220) * 100)) + '%');

const signalClass = (dbm) => (dbm >= -60 ? 'lat-ok' : dbm >= -75 ? 'lat-warn' : 'lat-bad');

// ─────────── ожидание ───────────

// Компонент, а не готовая vnode-константа: preact мутирует vnode при вставке
// (_dom), и один объект, отрисованный в двух местах, ломается.
const Spin = () => html`<i class="spin" aria-hidden="true"></i>`;

// Занят ли ИМЕННО этот элемент. Ключ занятости несёт вид операции и её
// аргумент: без аргумента панель знает, что что-то идёт, но не знает, на
// какой строке рисовать кольцо, — а до сих пор нажатие на узел Nikki и на
// сет b4 не меняло вообще ни одного пикселя.
const on = (busy, kind, arg) => busy === (arg == null ? kind : kind + ':' + arg);

// wrap задаётся параметром, а не зашит: .rows — колонка, .sets — строка
// с переносом, .log — колонка со своим отступом. Захардкоженный .rows
// разложил бы пилюли b4 вертикально, а при подстановке данных они прыгнули
// бы в строку — то самое дёрганье, ради которого скелетон и рисуют.
//
// aria-hidden: озвучивать нечего, за состояние отвечает aria-busy на
// карточке; болтливая заглушка хуже молчаливой.
const Skel = ({ n = 3, cls = '', wrap = 'rows' }) => html`
	<div class=${wrap} aria-hidden="true">
		${Array.from({ length: n }, () => html`<div class="skel ${cls}"></div>`)}
	</div>`;

// ─────────── корневой компонент ───────────

function App() {
	const [lang, setLang] = useState(() => localStorage.getItem('netmode.lang') || 'ru');
	const [status, setStatus] = useState(null);
	const [stale, setStale] = useState(false);
	// undefined, а не null: «ещё не спрашивали» и «спросили, не ответили» —
	// разные ответы, и рисуются они по-разному. Отказ в grab пишет сюда
	// именно null, поэтому различие достаётся даром от начального значения,
	// без второго флага. До этого карточки на первом кадре показывали
	// «Clash API не отвечает» и «Панель b4 не отвечает» — до восьми секунд
	// уверенной неправды про исправный роутер.
	const [nikki, setNikki] = useState();
	const [sets, setSets] = useState();
	const [nets, setNets] = useState();
	const [scan, setScan] = useState(null);
	const [logs, setLogs] = useState();
	const [busy, setBusy] = useState('');
	const [sheet, setSheet] = useState(null);
	const [toast, setToast] = useState(null);
	const tRef = useRef(null);
	// Джоб из ответа 202 — до того, как о нём узнает опрос. Ref, а не только
	// состояние: tick создаётся один раз в эффекте с пустыми зависимостями
	// и до состояния из своего замыкания не дотянется.
	const [seed, setSeed] = useState(null);
	const seedRef = useRef(null);
	const inFlight = useRef(false);
	// Настоящий мьютекс действий. Состояние busy для этого не годится: между
	// setBusy и следующим рендером второе нажатие успевает проскочить.
	const busyRef = useRef('');
	// У панели Nikki свой сторож: через act её вести нельзя — window.open
	// обязан остаться синхронным с жестом пользователя.
	const panelRef = useRef(false);
	// Докуда служба вправе молчать, не считаясь мёртвой. Ключ в том, ОТ ЧЕГО
	// ведётся отсчёт: от джоба, которым службу попросили подняться, а не от
	// момента, когда панель заметила молчание.
	//
	// Отсчёт от первого молчания выглядел правдоподобно и не работал вовсе.
	// netmode-apply гасит вторую службу всегда (files/usr/local/bin/
	// netmode-apply:129, `svc b4 stop` безусловно), поэтому в режиме nikki
	// статус отдаёт b4.available:false ПОСТОЯННО — метка вставала на первом
	// же тике опроса и к моменту нажатия давно протухала. Грация доставалась
	// ровно тому, кто открыл панель и переключил режим в пределах двадцати
	// секунд; на всех остальных путях штатный старт с первой секунды называли
	// отказом. Обратная ошибка ничем не лучше: над давно лежащей службой
	// панель двадцать секунд писала «запускается», хотя ничего не запускалось,
	// — и откладывала правду ровно тогда, когда её пришли узнать.
	//
	// Хранит либо 'running' (джоб идёт прямо сейчас, срок не тикает), либо
	// метку времени, до которой ждём слушателя. Ноль — «ждать нечего».
	const grace = useRef({ b4: 0, nikki: 0 });

	// Производное состояние службы: 'up' | 'starting' | 'down'. Пишется прямо
	// в рендере, поэтому useRef, а не useState: setState отсюда — это setState
	// во время рендера. Переходы идемпотентны, повторный рендер с тем же job
	// ничего не сдвигает.
	const svcState = (name, up, job, stale) => {
		const g = grace.current;
		// Джоб смены режима на этот движок — единственное доказательство, что
		// запуск идёт. Ищем и в статусе, и в засеве: засев кладётся из ответа
		// 202, то есть момент нажатия известен на первом же кадре, до того как
		// о джобе узнает опрос.
		const mine = job && job.kind === 'mode' && job.arg === name ? job : null;
		if (mine && mine.state === 'running') {
			// Пока джоб идёт, срок не начисляется: 5–15 секунд смены режима
			// не должны съедать время, отведённое на подъём слушателя.
			g[name] = 'running';
		} else if (mine && mine.state === 'failed') {
			// Провал — доказательство, что запуск НЕ идёт, поэтому срок
			// снимается, а не досчитывается. Без этой ветки один экран
			// одновременно утверждал «не удалось переключиться в b4» и «b4
			// запускается»; демон убирает завершённый джоб через пять секунд
			// (job.Manager, keepFinished), и ложная половина переживала
			// красную плашку ещё на пятнадцать.
			g[name] = 0;
		} else if (g[name] === 'running') {
			// Джоб этого движка только что перестал идти — вот отсюда и
			// начинается ожидание слушателя. Ветка идемпотентна: со второго
			// рендера g[name] уже число, и срок не продлевается.
			g[name] = Date.now() + START_GRACE;
		}

		if (up) { g[name] = 0; return 'up'; }
		// Устаревший статус — не доказательство запуска: свежих подтверждений
		// у нас нет ни одного, а «запускается» это утверждение о происходящем
		// прямо сейчас. Заодно это единственный выход из замёрзшего ожидания:
		// при падающем опросе setStatus не зовётся вовсе, Date.now() в рендере
		// читать некому, и без этой строки скелетон висел бы вечно. stale
		// переключается один раз, и одного рендера ровно хватает.
		if (stale) return 'down';
		if (g[name] === 'running') return 'starting';
		return g[name] > Date.now() ? 'starting' : 'down';
	};

	const t = makeT(lang);
	useEffect(() => { localStorage.setItem('netmode.lang', lang); document.documentElement.lang = lang; }, [lang]);

	// Опрос статуса. Провал не гасит панель: показываем последнее известное
	// состояние и честно помечаем его устаревшим — пустой экран в момент,
	// когда связь пропала, бесполезен именно тогда, когда нужен больше всего.
	useEffect(() => {
		let alive = true;
		const tick = async () => {
			// Потолок засева проверяется ДО сторожа и до запроса, поэтому
			// работает и когда опрос падает. Без него засев пережил бы свой
			// джоб: смена режима рвёт туннель, следующий опрос может не дойти
			// вовсе — и панель осталась бы запертой навсегда, с полоской на
			// 99% и мёртвыми кнопками до перезагрузки страницы. Раньше такого
			// не было, потому что джоб приходил только из статуса; засев эту
			// дыру и открывает, здесь она и закрывается.
			const sd = seedRef.current;
			if (sd && Date.now() - new Date(sd.started_at) > (sd.eta_sec || 15) * 1000 + SEED_SLACK) {
				seedRef.current = null; setSeed(null);
			}
			// T_STATUS больше POLL_MS: без сторожа на медленном канале висит
			// до четырёх запросов разом, и поздний ответ затирает более свежий.
			if (inFlight.current) return;
			inFlight.current = true;
			try {
				const s = await api('/api/status', null, T_STATUS);
				if (!alive) return;
				const cur = seedRef.current;
				// Ответ статуса, начатый ДО нажатия, о нашем джобе знать не мог:
				// снимать по нему засев значило бы погасить блок и через долю
				// секунды зажечь снова. Обе метки времени приходят с роутера,
				// поэтому сравнение честное. Иначе снимаем: либо опрос догнал
				// (дальше ведёт он), либо джоб кончился быстрее опроса.
				if (cur && ((s.job && s.job.id === cur.id)
					|| new Date(s.generated_at) >= new Date(cur.started_at))) {
					seedRef.current = null; setSeed(null);
				}
				setStatus(s); setStale(false);
			} catch {
				if (alive) setStale(true);
			} finally {
				inFlight.current = false;
			}
		};
		tick();
		const id = setInterval(tick, POLL_MS);
		return () => { alive = false; clearInterval(id); };
	}, []);

	// Побочные данные тянем реже: они меняются от действий, а не сами.
	//
	// Раньше тут был один reloadSide, и он перечитывал все четыре списка после
	// любого действия — внутри окна занятости, то есть держал запертой всю
	// панель до восьми секунд ради данных, которых действие не касалось.
	const grab = (p, set) => api(p, null, T_SIDE).then(set).catch(() => set(null));
	const loadNikki = () => grab('/api/nikki/proxies', setNikki);
	const loadSets = () => grab('/api/b4/sets', setSets);
	const loadNets = () => grab('/api/wifi/networks', setNets);
	const loadLogs = () => grab('/api/logs?n=5', setLogs);

	useEffect(() => { loadNets(); loadLogs(); }, []);
	// Узлы Nikki существуют, только когда поднят Nikki, сеты — когда поднят b4.
	//
	// Триггер перечитывания — доступность службы, а НЕ смена режима.
	// status.<svc>.available буквально значит «служба ответила по HTTP менее
	// полусекунды назад» (статус зовёт её каждый тик, кэш 500 мс), и это строго
	// лучший признак готовности, чем завершение джоба: netmode-apply считает
	// готовностью факт `/etc/init.d/b4 running` (files/usr/local/bin/
	// netmode-apply:118-121, 222-224) — то есть процесс, а не слушателя.
	//
	// По одному лишь mode эффект срабатывал ровно один раз, и срабатывал рано:
	// демон коммитит UCI mode ДО запуска службы, так что единственное
	// перечитывание попадало в connection refused, grab писал null — и карточка
	// печатала «не отвечает» до перезагрузки страницы, потому что mode больше
	// не менялся. Подпись баннера при этом чинилась сама (она смотрит на тот же
	// available), и расхождение выглядело как случайный глюк, а не как дефект.
	//
	// Зависимости — примитивы: [status] пересоздаётся каждым тиком опроса и дал
	// бы два запроса в секунду навсегда, а [status.mode] уронил бы первый
	// рендер, где status ещё null.
	const mode = status && status.mode;
	const b4Up = !!(status && status.b4 && status.b4.available);
	const nikkiUp = !!(status && status.nikki && status.nikki.available);
	// setSets(undefined), а не null — это возврат к «ещё не спрашивали»
	// (скелетон), а не к «спросили и получили отказ». Договорённость трёх
	// состояний выше обязана оставаться дословно верной: сложи эти два
	// значения в одно, и панель снова начнёт печатать отказ там, где ещё
	// ничего не спрашивала.
	useEffect(() => { if (mode === 'b4') { b4Up ? loadSets() : setSets(undefined); } }, [mode, b4Up]);
	useEffect(() => { if (mode === 'nikki') { nikkiUp ? loadNikki() : setNikki(undefined); } }, [mode, nikkiUp]);

	// Досылка при противоречии: служба отвечает, а списка нет. Само по себе
	// это не редкость — /api/b4/sets бьёт по живому b4 без кэша, а available
	// приходит из снимка статуса с кэшем 500 мс (StatusCacheTTL), так что
	// достаточно службе моргнуть внутри окна запроса, чтобы grab записал null,
	// а b4Up для клиента ни разу не стал false. Эффект выше при этом не
	// сработает НИКОГДА: [mode, b4Up] не изменились. Владелец оставался перед
	// «Панель b4 не отвечает» рядом с живой синей кнопкой b4 в шапке, и выхода
	// не было вовсе — в состоянии отказа кнопок сетов не рисуется, значит
	// onPick недостижим, значит и перечитывание после действия недостижимо.
	//
	// Ретраем работает сам опрос, а не свой таймер: у панели уже есть ровно
	// один источник времени, и второй развёл бы их по фазе. Поэтому [status] —
	// он пересоздаётся каждым тиком, и это здесь не дефект, а движок. Что
	// делает такую зависимость безопасной — retryAt: без него получились бы
	// обещанные два запроса в секунду навсегда.
	const retryAt = useRef({ b4: 0, nikki: 0 });
	useEffect(() => {
		const now = Date.now();
		const again = (name, up, data, load) => {
			if (!up || data !== null || now < retryAt.current[name]) return;
			retryAt.current[name] = now + RETRY_MS;
			load();
		};
		if (mode === 'b4') again('b4', b4Up, sets, loadSets);
		if (mode === 'nikki') again('nikki', nikkiUp, nikki, loadNikki);
	}, [status, mode, b4Up, nikkiUp, sets, nikki]);

	// extra добавляет к тосту запасную ссылку (href + cta). Она попадает
	// в состояние вместе с секретом, поэтому живёт ровно до таймаута тоста
	// или до клика по ней — см. dropToast ниже.
	const flash = (msg, kind = 'err', extra = null) => {
		setToast({ msg, kind, ...extra });
		clearTimeout(tRef.current);
		tRef.current = setTimeout(() => setToast(null), 6000);
	};
	const dropToast = () => { clearTimeout(tRef.current); setToast(null); };

	// Адрес панели Nikki берётся отдельным запросом и только по клику.
	//
	// «Запросить URL заранее, при монтировании, чтобы клик был синхронным» —
	// отвергнуто намеренно, не забыто. В этом адресе лежит api_secret, а он
	// же пароль веб-морды Nikki (docs/recon/raw/71-luci-nikki-open-dashboard.txt).
	// Запрошенный заранее, он висел бы в состоянии панели и в атрибуте href
	// всё время, пока страница открыта, — у любого, кто её открыл, и в любом
	// снимке DOM, который снимет расширение браузера. Ровно поэтому секрета
	// нет и в /api/status, который опрашивается раз в секунду. Плата за
	// решение — один запрос на клик, единицы миллисекунд по LAN.
	//
	// Вкладку открываем безусловно: кнопка теперь рисуется только тогда, когда
	// адрес известен и Clash API отвечает, — то есть промах здесь редкость,
	// а не штатный ход. Но именно редкость, а не невозможность: живой Clash API
	// ещё не означает открываемой панели (нет api_secret; дашборд zashboard
	// качается при первом запуске), поэтому отказ ниже разбирается по коду
	// и остаётся жёлтым объяснением, а не красным сбоем.
	const nikkiPanel = async () => {
		// window.open ДО await — это главное здесь. После await пользовательский
		// жест уже израсходован, и блокировщик всплывающих окон закроет вкладку.
		// Сам LuCI делает это неправильно: зовёт window.open в setTimeout после
		// await (тот же файл recon), и его кнопка «Open Dashboard» выживает
		// только там, где блокировщик выключен.
		//
		// Третьим аргументом 'noopener' не передаём: по спецификации window.open
		// тогда возвращает null, и мы уходили бы в ветку «заблокировано» на
		// каждом клике в исправном браузере. Связь с открывшей страницей рвём
		// присваиванием w.opener = null.
		// Свой сторож, а не act: во-первых, замок act запретил бы объяснение
		// во время любой другой операции, во-вторых, act асинхронен и увёл бы
		// window.open за пределы жеста. Двойной клик по живой ссылке иначе
		// открывает две вкладки и шлёт два запроса.
		if (panelRef.current) return;
		panelRef.current = true;
		const w = window.open('', '_blank');
		if (w) w.opener = null;
		// Тост ставится сразу, а не по ответу: круг обращения к серверу длится
		// до восьми секунд по T_SIDE, и всё это время исходная страница обязана
		// показывать, что нажатие принято.
		flash(t('links.opening'), 'info');
		try {
			const r = await api('/api/nikki/panel', null, T_SIDE);
			const url = r && r.url;
			if (!url) { if (w) w.close(); flash(t('links.err')); return; }
			if (w) {
				// replace, а не присваивание location: адрес с секретом не
				// должен оседать записью в истории пустой вкладки.
				w.location.replace(url);
				dropToast();
				return;
			}
			// Вкладки нет — её не дал блокировщик. Единственный оставшийся путь
			// к панели — настоящая <a href> в тосте: клик по ней сам по себе
			// полноценный жест, и блокировщик его не трогает. Секрет уходит
			// в атрибут, но не в текст: подписью служит отдельная строка.
			flash(t('links.blocked'), 'warn', { href: url, cta: t('links.blocked.cta') });
		} catch (e) {
			// Без close() остаётся белая вкладка без единого слова о том, что
			// произошло, а объяснение уезжает в тост на исходной странице.
			if (w) w.close();
			// Три штатные причины — объяснение, а не поломка, и красить их
			// в красный нельзя: роутер исправен, просто открывать пока нечего.
			// Красный остаётся настоящим сбоям (таймаут, 500), иначе цвет
			// перестаёт что-либо значить.
			flash(describe(e, t, 'links.err'), WHY_CODES.has(e.code) ? 'warn' : 'err');
		} finally {
			panelRef.current = false;
		}
	};

	// Обычный левый клик по живой ссылке перехватываем: за адресом с секретом
	// надо сходить на сервер. Клик с модификатором не трогаем — «открыть
	// в новой вкладке», «в новом окне» и «копировать адрес» обязаны работать
	// по-настоящему, и уводят они на адрес без секрета из /api/status. Средняя
	// кнопка сюда не приходит вовсе: она даёт auxclick, а не click.
	const onNikkiOpen = (e) => {
		if (e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
		e.preventDefault();
		nikkiPanel();
	};

	// Одна операция за раз — требование демона (второй джоб получает 409), и до
	// сих пор его держал disabled на кнопках. Из-за этого запрет расползался на
	// всё подряд, включая «Отмену» в форме. Теперь замок здесь, а disabled
	// снова означает только «нажимать бесполезно».
	const act = async (name, fn, after) => {
		// Молча возвращаться нельзя: мёртвая кнопка без объяснения — ровно тот
		// дефект, который чинил 8da43a7. Сообщение то же, что отдал бы демон.
		if (busyRef.current) { flash(t('err.busy')); return; }
		busyRef.current = name; setBusy(name);
		try { await fn(); }
		catch (e) { flash(describe(e, t)); return; }
		finally { busyRef.current = ''; setBusy(''); }
		// Перечитывание — уже вне окна занятости: кнопки отпускаются, когда
		// операция закончилась, а не когда доедут побочные списки.
		if (after) await after();
	};

	// Первый экран. Раньше здесь было голое '…', а при провале — foot.stale
	// («данные устарели»), который тут врал: устаревать было нечему, данных
	// не приходило ни разу.
	//
	// Подпись обязана быть прямым потомком .wrap с классом full: от 900px
	// .wrap — сетка в три колонки, и без него подпись заняла бы первую
	// из четырёх ячеек, а третий скелетон уехал бы во второй ряд. Группы
	// заворачиваются в .card, иначе на месте голых блоков потом появятся
	// рамка и отступы — то есть карточки подпрыгнут.
	if (!status) {
		if (stale) {
			return html`<div class="wrap"><div class="note err full">
				<h3>${t('boot.down.title')}</h3><p>${t('boot.down')}</p></div></div>`;
		}
		return html`<div class="wrap">
			<div class="note info full" style="display:flex;align-items:center;gap:9px">
				<${Spin} /><p>${t('boot')}</p></div>
			${[0, 1, 2].map(() => html`<div class="card"><${Skel} n=${3} /></div>`)}
		</div>`;
	}

	// Не «status.job || seed». Демон держит завершённый джоб в статусе ещё
	// пять секунд (job.go, keepFinished), а Start отбивает только идущий —
	// значит новый джоб успевает стартовать, пока в статусе лежит доигранный.
	// Простое «или» предпочло бы доигранный только что засеянному: полоска
	// не появилась бы, и секундная задержка вернулась бы ровно там, где её
	// чинят. Засев уступает статусу только тогда, когда статус говорит про
	// тот же самый джоб.
	const job = seed && !(status.job && status.job.id === seed.id) ? seed : status.job;
	const running = job && job.state === 'running' ? job : null;
	const failed = job && job.state === 'failed' ? job : null;
	const locked = !!running || !!busy;

	// Считается здесь, а не выше, потому что кормится job — тем самым, который
	// уже разобран строкой выше с учётом засева. Раньше запуска не докажет
	// ничто другое: available у обеих служб штатно false в чужом режиме.
	const svc = {
		b4: svcState('b4', b4Up, job, stale),
		nikki: svcState('nikki', nikkiUp, job, stale),
	};

	return html`
		<${Top} s=${status} t=${t} lang=${lang} setLang=${setLang} onNikkiOpen=${onNikkiOpen} />
		<${Banner} s=${status} t=${t} svc=${svc} running=${running} failed=${failed} busy=${busy} locked=${locked}
			onMode=${(m) => act('mode:' + m, async () => {
				const r = await api('/api/mode', { method: 'POST', body: JSON.stringify({ mode: m }) }, T_MODE);
				// Джоб из 202 — не заглушка: в нём настоящие started_at
				// и eta_sec, поэтому обратный отсчёт стартует верным.
				if (r && r.job) { seedRef.current = r.job; setSeed(r.job); }
			})} />

		<${Toasts} toast=${toast} onCta=${() => setTimeout(dropToast, 0)} />

		<div class="wrap">
			<!-- Причина провала живёт здесь, а не в баннере рядом со своим
			     заголовком. Сообщения демона длинные (internal/httpapi/
			     modehandler.go:129), а слот баннера держит постоянную высоту —
			     внутри него такой текст пришлось бы обрезать, а обрезанное
			     сообщение об ошибке хуже отсутствующего: оно выглядит полным.
			     .note — то место, где сообщения в этой панели уже живут: 14px
			     на --bad-bg во всю ширину, и растёт оно вниз, никуда не толкая
			     карточки. Заголовок остаётся в баннере, прямо над этой
			     строкой, — вместе они читаются как одно сообщение. -->
			${failed && failed.error && html`
				<div class="note err full"><p>${failed.error}</p></div>`}

			<${SelectionNote} s=${status} t=${t} />

			${status.mode === 'nikki' && html`
				<${NikkiCard} data=${nikki} svc=${svc.nikki} t=${t} busy=${busy} locked=${locked}
					onPick=${(n) => act('proxy:' + n, () => api('/api/nikki/proxy', { method: 'POST', body: JSON.stringify({ name: n }) }), loadNikki)}
					onTest=${() => act('test', () => api('/api/nikki/test', { method: 'POST' }), loadNikki)} />`}

			${status.mode === 'b4' && html`
				<${B4Card} data=${sets} svc=${svc.b4} t=${t} busy=${busy} locked=${locked}
					onPick=${(id) => act('set:' + id, () => api('/api/b4/set', { method: 'POST', body: JSON.stringify({ id }) }), loadSets)} />`}

			${status.mode === 'off' && html`
				<div class="card"><h2>${t('mode.off')}</h2>
					<p class="hint">${t('sub.off')}</p></div>`}

			<${WifiCard} nets=${nets} scan=${scan} t=${t} busy=${busy} status=${status}
				onScan=${() => act('scan', async () => setScan(await api('/api/wifi/scan', null, T_SCAN)))}
				onOpenSheet=${(s) => setSheet(s)}
				onDelete=${(n) => {
					if (!confirm(t('wifi.confirm.delete', { ssid: n.ssid }))) return;
					act('del:' + n.id, () => api('/api/wifi/networks/' + encodeURIComponent(n.id), {
						method: 'DELETE',
						headers: { 'If-Match': nets.fingerprint },
					}), loadNets);
				}} />

			<${SubCard} s=${status} logs=${logs} t=${t} lang=${lang} busy=${busy}
				onUpdate=${() => act('sub', async () => {
					const r = await api('/api/subscription/update', { method: 'POST' });
					if (r && r.job) { seedRef.current = r.job; setSeed(r.job); }
				// Обновление подписки — это то, что ПРОИЗВОДИТ список узлов
				// Nikki, поэтому перечитываем и его, а не только журнал.
				}, async () => { await loadLogs(); await loadNikki(); })} />
		</div>

		${sheet && html`
			<${NetworkSheet} sheet=${sheet} t=${t} busy=${busy} locked=${locked}
				onClose=${() => setSheet(null)}
				onSave=${(body, setErr) => act('save', async () => {
					try {
						// Отпечаток обязателен: без него демон отвечает 409.
						// Он же ловит правки, сделанные в LuCI, пока форма
						// была открыта.
						await api('/api/wifi/networks', {
							method: 'POST',
							headers: { 'If-Match': nets && nets.fingerprint },
							body: JSON.stringify(body),
						});
						setSheet(null);
					} catch (e) {
						// Ошибка формы докладывается ровно один раз — строкой
						// в самой форме. Раньше отсюда шёл ещё throw, и act
						// добавлял вторую копию тостом, другими словами.
						//
						// Расхождение отпечатка чинится обновлением списка,
						// а не повтором вслепую: иначе перезапишем чужое.
						// Перечитать обязательно — иначе wifi.err.stale
						// («список обновлён») врёт, и повтор шлёт тот же
						// устаревший отпечаток бесконечно.
						if (e.code === 'fingerprint_mismatch') {
							await loadNets();
							setErr(t('wifi.err.stale'));
						} else if (e.code === 'foreign_staged_changes') {
							setErr(t('wifi.err.foreign'));
						} else {
							setErr(describe(e, t));
						}
					}
				}, loadNets)} />`}

		<${Foot} s=${status} t=${t} lang=${lang} stale=${stale} />
	`;
}

// Машинный код ошибки → ключ словаря.
//
// Разбор идёт по коду, а не по статусу: под 503 ходят и подвисший ubus, и
// нехватка Clash API, и незаданный планировщик, и объяснять их одним текстом
// про Clash — врать владельцу ровно там, где он ищет причину.
const ERR_KEY = {
	ambiguous_selection: 'sel.ambiguous.title',
	enabled_network_readonly: 'wifi.locked',
	job_busy: 'err.busy',
	// Три кода оптимистичной блокировки wireless (ADR-0011). Без них
	// describe() проваливался в e.message, то есть печатал русский текст
	// демона в английском интерфейсе — ровно та утечка, против которой
	// заведён весь этот словарь.
	fingerprint_mismatch: 'wifi.err.stale',
	fingerprint_required: 'wifi.err.stale',
	foreign_staged_changes: 'wifi.err.foreign',
	// Сообщение демона русское (оно же уходит в syslog), поэтому даже там,
	// где текст совпадает по смыслу, панель берёт свой перевод.
	nikki_unavailable: 'srv.down',
	b4_unavailable: 'sets.down',
	b4_partial: 'err.b4.partial',
	ubus_unavailable: 'err.ubus',
	uci_unavailable: 'err.uci',
	scan_failed: 'err.scan',
	ifname_unknown: 'err.ifname',
	radio_unknown: 'err.radio',
	unavailable: 'err.sched',
	// Почему панель Nikki не открыть. Три кода — три разных текста, и это не
	// многословие: хост не выведен → откройте панель по адресу роутера; Nikki
	// не настроен → настройте; веб-морды нет → её нельзя открыть в принципе,
	// есть только Clash API. Один общий текст отправлял бы владельца чинить
	// не то, что сломано.
	host_unknown: 'links.why.host',
	nikki_unconfigured: 'links.why.unconfigured',
	panel_missing: 'links.why.nopanel',
};

// Те же три кода, но как множество: по ним тост красится жёлтым, а не
// красным. Все три означают «открывать нечего», а не «сломалось»: роутер
// исправен, Clash API отвечает — иначе кнопки Nikki в шапке не было бы
// вовсе. Красный остаётся таймаутам и 500, иначе цвет перестаёт значить.
const WHY_CODES = new Set(['host_unknown', 'nikki_unconfigured', 'panel_missing']);

// Сообщение об ошибке объясняет причину, а не показывает код: коды 409
// в этой панели означают три разные вещи, и «409» пользователю не говорит
// ничего.
//
// fallback — ключ запасного текста для мест, где сообщение сервера точно
// не годится: оно приходит по-русски (тот же текст уходит в syslog) и
// в английском интерфейсе читалось бы как утечка бэкенда.
function describe(e, t, fallback) {
	// Прерванный по таймауту запрос даёт DOMException с name AbortError и
	// сообщением от браузера («The user aborted a request») — оно и неверно
	// по сути, и не переводится.
	if (e.name === 'AbortError') return t('err.timeout');
	// typeof, а не просто истинность: код приходит с сервера, и попадание
	// вроде 'constructor' достало бы из прототипа функцию вместо ключа.
	const key = e.code && ERR_KEY[e.code];
	if (typeof key === 'string') return t(key);
	if (e.status === 501) return t('wifi.switch.soon');
	// Запасной вариант для 503 с незнакомым кодом — нейтральный: конкретика
	// здесь была бы догадкой.
	if (e.status === 503) return t('err.unavailable');
	if (fallback) return t(fallback);
	return e.message || t('err.generic');
}

// Метка операции строится на клиенте по kind и arg. Поле label с демона
// русское намеренно (syslog и диагностика по ssh), и в английском
// интерфейсе оно читалось бы как утечка бэкенда.
function jobText(job, t) {
	if (job.kind === 'mode' && ['nikki', 'b4', 'off'].includes(job.arg)) return t('job.mode.' + job.arg);
	if (job.kind === 'subscription') return t('job.subscription');
	return t('job.working');
}

// ─────────── тосты ───────────

// Первая живая область в панели: до неё aria-live здесь не было нигде.
// Заводилась она под серую кнопку в шапке, которой больше нет (ADR-0024), но
// пережила её и нужна не меньше: сюда кладут текст ВСЕ действия, у которых
// нет своего места на экране, — «открываю панель», отказ /api/nikki/panel,
// заблокированная браузером вкладка, ошибки нажатий в карточках. Без
// role="status" этот текст возникает молча, и владелец, который ведёт панель
// с клавиатуры и слушает её, нажатия попросту не заметит.
//
// Контейнер висит в разметке всегда, а не появляется вместе с текстом:
// живая область, вставленная в DOM одновременно со своим содержимым, —
// классический способ не быть озвученной. Скринридер обязан увидеть её
// раньше изменения. Пустой контейнер не занимает места (см.
// .wrap.toasts:empty в app.css); display:none там нет намеренно — скрытая
// область выпадает из дерева доступности, и объяснение снова замолчит.
//
// Внешняя разметка держится в одну строку намеренно: перенос дал бы текстовый
// узел из пробелов, и :empty перестал бы совпадать.
//
// toast.href — запасной путь для заблокированного всплывающего окна. Это
// настоящая <a>, а не кнопка: клик по ней браузер считает жестом владельца
// и открывает вкладку, что бы ни думал блокировщик. Адрес несёт секрет,
// поэтому он живёт только в атрибуте (подпись берётся из словаря) и только
// до клика или до конца тоста.
function Toasts({ toast, onCta }) {
	const body = toast && html`<div class="note ${toast.kind}"><p>${toast.msg}</p>${toast.href
		&& html`<a class="cta" href=${toast.href} target="_blank" rel="noreferrer" onClick=${onCta}>${toast.cta}</a>`}</div>`;
	return html`<div class="wrap toasts" role="status">${body}</div>`;
}

// ─────────── шапка ───────────

// Ссылка в шапке существует ровно в одном виде — настоящая <a>. Заглушки
// на её месте нет и быть не должно: недоступную панель не рисуем вовсе,
// решение о показе принимает Top ниже.
//
// aria-label не дублирует подпись, а разворачивает её: «Nikki» в списке
// ссылок само по себе не говорит, куда ведёт. Видимая подпись при этом
// целиком входит в озвученное имя, иначе голосовое управление промахнулось
// бы мимо этой ссылки.
//
// onOpen перехватывает обычный клик там, где настоящий адрес известен только
// серверу (Nikki: в нём секрет). href при этом остаётся живым и настоящим —
// без секрета, но рабочим: средний клик, «открыть в новой вкладке» и
// «копировать адрес» обязаны делать то, что обещает контекстное меню.
// href="#" с onClick сломал бы всё это разом.
function TopLink({ href, label, name, onOpen }) {
	// Стрелка — украшение направления, а не текст: скринридер прочитал бы
	// её как «стрелка вправо вверх» посреди имени ссылки.
	const arrow = html`<span aria-hidden="true">↗</span>`;
	return html`
		<a class="toplink" href=${href} target="_blank" rel="noreferrer"
			title=${name} aria-label=${name} onClick=${onOpen}>${label} ${arrow}</a>`;
}

function Top({ s, t, lang, setLang, onNikkiOpen }) {
	const ap = s.ap || {};
	const meta = [ap.band && ap.band.toUpperCase(), ap.clients != null && t('ap.clients', { n: ap.clients })]
		.filter(Boolean).join(' · ');
	const links = s.links || {};
	// Кнопка ДВИЖКА рисуется, только если известен адрес И служба отвечает
	// (у LuCI ниже условие другое, там же и почему). Второе условие не
	// перестраховка: у обеих служб веб-морда живёт на ТОМ ЖЕ
	// слушателе, который опрашивает демон, — у b4 это ":::7000"
	// (docs/recon/evidence.json:8), у Nikki «отдельного слушателя нет:
	// external-controller '[::]:9090' единственный» (там же:266). Значит при
	// available:false ссылка гарантированно упёрлась бы в connection refused:
	// это была не запасная дверь, а обещание, которое некому выполнить.
	//
	// Недоступная кнопка не рисуется вообще — ни серой, ни какой. Серая
	// заглушка отвечала на вопрос «где панель» ровно тем же молчанием, что и её
	// отсутствие, но занимала место в шапке и ловила палец. Клик по видимой
	// кнопке Nikki всё ещё может не открыть панель (нет api_secret, не скачан
	// дашборд) — на это отвечает nikkiPanel кодом отказа, и это уже настоящий
	// ответ, а не догадка панели по пустому links.nikki.
	return html`
		<div class="top">
			<div class="top-id">
				<span class="host">${s.hostname || 'netmoded'}</span>
				${ap.ssid && html`<span class="ap-meta">${t('ap.broadcasts', { ssid: ap.ssid })}${meta ? ' · ' + meta : ''}</span>`}
			</div>
			<div class="top-links">
				${links.nikki && s.nikki?.available && html`
					<${TopLink} href=${links.nikki} label="Nikki" name=${t('links.nikki')}
						onOpen=${onNikkiOpen} />`}
				${links.b4 && s.b4?.available && html`
					<${TopLink} href=${links.b4} label="b4" name=${t('links.b4')} />`}
				<!-- Условие тут короче соседского, и это не недосмотр: у LuCI нет
				     «&& available» намеренно. Правило «прятать недоступные»
				     заведено на морды движков, которыми управляет netmode-apply
				     (ADR-0024, «Границы правила»): они гаснут по нашей же
				     команде при смене режима, и кнопка в никуда скрывала бы
				     нашу собственную работу. uhttpd мы не гасим и живость его
				     не считаем — она стоила бы пробы к чужой службе на каждом
				     тике, 3600 запросов в час ради подсветки кнопки.
				     Практическое следствие: в режиме off кнопок движков нет ни
				     одной, а LuCI есть; при мёртвых b4 и nikki — тоже. Клик без
				     onOpen: секрета в адресе нет (порт 80, /cgi-bin/luci),
				     перехватывать нечего. -->
				${links.luci && html`
					<${TopLink} href=${links.luci} label="LuCI" name=${t('links.luci')} />`}
				<div class="lang">
					${['ru', 'en'].map((l) => html`
						<button aria-pressed=${lang === l} onClick=${() => setLang(l)}>${l.toUpperCase()}</button>`)}
				</div>
			</div>
		</div>`;
}

// ─────────── баннер ───────────

// Заголовок провала — явной таблицей, как jobText, а не склейкой
// t('job.fail.' + kind): makeT при промахе возвращает сам ключ, и заявленный
// контрактом kind «upstream» напечатал бы в интерфейсе строку job.fail.upstream.
const failText = (j, t) => (j.kind === 'mode' ? t('job.fail.mode', { mode: j.arg })
	: j.kind === 'subscription' ? t('job.fail.subscription') : t('job.fail'));

function Banner({ s, t, svc, running, failed, busy, locked, onMode }) {
	const m = ['nikki', 'b4', 'off'].includes(s.mode) ? s.mode : 'unknown';
	const ssid = s.configured_ssid || s.associated_ssid;

	// Подпись идёт от svc, а не от голого available. Демон коммитит новый режим
	// раньше, чем поднимет службу, поэтому первые секунды available честно
	// false — и «Clash API недоступен» в этот момент отправлял бы владельца
	// чинить исправный роутер ровно тогда, когда всё идёт по плану.
	let sub;
	if (m === 'nikki') {
		// Признак закрепления приходит отдельным полем: имя узла в обоих
		// случаях одно и то же, а состояния разные.
		sub = svc.nikki === 'down' ? t('sub.nikki.down')
			: svc.nikki === 'starting' ? t('sub.nikki.starting')
				: s.nikki.pinned ? t('sub.nikki.manual', { node: s.nikki.set })
					: t('sub.nikki.auto', { node: s.nikki.set || '—' });
	} else if (m === 'b4') {
		sub = svc.b4 === 'down' ? t('sub.b4.down')
			: svc.b4 === 'starting' ? t('sub.b4.starting')
				: t('sub.b4', { set: s.b4.set || '—' });
	} else sub = t('sub.' + m);

	// Текущий режим не нажимается: повторное нажатие запускает полный джоб со
	// стопом-стартом служб и рестартом firewall — реальный разрыв связи на
	// 5–15 секунд без всякого результата.
	//
	// Сравнение идёт с сырым s.mode, а не с нормализованным m: при 'unknown'
	// оно не совпадает ни с одной кнопкой, и все три остаются живыми — из
	// нераспознанного состояния владелец обязан иметь выход в любой режим.
	const current = (id) => s.mode === id;

	const online = s.online?.ok;
	const netTip = ssid
		? (online ? t('net.tip.online', { ssid }) : t('net.tip.offline', { ssid }))
		: t('net.nossid');

	// Прогресс считается по running, а не по job: у доигранного джоба, который
	// демон держит в статусе ещё пять секунд, отсчитывать нечего.
	const pct = running && running.eta_sec
		? Math.min(99, Math.round(((Date.now() - new Date(running.started_at)) / 1000 / running.eta_sec) * 100))
		: 0;
	const left = running && running.eta_sec
		? Math.max(0, running.eta_sec - Math.round((Date.now() - new Date(running.started_at)) / 1000))
		: null;

	// Содержимое слота — ровно одно из трёх, и последняя ветка обязана быть
	// else, а не `&&`. Слот держит высоту призраком, поэтому пустое содержимое
	// даёт не «ничего», а дыру ровно в высоту .job под кнопками режимов.
	// Здесь всегда есть что сказать: пока нечего показывать про операцию,
	// показывается подсказка.
	const slot = () => {
		if (running) {
			return html`
				<div class="job">
					<div class="job-row">
						<span class="blink">${jobText(running, t)}</span>
						<span style="font-family:var(--mono);opacity:.8">
							${left != null ? t('job.left', { sec: left }) : t('job.blocked')}</span>
					</div>
					<div class="bar"><i style="width:${pct}%"></i></div>
				</div>`;
		}
		// Только заголовок: причина уехала в .note err в .wrap — см. App.
		if (failed) {
			return html`<div class="job bad"><div class="job-row"><span>${failText(failed, t)}</span></div></div>`;
		}
		return html`<div class="hint" style="opacity:.8">${locked ? t('mode.hint.busy') : t('mode.hint')}</div>`;
	};

	return html`
		<div class="banner m-${m}">
			<div class="headline">
				<h1>${t('title.' + m)}</h1>
				<div class="sub">${sub}</div>
				<div class="chips">
					<span class="chip">↑ ${ssid || t('net.nossid')}</span>
					<span class="chip ${online ? 'ok' : 'bad'}" title=${netTip}>
						${running && running.kind === 'mode' ? t('net.checking')
						: online ? t('net.online') : t('net.offline')}
					</span>
					${s.pending_apply && html`<span class="chip" title="uci commit без применения">pending</span>`}
				</div>
			</div>
			<div class="side">
				<div class="modes">
					${['nikki', 'b4', 'off'].map((id) => html`
						<button class="m-${id}" aria-pressed=${s.mode === id} disabled=${locked || current(id)}
							aria-busy=${on(busy, 'mode', id)}
							onClick=${() => onMode(id)}>
							${on(busy, 'mode', id) && html`<${Spin} /> `}${t('mode.' + id)}</button>`)}
				</div>
				<!-- Слот постоянной высоты. Подсказка, идущий джоб и провал —
				     три состояния одного места, а не три блока, приходящих
				     и уходящих из потока: раньше старт джоба разом убирал .hint
				     и добавлял ~74px, и карточки под баннером уезжали вниз
				     ровно в тот момент, когда владелец смотрит, сработало ли
				     нажатие. Высоту держит призрак — безусловно, потому что
				     держать её больше нечем: он не смотрит ни на running, ни на
				     failed, иначе исчезал бы вместе с тем, что замещает.
				     Неразрывный пробел, а не пустой span: высоту строки задают
				     метрики шрифта, и без единого символа их нечему задать.
				     Символом, а не сущностью &#160;, — htm сущности не
				     разбирает и напечатал бы их как текст.
				     И .job, и .hint обязаны быть детьми .slot: до прямых детей
				     .side десктопное .slot{width:100%} не дотянется, и на
				     широком экране вернётся третья строка флекса. -->
				<div class="slot">
					<div class="job ghost" aria-hidden="true">
						<div class="job-row"><span>${'\u00a0'}</span></div>
						<div class="bar"></div>
					</div>
					${slot()}
				</div>
			</div>
		</div>`;
}

// ─────────── состояние выбора ───────────

function SelectionNote({ s, t }) {
	const st = s.selection_state;
	if (st === 'single') return null;

	if (st === 'ambiguous') {
		return html`
			<div class="note err full">
				<h3>${t('sel.ambiguous.title')}</h3>
				<p>${t('sel.ambiguous.text')}</p>
				<code>${t('sel.ambiguous.cmd', { host: s.hostname || 'router' })}</code>
			</div>`;
	}
	const key = st === 'empty' ? 'sel.empty' : 'sel.alldisabled';
	return html`
		<div class="note warn full">
			<h3>${t(key + '.title')}</h3>
			<p>${t(key + '.text')}</p>
		</div>`;
}

// ─────────── Nikki ───────────

function NikkiCard({ data, svc, t, busy, locked, onPick, onTest }) {
	// Заглушка держит и заголовок, и место под нижнюю кнопку: без них
	// карточка подскочила бы на полсотни пикселей ровно в тот момент, когда
	// на неё смотрят.
	//
	// data сам по себе больше не отвечает на вопрос «что показывать»:
	// undefined теперь означает не только «список ещё едет», но и «служба
	// молчит, спрашивать нечего» — так его выставляет эффект в App. Что
	// именно происходит, знает svc, поэтому ветки идут от него.
	//
	// ПОРЯДОК ВЕТОК ЗНАЧИМ, и обе перестановки ломают своё.
	//
	// 'starting' идёт раньше отказа: гонку легко проиграть — 503 от ещё не
	// поднявшегося Clash API прилетает раньше, чем available станет true,
	// и data успевает стать null. Проверь отказ первым — и карточка снова
	// напечатает «не отвечает» про службу, которая нормально запускается.
	//
	// 'down' идёт раньше data === undefined: эффект в App возвращает data
	// в «ещё не спрашивали» именно потому, что служба молчит, так что здесь
	// undefined — норма, а не редкость. Проверь скелетон первым — и он станет
	// вечным, то есть START_GRACE не будет значить ничего.
	const skeleton = (note) => html`<div class="card" aria-busy="true"><h2>${t('srv.title')}</h2>
		<${Skel} n=${3} />
		<div class="skel" style="height:46px;margin-top:12px"></div>${note}</div>`;
	const down = () => html`<div class="card"><h2>${t('srv.title')}</h2><div class="empty">${t('srv.down')}</div></div>`;

	if (svc === 'starting') return skeleton(html`<p class="hint tight">${t('srv.starting')}</p>`);
	if (svc === 'down') return down();
	if (data === undefined) return skeleton(null);
	if (!data || !data.available) return down();
	const members = data.members || [];
	if (!members.length) {
		return html`<div class="card"><h2>${t('srv.title')}</h2><div class="empty">${t('srv.empty')}</div></div>`;
	}
	// pinned приходит от демона отдельным полем: по одному имени узла
	// «движок подобрал» и «закреплено руками» неразличимы, а разница
	// принципиальна — во втором случае автоподбор выключен.
	const pinned = !!data.pinned;
	const active = data.fixed || data.selected;

	return html`
		<div class="card">
			<h2>${t('srv.title')}
				<button class="linkbtn" disabled=${!pinned || locked} aria-busy=${on(busy, 'proxy', 'AUTO')}
					onClick=${() => onPick('AUTO')}>
					${on(busy, 'proxy', 'AUTO') && html`<${Spin} /> `}${pinned ? t('srv.auto.back') : t('srv.auto')}</button>
			</h2>
			${pinned && html`<p class="hint tight" style="color:var(--warn)">${t('srv.pinned.note')}</p>`}
			<div class="rows">
				${members.map((m) => {
					const isActive = active === m.name;
					return html`
					<button class="row ${isActive ? 'sel' : ''} ${m.alive ? '' : 'dim'}"
						disabled=${locked} aria-busy=${on(busy, 'proxy', m.name)}
						onClick=${() => onPick(m.name)}>
						<span class="name">${m.name}</span>
						${isActive && html`
							<span class="tag ${pinned ? 'tag-pin' : 'tag-auto'}"
								title=${pinned ? t('srv.tip.pinned') : t('srv.tip.auto')}>
								${pinned ? '📌 ' + t('srv.tag.pinned') : t('srv.tag.auto')}</span>`}
						<span class="meter"><i class=${latBg(m.delay_ms)} style="width:${latPct(m.delay_ms)}"></i></span>
						<!-- Кольцо занимает слот задержки: у .ms фиксированные 54px,
						     поэтому имя узла не съезжает. Вставка в начало строки
						     сдвинула бы его на 22px вправо — дёрганье ровно на том
						     элементе, за которым в этот момент следят. -->
						<span class="ms ${latClass(m.delay_ms)}">${on(busy, 'proxy', m.name)
							? html`<${Spin} />`
							: (m.delay_ms != null ? m.delay_ms + ' ms' : '—')}</span>
					</button>`;
				})}
			</div>
			<button class="wide" disabled=${locked} aria-busy=${on(busy, 'test')} onClick=${onTest}>
				${on(busy, 'test') ? html`<${Spin} /> ${t('srv.measuring')}` : t('srv.measure')}</button>
		</div>`;
}

// ─────────── b4 ───────────

function B4Card({ data, svc, t, busy, locked, onPick }) {
	// Порядок веток тот же и по тем же двум причинам, что в NikkiCard:
	// 'starting' раньше отказа (иначе проигранная гонка печатает «не отвечает»
	// про запускающуюся службу), 'down' раньше скелетона (иначе скелетон
	// вечен). Именно здесь дефект и был виден дольше всего: подпись баннера
	// чинилась сама со следующим тиком опроса, а карточка оставалась
	// в «Панель b4 не отвечает» до F5.
	const skeleton = (note) => html`<div class="card" aria-busy="true"><h2>${t('sets.title')}</h2>
		<${Skel} n=${3} cls="pill" wrap="sets" />${note}</div>`;
	const down = () => html`<div class="card"><h2>${t('sets.title')}</h2><div class="empty">${t('sets.down')}</div></div>`;

	if (svc === 'starting') return skeleton(html`<p class="hint tight">${t('sets.starting')}</p>`);
	if (svc === 'down') return down();
	if (data === undefined) return skeleton(null);
	if (!data || !data.available) return down();
	return html`
		<div class="card">
			<h2>${t('sets.title')}</h2>
			<div class="sets">
				${(data.sets || []).map((x) => html`
					<button aria-pressed=${x.enabled} disabled=${locked} aria-busy=${on(busy, 'set', x.id)}
						onClick=${() => onPick(x.id)}>
						${on(busy, 'set', x.id) && html`<${Spin} /> `}${x.name}</button>`)}
			</div>
			<p class="hint tight">${t('sets.hint')}</p>
		</div>`;
}

// ─────────── WiFi ───────────

// Подпись про диапазон станции строится из машинного band, а не из
// статического текста.
//
// Индекс радио диапазон не означает (ADR-0019): на другой ревизии железа
// станция может оказаться на 5 ГГц. Раньше здесь висела зашитая фраза про
// 2.4 ГГц, и панель уверенно писала бы неправду ровно там, где владелец
// ищет пропавшую сеть. Поле note из ответа не берём: оно приходит с
// демона по-русски и в английском интерфейсе выглядело бы так же плохо,
// как job.label до F3.
function bandNote(scan, t) {
	const band = scan && scan.band;
	if (band !== '2g' && band !== '5g') return null;
	const other = band === '2g' ? '5' : '2.4';
	return html`<p class="hint tight">${t('wifi.band', { band: band === '2g' ? '2.4' : '5', other })}</p>`;
}

function WifiCard({ nets, scan, t, busy, status, onScan, onOpenSheet, onDelete }) {
	const saved = (nets && nets.networks) || [];
	const found = (scan && scan.networks) || [];
	const savedSsids = new Set(saved.map((n) => n.ssid));
	const extra = found.filter((f) => f.ssid && !savedSsids.has(f.ssid));
	const hidden = found.filter((f) => !f.ssid);

	// Уровень сигнала подтягивается к сохранённой сети по SSID: у секции
	// UCI сигнала нет, он есть только у эфира.
	const sig = new Map(found.filter((f) => f.ssid).map((f) => [f.ssid, f.signal_dbm]));

	// При неоднозначной конфигурации запись запрещена целиком — показываем
	// это заранее, а не отказом после нажатия.
	const frozen = status.selection_state === 'ambiguous';

	return html`
		<div class="card">
			<h2>${t('wifi.title')}
				<div style="display:flex;gap:14px">
					<button class="linkbtn" disabled=${!!busy || frozen}
						onClick=${() => onOpenSheet({ mode: 'add' })}>${t('wifi.add')}</button>
					<button class="linkbtn" disabled=${!!busy} aria-busy=${on(busy, 'scan')} onClick=${onScan}>
						${on(busy, 'scan') ? html`<${Spin} /> ${t('wifi.scanning')}`
							: scan ? t('wifi.rescan') : t('wifi.scan')}</button>
				</div>
			</h2>

			${nets === undefined ? html`<${Skel} n=${2} />` : html`
			<div class="rows">
				${nets === null && html`<div class="empty">${t('wifi.down')}</div>`}
				${nets && saved.length === 0 && html`<div class="empty">${t('sel.empty.title')}</div>`}
				${saved.map((n) => html`
					<div class="row ${n.enabled ? 'sel' : ''}">
						<span class="name">${n.ssid || t('wifi.hidden')}</span>
						<span class="tag">${n.enabled ? t('wifi.active') : t('wifi.saved')}</span>
						${sig.has(n.ssid) && html`
							<span class="ms ${signalClass(sig.get(n.ssid))}">${sig.get(n.ssid)} dBm</span>`}
						${n.editable
							? html`
								<button class="mini" title=${t('wifi.edit')} disabled=${!!busy}
									onClick=${() => onOpenSheet({ mode: 'edit', id: n.id, ssid: n.ssid, encryption: n.encryption })}
									>${t('wifi.key')}</button>
								<button class="mini danger icon"
									title=${on(busy, 'del', n.id) ? t('wifi.deleting') : t('wifi.delete')}
									disabled=${!!busy} aria-busy=${on(busy, 'del', n.id)}
									onClick=${() => onDelete(n)}
									>${on(busy, 'del', n.id) ? html`<${Spin} />` : '✕'}</button>`
							: html`<span class="mark" title=${n.enabled ? t('wifi.locked') : t('sel.ambiguous.title')}>🔒</span>`}
					</div>`)}
			</div>`}

			${extra.length > 0 && html`
				<div class="rows" style="margin-top:8px">
					${extra.map((f) => html`
						<button class="row" disabled=${!!busy || frozen}
							onClick=${() => onOpenSheet({ mode: 'add', ssid: f.ssid, encryption: f.encryption })}>
							<span class="name">${f.ssid}</span>
							<span class="tag">${f.encryption === 'none' ? t('wifi.none') : f.encryption}</span>
							<span class="ms ${signalClass(f.signal_dbm)}">${f.signal_dbm} dBm</span>
							<span class="mark">＋</span>
						</button>`)}
				</div>`}

			${hidden.length > 0 && html`
				<p class="hint tight">${t('wifi.hidden')} · ${hidden.length} — ${t('wifi.hidden.cant')}</p>`}

			${bandNote(scan, t)}
			<p class="hint tight">${t('wifi.switch.soon')}</p>
		</div>`;
}

// ─────────── форма сети ───────────

function NetworkSheet({ sheet, t, busy, locked, onSave, onClose }) {
	const editing = sheet.mode === 'edit';
	const [ssid, setSsid] = useState(sheet.ssid || '');
	const [enc, setEnc] = useState(sheet.encryption || 'psk2');
	const [key, setKey] = useState('');
	const [show, setShow] = useState(false);
	const [err, setErr] = useState('');

	const needsKey = enc !== 'none';

	const submit = () => {
		if (!editing && !ssid.trim()) { setErr(t('wifi.err.ssid')); return; }
		// При правке пустой пароль означает «не менять» — так же, как
		// в API: отсутствие поля и пустая строка это разные вещи.
		if (key !== '' && (key.length < 8 || key.length > 63)) { setErr(t('wifi.err.short')); return; }
		if (!editing && needsKey && key === '') { setErr(t('wifi.err.short')); return; }

		const body = {};
		if (editing) body.id = sheet.id;
		else { body.ssid = ssid.trim(); body.encryption = enc; }
		if (key !== '') body.key = key;
		onSave(body, setErr);
	};

	return html`
		<div class="sheet-bg" onClick=${(e) => e.target === e.currentTarget && onClose()}>
			<div class="sheet">
				<h3>${editing ? t('wifi.sheet.edit', { ssid: sheet.ssid }) : t('wifi.sheet.add')}</h3>

				${!editing && html`
					<label class="field">
						<span>${t('wifi.field.ssid')}</span>
						<input value=${ssid} onInput=${(e) => { setSsid(e.target.value); setErr(''); }}
							autocomplete="off" spellcheck="false" />
					</label>
					<label class="field">
						<span>${t('wifi.field.enc')}</span>
						<select value=${enc} onChange=${(e) => setEnc(e.target.value)}>
							${['psk2', 'psk-mixed', 'sae', 'sae-mixed', 'psk'].map((v) =>
								html`<option value=${v} selected=${v === enc}>${v}</option>`)}
							<option value="none" selected=${enc === 'none'}>${t('wifi.enc.open')}</option>
						</select>
					</label>`}

				${needsKey && html`
					<label class="field">
						<span>${t('wifi.field.key')}</span>
						<div style="display:flex;gap:8px">
							<input type=${show ? 'text' : 'password'} value=${key}
								onInput=${(e) => { setKey(e.target.value); setErr(''); }}
								autocomplete="new-password" spellcheck="false" style="flex:1;min-width:0" />
							<button class="mini" onClick=${() => setShow(!show)}>
								${show ? t('wifi.hide') : t('wifi.show')}</button>
						</div>
					</label>
					${editing && html`<p class="hint tight">${t('wifi.keykept')}</p>`}`}

				${err && html`<p class="hint tight" style="color:var(--bad)">${err}</p>`}
				<p class="hint tight">${t('wifi.keyhint')}</p>

				<div class="sheet-actions">
					<!-- Сохранение — мутация, поэтому запирается и чужим джобом
					     тоже: раньше оно смотрело только на busy, и форму можно
					     было отправить посреди смены режима. -->
					<button class="wide primary" disabled=${!!busy || locked} aria-busy=${on(busy, 'save')} onClick=${submit}>
						${on(busy, 'save') ? html`<${Spin} /> ${t('wifi.saving')}` : t('wifi.save')}</button>
					<!-- «Отмена» не блокируется никогда: это выход, а не действие.
					     Запирать выход из формы из-за постороннего действия — как
					     раз то, во что превращался disabled в роли мьютекса. -->
					<button class="wide" onClick=${onClose}>${t('wifi.cancel')}</button>
				</div>
			</div>
		</div>`;
}

// ─────────── подписка ───────────

function SubCard({ s, logs, t, lang, busy, onUpdate }) {
	const sub = s.subscription || {};
	return html`
		<div class="card">
			<h2>${t('sub.title')}</h2>
			<div class="hint" style="color:var(--fg-2);font-size:14px">
				${t('sub.when', { when: sub.last_update ? fmtTime(sub.last_update, lang) : t('sub.never') })}</div>
			<div class="hint">${t('sub.nodes', { n: sub.nodes ?? 0 })}</div>
			${sub.status === 'fail' && sub.error && html`<div class="hint" style="color:var(--bad)">${sub.error}</div>`}

			<button class="wide" disabled=${!!busy} aria-busy=${on(busy, 'sub')} onClick=${onUpdate}>
				${on(busy, 'sub') ? html`<${Spin} /> ${t('sub.updating')}` : t('sub.update')}</button>

			${logs === undefined ? html`<${Skel} n=${2} cls="line" wrap="log" />` : html`
			<div class="log">
				${logs === null && html`<div>${t('sub.log.down')}</div>`}
				${logs && (!logs.lines || !logs.lines.length) && html`<div>${t('sub.emptylog')}</div>`}
				${logs && logs.lines && logs.lines.map((l) => html`
					<div>
						<span style="color:${l.status === 'ok' ? 'var(--ok)' : 'var(--bad)'}">
							${l.status === 'ok' ? '✓' : '✕'}</span>
						<span class="when">${fmtTime(l.ts, lang)}</span>
						<span>${l.status === 'ok' ? l.nodes : (l.err || '—')}</span>
					</div>`)}
			</div>`}
		</div>`;
}

// ─────────── подвал ───────────

function Foot({ s, t, lang, stale }) {
	return html`
		<div class="foot">
			<span class=${stale ? 'stale' : ''}>
				${stale ? t('foot.stale') : t('foot.updated', { when: fmtTime(s.generated_at, lang) })}</span>
			<span>${t('foot.phase')}</span>
		</div>`;
}

render(html`<${App} />`, document.getElementById('app'));
