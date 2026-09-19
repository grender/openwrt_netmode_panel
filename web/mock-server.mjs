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
// Корень статики. По умолчанию — собранная панель, а не исходники: до
// сборки исходник и отгружаемое совпадали, теперь нет. У дев-сервера нет ни
// минификации, ни вынесенного CSS, ни того же разбиения на чанки, и мерить
// на нём геометрию значит мерить бандл, который никто не загрузит.
//
// MOCK_STATIC= (пусто) — не отдавать статику вовсе: так мок работает под
// дев-сервером Vite, который отдаёт её сам и проксирует сюда только API.
const WEB = process.env.MOCK_STATIC === undefined
	? path.join(HERE, 'panel', 'dist')
	: (process.env.MOCK_STATIC ? path.resolve(process.env.MOCK_STATIC) : '');
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
	// Итог замера задержек (POST /api/nikki/test). Обычный ответ виден на
	// любом сценарии с режимом nikki; эти два показывают то, чего кликом не
	// добиться: наружу не выбрался никто и замер не успел обойти всех.
	'nikki-test-dead': 'status-single.json',
	'nikki-test-slow': 'status-single.json',
	// Список узлов БЕЗ манифеста подписки: демон отдаёт живой список mihomo,
	// все строки — узлы, строки «Авто» в нём нет. Так выглядит свежая
	// установка, и это единственный сценарий, где кнопка возврата к
	// автовыбору обязана стоять в шапке карточки: взяться ей больше неоткуда.
	'nikki-no-manifest': 'status-single.json',
	// Адрес подписки не задан в /etc/config/netmode. Кнопка «Обновить сейчас»
	// заблокирована, над ней — команда для ssh. Состояние первой установки:
	// увидеть его на роутере можно ровно один раз, а чинить панель для него
	// приходится всегда.
	'sub-unset': 'status-single.json',
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
	// Переключение внешней сети (POST /api/upstream). База у всех шести —
	// status-single.json: mode nikki, configured_ssid == associated_ssid ==
	// John24, wireless_fingerprint sha256:1f0c9a3b7d2e4a58 — то же значение,
	// что fingerprint в wifi-networks.json, иначе If-Match проверить нечем.
	// Исход решает POST-обработчик через overlay, а не отдельная фикстура на
	// сценарий: до нажатия кнопки все шесть неотличимы.
	//
	// Исход каждого лежит в UPSTREAM_REASON ниже, и это не формальность:
	// upstream-busy был зарегистрирован ЗДЕСЬ, но в той таблице отсутствовал,
	// а отсутствие там означает успех. Сценарий с именем «занято» двадцать
	// секунд показывал удачное переключение — и на нём нельзя было ни
	// посмотреть текст про занятое радио, ни поймать регрессию в нём.
	// Сценарии проброса. Статус у них общий (мост живёт в своём
	// GET /api/bridge), а различаются они фикстурой состояния моста —
	// см. BRIDGE_FIXTURES ниже.
	'bridge-off': 'status-single.json',
	'bridge-on': 'status-single.json',
	'bridge-no-relayd': 'status-single.json',
	'bridge-fail': 'status-single.json',
	'bridge-fail-iface': 'status-single.json',
	'bridge-fail-relay': 'status-single.json',
	'bridge-fail-install': 'status-single.json',
	'bridge-busy': 'status-single.json',
	'upstream-ok': 'status-single.json',
	'upstream-fail-stale': 'status-single.json',
	'upstream-fail-nokey': 'status-single.json',
	'upstream-no-ipv4': 'status-single.json',
	'upstream-busy': 'status-single.json',
	// Застрявший черновик UCI (ADR-0028). Единственный исход, где совет —
	// не «повторите», а ssh и `uci revert wireless`: смотреть надо именно на
	// текст карточки, кнопка здесь не поможет.
	'upstream-stale-draft': 'status-single.json',
	// Половинчатая установка: демон приехал, /usr/local/bin/netmode-wifi нет.
	// Второй исход, где «повторите» — вредный совет: повтор упрётся в то же
	// отсутствие файла. Смотреть надо на текст карточки и на detail под ним —
	// именно detail и называет путь, который владелец унесёт в ssh.
	//
	// Баннер про отсутствующие исполнители тут НЕ показывается: он живёт в
	// missing_executors статуса, а сценарий выставляет только исход джоба.
	// Пара «баннер до нажатия» проверяется отдельным сценарием ниже.
	'upstream-no-executor': 'status-single.json',
	// Та же поломка, увиденная ДО нажатия: баннер про отсутствующие
	// исполнители. Отдельный сценарий, а не поле в предыдущем, потому что это
	// два разных доклада об одном факте, и проверять их надо порознь — на
	// роутере владелец может увидеть только один из двух (баннер, если
	// смотрит; причину джоба, если нажал не глядя).
	//
	// В фикстуре отсутствуют ОБА скрипта: список рисуется построчно, и на
	// одном элементе не видно ни вёрстки нескольких строк, ни того, что
	// порядок фиксирован.
	'no-executors': 'status-no-executors.json',
	// Исходы переключения, достижимые СРАЗУ ПРИ ЗАГРУЗКЕ — сменой сценария,
	// а не нажатием кнопки.
	//
	// Пять сценариев выше показывают исход только через двадцатисекундный джоб,
	// и мерить на них геометрию нельзя: замер приходится на гонку с таймером,
	// а перезагрузка страницы стирает результат. Здесь исход лежит в фикстуре
	// и виден с первого кадра, сколько на него ни смотри.
	//
	// Пары «короткий/длинный» существуют ради одного и того же измерения:
	// ssid ровно в 32 байта — предел стандарта (internal/httpapi/wifiwrite.go,
	// validateNetwork) и худший случай для вёрстки. Что именно ломается,
	// видно только рядом с базой сравнения, поэтому короткий вариант — тоже
	// сценарий, а не «просто single».
	'upstream-fail-seen': 'status-upstream-fail.json',
	'upstream-fail-long': 'status-upstream-fail-long.json',
	// База сравнения для длинных состояний, и существует она только ради
	// замера (scripts/measure-geometry.mjs, критерии C3/C4). Фикстура —
	// побайтовая копия status-upstream-fail-long.json с last_fail: null,
	// то есть отличается от неё РОВНО блоком провала и ничем больше.
	//
	// Без такой базы длинные состояния приходилось сравнивать с single, а он
	// отличается сразу двумя признаками — и провалом, и длиной ssid в шапке
	// и баннере. Разница по двум переменным не измеряет ни одну из них.
	'single-long': 'status-single-long.json',
	'upstream-job-long': 'status-job-upstream-long.json',
	// Три кнопки в ряду у сети с длинным именем: статус обычный, длина живёт
	// в списке (LISTS ниже), а не в статусе.
	'upstream-long-list': 'status-single.json',
	// Наборы geosite (GET/PUT /api/nikki/rulesets, GET .../catalog). Статус
	// у всех пяти общий — status-single.json, режим nikki: ломается не
	// роутер, а то, что на вкладке наборов, а исход решает RULESETS ниже.
	'rulesets-profile': 'status-single.json',
	'rulesets-only': 'status-single.json',
	'rulesets-foreign': 'status-single.json',
	'rulesets-down': 'status-single.json',
	'rulesets-nocatalog': 'status-single.json',
	'rulesets-custom': 'status-single.json',
	// Наблюдатель (ADR-0043). Статус общий — status-single.json, режим nikki:
	// ломается не роутер, а то, что показывает третий экран. Исход решает
	// WATCH ниже. Единственное исключение — watch-off: там наблюдать нечего
	// именно потому, что режим другой, и это состояние статуса.
	'watch-session': 'status-single.json',
	'watch-pick': 'status-single.json',
	'watch-quiet': 'status-single.json',
	'watch-restart': 'status-single.json',
	'watch-unparsed': 'status-single.json',
	'watch-off': 'status-all-disabled.json',
};

// Что показывает сценарий наблюдателя. null — сессии нет, и экран обязан
// показать выбор устройства; во всех остальных сессия заводится сразу при
// переключении сценария, иначе половину экрана видно только после нажатия.
const WATCH = {
	'watch-session': { engine: 'running', unparsed: 0, quiet: false },
	'watch-restart': { engine: 'restarting', unparsed: 0, quiet: false },
	'watch-unparsed': { engine: 'running', unparsed: 7, quiet: false },
	'watch-quiet': { engine: 'running', unparsed: 0, quiet: true },
};

