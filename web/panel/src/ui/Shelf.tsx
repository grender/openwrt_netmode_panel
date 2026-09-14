import type { T } from '../i18n';

export interface ShelfRow {
	/** Хеш экрана: #routes, #sub, #bridge — настройки, #watch — наблюдатель. */
	id: 'routes' | 'sub' | 'bridge' | 'watch';
	title: string;
	summary: string;
	/** Жёлтая сводка — про аномалию: шлюз молчит, ПК не отвечает. */
	warn?: boolean;
}

/**
 * Полка сводок: три строки вместо трёх карточек.
 *
 * То, что настраивают раз и надолго, с главной уезжает на экран настроек, а
 * здесь остаётся ответ на вопрос «надо ли туда идти». Сводка обязана называть
 * СОСТОЯНИЕ, а не раздел: имя без цифр заставляет заходить внутрь, чтобы
 * узнать, надо ли было заходить.
 */
export function Shelf({ rows, t }: { rows: ShelfRow[]; t: T }) {
	return (
		<nav class="shelf full" data-part="shelf" aria-label={t('settings.title')}>
			{rows.map((r) => (
				<a key={r.id} href={`#${r.id}`} class="shelf-row" data-anchor={`shelf-${r.id}`}>
					<span class="shelf-title">{r.title}</span>
					<span class={`shelf-sum${r.warn ? ' warn' : ''}`}>{r.summary}</span>
					<span class="chev" aria-hidden="true">
						›
					</span>
				</a>
			))}
		</nav>
	);
}
