import { useState } from 'preact/hooks';
import type { CustomRule, RuleAction, RuleKind, RulesDraft, RulesetsCatalog, RulesetsResponse } from '../api/types';
import type { Key, Lang, T } from '../i18n';
import type { Lock } from '../state/lock';
import type { Side } from '../state/side';
import { Confirm, Skel, Spin } from './bits';

/**
 * Сколько имён показывается разом. Каталог — тысяча девятьсот наборов, и
 * список целиком не читают: его либо сужают поиском, либо берут паком.
 */
const ROWS = 8;

/** Потолок своих правил — тот же, что у демона (rulesets.MaxRules). */
export const MAX_RULES = 64;
/** Потолок комментария в знаках — как у демона (rulesets.MaxCommentRunes). */
export const MAX_COMMENT = 80;

const KINDS: RuleKind[] = ['suffix', 'domain', 'cidr'];
const ACTIONS: RuleAction[] = ['tunnel', 'direct'];

export interface RulesetsProps {
	/** Что применено. Три значения побочного списка, и они разные. */
	applied: Side<RulesetsResponse>;
	/** Имена с GitHub. Грузится лениво: 60 КБ ради вкладки, куда заходят редко. */
	catalog: Side<RulesetsCatalog>;
	/** null означает «совпадает с применённым», а не «пусто». */
	draft: RulesDraft | null;
	setDraft(d: RulesDraft | null): void;
	/** Нужен ЧИСЛИТЕЛЬНОМУ: у русского три формы, у английского две. */
	lang: Lang;
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
		// Копии, а не те же объекты: строки правил правятся на месте, и
		// править применённое значило бы менять то, с чем сверяется черновик.
		rules: (a?.rules ?? []).map((r) => ({ ...r })),
	};
}

/** Правило одной строкой — для сверки черновика с применённым. Комментарий
 *  тоже в счёт: сменился текст — файл переписывается. */
const ruleTok = (r: CustomRule) => `${r.kind}:${r.value}>${r.action}|${r.comment}`;

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

	// Свои правила: добавленные и убранные — по одному, и ещё одно за
	// перестановку при том же составе. У наборов порядок не считается, у
	// правил считается: у mihomo побеждает первое совпадение, и переставить
	// два правила значит поменять, куда идёт домен.
	const wasR = base.rules.map(ruleTok);
	const nowR = d.rules.map(ruleTok);
	const wasSet = new Set(wasR);
	const nowSet = new Set(nowR);
	let diff = 0;
	for (const r of nowSet) if (!wasSet.has(r)) diff++;
	for (const r of wasSet) if (!nowSet.has(r)) diff++;
	n += diff;
	if (diff === 0 && wasR.join('\n') !== nowR.join('\n')) n++;
	return n;
}

/**
 * Что не так со строкой своего правила; null — всё в порядке.
 *
 * Зеркало серверной проверки (rulesets.ValidateRules), а не её замена:
 * демон отобьёт всё то же кодом bad_rule, но с ним владелец узнал бы об
 * опечатке только после нажатия «Применить», а не пока печатает. Верхний
 * регистр сюда не доезжает — поле приводит ввод к строчным на глазах.
 */
export function ruleProblem(r: CustomRule, all: CustomRule[], i: number): Key | null {
	if (i >= MAX_RULES) return 'rules.custom.max';
	const v = r.value;
	if (v === '') return 'rules.custom.err.empty';
	// Поле держит 80 знаков само (maxLength), но черновик мог прийти из
	// применённого или из вставки: проверка та же, что у демона.
	if ([...r.comment].length > MAX_COMMENT || /[\x00-\x1f\x7f]/.test(r.comment)) {
		return 'rules.custom.err.comment';
	}
	for (let j = 0; j < i; j++) {
		const prev = all[j];
		if (prev && prev.kind === r.kind && prev.value === v) return 'rules.custom.err.dup';
	}
	if (r.kind === 'cidr') return cidrProblem(v);
	return domainProblem(v);
}

const V4 = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/;
const V6ISH = /^[0-9a-f:]+$/;

