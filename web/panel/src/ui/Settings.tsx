import type { ComponentChildren } from 'preact';
import type {
	AutopoolDraft,
	AutopoolResponse,
	BridgeState,
	Job,
	LogsResponse,
	ProxiesResponse,
	RulesDraft,
	RulesetsCatalog,
	RulesetsResponse,
	Status,
	SubscriptionURL,
} from '../api/types';
import type { Lang, T } from '../i18n';
import type { Lock } from '../state/lock';
import type { Side } from '../state/side';
import { Section } from './bits';
import { Autopool, poolSummary } from './Autopool';
import { Bridge, bridgeProblem, bridgeSummary } from './Bridge';
import { Rulesets, rulesSummary } from './Rulesets';
import { Subscription, subSummary } from './Subscription';

/** Разделы экрана настроек — они же хеши адреса: #routes, #sub, #bridge. */
export type SettingsRow = 'routes' | 'pool' | 'sub' | 'bridge';
export const SETTINGS_ROWS: SettingsRow[] = ['routes', 'pool', 'sub', 'bridge'];

export interface SettingsProps {
	/** Открытый раздел на узком экране; на широком раскрыты все. */
	row: SettingsRow | '';
	onRow(id: SettingsRow): void;
	wide: boolean;
	t: T;
	lang: Lang;
	lock: Lock;
	locked: boolean;
	/** Идущий джоб — чтобы кнопка держала кольцо до конца операции, а не до 202. */
	running: Job | null;
	status: Status;

	rulesets: Side<RulesetsResponse>;
	catalog: Side<RulesetsCatalog>;
	draft: RulesDraft | null;
	setDraft(d: RulesDraft | null): void;
	onApplyRules(d: RulesDraft): void;
	onLoadCatalog(): void;

	autopool: Side<AutopoolResponse>;
	poolDraft: AutopoolDraft | null;
	setPoolDraft(d: AutopoolDraft | null): void;
	onApplyPool(d: AutopoolDraft): void;

	sub: Side<SubscriptionURL>;
	logs: Side<LogsResponse>;
	nikki: Side<ProxiesResponse>;
	onUpdateSub(): void;
	onSaveURL(url: string): void;

	bridge: Side<BridgeState>;
	onBridgeProbe(): void;
	onBridgeAccess(on: boolean): void;
	onBridgeDisable(): void;
	onOpenBridgeForm(): void;
	/** Блок провала проброса — живёт под разделом проброса, а не на главной. */
	bridgeFail?: ComponentChildren;
}

/**
 * Экран настроек: то, что настраивают раз и надолго.
 *
 * Три раздела в одну колонку. Строки — тот же Section, что и на главной: на
 * узком экране кнопка с aria-expanded и сводкой, открыт ровно один (два
 * развёрнутых снова дали бы страницу в два экрана); на широком — обычные
 * карточки без кнопок, все раскрыты. Закрытый раздел выглядит как строка
 * полки на главной — переход «полка → настройки» не меняет картинку,
 * меняет только открытую строку (ADR-0042).
 */
export function Settings(p: SettingsProps) {
	const { t, wide } = p;
	const open = (id: SettingsRow) => wide || p.row === id;
	const problem = bridgeProblem(p.bridge);

	return (
		<div class="grid settings" data-part="grid">
			<Section id="routes" title={t('shelf.rules')} summary={rulesSummary(p.rulesets, p.draft, t)} wide={wide} open={open('routes')} onToggle={() => p.onRow('routes')} t={t} full>
				<Rulesets
					applied={p.rulesets}
					catalog={p.catalog}
					draft={p.draft}
					setDraft={p.setDraft}
					lang={p.lang}
					lock={p.lock}
					locked={p.locked}
					running={p.running}
					t={t}
					onApply={p.onApplyRules}
					onLoadCatalog={p.onLoadCatalog}
				/>
			</Section>

			{/* Авто-пул стоит сразу за наборами: обе секции живут в одном
			    mixin.yaml и отвечают на соседние вопросы — что идёт в
			    туннель и через какие узлы. */}
			<Section id="pool" title={t('shelf.pool')} summary={poolSummary(p.autopool, p.poolDraft, t)} wide={wide} open={open('pool')} onToggle={() => p.onRow('pool')} t={t} full>
				<Autopool
					applied={p.autopool}
					draft={p.poolDraft}
					setDraft={p.setPoolDraft}
					lock={p.lock}
					locked={p.locked}
					running={p.running}
					t={t}
					onApply={p.onApplyPool}
				/>
			</Section>

			<Section id="sub" title={t('shelf.sub')} summary={subSummary(p.status, p.lang, t)} wide={wide} open={open('sub')} onToggle={() => p.onRow('sub')} t={t} full>
				<Subscription
					status={p.status}
					sub={p.sub}
					logs={p.logs}
					nikki={p.nikki}
					lang={p.lang}
					lock={p.lock}
					locked={p.locked}
					running={p.running}
					t={t}
					onUpdate={p.onUpdateSub}
					onSaveURL={p.onSaveURL}
				/>
			</Section>

			<Section
				id="bridge"
				title={t('shelf.bridge')}
				summary={bridgeSummary(p.bridge, t)}
				wide={wide}
				open={open('bridge')}
				onToggle={() => p.onRow('bridge')}
				t={t}
				full
				warn={!!problem}
			>
				<Bridge
					bridge={p.bridge}
					lock={p.lock}
					locked={p.locked}
					running={p.running}
					t={t}
					onProbe={p.onBridgeProbe}
					onAccess={p.onBridgeAccess}
					onDisable={p.onBridgeDisable}
					onOpenForm={p.onOpenBridgeForm}
				/>
			</Section>

			{p.bridgeFail}
		</div>
	);
}
