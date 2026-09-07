import { useState } from 'preact/hooks';
import type { RulesDraft, RulesetsCatalog, RulesetsResponse } from '../api/types';
import type { Key, T } from '../i18n';
import type { Lock } from '../state/lock';
import type { Side } from '../state/side';
import { Confirm, Skel, Spin } from './bits';

/**
 * Сколько имён показывается разом. Каталог — тысяча девятьсот наборов, и
 * список целиком не читают: его либо сужают поиском, либо берут паком.
 */
const ROWS = 8;

export interface RulesetsProps {
	/** Что применено. Три значения побочного списка, и они разные. */
	applied: Side<RulesetsResponse>;
	/** Имена с GitHub. Грузится лениво: 60 КБ ради вкладки, куда заходят редко. */
	catalog: Side<RulesetsCatalog>;
	/** null означает «совпадает с применённым», а не «пусто». */
	draft: RulesDraft | null;
	setDraft(d: RulesDraft | null): void;
	lock: Lock;
	locked: boolean;
	t: T;
	onApply(d: RulesDraft): void;
	onLoadCatalog(): void;
}

/**
 * Черновик из применённого.
 *
 * Отдельная функция, потому что «ничего не меняли» выражено null-ом:
 * дальше по вкладке считать надо один объект, а не разбирать два случая
 * в каждой строке.
 */
export function draftOf(a: Side<RulesetsResponse>): RulesDraft {
	return {
		policy: a?.policy ?? 'profile',
		download: a?.download ?? 'direct',
		sets: (a?.sets ?? []).map((s) => s.name),
	};
}

/**
 * Сколько правок ждёт применения. Ноль значит «применять нечего» — и это
 * не то же самое, что «черновика нет»: вернуть выбор к применённому руками
 * владелец вправе, и предлагать ему после этого «Применить» незачем.
 */
export function dirtyCount(a: Side<RulesetsResponse>, d: RulesDraft | null): number {
	if (!d) return 0;
	const base = draftOf(a);
	let n = 0;
	if (d.policy !== base.policy) n++;
	// Скачивание при profile демон игнорирует (правил панели в файле нет
	// вовсе), поэтому изменением оно там не считается: иначе панель
	// предлагала бы применить то, что ничего не поменяет.
	if (d.policy !== 'profile' && d.download !== base.download) n++;
	const was = new Set(base.sets);
	const now = new Set(d.sets);
	for (const s of now) if (!was.has(s)) n++;
	for (const s of was) if (!now.has(s)) n++;
	return n;
}

/**
 * «3 набора» по-русски.
 *
 * Числительное собирается выбором ключа, а не окончанием: общего правила
 * множественного числа в словаре нет намеренно (i18n/index.ts), и одно
 * исключение ради одной строки завело бы второй механизм подстановки.
 */
export function countSets(n: number, t: T): string {
	const ten = n % 10;
	const hundred = n % 100;
	if (ten === 1 && hundred !== 11) return t('rules.count.one', { n });
	if (ten >= 2 && ten <= 4 && (hundred < 12 || hundred > 14)) return t('rules.count.few', { n });
	return t('rules.count.many', { n });
}

/** Подпись «В туннель · 3 набора» — она же уходит в сводку свёрнутого раздела. */
export function onLabel(d: RulesDraft, t: T): string {
	return t(d.policy === 'except' ? 'rules.on.except' : 'rules.on.only', {
		n: countSets(d.sets.length, t),
	});
}

/**
 * Что идёт в туннель.
 *
 * Вкладка — чистое представление: состояние (вкладка, черновик) и запись
 * живут в App. Здесь только выбор того, ЧТО показать, потому что порядок
 * блоков и есть содержание раздела: диагноз (кто сейчас решает маршрут) →
 * действие (политика) → параметры (наборы) → опасное («вернуть профилю»).
 */
