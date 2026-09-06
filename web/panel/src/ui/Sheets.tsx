import { useState } from 'preact/hooks';
import type { Lock } from '../state/lock';
import type { BridgeState } from '../api/types';
import type { T } from '../i18n';
import { Spin } from './bits';

export interface NetSeed {
	mode: 'add' | 'edit';
	id?: string;
	ssid?: string;
	encryption?: string;
}

export interface NetBody {
	id?: string;
	ssid: string;
	encryption: string;
	key?: string;
}

const ENC = ['psk2', 'psk-mixed', 'sae', 'sae-mixed', 'psk'];

/**
 * Форма сети. Снизу на телефоне, диалогом на десктопе: низ экрана — зона
 * большого пальца, центр читается как диалог.
 *
 * Это <form>, и все кнопки внутри обязаны быть type="button", кроме той,
 * что отправляет. Кнопка без типа отправляет форму: «скрыть уведомление»
 * означало бы «сохранить сеть».
 */
export function NetworkForm({
	seed,
	savedSsids,
	lock,
	locked,
	t,
	onSave,
	onSaveConnect,
	onClose,
}: {
	seed: NetSeed;
	savedSsids: Set<string>;
	lock: Lock;
	locked: boolean;
	t: T;
	onSave(body: NetBody, setErr: (s: string) => void): void;
	onSaveConnect(body: NetBody, setErr: (s: string) => void): void;
	onClose(): void;
}) {
	const editing = seed.mode === 'edit';
	const [ssid, setSsid] = useState(seed.ssid ?? '');
	const [enc, setEnc] = useState(seed.encryption ?? 'psk2');
	const [key, setKey] = useState('');
	const [show, setShow] = useState(false);
	const [err, setErr] = useState('');

	const needsKey = enc !== 'none';
	const dup = !editing && ssid.trim() !== '' && savedSsids.has(ssid.trim());
	const busy = !!lock.busy;

	const build = (): NetBody | null => {
		const s = ssid.trim();
		if (!editing && !s) {
			setErr(t('wifi.err.ssid'));
			return null;
		}
		// Пустой пароль при правке означает «не менять» — поэтому проверка
		// длины к нему не применяется.
		if (needsKey && key !== '' && (key.length < 8 || key.length > 63)) {
			setErr(t('wifi.err.short'));
			return null;
		}
		if (needsKey && !editing && key === '') {
			setErr(t('wifi.err.short'));
			return null;
		}
		const body: NetBody = { ssid: s, encryption: enc };
		if (seed.id) body.id = seed.id;
		if (key !== '') body.key = key;
		return body;
	};

	return (
		<div class="sheet-bg" role="dialog" aria-modal="true" aria-label={editing ? t('wifi.sheet.edit', { ssid: seed.ssid ?? '' }) : t('wifi.sheet.add')}>
			<form
				class="sheet"
				onSubmit={(e) => {
					e.preventDefault();
					const b = build();
					if (b) onSave(b, setErr);
				}}
			>
				<h3>{editing ? t('wifi.sheet.edit', { ssid: seed.ssid ?? '' }) : t('wifi.sheet.add')}</h3>

				{!editing ? (
					<>
						<label class="field">
							<span>{t('wifi.field.ssid')}</span>
							<input
								value={ssid}
								autocomplete="off"
								spellcheck={false}
								onInput={(e) => {
									setSsid((e.target as HTMLInputElement).value);
									setErr('');
								}}
							/>
						</label>
						{dup ? <p class="hint">{t('wifi.hint.dup', { ssid: ssid.trim() })}</p> : null}
						<label class="field">
							<span>{t('wifi.field.enc')}</span>
							<select value={enc} onChange={(e) => setEnc((e.target as HTMLSelectElement).value)}>
								{ENC.map((v) => (
									<option key={v} value={v}>
										{v}
									</option>
								))}
								<option value="none">{t('wifi.enc.open')}</option>
							</select>
						</label>
					</>
				) : null}

				{needsKey ? (
					<>
						<label class="field">
							<span>{t('wifi.field.key')}</span>
							<div class="form-row">
								<input
									type={show ? 'text' : 'password'}
									value={key}
									autocomplete="new-password"
									spellcheck={false}
									onInput={(e) => {
										setKey((e.target as HTMLInputElement).value);
										setErr('');
									}}
								/>
								<button type="button" class="mini" onClick={() => setShow(!show)}>
									{show ? t('wifi.hide') : t('wifi.show')}
								</button>
							</div>
						</label>
						{editing ? <p class="hint">{t('wifi.keykept')}</p> : null}
					</>
				) : null}

				{err ? <p class="hint" style={{ color: 'var(--bad)' }}>{err}</p> : null}
				<p class="hint">{t('wifi.keyhint')}</p>

				<div class="sheet-actions">
					<button
						type="submit"
						class="wide primary"
						disabled={busy || locked}
						aria-busy={lock.on('save')}
					>
						{lock.on('save') ? (
							<>
								<Spin /> {t('wifi.saving')}
							</>
						) : (
							t('wifi.save')
						)}
					</button>
					{/* «Сохранить и подключиться» — надмножество по риску, и
					    действием по умолчанию быть не может: сеть дачи заводят
					    заранее, не собираясь туда сегодня. */}
					{!editing ? (
						<button
							type="button"
							class="wide"
							disabled={busy || locked}
							onClick={() => {
								const b = build();
								if (b) onSaveConnect(b, setErr);
							}}
						>
							{t('wifi.sheet.saveconnect')}
						</button>
					) : null}
					{/* Выход из формы НЕ запирается чужой операцией: запирать
					    «Отмена» из-за постороннего действия — это тот самый
					    disabled в роли мьютекса, от которого ушли. */}
					<button type="button" class="wide" onClick={onClose}>
						{t('wifi.cancel')}
					</button>
				</div>
			</form>
		</div>
	);
}

