import { useState } from 'preact/hooks';
import { jobOn } from '../state/job';
import type { Lock } from '../state/lock';
import type { Side } from '../state/side';
import type { BridgeState, Job } from '../api/types';
import type { T } from '../i18n';
import { Confirm, Skel, Spin } from './bits';

export interface BridgeProps {
	bridge: Side<BridgeState>;
	lock: Lock;
	locked: boolean;
	/** Идущий джоб: кнопка держит кольцо до конца операции, а не до 202. */
	running: Job | null;
	t: T;
	onProbe(): void;
	onAccess(on: boolean): void;
	onDisable(): void;
	onOpenForm(): void;
}

export function bridgeSummary(bridge: Side<BridgeState>, t: T): string {
	if (bridge === undefined) return '…';
	if (bridge === null) return t('bridge.down');
	if (!bridge.enabled) return t('bridge.sum.off');
	const gwSilent = bridge.probes?.gateway ? !bridge.probes.gateway.answered : false;
	return gwSilent ? t('bridge.sum.gw.silent') : t('bridge.sum.on');
}

/** Есть ли у проброса состояние, о котором надо сказать наверху экрана. */
export function bridgeProblem(bridge: Side<BridgeState>): 'gateway' | 'pc' | null {
	if (!bridge || !bridge.enabled || !bridge.probes) return null;
	if (bridge.probes.gateway && !bridge.probes.gateway.answered) return 'gateway';
	if (bridge.probes.pc && !bridge.probes.pc.answered) return 'pc';
	return null;
}

/**
 * Проброс LAN в uplink.
 *
 * Порядок внутри раздела: диагноз → действие → параметры → опасное действие.
 * Раньше первыми встречали восемь строк техданных, и владелец, пришедший
 * с вопросом «работает или нет», читал их все, чтобы догадаться.
 */
