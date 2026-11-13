import { useEffect, useState } from 'preact/hooks';
import type {
	CustomRule,
	RuleKind,
	RulesDraft,
	RulesetsCatalog,
	RulesetsResponse,
	SetAction,
	Target,
	Job,
} from '../api/types';
import type { Key, Lang, T } from '../i18n';
import { jobOn } from '../state/job';
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
/** Кнопки сегмента. Третья цель — exit — выбирается списком групп рядом. */
const ACTIONS: SetAction[] = ['tunnel', 'direct'];

/** Цель одной строкой: «tunnel», «direct», «@EXIT-RESI» — как в файле. */
const targetTok = (x: Target) => (x.action === 'exit' ? `@${x.group ?? ''}` : x.action);

/** Цель для сравнения и записи: группа есть только у exit. */
export function targetOf(x: Target): Target {
	return x.action === 'exit' ? { action: 'exit', group: x.group ?? '' } : { action: x.action };
}

/** Подпись цели в чипе: «→ туннель», «→ напрямую», «→ EXIT-RESI». */
export function targetLabel(x: Target, t: T): string {
	return x.action === 'exit' ? `→ ${x.group ?? ''}` : t(`rules.dir.${x.action}` as Key);
}

/** Сдвиг элемента списка на шаг вверх (-1) или вниз (+1). */
export function moved<V>(list: V[], i: number, d: -1 | 1): V[] {
	const j = i + d;
	if (j < 0 || j >= list.length) return list;
	const out = [...list];
	[out[i], out[j]] = [out[j] as V, out[i] as V];
	return out;
}

/**
 * Три шага в порядке проверки mihomo: свои правила → наборы → остальной
 * трафик. Номера — не украшение: у mihomo побеждает первое совпадение, и
 * порядок шагов и есть порядок, в котором решается судьба соединения.
 */
type Step = 'custom' | 'packs' | 'rest';
const STEPS: Step[] = ['custom', 'packs', 'rest'];

export interface RulesetsProps {
	/** Что применено. Три значения побочного списка, и они разные. */
	applied: Side<RulesetsResponse>;
	/** Имена с GitHub. Грузится лениво: 60 КБ ради шага, куда заходят редко. */
	catalog: Side<RulesetsCatalog>;
	/** Каталог грузится: после отказа значение null, и без флага «Повторить» молчала бы. */
	catalogLoading: boolean;
	/** null означает «совпадает с применённым», а не «пусто». */
	draft: RulesDraft | null;
	setDraft(d: RulesDraft | null): void;
	/** Нужен ЧИСЛИТЕЛЬНОМУ: у русского три формы, у английского две. */
	lang: Lang;
	lock: Lock;
	locked: boolean;
	/** Идущий джоб: кнопка держит кольцо до конца операции, а не до 202. */
	running: Job | null;
	t: T;
	onApply(d: RulesDraft): void;
	onLoadCatalog(): void;
}

/**
 * Политика после правки черновика.
 *
 * Первый выбранный набор или своё правило при политике профиля означают
 * «маршрут теперь решает панель». Общая функция, а не копия в каждом месте
 * правки: наблюдатель добавлял правило мимо неё, черновик уходил с
 * policy:profile и правилами сразу, и демон отбивал его bad_rule — на
 * свежей установке «Применить» из наблюдателя не работало никогда.
 */
export function policyWith(policy: RulesDraft['policy'], any: boolean): RulesDraft['policy'] {
	return policy === 'profile' && any ? 'direct' : policy;
}

/**
 * Черновик из применённого.
 *
 * Отдельная функция, потому что «ничего не меняли» выражено null-ом:
 * дальше по разделу считать надо один объект, а не разбирать два случая
 * в каждой строке. Сводка полки на главной строится из него же.
 */