// Какой файл списка сетей отдаётся в сценарии. Умолчание — wifi-networks.json.
//
// Таблица, а не лесенка условий: файлов уже три, и каждое следующее состояние
// добавляло бы к выражению ещё одну ветку в месте, где решение принимается
// одно — «какой файл». Рядом с SCENARIOS видно и вторую половину пары: чем
// сценарий отличается по статусу и чем по списку.
const LISTS = {
	ambiguous: 'wifi-networks-ambiguous.json',
	'upstream-fail-long': 'wifi-networks-long.json',
	'upstream-job-long': 'wifi-networks-long.json',
	'upstream-long-list': 'wifi-networks-long.json',
	// Тот же список, что у upstream-fail-long, и это обязательно: список
	// задаёт высоту карточки Wi-Fi, а значит и subTop под ней. Оставь здесь
	// умолчание — и база разошлась бы с измеряемым состоянием ещё и по
	// содержимому списка, то есть по второй переменной, ради устранения
	// которой она и заведена. Заодно совпадёт fingerprint: у длинной фикстуры
	// он sha256:5c2ea8d417b60f93, как в wifi-networks-long.json.
	'single-long': 'wifi-networks-long.json',
};

// Какой пример GET /api/nikki/rulesets отдаётся в сценарии. Умолчание —
// rulesets-profile: mixin.yaml на диске нет, и это ровно то, что демон
// отдаёт на свежей установке (internal/httpapi/rulesetshandlers.go,
// readMixin, случай os.ErrNotExist).
//
// rulesets-down и rulesets-nocatalog берут ту же фикстуру, что и
// rulesets-only, — намеренно: первый показывает движок, переставший
// отвечать про уже применённый выбор (обработчик роута гасит поля до
// null), второй — тот же выбор с недоступным каталогом. Разные файлы
// потребовались бы, если бы менялся сам выбор, а не то, что о нём известно.
const RULESETS = {
	'rulesets-profile': 'nikki-rulesets-profile.json',
	'rulesets-only': 'nikki-rulesets-only.json',
	'rulesets-foreign': 'nikki-rulesets-foreign.json',
	'rulesets-down': 'nikki-rulesets-only.json',
	'rulesets-nocatalog': 'nikki-rulesets-only.json',
	// Свои правила владельца рядом с набором: домен в туннель и подсеть
	// напрямую — по одному на каждый вид отказа демона, который панель
	// обязана предупреждать сама (ruleProblem).
	'rulesets-custom': 'nikki-rulesets-rules.json',
	// У наблюдателя своё правило владельца обязано быть ПРИМЕНЁННЫМ: только
	// тогда строка netbird.io называется «своё правило», а не сырым
	// DomainSuffix от mihomo, и только тогда виден зелёный тег «правило»
	// рядом с жёлтым «в черновике». Без этого половина разметки строки
	// проверяется лишь на роутере.
	'watch-session': 'nikki-rulesets-watch.json',
	'watch-restart': 'nikki-rulesets-watch.json',
	'watch-unparsed': 'nikki-rulesets-watch.json',
	'watch-quiet': 'nikki-rulesets-watch.json',
	'watch-pick': 'nikki-rulesets-watch.json',
};

// Зеркало серверной проверки своих правил (rulesets.ValidateRules) в той
// мере, в какой автор панели обязан узнать про отказ здесь, а не на
// роутере: вид, действие, пустота, дубль, потолок, адрес без маски. Текст
// с номером строки — тот же формат, что у демона (ruleErrText).
function badRule(rules) {
	if (!Array.isArray(rules)) return 'Поле rules обязано быть списком правил';
	if (rules.length > 64) return `Правило 65 (${rules[64].kind} ${rules[64].value}) не принято: своих правил больше 64`;
	const seen = new Set();
	for (let i = 0; i < rules.length; i++) {
		const r = rules[i] || {};
		const at = `Правило ${i + 1} (${r.kind} ${r.value}) не принято: `;
		if (!['suffix', 'domain', 'cidr'].includes(r.kind)) return at + `неизвестный вид "${r.kind}" — бывают suffix, domain, cidr`;
		if (!['tunnel', 'direct'].includes(r.action)) return at + `неизвестное действие "${r.action}" — бывают tunnel, direct`;
		if (typeof r.value !== 'string' || r.value === '') return at + 'пустое значение';
		const c = r.comment === undefined ? '' : r.comment;
		if (typeof c !== 'string' || [...c].length > 80) return at + 'комментарий длиннее 80 знаков';
		if (/[\x00-\x1f\x7f]/.test(c)) return at + 'в комментарии перевод строки или управляющий символ';
		if (c.trim() !== c) return at + 'пробелы по краям комментария';
		if (r.kind === 'cidr' && !r.value.includes('/')) return at + 'подсеть записывается с маской, например 10.0.0.0/8 или 10.0.0.1/32';
		if (r.kind !== 'cidr' && !/^[a-z0-9.-]+$/.test(r.value)) return at + 'не похоже на имя хоста: бывают латиница, цифры, дефис и точка';
		const key = r.kind + ':' + r.value;
		if (seen.has(key)) return at + `значение "${r.value}" указано дважды`;
		seen.add(key);
	}
	return '';
}

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
//
// Значение одно на обоих пользователей константы: и на джобы из фикстур
// (state.jobUntil), и на джобы, которые мок запускает сам (startJob).
const JOB_KEEP_MS = 5000;

// Сдвиг часов «роутера» относительно часов машины, где открыт браузер.
//
// ЗАЧЕМ. Все метки времени в контракте — router time: generated_at, started_at,
// last_fail.at. На ноутбуке разработчика мок и браузер живут на одних часах,
// поэтому ЛЮБАЯ ошибка вида «местное время минус метка роутера» здесь даёт
// верный ответ и не воспроизводится ни одним прогоном. На коробке без RTC,
// у которой не поднялся upstream, расхождение — часы и больше, и ровно такую
// коробку панель и обслуживает. Знак важен обеих: отстающий роутер и
// опережающий ломают разное.
//
// Задаётся при запуске (MOCK_SKEW_SEC=-3600 node web/mock-server.mjs 8090)
// либо на ходу: curl 'http://localhost:PORT/__clock?skew=3600'.
let skewMs = Number(process.env.MOCK_SKEW_SEC || 0) * 1000;
// Единственный источник времени мока. Всё, что уезжает в ответ меткой,
// обязано идти отсюда: метка, снятая мимо сдвига, рассинхронизировала бы
// «роутер» сам с собой, и проверялось бы не то.
const nowISO = () => new Date(Date.now() + skewMs).toISOString();

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
	// Состояния, которых НЕ должно быть в overlay. overlay целиком
	// расплёскивается в /api/status (`...state.overlay`), поэтому всё, что
	// туда положено, становится полем статуса. Для адреса подписки это
	// прямая утечка секрета в ответ, который панель опрашивает раз в
	// секунду; для сетов b4 — лишнее поле, которого на роутере нет.
	sub: {},
	b4: {},
	// Сети, созданные через POST /api/wifi/networks. Держать их обязательно:
	// панель после сохранения читает networks ИЗ ОТВЕТА на запись и находит
	// свою запись ДИФФОМ ПО id — вычитает множество id, снятое до записи, из
	// списка после неё (поиск по ssid снят: дубли имён штатны, ADR-0005).
	// Мок, забывающий созданную сеть, отдал бы разность пустой, и составное
	// действие «Сохранить и подключиться» стало бы непроверяемым: список
	// схлопывается обратно к фикстуре сразу после закрытия формы.
	created: [],
	// Правки существующих сетей: id → изменённые поля. Фикстуру не трогаем
	// по той же причине, что и везде, — она эталон.
	edits: {},
	job: null,
	// Окно молчания службы: upEngine — чей порт молчит, upAt — момент (мс),
	// с которого он начинает отвечать. Ноль означает «окна нет»: движок виден
	// таким, каким его показывает фикстура.
	upEngine: '',
	upAt: 0,
	// Докуда показывается провалившийся джоб из фикстуры. Ноль — бессрочно.
	jobUntil: 0,
	// Сессия наблюдения: null — её нет. since нужен, чтобы числа РОСЛИ:
	// экран, у которого главное свойство «числа меняются раз в секунду, а
	// раскладка не прыгает», на застывшей фикстуре не проверяется вовсе.
	watch: null,
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

