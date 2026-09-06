// ПОРОЖДЁННЫЙ ФАЙЛ. Правки будут затёрты.
//
// Источник: docs/api/openapi.yaml
// Обновить: node scripts/gen-api.mjs
//
// Файл коммитится: `make verify` обязан работать на машине, где есть
// только Go и POSIX-шелл, а разборщик YAML нужен лишь для ОБНОВЛЕНИЯ.
// Свежесть сверяет scripts/check-routes.sh.

/** Причины, по которым не состоялась смена внешней сети (ADR-0025). */
export const UPSTREAM_REASONS = [
	'apply_failed',
	'busy',
	'prereq_missing',
	'executor_missing',
	'stayed_on_previous',
	'other_ssid',
	'not_associated',
	'no_ipv4',
	'unverifiable',
	'stale_draft',
] as const;

/** Причины, по которым не состоялась операция проброса (ADR-0030). */
export const BRIDGE_REASONS = [
	'apply_failed',
	'busy',
	'prereq_missing',
	'executor_missing',
	'install_failed',
	'no_iface',
	'relay_down',
	'unverifiable',
	'stale_draft',
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
