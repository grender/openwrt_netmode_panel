import { useRef } from 'preact/hooks';
import type { Job, Status } from '../api/types';
import { START_GRACE, STATUS_TTL } from './consts';

export interface Resolved {
	/** Идущая операция, если есть. */
	running: Job | null;
	/** Провалившаяся операция, пока демон её ещё показывает. */
	failed: Job | null;
	/** Панель занята: своей операцией или чужой. */
	locked: boolean;
}

/**
 * Какой джоб показывать: засеянный из 202 или пришедший статусом.
 *
 * НЕ `statusJob || seed`. Демон держит завершённый джоб в статусе ещё пять
 * секунд (keepFinished), и простое «или» предпочло бы протухший завершённый
 * только что засеянному. Засев уступает статусу лишь тогда, когда статус
 * описывает ТОТ ЖЕ джоб по id — сравнение идёт с сырым status.job, а не с
 * отфильтрованным: устаревший статус всё равно доказывает, что опрос про
 * наш джоб узнал.
 */
export function resolveJob(
	status: Status | undefined,
	seed: Job | null,
	busy: string,
	skewMs: number,
): Resolved {
	const raw = status?.job ?? null;

	// Статусу верим, только если он свежий. Иначе панель показывала бы
	// «идёт операция» по снимку пятиминутной давности.
	const fresh =
		!!status &&
		Date.now() + skewMs - new Date(status.generated_at).getTime() <= STATUS_TTL;
	const sJob = fresh ? raw : null;

	const job = seed && !(raw && raw.id === seed.id) ? seed : sJob;
	const running = job && job.state === 'running' ? job : null;
	const failed = job && job.state === 'failed' ? job : null;
	return { running, failed, locked: !!running || !!busy };
}

export type SvcState = 'up' | 'starting' | 'down';

/**
 * Состояние движка: работает, запускается, не отвечает.
 *
 * «Запускается» существует потому, что демон коммитит режим в UCI ДО того,
 * как служба поднялась: сразу после переключения молчание движка — норма.
 * Без этого различия панель писала бы «Clash API не отвечает» в тот самый
 * момент, когда владелец только что нажал «Nikki», и он шёл бы чинить
 * исправное.
 *
 * Окно живёт в ref, а не в состоянии, и пишется во время отрисовки: это
 * производная от джоба, а не самостоятельное состояние, и лишний повторный
 * рендер на каждый тик опроса ей ни к чему.
 */
export function useSvc() {
	const grace = useRef<Record<string, number | 'running'>>({ nikki: 0, b4: 0 });

	return (name: 'nikki' | 'b4', up: boolean, job: Job | null, stale: boolean): SvcState => {
		const g = grace.current;
		const mine = job && job.kind === 'mode' && job.arg === name ? job : null;

		if (mine && mine.state === 'running') {
			g[name] = 'running';
		} else if (mine && mine.state === 'failed') {
			// Провал ДОКАЗЫВАЕТ, что запуска не происходит. Оставить окно
			// значило бы ещё двадцать секунд писать «запускается» про то,
			// что уже сказало «не смогло».
			g[name] = 0;
		} else if (g[name] === 'running') {
			g[name] = Date.now() + START_GRACE;
		}

		if (up) {
			g[name] = 0;
			return 'up';
		}
		// Связь с демоном потеряна — про движок мы не знаем ничего, и
		// «запускается» было бы выдумкой. Это же единственный выход из
		// зависшего ожидания, если опрос умер посреди окна.
		if (stale) return 'down';
		if (g[name] === 'running') return 'starting';
		return (g[name] as number) > Date.now() ? 'starting' : 'down';
	};
}