export function draftOf(a: Side<RulesetsResponse>): RulesDraft {
	return {
		policy: a?.policy ?? 'profile',
		download: a?.download ?? 'direct',
		sets: (a?.sets ?? []).map((s) => ({ name: s.name, ...targetOf(s) })),
		// Копии, а не те же объекты: строки правил правятся на месте, и
		// править применённое значило бы менять то, с чем сверяется черновик.
		rules: (a?.rules ?? []).map((r) => ({ ...r })),
	};
}

/** Правило одной строкой — для сверки черновика с применённым. Комментарий
 *  тоже в счёт: сменился текст — файл переписывается. */
const ruleTok = (r: CustomRule) => `${r.kind}:${r.value}>${targetTok(r)}|${r.comment}`;
/** Набор одной строкой: смена цели — такое же изменение, как снятие. */
const setTok = (s: { name: string } & Target) => `${s.name}>${targetTok(s)}`;

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
	const wasS = base.sets.map(setTok);
	const nowS = d.sets.map(setTok);
	const was = new Set(wasS);
	const now = new Set(nowS);
	let setDiff = 0;
	for (const s of now) if (!was.has(s)) setDiff++;
	for (const s of was) if (!now.has(s)) setDiff++;
	n += setDiff;
	// Перестановка при том же составе — тоже правка, как у своих правил:
	// у mihomo побеждает первое совпадение, и порядок наборов решает, куда
	// идёт домен из двух наборов сразу (google включает youtube).
	if (setDiff === 0 && wasS.join('\n') !== nowS.join('\n')) n++;

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
 * Числительное по трём формам русского и двум английского.
 *
 * Числительное собирается выбором ключа, а не окончанием: общего правила
 * множественного числа в словаре нет намеренно (i18n/index.ts), и одно
 * исключение ради одной строки завело бы второй механизм подстановки.
 * Язык обязателен: русское правило, применённое к английскому, дало бы
 * «21 rule» рядом с «5 rules» — форма второго ключа там просто другая.
 */
export function plural(n: number, lang: Lang, one: Key, few: Key, many: Key, t: T): string {
	if (lang !== 'ru') return t(n === 1 ? one : many, { n });
	const ten = n % 10;
	const hundred = n % 100;
	if (ten === 1 && hundred !== 11) return t(one, { n });
	if (ten >= 2 && ten <= 4 && (hundred < 12 || hundred > 14)) return t(few, { n });
	return t(many, { n });
}

/**
 * «в туннель: 2 · напрямую: 1» — сводка наборов называет ОБА направления:
 * одно число «3 набора» скрывало бы, что часть из них выключена из туннеля.
 */
export function setsSummary(sets: RulesDraft['sets'], t: T): string {
	if (sets.length === 0) return t('rules.sum.sets.none');
	const tun = sets.filter((s) => s.action === 'tunnel').length;
	const dir = sets.filter((s) => s.action === 'direct').length;
	const parts: string[] = [];
	if (tun) parts.push(t('rules.sum.tunnel', { n: tun }));
	if (dir) parts.push(t('rules.sum.direct', { n: dir }));
	// Каждая группа-выход своим числом: «EXIT-RESI: 3» говорит больше, чем
	// «в выходы: 4» — выходы разные, и смысл у них разный.
	const byGroup = new Map<string, number>();
	for (const s of sets) if (s.action === 'exit') byGroup.set(s.group ?? '', (byGroup.get(s.group ?? '') ?? 0) + 1);
	for (const [g, k] of byGroup) parts.push(t('rules.sum.exit', { g, n: k }));
	return parts.join(' · ');
}

/**
 * Сводка раздела «Что в туннель» — она же строка полки на главной и
 * заголовок свёрнутой карточки. Обязана называть СОСТОЯНИЕ, а не раздел:
 * имя без цифр заставляет заходить внутрь, чтобы узнать, надо ли было.
 */
