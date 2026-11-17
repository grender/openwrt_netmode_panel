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
	exitsReload: '/api/exits/reload',
	logs: '/api/logs',
	mode: '/api/mode',
	nikkiAutopool: '/api/nikki/autopool',
	nikkiExits: '/api/nikki/exits',
	nikkiExitsSelect: '/api/nikki/exits/select',
	nikkiHops: '/api/nikki/hops',
	nikkiPanel: '/api/nikki/panel',
	nikkiPathtest: '/api/nikki/pathtest',
	nikkiProxies: '/api/nikki/proxies',
	nikkiProxy: '/api/nikki/proxy',
	nikkiRulesets: '/api/nikki/rulesets',
	nikkiRulesetsCatalog: '/api/nikki/rulesets/catalog',
	nikkiTest: '/api/nikki/test',
	status: '/api/status',
	subLink: '/api/sub/link',
	subRotate: '/api/sub/rotate',
	subscription: '/api/subscription',
	subscriptionUpdate: '/api/subscription/update',
	subscriptions: '/api/subscriptions',
	subscriptionsById: '/api/subscriptions/{id}',
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
