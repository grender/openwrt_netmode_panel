// Панель netmoded. Preact + htm, без сборки: файл грузится браузером как
// ESM напрямую и зашивается в бинарь через go:embed.
//
// Состояние тянется опросом /api/status раз в секунду — так требует SPEC §7.
// Ни WebSocket, ни SSE вводить нельзя: на роутере они стоят памяти и ничего
// не покупают при одном пользователе в LAN.

import { html, render, useState, useEffect, useRef } from './vendor/htm-preact-standalone.module.js';
import { makeT } from './i18n.js';

const POLL_MS = 1000;

// Бюджеты ожидания. Один общий не годится: скан эфира штатно идёт секунды,
// а опрос статуса обязан уложиться в интервал опроса, иначе запросы копятся.
const T_STATUS = 4000;
const T_SIDE = 8000;
const T_SCAN = 20000;
const T_MODE = 8000; // 202 приходит сразу, ждать завершения джоба тут нечего
const T_DEFAULT = 8000;

// ─────────── утилиты ───────────

// Таймаут обязателен на каждом запросе: зависший (именно зависший, а не
// отклонённый) fetch навсегда оставил бы busy взведённым, а он блокирует все
// кнопки панели до перезагрузки страницы.
//
// AbortController + setTimeout, а не AbortSignal.timeout(): второй новее и
// может отсутствовать в браузере владельца, а полифиллов у панели нет по
// определению — она грузится в браузер как есть, без сборки.
const api = async (path, opts, timeoutMs = T_DEFAULT) => {
	const ctl = new AbortController();
	const timer = setTimeout(() => ctl.abort(), timeoutMs);
	try {
		// signal стоит ПОСЛЕ ...opts намеренно: вызывающий со своим signal
		// иначе перезаписал бы наш, и таймаут молча исчез бы. Таких
		// вызывающих сейчас нет, но порядок здесь значим.
		const r = await fetch(path, {
			headers: { 'Content-Type': 'application/json' },
			...opts,
			signal: ctl.signal,
		});
		const text = await r.text();
		let body = null;
		try { body = text ? JSON.parse(text) : null; } catch { /* не-JSON оставляем как null */ }
		if (!r.ok) {
			const err = new Error((body && body.error) || r.statusText);
			err.status = r.status;
			err.code = body && body.code;
			throw err;
		}
		return body;
	} finally {
		// Снимаем в finally: иначе после быстрого ответа таймер ещё секунды
		// живёт и дёргает abort уже завершённого запроса.
		clearTimeout(timer);
	}
};

const fmtTime = (iso, lang) => {
	if (!iso) return '—';
	const d = new Date(iso);
	if (isNaN(d)) return '—';
	return d.toLocaleString(lang === 'en' ? 'en-GB' : 'ru-RU', {
		day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit',
	});
};

// Пороги задержки — контракт из макета, а не украшение.
const latClass = (ms) => (ms == null ? 'lat-bad' : ms < 55 ? 'lat-ok' : ms < 110 ? 'lat-warn' : 'lat-bad');
const latBg = (ms) => (ms == null ? 'bg-bad' : ms < 55 ? 'bg-ok' : ms < 110 ? 'bg-warn' : 'bg-bad');
// Мёртвый узел — пустая полоска, а не полная. Полная читалась бы как
// «задержка максимальная», то есть узел живой и очень медленный; это
// прямо противоположно тому, что произошло.
const latPct = (ms) => (ms == null ? '0%' : Math.min(100, Math.round((ms / 220) * 100)) + '%');

const signalClass = (dbm) => (dbm >= -60 ? 'lat-ok' : dbm >= -75 ? 'lat-warn' : 'lat-bad');

// ─────────── корневой компонент ───────────

