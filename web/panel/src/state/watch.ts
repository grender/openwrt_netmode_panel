import { useEffect, useRef, useState } from 'preact/hooks';
import { api, T } from '../api/client';
import type { WatchState } from '../api/types';
import { POLL_MS } from './consts';

export interface WatchPoll {
	/** Последнее известное состояние. undefined — ещё ни одного ответа. */
	value: WatchState | undefined;
	/** Демон не отвечает: показывается последнее известное. */
	stale: boolean;
	/** Положить состояние из ответа POST/DELETE, не дожидаясь тика. */
	put(s: WatchState): void;
}

/**
 * Опрос наблюдателя раз в секунду, и только пока экран открыт.
 *
 * Свой цикл, а не общий опрос статуса: состояние наблюдения велико (до
 * пятисот адресатов), а нужно оно ровно на одном экране. Класть его в
 * /api/status значило бы возить эти килобайты каждому, кто просто смотрит
 * на главную, — и заодно продлевать TTL сессии тому, кто давно ушёл.
 *
 * Форма повторяет state/poll.ts, и три вещи в ней перенесены дословно,
 * потому что каждая чинила настоящий отказ:
 *
 *  1. Страж inFlight: бюджет ответа больше интервала, и без него запросы
 *     копятся стопкой.
 *  2. Отказ НЕ стирает состояние: экран показывает последнее известное с
 *     пометкой. Пустая таблица при живом наблюдении читается как «устройство
 *     замолчало» — то есть как ответ на вопрос владельца, которого никто не
 *     давал.
 *  3. Ответ, приехавший после следующего, не пишет ничего.
 */
export function useWatchPoll(on: boolean): WatchPoll {
	const [value, setValue] = useState<WatchState | undefined>(undefined);
	const [stale, setStale] = useState(false);
	const seq = useRef(0);
	const inFlight = useRef(false);

	useEffect(() => {
		if (!on) return;
		let alive = true;

		const tick = async () => {
			if (inFlight.current) return;
			inFlight.current = true;
			const mine = ++seq.current;
			try {
				const s = await api<WatchState>('watch', { timeoutMs: T.SIDE });
				if (!alive || mine !== seq.current) return;
				setValue(s);
				setStale(false);
			} catch {
				if (alive && mine === seq.current) setStale(true);
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
	}, [on]);

	// Уход с экрана забывает накопленное: вернувшись, владелец обязан
	// увидеть то, что есть сейчас, а не снимок десятиминутной давности,
	// который выглядит как живой.
	useEffect(() => {
		if (on) return;
		setValue(undefined);
		setStale(false);
	}, [on]);

	const put = (s: WatchState) => {
		// Засев обгоняет опрос: иначе после «Начать» экран секунду показывал
		// бы выбор устройства, будто нажатие не сработало.
		seq.current++;
		setValue(s);
		setStale(false);
	};

	return { value, stale, put };
}
