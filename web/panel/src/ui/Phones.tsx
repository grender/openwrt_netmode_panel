import { useEffect, useRef, useState } from 'preact/hooks';
import type { SubLinkResponse } from '../api/types';
import type { Lang, T } from '../i18n';
import type { Lock } from '../state/lock';
import type { Side } from '../state/side';
import { Skel, Spin } from './bits';

/**
 * Подписка для телефонов (Happ): выпуск, показ и перевыпуск ссылки.
 *
 * Ссылка — секрет: в ней доступ к своим нодам. Поэтому показывается она
 * только по нажатию и прячется, когда раздел закрывают. QR нет намеренно:
 * у панели одна рантайм-зависимость (check-panel-deps), а панель обычно
 * открыта на том же телефоне — Happ берёт ссылку из буфера.
 */
export interface PhonesProps {
	link: Side<SubLinkResponse>;
	/** Раздел раскрыт: закрытие прячет показанную ссылку. */
	open: boolean;
	lang: Lang;
	lock: Lock;
	locked: boolean;
	t: T;
	/** first — первый выпуск, а не перевыпуск: от этого зависит тост. */
	onRotate(first: boolean): void;
}

export function phonesSummary(v: Side<SubLinkResponse>, t: T): string {
	if (v === undefined) return '…';
	if (v === null) return t('phones.sum.unknown');
	if (!v.url) return t('phones.sum.none');
	return t('phones.sum.on', { n: v.clients.length });
}

/**
 * Копирование: clipboard API есть только в безопасном контексте, а панель
 * ходит по http в LAN — там его часто нет. Запасной путь — выделить поле и
 * execCommand('copy'): устаревший, но единственный, что работает по http.
 */
async function copyText(text: string, input: HTMLInputElement | null): Promise<boolean> {
	try {
		if (navigator.clipboard && window.isSecureContext) {
			await navigator.clipboard.writeText(text);
			return true;
		}
	} catch {
		// упадём на запасной путь
	}
	if (!input) return false;
	input.focus();
	input.select();
	try {
		return document.execCommand('copy');
	} catch {
		return false;
	}
}

export function Phones(p: PhonesProps) {
	const { t } = p;
	const [shown, setShown] = useState(false);
	const [ask, setAsk] = useState(false);
	const input = useRef<HTMLInputElement>(null);

	useEffect(() => {
		if (!p.open) {
			setShown(false);
			setAsk(false);
		}
	}, [p.open]);

	const v = p.link;
	if (v === undefined) return <Skel n={2} />;
	if (v === null) return <div class="empty">{t('phones.unknown')}</div>;

	const busy = p.lock.on('subrotate');

	return (
		<>
			<p class="hint">{t('phones.text')}</p>

			{!v.url ? (
				<>
					<p class="hint">{t('phones.none')}</p>
					<button type="button" class="wide" disabled={p.locked} aria-busy={busy} onClick={() => p.onRotate(true)}>
						{busy ? <Spin /> : null} {t('phones.issue')}
					</button>
				</>
			) : (
				<>
					{shown ? (
						<div class="confirm" data-confirm-for="sublink">
							<label class="field">
								<span>{t('shelf.phones')}</span>
								<input
									ref={input}
									type="text"
									readOnly
									spellcheck={false}
									value={v.url}
									onFocus={(e) => (e.currentTarget as HTMLInputElement).select()}
								/>
							</label>
							<div class="buttons">
								<button
									type="button"
									class="go"
									onClick={async () => {
										const ok = await copyText(v.url ?? '', input.current);
										p.lock.flash(t(ok ? 'phones.copied' : 'phones.copy.fail'), ok ? 'ok' : 'warn');
									}}
								>
									{t('phones.copy')}
								</button>
								<button type="button" class="no" onClick={() => setShown(false)}>
									{t('phones.hide')}
								</button>
							</div>
							<p>{t('phones.howto')}</p>
						</div>
					) : (
						<button type="button" class="wide" onClick={() => setShown(true)}>
							{t('phones.show')}
						</button>
					)}

					<button type="button" class="wide" disabled={p.locked} onClick={() => setAsk(true)}>
						{t('phones.rotate')}
					</button>
					{/* Подтверждение — в панели, не confirm(): модальное окно
					    браузера останавливает всё остальное на странице. */}
					{ask ? (
						<div class="confirm" data-confirm-for="subrotate">
							<b>{t('phones.rotate.ask')}</b>
							<p>{t('phones.rotate.text')}</p>
							<div class="buttons">
								<button
									type="button"
									class="go"
									disabled={p.locked}
									aria-busy={busy}
									onClick={() => {
										setAsk(false);
										setShown(false);
										p.onRotate(false);
									}}
								>
									{busy ? <Spin /> : null} {t('phones.rotate')}
								</button>
								<button type="button" class="no" onClick={() => setAsk(false)}>
									{t('wifi.cancel')}
								</button>
							</div>
						</div>
					) : null}

					<div class="label">{t('phones.clients')}</div>
					{v.clients.length === 0 ? (
						<p class="hint">{t('phones.clients.none')}</p>
					) : (
						<div class="rows">
							{v.clients.map((c) => (
								<div key={c.user_agent} class="row">
									<span class="name">{c.user_agent}</span>
									<span class="tag tag-dim">
										{new Date(c.at).toLocaleString(p.lang === 'ru' ? 'ru-RU' : 'en-GB')}
									</span>
								</div>
							))}
						</div>
					)}
				</>
			)}
		</>
	);
}
