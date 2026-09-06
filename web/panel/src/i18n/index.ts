// Словари и подстановка.
//
// Русский — ИСХОДНИК: строки берутся из макета дословно, потому что тихий
// снос смысла при копировании не отрецензировать. Английский — перевод, и
// когда ключа в нём нет, берётся русский.
//
// Ключи плоские, с точками, без вложенности и без правил множественного
// числа. Так было в панели до сборки, и менять это незачем: вложенность
// усложняет grep, а grep по ключу — то, чем живёт scripts/check-fail-reasons.sh.
//
// JSON, а не TypeScript, и это не косметика. Гейт вычитывает ключи
// `wifi.fail.<код>.title` из обеих локалей текстом. Прежний формат требовал
// табов и одинарных кавычек — то есть был контрактом на ФОРМАТИРОВАНИЕ,
// который первый же прогон prettier сломал бы, и гейт покраснел бы без
// единой продуктовой причины. У JSON одна форма кавычек, нет значимых
// отступов и нет порядка: переформатировать его можно как угодно, а grep
// по ключу всё равно найдёт всё.
import ru from './ru.json';
import en from './en.json';

export type Lang = 'ru' | 'en';
export type Key = keyof typeof ru;

// Английский обязан покрывать русский ключ в ключ. Проверка типом, а не
// тестом: недостающий перевод — ошибка компиляции, то есть узнаётся до
// сборки, а не по пустому месту в интерфейсе.
const _en: Record<Key, string> = en;

const DICT: Record<Lang, Record<Key, string>> = { ru, en: _en };

export type Vars = Record<string, string | number>;

/**
 * Подстановка простым split/join — ровно как было. Regexp тут не нужен:
 * значения приходят из контракта и вполне могут содержать спецсимволы,
 * а экранировать их пришлось бы вручную и однажды забыть.
 */
export function makeT(lang: Lang) {
	const d = DICT[lang] ?? DICT.ru;
	return (key: Key, vars?: Vars): string => {
		// Пропущенный ключ отдаётся КАК КЛЮЧ, а не пустой строкой: пустое
		// место в интерфейсе выглядит как задуманное и живёт годами, а
		// «wifi.connect» на кнопке чинится при первом же взгляде.
		let s: string = d[key] ?? DICT.ru[key] ?? key;
		if (vars) {
			for (const [k, v] of Object.entries(vars)) {
				s = s.split('{' + k + '}').join(String(v));
			}
		}
		return s;
	};
}

export type T = ReturnType<typeof makeT>;

const STORE = 'netmode.lang';

export function loadLang(): Lang {
	// navigator.language не нюхаем намеренно: панель русская по умолчанию,
	// и владелец, у которого система на английском, получил бы английскую
	// панель к русскому роутеру.
	try {
		return localStorage.getItem(STORE) === 'en' ? 'en' : 'ru';
	} catch {
		// Приватный режим и прочие места, где localStorage кидает. Язык —
		// удобство; падать из-за него панель не должна.
		return 'ru';
	}
}

export function saveLang(lang: Lang): void {
	try {
		localStorage.setItem(STORE, lang);
	} catch {
		/* см. loadLang */
	}
	document.documentElement.lang = lang;
}

/**
 * Время роутера в местном формате. Часы браузера и роутера расходятся
 * (на боксе без RTC — на часы), поэтому показывается ровно то, что прислал
 * демон, без пересчёта.
 */
export function fmtTime(iso: string | null | undefined, lang: Lang): string {
	if (!iso) return '—';
	const d = new Date(iso);
	if (Number.isNaN(d.getTime())) return '—';
	return d.toLocaleString(lang === 'en' ? 'en-GB' : 'ru-RU', {
		day: '2-digit',
		month: '2-digit',
		hour: '2-digit',
		minute: '2-digit',
	});
}
