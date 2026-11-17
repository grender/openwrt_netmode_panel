import { useState } from 'preact/hooks';
import { jobOn } from '../state/job';
import type { Lock } from '../state/lock';
import type { Side } from '../state/side';
import type { Job, LogsResponse, ProxiesResponse, Status, SubscriptionSource, SubscriptionsResponse } from '../api/types';
import { fmtTime, type Lang, type T } from '../i18n';
import { Confirm, Skel, Spin } from './bits';

/** id основной подписки — её нельзя удалить и ей не нужен ID устройства. */
const MAIN = 'main';

/** Что уходит демону при добавлении и правке подписки. */
export interface SubDraft {
	label: string;
	url: string;
	hwid: boolean;
}

export interface SubProps {
	status: Status;
	/** Все подписки (ADR-0052): undefined — грузится, null — список не пришёл. */
	subs: Side<SubscriptionsResponse>;
	logs: Side<LogsResponse>;
	nikki: Side<ProxiesResponse>;
	lang: Lang;
	lock: Lock;
	locked: boolean;
	running: Job | null;
	t: T;
	onUpdate(): void;
	/** id === null — новая подписка. */
	onSave(id: string | null, d: SubDraft, setErr: (s: string) => void, done: () => void): void;
	onDelete(id: string): void;
}

/**
 * Сводка для строки настроек: сколько узлов в общем пуле и когда обновлялись.
 * «Частично» — в сводке: это то, ради чего сюда стоит зайти.
 */
export function subSummary(status: Status, lang: Lang, t: T): string {
	const s = status.subscription;
	if (!s) return t('sub.log.down');
	if (!s.configured) return t('sub.unset.short');
	const base = `${t('sub.nodes.short', { n: s.nodes })} · ${t('sub.when', {
		when: s.last_update ? fmtTime(s.last_update, lang) : t('sub.never'),
	})}`;
	return s.partial ? `${base} · ${t('sub.partial.short')}` : base;
}

