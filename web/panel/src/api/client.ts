// Единственное место, где панель ходит в сеть.
//
// Перенесено из панели без сборки почти дословно: бюджеты, форма ошибки и
// порядок сборки заголовков доказаны на роутере, и переписывать их «покрасивее»
// значит проверять заново то, что уже проверено.

import { path, type RouteName } from './routes.gen';

/** Бюджеты. Каждый — про то, сколько демон РЕАЛЬНО может думать. */
export const T = {
	/** Статус обязан уложиться внутрь интервала опроса. */
	STATUS: 4000,
	/** Побочные списки и адрес панели Nikki. */
	SIDE: 8000,
	/** Скан эфира: радио замолкает надолго. */
	SCAN: 20000,
	/** Смена режима, аплинка, проброса: 202 приходит сразу, ждать нечего. */
	MODE: 8000,
	/** Замер задержек: у демона на него бюджет 9 с плюс накладные. */
	TEST: 15000,
	DEFAULT: 8000,
} as const;

/** Ошибка запроса, несущая и HTTP-код, и машинный код тела. */
export class ApiError extends Error {
	readonly status: number;
	/** Код из тела ответа. Пусто, если тело не разобралось. */
	readonly code: string;

	constructor(message: string, status: number, code: string) {
		super(message);
		this.name = 'ApiError';
		this.status = status;
		this.code = code;
	}
}

export interface ApiOptions {
	method?: string;
	body?: string;
	headers?: Record<string, string>;
	signal?: AbortSignal;
	timeoutMs?: number;
	/** Параметры пути и строки запроса. */
	params?: Record<string, string>;
	query?: Record<string, string>;
}

/**
 * Запрос к демону.
 *
 * Маршрут — ИМЯ из порождённого контракта, а не строка. Сырой путь вне
 * порождённых файлов запрещён гейтом check-routes.sh: перечисление
 * литералов перестаёт работать, как только путь собирается из кусков, и
 * гейт зеленеет над маршрутом, которого не видел. Один раз это уже стоило
 * проекту мёртвой кнопки. Запрет распространяется и на комментарии — так
 * правило остаётся одним и не требует разбирать, где код, а где текст.
 *
 * Авторизации панель не шлёт. Демон ставит cookie netmode_token при первом
 * заходе по ?token=… и дальше всё едет на ней (ADR-0014).
 */
export async function api<TResponse>(route: RouteName, opts: ApiOptions = {}): Promise<TResponse> {
	const { timeoutMs = T.DEFAULT, params, query, ...rest } = opts;

	// AbortController с таймером, а не AbortSignal.timeout(): у второго нет
	// способа объединить свой сигнал с чужим, а отменять запрос снаружи
	// панели надо (уход со страницы, вытеснение более свежим запросом).
	const ctrl = new AbortController();
	const timer = setTimeout(() => ctrl.abort(), timeoutMs);
	if (rest.signal) {
		if (rest.signal.aborted) ctrl.abort();
		else rest.signal.addEventListener('abort', () => ctrl.abort(), { once: true });
	}

	let url = path(route, params);
	if (query) {
		const q = new URLSearchParams(query).toString();
		if (q) url += '?' + q;
	}

	try {
		const r = await fetch(url, {
			method: rest.method ?? 'GET',
			...(rest.body === undefined ? {} : { body: rest.body }),
			headers: {
				'Content-Type': 'application/json',
				...rest.headers,
			},
			// СВОЙ сигнал, и он ставится последним: сигнал вызывающего уже
			// подшит к нему выше. Пропусти мы его сюда напрямую — таймаут
			// молча перестал бы существовать.
			signal: ctrl.signal,
		});

		const text = await r.text();
		let parsed: unknown = null;
		try {
			parsed = text ? JSON.parse(text) : null;
		} catch {
			// Не-JSON в ответе — это чужой прокси или отвалившийся демон.
			// Разбирать нечего, но код ответа знать важно.
		}

		if (!r.ok) {
			const b = (parsed ?? {}) as { error?: string; code?: string };
			throw new ApiError(b.error || `HTTP ${r.status}`, r.status, b.code ?? '');
		}
		return parsed as TResponse;
	} finally {
		clearTimeout(timer);
	}
}

/**
 * Отличить наш таймаут и отмену от настоящего отказа.
 *
 * Нужно там, где вытесненный запрос не должен ничего показывать владельцу:
 * ушли со страницы, обогнал более свежий опрос.
 */
export function isAbort(e: unknown): boolean {
	return e instanceof DOMException && e.name === 'AbortError';
}