function cidrProblem(v: string): Key | null {
	const cut = v.indexOf('/');
	if (cut < 0) return 'rules.custom.err.cidr';
	const addr = v.slice(0, cut);
	const bits = Number(v.slice(cut + 1));
	const m = V4.exec(addr);
	if (m) {
		const [a = 0, b = 0, c = 0, d = 0] = m.slice(1).map(Number);
		if ([a, b, c, d].some((o) => o > 255) || !Number.isInteger(bits) || bits < 0 || bits > 32) {
			return 'rules.custom.err.cidr';
		}
		// Биты вне маски: адрес должен быть первым в подсети. Демон отвечает
		// тем же и подсказывает исправленный, здесь достаточно назвать беду.
		const n = ((a << 24) | (b << 16) | (c << 8) | d) >>> 0;
		const mask = bits === 0 ? 0 : (0xffffffff << (32 - bits)) >>> 0;
		if ((n & mask) >>> 0 !== n) return 'rules.custom.err.bits';
		return null;
	}
	if (!V6ISH.test(addr) || !addr.includes(':') || !Number.isInteger(bits) || bits < 0 || bits > 128) {
		return 'rules.custom.err.cidr';
	}
	// Каноничность IPv6 и его биты вне маски сверяет демон: считать
	// 128-битную маску ради подсказки в поле — не та цена.
	return null;
}

function domainProblem(v: string): Key | null {
	if (V4.test(v) || (v.includes(':') && V6ISH.test(v))) return 'rules.custom.err.ip';
	if (v.includes('/') || v.includes(':')) return 'rules.custom.err.scheme';
	for (const c of v) if (c > '') return 'rules.custom.err.idn';
	if (v.endsWith('.') || !/^[a-z0-9.-]+$/.test(v)) return 'rules.custom.err.domain';
	for (const label of v.split('.')) {
		if (label === '' || label.length > 63 || label.startsWith('-') || label.endsWith('-')) {
			return 'rules.custom.err.domain';
		}
	}
	return v.length > 253 ? 'rules.custom.err.domain' : null;
}

/**
 * «3 набора» по-русски, «3 sets» по-английски.
 *
 * Числительное собирается выбором ключа, а не окончанием: общего правила
 * множественного числа в словаре нет намеренно (i18n/index.ts), и одно
 * исключение ради одной строки завело бы второй механизм подстановки.
 *
 * Язык обязателен: русское правило, применённое к английскому, дало бы
 * «21 set» рядом с «5 sets» — форма второго ключа там просто другая.
 */
export function countSets(n: number, t: T, lang: Lang): string {
	if (lang !== 'ru') return t(n === 1 ? 'rules.count.one' : 'rules.count.many', { n });
	const ten = n % 10;
	const hundred = n % 100;
	if (ten === 1 && hundred !== 11) return t('rules.count.one', { n });
	if (ten >= 2 && ten <= 4 && (hundred < 12 || hundred > 14)) return t('rules.count.few', { n });
	return t('rules.count.many', { n });
}

