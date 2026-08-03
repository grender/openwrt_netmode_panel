// Словари интерфейса. Русский — исходный: строки взяты из макета дословно,
// потому что тихий дрейф копирайта нечем отревьюить. Английский — перевод.
//
// Ключ без перевода отдаётся как есть (см. t() ниже), а не превращается в
// пустоту: недостающая строка должна быть заметна, а не невидима.

export const DICT = {
	ru: {
		'mode.nikki': 'Nikki',
		'mode.b4': 'b4',
		'mode.off': 'Выключено',
		'mode.unknown': 'Неизвестно',

		'title.nikki': 'Nikki включён',
		'title.b4': 'b4 включён',
		'title.off': 'Обход выключен',
		'title.unknown': 'Режим не распознан',

		'sub.nikki.auto': 'Авто → {node}',
		'sub.nikki.manual': 'Закреплён вручную: {node}',
		'sub.nikki.down': 'Clash API недоступен',
		'sub.b4': 'Обход DPI · сет {set}',
		'sub.b4.down': 'Панель b4 недоступна',
		'sub.off': 'Трафик идёт напрямую, без туннеля',
		'sub.unknown': 'В /etc/config/netmode значение вне набора. Демон не исправляет его сам.',

		'net.online': 'Интернет есть',
		'net.offline': 'Нет интернета',
		'net.checking': 'Проверяю канал',
		'net.tip.online': 'Внешний канал через {ssid} отвечает.',
		'net.tip.offline': 'Роутер подключён к {ssid}, но канал не отвечает. Проверьте провайдера.',
		'net.nossid': 'канал не выбран',

		'mode.hint': 'Одно нажатие — переключение занимает 5–15 секунд',
		'mode.hint.busy': 'Идёт операция, кнопки заблокированы',

		'job.left': 'осталось ~{sec} с',
		'job.blocked': 'кнопки заблокированы',

		// Метка операции переводится здесь, а не приходит с демона: на роутере
		// словарь означал бы вторую копию этого файла и второе место, где
		// строки разъезжаются. С демона приходят только kind и arg.
		'job.mode.nikki': 'Переключаю на Nikki',
		'job.mode.b4': 'Переключаю на b4',
		'job.mode.off': 'Выключаю обход',
		'job.subscription': 'Обновляю подписку',
		'job.working': 'Идёт операция',

		// Тексты ошибок по машинному коду от демона. Сообщение самого демона
		// в панель не попадает: оно русское и уходит в syslog.
		'err.generic': 'Ошибка',
		'err.busy': 'Уже идёт другая операция. Дождитесь её завершения.',
		'err.timeout': 'Демон не ответил вовремя. Повторите — панель не знает, дошёл ли запрос.',
		'err.unavailable': 'Служба сейчас недоступна. Повторите через несколько секунд.',
		'err.ubus': 'ubus не отвечает — состояние беспроводной части не прочитать.',
		'err.uci': 'UCI не отвечает — конфигурацию не прочитать и не записать.',
		'err.scan': 'Сканирование эфира не удалось. Радио могло быть занято — повторите.',
		'err.ifname': 'Не удалось определить интерфейс станции — сканировать нечем.',
		'err.radio': 'Не удалось определить, какое радио работает станцией.',
		'err.sched': 'Планировщик обновлений не настроен — подписку не обновить.',
		'err.b4.partial': 'b4 погасил прочие сеты, но целевой не включил: обход DPI сейчас выключен целиком. Нажмите ещё раз.',

		'sel.empty.title': 'Сохранённых сетей нет',
		'sel.empty.text': 'Свежая установка выглядит именно так. Добавьте сеть — она сохранится выключенной.',
		'sel.alldisabled.title': 'Внешняя сеть не выбрана',
		'sel.alldisabled.text': 'Сети сохранены, но ни одна не включена. Включить можно в LuCI или по ssh — переключение из панели появится во второй фазе.',
		'sel.ambiguous.title': 'В конфигурации включено несколько сетей',
		'sel.ambiguous.text': 'Демон не знает, какую из них поднимет netifd, и не исправляет это сам. Оставьте одну — через LuCI или по ssh. До этого запись в wireless запрещена.',
		'sel.ambiguous.cmd': 'ssh root@{host} uci show wireless',

		'srv.title': 'Сервер',
		'srv.auto': 'Выбирает автоматика',
		'srv.auto.back': 'Вернуть автовыбор',
		'srv.tag.auto': 'авто',
		'srv.tag.pinned': 'закреплён',
		'srv.tip.auto': 'Узел выбран движком по замерам задержки',
		'srv.tip.pinned': 'Узел закреплён вручную — автоподбор отключён, пока не вернёте автовыбор',
		'srv.pinned.note': 'Узел закреплён. Автоподбор не работает, пока закрепление не снято.',
		'srv.measure': 'Замерить все',
		'srv.measuring': 'Замеряю…',
		'srv.down': 'Clash API не отвечает. Узлы недоступны, режим переключается по-прежнему.',
		'srv.empty': 'Узлов ещё нет — подписка не обновлялась.',
		'srv.dead': 'не отвечает',

		'sets.title': 'Сет стратегий',
		'sets.down': 'Панель b4 не отвечает. Сеты недоступны, режим переключается по-прежнему.',
		'sets.hint': 'Выбор одного сета гасит остальные — это поведение панели, не b4.',

		'wifi.title': 'Внешняя сеть',
		'wifi.scan': 'Найти сети',
		'wifi.scanning': 'Ищу сети…',
		'wifi.rescan': 'Искать снова',
		'wifi.band': 'Станция работает на {band} ГГц — сети {other} ГГц здесь не появятся.',
		'wifi.saved': 'Сохранена',
		'wifi.active': 'Активная',
		'wifi.locked': 'Активную сеть в этой фазе менять нельзя',
		'wifi.switch.soon': 'Переключение внешней сети появится во второй фазе.',
		'wifi.hidden': '(скрытая сеть)',
		'wifi.none': 'открытая',
		'wifi.empty': 'Сканирование не запускалось.',
		'wifi.add': 'Добавить сеть',
		'wifi.edit': 'Изменить пароль',
		'wifi.key': 'Пароль',
		'wifi.delete': 'Удалить',
		'wifi.deleting': 'Удаляю…',
		'wifi.confirm.delete': 'Удалить сеть {ssid} и её пароль с роутера?',
		'wifi.sheet.add': 'Новая сеть',
		'wifi.sheet.edit': 'Пароль сети {ssid}',
		'wifi.field.ssid': 'Имя сети (SSID)',
		'wifi.field.key': 'Пароль',
		'wifi.field.enc': 'Шифрование',
		'wifi.enc.open': 'открытая, без пароля',
		'wifi.save': 'Сохранить',
		'wifi.saving': 'Сохраняю…',
		'wifi.cancel': 'Отмена',
		'wifi.show': 'Показать',
		'wifi.hide': 'Скрыть',
		'wifi.keyhint': 'Пароль сохранится на роутере выключенной сетью. Домашняя сеть не пострадает: внешний канал в этой фазе не переключается.',
		'wifi.keykept': 'Оставьте поле пустым, чтобы не менять пароль.',
		'wifi.err.short': 'Пароль WiFi — от 8 до 63 символов',
		'wifi.err.ssid': 'Укажите имя сети',
		'wifi.err.stale': 'Конфигурация изменилась. Список обновлён — повторите.',
		'wifi.err.foreign': 'В LuCI есть незакоммиченные правки. Примените или отмените их.',
		'wifi.hidden.cant': 'К скрытой сети нельзя подключиться из списка: имя неизвестно.',

		'sub.title': 'Подписка',
		'sub.when': 'Обновлена {when}',
		'sub.never': 'никогда',
		'sub.nodes': 'Получено узлов: {n}',
		'sub.update': 'Обновить сейчас',
		'sub.updating': 'Обновляю…',
		'sub.emptylog': 'Обновлений ещё не было',

		// Ссылки на веб-морды в шапке. Видимая подпись остаётся короткой
		// («Nikki ↗»), полное имя уходит в aria-label и title: голое «Nikki»
		// в списке ссылок не говорит, что это, ни скринридеру, ни владельцу.
		'links.nikki': 'Панель Nikki',
		'links.b4': 'Панель b4',
		// Причина, по которой адреса может не быть. У b4 она одна и выбирается
		// панелью (links.b4 пуст — значит хост не выведен). У Nikki их три,
		// и приходят они кодом отказа от GET /api/nikki/panel: host_unknown,
		// nikki_unconfigured, panel_missing — см. ERR_KEY в app.js.
		'links.why.host': 'Адрес роутера не выводится из адреса, по которому открыта панель, — так бывает при доступе через ssh-туннель по localhost. Откройте панель по адресу роутера в LAN, и ссылка появится.',
		'links.why.unconfigured': 'Nikki на роутере не настроен — открывать нечего.',
		'links.why.nopanel': 'У Nikki на этом роутере нет веб-морды: доступен только Clash API.',
		'links.err': 'Адрес панели определить не удалось.',
		'links.blocked': 'Браузер не дал открыть новую вкладку — похоже, всплывающие окна заблокированы.',
		// Подпись у запасной ссылки: она открывает панель по-настоящему,
		// в новой вкладке. Текст «в этой вкладке» врал бы про target="_blank",
		// а адрес с секретом в подпись не выносится намеренно.
		'links.blocked.cta': 'Открыть панель Nikki',
		'links.opening': 'Открываю…',
		// Серую кнопку нажали, а сервер неожиданно отдал адрес: статус
		// опрашивается раз в секунду и мог отстать от настройки роутера.
		'links.ready': 'Адрес панели уже известен — ссылка в шапке заработает через секунду.',

		'ap.broadcasts': 'вещает {ssid}',
		'ap.clients': '{n} устройств',
		'foot.updated': 'обновлено {when}',
		'foot.stale': 'данные устарели — панель не достучалась до демона',
		'foot.phase': 'фаза 1: смена внешней сети отключена',
	},

	en: {
		'mode.nikki': 'Nikki',
		'mode.b4': 'b4',
		'mode.off': 'Off',
		'mode.unknown': 'Unknown',

		'title.nikki': 'Nikki is on',
		'title.b4': 'b4 is on',
		'title.off': 'Bypass is off',
		'title.unknown': 'Mode not recognised',

		'sub.nikki.auto': 'Auto → {node}',
		'sub.nikki.manual': 'Pinned manually: {node}',
		'sub.nikki.down': 'Clash API unreachable',
		'sub.b4': 'DPI bypass · set {set}',
		'sub.b4.down': 'b4 panel unreachable',
		'sub.off': 'Traffic goes straight out, no tunnel',
		'sub.unknown': 'The value in /etc/config/netmode is outside the set. The daemon does not correct it.',

		'net.online': 'Internet is up',
		'net.offline': 'No internet',
		'net.checking': 'Checking the link',
		'net.tip.online': 'The uplink through {ssid} responds.',
		'net.tip.offline': 'The router is on {ssid}, but the uplink does not respond. Check the provider.',
		'net.nossid': 'no network selected',

		'mode.hint': 'One tap — switching takes 5–15 seconds',
		'mode.hint.busy': 'Operation in progress, buttons are locked',

		'job.left': '~{sec}s left',
		'job.blocked': 'buttons locked',

		'job.mode.nikki': 'Switching to Nikki',
		'job.mode.b4': 'Switching to b4',
		'job.mode.off': 'Turning the bypass off',
		'job.subscription': 'Updating the subscription',
		'job.working': 'Operation in progress',

		'err.generic': 'Error',
		'err.busy': 'Another operation is already running. Wait for it to finish.',
		'err.timeout': 'The daemon did not answer in time. Try again — the panel cannot tell whether the request got through.',
		'err.unavailable': 'The service is unavailable right now. Try again in a few seconds.',
		'err.ubus': 'ubus does not respond — the wireless state cannot be read.',
		'err.uci': 'UCI does not respond — the configuration cannot be read or written.',
		'err.scan': 'The scan failed. The radio may have been busy — try again.',
		'err.ifname': 'The station interface could not be determined — there is nothing to scan with.',
		'err.radio': 'Could not tell which radio runs as the station.',
		'err.sched': 'The update scheduler is not configured — the subscription cannot be updated.',
		'err.b4.partial': 'b4 disabled the other sets but did not enable the target one: DPI bypass is fully off now. Press again.',

		'sel.empty.title': 'No saved networks',
		'sel.empty.text': 'A fresh install looks exactly like this. Add a network — it is saved disabled.',
		'sel.alldisabled.title': 'No uplink selected',
		'sel.alldisabled.text': 'Networks are saved but none is enabled. Enable one in LuCI or over ssh — switching from the panel arrives in phase 2.',
		'sel.ambiguous.title': 'Several networks are enabled at once',
		'sel.ambiguous.text': 'The daemon cannot tell which one netifd will bring up, and does not fix it on its own. Leave one — via LuCI or ssh. Until then writing to wireless is refused.',
		'sel.ambiguous.cmd': 'ssh root@{host} uci show wireless',

		'srv.title': 'Server',
		'srv.auto': 'Chosen automatically',
		'srv.auto.back': 'Back to auto',
		'srv.tag.auto': 'auto',
		'srv.tag.pinned': 'pinned',
		'srv.tip.auto': 'Picked by the engine from latency probes',
		'srv.tip.pinned': 'Pinned by hand — automatic picking is off until you restore it',
		'srv.pinned.note': 'A node is pinned. Automatic picking stays off until you unpin it.',
		'srv.measure': 'Measure all',
		'srv.measuring': 'Measuring…',
		'srv.down': 'Clash API does not respond. Nodes are unavailable; mode switching still works.',
		'srv.empty': 'No nodes yet — the subscription has never been updated.',
		'srv.dead': 'no reply',

		'sets.title': 'Strategy set',
		'sets.down': 'The b4 panel does not respond. Sets are unavailable; mode switching still works.',
		'sets.hint': 'Choosing one set disables the rest — that is the panel behaviour, not b4.',

		'wifi.title': 'Uplink network',
		'wifi.scan': 'Scan',
		'wifi.scanning': 'Scanning…',
		'wifi.rescan': 'Scan again',
		'wifi.band': 'The station runs on {band} GHz — {other} GHz networks will not show up here.',
		'wifi.saved': 'Saved',
		'wifi.active': 'Active',
		'wifi.locked': 'The active network cannot be edited in this phase',
		'wifi.switch.soon': 'Switching the uplink arrives in phase 2.',
		'wifi.hidden': '(hidden network)',
		'wifi.none': 'open',
		'wifi.empty': 'No scan has been run.',
		'wifi.add': 'Add network',
		'wifi.edit': 'Change password',
		'wifi.key': 'Password',
		'wifi.delete': 'Delete',
		'wifi.deleting': 'Deleting…',
		'wifi.confirm.delete': 'Delete {ssid} and its password from the router?',
		'wifi.sheet.add': 'New network',
		'wifi.sheet.edit': 'Password for {ssid}',
		'wifi.field.ssid': 'Network name (SSID)',
		'wifi.field.key': 'Password',
		'wifi.field.enc': 'Encryption',
		'wifi.enc.open': 'open, no password',
		'wifi.save': 'Save',
		'wifi.saving': 'Saving…',
		'wifi.cancel': 'Cancel',
		'wifi.show': 'Show',
		'wifi.hide': 'Hide',
		'wifi.keyhint': 'The password is stored on the router as a disabled network. Your home network is unaffected: the uplink is not switched in this phase.',
		'wifi.keykept': 'Leave the field empty to keep the current password.',
		'wifi.err.short': 'WiFi password must be 8 to 63 characters',
		'wifi.err.ssid': 'Enter the network name',
		'wifi.err.stale': 'The configuration changed. The list has been refreshed — try again.',
		'wifi.err.foreign': 'LuCI has uncommitted changes. Apply or discard them first.',
		'wifi.hidden.cant': 'A hidden network cannot be joined from the list: its name is unknown.',

		'sub.title': 'Subscription',
		'sub.when': 'Updated {when}',
		'sub.never': 'never',
		'sub.nodes': 'Nodes received: {n}',
		'sub.update': 'Update now',
		'sub.updating': 'Updating…',
		'sub.emptylog': 'No updates yet',

		'links.nikki': 'Nikki panel',
		'links.b4': 'b4 panel',
		'links.why.host': 'The router address does not follow from the address this panel is open at — that happens over an ssh tunnel through localhost. Open the panel at the router address on your LAN and the link will appear.',
		'links.why.unconfigured': 'Nikki is not configured on the router — there is nothing to open.',
		'links.why.nopanel': 'Nikki on this router has no web panel: only the Clash API is available.',
		'links.err': 'The panel address could not be determined.',
		'links.blocked': 'The browser refused to open a new tab — pop-ups look blocked.',
		'links.blocked.cta': 'Open the Nikki panel',
		'links.opening': 'Opening…',
		'links.ready': 'The panel address is known now — the header link starts working within a second.',

		'ap.broadcasts': 'broadcasting {ssid}',
		'ap.clients': '{n} devices',
		'foot.updated': 'updated {when}',
		'foot.stale': 'data is stale — the panel cannot reach the daemon',
		'foot.phase': 'phase 1: uplink switching is disabled',
	},
};

export function makeT(lang) {
	const d = DICT[lang] || DICT.ru;
	return (key, vars) => {
		// Недостающий ключ отдаётся как есть: пустая строка спрячет пробел
		// в интерфейсе, а видимый ключ сам себя починит на ближайшем взгляде.
		let s = d[key] ?? DICT.ru[key] ?? key;
		if (vars) for (const [k, v] of Object.entries(vars)) s = s.split('{' + k + '}').join(v);
		return s;
	};
}
