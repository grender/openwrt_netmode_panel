import { ApiError } from './client';
import type { Key, T } from '../i18n';
import type { Job } from './types';

/**
 * Машинный код отказа → ключ словаря.
 *
 * Разбор идёт по КОДУ, а не по HTTP-статусу: под 503 ходят и подвисший ubus,
 * и нехватка Clash API, и незаданный планировщик, — объяснять их одним
 * текстом про Clash значит врать владельцу ровно там, где он ищет причину.
 *
 * Словарь нужен ещё и потому, что тексты демона русские намеренно (тот же
 * текст уходит в syslog и читается по ssh). В английском интерфейсе они
 * читались бы как утечка бэкенда.
 */
const ERR_KEY: Record<string, Key> = {
	ambiguous_selection: 'sel.ambiguous.title',
	enabled_network_readonly: 'wifi.locked',
	job_busy: 'err.busy',

	// Три кода оптимистичной блокировки wireless (ADR-0011).
	fingerprint_mismatch: 'wifi.err.stale',
	fingerprint_required: 'wifi.err.stale',
	foreign_staged_changes: 'wifi.err.foreign',

	// already_selected означает не «нельзя», а «список, по которому нажали,
	// устарел»: switchable считает сервер, и кнопки на такой строке панель
	// не рисует вовсе — значит запрос мог родиться только из старого списка.
	already_selected: 'wifi.err.already_selected',
	network_incomplete: 'wifi.err.network_incomplete',

	nikki_unavailable: 'srv.down',
	member_not_selectable: 'srv.err.notnode',
	subscription_not_configured: 'sub.err.unset',
	b4_unavailable: 'sets.down',
	ubus_unavailable: 'err.ubus',
	uci_unavailable: 'err.uci',
	scan_failed: 'err.scan',
	ifname_unknown: 'err.ifname',
	radio_unknown: 'err.radio',
	unavailable: 'err.sched',

	// Почему панель Nikki не открыть. Три кода — три текста, и это не
	// многословие: хост не выведен → откройте по адресу роутера; Nikki не
	// настроен → настройте; веб-морды нет → её нет в принципе, есть только
	// Clash API. Общий текст отправлял бы чинить не то, что сломано.
	host_unknown: 'links.why.host',
	nikki_unconfigured: 'links.why.unconfigured',
	panel_missing: 'links.why.nopanel',
};

/**
 * Те же три кода как множество: тост красится жёлтым, а не красным.
 *
 * Все три означают «открывать нечего», а не «сломалось»: роутер исправен,
 * Clash API отвечает — иначе кнопки Nikki в шапке не было бы вовсе. Красный
 * остаётся таймаутам и пятисоткам, иначе цвет перестаёт что-либо значить.
 */
export const WHY_CODES = new Set(['host_unknown', 'nikki_unconfigured', 'panel_missing']);

/**
 * Отказы записи, которые лечатся перечитыванием списка. Тексты разные,
 * лекарство одно: строка, по которой нажали, описывает уже не то, что лежит
 * в конфигурации.
 */
export const STALE_CODES = new Set([
	'already_selected',
	'fingerprint_mismatch',
	'fingerprint_required',
]);

export const BRIDGE_STALE_CODES = new Set([
	'already_enabled',
	'already_disabled',
	'already_set',
	'fingerprint_mismatch',
	'fingerprint_required',
]);

/**
 * Объяснение отказа. Показывается ПРИЧИНА, а не код: 409 в этой панели
 * означает три разные вещи, и «409» не говорит владельцу ничего.
 */
export function describe(e: unknown, t: T, fallback?: Key): string {
	// Прерванный по таймауту запрос даёт DOMException с сообщением браузера
	// («The user aborted a request») — оно и неверно по сути, и не переводится.
	if (e instanceof DOMException && e.name === 'AbortError') return t('err.timeout');

	if (e instanceof ApiError) {
		// Object.hasOwn, а не просто истинность: код приходит с сервера, и
		// попадание вроде 'constructor' достало бы из прототипа функцию.
		if (e.code && Object.hasOwn(ERR_KEY, e.code)) {
			const key = ERR_KEY[e.code];
			if (key) return t(key);
		}
		// Незнакомый код под 503 — нейтральный текст: конкретика тут была бы
		// догадкой.
		if (e.status === 503) return t('err.unavailable');
		if (fallback) return t(fallback);
		return e.message || t('err.generic');
	}

	if (fallback) return t(fallback);
	return e instanceof Error && e.message ? e.message : t('err.generic');
}

export function errCode(e: unknown): string {
	return e instanceof ApiError ? e.code : '';
}

/**
 * Подпись идущей операции строится НА КЛИЕНТЕ по kind и arg. Поле label
 * с демона русское намеренно (syslog и диагностика по ssh).
 */
export function jobText(job: Job, t: T): string {
	if (job.kind === 'mode' && (job.arg === 'nikki' || job.arg === 'b4' || job.arg === 'off')) {
		return t(`job.mode.${job.arg}` as Key);
	}
	if (job.kind === 'subscription') return t('job.subscription');
	if (job.kind === 'bridge') return t('job.bridge');
	// У upstream arg — ssid целевой сети, и подставляется он как ДАННЫЕ, а не
	// как часть ключа: набор ssid открыт, ключа под каждый не заведёшь.
	if (job.kind === 'upstream' && job.arg) return t('job.upstream', { ssid: job.arg });
	return t('job.working');
}

/** Подпись провалившейся операции. */
export function failText(job: Job, t: T): string {
	if (job.kind === 'mode') return t('job.fail.mode', { mode: job.arg });
	if (job.kind === 'subscription') return t('job.fail.subscription');
	if (job.kind === 'bridge') return t('job.fail.bridge');
	if (job.kind === 'upstream') return t('job.fail.upstream');
	return t('job.fail');
}
