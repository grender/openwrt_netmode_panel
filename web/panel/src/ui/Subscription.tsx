import { useState } from 'preact/hooks';
import type { Lock } from '../state/lock';
import type { Side } from '../state/side';
import type { LogsResponse, ProxiesResponse, Status, SubscriptionURL } from '../api/types';
import { fmtTime, type Lang, type T } from '../i18n';
import { Skel, Spin } from './bits';

export interface SubProps {
	status: Status;
	sub: Side<SubscriptionURL>;
	logs: Side<LogsResponse>;
	nikki: Side<ProxiesResponse>;
	lang: Lang;
	lock: Lock;
	locked: boolean;
	t: T;
	onUpdate(): void;
	onSaveURL(url: string): void;
}

export function subSummary(status: Status, lang: Lang, t: T): string {
	const s = status.subscription;
	if (!s) return t('sub.log.down');
	if (!s.configured) return t('sub.unset.short');
	return `${t('sub.nodes.short', { n: s.nodes })} · ${t('sub.when', {
		when: s.last_update ? fmtTime(s.last_update, lang) : t('sub.never'),
	})}`;
}

/**
 * Подписка — отдельный раздел, а не часть карточки Nikki.
 *
 * Скачивание не зависит от режима: подписку можно обновить и при
 * остановленном движке, узлы появятся, когда он включится. Пока карточка
 * жила внутри Nikki, эта кнопка исчезала вместе с ним — то есть панель
 * прятала рабочее действие ровно тогда, когда оно нужно.
 */
export function Subscription({
	status,
	sub,
	logs,
	nikki,
	lang,
	lock,
	locked,
	t,
	onUpdate,
	onSaveURL,
}: SubProps) {
	const [editing, setEditing] = useState(false);
	const [draft, setDraft] = useState('');
	// Журнал свёрнут: обновление в норме заканчивается тостом, а журнал
	// нужен, когда «обновлена вчера» противоречит «нажимал сегодня».
	const [showLog, setShowLog] = useState(false);
	const s = status.subscription;
	const unset = s ? !s.configured : false;

	// Строки, которые не стали узлами. Показывается только когда движок
	// ответил и виды строк известны: при выключенном Nikki их взять неоткуда,
	// и «0 строк не стали узлами» было бы утверждением, которого мы не знаем.
	const unsupported = nikki?.members.filter((m) => m.kind === 'unsupported') ?? [];

	return (
		<>
			<p class="prose">{t('sub.independent')}</p>

			<div class="label">{t('sub.url.label')}</div>
			{editing ? (
				<>
					<label class="field">
						<span class="sr-only">{t('sub.url.label')}</span>
						<input
							type="url"
							value={draft}
							placeholder={t('sub.url.placeholder')}
							autocomplete="off"
							spellcheck={false}
							onInput={(e) => setDraft((e.target as HTMLInputElement).value)}
						/>
					</label>
					<p class="hint">{t('sub.url.hint')}</p>
					<div class="form-row">
						<button
							type="button"
							class="wide primary"
							disabled={locked}
							aria-busy={lock.on('suburl')}
							onClick={() => {
								onSaveURL(draft);
								setEditing(false);
							}}
						>
							{lock.on('suburl') ? <Spin /> : null} {t('sub.url.save')}
						</button>
						<button type="button" class="wide" onClick={() => setEditing(false)}>
							{t('wifi.cancel')}
						</button>
					</div>
				</>
			) : (
				<div class="row">
					{/* Наружу уходит только МАСКА: схема, хост, путь и имена
					    параметров. Ни одного символа секрета (ADR-0012,
					    ADR-0034) — «хвостик из четырёх знаков» это утечка
					    четырёх знаков, а не мера защиты. */}
					<span class="name" title={sub?.masked ?? ''}>
						{sub === undefined ? '…' : sub?.masked || t('sub.url.none')}
					</span>
					<span class="acts" style={{ width: 'auto' }}>
						<button
							type="button"
							class="mini"
							disabled={!!lock.busy}
							onClick={() => {
								// Поле начинается ПУСТЫМ, а не с маски: маска —
								// не адрес, и отправить её обратно значило бы
								// записать многоточия вместо токена.
								setDraft('');
								setEditing(true);
							}}
						>
							{t('sub.url.edit')}
						</button>
					</span>
				</div>
			)}

			{unset ? <p class="hint" style={{ color: 'var(--warn)' }}>{t('sub.unset')}</p> : null}

			<button
				type="button"
				class="wide"
				disabled={locked || unset}
				aria-busy={lock.on('sub')}
				onClick={onUpdate}
			>
				{lock.on('sub') ? (
					<>
						<Spin /> {t('sub.updating')}
					</>
				) : (
					t('sub.update')
				)}
			</button>

			{s && s.status === 'fail' && s.error ? (
				<p class="hint" style={{ color: 'var(--bad)' }}>
					{s.error}
				</p>
			) : null}

			{unsupported.length > 0 ? (
				<div class="confirm">
					<b>
						{t('sub.unsupported.title', {
							n: unsupported.length,
							names: unsupported.map((m) => m.name).join(', '),
						})}
					</b>
					<p>{unsupported[0]?.reason ?? t('sub.unsupported.text')}</p>
				</div>
			) : null}

			<button type="button" class="linkbtn" aria-expanded={showLog} onClick={() => setShowLog(!showLog)}>
				{t(showLog ? 'sub.log.hide' : 'sub.log.show')}
			</button>
			{!showLog ? null : logs === undefined ? (
				<Skel n={2} cls="line" wrap="log" />
			) : logs === null ? (
				<div class="log-line">{t('sub.log.down')}</div>
			) : !logs.lines || logs.lines.length === 0 ? (
				<div class="log-line">{t('sub.emptylog')}</div>
			) : (
				<div class="log">
					{logs.lines.map((l, i) => (
						<div key={i} class="log-line">
							<span style={{ color: l.status === 'ok' ? 'var(--ok)' : 'var(--bad)' }}>
								{l.status === 'ok' ? '✓' : '✕'}
							</span>
							<span>{fmtTime(l.ts, lang)}</span>
							<span>{l.status === 'ok' ? t('sub.nodes.short', { n: l.nodes }) : l.err || '—'}</span>
						</div>
					))}
				</div>
			)}
		</>
	);
}
