import { useCallback, useRef, useState } from 'preact/hooks';
import { api, T } from '../api/client';
import type { ApiOptions } from '../api/client';
import type { RouteName } from '../api/routes.gen';

/**
 * Три значения побочного списка, и они РАЗНЫЕ.
 *
 *   undefined — не спрашивали ещё ни разу     → скелет
 *   null      — спросили, отказали            → «не отвечает»
 *   объект    — данные
 *
 * Слить второе с третьим (пустой список вместо null) значит показать
 * «узлов нет» там, где на самом деле «движок молчит»: разное лечение,
 * одинаковый экран.
 */
export type Side<T> = T | null | undefined;

export interface SideList<T> {
	value: Side<T>;
	/** Перечитать. Ответ, обогнанный более свежим запросом, не пишет НИЧЕГО. */
	load(opts?: ApiOptions): Promise<void>;
	/** Положить значение, полученное из ответа на собственную запись. */
	put(v: T): void;
}

/**
 * Побочный список с порядковыми номерами.
 *
 * Номер нужен потому, что запросы обгоняют друг друга: медленный ответ,
 * приехавший после быстрого, затирал бы свежие данные старыми. Обогнанный
 * ответ не пишет ничего — в том числе не пишет null, иначе разовый таймаут
 * гасил бы уже приехавший список.
 */
export function useSide<T>(route: RouteName): SideList<T> {
	const [value, setValue] = useState<Side<T>>(undefined);
	const seq = useRef(0);

	const load = useCallback<SideList<T>['load']>(
		async (opts) => {
			const n = ++seq.current;
			try {
				const d = await api<T>(route, { timeoutMs: T.SIDE, ...opts });
				if (n === seq.current) setValue(d);
			} catch {
				if (n === seq.current) setValue(null);
			}
		},
		[route],
	);

	// Значение из ответа на СВОЮ запись тоже занимает очередь: иначе
	// перечитывание, стартовавшее раньше записи, приехало бы позже и вернуло
	// список в состояние «до».
	const put = useCallback((v: T) => {
		seq.current++;
		setValue(v);
	}, []);

	return { value, load, put };
}