export function Rulesets(p: RulesetsProps) {
	const { applied, catalog, t } = p;
	const [query, setQuery] = useState('');
	const [ask, setAsk] = useState(false);

	if (applied === undefined) return <Skel n={3} />;
	// null — спросили и отказали. Это НЕ «наборов нет»: файл на диске жив,
	// и предлагать выбирать наборы поверх непрочитанного значило бы
	// предложить затереть неизвестно что.
	if (applied === null) return <div class="empty">{t('rules.applied.down')}</div>;

	const eff = p.draft ?? draftOf(applied);
	const dirty = dirtyCount(applied, p.draft);
	const busy = p.lock.on('rulesets');

	// Чужой файл замораживает вкладку целиком, а не одну кнопку: демон
	// отобьёт запись кодом foreign_mixin, и черновик, который заведомо
	// некуда применить, — это приглашение к отказу.
	const frozen = applied.foreign;
	const off = p.locked || frozen;

	const was = new Map(applied.sets.map((s) => [s.name, s]));
	const ipNames = new Set(catalog?.ip ?? []);
	// Признак ip берётся из каталога, а при его отсутствии — из применённого:
	// так метка не исчезает вместе с недоступным GitHub.
	const hasIP = (name: string) => ipNames.has(name) || !!was.get(name)?.ip;
	const missed = (name: string) => was.get(name)?.loaded === false;

	const applySets = (sets: string[]) =>
		p.setDraft({
			...eff,
			// Выбор набора при политике профиля означает «маршрут теперь
			// решает панель»: без переключения нажатие меняло бы только чип,
			// а трафик шёл бы по-прежнему весь в туннель.
			policy: eff.policy === 'profile' && sets.length > 0 ? 'only' : eff.policy,
			sets,
		});

	const toggleSet = (name: string) =>
		applySets(eff.sets.includes(name) ? eff.sets.filter((s) => s !== name) : [...eff.sets, name]);

	const togglePack = (names: string[]) =>
		// Пак, выбранный целиком, снимается целиком: иначе повторное нажатие
		// не делало бы ничего, и кнопка выглядела бы сломанной.
		applySets(
			names.every((n) => eff.sets.includes(n))
				? eff.sets.filter((n) => !names.includes(n))
				: [...eff.sets, ...names.filter((n) => !eff.sets.includes(n))],
		);

	const q = query.trim().toLowerCase();
	const packNames = (catalog?.packs ?? []).flatMap((k) => k.sets);
	// Без запроса показываются выбранное и частое, с запросом — совпадения.
	// Тысяча девятьсот имён списком не читаются ни на каком экране.
	const found = q
		? (catalog?.names ?? []).filter((n) => n.toLowerCase().includes(q))
		: [...new Set([...eff.sets, ...packNames])];

	return (
		<>
			<p class="prose">{t('rules.intro')}</p>

			{/* Молчащий движок — не отказ, а «не знаем»: выбор читается из
			    файла, а про загрузку наборов не известно ничего. */}
			{applied.live ? null : <p class="hint">{t('rules.live.down')}</p>}

			{frozen ? (
				<div class="confirm danger" data-part="confirm" data-confirm-for="rulesets-foreign">
					<b>{t('rules.foreign')}</b>
				</div>
			) : null}

			{/* Профиль — это не «пусто», а другой хозяин маршрута, поэтому
			    у переключателя политики нет нажатого положения: панель не
			    делает вид, что выбор за ней. */}
			{eff.policy === 'profile' ? <div class="summary">{t('rules.profile.note')}</div> : null}

			<div class="label">{t('rules.policy.label')}</div>
			<div class="seg">
				{(['only', 'except'] as const).map((k) => (
					<button
						key={k}
						type="button"
						class="accent"
						aria-pressed={eff.policy === k}
						disabled={off}
						onClick={() => p.setDraft({ ...eff, policy: k })}
					>
						{t(`rules.policy.${k}` as Key)}
					</button>
				))}
			</div>
			{eff.policy === 'profile' ? null : (
				<p class="hint">{t(`rules.policy.${eff.policy}.hint` as Key)}</p>
			)}

			<div class="label">{t('rules.download.label')}</div>
			<div class="seg">
				{(['direct', 'tunnel'] as const).map((k) => (
					<button
						key={k}
						type="button"
						aria-pressed={eff.download === k}
						// При профиле выбор скачивания не значит ничего: правил
						// панели в файле нет, качать нечего.
						disabled={off || eff.policy === 'profile'}
						onClick={() => p.setDraft({ ...eff, download: k })}
					>
						{t(`rules.download.${k}` as Key)}
					</button>
				))}
			</div>
			<p class="hint">{t('rules.download.hint')}</p>

			{/* Подпись и чипы рисуются только там, где панель решает маршрут.
			    «В туннель · 0 наборов» при профиле было бы ложью: туда идёт
			    весь трафик, просто не по нашим правилам. */}
			{eff.policy === 'profile' ? null : (
				<>
					<div class="label">{onLabel(eff, t)}</div>
					{eff.sets.length === 0 ? (
						<div class="empty">
							{t(eff.policy === 'except' ? 'rules.none.except' : 'rules.none.only')}
						</div>
					) : (
						<div class="chips">
							{eff.sets.map((n) => (
								<span key={n} class={`chip${missed(n) ? ' chip-bad' : ''}`}>
									<span>{n}</span>
									{hasIP(n) ? <i class="ip">{t('rules.chip.ip')}</i> : null}
									<button
										type="button"
										disabled={off}
										title={t('rules.chip.off')}
										aria-label={`${t('rules.chip.off')}: ${n}`}
										onClick={() => toggleSet(n)}
									>
										✕
									</button>
								</span>
							))}
						</div>
					)}
				</>
			)}

			{catalog?.packs?.length ? (
				<>
					<div class="label">{t('rules.packs')}</div>
					<div class="pills dashed">
						{catalog.packs.map((k) => (
							<button
								key={k.id}
								type="button"
								aria-pressed={k.sets.every((n) => eff.sets.includes(n))}
								disabled={off}
								onClick={() => togglePack(k.sets)}
							>
								{/* Незнакомый пак покажется своим id: демон и панель
								    обновляются порознь, и новый пак приедет раньше
								    перевода. */}
								{t(`pack.${k.id}` as Key)}
							</button>
						))}
					</div>
				</>
			) : null}

			<input
				class="search"
				type="search"
				value={query}
				placeholder={t('rules.search.placeholder')}
				aria-label={t('rules.search.placeholder')}
				// Замок поиску не мешает: это чтение, а не действие. Гасит
				// поле только чужой файл — там искать нечего, применить всё
				// равно не дадут.
				disabled={frozen}
				onFocus={p.onLoadCatalog}
				onInput={(e) => setQuery((e.currentTarget as HTMLInputElement).value)}
			/>

			{catalog === undefined ? (
				<Skel n={4} />
			) : catalog === null ? (
				<div class="empty">
					{/* Причина берётся из словаря, а не из ответа: побочный
					    список сообщение отказа не хранит. Поэтому текст
					    говорит про ПОСЛЕДСТВИЕ (имён нет, поиск не работает,
					    выбранное цело), а не выдумывает причину — 503 это был
					    или таймаут, панель отсюда не знает. */}
					{t('rules.catalog.down', { why: t('rules.catalog.why') })}{' '}
					<button type="button" class="linkbtn" onClick={p.onLoadCatalog}>
						{t('rules.catalog.retry')}
					</button>
				</div>
			) : (
				<>
					{/* Дата показывается КАК ПРИСЛАЛ демон, без пересчёта в
					    местное время: часы роутера и браузера расходятся, и
					    пересчёт врал бы на величину расхождения. */}
					{catalog.stale ? (
						<p class="hint">{t('rules.catalog.stale', { date: catalog.fetched_at.slice(0, 10) })}</p>
					) : null}

					<div class="rows">
						{found.slice(0, ROWS).map((n) => (
							<Row
								key={n}
								name={n}
								picked={eff.sets.includes(n)}
								applied={was.has(n)}
								except={eff.policy === 'except'}
								missed={missed(n)}
								off={off}
								t={t}
								onPick={() => toggleSet(n)}
							/>
						))}
					</div>

					<p class="hint">
						{q
							? found.length > 0
								? t('rules.found', { q: query.trim(), n: found.length })
								: t('rules.found.none', { q: query.trim() })
							: t('rules.found.default')}
					</p>
				</>
			)}

			{dirty > 0 ? (
				<div class="confirm" data-part="confirm" data-confirm-for="rulesets">
					<b>{t('rules.dirty.title', { n: dirty })}</b>
					<p>{t('rules.dirty.text')}</p>
					<div class="buttons">
						<button
							type="button"
							class="go"
							disabled={off}
							aria-busy={busy}
							onClick={() => p.onApply(eff)}
						>
							{busy ? (
								<>
									<Spin /> {t('rules.applying')}
								</>
							) : (
								t('rules.apply')
							)}
						</button>
						<button type="button" class="no" onClick={() => p.setDraft(null)}>
							{t('rules.reset')}
						</button>
					</div>
				</div>
			) : null}

			<p class="hint">{t('rules.source')}</p>

			{/* Возврат профилю стоит последним и открывается подтверждением:
			    он стирает весь выбор и перезапускает движок, а откатов
			    в проекте нет (ADR-0006). Пока правил панели и так нет,
			    кнопки нет вовсе — нажимать было бы не на что. */}
			{applied.policy === 'profile' ? null : (
				<>
					<button
						type="button"
						class="linkbtn"
						disabled={p.locked}
						aria-expanded={ask}
						onClick={() => setAsk(!ask)}
					>
						{t('rules.profile.back')}
					</button>
					{ask ? (
						<Confirm
							forAction="rulesets-profile"
							danger
							title={t('rules.profile.back')}
							text={t('rules.profile.back.text')}
							go={t('rules.profile.back')}
							cancel={t('wifi.cancel')}
							onGo={() => {
								setAsk(false);
								p.onApply({ policy: 'profile', download: 'direct', sets: [] });
							}}
							onCancel={() => setAsk(false)}
						/>
					) : null}
				</>
			)}
		</>
	);
}

/**
 * Строка каталога.
 *
 * Тегов может быть два, и это не многословие: «добавлен, не применён»
 * говорит про черновик, «не загрузился» — про то, что уже лежит в файле.
 * Один тег вместо двух скрыл бы либо намерение владельца, либо отказ
 * движка.
 */
function Row({
	name,
	picked,
	applied,
	except,
	missed,
	off,
	t,
	onPick,
}: {
	name: string;
	picked: boolean;
	applied: boolean;
	except: boolean;
	missed: boolean;
	off: boolean;
	t: T;
	onPick(): void;
}) {
	const staged = picked !== applied;
	return (
		<button type="button" class={`row${picked ? ' sel' : ''}`} disabled={off} onClick={onPick}>
			<span class="name">{name}</span>
			{staged ? (
				<span class="tag tag-pin">{t(picked ? 'rules.tag.added' : 'rules.tag.removed')}</span>
			) : picked ? (
				<span class="tag tag-dim">{t(except ? 'rules.tag.direct' : 'rules.tag.tunnel')}</span>
			) : null}
			{missed ? <span class="tag tag-bad">{t('rules.tag.notloaded')}</span> : null}
			<span class="id">geosite:{name}</span>
		</button>
	);
}