/** Подпись «В туннель · 3 набора» — она же уходит в сводку свёрнутого раздела. */
export function onLabel(d: RulesDraft, t: T, lang: Lang): string {
	return t(d.policy === 'except' ? 'rules.on.except' : 'rules.on.only', {
		n: countSets(d.sets.length, t, lang),
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
	const problems = eff.rules.map((r, i) => ruleProblem(r, eff.rules, i));
	const invalid = problems.filter((x) => x !== null).length;

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

	// Правила при политике профиля — то же, что и наборы: первое своё
	// правило означает «маршрут теперь решает панель».
	const applyRules = (rules: CustomRule[]) =>
		p.setDraft({
			...eff,
			policy: eff.policy === 'profile' && rules.length > 0 ? 'only' : eff.policy,
			rules,
		});
	const patchRule = (i: number, patch: Partial<CustomRule>) =>
		applyRules(eff.rules.map((r, j) => (j === i ? { ...r, ...patch } : r)));

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
					<div class="label">{onLabel(eff, t, p.lang)}</div>
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

			{/* Свои правила стоят между чипами и паками: это тоже «что в
			    туннель», только словами владельца, а не именем набора. Блок
			    виден и при профиле — добавить правило можно всегда, и это
			    переключит политику, как и первый выбранный набор. */}
			<div class="label">{t('rules.custom.label')}</div>
			<p class="hint">{t('rules.custom.hint')}</p>
			{eff.rules.length > 0 ? (
				<div class="rows">
					{eff.rules.map((r, i) => (
						<div key={i} class="rule" data-bad={problems[i] ? '' : undefined}>
							<select
								aria-label={t('rules.custom.kind.label')}
								value={r.kind}
								disabled={off}
								onChange={(e) => patchRule(i, { kind: (e.target as HTMLSelectElement).value as RuleKind })}
							>
								{KINDS.map((k) => (
									<option key={k} value={k}>
										{t(`rules.custom.kind.${k}` as Key)}
									</option>
								))}
							</select>
							<button
								type="button"
								class="mini danger"
								disabled={off}
								title={t('rules.custom.remove')}
								aria-label={`${t('rules.custom.remove')}: ${r.value || i + 1}`}
								onClick={() => applyRules(eff.rules.filter((_, j) => j !== i))}
							>
								✕
							</button>
							<input
								value={r.value}
								placeholder={t(`rules.custom.ph.${r.kind}` as Key)}
								inputMode="url"
								autocapitalize="off"
								autocomplete="off"
								spellcheck={false}
								disabled={off}
								aria-invalid={problems[i] ? true : undefined}
								// К строчным — на глазах, а не молча в демоне: домен
								// регистра не имеет, а отбивать «Example.com» после
								// нажатия было бы придиркой к тому, чего владелец не видел.
								onInput={(e) =>
									patchRule(i, { value: (e.target as HTMLInputElement).value.trim().toLowerCase() })
								}
							/>
							<input
								class="note"
								value={r.comment}
								placeholder={t('rules.custom.ph.comment')}
								maxLength={MAX_COMMENT}
								autocomplete="off"
								disabled={off}
								// Края обрезаются при потере фокуса, а не на каждом
								// вводе: иначе не набрать пробел между словами.
								// Перевод строки из вставки убирается сразу.
								onInput={(e) =>
									patchRule(i, { comment: (e.target as HTMLInputElement).value.replace(/[\r\n]+/g, ' ') })
								}
								onBlur={(e) => {
									const v = (e.target as HTMLInputElement).value.trim();
									if (v !== r.comment) patchRule(i, { comment: v });
								}}
							/>
							<div class="seg">
								{ACTIONS.map((a) => (
									<button
										key={a}
										type="button"
										class="accent"
										aria-pressed={r.action === a}
										disabled={off}
										onClick={() => patchRule(i, { action: a })}
									>
										{t(`rules.custom.action.${a}` as Key)}
									</button>
								))}
							</div>
							{problems[i] ? <p class="hint bad">{t(problems[i] as Key, { n: MAX_RULES })}</p> : null}
						</div>
					))}
				</div>
			) : null}
			{eff.rules.length >= MAX_RULES ? (
				<p class="hint">{t('rules.custom.max', { n: MAX_RULES })}</p>
			) : (
				<button
					type="button"
					class="linkbtn"
					disabled={off}
					onClick={() =>
						applyRules([...eff.rules, { kind: 'suffix', value: '', action: 'tunnel', comment: '' }])
					}
				>
					+ {t('rules.custom.add')}
				</button>
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
								{/* Незнакомый пак покажет ключ «pack.<id>» — так по всей
								    панели поступает пропущенный ключ (i18n/index.ts):
								    пустое место выглядит задуманным и живёт годами,
								    а ключ на кнопке чинится при первом же взгляде.
								    Демон и панель обновляются порознь, и новый пак
								    вполне может приехать раньше перевода. */}
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
					<p>{invalid > 0 ? t('rules.dirty.invalid') : t('rules.dirty.text')}</p>
					<div class="buttons">
						<button
							type="button"
							class="go"
							// Строка с ошибкой гасит кнопку, а не ждёт отказа демона:
							// тот ответит bad_rule и тем же, но уже после нажатия.
							disabled={off || invalid > 0}
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
						disabled={off}
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
								p.onApply({ policy: 'profile', download: 'direct', sets: [], rules: [] });
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