function App() {
	const [lang, setLang] = useState(() => localStorage.getItem('netmode.lang') || 'ru');
	const [status, setStatus] = useState(null);
	const [stale, setStale] = useState(false);
	const [nikki, setNikki] = useState(null);
	const [sets, setSets] = useState(null);
	const [nets, setNets] = useState(null);
	const [scan, setScan] = useState(null);
	const [logs, setLogs] = useState(null);
	const [busy, setBusy] = useState('');
	const [sheet, setSheet] = useState(null);
	const [toast, setToast] = useState(null);
	const tRef = useRef(null);

	const t = makeT(lang);
	useEffect(() => { localStorage.setItem('netmode.lang', lang); document.documentElement.lang = lang; }, [lang]);

	// Опрос статуса. Провал не гасит панель: показываем последнее известное
	// состояние и честно помечаем его устаревшим — пустой экран в момент,
	// когда связь пропала, бесполезен именно тогда, когда нужен больше всего.
	useEffect(() => {
		let alive = true;
		const tick = async () => {
			try {
				const s = await api('/api/status', null, T_STATUS);
				if (!alive) return;
				setStatus(s); setStale(false);
			} catch {
				if (alive) setStale(true);
			}
		};
		tick();
		const id = setInterval(tick, POLL_MS);
		return () => { alive = false; clearInterval(id); };
	}, []);

	// Побочные данные тянем реже: они меняются от действий, а не сами.
	const reloadSide = async () => {
		const grab = (p, set) => api(p, null, T_SIDE).then(set).catch(() => set(null));
		await Promise.all([
			grab('/api/nikki/proxies', setNikki),
			grab('/api/b4/sets', setSets),
			grab('/api/wifi/networks', setNets),
			grab('/api/logs?n=5', setLogs),
		]);
	};
	useEffect(() => { reloadSide(); }, []);

	const flash = (msg, kind = 'err') => {
		setToast({ msg, kind });
		clearTimeout(tRef.current);
		tRef.current = setTimeout(() => setToast(null), 6000);
	};

	const act = async (name, fn) => {
		setBusy(name);
		try { await fn(); await reloadSide(); }
		catch (e) { flash(describe(e, t)); }
		finally { setBusy(''); }
	};

	if (!status) {
		return html`<div class="wrap"><div class="card">${stale
			? html`<div class="note err"><h3>${t('foot.stale')}</h3></div>`
			: '…'}</div></div>`;
	}

	const job = status.job && status.job.state === 'running' ? status.job : null;
	const locked = !!job || !!busy;

	return html`
		<${Top} s=${status} t=${t} lang=${lang} setLang=${setLang} />
		<${Banner} s=${status} t=${t} job=${job} locked=${locked}
			onMode=${(m) => act('mode', () => api('/api/mode', { method: 'POST', body: JSON.stringify({ mode: m }) }, T_MODE))} />

		${toast && html`<div class="wrap"><div class="note ${toast.kind}"><p>${toast.msg}</p></div></div>`}

		<div class="wrap">
			<${SelectionNote} s=${status} t=${t} />

			${status.mode === 'nikki' && html`
				<${NikkiCard} data=${nikki} t=${t} busy=${busy} locked=${locked}
					onPick=${(n) => act('proxy', () => api('/api/nikki/proxy', { method: 'POST', body: JSON.stringify({ name: n }) }))}
					onTest=${() => act('test', () => api('/api/nikki/test', { method: 'POST' }))} />`}

			${status.mode === 'b4' && html`
				<${B4Card} data=${sets} t=${t} busy=${busy} locked=${locked}
					onPick=${(id) => act('set', () => api('/api/b4/set', { method: 'POST', body: JSON.stringify({ id }) }))} />`}

			${status.mode === 'off' && html`
				<div class="card"><h2>${t('mode.off')}</h2>
					<p class="hint">${t('sub.off')}</p></div>`}

			<${WifiCard} nets=${nets} scan=${scan} t=${t} busy=${busy} status=${status}
				onScan=${() => act('scan', async () => setScan(await api('/api/wifi/scan', null, T_SCAN)))}
				onOpenSheet=${(s) => setSheet(s)}
				onDelete=${(n) => {
					if (!confirm(t('wifi.confirm.delete', { ssid: n.ssid }))) return;
					act('del-' + n.id, () => api('/api/wifi/networks/' + encodeURIComponent(n.id), {
						method: 'DELETE',
						headers: { 'If-Match': nets.fingerprint },
					}));
				}} />

			<${SubCard} s=${status} logs=${logs} t=${t} lang=${lang} busy=${busy}
				onUpdate=${() => act('sub', () => api('/api/subscription/update', { method: 'POST' }))} />
		</div>

		${sheet && html`
			<${NetworkSheet} sheet=${sheet} t=${t} busy=${busy}
				onClose=${() => setSheet(null)}
				onSave=${(body, setErr) => act('save', async () => {
					try {
						// Отпечаток обязателен: без него демон отвечает 409.
						// Он же ловит правки, сделанные в LuCI, пока форма
						// была открыта.
						await api('/api/wifi/networks', {
							method: 'POST',
							headers: { 'If-Match': nets && nets.fingerprint },
							body: JSON.stringify(body),
						});
						setSheet(null);
					} catch (e) {
						// Расхождение отпечатка чинится обновлением списка,
						// а не повтором вслепую: иначе перезапишем чужое.
						if (e.code === 'fingerprint_mismatch') {
							await reloadSide();
							setErr(t('wifi.err.stale'));
						} else if (e.code === 'foreign_staged_changes') {
							setErr(t('wifi.err.foreign'));
						} else {
							setErr(describe(e, t));
						}
						throw e;
					}
				})} />`}

		<${Foot} s=${status} t=${t} lang=${lang} stale=${stale} />
	`;
}

