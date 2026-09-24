import { useCallback, useMemo, useRef, useState } from 'preact/hooks';
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
	/**
	 * Перечитать. Ответ, обогнанный более свежим запросом, не пишет НИЧЕГО.
	 * true — этот запрос ответил данными, false — отказал.
	 */
	load(opts?: ApiOptions): Promise<boolean>;
	/**
	 * Первая загрузка: только если списка ещё нет и за ним уже не пошли.
	 *
	 * Эффекты первой загрузки перезапускаются на каждом тике опроса (статус —
	 * новый объект раз в секунду), а значение до ответа остаётся undefined.
	 * Через load каждый тик отправлял новый запрос, номер рос, и ответ
	 * медленнее секунды выбрасывался как обогнанный — список не приезжал
	 * НИКОГДА, а роутер получал стопку запросов.
	 */
	ensure(opts?: ApiOptions): void;
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
	// Сколько запросов в пути. Ref, а не состояние: смена не должна
	// перерисовывать — иначе эффекты, зависящие от списка, запускались бы
	// снова на каждый старт и конец запроса.
	const inFlight = useRef(0);
	const valueRef = useRef<Side<T>>(undefined);

	const load = useCallback<SideList<T>['load']>(
		async (opts) => {
			const n = ++seq.current;
			inFlight.current++;
			try {
				const d = await api<T>(route, { timeoutMs: T.SIDE, ...opts });
				if (n === seq.current) {
					valueRef.current = d;
					setValue(d);
				}
				return true;
			} catch {
				if (n === seq.current) {
					valueRef.current = null;
					setValue(null);
				}
				return false;
			} finally {
				inFlight.current--;
			}
		},
		[route],
	);

	const ensure = useCallback<SideList<T>['ensure']>(
		(opts) => {
			if (valueRef.current !== undefined || inFlight.current > 0) return;
			void load(opts);
		},
		[load],
	);

	// Значение из ответа на СВОЮ запись тоже занимает очередь: иначе
	// перечитывание, стартовавшее раньше записи, приехало бы позже и вернуло
	// список в состояние «до».
	const put = useCallback((v: T) => {
		seq.current++;
		valueRef.current = v;
		setValue(v);
	}, []);

	// Один и тот же объект, пока не сменилось значение: зависящие от списка
	// эффекты и колбэки не перезапускаются на каждой отрисовке.
	return useMemo(() => ({ value, load, ensure, put }), [value, load, ensure, put]);
}