export function rulesSummary(a: Side<RulesetsResponse>, d: RulesDraft | null, t: T): string {
	// Пока файл не прочитан, сводка — многоточие, как у проброса. Черновик
	// из undefined дал бы «по профилю», и полка утверждала бы это как факт.
	if (a === undefined && !d) return '…';
	const eff = d ?? draftOf(a);
	if (eff.policy === 'profile') return t('rules.sum.profile');
	let s = `${setsSummary(eff.sets, t)} · ${t(`rules.rest.sum.${eff.policy}` as Key)}`;
	if (eff.rules.length > 0) s += ` · ${t('rules.sum.custom', { n: eff.rules.length })}`;
	if (dirtyCount(a, d) > 0) s += ` · ${t('rules.sum.dirty')}`;
	return s;
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
	for (const c of v) if (c > '\u007f') return 'rules.custom.err.idn';
	if (v.endsWith('.') || !/^[a-z0-9.-]+$/.test(v)) return 'rules.custom.err.domain';
	for (const label of v.split('.')) {
		if (label === '' || label.length > 63 || label.startsWith('-') || label.endsWith('-')) {
			return 'rules.custom.err.domain';
		}
	}
	return v.length > 253 ? 'rules.custom.err.domain' : null;
}

/**
 * Что идёт в туннель.
 *
 * Раздел — чистое представление: состояние (черновик) и запись живут в
 * App. Здесь только выбор того, ЧТО показать, и он повторяет порядок
 * проверки mihomo: свои правила → наборы → остальной трафик. Открыт один
 * шаг из трёх — два развёрнутых снова дали бы страницу в два экрана.
 */