// Машинный код ошибки → ключ словаря.
//
// Разбор идёт по коду, а не по статусу: под 503 ходят и подвисший ubus, и
// нехватка Clash API, и незаданный планировщик, и объяснять их одним текстом
// про Clash — врать владельцу ровно там, где он ищет причину.
const ERR_KEY = {
	ambiguous_selection: 'sel.ambiguous.title',
	enabled_network_readonly: 'wifi.locked',
	job_busy: 'err.busy',
	// Сообщение демона русское (оно же уходит в syslog), поэтому даже там,
	// где текст совпадает по смыслу, панель берёт свой перевод.
	nikki_unavailable: 'srv.down',
	b4_unavailable: 'sets.down',
	b4_partial: 'err.b4.partial',
	ubus_unavailable: 'err.ubus',
	uci_unavailable: 'err.uci',
	scan_failed: 'err.scan',
	ifname_unknown: 'err.ifname',
	radio_unknown: 'err.radio',
	unavailable: 'err.sched',
};

// Сообщение об ошибке объясняет причину, а не показывает код: коды 409
// в этой панели означают три разные вещи, и «409» пользователю не говорит
// ничего.
function describe(e, t) {
	// Прерванный по таймауту запрос даёт DOMException с name AbortError и
	// сообщением от браузера («The user aborted a request») — оно и неверно
	// по сути, и не переводится.
	if (e.name === 'AbortError') return t('err.timeout');
	// typeof, а не просто истинность: код приходит с сервера, и попадание
	// вроде 'constructor' достало бы из прототипа функцию вместо ключа.
	const key = e.code && ERR_KEY[e.code];
	if (typeof key === 'string') return t(key);
	if (e.status === 501) return t('wifi.switch.soon');
	// Запасной вариант для 503 с незнакомым кодом — нейтральный: конкретика
	// здесь была бы догадкой.
	if (e.status === 503) return t('err.unavailable');
	return e.message || t('err.generic');
}

// Метка операции строится на клиенте по kind и arg. Поле label с демона
// русское намеренно (syslog и диагностика по ssh), и в английском
// интерфейсе оно читалось бы как утечка бэкенда.
function jobText(job, t) {
	if (job.kind === 'mode' && ['nikki', 'b4', 'off'].includes(job.arg)) return t('job.mode.' + job.arg);
	if (job.kind === 'subscription') return t('job.subscription');
	return t('job.working');
}

// ─────────── шапка ───────────

