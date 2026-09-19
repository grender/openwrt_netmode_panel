// ПОРОЖДЁННЫЙ ФАЙЛ. Правки будут затёрты.
//
// Источник: docs/api/openapi.yaml
// Обновить: node scripts/gen-api.mjs
//
// Файл коммитится: `make verify` обязан работать на машине, где есть
// только Go и POSIX-шелл, а разборщик YAML нужен лишь для ОБНОВЛЕНИЯ.
// Свежесть сверяет scripts/check-routes.sh.

export const ROUTES = {
	b4Set: '/api/b4/set',
	b4Sets: '/api/b4/sets',
	bridge: '/api/bridge',
	bridgeAccess: '/api/bridge/access',
	bridgeDisable: '/api/bridge/disable',
	bridgeEnable: '/api/bridge/enable',
	logs: '/api/logs',
	mode: '/api/mode',
	nikkiAutopool: '/api/nikki/autopool',
	nikkiPanel: '/api/nikki/panel',
	nikkiProxies: '/api/nikki/proxies',
	nikkiProxy: '/api/nikki/proxy',
	nikkiRulesets: '/api/nikki/rulesets',
	nikkiRulesetsCatalog: '/api/nikki/rulesets/catalog',
	nikkiTest: '/api/nikki/test',
	status: '/api/status',
	subscription: '/api/subscription',
	subscriptionUpdate: '/api/subscription/update',
	upstream: '/api/upstream',
	watch: '/api/watch',
	watchHosts: '/api/watch/hosts',
	wifiNetworks: '/api/wifi/networks',
	wifiNetworksById: '/api/wifi/networks/{id}',
	wifiScan: '/api/wifi/scan',
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