export function Bridge({ bridge, lock, locked, running, t, onProbe, onAccess, onDisable, onOpenForm }: BridgeProps) {
	// Ключ замка и arg джоба совпадают (access-on, access-off, disable,
	// enable): кольцо стоит на той кнопке, которую нажали, от клика до
	// конца операции у демона.
	const doing = (arg: string) => lock.on('bridge', arg) || jobOn(running, 'bridge', arg);
	const [ask, setAsk] = useState(false);
	const [details, setDetails] = useState(false);

	if (bridge === undefined) return <Skel n={3} />;
	if (bridge === null) return <div class="empty">{t('bridge.down')}</div>;

	const { enabled, port, relayd, uplink, probes } = bridge;
	const dash = '—';

	return (
		<>
			<p class="prose">
				{enabled ? t('bridge.on.text', { ip: bridge.pc_ip ?? dash }) : t('bridge.off.text')}
			</p>

			{enabled ? (
				<>
					<div class="label">{t('bridge.access.label')}</div>
					<div class="seg">
						{/* Два сегмента, а не чекбокс: в панели уже есть ровно
						    один идиом для «одно из N», и второй язык для того
						    же смысла — это два разных контрола для скринридера. */}
						<button
							type="button"
							aria-pressed={!bridge.ap_access}
							disabled={locked || !bridge.ap_access}
							aria-busy={doing('access-off')}
							onClick={() => onAccess(false)}
						>
							{doing('access-off') ? <Spin /> : null} {t('bridge.access.off')}
						</button>
						<button
							type="button"
							class="accent"
							aria-pressed={bridge.ap_access}
							disabled={locked || bridge.ap_access}
							aria-busy={doing('access-on')}
							onClick={() => onAccess(true)}
						>
							{doing('access-on') ? <Spin /> : null} {t('bridge.access.on')}
						</button>
					</div>
					<p class="hint">
						{bridge.ap_access ? t('bridge.access.hint.on') : t('bridge.access.hint.off')}
					</p>
				</>
			) : null}

			<div class="form-row">
				{enabled ? (
					<button
						type="button"
						class="wide danger"
						disabled={locked}
						aria-busy={doing('disable')}
						onClick={() => setAsk(true)}
					>
						{doing('disable') ? (
							<>
								<Spin /> {t('bridge.disabling')}
							</>
						) : (
							t('bridge.disable')
						)}
					</button>
				) : (
					<button type="button" class="wide primary" disabled={locked} aria-busy={doing('enable')} onClick={onOpenForm}>
						{doing('enable') ? (
							<>
								<Spin /> {t('bridge.enabling')}
							</>
						) : (
							t('bridge.enable')
						)}
					</button>
				)}
				<button
					type="button"
					class="wide"
					disabled={locked}
					aria-busy={lock.on('bridge', 'probe')}
					onClick={onProbe}
				>
					{lock.on('bridge', 'probe') ? (
						<>
							<Spin /> {t('bridge.probing')}
						</>
					) : (
						t('bridge.probe')
					)}
				</button>
			</div>

			{ask ? (
				<Confirm
					forAction="bridge-off"
					danger
					title={t('bridge.confirm.title')}
					text={t('bridge.confirm.text', { ip: bridge.pc_ip ?? dash })}
					go={t('bridge.confirm.go')}
					cancel={t('wifi.cancel')}
					onGo={() => {
						setAsk(false);
						onDisable();
					}}
					onCancel={() => setAsk(false)}
				/>
			) : null}

			{/* Техданные под «показать»: восемь строк параметров больше не
			    встречают первыми того, кто пришёл с вопросом «работает или
			    нет». На широком экране места хватает, но правило одно —
			    иначе раздел читается по-разному на двух ширинах. */}
			<button type="button" class="linkbtn" aria-expanded={details} onClick={() => setDetails(!details)}>
				{details ? t('bridge.details.hide') : t('bridge.details.show')}
			</button>

			{details ? (
				<div class="diags">
					<Diag k={t('bridge.diag.relayd')} v={
						!relayd ? t('bridge.unknown')
							: !relayd.installed ? t('bridge.relayd.absent')
								: relayd.running ? t('bridge.relayd.running') : t('bridge.relayd.stopped')
					} tone={!relayd ? '' : relayd.running ? 'lat-ok' : enabled ? 'lat-bad' : ''} />
					<Diag k={t('bridge.diag.port')} v={
						!port ? t('bridge.unknown')
							: port.carrier
								? t('bridge.port.up', { name: port.name, speed: port.speed_mbps ?? dash })
								: t('bridge.port.down', { name: port.name })
					} tone={!port ? '' : port.carrier ? 'lat-ok' : 'lat-warn'} />
					{port && port.carrier_changes != null ? (
						<Diag
							k={t('bridge.diag.flaps')}
							v={port.carrier_changes > 20
								? t('bridge.flaps.many', { n: port.carrier_changes })
								: String(port.carrier_changes)}
							tone={port.carrier_changes > 20 ? 'lat-bad' : ''}
						/>
					) : null}
					<Diag k={t('bridge.diag.uplink')} v={
						!uplink || !uplink.up ? t('bridge.uplink.down')
							: t('bridge.uplink.up', {
									addr: uplink.address ?? dash,
									mask: uplink.mask ?? dash,
									gw: uplink.gateway ?? dash,
								})
					} tone={uplink && uplink.up ? 'lat-ok' : 'lat-bad'} />
					{enabled ? <Diag k={t('bridge.diag.leg')} v={bridge.leg_ip ?? dash} /> : null}
					{probes ? (
						<>
							<Diag k={t('bridge.diag.pc')} v={
								!probes.pc ? t('bridge.unknown')
									: probes.pc.answered ? t('bridge.probe.ok') : t('bridge.probe.silent')
							} tone={!probes.pc ? '' : probes.pc.answered ? 'lat-ok' : 'lat-bad'} />
							<Diag k={t('bridge.diag.gw')} v={
								!probes.gateway ? t('bridge.unknown')
									: probes.gateway.answered ? t('bridge.probe.ok') : t('bridge.probe.silent')
							} tone={!probes.gateway ? '' : probes.gateway.answered ? 'lat-ok' : 'lat-bad'} />
						</>
					) : null}
				</div>
			) : null}
		</>
	);
}

function Diag({ k, v, tone }: { k: string; v: string | number; tone?: string }) {
	return (
		<div class="diag">
			<span>{k}</span>
			<span class={tone}>{v}</span>
		</div>
	);
}