function Top({ s, t, lang, setLang }) {
	const ap = s.ap || {};
	const meta = [ap.band && ap.band.toUpperCase(), ap.clients != null && t('ap.clients', { n: ap.clients })]
		.filter(Boolean).join(' · ');
	return html`
		<div class="top">
			<div class="top-id">
				<span class="host">${s.hostname || 'netmoded'}</span>
				${ap.ssid && html`<span class="ap-meta">${t('ap.broadcasts', { ssid: ap.ssid })}${meta ? ' · ' + meta : ''}</span>`}
			</div>
			<div class="top-links">
				<a href=${s.links?.nikki || '#'} target="_blank" rel="noreferrer"
					aria-disabled=${!s.links?.nikki}>Nikki ↗</a>
				<a href=${s.links?.b4 || '#'} target="_blank" rel="noreferrer"
					aria-disabled=${!s.links?.b4}>b4 ↗</a>
				<div class="lang">
					${['ru', 'en'].map((l) => html`
						<button aria-pressed=${lang === l} onClick=${() => setLang(l)}>${l.toUpperCase()}</button>`)}
				</div>
			</div>
		</div>`;
}

// ─────────── баннер ───────────

function Banner({ s, t, job, locked, onMode }) {
	const m = ['nikki', 'b4', 'off'].includes(s.mode) ? s.mode : 'unknown';
	const ssid = s.configured_ssid || s.associated_ssid;

	let sub;
	if (m === 'nikki') {
		// Признак закрепления приходит отдельным полем: имя узла в обоих
		// случаях одно и то же, а состояния разные.
		sub = !s.nikki?.available ? t('sub.nikki.down')
			: s.nikki.pinned ? t('sub.nikki.manual', { node: s.nikki.set })
				: t('sub.nikki.auto', { node: s.nikki.set || '—' });
	} else if (m === 'b4') {
		sub = !s.b4?.available ? t('sub.b4.down') : t('sub.b4', { set: s.b4.set || '—' });
	} else sub = t('sub.' + m);

	// Текущий режим не нажимается: повторное нажатие запускает полный джоб со
	// стопом-стартом служб и рестартом firewall — реальный разрыв связи на
	// 5–15 секунд без всякого результата.
	//
	// Сравнение идёт с сырым s.mode, а не с нормализованным m: при 'unknown'
	// оно не совпадает ни с одной кнопкой, и все три остаются живыми — из
	// нераспознанного состояния владелец обязан иметь выход в любой режим.
	const current = (id) => s.mode === id;

	const online = s.online?.ok;
	const netTip = ssid
		? (online ? t('net.tip.online', { ssid }) : t('net.tip.offline', { ssid }))
		: t('net.nossid');

	const pct = job && job.eta_sec
		? Math.min(99, Math.round(((Date.now() - new Date(job.started_at)) / 1000 / job.eta_sec) * 100))
		: 0;
	const left = job && job.eta_sec
		? Math.max(0, job.eta_sec - Math.round((Date.now() - new Date(job.started_at)) / 1000))
		: null;

	return html`
		<div class="banner m-${m}">
			<div class="headline">
				<h1>${t('title.' + m)}</h1>
				<div class="sub">${sub}</div>
				<div class="chips">
					<span class="chip">↑ ${ssid || t('net.nossid')}</span>
					<span class="chip ${online ? 'ok' : 'bad'}" title=${netTip}>
						${job ? t('net.checking') : online ? t('net.online') : t('net.offline')}
					</span>
					${s.pending_apply && html`<span class="chip" title="uci commit без применения">pending</span>`}
				</div>
			</div>
			<div class="side">
				<div class="modes">
					${['nikki', 'b4', 'off'].map((id) => html`
						<button class="m-${id}" aria-pressed=${s.mode === id} disabled=${locked || current(id)}
							onClick=${() => onMode(id)}>${t('mode.' + id)}</button>`)}
				</div>
				${!job && html`<div class="hint" style="opacity:.8">${locked ? t('mode.hint.busy') : t('mode.hint')}</div>`}
			</div>
			${job && html`
				<div class="job">
					<div class="job-row">
						<span class="blink">${jobText(job, t)}</span>
						<span style="font-family:var(--mono);opacity:.8">
							${left != null ? t('job.left', { sec: left }) : t('job.blocked')}</span>
					</div>
					<div class="bar"><i style="width:${pct}%"></i></div>
				</div>`}
		</div>`;
}