// Список сохранённых сетей — ровно то тело, что отдаёт GET /api/wifi/networks.
//
// Вынесен отдельно, потому что читателей у него три и все обязаны видеть ОДИН
// список: сам GET, ответ на запись (по контракту тело то же) и поиск секции
// в POST /api/upstream. Прежде третий читал файл напрямую, и переключиться на
// только что созданную сеть было нельзя — её в файле нет, а значит 404.
//
// Отпечаток берётся из статуса, а не из файла, и это не подгонка. Демон
// считает оба значения от одного и того же `uci show wireless`
// (internal/httpapi/respondNetworks и status.go), поэтому разойтись они на
// роутере не могут в принципе. В моке разойтись могли: панель шлёт в If-Match
// отпечаток из списка, а POST /api/upstream сверяет его с отпечатком статуса,
// и любая пара фикстур, собранная порознь, давала бы fingerprint_mismatch на
// первом же нажатии «Подключить».
const currentNetworks = async () => {
	const d = await readJSON(LISTS[state.scenario] || 'wifi-networks.json');
	const s = { ...(await currentStatus()), ...state.overlay };
	// Список сетей обязан согласовываться со сценарием. Мок, у которого
	// баннер говорит «сохранённых сетей нет», а список показывает две, —
	// хуже отсутствующего: он учит неправде о собственном интерфейсе.
	d.selection_state = s.selection_state;
	d.fingerprint = s.wireless_fingerprint;
	if (state.scenario === 'empty') {
		d.networks = [];
	} else if (state.scenario === 'all-disabled') {
		d.networks = d.networks.map((n) => ({ ...n, enabled: false, editable: true }));
	} else if (s.selection_state === 'single' && s.configured_ssid) {
		// Какая секция включена — знает статус, и список обязан говорить то же
		// самое. Это одинаково верно и для фикстуры, где переключение уже
		// состоялось (configured_ssid не John24), и для успешного POST
		// /api/upstream, положившего новый ssid в overlay: иначе панель,
		// перечитавшая список по расхождению fingerprint, увидела бы список,
		// который сам себе противоречит.
		d.networks = d.networks.map((n) => {
			const enabled = n.ssid === s.configured_ssid;
			return { ...n, enabled, editable: !enabled, switchable: !enabled };
		});
	}
	// Созданные — в конец: uci add дописывает секцию в файл, а порядок в
	// ответе тот же, что в файле.
	d.networks = [...d.networks, ...state.created]
		.map((n) => (state.edits[n.id] ? { ...n, ...state.edits[n.id] } : n));
	return d;
};

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
	// Блок b4 обязан согласоваться с наложением, которое положил
	// POST /api/b4/set. Раньше он был константой, и переключение сета не
	// меняло в статусе ни байта: пока сет был ровно один, расхождения не
	// видно, но после ADR-0033 состояний три, и «ни одного» в статусе
	// выглядело бы как «b4 отдаёт general», то есть мок врал бы ровно про
	// то, ради чего решение и принято.
	if (s.b4 && s.b4.available && state.b4.enabled) {
		const names = B4_NAMES;
		const on = state.b4.enabled.map((id) => names[id]).filter(Boolean);
		s.b4 = { ...s.b4, set: on.length === 1 ? on[0] : '', enabled_count: on.length };
	}
};