export function Subscription({ status, subs, logs, nikki, lang, lock, locked, running, t, onUpdate, onSave, onDelete }: SubProps) {
	// Какая строка раскрыта формой: id подписки, 'new' или ничего.
	const [editing, setEditing] = useState<string | null>(null);
	const [draft, setDraft] = useState<SubDraft>({ label: '', url: '', hwid: false });
	const [err, setErr] = useState('');
	const [asking, setAsking] = useState<string | null>(null);
	const [showLog, setShowLog] = useState(false);
	const s = status.subscription;
	const unset = s ? !s.configured : false;
	const updating = lock.on('sub') || jobOn(running, 'subscription');

	const unsupported = nikki?.members.filter((m) => m.kind === 'unsupported') ?? [];
	const sources = subs?.sources ?? [];
	const max = subs?.max ?? 4;
	const full = sources.length >= max;
	const nameOf = (x: SubscriptionSource) => (x.id === MAIN ? t('sub.main') : x.label);

	const open = (id: string, d: SubDraft) => {
		setDraft(d);
		setErr('');
		setAsking(null);
		setEditing(id);
	};

	const form = (id: string | null) => {
		const isMain = id === MAIN;
		return (
			<div class="s-form" data-part="sub-form">
				{isMain ? null : (
					<label class="field">
						<span>{t('sub.f.label')}</span>
						<input
							type="text"
							value={draft.label}
							maxLength={24}
							autocomplete="off"
							spellcheck={false}
							placeholder={t('sub.f.label.ph')}
							onInput={(e) => {
								setDraft({ ...draft, label: (e.target as HTMLInputElement).value });
								setErr('');
							}}
						/>
					</label>
				)}
				<label class="field">
					<span>{t('sub.f.url')}</span>
					<input
						type="url"
						value={draft.url}
						placeholder={t('sub.url.placeholder')}
						autocomplete="off"
						spellcheck={false}
						onInput={(e) => {
							setDraft({ ...draft, url: (e.target as HTMLInputElement).value });
							setErr('');
						}}
					/>
				</label>
				<p class="hint">{t(id === null || isMain ? 'sub.url.hint' : 'sub.f.url.keep')}</p>
				{isMain ? null : (
					<>
						<label class="s-check">
							<input
								type="checkbox"
								checked={draft.hwid}
								onChange={(e) => setDraft({ ...draft, hwid: (e.target as HTMLInputElement).checked })}
							/>
							<span>{t('sub.f.hwid')}</span>
						</label>
						<p class="hint">{t('sub.f.hwid.hint')}</p>
					</>
				)}
				{err ? (
					<p class="hint" style={{ color: 'var(--bad)' }}>
						{err}
					</p>
				) : null}
				<div class="form-row">
					<button
						type="button"
						class="wide primary"
						disabled={locked}
						aria-busy={lock.on('suburl')}
						onClick={() => onSave(id, draft, setErr, () => setEditing(null))}
					>
						{lock.on('suburl') ? <Spin /> : null} {t('sub.url.save')}
					</button>
					<button type="button" class="wide" onClick={() => setEditing(null)}>
						{t('wifi.cancel')}
					</button>
				</div>
			</div>
		);
	};

	return (
		<>
			<p class="prose">{t('sub.independent')}</p>

			<div class="label s-head">
				<span>{t('sub.list.label')}</span>
				{subs ? <span class="s-count">{t('sub.list.count', { n: sources.length, max })}</span> : null}
			</div>

			{subs === undefined ? (
				<Skel n={2} />
			) : subs === null ? (
				<p class="hint" style={{ color: 'var(--bad)' }}>
					{t('sub.list.down')}
				</p>
			) : (
				<div class="s-list" data-part="sub-list">
					{sources.map((x) =>
						editing === x.id ? (
							<div key={x.id} class="s-src">
								<div class="s-name">{nameOf(x)}</div>
								{form(x.id)}
							</div>
						) : (
							<div key={x.id} class="s-src" data-sub={x.id}>
								<div class="s-top">
									<span class="s-name">
										{nameOf(x)}
										{x.hwid ? <span class="tag tag-dim"> · {t('sub.tag.hwid')}</span> : null}
									</span>
									<SourceResult x={x} updating={updating} lang={lang} t={t} />
								</div>
								<div class="s-bottom">
									{/* Наружу уходит только МАСКА (ADR-0012, ADR-0034): ни
									    одного символа секрета — ни из параметров, ни из пути. */}
									<span class="s-url" title={x.masked}>
										{x.masked || t('sub.url.none')}
									</span>
									<span class="s-acts">
										<button
											type="button"
											class="mini"
											disabled={!!lock.busy}
											onClick={() => open(x.id, { label: x.label, url: '', hwid: x.hwid })}
										>
											{t('sub.url.edit')}
										</button>
										{x.id === MAIN ? null : (
											<button
												type="button"
												class="mini"
												disabled={!!lock.busy}
												aria-busy={lock.on('subdel', x.id)}
												onClick={() => {
													setEditing(null);
													setAsking(x.id);
												}}
											>
												{t('sub.del')}
											</button>
										)}
									</span>
								</div>
								{x.last && x.last.status === 'fail' ? (
									<>
										<p class="hint" style={{ color: 'var(--bad)' }}>
											{x.last.error}
										</p>
										{x.last.ok_at ? (
											<p class="hint">{t('sub.kept', { n: x.last.nodes, when: fmtTime(x.last.ok_at, lang) })}</p>
										) : null}
									</>
								) : null}
								{asking === x.id ? (
									<Confirm
										title={t('sub.del.title', { name: nameOf(x) })}
										text={t('sub.del.text', { n: x.last?.nodes ?? 0 })}
										go={t('sub.del.go')}
										cancel={t('wifi.cancel')}
										danger
										forAction={`subdel:${x.id}`}
										onGo={() => {
											setAsking(null);
											onDelete(x.id);
										}}
										onCancel={() => setAsking(null)}
									/>
								) : null}
							</div>
						),
					)}
					{editing === 'new' ? <div class="s-src">{form(null)}</div> : null}
				</div>
			)}

			{subs && editing !== 'new' ? (
				<button
					type="button"
					class="linkbtn"
					disabled={!!lock.busy || full}
					title={full ? t('sub.add.full', { max }) : ''}
					onClick={() => open('new', { label: '', url: '', hwid: false })}
				>
					{full ? t('sub.add.full', { max }) : t('sub.add')}
				</button>
			) : null}

			{unset ? (
				<p class="hint" style={{ color: 'var(--warn)' }}>
					{t('sub.unset')}
				</p>
			) : null}

			<button type="button" class="wide" disabled={locked || unset} aria-busy={updating} onClick={onUpdate}>
				{updating ? (
					<>
						<Spin /> {t('sub.updating')}
					</>
				) : sources.length > 1 ? (
					t('sub.update.all', { n: s?.nodes ?? 0 })
				) : (
					t('sub.update')
				)}
			</button>

			{s && s.status === 'fail' && s.error ? (
				<p class="hint" style={{ color: 'var(--bad)' }}>
					{s.error}
				</p>
			) : null}
			{s && s.partial ? (
				<p class="hint" style={{ color: 'var(--warn)' }}>
					{t('sub.partial')}
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
					{logs.lines.map((l, i) => {
						// ok с непустым err — «частично»: обновились не все подписки.
						const part = l.status === 'ok' && !!l.err;
						return (
							<div key={i} class="log-line">
								<span style={{ color: part ? 'var(--warn)' : l.status === 'ok' ? 'var(--ok)' : 'var(--bad)' }}>
									{part ? '!' : l.status === 'ok' ? '✓' : '✕'}
								</span>
								<span>{fmtTime(l.ts, lang)}</span>
								<span>
									{l.status === 'ok' ? t('sub.nodes.short', { n: l.nodes }) : l.err || '—'}
									{part ? ` · ${l.err}` : ''}
								</span>
							</div>
						);
					})}
				</div>
			)}
		</>
	);
}

/** Итог последней попытки по одной подписке — справа в её строке. */
function SourceResult({ x, updating, lang, t }: { x: SubscriptionSource; updating: boolean; lang: Lang; t: T }) {
	if (updating && x.configured) {
		return (
			<span class="s-res">
				<Spin />
			</span>
		);
	}
	if (!x.configured) return <span class="s-res">{t('sub.url.none')}</span>;
	if (!x.last) return <span class="s-res">{t('sub.never')}</span>;
	const ok = x.last.status === 'ok';
	return (
		<span class={`s-res ${ok ? 'ok' : 'bad'}`}>
			{ok ? '✓' : '✕'} {fmtTime(x.last.at, lang)}
			{ok ? ` · ${t('sub.nodes.short', { n: x.last.nodes })}` : ''}
		</span>
	);
}