// ─────────── состояние выбора ───────────

function SelectionNote({ s, t }) {
	const st = s.selection_state;
	if (st === 'single') return null;

	if (st === 'ambiguous') {
		return html`
			<div class="note err full">
				<h3>${t('sel.ambiguous.title')}</h3>
				<p>${t('sel.ambiguous.text')}</p>
				<code>${t('sel.ambiguous.cmd', { host: s.hostname || 'router' })}</code>
			</div>`;
	}
	const key = st === 'empty' ? 'sel.empty' : 'sel.alldisabled';
	return html`
		<div class="note warn full">
			<h3>${t(key + '.title')}</h3>
			<p>${t(key + '.text')}</p>
		</div>`;
}

// ─────────── Nikki ───────────

function NikkiCard({ data, t, busy, locked, onPick, onTest }) {
	if (!data || !data.available) {
		return html`<div class="card"><h2>${t('srv.title')}</h2><div class="empty">${t('srv.down')}</div></div>`;
	}
	const members = data.members || [];
	if (!members.length) {
		return html`<div class="card"><h2>${t('srv.title')}</h2><div class="empty">${t('srv.empty')}</div></div>`;
	}
	// pinned приходит от демона отдельным полем: по одному имени узла
	// «движок подобрал» и «закреплено руками» неразличимы, а разница
	// принципиальна — во втором случае автоподбор выключен.
	const pinned = !!data.pinned;
	const active = data.fixed || data.selected;

	return html`
		<div class="card">
			<h2>${t('srv.title')}
				<button class="linkbtn" disabled=${!pinned || locked} onClick=${() => onPick('AUTO')}>
					${pinned ? t('srv.auto.back') : t('srv.auto')}</button>
			</h2>
			${pinned && html`<p class="hint tight" style="color:var(--warn)">${t('srv.pinned.note')}</p>`}
			<div class="rows">
				${members.map((m) => {
					const isActive = active === m.name;
					return html`
					<button class="row ${isActive ? 'sel' : ''} ${m.alive ? '' : 'dim'}"
						disabled=${locked} onClick=${() => onPick(m.name)}>
						<span class="name">${m.name}</span>
						${isActive && html`
							<span class="tag ${pinned ? 'tag-pin' : 'tag-auto'}"
								title=${pinned ? t('srv.tip.pinned') : t('srv.tip.auto')}>
								${pinned ? '📌 ' + t('srv.tag.pinned') : t('srv.tag.auto')}</span>`}
						<span class="meter"><i class=${latBg(m.delay_ms)} style="width:${latPct(m.delay_ms)}"></i></span>
						<span class="ms ${latClass(m.delay_ms)}">${m.delay_ms != null ? m.delay_ms + ' ms' : '—'}</span>
					</button>`;
				})}
			</div>
			<button class="wide" disabled=${locked} onClick=${onTest}>
				${busy === 'test' ? t('srv.measuring') : t('srv.measure')}</button>
		</div>`;
}

// ─────────── b4 ───────────

function B4Card({ data, t, busy, locked, onPick }) {
	if (!data || !data.available) {
		return html`<div class="card"><h2>${t('sets.title')}</h2><div class="empty">${t('sets.down')}</div></div>`;
	}
	return html`
		<div class="card">
			<h2>${t('sets.title')}</h2>
			<div class="sets">
				${(data.sets || []).map((x) => html`
					<button aria-pressed=${x.enabled} disabled=${locked || busy === 'set'}
						onClick=${() => onPick(x.id)}>${x.name}</button>`)}
			</div>
			<p class="hint tight">${t('sets.hint')}</p>
		</div>`;
}

// ─────────── WiFi ───────────

