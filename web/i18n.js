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
		// Отдельная строка для первых секунд после переключения. Демон
		// коммитит режим раньше, чем поднимет службу, и «недоступен» в этот
		// момент — неправда про исправный роутер. Молчать тоже нельзя:
		// владелец, который смотрит на баннер, обязан понимать, чего ждёт.
		'sub.nikki.starting': 'Clash API запускается…',
		'sub.b4': 'Обход DPI · сет {set}',
		'sub.b4.down': 'Панель b4 недоступна',
		'sub.b4.starting': 'b4 запускается…',
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

		// ─── проброс LAN в uplink (ADR-0030) ───
		//
		// Причины провала: закрытый набор, тот же, что allBridgeReasons в Go,
		// сверяет scripts/check-fail-reasons.sh. Каждая пара title/text
		// обязательна — половина пары печатается в интерфейсе как сам ключ.
		'bridge.fail.apply_failed.title': 'Применить не удалось',
		'bridge.fail.apply_failed.text': 'Роутер не применил настройки проброса. Записанное могло остаться неприменённым — повторите или проверьте состояние по ssh.',
		'bridge.fail.busy.title': 'Сеть занята другим процессом',
		'bridge.fail.busy.text': 'Кто-то ещё сейчас применяет настройки — например, из LuCI или по ssh. Ничего не изменено: подождите и повторите.',
		'bridge.fail.prereq_missing.title': 'На роутере не хватает нужных программ',
		'bridge.fail.prereq_missing.text': 'Не нашлось flock, ubus, apk или ping. Это неполадка прошивки, не настроек, — повтор не поможет, нужен ssh.',
		'bridge.fail.executor_missing.title': 'На роутере не установлен netmode-bridge',
		'bridge.fail.executor_missing.text': 'Настройки записаны, но применить их нечем: скрипта нет на роутере. Установите пакет заново (deploy.sh --install) и повторите.',
		'bridge.fail.install_failed.title': 'Не удалось установить relayd',
		'bridge.fail.install_failed.text': 'apk не смог поставить пакет — чаще всего нет интернета через внешнюю сеть или места на флеш-памяти. Конфигурация не тронута: установка идёт до первой записи.',
		'bridge.fail.no_iface.title': 'Интерфейс проброса не поднялся',
		'bridge.fail.no_iface.text': 'Настройки применены, но нога роутера во внешней сети не появилась с нужным адресом. Проверьте, свободен ли выбранный адрес, и посмотрите logread на роутере.',
		'bridge.fail.relay_down.title': 'Служба relayd не в нужном состоянии',
		'bridge.fail.relay_down.text': 'Интерфейс поднят, а сам мост не работает: процесс relayd не запустился (или не остановился при выключении). ПК не будет виден внешней сети — смотрите logread.',
		'bridge.fail.unverifiable.title': 'Исход неизвестен',
		'bridge.fail.unverifiable.text': 'Настройки применены, но проверить результат не удалось: ubus не ответил. Успех и провал одинаково возможны — откройте панель заново или проверьте состояние по ssh.',
		'bridge.fail.stale_draft.title': 'В конфигурации застрял черновик',
		'bridge.fail.stale_draft.text': 'Операция не состоялась, и отменить недописанные правки роутер не смог. Повтор не поможет: панель будет отказываться, ссылаясь на незакоммиченные правки, а в LuCI при этом пусто — черновик наш. Зайдите по ssh и выполните: uci revert network; uci revert firewall',
		// Запасной ключ: reason вне набора (панель старее демона) либо reason
		// ещё не приехал — те же два случая, что у wifi.fail.unknown.
		'bridge.fail.unknown.title': 'Операция не вышла, причина неизвестна',
		'bridge.fail.unknown.text': 'Роутер сообщил о неудаче, но причину панель не распознала. Если панель и демон разных версий — обновите панель; иначе подождите пару секунд, причина придёт следующим опросом.',
		// Вкладки. Подписи короткие намеренно: сегмент-контрол делит ширину
		// телефона на три, и длинное слово сломало бы ряд.
		'tabs.wifi': 'Wi-Fi',
		'tabs.bypass': 'Обход',
		'tabs.bridge': 'Проброс',
		// Пустое состояние ОБЩЕГО слота операции. Не про смену режима, как
		// mode.hint: слот виден на всех вкладках, в том числе там, где кнопок
		// режима нет вовсе.
		'job.hint': 'Панель выполняет одну операцию за раз',
		// Заглушка вкладки проброса до следующей фазы: честнее пустой карточки.
		'bridge.title': 'Проброс LAN в uplink',
		'bridge.stub': 'Управление пробросом появится в следующей фазе. Пока схема настраивается только по ssh.',

		'job.left': 'осталось ~{sec} с',
		'job.blocked': 'кнопки заблокированы',

		// Метка операции переводится здесь, а не приходит с демона: на роутере
		// словарь означал бы вторую копию этого файла и второе место, где
		// строки разъезжаются. С демона приходят только kind и arg.
		'job.mode.nikki': 'Переключаю на Nikki',
		'job.mode.b4': 'Переключаю на b4',
		'job.mode.off': 'Выключаю обход',
		// arg здесь — ssid целевой сети, а не имя секции: netmode_1a2b3c4d
		// человеку не говорит ничего (openapi, Job.arg).
		'job.upstream': 'Переключаю на {ssid}',
		'job.subscription': 'Обновляю подписку',
		'job.working': 'Идёт операция',
		// Провал виден пять секунд — ровно столько демон держит завершённую
		// операцию в статусе. Раньше панель выбрасывала всё, кроме running,
		// и сорванное переключение заканчивалось молча: полоска исчезала,
		// кнопки отпускались, а что случилось — не говорил никто. Здесь
		// заголовок, причина приходит в job.error и по контракту пригодна
		// для показа.
		'job.fail.mode': 'Не удалось переключиться на {mode}',
		'job.fail.subscription': 'Подписка не обновилась',
		// Заголовок в слоте баннера. Причину и подробности несёт блок у карточки
		// сети — там, где нажимали: см. UpstreamFailNote в app.js.
		'job.fail.upstream': 'Переключение сети не выполнено',
		'job.fail': 'Операция не удалась',

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
		'sel.alldisabled.text': 'Сети сохранены, но ни одна не включена. Нажмите «Подключить» у нужной сети ниже, чтобы сделать её активной.',
		'sel.ambiguous.title': 'В конфигурации включено несколько сетей',
		// «Запись в wireless запрещена» было верно до ADR-0026 и стало неточным
		// после: переключение — как раз запись, и как раз разрешённая. Фраза
		// сужена до того, что осталось правдой, — правки и удаления.
		'sel.ambiguous.text': 'Демон не знает, какую из них поднимет netifd, и не исправляет это сам. До тех пор пока включено больше одной, править или удалять сохранённые сети нельзя — ни через панель, ни в обход её.',
		// Почему одна кнопка в строке живая, а соседняя заперта. Без этого абзаца
		// асимметрия читается как баг: кнопки стоят рядом и выглядят одинаково.
		'sel.ambiguous.switch': 'Кнопка «Подключить» у сетей ниже работает и сейчас: выбрать одну из них — это и есть способ выйти из этой ситуации, поэтому она не заперта вместе с остальными. Изменить пароль или удалить сеть нельзя, пока включено больше одной, — правку в этом состоянии невозможно доказать безвредной.',
		'sel.ambiguous.cmd': 'ssh root@{host} uci show wireless',

		// Половинчатая установка пакета. Формулировка обязана сказать три
		// вещи в этом порядке: что не работает, чего именно нет, что делать.
		// Прежде чем блок появился, владелец узнавал всё это по факту —
		// нажатием, после которого конфигурация оказывалась опубликованной и
		// неприменённой.
		'exec.missing.title': 'На роутере не хватает скриптов применения',
		'exec.missing.text': 'Демон установлен не полностью: на роутере нет этих файлов. Смена режима и переключение внешней сети запишут выбор в конфигурацию, но применить его будет нечем — роутер останется в прежнем состоянии, а записанное повиснет неприменённым.',
		'exec.missing.fix': 'Переустановите пакет целиком: ./scripts/deploy.sh --install. Повторные нажатия до этого не помогут.',

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
		'srv.test.ok': 'Замерены все {n} узлов',
		'srv.test.part': 'Замерено {ok} из {n}: не ответили {bad}',
		'srv.test.cut': 'Замерено {ok} из {n} — на остальные не хватило времени, о них ничего не известно',
		'srv.test.none': 'Ни один из {n} узлов не ответил. Похоже, наружу не выходит ничего.',
		'srv.test.empty': 'Замерять нечего: в группе нет узлов',
		'srv.down': 'Clash API не отвечает. Узлы недоступны, режим переключается по-прежнему.',
		// Подпись под скелетоном. Скелетон без слов честен, но молчалив:
		// на третьей секунде владелец обязан понимать, что идёт запуск, а не
		// вечная загрузка. Поэтому здесь сказано, чего именно ждём и чем
		// ожидание кончится, — «загрузка…» не говорит ни того, ни другого.
		'srv.starting': 'Nikki запускается — узлы появятся, как только ответит Clash API.',
		'srv.empty': 'Узлов ещё нет — подписка не обновлялась.',
		'srv.dead': 'не отвечает',

		'sets.title': 'Сет стратегий',
		'sets.down': 'Панель b4 не отвечает. Сеты недоступны, режим переключается по-прежнему.',
		'sets.starting': 'b4 запускается — сеты появятся, как только он ответит.',
		'sets.hint': 'Выбор одного сета гасит остальные — это поведение панели, не b4.',

		'wifi.title': 'Внешняя сеть',
		'wifi.scan': 'Найти сети',
		'wifi.scanning': 'Ищу сети…',
		'wifi.rescan': 'Искать снова',
		'wifi.band': 'Станция работает на {band} ГГц — сети {other} ГГц здесь не появятся.',
		'wifi.saved': 'Сохранена',
		'wifi.active': 'Активная',
		// Правило не фазовое, а постоянное (ADR-0026): правка включённой секции
		// рвёт ассоциацию при ближайшем применении. Зато выход из него теперь
		// есть, и он в этой же панели, — про него вторая фраза.
		'wifi.locked': 'Активную сеть менять нельзя. Чтобы сменить её пароль, сначала переключитесь на другую сеть.',
		'wifi.connect': 'Подключить',
		'wifi.connect.title': 'Сделать эту сеть активной',
		'wifi.connecting': 'Подключаю…',
		// Диалог называет цену прямо: «интернета не будет» вместо «может быть
		// недоступен». Отката нет (ADR-0006), и понимание этого — единственное,
		// что здесь заменяет автоматическое восстановление. Про домашнюю сеть
		// сказано «не отключит ваши устройства», а не «не потеряете ни пакета»:
		// первое измерено (ADR-0025), второе — нет.
		'wifi.confirm.switch': 'Переключить внешнюю сеть на {ssid}? Отката нет: если пароль окажется неверным или сеть не поднимется, интернета через роутер не будет, пока вы не переключитесь обратно сами. Панель останется доступна по локальной сети, а домашняя сеть {home} не отключит ваши устройства — это измерено.',
		'wifi.hidden': '(скрытая сеть)',
		'wifi.none': 'открытая',
		'wifi.empty': 'Сканирование не запускалось.',
		// Отказ эндпоинта, а не пустой список: sel.empty.title утверждает,
		// что сохранённых сетей нет, — про роутер, у которого их просто
		// не удалось прочитать, это неправда.
		'wifi.down': 'Список сохранённых сетей недоступен',
		'wifi.add': 'Добавить сеть',
		'wifi.edit': 'Изменить пароль',
		'wifi.key': 'Пароль',
		'wifi.delete': 'Удалить',
		'wifi.deleting': 'Удаляю…',
		'wifi.confirm.delete': 'Удалить сеть {ssid} и её пароль с роутера?',
		'wifi.sheet.add': 'Новая сеть',
		'wifi.sheet.saveconnect': 'Сохранить и подключиться',
		// Дубль ssid — штатный случай (ADR-0005), а не ошибка ввода: два профиля
		// одной сети с разными паролями бывают. Поэтому подсказка, а не запрет.
		'wifi.hint.dup': 'Сеть с именем {ssid} уже сохранена — это будет вторая запись с тем же именем.',
		// Форма сохранила сеть, но какую именно запись включать — не разобрала.
		// Поиск идёт диффом по id (совпадение по ssid снято: дубли имён штатны),
		// и сюда приводит ответ, где новых записей не ровно одна: ноль либо две
		// и больше — список поменяла не только наша запись. Гадать нельзя:
		// у чужой записи может быть другой пароль, включится не та сеть.
		'wifi.saved.pick': 'Сеть {ssid} сохранена. Какую именно запись включать, панель не определила однозначно — нажмите «Подключить» у нужной строки.',
		// Два шага — две новости, и вторая без первой врёт. Форма к этому
		// моменту закрыта, поэтому общее «не удалось» читается как «ничего
		// не сохранилось»: владелец заводит сеть заново и получает два профиля
		// одной сети с разными паролями (дубли штатны, ADR-0005).
		// {why} стоит В КОНЦЕ, и это не вкусовщина: подставляется туда готовая
		// фраза из describe(), почти всегда со своей точкой. В середине она
		// давала бы «не прочитать.. Сеть в списке» — две точки подряд на каждом
		// втором коде.
		'wifi.saved.nolink': 'Сеть {ssid} сохранена, но переключиться на неё не удалось. Она есть в списке — повторить можно кнопкой «Подключить» в её строке. Причина: {why}',
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
		// Вторая фраза раньше обещала, что внешний канал не переключается вовсе;
		// теперь он переключается с этого же экрана. Верным осталось то, что
		// сохранение само по себе ничего не включает.
		'wifi.keyhint': 'Пароль сохранится на роутере выключенной сетью. Домашняя сеть не пострадает: это отдельная запись, и пока вы не подключитесь к ней явно, она не используется.',
		'wifi.keykept': 'Оставьте поле пустым, чтобы не менять пароль.',
		'wifi.err.short': 'Пароль WiFi — от 8 до 63 символов',
		'wifi.err.ssid': 'Укажите имя сети',
		'wifi.err.stale': 'Конфигурация изменилась. Список обновлён — повторите.',
		'wifi.err.foreign': 'В LuCI есть незакоммиченные правки. Примените или отмените их.',
		'wifi.err.already_selected': 'Список устарел: эта сеть уже активна. Список обновлён — переключать больше не на что.',
		'wifi.err.network_incomplete': 'Эту сеть нельзя сделать активной: в конфигурации не хватает данных (имени, пароля или сети). Проверьте её через «Пароль» или в LuCI.',
		'wifi.hidden.cant': 'К скрытой сети нельзя подключиться из списка: имя неизвестно.',

		// Исход переключения: заголовок и объяснение на каждую машинную причину.
		// Одного текста на десять причин не хватает — «не вышло» означает разное:
		// заведомо не применилось (apply_failed, busy, prereq_missing,
		// executor_missing), применилось не туда (stayed_on_previous,
		// other_ssid), результат неизвестен (unverifiable), а no_ipv4 — не
		// провал ассоциации вовсе.
		//
		// Текст apply_failed называет КЛАСС, а не механизм, и это исправление
		// по живому отказу. Прежняя формулировка обещала «оба способа
		// применения отказали», хотя код выставляется шестью разными путями:
		// неудачной записью, разошедшимся отпечатком, неудавшимся коммитом,
		// отказом глаголов и неизвестным кодом возврата. Владелец, у которого
		// на роутере не было самого скрипта применения, читал уверенный
		// диагноз про механизм, который ни разу не запускался. Конкретика
		// теперь приезжает отдельным полем detail.
		'wifi.fail.apply_failed.title': 'Применить не удалось',
		'wifi.fail.apply_failed.text': 'Роутер не применил переключение на {ssid}. Конфигурация записана — повторите или проверьте состояние по ssh.',
		'wifi.fail.busy.title': 'Радио занято другим процессом',
		'wifi.fail.busy.text': 'Кто-то ещё сейчас настраивает радио — например, из LuCI или по ssh. Подождите и повторите переключение на {ssid}.',
		'wifi.fail.prereq_missing.title': 'На роутере не хватает нужных программ',
		'wifi.fail.prereq_missing.text': 'Намерение записано, но переключение на {ssid} не выполнено: на роутере не нашлось flock или ubus. Это неполадка прошивки, не пароля — обратитесь по ssh.',
		// Отдельно от prereq_missing, и разница не в оттенке: там нет системной
		// утилиты (чинит прошивка), здесь нет НАШЕГО скрипта — половинчатая
		// установка пакета. Совет поэтому разный, и «повторите» не годится ни
		// в каком виде: повтор упрётся в то же отсутствие файла.
		//
		// Про «уже записано» сказано прямым текстом намеренно. Конфигурация
		// опубликована и висит неприменённой: станция не подключена ни к
		// старой сети, ни к новой, и владелец, не знающий этого, идёт искать
		// поломку в эфире. Команда для ssh названа целиком — «передёрните
		// радио» ему там не поможет.
		'wifi.fail.executor_missing.title': 'На роутере не установлен netmode-wifi',
		'wifi.fail.executor_missing.text': 'Переключение на {ssid} записано в конфигурацию, но применить его нечем: скрипта применения нет на роутере. Установите пакет заново (deploy.sh --install) и повторите. Уже записанное можно дожать по ssh: wifi up <радио>.',
		'wifi.fail.stayed_on_previous.title': 'Осталась на прежней сети',
		'wifi.fail.stayed_on_previous.text': 'Роутер принял команду, но станция не перешла на {ssid} и осталась на прежней сети. Конфигурация уже записана — повторное нажатие «Подключить» имеет смысл.',
		'wifi.fail.other_ssid.title': 'Подключилась к другой сети',
		'wifi.fail.other_ssid.text': 'Станция ассоциировалась не с {ssid} и не с прежней сетью, а с какой-то третьей. Проверьте эфир поблизости и повторите.',
		'wifi.fail.not_associated.title': 'Не подключилась к {ssid}',
		'wifi.fail.not_associated.text': 'За отведённое время станция не ассоциировалась с сетью. Самая частая причина — неверный пароль, но могла быть и слабым сигналом или недоступностью точки доступа. Проверьте пароль и повторите.',
		'wifi.fail.no_ipv4.title': 'Подключилась, но без адреса',
		'wifi.fail.no_ipv4.text': 'Станция ассоциировалась с {ssid}, но не получила адрес по DHCP — интернета через эту сеть нет. Проверьте настройки этой сети или роутер, который её раздаёт.',
		'wifi.fail.unverifiable.title': 'Исход неизвестен',
		'wifi.fail.unverifiable.text': 'Роутер применил переключение на {ssid}, но не смог проверить результат: ubus не ответил или ответ не разобрался. Успех и провал одинаково возможны — откройте панель заново или проверьте состояние по ssh.',
		// Единственная причина, где «повторите» — вредный совет: повтор упрётся
		// в тот же застрявший черновик и выдаст отказ про чужие правки. Поэтому
		// текст ведёт не к кнопке, а в терминал, и называет команду целиком.
		'wifi.fail.stale_draft.title': 'В конфигурации застрял черновик',
		'wifi.fail.stale_draft.text': 'Переключение на {ssid} не состоялось, и отменить недописанные правки роутер не смог — они остались в конфигурации черновиком. Повторное нажатие не поможет: панель будет отказываться, ссылаясь на незакоммиченные правки, и в LuCI при этом пусто — черновик наш. Зайдите на роутер по ssh и выполните: uci revert wireless',
		// Запасной ключ, и случаев в нём ДВА, а не один (web/app.js, upFail
		// и UpstreamFailNote): reason вне закрытого набора — панель старее
		// демона; reason === '' — джоб уже провалился, а last_fail ещё не
		// приехал (поле в памяти демона, придёт следующим опросом). Прежний
		// текст описывал только первый и во втором врал: обещал незнакомый
		// код там, где кода не было вовсе. Поэтому новый называет не причину,
		// а своё незнание — и разводит ДЕЙСТВИЯ: в гонке ждать (причина придёт
		// сама), при рассинхроне версий не ждать (не придёт никогда).
		// Заголовок намеренно не сближен с unverifiable выше: там исход правда
		// неизвестен (успех равновозможен), здесь провал установлен и неясна
		// лишь причина. Один заголовок на двоих стёр бы разницу между
		// «может, получилось» и «точно не получилось».
		'wifi.fail.unknown.title': 'Переключение не вышло, причина неизвестна',
		'wifi.fail.unknown.text': 'Переключиться на {ssid} не удалось, но причину панель назвать не может. Либо она ещё не доехала — тогда появится здесь сама через секунду, подождите. Либо демон прислал код, которого эта версия панели не знает, — тогда не появится никогда, и остаётся обновить панель или посмотреть состояние по ssh.',

		'sub.title': 'Подписка',
		'sub.when': 'Обновлена {when}',
		'sub.never': 'никогда',
		'sub.nodes': 'Получено узлов: {n}',
		'sub.update': 'Обновить сейчас',
		'sub.updating': 'Обновляю…',
		'sub.emptylog': 'Обновлений ещё не было',
		// Список не доехал — это не то же самое, что «обновлений не было».
		// Второе — утверждение о роутере, и оно ложно, когда просто лёг
		// эндпоинт. Разделение видно с тех пор, как побочные списки стартуют
		// с undefined, а null означает именно отказ.
		'sub.log.down': 'Журнал обновлений недоступен',

		// Ссылки на чужие веб-морды в шапке — их три. Видимая подпись остаётся
		// короткой («Nikki ↗»), полное имя уходит в aria-label и title: голое
		// «Nikki» в списке ссылок не говорит, что это, ни скринридеру, ни
		// владельцу. Слово «панель» здесь занято нашей собственной, поэтому
		// у LuCI подпись говорит «веб-интерфейс роутера», а не «панель LuCI».
		'links.nikki': 'Панель Nikki',
		'links.b4': 'Панель b4',
		'links.luci': 'Веб-интерфейс роутера (LuCI)',
		// Почему панель не открылась. Тексты нужны только Nikki: её кнопка
		// видима, пока Clash API отвечает, но живой Clash API ещё не значит
		// открываемой панели. Причину называет сам сервер кодом отказа от
		// GET /api/nikki/panel — host_unknown, nikki_unconfigured,
		// panel_missing — см. ERR_KEY в app.js. Панель ничего не выбирает
		// сама: кнопки без адреса она просто не рисует. links.why.host
		// остаётся страховкой describe() на случай, когда адрес успел
		// протухнуть между опросом и кликом.
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

		// Крестик убирает сообщение с глаз и ничего не сообщает демону:
		// «прочитал», а не «исправлено».
		'ui.dismiss': 'Скрыть',

		'ap.broadcasts': 'вещает {ssid}',
		'ap.clients': '{n} устройств',
		'foot.updated': 'обновлено {when}',
		'foot.stale': 'данные устарели — панель не достучалась до демона',

		// Первый экран. foot.stale тут врал бы: устаревать было нечему,
		// данных ещё не было ни одного раза.
		'boot': 'Читаю состояние роутера…',
		'boot.down.title': 'Демон не отвечает',
		'boot.down': 'Панель продолжает спрашивать и откроется, как только он ответит.',
		// Не про фазу, а про постоянное свойство операции: у переключения нет
		// отката (ADR-0006), и напоминать об этом стоит не только в диалоге.
		'foot.phase': 'смена внешней сети — без автоотката',
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
		'sub.nikki.starting': 'Clash API is starting…',
		'sub.b4': 'DPI bypass · set {set}',
		'sub.b4.down': 'b4 panel unreachable',
		'sub.b4.starting': 'b4 is starting…',
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

		'bridge.fail.apply_failed.title': 'Could not apply',
		'bridge.fail.apply_failed.text': 'The router did not apply the bridge settings. What was written may remain unapplied — retry or check over ssh.',
		'bridge.fail.busy.title': 'Network is busy with another process',
		'bridge.fail.busy.text': 'Someone else is applying settings right now — LuCI or ssh. Nothing was changed: wait and retry.',
		'bridge.fail.prereq_missing.title': 'The router is missing required tools',
		'bridge.fail.prereq_missing.text': 'flock, ubus, apk or ping was not found. This is a firmware problem, not a settings one — retrying will not help, you need ssh.',
		'bridge.fail.executor_missing.title': 'netmode-bridge is not installed on the router',
		'bridge.fail.executor_missing.text': 'Settings are written, but there is nothing to apply them with: the script is missing. Reinstall the package (deploy.sh --install) and retry.',
		'bridge.fail.install_failed.title': 'Could not install relayd',
		'bridge.fail.install_failed.text': 'apk failed to install the package — usually no internet over the uplink, or no space in flash. The configuration is untouched: installation runs before the first write.',
		'bridge.fail.no_iface.title': 'Bridge interface did not come up',
		'bridge.fail.no_iface.text': 'Settings were applied, but the router leg in the uplink network never appeared with the expected address. Check whether the chosen address is free and look at logread on the router.',
		'bridge.fail.relay_down.title': 'The relayd service is in the wrong state',
		'bridge.fail.relay_down.text': 'The interface is up but the bridge itself is not working: relayd did not start (or did not stop when disabling). The PC will not be visible to the uplink network — check logread.',
		'bridge.fail.unverifiable.title': 'Outcome unknown',
		'bridge.fail.unverifiable.text': 'Settings were applied, but the result could not be verified: ubus did not answer. Success and failure are equally possible — reopen the panel or check over ssh.',
		'bridge.fail.stale_draft.title': 'A draft is stuck in the configuration',
		'bridge.fail.stale_draft.text': 'The operation did not go through and the router could not revert the half-written changes. Retrying will not help: the panel will refuse, citing uncommitted changes, while LuCI shows none — the draft is ours. Go in over ssh and run: uci revert network; uci revert firewall',
		'bridge.fail.unknown.title': 'The operation failed, reason unknown',
		'bridge.fail.unknown.text': 'The router reported a failure but the panel did not recognise the reason. If the panel and the daemon are different versions — update the panel; otherwise wait a couple of seconds, the reason arrives with the next poll.',
		'tabs.wifi': 'Wi-Fi',
		'tabs.bypass': 'Bypass',
		'tabs.bridge': 'Bridge',
		'job.hint': 'The panel runs one operation at a time',
		'bridge.title': 'LAN-to-uplink bridge',
		'bridge.stub': 'Bridge management arrives in the next phase. For now the setup is ssh-only.',

		'job.left': '~{sec}s left',
		'job.blocked': 'buttons locked',

		'job.mode.nikki': 'Switching to Nikki',
		'job.mode.b4': 'Switching to b4',
		'job.mode.off': 'Turning the bypass off',
		'job.upstream': 'Switching to {ssid}',
		'job.subscription': 'Updating the subscription',
		'job.working': 'Operation in progress',
		'job.fail.mode': 'Could not switch to {mode}',
		'job.fail.subscription': 'The subscription did not update',
		'job.fail.upstream': 'The network switch did not run',
		'job.fail': 'The operation failed',

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
		'sel.alldisabled.text': 'Networks are saved but none is enabled. Press “Connect” on the network you want below to make it active.',
		'sel.ambiguous.title': 'Several networks are enabled at once',
		'sel.ambiguous.text': 'The daemon cannot tell which one netifd will bring up, and does not fix it on its own. While more than one is enabled, saved networks cannot be edited or deleted — neither through the panel nor around it.',
		'sel.ambiguous.switch': 'The “Connect” button on the networks below still works right now: picking one of them is exactly how you get out of this situation, which is why it is not locked along with the rest. Changing a password or deleting a network is not possible while more than one is enabled — an edit cannot be shown safe in that state.',
		'sel.ambiguous.cmd': 'ssh root@{host} uci show wireless',

		'exec.missing.title': 'The router is missing the apply scripts',
		'exec.missing.text': 'The daemon is only partly installed: these files are not on the router. Changing the mode or switching the upstream network will write the choice into the configuration, but there will be nothing to apply it with — the router stays as it is, and what was written hangs unapplied.',
		'exec.missing.fix': 'Reinstall the whole package: ./scripts/deploy.sh --install. Pressing again before that will not help.',

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
		'srv.test.ok': 'All {n} nodes measured',
		'srv.test.part': 'Measured {ok} of {n}: {bad} did not answer',
		'srv.test.cut': 'Measured {ok} of {n} — no time left for the rest, nothing is known about them',
		'srv.test.none': 'None of the {n} nodes answered. Looks like nothing gets out at all.',
		'srv.test.empty': 'Nothing to measure: the group has no nodes',
		'srv.down': 'Clash API does not respond. Nodes are unavailable; mode switching still works.',
		'srv.starting': 'Nikki is starting — nodes appear as soon as the Clash API answers.',
		'srv.empty': 'No nodes yet — the subscription has never been updated.',
		'srv.dead': 'no reply',

		'sets.title': 'Strategy set',
		'sets.down': 'The b4 panel does not respond. Sets are unavailable; mode switching still works.',
		'sets.starting': 'b4 is starting — sets appear as soon as it answers.',
		'sets.hint': 'Choosing one set disables the rest — that is the panel behaviour, not b4.',

		'wifi.title': 'Uplink network',
		'wifi.scan': 'Scan',
		'wifi.scanning': 'Scanning…',
		'wifi.rescan': 'Scan again',
		'wifi.band': 'The station runs on {band} GHz — {other} GHz networks will not show up here.',
		'wifi.saved': 'Saved',
		'wifi.active': 'Active',
		'wifi.locked': 'The active network cannot be edited. To change its password, switch to a different network first.',
		'wifi.connect': 'Connect',
		'wifi.connect.title': 'Make this network active',
		'wifi.connecting': 'Connecting…',
		'wifi.confirm.switch': 'Switch the uplink to {ssid}? There is no rollback: if the password turns out to be wrong or the network does not come up, there will be no internet through the router until you switch back yourself. The panel stays reachable on the local network, and the home network {home} will not disconnect your devices — this is measured.',
		'wifi.hidden': '(hidden network)',
		'wifi.none': 'open',
		'wifi.empty': 'No scan has been run.',
		'wifi.down': 'The saved-network list is unavailable',
		'wifi.add': 'Add network',
		'wifi.edit': 'Change password',
		'wifi.key': 'Password',
		'wifi.delete': 'Delete',
		'wifi.deleting': 'Deleting…',
		'wifi.confirm.delete': 'Delete {ssid} and its password from the router?',
		'wifi.sheet.add': 'New network',
		'wifi.sheet.saveconnect': 'Save and connect',
		'wifi.hint.dup': 'A network named {ssid} is already saved — this will be a second entry with the same name.',
		'wifi.saved.pick': 'The network {ssid} is saved. The panel could not tell which entry to enable — press “Connect” on the row you want.',
		'wifi.saved.nolink': 'The network {ssid} was saved, but switching to it failed. It is in the list — you can retry with “Connect” on its row. Reason: {why}',
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
		'wifi.keyhint': 'The password is stored on the router as a disabled network. Your home network is unaffected: this is a separate entry, and it is not used until you connect to it explicitly.',
		'wifi.keykept': 'Leave the field empty to keep the current password.',
		'wifi.err.short': 'WiFi password must be 8 to 63 characters',
		'wifi.err.ssid': 'Enter the network name',
		'wifi.err.stale': 'The configuration changed. The list has been refreshed — try again.',
		'wifi.err.foreign': 'LuCI has uncommitted changes. Apply or discard them first.',
		'wifi.err.already_selected': 'The list was stale: this network is already active. The list has been refreshed — there is nothing left to switch to.',
		'wifi.err.network_incomplete': 'This network cannot be made active: the configuration is missing data (name, password, or network). Check it via “Password” or in LuCI.',
		'wifi.hidden.cant': 'A hidden network cannot be joined from the list: its name is unknown.',

		'wifi.fail.apply_failed.title': 'Could not apply',
		'wifi.fail.apply_failed.text': 'The router did not apply the switch to {ssid}. The configuration is saved — try again, or check over ssh.',
		'wifi.fail.busy.title': 'Radio busy with another process',
		'wifi.fail.busy.text': 'Something else is configuring the radio right now — from LuCI or over ssh, for example. Wait and try switching to {ssid} again.',
		'wifi.fail.prereq_missing.title': 'The router is missing required tools',
		'wifi.fail.prereq_missing.text': 'The intent is saved, but the switch to {ssid} did not run: the router is missing flock or ubus. This is a firmware problem, not the password — check over ssh.',
		'wifi.fail.executor_missing.title': 'netmode-wifi is not installed on the router',
		'wifi.fail.executor_missing.text': 'The switch to {ssid} is written to the configuration, but there is nothing to apply it with: the apply script is missing from the router. Reinstall the package (deploy.sh --install) and try again. What is already written can be forced over ssh: wifi up <radio>.',
		'wifi.fail.stayed_on_previous.title': 'Stayed on the previous network',
		'wifi.fail.stayed_on_previous.text': 'The router accepted the command, but the station did not move to {ssid} and stayed on the previous network. The configuration is already saved — pressing Connect again makes sense.',
		'wifi.fail.other_ssid.title': 'Connected to a different network',
		'wifi.fail.other_ssid.text': 'The station associated with neither {ssid} nor the previous network, but with a third one. Check nearby networks and try again.',
		'wifi.fail.not_associated.title': 'Did not connect to {ssid}',
		'wifi.fail.not_associated.text': 'The station did not associate with the network within the allotted time. The most common cause is a wrong password, but a weak signal or an unreachable access point can also do this. Check the password and try again.',
		'wifi.fail.no_ipv4.title': 'Connected, but without an address',
		'wifi.fail.no_ipv4.text': 'The station associated with {ssid} but did not get an address over DHCP — there is no internet through this network. Check that network’s settings or the router providing it.',
		'wifi.fail.unverifiable.title': 'Outcome unknown',
		'wifi.fail.unverifiable.text': 'The router applied the switch to {ssid} but could not verify the result: ubus did not answer, or its answer did not parse. Success and failure are equally possible — reopen the panel or check over ssh.',
		'wifi.fail.stale_draft.title': 'A draft is stuck in the configuration',
		'wifi.fail.stale_draft.text': 'The switch to {ssid} did not happen, and the router could not undo the half-written changes — they stayed in the configuration as a draft. Trying again will not help: the panel will keep refusing, citing uncommitted changes, and LuCI will show none — the draft is ours. Log in to the router over ssh and run: uci revert wireless',
		'wifi.fail.unknown.title': 'Switch failed, reason unknown',
		'wifi.fail.unknown.text': 'The switch to {ssid} did not succeed, but the panel cannot name the reason. Either it has not arrived yet — then it shows up here on its own within a second, so just wait. Or the daemon sent a code this panel version does not recognize — then it never will, and what is left is to update the panel or check over ssh.',

		'sub.title': 'Subscription',
		'sub.when': 'Updated {when}',
		'sub.never': 'never',
		'sub.nodes': 'Nodes received: {n}',
		'sub.update': 'Update now',
		'sub.updating': 'Updating…',
		'sub.emptylog': 'No updates yet',
		'sub.log.down': 'The update log is unavailable',

		'links.nikki': 'Nikki panel',
		'links.b4': 'b4 panel',
		'links.luci': 'Router web interface (LuCI)',
		'links.why.host': 'The router address does not follow from the address this panel is open at — that happens over an ssh tunnel through localhost. Open the panel at the router address on your LAN and the link will appear.',
		'links.why.unconfigured': 'Nikki is not configured on the router — there is nothing to open.',
		'links.why.nopanel': 'Nikki on this router has no web panel: only the Clash API is available.',
		'links.err': 'The panel address could not be determined.',
		'links.blocked': 'The browser refused to open a new tab — pop-ups look blocked.',
		'links.blocked.cta': 'Open the Nikki panel',
		'links.opening': 'Opening…',

		'ui.dismiss': 'Dismiss',

		'ap.broadcasts': 'broadcasting {ssid}',
		'ap.clients': '{n} devices',
		'foot.updated': 'updated {when}',
		'foot.stale': 'data is stale — the panel cannot reach the daemon',

		'boot': 'Reading the router state…',
		'boot.down.title': 'The daemon does not answer',
		'boot.down': 'The panel keeps asking and opens as soon as it does.',
		'foot.phase': 'uplink switching has no auto-rollback',
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