export function Rulesets(p: RulesetsProps) {
	const { applied, catalog, t, lang } = p;
	const [query, setQuery] = useState('');
	const [ask, setAsk] = useState(false);
	const [step, setStep] = useState<Step>('custom');
	// Режим перестановки: строки со стрелками вместо чипов. Отдельный режим,
	// а не стрелки всегда: порядок правят редко, а лишние кнопки в каждой
	// строке мешали бы главному — выбору.
	const [ordering, setOrdering] = useState(false);

	// Каталог нужен только шагу с наборами — и тянется, когда его открыли.
	useEffect(() => {
		if (step === 'packs') p.onLoadCatalog();
		// Только шаг: запрос — реакция на ЗАХОД во вкладку. Зависимость от
		// колбэка перезапрашивала бы каталог при каждой его смене, в том
		// числе сразу после отказа, — повтор без нажатия «Повторить».
	}, [step]);

	if (applied === undefined) return <Skel n={3} />;
	// null — спросили и отказали. Это НЕ «наборов нет»: файл на диске жив,
	// и предлагать выбирать наборы поверх непрочитанного значило бы
	// предложить затереть неизвестно что.
	if (applied === null) return <div class="empty">{t('rules.applied.down')}</div>;

	const eff = p.draft ?? draftOf(applied);
	const dirty = dirtyCount(applied, p.draft);
	const busy = p.lock.on('rulesets') || jobOn(p.running, 'rulesets');
	const problems = eff.rules.map((r, i) => ruleProblem(r, eff.rules, i));
	const invalid = problems.filter((x) => x !== null).length;
	const profile = eff.policy === 'profile';

	// Чужой файл замораживает раздел целиком, а не одну кнопку: демон
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
	const cur = (name: string) => eff.sets.find((s) => s.name === name);

	// Первый выбранный набор или своё правило при политике профиля означают
	// «маршрут теперь решает панель»: без переключения нажатие меняло бы
	// только строку, а трафик шёл бы по-прежнему весь в туннель.
	const leaveProfile = (any: boolean) => policyWith(eff.policy, any);

	const applySets = (sets: RulesDraft['sets']) =>
		p.setDraft({ ...eff, policy: leaveProfile(sets.length > 0), sets });

	// По умолчанию набор ведёт туда, куда НЕ идёт остальное: набор выбирают
	// всегда ради исключения из общего правила.
	const defaultAction = (): SetAction => (eff.policy === 'tunnel' ? 'direct' : 'tunnel');

	const setAction = (name: string, target: Target | null) => {
		if (target === null) return applySets(eff.sets.filter((s) => s.name !== name));
		const next = { name, ...targetOf(target) };
		applySets(cur(name) ? eff.sets.map((s) => (s.name === name ? next : s)) : [...eff.sets, next]);
	};

	// Группы, которые можно предложить в цель: что видит движок, плюс то, что
	// уже применено или выбрано в черновике. Второе нужно, когда движок
	// молчит: иначе строка с применённой группой показывала бы пустой выбор,
	// и владелец решил бы, что цель потерялась.
	const exitNames = [
		...new Set([
			...(applied.exit_groups ?? []).map((g) => g.name),
			...[...applied.sets, ...applied.rules, ...eff.sets, ...eff.rules]
				.filter((x) => x.action === 'exit' && x.group)
				.map((x) => x.group as string),
		]),
	].sort();
	const exitNow = new Map((applied.exit_groups ?? []).map((g) => [g.name, g.now]));

	const togglePack = (names: string[]) => {
		// Пак, выбранный целиком, снимается целиком: иначе повторное нажатие
		// не делало бы ничего, и кнопка выглядела бы сломанной.
		const missing = names.filter((n) => !cur(n));
		const act = defaultAction();
		applySets(
			missing.length
				? [...eff.sets, ...missing.map((name) => ({ name, action: act }))]
				: eff.sets.filter((s) => !names.includes(s.name)),
		);
	};

	const applyRules = (rules: CustomRule[]) =>
		p.setDraft({ ...eff, policy: leaveProfile(rules.length > 0), rules });
	const patchRule = (i: number, patch: Partial<CustomRule>) =>
		applyRules(eff.rules.map((r, j) => (j === i ? { ...r, ...patch } : r)));
	// Смена цели правила — целиком: группа от прежнего exit не должна
	// пережить переключение на «в туннель».
	const targetRule = (i: number, target: Target) =>
		applyRules(
			eff.rules.map((r, j) => {
				if (j !== i) return r;
				const { group: _old, ...rest } = r;
				return { ...rest, ...targetOf(target) };
			}),
		);

	const q = query.trim().toLowerCase();
	const packNames = (catalog?.packs ?? []).flatMap((k) => k.sets);
	const names = eff.sets.map((s) => s.name);
	// Без запроса показываются выбранное и частое, с запросом — совпадения.
	// Тысяча девятьсот имён списком не читаются ни на каком экране.
	const found = q
		? (catalog?.names ?? []).filter((n) => n.toLowerCase().includes(q))
		: [...new Set([...names, ...packNames])];

	// Сводки шагов: каждая называет состояние своего шага, а не его имя.
	const customSum =
		eff.rules.length === 0
			? t('rules.step.custom.none')
			: plural(eff.rules.length, lang, 'rules.custom.count.one', 'rules.custom.count.few', 'rules.custom.count.many', t) +
				(invalid > 0 ? ` · ${t('rules.step.custom.bad', { n: invalid })}` : '');
	const packsSum = profile ? t('rules.step.packs.profile') : setsSummary(eff.sets, t);
	const restSum = profile ? t('rules.step.rest.profile') : t(`rules.rest.sum.${eff.policy}` as Key);
	const sums: Record<Step, string> = { custom: customSum, packs: packsSum, rest: restSum };

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

			{/* Профиль — это не «пусто», а другой хозяин маршрута. */}
			{profile ? <div class="summary">{t('rules.profile.note')}</div> : null}

			<div class="label">{t('rules.order.label')}</div>
			<p class="hint">{t('rules.order.text')}</p>

			<div class="steps" role="tablist" aria-label={t('rules.order.label')}>
				{STEPS.map((k, i) => (
					<button
						key={k}
						type="button"
						role="tab"
						id={`step-${k}`}
						aria-selected={step === k}
						aria-controls="step-panel"
						class={`step${k === 'custom' && invalid > 0 ? ' step-bad' : ''}`}
						onClick={() => setStep(k)}
					>
						<span class="step-head">
							<i>{i + 1}</i>
							{t(`rules.step.${k}` as Key)}
							<span class="chev" aria-hidden="true">
								{step === k ? '⌃' : '⌄'}
							</span>
						</span>
						<span class="step-sum">{sums[k]}</span>
					</button>
				))}
			</div>

			<div id="step-panel" class="steppanel" role="tabpanel" aria-labelledby={`step-${step}`}>
				{step === 'custom' ? (
					<>
						<p class="hint">{t('rules.custom.hint')}</p>
						{/* Вход в наблюдатель стоит здесь, а не только на полке главной:
						    трудно не добавить правило, а понять, КАКОЕ именно нужно, — и
						    вопрос этот возникает ровно тут. */}
						<a class="linkbtn" href="#watch">
							{t('rules.custom.peek')}
						</a>
						{eff.rules.length > 1 ? (
							<OrderToggle on={ordering} off={off} t={t} onToggle={() => setOrdering(!ordering)} />
						) : null}
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
													onClick={() => targetRule(i, { action: a })}
												>
													{t(`rules.custom.action.${a}` as Key)}
												</button>
											))}
											<ExitPick value={r} groups={exitNames} off={off} t={t} onPick={(x) => targetRule(i, x)} />
										</div>
										{ordering ? (
											<div class="ordacts">
												<span>{t('rules.order.pos', { n: i + 1 })}</span>
												<button
													type="button"
													class="mini"
													disabled={off || i === 0}
													aria-label={`${t('rules.order.up')}: ${r.value || i + 1}`}
													onClick={() => applyRules(moved(eff.rules, i, -1))}
												>
													↑
												</button>
												<button
													type="button"
													class="mini"
													disabled={off || i === eff.rules.length - 1}
													aria-label={`${t('rules.order.down')}: ${r.value || i + 1}`}
													onClick={() => applyRules(moved(eff.rules, i, 1))}
												>
													↓
												</button>
											</div>
										) : null}
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
					</>
				) : null}

				{step === 'packs' ? (
					<>
						{/* Подпись и чипы рисуются только там, где панель решает
						    маршрут: при профиле «наборов нет» было бы ложью — туда
						    идёт весь трафик, просто не по нашим правилам. */}
						{profile ? null : (
							<>
								<div class="label">{`${setsSummary(eff.sets, t)} · ${t(`rules.rest.sum.${eff.policy}` as Key)}`}</div>
								{eff.sets.length > 1 ? (
									<OrderToggle on={ordering} off={off} t={t} onToggle={() => setOrdering(!ordering)} />
								) : null}
								{eff.sets.length === 0 ? (
									<div class="empty">{t(`rules.none.${eff.policy}` as Key)}</div>
								) : ordering ? (
									<>
										<p class="hint">{t('rules.order.sets')}</p>
										<ol class="orderlist">
											{eff.sets.map((s, i) => (
												<li key={s.name} class={missed(s.name) ? 'bad' : undefined}>
													<i class="pos">{i + 1}</i>
													<span class="name">{s.name}</span>
													<i class={`dir dir-${s.action}`}>{targetLabel(s, t)}</i>
													<button
														type="button"
														class="mini"
														disabled={off || i === 0}
														aria-label={`${t('rules.order.up')}: ${s.name}`}
														onClick={() => applySets(moved(eff.sets, i, -1))}
													>
														↑
													</button>
													<button
														type="button"
														class="mini"
														disabled={off || i === eff.sets.length - 1}
														aria-label={`${t('rules.order.down')}: ${s.name}`}
														onClick={() => applySets(moved(eff.sets, i, 1))}
													>
														↓
													</button>
												</li>
											))}
										</ol>
									</>
								) : (
									<div class="chips">
										{eff.sets.map((s) => (
											<span key={s.name} class={`chip${missed(s.name) ? ' chip-bad' : ''}`}>
												<span>{s.name}</span>
												<i class={`dir dir-${s.action}`} title={s.action === 'exit' ? exitNow.get(s.group ?? '') : undefined}>
													{targetLabel(s, t)}
												</i>
												{hasIP(s.name) ? <i class="ip">{t('rules.chip.ip')}</i> : null}
												<button
													type="button"
													disabled={off}
													title={t('rules.chip.off')}
													aria-label={`${t('rules.chip.off')}: ${s.name}`}
													onClick={() => setAction(s.name, null)}
												>
													✕
												</button>
											</span>
										))}
									</div>
								)}
							</>
						)}

						{profile ? null : applied.exit_groups === null ? (
							<p class="hint">{t('rules.exit.down')}</p>
						) : exitNames.length === 0 ? (
							<p class="hint">{t('rules.exit.none')}</p>
						) : null}

						{catalog?.packs?.length ? (
							<>
								<div class="label">{t('rules.packs')}</div>
								<div class="pills dashed">
									{catalog.packs.map((k) => (
										<button
											key={k.id}
											type="button"
											aria-pressed={k.sets.every((n) => !!cur(n))}
											disabled={off}
											onClick={() => togglePack(k.sets)}
										>
											{/* Незнакомый пак покажет ключ «pack.<id>» — так по всей
											    панели поступает пропущенный ключ (i18n/index.ts):
											    демон новее панели, и это не отказ. */}
											{t(`pack.${k.id}` as Key)}
										</button>
									))}
								</div>
							</>
						) : null}

						<label class="field">
							<span class="sr-only">{t('rules.search.placeholder')}</span>
							<input
								value={query}
								placeholder={t('rules.search.placeholder')}
								autocomplete="off"
								spellcheck={false}
								onInput={(e) => setQuery((e.target as HTMLInputElement).value)}
							/>
						</label>

						{catalog === undefined ? (
							<Skel n={3} />
						) : catalog === null ? (
							<>
								<p class="hint">{t('rules.catalog.down', { why: t('rules.catalog.why') })}</p>
								<button
									type="button"
									class="linkbtn"
									disabled={p.catalogLoading}
									aria-busy={p.catalogLoading}
									onClick={p.onLoadCatalog}
								>
									{p.catalogLoading ? <Spin /> : null} {t('rules.catalog.retry')}
								</button>
								{/* Каталога нет, но применённое показать можно: оно из файла. */}
								{!q && names.length > 0 ? (
									<div class="rows">
										{names.map((n) => (
											<Row key={n} name={n} draft={cur(n)} applied={was.get(n)} groups={exitNames} off={off} t={t} onSet={(a) => setAction(n, a)} />
										))}
									</div>
								) : null}
							</>
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
										<Row key={n} name={n} draft={cur(n)} applied={was.get(n)} groups={exitNames} off={off} t={t} onSet={(a) => setAction(n, a)} />
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

						<div class="label">{t('rules.download.label')}</div>
						<div class="seg">
							{(['direct', 'tunnel'] as const).map((k) => (
								<button
									key={k}
									type="button"
									aria-pressed={eff.download === k}
									// При профиле выбор скачивания не значит ничего: правил
									// панели в файле нет, качать нечего.
									disabled={off || profile}
									onClick={() => p.setDraft({ ...eff, download: k })}
								>
									{t(`rules.download.${k}` as Key)}
								</button>
							))}
						</div>
						<p class="hint">{t('rules.download.hint')}</p>
						<p class="hint">{t('rules.source')}</p>
					</>
				) : null}

				{step === 'rest' ? (
					<>
						<p class="hint">{t('rules.rest.text')}</p>
						<div class="seg">
							{(['direct', 'tunnel'] as const).map((k) => (
								<button
									key={k}
									type="button"
									class="accent"
									aria-pressed={eff.policy === k}
									disabled={off}
									// Политика — такое же изменение конфигурации, как список:
									// попадает в черновик и применяется тем же джобом.
									// Инверсия маршрутизации, объявленная без применения, —
									// самая дорогая ложь в этой панели.
									onClick={() => p.setDraft({ ...eff, policy: k })}
								>
									{t(`rules.rest.${k}` as Key)}
								</button>
							))}
						</div>
						{profile ? null : <p class="hint">{t(`rules.rest.hint.${eff.policy}` as Key)}</p>}

						{/* Возврат профилю стоит последним и открывается подтверждением:
						    он стирает весь выбор и перезапускает движок, а откатов
						    в проекте нет (ADR-0006). Пока правил панели и так нет,
						    кнопки нет вовсе — нажимать было бы не на что. */}
						{applied.policy === 'profile' ? null : (
							<>
								<div class="label">{t('rules.rest.profile.label')}</div>
								<p class="hint">{t('rules.rest.profile.text')}</p>
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
				) : null}
			</div>

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
		</>
	);
}

/**
 * Строка каталога с трёхсегментным выбором: в туннель / напрямую / не
 * выбран. Тегов может быть два смысла, и это не многословие: «не
 * применено» говорит про черновик, «не загрузился» — про то, что уже лежит
 * в файле.
 */
function Row({
	name,
	draft,
	applied,
	groups,
	off,
	t,
	onSet,
}: {
	name: string;
	draft: ({ name: string } & Target) | undefined;
	applied: RulesetsResponse['sets'][number] | undefined;
	groups: string[];
	off: boolean;
	t: T;
	onSet(a: Target | null): void;
}) {
	const act = draft?.action ?? null;
	const changed = (draft ? targetTok(draft) : '-') !== (applied ? targetTok(applied) : '-');
	const missed = applied?.loaded === false;
	return (
		<div class={`row set${act ? ' sel' : ''}${act === 'direct' ? ' sel-direct' : ''}`}>
			<span class="name">{name}</span>
			<span class="id">geosite:{name}</span>
			<div class="setacts">
				<div class="seg small">
					{ACTIONS.map((a) => (
						<button
							key={a}
							type="button"
							class="accent"
							aria-pressed={act === a}
							disabled={off}
							onClick={() => onSet({ action: a })}
						>
							{t(`rules.row.${a}` as Key)}
						</button>
					))}
					<ExitPick value={draft} groups={groups} off={off} t={t} onPick={onSet} />
					<button type="button" aria-pressed={act === null} disabled={off} onClick={() => onSet(null)}>
						{t('rules.row.none')}
					</button>
				</div>
				{changed ? (
					<span class="tag tag-pin">{t(act ? 'rules.tag.changed' : 'rules.tag.removed')}</span>
				) : missed ? (
					<span class="tag tag-bad">{t('rules.tag.notloaded')}</span>
				) : null}
			</div>
		</div>
	);
}

/**
 * Третья цель — группа-выход профиля (ADR-0049) — списком внутри сегмента.
 * Список, а не кнопки: групп может быть сколько угодно, и сегмент на
 * телефоне разъехался бы на третьей. Нет групп и цель не exit — нет и
 * списка: предлагать пустой выбор незачем.
 */
function ExitPick({
	value,
	groups,
	off,
	t,
	onPick,
}: {
	value: Target | undefined;
	groups: string[];
	off: boolean;
	t: T;
	onPick(x: Target): void;
}) {
	const on = value?.action === 'exit';
	if (groups.length === 0 && !on) return null;
	return (
		<label class={`exitpick${on ? ' on' : ''}`}>
			<span class="sr-only">{t('rules.exit.label')}</span>
			<select
				value={on ? (value?.group ?? '') : ''}
				disabled={off}
				onChange={(e) => {
					const g = (e.target as HTMLSelectElement).value;
					if (g) onPick({ action: 'exit', group: g });
				}}
			>
				<option value="" disabled>
					{t('rules.exit.pick')}
				</option>
				{groups.map((g) => (
					<option key={g} value={g}>
						{g}
					</option>
				))}
			</select>
		</label>
	);
}

/** Вход в режим перестановки и выход из него. */
function OrderToggle({ on, off, t, onToggle }: { on: boolean; off: boolean; t: T; onToggle(): void }) {
	return (
		<button type="button" class="linkbtn" aria-pressed={on} disabled={off} onClick={onToggle}>
			{t(on ? 'rules.order.done' : 'rules.order.edit')}
		</button>
	);
}