function WifiCard({ nets, scan, t, busy, status, onScan, onOpenSheet, onDelete }) {
	const saved = (nets && nets.networks) || [];
	const found = (scan && scan.networks) || [];
	const savedSsids = new Set(saved.map((n) => n.ssid));
	const extra = found.filter((f) => f.ssid && !savedSsids.has(f.ssid));
	const hidden = found.filter((f) => !f.ssid);

	// Уровень сигнала подтягивается к сохранённой сети по SSID: у секции
	// UCI сигнала нет, он есть только у эфира.
	const sig = new Map(found.filter((f) => f.ssid).map((f) => [f.ssid, f.signal_dbm]));

	// При неоднозначной конфигурации запись запрещена целиком — показываем
	// это заранее, а не отказом после нажатия.
	const frozen = status.selection_state === 'ambiguous';

	return html`
		<div class="card">
			<h2>${t('wifi.title')}
				<div style="display:flex;gap:14px">
					<button class="linkbtn" disabled=${!!busy || frozen}
						onClick=${() => onOpenSheet({ mode: 'add' })}>${t('wifi.add')}</button>
					<button class="linkbtn" disabled=${!!busy} onClick=${onScan}>
						${busy === 'scan' ? t('wifi.scanning') : scan ? t('wifi.rescan') : t('wifi.scan')}</button>
				</div>
			</h2>

			<div class="rows">
				${saved.length === 0 && html`<div class="empty">${t('sel.empty.title')}</div>`}
				${saved.map((n) => html`
					<div class="row ${n.enabled ? 'sel' : ''}">
						<span class="name">${n.ssid || t('wifi.hidden')}</span>
						<span class="tag">${n.enabled ? t('wifi.active') : t('wifi.saved')}</span>
						${sig.has(n.ssid) && html`
							<span class="ms ${signalClass(sig.get(n.ssid))}">${sig.get(n.ssid)} dBm</span>`}
						${n.editable
							? html`
								<button class="mini" title=${t('wifi.edit')} disabled=${!!busy}
									onClick=${() => onOpenSheet({ mode: 'edit', id: n.id, ssid: n.ssid, encryption: n.encryption })}
									>${t('wifi.key')}</button>
								<button class="mini danger icon" title=${t('wifi.delete')} disabled=${!!busy}
									onClick=${() => onDelete(n)}
									>${busy === 'del-' + n.id ? '…' : '✕'}</button>`
							: html`<span class="mark" title=${n.enabled ? t('wifi.locked') : t('sel.ambiguous.title')}>🔒</span>`}
					</div>`)}
			</div>

			${extra.length > 0 && html`
				<div class="rows" style="margin-top:8px">
					${extra.map((f) => html`
						<button class="row" disabled=${!!busy || frozen}
							onClick=${() => onOpenSheet({ mode: 'add', ssid: f.ssid, encryption: f.encryption })}>
							<span class="name">${f.ssid}</span>
							<span class="tag">${f.encryption === 'none' ? t('wifi.none') : f.encryption}</span>
							<span class="ms ${signalClass(f.signal_dbm)}">${f.signal_dbm} dBm</span>
							<span class="mark">＋</span>
						</button>`)}
				</div>`}

			${hidden.length > 0 && html`
				<p class="hint tight">${t('wifi.hidden')} · ${hidden.length} — ${t('wifi.hidden.cant')}</p>`}

			<p class="hint tight">${t('wifi.band')}</p>
			<p class="hint tight">${t('wifi.switch.soon')}</p>
		</div>`;
}

// ─────────── форма сети ───────────

