import { useState } from 'preact/hooks';
import type { Lock } from '../state/lock';
import type { Side } from '../state/side';
import type { NetworksResponse, SavedNetwork, ScanResponse, Status } from '../api/types';
import type { T } from '../i18n';
import { Confirm, Skel, Spin, sigClass } from './bits';

export interface UplinkProps {
	nets: Side<NetworksResponse>;
	scan: Side<ScanResponse>;
	status: Status;
	lock: Lock;
	locked: boolean;
	t: T;
	onScan(): void;
	onConnect(n: SavedNetwork): void;
	onDelete(n: SavedNetwork): void;
	onOpenForm(seed: { mode: 'add' | 'edit'; id?: string; ssid?: string; encryption?: string }): void;
}

/**
 * Внешняя сеть: активная, сохранённые, найденные.
 *
 * Порядок разделов — по частоте: сначала то, через что роутер работает
 * сейчас, потом то, на что можно переключиться, потом эфир.
 */
export function Uplink({
	nets,
	scan,
	status,
	lock,
	locked,
	t,
	onScan,
	onConnect,
	onDelete,
	onOpenForm,
}: UplinkProps) {
	// Подтверждение живёт ЗДЕСЬ, а не в общем состоянии панели: оно
	// принадлежит строке, рядом с которой раскрывается, и уносить его выше
	// значило бы уметь показать его не там, где нажали.
	const [ask, setAsk] = useState<SavedNetwork | null>(null);

	const saved = nets?.networks ?? [];
	const found = scan?.networks ?? [];
	const savedSsids = new Set(saved.map((n) => n.ssid));
	const extra = found.filter((f) => f.ssid && !savedSsids.has(f.ssid));
	const hidden = found.filter((f) => !f.ssid);
	const sig = new Map(found.filter((f) => f.ssid).map((f) => [f.ssid, f.signal_dbm]));

	// В неоднозначном состоянии правка и удаление запрещены (ADR-0026), а
	// «Подключить» работает: выбрать одну сеть — это и есть способ выйти.
	const frozen = status.selection_state === 'ambiguous';
	const busyAny = !!lock.busy;

	return (
		<>
			<div class="label">{t('wifi.group.saved')}</div>

			{nets === undefined ? (
				<Skel n={2} />
			) : nets === null ? (
				<div class="empty">{t('wifi.down')}</div>
			) : saved.length === 0 ? (
				<div class="empty">{t('sel.empty.title')}</div>
			) : (
				<div class="rows">
					{saved.map((n) => (
						<div key={n.id} class={`row${n.enabled ? ' sel' : ''}`}>
							<span class="name">{n.ssid || t('wifi.hidden')}</span>
							<span class="tag tag-dim">{n.enabled ? t('wifi.active') : t('wifi.saved')}</span>
							{sig.has(n.ssid) ? (
								<span class={`ms ${sigClass(sig.get(n.ssid) as number)}`}>
									{sig.get(n.ssid)} dBm
								</span>
							) : null}

							{/* Показывать ли «Подключить», решает СЕРВЕР полем switchable.
							    Своей копии правила тут нет — иначе она разойдётся
							    с ADR-0026. У активной сети оно false, и кнопки нет
							    вовсе: серых заглушек в этой панели не бывает. */}
							{n.switchable || n.editable ? (
								<span class="acts">
									{n.switchable ? (
										<button
											type="button"
											class="mini grow"
											disabled={locked}
											aria-busy={lock.on('switch', n.id)}
											onClick={() => setAsk(n)}
										>
											{lock.on('switch', n.id) ? (
												<>
													<Spin /> {t('wifi.connecting')}
												</>
											) : (
												t('wifi.connect')
											)}
										</button>
									) : null}
									{n.editable ? (
										<>
											<button
												type="button"
												class="mini ghosted"
												disabled={busyAny}
												onClick={() =>
													onOpenForm({
														mode: 'edit',
														id: n.id,
														ssid: n.ssid,
														encryption: n.encryption,
													})
												}
											>
												{t('wifi.key')}
											</button>
											<button
												type="button"
												class="mini danger"
												title={t('wifi.delete')}
												aria-label={t('wifi.delete')}
												disabled={busyAny}
												aria-busy={lock.on('del', n.id)}
												onClick={() => onDelete(n)}
											>
												{lock.on('del', n.id) ? <Spin /> : '✕'}
											</button>
										</>
									) : (
										<span
											class="mark"
											title={n.enabled ? t('wifi.locked') : t('sel.ambiguous.title')}
										>
											🔒
										</span>
									)}
								</span>
							) : null}
						</div>
					))}
				</div>
			)}

			{ask ? (
				<Confirm
					forAction="uplink"
					title={t('wifi.confirm.title', { ssid: ask.ssid })}
					text={t('wifi.confirm.text')}
					go={t('wifi.confirm.go')}
					cancel={t('wifi.cancel')}
					onGo={() => {
						const n = ask;
						setAsk(null);
						onConnect(n);
					}}
					onCancel={() => setAsk(null)}
				/>
			) : null}

			{extra.length > 0 ? (
				<>
					<div class="label">
						{t('wifi.group.found')}
						{scan?.band ? ` · ${scan.band === '2g' ? '2.4' : '5'} ${t('wifi.ghz')}` : ''}
					</div>
					<div class="rows">
						{extra.map((f) => (
							<button
								key={f.ssid}
								type="button"
								class="row"
								disabled={busyAny || frozen}
								onClick={() => onOpenForm({ mode: 'add', ssid: f.ssid, encryption: f.encryption })}
							>
								<span class="name">{f.ssid}</span>
								<span class="tag tag-dim">
									{f.encryption === 'none' ? t('wifi.none') : f.encryption}
								</span>
								<span class={`ms ${sigClass(f.signal_dbm)}`}>{f.signal_dbm} dBm</span>
							</button>
						))}
					</div>
				</>
			) : null}

			<div class="form-row">
				<button
					type="button"
					class="wide dashed"
					disabled={busyAny || frozen}
					onClick={() => onOpenForm({ mode: 'add' })}
				>
					{t('wifi.add.manual')}
				</button>
				<button
					type="button"
					class="wide dashed"
					disabled={busyAny}
					aria-busy={lock.on('scan')}
					onClick={onScan}
				>
					{lock.on('scan') ? (
						<>
							<Spin /> {t('wifi.scanning')}
						</>
					) : scan ? (
						t('wifi.rescan')
					) : (
						t('wifi.scan')
					)}
				</button>
			</div>

			<p class="hint">
				{scan?.band === '2g' || scan?.band === '5g'
					? t('wifi.band', {
							band: scan.band === '2g' ? '2.4' : '5',
							other: scan.band === '2g' ? '5' : '2.4',
						}) + ' '
					: ''}
				{hidden.length > 0 ? t('wifi.hidden.cant.n', { n: hidden.length }) : t('wifi.hidden.cant')}
			</p>
		</>
	);
}
