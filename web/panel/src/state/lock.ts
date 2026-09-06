import { useCallback, useRef, useState } from 'preact/hooks';
import { TOAST_MS } from './consts';

export type ToastKind = 'ok' | 'err' | 'warn' | 'info';

export interface Toast {
	msg: string;
	kind: ToastKind;
	/** Запасная ссылка: появляется, когда обычный путь не сработал. */
	href?: string;
	cta?: string;
}

export interface Lock {
	/** Ключ занятости: `kind` или `kind:arg`. Пусто — панель свободна. */
	busy: string;
	/** Занят ли ИМЕННО этот элемент. */
	on(kind: string, arg?: string): boolean;
	/**
	 * Выполнить операцию под единственным глобальным замком.
	 *
	 * `after` перечитывает списки ПОСЛЕ снятия замка: иначе кнопки
	 * разблокируются не когда операция кончилась, а когда приехали побочные
	 * списки, — и владелец ждёт дольше, чем происходит.
	 */
	act(key: string, fn: () => Promise<unknown>, after?: () => Promise<unknown>): Promise<void>;
	/** Переставить ключ занятости внутри уже идущей операции. */
	rekey(key: string): void;
	toast: Toast | null;
	flash(msg: string, kind?: ToastKind, extra?: Omit<Toast, 'msg' | 'kind'>): void;
	dropToast(): void;
}

/**
 * Единственный глобальный замок панели.
 *
 * Он существует, потому что демон отбивает вторую операцию кодом 409: очереди
 * нет, и вторая кнопка гарантированно вернула бы job_busy. Раньше это
 * ограничение жило как `disabled` на каждой кнопке — и расползлось на вещи
 * вроде «Отмена» в форме, где нажатие ничему не мешало. Теперь `disabled`
 * снова значит ровно «нажимать бесполезно», а запрет — это замок.
 *
 * Занятая операция НИКОГДА не отбивается молча: попытка при занятом замке
 * говорит владельцу, почему ничего не произошло.
 */
export function useLock(busyMessage: string, describe: (e: unknown) => string): Lock {
	const [busy, setBusy] = useState('');
	const [toast, setToast] = useState<Toast | null>(null);
	// Мьютекс отдельно от состояния: setState асинхронен, и два быстрых
	// нажатия успели бы пройти оба.
	const busyRef = useRef('');
	const timer = useRef<ReturnType<typeof setTimeout> | undefined>(undefined);

	const dropToast = useCallback(() => {
		clearTimeout(timer.current);
		setToast(null);
	}, []);

	const flash = useCallback<Lock['flash']>((msg, kind = 'err', extra) => {
		setToast({ msg, kind, ...extra });
		clearTimeout(timer.current);
		timer.current = setTimeout(() => setToast(null), TOAST_MS);
	}, []);

	// Ключ и мьютекс ставятся ОДНОЙ строкой всегда: разойдись они, панель
	// считала бы себя занятой под одним ключом, а кольцо рисовала под другим.
	const rekey = useCallback((key: string) => {
		busyRef.current = key;
		setBusy(key);
	}, []);

	const act = useCallback<Lock['act']>(
		async (key, fn, after) => {
			if (busyRef.current) {
				flash(busyMessage);
				return;
			}
			rekey(key);
			try {
				await fn();
			} catch (e) {
				flash(describe(e));
				return;
			} finally {
				busyRef.current = '';
				setBusy('');
			}
			if (after) await after();
		},
		[busyMessage, describe, flash, rekey],
	);

	const on = useCallback(
		(kind: string, arg?: string) => busy === (arg == null ? kind : `${kind}:${arg}`),
		[busy],
	);

	return { busy, on, act, rekey, toast, flash, dropToast };
}