function NetworkSheet({ sheet, t, busy, onSave, onClose }) {
	const editing = sheet.mode === 'edit';
	const [ssid, setSsid] = useState(sheet.ssid || '');
	const [enc, setEnc] = useState(sheet.encryption || 'psk2');
	const [key, setKey] = useState('');
	const [show, setShow] = useState(false);
	const [err, setErr] = useState('');

	const needsKey = enc !== 'none';

	const submit = () => {
		if (!editing && !ssid.trim()) { setErr(t('wifi.err.ssid')); return; }
		// При правке пустой пароль означает «не менять» — так же, как
		// в API: отсутствие поля и пустая строка это разные вещи.
		if (key !== '' && (key.length < 8 || key.length > 63)) { setErr(t('wifi.err.short')); return; }
		if (!editing && needsKey && key === '') { setErr(t('wifi.err.short')); return; }

		const body = {};
		if (editing) body.id = sheet.id;
		else { body.ssid = ssid.trim(); body.encryption = enc; }
		if (key !== '') body.key = key;
		onSave(body, setErr);
	};

	return html`
		<div class="sheet-bg" onClick=${(e) => e.target === e.currentTarget && onClose()}>
			<div class="sheet">
				<h3>${editing ? t('wifi.sheet.edit', { ssid: sheet.ssid }) : t('wifi.sheet.add')}</h3>

				${!editing && html`
					<label class="field">
						<span>${t('wifi.field.ssid')}</span>
						<input value=${ssid} onInput=${(e) => { setSsid(e.target.value); setErr(''); }}
							autocomplete="off" spellcheck="false" />
					</label>
					<label class="field">
						<span>${t('wifi.field.enc')}</span>
						<select value=${enc} onChange=${(e) => setEnc(e.target.value)}>
							${['psk2', 'psk-mixed', 'sae', 'sae-mixed', 'psk'].map((v) =>
								html`<option value=${v} selected=${v === enc}>${v}</option>`)}
							<option value="none" selected=${enc === 'none'}>${t('wifi.enc.open')}</option>
						</select>
					</label>`}

				${needsKey && html`
					<label class="field">
						<span>${t('wifi.field.key')}</span>
						<div style="display:flex;gap:8px">
							<input type=${show ? 'text' : 'password'} value=${key}
								onInput=${(e) => { setKey(e.target.value); setErr(''); }}
								autocomplete="new-password" spellcheck="false" style="flex:1;min-width:0" />
							<button class="mini" onClick=${() => setShow(!show)}>
								${show ? t('wifi.hide') : t('wifi.show')}</button>
						</div>
					</label>
					${editing && html`<p class="hint tight">${t('wifi.keykept')}</p>`}`}

				${err && html`<p class="hint tight" style="color:var(--bad)">${err}</p>`}
				<p class="hint tight">${t('wifi.keyhint')}</p>

				<div class="sheet-actions">
					<button class="wide primary" disabled=${!!busy} onClick=${submit}>
						${busy === 'save' ? t('wifi.saving') : t('wifi.save')}</button>
					<button class="wide" disabled=${!!busy} onClick=${onClose}>${t('wifi.cancel')}</button>
				</div>
			</div>
		</div>`;
}

// ─────────── подписка ───────────

function SubCard({ s, logs, t, lang, busy, onUpdate }) {
	const sub = s.subscription || {};
	return html`
		<div class="card">
			<h2>${t('sub.title')}</h2>
			<div class="hint" style="color:var(--fg-2);font-size:14px">
				${t('sub.when', { when: sub.last_update ? fmtTime(sub.last_update, lang) : t('sub.never') })}</div>
			<div class="hint">${t('sub.nodes', { n: sub.nodes ?? 0 })}</div>
			${sub.status === 'fail' && sub.error && html`<div class="hint" style="color:var(--bad)">${sub.error}</div>`}

			<button class="wide" disabled=${!!busy} onClick=${onUpdate}>
				${busy === 'sub' ? t('sub.updating') : t('sub.update')}</button>

			<div class="log">
				${(!logs || !logs.lines || !logs.lines.length) && html`<div>${t('sub.emptylog')}</div>`}
				${logs && logs.lines && logs.lines.map((l) => html`
					<div>
						<span style="color:${l.status === 'ok' ? 'var(--ok)' : 'var(--bad)'}">
							${l.status === 'ok' ? '✓' : '✕'}</span>
						<span class="when">${fmtTime(l.ts, lang)}</span>
						<span>${l.status === 'ok' ? l.nodes : (l.err || '—')}</span>
					</div>`)}
			</div>
		</div>`;
}

// ─────────── подвал ───────────

function Foot({ s, t, lang, stale }) {
	return html`
		<div class="foot">
			<span class=${stale ? 'stale' : ''}>
				${stale ? t('foot.stale') : t('foot.updated', { when: fmtTime(s.generated_at, lang) })}</span>
			<span>${t('foot.phase')}</span>
		</div>`;
}

render(html`<${App} />`, document.getElementById('app'));
