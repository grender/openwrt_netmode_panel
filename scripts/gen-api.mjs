#!/usr/bin/env node
// Порождает клиентскую часть контракта: пути и таксономию причин провала.
//
// ЗАЧЕМ. Гейт check-routes.sh перечисляет литералы `/api/...` в панели и
// сверяет их с openapi.yaml. Перечисление работает, пока путь написан
// литералом. Стоит собрать его из кусков — `/api/${section}/${verb}` — и
// перечисление придёт коротким, членство пройдёт вхолостую, а гейт
// останется зелёным над маршрутом, которого он не видел. Это ровно тот
// отказ, ради которого check-routes.sh и был написан: мёртвая кнопка
// «Замерить все» проехала через тринадцать зелёных проверок.
//
// Поэтому направление переворачивается. Пути живут в ОДНОМ порождённом
// файле, а гейт запрещает сырой литерал `/api` где бы то ни было ещё.
// Генерация отвечает за «каждая запись есть в контракте», запрет — за
// «каждый вызов идёт через запись». Одно без другого не работает.
//
// Node, только stdlib — в традиции web/mock-server.mjs и
// scripts/measure-geometry.mjs. Своего разбора YAML здесь нет: он берётся
// тем же ruby-затем-python3 путём, что уже есть в check-routes.sh. Писать
// третий разбор YAML в проекте, где два уже есть, значит завести третье
// место, которое разойдётся с первыми двумя.
import { execFileSync } from 'node:child_process';
import { readFileSync, writeFileSync, mkdirSync, existsSync } from 'node:fs';
import { dirname } from 'node:path';

const SPEC = 'docs/api/openapi.yaml';
const OUT_ROUTES = 'web/src/api/routes.gen.ts';
const OUT_REASONS = 'web/src/api/reasons.gen.ts';

// --check — режим гейта: ничего не пишет, сверяет содержимое и краснеет на
// расхождении. Отдельный режим, а не «сгенерируй во временный каталог и
// сравни»: временный каталог пришлось бы за собой убирать, а гейт обязан
// быть простым настолько, чтобы его не хотелось обойти.
const CHECK = process.argv.includes('--check');

// ─────────── YAML → JSON ───────────

function loadSpec() {
	const ruby = [
		'-ryaml', '-rjson', '-e',
		'print JSON.generate(YAML.safe_load(File.read(ARGV[0]), aliases: true))',
		SPEC,
	];
	const python = [
		'-c',
		'import sys,yaml,json;print(json.dumps(yaml.safe_load(open(sys.argv[1],encoding="utf-8"))))',
		SPEC,
	];
	for (const [bin, args] of [['ruby', ruby], ['python3', python]]) {
		try {
			return JSON.parse(execFileSync(bin, args, { encoding: 'utf8', maxBuffer: 64 << 20 }));
		} catch {
			// Следующий разборщик. Отличать «нет ruby» от «ruby упал на
			// синтаксисе» здесь незачем: синтаксис контракта проверяет
			// check-routes.sh, и падает он там громко и один раз.
		}
	}
	if (CHECK) {
		// Пропуск, а не отказ, — по той же причине и в тех же словах, что
		// у проверки синтаксиса YAML в check-routes.sh: гейт обязан
		// работать на машине, где стоит только Go. Но молчать нельзя.
		console.log('-- gen-api: YAML-парсера нет, свежесть порождённого клиента НЕ проверена');
		process.exit(0);
	}
	console.error('gen-api: не нашлось ни ruby, ни python3 с pyyaml — контракт разобрать нечем');
	console.error('  порождённые файлы коммитятся, поэтому сборка панели без них не встанет;');
	console.error('  но обновить их без разборщика YAML нельзя.');
	process.exit(1);
}

// ─────────── имена ───────────

