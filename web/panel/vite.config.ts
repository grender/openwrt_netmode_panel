import { defineConfig, type Plugin } from 'vite';
import preact from '@preact/preset-vite';
import { createHash } from 'node:crypto';

// Куда ходит API в разработке. Мок по умолчанию; NETMODE_API=http://127.0.0.1:8099
// переключает на cmd/netmoded-dev — настоящие обработчики Go против фикстур.
const API = process.env.NETMODE_API ?? 'http://127.0.0.1:8088';

// Токен для разработки.
//
// В боевом режиме панель НЕ шлёт заголовок авторизации вовсе: демон ставит
// cookie netmode_token на первом же заходе по ?token=… и дальше всё едет
// на ней (ADR-0014). В разработке заголовок подставляется явно, и это не
// упрощение, а осознанный отказ от подражания: cookie ключуется хостом, а
// не портом, поэтому «оно и так работает» между :5173 и :8099 — по
// совпадению, невидимо и до первой замены localhost на 127.0.0.1.
// Половина работающего механизма хуже неработающего: она не даёт заметить,
// что механизм не проверен.
//
// Поэтому cookie-рукопожатие в дев-петле не задействовано ВООБЩЕ, а
// проверяется отдельно: `make preview` и `go run ./cmd/netmoded-dev`.
const DEV_TOKEN = process.env.NETMODE_DEV_TOKEN ?? 'dev-token-not-for-production-use';

// Версия в query вместо контент-хеша в имени.
//
// Контент-хеш добавлял бы в git по два вечных блоба со случайными именами
// на каждую правку панели: артефакт коммитится (go:embed нужны файлы на
// момент компиляции), диффы стали бы нечитаемыми, а `go:embed all:` увозил
// бы на роутер осиротевшие ассеты. Между роутером и браузером нет ни CDN,
// ни прокси — единственный сценарий, в котором query-busting не работает,
// здесь отсутствует.
//
// Цена: кэш-заголовки обязаны быть выставлены на стороне демона, иначе
// query не покупает ничего. Это отдельный этап и отдельное решение.
function versionQuery(): Plugin {
	return {
		name: 'netmode-version-query',
		enforce: 'post',
		apply: 'build',
		transformIndexHtml: {
			order: 'post',
			handler(html, ctx) {
				// Свёртка считается ЗДЕСЬ, из ctx.bundle, а не в отдельном
				// generateBundle. Первая попытка так и делала — и штамп
				// приезжал пустым: порядок между двумя generateBundle
				// разных плагинов Vite не обещает, а преобразование HTML
				// успевало отработать раньше. Ошибка была тихой: адрес
				// оставался валидным, просто версия в нём отсутствовала.
				const bundle = ctx.bundle;
				if (!bundle) return html;

				// По СОДЕРЖИМОМУ, а не по времени сборки: пересборка без
				// правок обязана давать тот же адрес, иначе браузер
				// перекачивал бы панель на каждый деплой. HTML в свёртку не
				// входит — он и есть то, что мы сейчас правим.
				const h = createHash('sha256');
				for (const name of Object.keys(bundle).sort()) {
					if (name.endsWith('.html')) continue;
					const c = bundle[name];
					if (!c) continue;
					h.update(name);
					h.update(c.type === 'chunk' ? c.code : Buffer.from(c.source));
				}
				const stamp = h.digest('hex').slice(0, 8);

				// Штамп ставится только на СВОИ ассеты (./assets/…): чужой
				// адрес трогать нельзя, а внешних тут и не бывает.
				return html.replace(/(["'])(\.?\/assets\/[^"']+?)\1/g, (_m, q, url) =>
					`${q}${url}?v=${stamp}${q}`);
			},
		},
	};
}

export default defineConfig({
	plugins: [preact(), versionQuery()],

	// Относительный base: панель отдаётся с корня роутера, но пути в HTML
	// обязаны работать и когда её открывают из файла — например, когда
	// смотрят собранный артефакт глазами.
	base: './',

	build: {
		outDir: 'dist',
		emptyOutDir: true,
		// Имена стабильные — см. versionQuery выше.
		rollupOptions: {
			output: {
				entryFileNames: 'assets/app.js',
				chunkFileNames: 'assets/[name].js',
				assetFileNames: 'assets/[name][extname]',
			},
		},
		// Панель — один экран; резать её на чанки незачем, а лишний
		// запрос на роутере стоит дороже, чем сэкономленные байты.
		cssCodeSplit: false,
		// Карты исходников не едут на роутер: они больше самой панели, а
		// отлаживают её на ноуте против мока, где есть исходники.
		sourcemap: false,
		target: 'es2022',
	},

	server: {
		port: 5173,
		strictPort: true,
		// Записи развёрнуты, а не собраны Object.fromEntries: контекстная
		// типизация Vite доходит только до литерала, и без неё тип proxy
		// в configure пришлось бы объявлять руками — то есть держать копию
		// чужого типа, которая разойдётся с ним при первом обновлении.
		// Заголовок ставится статическим полем headers, а не колбэком
		// configure: колбэк даёт объект чужого типа, который пришлось бы
		// описывать руками, и эта копия разошлась бы с оригиналом при первом
		// обновлении Vite. Значение здесь постоянное — колбэк ничего не даёт.
		proxy: Object.fromEntries(
			// Служебные маршруты мока (__scenario, __clock) стоят рядом с
			// контрактными: в openapi их нет и на роутере не существует, но в
			// дев-петле они ходят к тому же адресу и с тем же токеном.
			['/api', '/__scenario', '/__clock'].map((route) => [route, {
				target: API,
				changeOrigin: false,
				headers: { Authorization: `Bearer ${DEV_TOKEN}` },
			}]),
		),
	},
});
