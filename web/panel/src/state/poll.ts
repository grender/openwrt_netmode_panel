import { useEffect, useRef, useState } from 'preact/hooks';
import { api, T } from '../api/client';
import type { Job, Status } from '../api/types';
import { JOB_MAX, POLL_MS, STATUS_TTL } from './consts';

export interface Poll {
	/** Последний известный статус. undefined — ещё ни одного ответа. */
	status: Status | undefined;
	/** Демон не отвечает. Показывается ПОСЛЕДНЕЕ известное состояние с пометкой. */
	stale: boolean;
	/** Часы роутера минус часы браузера, мс. */
	skewMs: number;
	/** Джоб из ответа 202, пока опрос про него не узнал. */
	seed: Job | null;
	/** Засеять джоб из 202. */
	sow(r: { job?: Job | null } | null | undefined): void;
}

/**
 * Опрос статуса раз в секунду.
 *
 * Перенесено дословно вместе с тремя вещами, каждая из которых чинила
 * настоящий отказ:
 *
 *  1. Оба сторожа (потолок засева, доверие статусу) срабатывают ДО запроса —
 *     иначе они не работают ровно тогда, когда нужны: когда опрос не проходит.
 *  2. Страж inFlight: бюджет статуса 4 с при интервале 1 с, и без него
 *     запросы копятся стопкой.
 *  3. Отказ НЕ стирает состояние: панель показывает последнее известное с
 *     пометкой «устарело». Пустой экран при живом роутере — худшее, что она
 *     может сделать: он выглядит как «всё сломалось».
 */
export function usePoll(): Poll {
	const [status, setStatus] = useState<Status | undefined>(undefined);
	const [stale, setStale] = useState(false);
	const [seed, setSeed] = useState<Job | null>(null);

	// Ref рядом с состоянием, потому что читать засев надо и внутри тика,
	// где замыкание держит старое значение. Присваиваются всегда парой:
	// разойдись они — засев стал бы бессмертным, то есть запер бы панель до F5.
	const seedRef = useRef<Job | null>(null);
	const seedAt = useRef(0);

	const okAt = useRef(0);
	const skew = useRef(0);
	const inFlight = useRef(false);

	useEffect(() => {
		let alive = true;

		const tick = async () => {
			const now = Date.now();
			if (seedRef.current && now - seedAt.current > JOB_MAX) {
				seedRef.current = null;
				setSeed(null);
			}
			// «Верим ли мы своему статусу» считается от локального момента
			// ПОЛУЧЕНИЯ, а не от generated_at: у роутера без RTC часы уезжают
			// на часы, и разность двух router-меток тут ничего не измеряет.
			setStale((prev) => {
				const trusted = !okAt.current || now - okAt.current <= STATUS_TTL;
				return trusted ? prev : true;
			});

			if (inFlight.current) return;
			inFlight.current = true;
			try {
				const s = await api<Status>('status', { timeoutMs: T.STATUS });
				if (!alive) return;
				const at = Date.now();
				okAt.current = at;
				skew.current = new Date(s.generated_at).getTime() - at;

				// Засев уступает статусу по ДВУМ признакам: тот же джоб по id
				// либо статус собран позже, чем начался засеянный джоб.
				// Второй нужен для быстрых операций, у которых джоба нет вовсе.
				const cur = seedRef.current;
				if (
					cur &&
					((s.job && s.job.id === cur.id) ||
						new Date(s.generated_at).getTime() >= new Date(cur.started_at).getTime())
				) {
					seedRef.current = null;
					setSeed(null);
				}
				setStatus(s);
				setStale(false);
			} catch {
				if (alive) setStale(true);
			} finally {
				inFlight.current = false;
			}
		};

		void tick();
		const id = setInterval(() => void tick(), POLL_MS);
		return () => {
			alive = false;
			clearInterval(id);
		};
	}, []);

	const sow: Poll['sow'] = (r) => {
		if (!r || !r.job) return;
		// Значение и локальная метка ставятся ОДНОЙ операцией: забытое
		// присваивание метки сделало бы засев бессмертным.
		seedRef.current = r.job;
		seedAt.current = Date.now();
		setSeed(r.job);
	};

	return { status, stale, skewMs: skew.current, seed, sow };
}