// B4_NAMES — id → имя из той же фикстуры, что отдаёт /api/b4/sets. Карта, а не
// чтение файла: applyEngines синхронна и зовётся из статуса раз в секунду.
const B4_NAMES = {
	'4d1f9a2e-7c30-4b58-9a61-0e2f8b7c1d34': 'general',
	'b8e0c153-2a44-49df-8f27-63a1d905ee72': 'youtube',
	'0c73a6b1-95d2-4e10-b3cc-51f8e4270a9b': 'discord',
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

// cache по умолчанию no-store: почти весь API читается раз в секунду или по
// клику, и кэшировать его нечем. Каталог наборов geosite — единственное
// исключение (см. /api/nikki/rulesets/catalog): 60 КБ, меняются раз в
// несколько часов, и повторное открытие вкладки обязано стоить дешевле, чем
// весь список заново.
const send = (res, code, body, type = MIME['.json'], cache = 'no-store') => {
	const b = typeof body === 'string' || Buffer.isBuffer(body) ? body : JSON.stringify(body, null, 2);
	res.writeHead(code, { 'Content-Type': type, 'Cache-Control': cache });
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
// autopoolBody — тело GET /api/nikki/autopool.
//
// Узлы берутся из того же примера, что и список в карточке Nikki: два
// разных списка одних и тех же серверов в одной панели читались бы как
// расхождение демона с самим собой. Пул провайдера — первые два имени:
// в снятой подписке балансировщик у провайдера уже своей отбор, и важно
// показать, что он УЖЕ отличается от всего списка.
const autopoolBody = async () => {
	const proxies = await readProxies();
	const available = (proxies.members || []).filter((m) => m.kind === 'node').map((m) => m.name);
	const o = state.overlay.autopool || {};
	const nodes = o.nodes || [];
	return {
		fingerprint: o.fingerprint || 'sha256:0f3c1a2b4d5e6f70',
		mode: o.mode || 'deny',
		nodes,
		foreign: state.scenario === 'rulesets-foreign',
		available,
		provider_pool: available.slice(0, 2),
		missing: nodes.filter((n) => !available.includes(n)),
		// Движок молчит — размер неизвестен. Ноль соврал бы про пустой пул.
		pool_size: (await engineUp('nikki')) ? (o.pool_size ?? available.length) : null,
	};
};

const startJob = (kind, arg, label, sec, finish) => {
	state.job = {
		id: 'j-' + Math.random().toString(16).slice(2, 8),
		kind, arg, label,
		started_at: nowISO(),
		finished_at: null,
		eta_sec: sec,
		state: 'running',
		error: null,
	};
	// Таймеры привязаны к своему джобу по id. Без этого таймер завершившейся
	// операции догонял уже следующую и гасил её: доиграл первый джоб — и через
	// окно показа из статуса пропадал второй, только что запущенный.
	const id = state.job.id;
	const mine = () => state.job && state.job.id === id;
	setTimeout(() => {
		if (mine()) {
			if (finish) finish(); else state.job.state = 'done';
			state.job.finished_at = nowISO();
		}
		// Завершённый джоб держится JOB_KEEP_MS — столько же, сколько у демона.
		// Здесь стояло 1500, и мок расходился с контрактом ровно в том окне,
		// на которое опирается панель: «новый джоб успевает стартовать, пока
		// в статусе лежит доигранный» (разбор засева в панели) на моке
		// не воспроизводилось вовсе, а исход операции успевал уйти с экрана
		// втрое раньше, чем уйдёт на роутере.
		setTimeout(() => { if (mine()) state.job = null; }, JOB_KEEP_MS);
	}, sec * 1000);
};

// Причина неудачи переключения по имени сценария. Отсутствие в таблице —
// успех. Строки те же, что в закрытом наборе LastFail.reason
// (docs/api/openapi.yaml) и в internal/httpapi/upstreamhandler.go.
//
// Мок вправе знать НЕ ВСЕ десять причин — он сценарный, а не справочник.
// Но выдумывать одиннадцатую нельзя: код, которого нет в allReasons, с
// роутера не придёт никогда, и панель отрисовала бы на моке то, чего не
// бывает. Подмножество стережёт scripts/check-fail-reasons.sh.
const FAIL_REASONS = JSON.parse(
	await fs.readFile(path.join(HERE, 'mock', 'fail-reasons.json'), 'utf8'),
);
const UPSTREAM_REASON = FAIL_REASONS.upstream;

// То же для сценариев проброса (ADR-0030). Отдельная таблица, а не общая:
// наборы причин разные, и общая позволила бы моку отдать «no_ipv4» на
// включение моста — код, которого демон в этом слоте не отдаст никогда.
// Подмножество allBridgeReasons стережёт scripts/check-fail-reasons.sh.
// Состояние проброса по сценарию. Фикстуры golden — те же файлы, что
// сверяют Go-тесты (docs/api/examples), поэтому мок и демон показывают
// панели одну и ту же форму, а не две похожие.
const BRIDGE_FIXTURES = {
	'bridge-on': 'bridge-enabled.json',
	'bridge-no-relayd': 'bridge-no-relayd.json',
	'bridge-fail': 'bridge-fail.json',
};

const BRIDGE_REASON = FAIL_REASONS.bridge;

// Текст job.error для моста — дословно из internal/httpapi/bridgehandler.go
// (runBridge): панель и мок обязаны показывать одну и ту же историю про
// один и тот же код.
const bridgeFailText = (reason) => {
	if (reason === 'no_iface') {
		return 'network reload прошёл, но интерфейс проброса не поднялся с адресом ' +
			'192.168.0.85 за отведённое окно';
	}
	if (reason === 'relay_down') {
		return 'интерфейс проброса поднят, но процесс relayd не запустился — ' +
			'ПК не будет виден uplink-сети';
	}
	if (reason === 'install_failed') {
		return 'netmode-bridge: установить relayd не удалось: apk add relayd отказал (код 9)';
	}
	if (reason === 'busy') {
		return 'netmode-bridge: занято: применение уже идёт';
	}
	return 'операция проброса не удалась';
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
	if (reason === 'busy') {
		// Форма сентинела из internal/executor/executor.go: errorForCode
		// заворачивает ErrUpstreamBusy и stderr скрипта одним «%w: %w».
		return 'netmode-wifi: занято: не удалось взять блокировку — ' +
			'радио настраивает другой процесс';
	}
	if (reason === 'executor_missing') {
		// Форма из internal/executor: ErrNoExecutor и под ним *fs.PathError,
		// который отдаёт os/exec, когда файла нет. Панель показывает эту
		// строку как detail, и она обязана выглядеть ровно так же, как в
		// logread роутера, — иначе владелец не опознает её при сверке.
		return 'netmode-wifi radio0: исполнителя нет на роутере: ' +
			'fork/exec /usr/local/bin/netmode-wifi: no such file or directory';
	}
	if (reason === 'stale_draft') {
		// Две новости одной строкой, как в switchUpstream, шаг 5:
		// «%w; отменить не удалось: %w». Первая половина — почему свернулись,
		// вторая — почему черновик остался лежать.
		return 'запись секций не удалась; отменить не удалось: ' +
			'uci revert wireless вернул ошибку — черновик остался в конфигурации';
	}
	return `станция не подключилась к «${ssid}» за отведённое время`;
};

// ─── список узлов в форме подписки ───
//
// Golden-фикстура nikki-proxies.json остаётся источником ОБОЛОЧКИ (версия,
// группа, selected, pinned) и первых трёх узлов: её же сверяют Go-тесты, и
// расходиться с ней мок не вправе. Но узлов в ней три, и все они обычные, —
// а увидеть надо все четыре вида и настоящую длину списка провайдера.
// Разделитель, до которого надо доскроллить, и три строки в одном экране —
// это разные состояния интерфейса.
//
// Порядок — порядок ПОДПИСКИ, а не живого списка mihomo: «Авто» первой
// (провайдер ставит балансировщик в начало), заголовок раздела — перед своим
// разделом, непереводимая запись — там, где её выдал провайдер. Именно этот
// порядок демон и восстанавливает по манифесту; вид строки структурно не
// определяется, поэтому едет полем kind (ADR-0031).
const nd = (name, delay) => ({
	// alive выведен из задержки намеренно: пара «жив, но замера нет» у демона
	// бывает только в первые секунды после рестарта mihomo, и городить её в
	// моке значило бы учить панель состоянию, которого она почти не видит.
	name, type: 'Vless', alive: delay != null, delay_ms: delay,
	pinned: false, selectable: false, kind: 'node',
});
// Строка не-узел: alive всегда false, delay_ms всегда null — так их отдаёт
// демон, и панель обязана НЕ читать это как «узел мёртв».
const nx = (name, kind, reason) => ({
	name, type: kind === 'auto' ? 'URLTest' : 'Direct', alive: false, delay_ms: null,
	pinned: false, selectable: false, kind, ...(reason ? { reason } : {}),
});

// Двадцать узлов провайдера. Один намеренно длиннее коробки — на нём видно,
// работает ли многоточие в .row .name на 320px; мёртвые расставлены вперемешку,
// а не хвостом, иначе список выглядит отсортированным по живости, каким он
// не бывает.
const SUB_NODES = [
	['🇳🇱 Нидерланды · Амстердам', 121], ['🇩🇪 Германия · Франкфурт', 96],
	['🇫🇮 Финляндия · Хельсинки', 88], ['🇸🇪 Швеция · Стокгольм', null],
	['🇬🇧 Британия · Лондон', 143], ['🇫🇷 Франция · Париж', 134],
	['🇺🇸 США · Нью-Йорк (только для стриминга, без торрентов)', 187],
	['🇺🇸 США · Лос-Анджелес', 214], ['🇯🇵 Япония · Токио', null],
	['🇸🇬 Сингапур', 246], ['🇹🇷 Турция · Стамбул', 74],
	['🇦🇪 ОАЭ · Дубай', 158], ['🇨🇭 Швейцария · Цюрих', 103],
	['🇵🇱 Польша · Варшава', 61], ['🇱🇻 Латвия · Рига', 57],
	['🇪🇪 Эстония · Таллин', null], ['🇨🇿 Чехия · Прага', 79],
	['🇦🇹 Австрия · Вена', 92], ['🇮🇹 Италия · Милан', 111],
	['🇪🇸 Испания · Мадрид', 129],
];

// Раздел под заголовком: узлы внутри страны, через которые открываются
// сервисы, закрытые для зарубежных адресов.
const SUB_WHITELIST = [
	['🇷🇺 Россия · Москва (банки)', 18], ['🇷🇺 Россия · Москва (госуслуги)', 22],
	['🇷🇺 Россия · Санкт-Петербург', 31], ['🇷🇺 Россия · Екатеринбург', null],
];

const subMembers = (fixture) => [
	nx('Авто | Лучший сервер', 'auto'),
	// kind у фикстурных узлов берётся из неё самой, если он там появится:
	// мок не вправе перекрашивать то, что сверяют Go-тесты.
	...fixture.map((m) => ({ ...m, kind: m.kind || 'node' })),
	...SUB_NODES.map(([n, d]) => nd(n, d)),
	nx('⬇️ Обходы белых списков ⬇️', 'separator'),
	...SUB_WHITELIST.map(([n, d]) => nd(n, d)),
	// Причина — русский текст от демона, дословно той же формы, что уходит
	// в syslog. Панель обязана подать его в переведённой рамке, а не выдать
	// за свой текст.
	nx('🎁 Бонус | Пригласи друга', 'unsupported',
		'ссылка не содержит протокола: happ://invite?ref=…, а не vless:// или ss://'),
];

// Список узлов целиком. Сценарий nikki-no-manifest отдаёт фикстуру как есть —
// так демон отвечает на свежей установке, где манифеста подписки нет и взять
// порядок с видами неоткуда: все строки приезжают узлами.
const readProxies = async () => {
	const d = await readJSON('nikki-proxies.json');
	d.members = state.scenario === 'nikki-no-manifest'
		? d.members.map((m) => ({ ...m, kind: m.kind || 'node' }))
		: subMembers(d.members);
	return d;
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
		const s = { ...base, ...state.overlay, links: linksFor(req), generated_at: nowISO() };
		// configured — задан ли адрес подписки в конфиге роутера. Поля нет ни
		// в одной golden-фикстуре, и дописывает его мок: иначе заблокированную
		// кнопку обновления можно было бы увидеть только на роутере с пустым
		// netmode.main.subscription_url, то есть один раз в жизни, при первой
		// установке — ровно в тот момент, когда панель открывают впервые.
		s.subscription = {
			...s.subscription,
			configured: state.sub.configured !== undefined
				? state.sub.configured
				: state.scenario !== 'sub-unset',
		};
		// Сводка наблюдения едет в статусе (ADR-0043): строка полки обязана
		// быть верной каждую секунду. TTL она НЕ продлевает — здесь это
		// видно прямо: watchState() своих сроков не трогает вовсе.
		const w = await watchState();
		s.watch = w.active
			? {
				ip: w.ip,
				since: w.since,
				engine: w.engine,
				problems: (w.targets || []).filter((x) => x.verdict !== 'ok').length,
			}
			: null;
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

		// Секция ищется в том же списке, что видит панель, — вместе с только
		// что созданными: второй шаг «Сохранить и подключиться» приходит именно
		// с таким id, и файла за ним нет.
		const target = (await currentNetworks()).networks.find((n) => n.id === body.id);
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
				//
				// Текст в ОБОИХ каналах один и тот же, как в switchFailed:
				// он собирается один раз и уходит и в job.error, и в слот.
				// Разные строки здесь означали бы, что панель показывает
				// разное до и после того, как джоб уйдёт из статуса, — а
				// проверять на моке надо ровно переход между ними.
				const detail = upstreamFailText(reason, ssid, prevSsid);
				state.job.state = 'failed';
				state.job.error = detail;
				state.overlay.last_fail = { ssid, reason, detail, at: nowISO() };
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
	// --- проброс LAN в uplink (ADR-0030) ---
	//
	// Порядок отказов повторяет internal/httpapi/bridgehandler.go: тело,
	// затем отпечаток, затем предметные проверки, и только потом job_busy —
	// занятость в реальности выясняется при СТАРТЕ джоба, то есть последней.
	if (p === '/api/bridge' && method === 'GET') {
		const body = await readJSON(BRIDGE_FIXTURES[state.scenario] || 'bridge-disabled.json');
		// Пробы стоят секунды и потому идут только по запросу — как у демона.
		if (u.searchParams.get('probe') === '1') {
			body.probes = {
				pc: body.enabled ? { answered: state.scenario === 'bridge-on' } : null,
				gateway: { answered: true },
				at: nowISO(),
			};
		}
		return send(res, 200, body);
	}
	if (p.startsWith('/api/bridge/') && method === 'POST') {
		const action = p.slice('/api/bridge/'.length);
		if (!['enable', 'disable', 'access'].includes(action)) {
			return fail(res, 404, 'not_found', 'Нет такой операции моста');
		}
		const cur = await readJSON(BRIDGE_FIXTURES[state.scenario] || 'bridge-disabled.json');
		const ifMatch = req.headers['if-match'];
		if (!ifMatch) {
			return fail(res, 409, 'fingerprint_required',
				'Нужен заголовок If-Match с отпечатком из GET /api/bridge');
		}
		if (ifMatch !== cur.fingerprint) {
			return fail(res, 409, 'fingerprint_mismatch',
				'Состояние изменилось с момента чтения — перечитайте и повторите');
		}

		if (action === 'enable') {
			const body = await readBody(req);
			const allowed = ['leg_ip', 'pc_ip', 'ap_access', 'install'];
			if (Object.keys(body).some((k) => !allowed.includes(k))) {
				return fail(res, 400, 'unsupported_field', 'В теле разрешены только ' + allowed.join(', '));
			}
			if (cur.enabled) {
				return fail(res, 409, 'already_enabled', 'Проброс уже включён');
			}
			// Сценарий «пакета нет»: панель обязана спросить согласие и
			// повторить с install:true — ровно этот путь тут и проверяется.
			if (!cur.relayd.installed && !body.install) {
				return fail(res, 409, 'relayd_missing',
					'Пакет relayd не установлен. Панель спросит согласие и повторит с install:true.');
			}
		} else if (action === 'access') {
			const body = await readBody(req);
			if (!cur.enabled) {
				return fail(res, 409, 'bridge_not_enabled',
					'Проброс выключен — доступ настраивается поверх включённого');
			}
			if (!!cur.ap_access === !!body.enabled) {
				return fail(res, 409, 'already_set', 'Доступ уже в запрошенном состоянии');
			}
		} else if (!cur.enabled) {
			return fail(res, 409, 'already_disabled', 'Проброс не включён — демонтировать нечего');
		}

		if (busyJob()) return fail(res, 409, 'job_busy', 'Уже идёт другая операция');

		const reason = BRIDGE_REASON[state.scenario];
		const arg = action === 'access' ? 'access-on' : action;
		const eta = action === 'enable' ? 30 : action === 'disable' ? 20 : 10;
		startJob('bridge', arg, 'Операция проброса: ' + arg, eta, reason ? () => {
			state.job.state = 'failed';
			state.job.error = bridgeFailText(reason);
		} : null);
		return send(res, 202, { job: state.job });
	}
	if (p === '/api/wifi/scan' && method === 'GET') {
		await new Promise((r) => setTimeout(r, 1200)); // скан не мгновенный
		return send(res, 200, await readJSON('wifi-scan.json'));
	}
	if (p === '/api/wifi/networks' && method === 'GET') {
		// При ambiguous обе сети включены — это и есть конфликт: править и
		// добавлять нельзя ни одну (editable: false у обеих), а переключить —
		// можно (switchable: true у обеих, ADR-0026) — выход из
		// неоднозначности и есть единственная разрешённая на ней операция.
		// Отдельной ветки это не требует: раскладка лежит в самой фикстуре.
		return send(res, 200, await currentNetworks());
	}
	if (p === '/api/wifi/networks' && method === 'POST') {
		if (state.scenario === 'ambiguous') {
			return fail(res, 409, 'ambiguous_selection',
				'В конфигурации включено несколько сетей — запись запрещена');
		}
		const body = await readBody(req);
		if (body.id) {
			const target = (await currentNetworks()).networks.find((n) => n.id === body.id);
			if (!target) return fail(res, 404, 'not_found', 'Сеть с таким id не найдена');
			// Запрет постоянный, а не «до конца фазы» (ADR-0026, «Что
			// сохраняется из ADR-0009 дословно»): смена ssid или пароля
			// активной сети рвёт ассоциацию при ближайшем применении, а
			// применение теперь вызываем в том числе мы. Порядок для
			// владельца — сначала переключиться, потом править.
			if (target.enabled) {
				return fail(res, 409, 'enabled_network_readonly',
					'Активную внешнюю сеть менять нельзя: сначала переключитесь на другую');
			}
			state.edits[body.id] = { ...state.edits[body.id] };
			if (body.ssid) state.edits[body.id].ssid = body.ssid;
			if (body.encryption) state.edits[body.id].encryption = body.encryption;
			if (typeof body.key === 'string') state.edits[body.id].has_key = body.key !== '';
		} else {
			// Имя секции — netmode_<8 hex>, как у демона (newSectionName,
			// internal/httpapi/wifiwrite.go). Именованная, а не анонимная:
			// анонимную нельзя адресовать стабильно (ADR-0005), а вторым шагом
			// панель адресует именно её.
			//
			// Создаётся ВЫКЛЮЧЕННОЙ всегда: отсутствие disabled означает
			// «включена», и создать включённую станционную секцию демон не
			// вправе (ADR-0009, сохранено ADR-0026). Отсюда же editable и
			// switchable: выключенную можно и править, и включить.
			state.created.push({
				id: 'netmode_' + crypto.randomBytes(4).toString('hex'),
				ssid: String(body.ssid || ''),
				encryption: body.encryption || 'psk2',
				has_key: typeof body.key === 'string' && body.key !== '',
				network: 'wwan',
				enabled: false,
				editable: true,
				switchable: true,
			});
		}
		// Запись изменила /etc/config/wireless — значит изменился и отпечаток.
		// Он один на статус и на список (см. currentNetworks), поэтому кладётся
		// в overlay: панель тут же шлёт его в If-Match следующим запросом.
		state.overlay.wireless_fingerprint = nextFingerprint();
		// Тело ответа на запись — тот же список, что у GET (openapi: «Записано.
		// Тело — обновлённый список, как у GET»). Прежде здесь лежало
		// { ok: true }, и панель, читающая r.networks вторым шагом составного
		// действия, не находила ничего: проверить «Сохранить и подключиться»
		// в моке было нечем.
		return send(res, 200, await currentNetworks());
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
		const d = await readProxies();
		if (state.overlay.nikkiSelected) d.selected = state.overlay.nikkiSelected;
		// Закрепление — состояние, а не имя. По одному selected «движок
		// подобрал» и «закреплено руками» неразличимы, а панель показывает
		// их по-разному: метка «закреплён», заметка над списком и строка
		// «Авто», которая перестаёт быть активной. Не отдавай мок pinned —
		// автоподбор на нём выглядел бы включённым всегда, в том числе сразу
		// после нажатия на узел, и половину новой разметки было бы не увидеть.
		if (state.overlay.nikkiPinned) { d.pinned = true; d.fixed = state.overlay.nikkiSelected; }
		// Замер обязан пережить перечитывание списка: панель после нажатия
		// зовёт GET, и мок, забывший измеренное, схлопнул бы числа обратно
		// к фикстуре. Тогда «замерили» и «сходили впустую» снова выглядят
		// одинаково — ровно то, ради чего и заводился итог замера.
		const seen = state.overlay.nikkiDelays;
		if (seen) d.members = d.members.map((m) => (m.name in seen ? { ...m, delay_ms: seen[m.name] } : m));
		return send(res, 200, d);
	}
	if (p === '/api/nikki/proxy' && method === 'POST') {
		if (!(await engineUp('nikki'))) {
			return fail(res, 503, 'nikki_unavailable', 'Nikki не отвечает');
		}
		const { name } = await readBody(req);
		// AUTO — не имя узла, а снятие закрепления: выбранный узел остаётся
		// прежним, движок снова волен его сменить. Записать AUTO в selected
		// значило бы показать в списке активной строку, которой в нём нет.
		if (name === 'AUTO') {
			state.overlay.nikkiPinned = false;
			return send(res, 200, { selected: state.overlay.nikkiSelected || '' });
		}
		state.overlay.nikkiSelected = name;
		state.overlay.nikkiPinned = true;
		return send(res, 200, { selected: name }); // быстрая операция: без джоба
	}
	if (p === '/api/nikki/test' && method === 'POST') {
		if (!(await engineUp('nikki'))) {
			return fail(res, 503, 'nikki_unavailable', 'Nikki не отвечает');
		}
		await new Promise((r) => setTimeout(r, 900));
		// Тело — как у GET /api/nikki/proxies плюс test. Прежде здесь стоял
		// {ok:true}, и панельная половина замера проверялась ровно ничем:
		// ответ, не похожий на список, панель молча проглатывала.
		const d = await readProxies();
		if (state.overlay.nikkiSelected) d.selected = state.overlay.nikkiSelected;
		if (state.overlay.nikkiPinned) { d.pinned = true; d.fixed = state.overlay.nikkiSelected; }
		// Живые узлы получают новые числа, мёртвые остаются с null — иначе
		// «замерили» и «сходили впустую» выглядят одинаково.
		//
		// Сценарий, где наружу не выбрался никто, — единственный способ
		// увидеть тост про полный провал, не выдёргивая кабель. Где кончился
		// бюджет — способ увидеть частичный итог; там последние два узла
		// остаются НЕ ЗАМЕРЕННЫМИ, а не проваленными, и числа у них прежние.
		const dead = state.scenario === 'nikki-test-dead';
		const budget = state.scenario === 'nikki-test-slow' ? 2 : 0;
		// Замеряются ТОЛЬКО узлы. Разделитель и «Авто» демон в замер не берёт
		// (контракт TestSummary.total), и попади они туда — осели бы в failed
		// как мёртвые узлы, которых не существует: итог «не ответили двое»
		// про строки, у которых нет сервера, — это ложь, а не округление.
		const total = d.members.filter((m) => m.kind === 'node').length;
		const cut = total - budget;
		let seen = 0;
		let measured = 0;
		let failed = 0;
		d.members = d.members.map((m) => {
			if (m.kind !== 'node') return m;
			if (seen++ >= cut) return m; // до этого узла замер не дошёл
			if (dead || m.delay_ms == null) { failed++; return { ...m, delay_ms: null }; }
			measured++;
			return { ...m, delay_ms: 20 + ((m.delay_ms * 7) % 180) };
		});
		// Измеренное запоминается: панель сразу после нажатия перечитывает
		// список, и мок, забывший результат, схлопнул бы числа обратно к
		// фикстуре — «замерили» опять стало бы неотличимо от «сходили зря».
		state.overlay.nikkiDelays = Object.fromEntries(
			d.members.filter((m) => m.kind === 'node').map((m) => [m.name, m.delay_ms]));
		return send(res, 200, {
			...d,
			test: {
				total,
				measured,
				failed,
				skipped: total - measured - failed,
				elapsed_ms: 900,
			},
		});
	}

	// --- наборы geosite (internal/httpapi/rulesetshandlers.go) ---
	//
	// Список имён — с GitHub, а не с роутера, и потому у него своё отдельное
	// состояние отказа (503 catalog_unavailable), которого у остального API
	// нет: применённый выбор при этом продолжает читаться как ни в чём не
	// бывало — он лежит в файле и каталога не спрашивает.
	// Авто-пул. Своего примера у него нет: список узлов совпадает с тем,
	// что отдаёт /api/nikki/proxies, и разводить два источника значило бы
	// показывать в панели два разных списка одних и тех же серверов.
	if (p === '/api/nikki/autopool' && method === 'GET') {
		return send(res, 200, await autopoolBody());
	}
	if (p === '/api/nikki/autopool' && method === 'PUT') {
		const body = await readBody(req);
		if (!['deny', 'allow', 'provider'].includes(body.mode)) {
			return fail(res, 400, 'bad_request', 'Неизвестный режим авто-пула «' + body.mode + '»: ожидались deny, allow или provider.');
		}
		const cur = await autopoolBody();
		const got = (req.headers['if-match'] || '').trim();
		if (!got) {
			return fail(res, 409, 'stale_autopool', 'Нужен заголовок If-Match с отпечатком из GET /api/nikki/autopool: без него запись не докажет, что видела нынешний выбор.');
		}
		if (got !== cur.fingerprint) {
			return fail(res, 409, 'stale_autopool', 'Авто-пул изменился, пока вы его правили: перечитайте GET /api/nikki/autopool и повторите с новым отпечатком.');
		}
		if (body.mode === 'provider' && cur.provider_pool.length === 0) {
			return fail(res, 409, 'no_provider_pool', 'В подписке нет своего авторежима: балансировщика у провайдера не нашлось, и брать состав пула неоткуда. Выберите узлы вручную.');
		}
		const nodes = body.mode === 'provider' ? cur.provider_pool : (body.nodes || []);
		const unknown = nodes.filter((n) => !cur.available.includes(n));
		if (unknown.length > 0) {
			return fail(res, 400, 'unknown_node', 'В подписке нет таких узлов: ' + unknown.slice(0, 3).join(', ') + '. Список узлов мог измениться — перечитайте GET /api/nikki/autopool.');
		}
		const left = body.mode === 'deny' ? cur.available.filter((n) => !nodes.includes(n)).length : nodes.length;
		if (left === 0) {
			return fail(res, 400, 'empty_pool', 'После такого выбора в авто-пуле не остаётся ни одного узла. Снимите часть отметок или смените режим.');
		}
		startJob('autopool', '', 'Применение авто-пула', 6, () => {
			// Отпечаток обязан смениться: иначе оптимистичная блокировка в
			// моке ненастоящая, и путь stale_autopool в панели не нажать.
			state.overlay.autopool = { mode: body.mode, nodes, pool_size: left, fingerprint: nextFingerprint() };
		});
		return send(res, 202, { job: state.job });
	}
	if (p === '/api/nikki/rulesets/catalog' && method === 'GET') {
		if (state.scenario === 'rulesets-nocatalog') {
			return fail(res, 503, 'catalog_unavailable',
				'Список наборов geosite не загружен: api.github.com не ответил за отведённое время');
		}
		return send(res, 200, await readJSON('nikki-rulesets-catalog.json'),
			MIME['.json'], 'private, max-age=3600');
	}

	if (p === '/api/nikki/rulesets' && method === 'GET') {
		const base = await readJSON(RULESETS[state.scenario] || RULESETS['rulesets-profile']);
		const body = { ...base, ...state.overlay.rulesets };
		// live:false и null в полях — либо явный сценарий отказа, либо тот же
		// расчёт, что у остального API: движок молчит, а выбор на диске мы
		// всё равно показываем (rulesetsBody, haveLive:false).
		if (state.scenario === 'rulesets-down' || !(await engineUp('nikki'))) {
			body.live = false;
			body.sets = body.sets.map((s) => ({ ...s, loaded: null, rules: null, updated_at: null }));
		}
		return send(res, 200, body);
	}
	if (p === '/api/nikki/rulesets' && method === 'PUT') {
		const body = await readBody(req);
		// Форма до ADR-0041 отбивается с подсказкой, как у демона.
		if (body.policy === 'only' || body.policy === 'except') {
			return fail(res, 400, 'bad_request', 'Политика ' + body.policy + ' переименована: направление теперь у каждого набора (sets — объекты {name, action}), а policy — куда идёт остальное: direct, tunnel или profile.');
		}
		if (!['profile', 'direct', 'tunnel'].includes(body.policy)) {
			return fail(res, 400, 'bad_request', 'Неизвестная политика наборов geosite');
		}
		if (!Array.isArray(body.sets)) {
			return fail(res, 400, 'bad_request', 'Поле sets обязано быть списком наборов');
		}
		if (body.sets.some((x) => typeof x !== 'object' || !x)) {
			return fail(res, 400, 'bad_request', 'Наборы теперь с направлением: sets — объекты {"name": …, "action": "tunnel"|"direct"}, а не строки.');
		}
		for (const x of body.sets) {
			if (!['tunnel', 'direct'].includes(x.action)) {
				return fail(res, 400, 'bad_request', 'У набора ' + x.name + ' неизвестное направление ' + JSON.stringify(x.action) + ' — бывают tunnel, direct');
			}
		}
		// Свои правила — до каталога, как у демона (шаг 1а): опечатка в
		// домене от каталога не зависит.
		const rules = body.rules || [];
		if (body.policy === 'profile' && rules.length > 0) {
			return fail(res, 400, 'bad_rule',
				`Правило 1 (${rules[0].kind} ${rules[0].value}) не принято: политика "profile" не может идти со своими правилами`);
		}
		const why = badRule(rules);
		if (why) return fail(res, 400, 'bad_rule', why);
		const names = body.sets.map((x) => x.name);
		const actionOf = new Map(body.sets.map((x) => [x.name, x.action]));
		const cat = await readJSON('nikki-rulesets-catalog.json');
		const catNames = new Set(cat.names);
		const unknown = names.filter((n) => !catNames.has(n));
		if (unknown.length) {
			return fail(res, 400, 'unknown_set', 'Таких наборов нет в списке: ' + unknown.join(', '));
		}
		// Отпечаток берётся из overlay, если применение уже случалось на этом
		// сценарии, иначе — из фикстуры: та же пара источников, что и у
		// самого GET чуть выше.
		const cur = await readJSON(RULESETS[state.scenario] || RULESETS['rulesets-profile']);
		const fp = (state.overlay.rulesets && state.overlay.rulesets.fingerprint) || cur.fingerprint;
		const ifMatch = req.headers['if-match'];
		// Отсутствие заголовка — тот же отказ, что и несовпадение (реальный
		// обработчик отбивает оба случая одним stale_rulesets,
		// rulesetshandlers.go:403-408): автор панели обязан узнать про
		// обязательность If-Match здесь, а не на роутере.
		if (!ifMatch) {
			return fail(res, 409, 'stale_rulesets',
				'Нужен заголовок If-Match с отпечатком из GET /api/nikki/rulesets: без него запись не докажет, что видела нынешний выбор.');
		}
		if (ifMatch !== fp) {
			return fail(res, 409, 'stale_rulesets',
				'Выбор наборов изменился, пока вы его правили: перечитайте GET /api/nikki/rulesets и повторите с новым отпечатком.');
		}
		if (busyJob()) return fail(res, 409, 'job_busy', 'Уже идёт другая операция. Дождитесь её завершения.');

		const ip = new Set(cat.ip);
		startJob('rulesets', '', 'Применение наборов geosite', 6, () => {
			// Последний набор нарочно не загружается — только когда наборов
			// хотя бы два: панели есть на чём показать тег «не загрузился»
			// рядом с загруженными, а не гадать по единственной строке,
			// нормально это или нет.
			const forceMiss = names.length >= 2;
			const sets = names.map((name, i) => {
				const missed = forceMiss && i === names.length - 1;
				return {
					name,
					ip: ip.has(name),
					action: actionOf.get(name) || 'tunnel',
					loaded: !missed,
					rules: missed ? 0 : 100 + i,
					updated_at: missed ? null : nowISO(),
				};
			});
			state.overlay.rulesets = {
				fingerprint: nextFingerprint(),
				policy: body.policy,
				download: body.policy === 'profile' ? 'direct' : (body.download || 'direct'),
				tunnel_group: 'BYPASS',
				sets: body.policy === 'profile' ? [] : sets,
				// Как прислали, в том же порядке: демон отдаёт правила из
				// файла, а файл пишется из тела.
				rules: body.policy === 'profile' ? [] : rules.map((r) => ({ kind: r.kind, value: r.value, action: r.action, comment: r.comment || '' })),
				live: true,
				foreign: false,
			};
			state.job.state = 'done';
		});
		return send(res, 202, { job: state.job });
	}

	// --- b4 ---
	// Сеты не эксклюзивны (ADR-0033): наложение хранит МНОЖЕСТВО включённых
	// id, а не один выбранный. Прежнее `b4SetId` умело выражать только
	// «включён ровно этот», то есть мок физически не мог воспроизвести ни
	// «ни одного», ни «несколько» — два из трёх состояний, которые панель
	// теперь обязана показывать по-разному.
	const b4Body = async () => {
		const d = await readJSON('b4-sets.json');
		if (state.b4.enabled) {
			const on = new Set(state.b4.enabled);
			d.sets = d.sets.map((x) => ({ ...x, enabled: on.has(x.id) }));
		}
		const enabled = d.sets.filter((x) => x.enabled);
		d.enabled_count = enabled.length;
		// Пусто и при нуле, и при двух и более: единственный честный ответ на
		// «через какой сет идёт трафик», когда ответа нет (b4.Selected).
		d.selected = enabled.length === 1 ? enabled[0].name : '';
		return d;
	};

	if (p === '/api/b4/sets' && method === 'GET') {
		if (!(await engineUp('b4'))) {
			return fail(res, 503, 'b4_unavailable', 'Панель b4 не отвечает');
		}
		return send(res, 200, await b4Body());
	}
	if (p === '/api/b4/set' && method === 'POST') {
		if (!(await engineUp('b4'))) {
			return fail(res, 503, 'b4_unavailable', 'Панель b4 не отвечает');
		}
		const body = await readBody(req);
		const id = body && body.id;
		if (!id) return fail(res, 400, 'bad_request', 'Не указан id сета');
		// Отсутствующий enabled — 400, а не «выключить»: тот же отказ, что
		// у демона, иначе мок учил бы панель работать так, как боевой
		// обработчик не работает.
		if (typeof (body && body.enabled) !== 'boolean') {
			return fail(res, 400, 'bad_request', 'Не указано enabled');
		}
		const cur = await b4Body();
		const hit = cur.sets.find((x) => x.id === id);
		if (!hit) return fail(res, 404, 'not_found', 'Сет не найден');
		const on = new Set(cur.sets.filter((x) => x.enabled).map((x) => x.id));
		if (body.enabled) on.add(id); else on.delete(id);
		state.b4.enabled = [...on];
		// Тело как у GET — так же, как отвечает демон: панели список нужен
		// сразу, а второй запрос был бы гонкой с правкой из морды b4.
		return send(res, 200, await b4Body()); // быстрая операция
	}

	// --- подписка и логи ---

	// Маска адреса — ровно та же, что у демона (internal/httpapi/
	// subscriptionurl.go): схема, хост, путь и ИМЕНА параметров, значения
	// многоточием. Мок, показывающий больше, учил бы панель верстать то,
	// чего на роутере не приедет.
	const maskSub = (raw) => {
		if (!raw) return '';
		let u;
		try { u = new URL(raw); } catch { return 'адрес задан, но не разбирается как URL'; }
		let out = u.protocol + '//' + u.host + u.pathname;
		const keys = [...u.searchParams.keys()].sort().map((k) => k + '=…');
		if (keys.length) out += '?' + keys.join('&');
		if (u.hash) out += '#…';
		return out;
	};
	// Начальное значение зависит от сценария: sub-unset — свежая установка,
	// где адрес ещё не задан. Иначе адрес есть и показан маской.
	const subURL = () => (state.sub.url !== undefined
		? state.sub.url
		: (state.scenario === 'sub-unset' ? '' : 'https://sub.example.net/api/v1/client/subscribe?token=S3CRET'));

	if (p === '/api/subscription' && method === 'GET') {
		const raw = subURL();
		return send(res, 200, { configured: raw !== '', masked: maskSub(raw) });
	}
	if (p === '/api/subscription' && method === 'PUT') {
		const body = await readBody(req);
		const raw = typeof (body && body.url) === 'string' ? body.url.trim() : null;
		if (raw === null) return fail(res, 400, 'bad_request', 'Тело запроса не разбирается как JSON');
		// Пустая строка стирает настройку — это валидное значение, а не отказ.
		if (raw !== '') {
			let u;
			try { u = new URL(raw); } catch { u = null; }
			if (!u || !u.host) return fail(res, 400, 'bad_request', 'Адрес не разбирается как URL');
			if (u.protocol !== 'http:' && u.protocol !== 'https:') {
				return fail(res, 400, 'bad_request', 'Адрес подписки обязан начинаться с http:// или https://');
			}
		}
		state.sub.url = raw;
		// Признак в статусе обязан пойти за записью: иначе панель показывает
		// «адрес не задан» рядом с кнопкой, которая уже работает.
		state.sub.configured = raw !== '';
		return send(res, 200, { configured: raw !== '', masked: maskSub(raw) });
	}

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

	// --- наблюдатель трафика устройства (ADR-0043) ---

	if (p === '/api/watch/hosts' && method === 'GET') {
		// Список устройств отвечает при ЛЮБОМ режиме, в том числе при
		// выключенном движке: он нужен панели ровно затем, чтобы объяснить
		// владельцу, почему наблюдать нечего.
		return send(res, 200, await readJSON('watch-hosts.json'));
	}

	if (p === '/api/watch' && method === 'GET') {
		return send(res, 200, await watchState());
	}

	if (p === '/api/watch' && method === 'POST') {
		const body = await readBody(req);
		const ip = String(body.ip || '');
		if (!/^\d{1,3}(\.\d{1,3}){3}$/.test(ip)) {
			return fail(res, 400, 'bad_ip', 'Нужен адрес IPv4 устройства из локальной сети');
		}
		if (!ip.startsWith('192.168.9.')) {
			return fail(res, 400, 'bad_ip', 'Адрес не из локальной сети 192.168.9.0/24');
		}
		const mode = (await currentStatus()).mode;
		if ((state.overlay.mode || mode) !== 'nikki') {
			return fail(res, 409, 'engine_off', 'Наблюдатель читает решения движка Nikki; сейчас режим другой');
		}
		state.watch = { ip, since: Date.now(), engine: 'running', unparsed: 0, quiet: state.scenario === 'watch-quiet' };
		return send(res, 200, await watchState());
	}

	if (p === '/api/watch' && method === 'DELETE') {
		state.watch = null;
		res.writeHead(204, { 'Cache-Control': 'no-store' });
		return res.end();
	}

	return fail(res, 404, 'not_found', `Нет обработчика для ${method} ${p}`);
}

// watchState собирает состояние сессии из golden-фикстуры, наращивая числа
// от момента старта.
//
// Растут они не для красоты: экран обязан переживать смену чисел раз в
// секунду, не двигая раскладку, а на застывшем ответе это свойство
// непроверяемо. Пометка «новый» гаснет сама через 30 секунд — её считает
// демон, и мок обязан вести себя так же.
async function watchState() {
	const w = state.watch;
	if (!w) return { active: false, unparsed: 0, dropped: 0 };
	const secs = Math.max(0, Math.floor((Date.now() - w.since) / 1000));
	const base = await readJSON('watch.json');
	const restarting = w.engine === 'restarting';
	const targets = w.quiet ? [] : base.targets.map((x, i) => {
		const up = x.rate_up * secs;
		const down = x.rate_down * secs;
		return {
			...x,
			up: x.up + up,
			down: x.down + down,
			// При перезагрузке движка живых соединений нет, а счётчики
			// остаются на прежних числах: они продолжатся, а не начнутся
			// заново.
			live: restarting ? 0 : x.live,
			rate_up: restarting ? 0 : x.rate_up,
			rate_down: restarting ? 0 : x.rate_down,
			new: i === 1 && secs < 30,
		};
	});
	return {
		active: true,
		ip: w.ip,
		since: new Date(w.since).toISOString(),
		engine: w.engine,
		unparsed: w.unparsed,
		dropped: 0,
		targets,
	};
}

const server = http.createServer(async (req, res) => {
	const u = new URL(req.url, 'http://localhost');

	// Переключение сценария — служебный маршрут мока, вне контракта.
	if (u.pathname === '/__scenario') {
		const name = u.searchParams.get('name');
		if (name && SCENARIOS[name]) {
			state.scenario = name;
			// Обнуляет и state.overlay.rulesets: применённый на прошлом
			// сценарии выбор наборов — часть overlay, а не отдельное поле,
			// и без сброса rulesets-profile после PUT на rulesets-only
			// показывал бы чужое применение вместо своей фикстуры.
			state.overlay = {};
			// Сбрасываются вместе с наложением: сценарий sub-unset обязан
			// давать «адрес не задан» и после того, как в прошлом сценарии
			// его записали через PUT.
			state.sub = {};
			state.b4 = {};
			state.created = [];
			state.edits = {};
			state.job = null;
			// Окно молчания и срок показа провала отсчитываются от переключения
			// сценария: оба состояния кратковременны, и наблюдать их надо
			// с самого начала. Пересмотреть — переключить сценарий заново.
			arm(PREARM[name] || '');
			state.jobUntil = name === 'job-fail-vanish' ? Date.now() + JOB_KEEP_MS : 0;
			// Сессия наблюдения заводится вместе со сценарием: ждать нажатия
			// значило бы прятать за кликом весь экран, ради которого сценарий
			// и выбран.
			state.watch = WATCH[name] ? { ip: '192.168.9.219', since: Date.now(), ...WATCH[name] } : null;
		}
		return send(res, 200, { scenario: state.scenario, available: Object.keys(SCENARIOS) });
	}

	// Подмена часов «роутера» — служебный маршрут мока, вне контракта.
	//
	// Сценарием это быть не может: сдвиг ортогонален состоянию роутера и
	// проверяется вместе с любым из них. Секунды, а не миллисекунды: интересны
	// величины от минут (дрейф) до часов (коробка без RTC), и в миллисекундах
	// такие числа только неудобно набирать.
	if (u.pathname === '/__clock') {
		const v = Number(u.searchParams.get('skew'));
		if (Number.isFinite(v)) skewMs = v * 1000;
		return send(res, 200, { skew_sec: skewMs / 1000, router_now: nowISO() });
	}

	if (u.pathname.startsWith('/api/')) {
		try {
			return await handleAPI(req, res, u);
		} catch (e) {
			return fail(res, 500, 'internal', String(e && e.message));
		}
	}

	// Статика. Пустой корень — не отдавать её вовсе: под дев-сервером Vite
	// он отдаёт статику сам, а 404 отсюда честнее, чем чужой index.html.
	if (!WEB) return send(res, 404, 'static disabled (MOCK_STATIC=)', 'text/plain; charset=utf-8');

	// Версия в query отбрасывается: имя файла стабильное, а ?v= служит
	// только кэшу браузера (ADR-0036).
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