export interface BridgeBody {
	leg_ip: string;
	pc_ip: string;
	ap_access: boolean;
}

export function BridgeForm({
	state,
	lock,
	locked,
	t,
	onSubmit,
	onClose,
}: {
	state: BridgeState;
	lock: Lock;
	locked: boolean;
	t: T;
	onSubmit(body: BridgeBody, setErr: (s: string) => void): void;
	onClose(): void;
}) {
	const [leg, setLeg] = useState(state.leg_ip ?? '');
	const [pc, setPc] = useState(state.pc_ip ?? '');
	const [access, setAccess] = useState(state.ap_access);
	const [err, setErr] = useState('');

	const up = state.uplink;
	const net = up && up.up && up.address ? `${up.address}/${up.mask ?? ''}` : '';

	const ip4 = (s: string) =>
		/^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.test(s) &&
		s.split('.').every((p) => Number(p) <= 255);

	return (
		<div class="sheet-bg" role="dialog" aria-modal="true" aria-label={t('bridge.sheet.title')}>
			<form
				class="sheet"
				onSubmit={(e) => {
					e.preventDefault();
					if (!up || !up.up) {
						setErr(t('bridge.err.uplink'));
						return;
					}
					if (!ip4(leg)) {
						setErr(t('bridge.err.leg'));
						return;
					}
					if (!ip4(pc)) {
						setErr(t('bridge.err.pc'));
						return;
					}
					if (leg === pc || leg === up.gateway || pc === up.gateway) {
						setErr(t('bridge.err.conflict'));
						return;
					}
					onSubmit({ leg_ip: leg, pc_ip: pc, ap_access: access }, setErr);
				}}
			>
				<h3>{t('bridge.sheet.title')}</h3>
				<p class="hint">{t('bridge.sheet.intro', { net: net || '—' })}</p>

				<label class="field">
					<span>{t('bridge.field.leg')}</span>
					<input value={leg} inputMode="decimal" onInput={(e) => { setLeg((e.target as HTMLInputElement).value); setErr(''); }} />
				</label>
				<p class="hint">{t('bridge.field.leg.hint')}</p>

				<label class="field">
					<span>{t('bridge.field.pc')}</span>
					<input value={pc} inputMode="decimal" onInput={(e) => { setPc((e.target as HTMLInputElement).value); setErr(''); }} />
				</label>
				<p class="hint">{t('bridge.field.pc.hint')}</p>

				<label class="field check">
					<input
						type="checkbox"
						checked={access}
						onChange={(e) => setAccess((e.target as HTMLInputElement).checked)}
					/>
					<span>{t('bridge.field.access')}</span>
				</label>
				<p class="hint">{t('bridge.field.access.hint')}</p>

				<div class="confirm danger">
					<b>{t('bridge.sheet.warn.title')}</b>
					<p>{t('bridge.sheet.warn')}</p>
				</div>

				{err ? <p class="hint" style={{ color: 'var(--bad)' }}>{err}</p> : null}

				<div class="sheet-actions">
					<button
						type="submit"
						class="wide primary"
						disabled={locked}
						aria-busy={lock.on('bridge', 'enable')}
					>
						{lock.on('bridge', 'enable') ? (
							<>
								<Spin /> {t('bridge.enabling')}
							</>
						) : (
							t('bridge.enable')
						)}
					</button>
					<button type="button" class="wide" onClick={onClose}>
						{t('wifi.cancel')}
					</button>
				</div>
			</form>
		</div>
	);
}