// Путь `/api/wifi/networks/{id}` → `wifiNetworkById`. Имя выводится, а не
// пишется руками: рукописное разошлось бы с путём молча, и `ROUTES.foo`
// указывал бы не туда, куда думает читатель.
function nameFor(path) {
	const parts = path.replace(/^\/api\//, '').split('/').filter(Boolean);
	const words = [];
	for (const p of parts) {
		const m = /^\{(.+)\}$/.exec(p);
		if (m) {
			words.push('by', m[1]);
			continue;
		}
		words.push(...p.split(/[-_.]/).filter(Boolean));
	}
	return words
		.map((w, i) => (i === 0 ? w : w[0].toUpperCase() + w.slice(1)))
		.join('');
}

// ─────────── таксономия причин ───────────
//
// Причины провала лежат в контракте перечислениями внутри схем. Ищем их по
// имени схемы, а не по позиции: порядок ключей в YAML менять можно.
function reasonsOf(spec, schemaName, field) {
	const sch = spec?.components?.schemas?.[schemaName];
	const enumv = sch?.properties?.[field]?.enum;
	if (!Array.isArray(enumv) || enumv.length === 0) {
		console.error(`gen-api: в схеме ${schemaName}.${field} нет перечисления причин`);
		console.error('  контракт изменился — почините генератор, а не глушите его');
		process.exit(1);
	}
	return enumv.slice();
}

// ─────────── запись ───────────

const HEAD = `// ПОРОЖДЁННЫЙ ФАЙЛ. Правки будут затёрты.
//
// Источник: ${SPEC}
// Обновить: node scripts/gen-api.mjs
//
// Файл коммитится: \`make verify\` обязан работать на машине, где есть
// только Go и POSIX-шелл, а разборщик YAML нужен лишь для ОБНОВЛЕНИЯ.
// Свежесть сверяет scripts/check-routes.sh.
`;

let drift = 0;

function writeOut(file, body) {
	const want = HEAD + body;
	if (CHECK) {
		if (!existsSync(file)) {
			console.error(`gen-api: нет порождённого файла ${file}`);
			drift++;
			return;
		}
		if (readFileSync(file, 'utf8') !== want) {
			console.error(`gen-api: ${file} разошёлся с контрактом`);
			drift++;
		}
		return;
	}
	mkdirSync(dirname(file), { recursive: true });
	writeFileSync(file, want);
	console.log(`-- gen-api: ${file}`);
}

const spec = loadSpec();

const paths = Object.keys(spec.paths || {}).filter((p) => p.startsWith('/api')).sort();
if (paths.length === 0) {
	console.error('gen-api: в контракте не нашлось ни одного пути /api — почините разбор');
	process.exit(1);
}

const seen = new Map();
for (const p of paths) {
	const n = nameFor(p);
	if (seen.has(n)) {
		console.error(`gen-api: имена столкнулись: ${seen.get(n)} и ${p} → ${n}`);
		console.error('  почините nameFor — молчаливое переименование увело бы вызов не туда');
		process.exit(1);
	}
	seen.set(n, p);
}

const routeLines = paths.map((p) => `\t${nameFor(p)}: '${p}',`).join('\n');

writeOut(OUT_ROUTES, `
export const ROUTES = {
${routeLines}
} as const;

export type RouteName = keyof typeof ROUTES;

/**
 * Подставляет параметры пути: path('wifiNetworksById', { id }).
 *
 * encodeURIComponent обязателен — имя секции приходит из конфигурации
 * роутера и вполне может содержать что угодно.
 */
export function path(name: RouteName, params?: Record<string, string>): string {
	let out: string = ROUTES[name];
	if (params) {
		for (const [k, v] of Object.entries(params)) {
			out = out.split('{' + k + '}').join(encodeURIComponent(v));
		}
	}
	if (out.includes('{')) {
		// Незаполненная дырка — это запрос по пути, которого нет. Тихо
		// отправить его значит получить 404 и искать причину в демоне.
		throw new Error('routes: не заполнен параметр пути: ' + out);
	}
	return out;
}
`);

// Имена схем те же, что читает scripts/check-fail-reasons.sh (LastFail и
// BridgeLastFail). Держать их в двух местах приходится, но разойтись молча
// они не могут: гейт сверяет ПЯТЬ источников, и порождённый отсюда файл —
// один из них.
const upstream = reasonsOf(spec, 'LastFail', 'reason');
const bridge = reasonsOf(spec, 'BridgeLastFail', 'reason');

const list = (xs) => xs.map((x) => `\t'${x}',`).join('\n');

writeOut(OUT_REASONS, `
/** Причины, по которым не состоялась смена внешней сети (ADR-0025). */
export const UPSTREAM_REASONS = [
${list(upstream)}
] as const;

/** Причины, по которым не состоялась операция проброса (ADR-0030). */
export const BRIDGE_REASONS = [
${list(bridge)}
] as const;

export type UpstreamReason = (typeof UPSTREAM_REASONS)[number];
export type BridgeReason = (typeof BRIDGE_REASONS)[number];

/**
 * Причина, которой панель не знает, — это не ошибка панели: демон и панель
 * обновляются порознь, и незнакомый код приедет раньше своего перевода.
 * Поэтому у обеих таксономий есть запасной ключ 'unknown', и он ОБЯЗАН
 * быть в словарях (это проверяет scripts/check-fail-reasons.sh).
 */
export const UNKNOWN_REASON = 'unknown';

export function upstreamReason(code: string): UpstreamReason | typeof UNKNOWN_REASON {
	return (UPSTREAM_REASONS as readonly string[]).includes(code)
		? (code as UpstreamReason)
		: UNKNOWN_REASON;
}

export function bridgeReason(code: string): BridgeReason | typeof UNKNOWN_REASON {
	return (BRIDGE_REASONS as readonly string[]).includes(code)
		? (code as BridgeReason)
		: UNKNOWN_REASON;
}
`);

if (CHECK) {
	if (drift > 0) {
		console.error('  контракт изменился, а клиент панели — нет. Обновите:');
		console.error('    node scripts/gen-api.mjs && git add web/src/api');
		process.exit(1);
	}
	console.log('-- gen-api: порождённый клиент свеж относительно контракта');
}
