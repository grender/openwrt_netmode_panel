package happ

// Перенос xhttpSettings.extra в xhttp-opts mihomo.
//
// ЭТО НЕ ТЮНИНГ, И ОТКАТЫВАТЬ ЕГО НЕЛЬЗЯ. Соблазн ровно обратный и очень
// сильный: набор ключей выглядит как подстройка производительности, которую
// «разумнее» оставить на умолчаниях обеих сторон. Такая правка ломает узлы,
// причём не сразу и не все.
//
// Ключевое поле — xPaddingBytes. Сервер Xray ПРОВЕРЯЕТ длину паддинга на
// каждом запросе, до разбора режима и сессии, и на несовпадение отвечает
// 400 (transport/internet/splithttp/hub.go:143-150; IsPaddingValid в
// xpadding.go:307-333 даёт false и при пустом значении, и при выходе за
// диапазон). Клиент mihomo при отсутствии ключа берёт СВОЁ умолчание
// "100-1000" (transport/xhttp/xpadding.go:181).
//
// В снятой подписке у 18 из 21 xhttp-outbound стоит "50-150": диапазоны
// пересекаются только на 100–150, то есть около 94% запросов получают 400.
// Узел при этом не «мёртв», а «то работает, то нет» — самый дорогой в
// поиске вид отказа. Ещё у двух узлов стоит "0-0", то есть паддинга нет
// вовсе, и умолчание mihomo ломает их полностью.
//
// Второе: обфускация. У одного wl-узла сессия и счётчик уезжают в cookies, а
// паддинг — в query-параметр заголовка Referer. Умолчание обеих сторон —
// path. Без переноса сервер не найдёт ни паддинга, ни сессии, и узел мёртв
// на все сто процентов.
//
// Отображение сверено по adapter/outbound/vless.go:81-105 mihomo v1.19.27.

// xhttpExtraKeys — таблица переноса, СПИСКОМ, а не картой.
//
// Порядок здесь несёт смысл и обязан быть фиксированным: два разных ключа
// Xray (sessionPlacement и sessionIDPlacement) отображаются в один ключ
// mihomo, и обход карты — со случайным порядком в Go — давал бы разный
// результат от запуска к запуску. Побеждает первый непустой, то есть
// написание без ID. В снятой подписке оба написания несут одно и то же
// значение, так что сегодня выбор невидим; правило существует ради того,
// чтобы завтрашнее расхождение разрешалось одинаково каждый раз, а не
// подбрасыванием монеты.
var xhttpExtraKeys = []struct{ from, to string }{
	{"xPaddingBytes", "x-padding-bytes"},
	{"xPaddingObfsMode", "x-padding-obfs-mode"},
	{"xPaddingKey", "x-padding-key"},
	{"xPaddingHeader", "x-padding-header"},
	{"xPaddingPlacement", "x-padding-placement"},
	{"xPaddingMethod", "x-padding-method"},

	{"sessionPlacement", "session-placement"},
	{"sessionIDPlacement", "session-placement"},
	{"sessionKey", "session-key"},
	{"sessionIDKey", "session-key"},

	{"seqPlacement", "seq-placement"},
	{"seqKey", "seq-key"},

	{"uplinkHTTPMethod", "uplink-http-method"},
	{"uplinkDataPlacement", "uplink-data-placement"},
	{"uplinkDataKey", "uplink-data-key"},
	{"uplinkChunkSize", "uplink-chunk-size"},

	{"scMaxEachPostBytes", "sc-max-each-post-bytes"},
	{"scMinPostsIntervalMs", "sc-min-posts-interval-ms"},

	{"noGRPCHeader", "no-grpc-header"},
	{"headers", "headers"},
}

// xmuxKeys — вложенный блок xmux, он же reuse-settings у mihomo.
var xmuxKeys = []struct{ from, to string }{
	{"cMaxReuseTimes", "c-max-reuse-times"},
	{"maxConcurrency", "max-concurrency"},
	{"maxConnections", "max-connections"},
	{"hMaxRequestTimes", "h-max-request-times"},
	{"hMaxReusableSecs", "h-max-reusable-secs"},
	{"hKeepAlivePeriod", "h-keep-alive-period"},
}

// Ключи, которые НЕ переносятся, — перечислены поимённо намеренно.
//
// Это серверные параметры: у outbound-а mihomo аналога им нет, и попытка
// подставить их в профиль даст отказ разбора конфига вместо узла. Список
// нужен читателю, чтобы отличить «исключено сознательно» от «не дошли руки»:
//
//	scMaxBufferedPosts, scStreamUpServerSecs — размер буфера и окно на
//	    стороне сервера;
//	noSSEHeader — поведение серверного ответа;
//	sessionIDTable, sessionIDLength — алфавит и длина идентификатора,
//	    которые генерирует сервер.
//
// Всё, чего нет ни в таблице выше, ни в этом перечне, тоже не переносится:
// имя ключа у mihomo нам неизвестно, а выдуманное имя — это отказ разбора
// всего файла провайдера ради одного узла. Но молча — нельзя: такой ключ
// возвращается наверх и доходит до журнала демона (Summary.UnknownKeys).
// Провайдер, переименовавший xPaddingBytes, дал бы узел «то работает, то
// нет» без единой зацепки; одна строка в журнале — вся диагностика, какая
// у владельца в этом случае есть.
var xhttpServerOnlyKeys = map[string]bool{
	"scMaxBufferedPosts":   true,
	"scStreamUpServerSecs": true,
	"noSSEHeader":          true,
	"sessionIDTable":       true,
	"sessionIDLength":      true,
}

// copyXHTTPExtra переносит extra в xhttp-opts и возвращает непустые ключи,
// которых не знает ни таблица переноса, ни перечень серверных.
func copyXHTTPExtra(opts map[string]any, extra map[string]any) (unknown []string) {
	known := map[string]bool{"xmux": true}
	for _, k := range xhttpExtraKeys {
		known[k.from] = true
		v, ok := extra[k.from]
		if !ok || isBlank(v) {
			continue
		}
		// Первый непустой побеждает: см. про sessionPlacement выше.
		if _, taken := opts[k.to]; taken {
			continue
		}
		opts[k.to] = v
	}
	for k, v := range extra {
		if known[k] || xhttpServerOnlyKeys[k] || isBlank(v) {
			continue
		}
		unknown = append(unknown, k)
	}

	xmux, _ := extra["xmux"].(map[string]any)
	reuse := map[string]any{}
	for _, k := range xmuxKeys {
		v, ok := xmux[k.from]
		if !ok || isBlank(v) {
			continue
		}
		reuse[k.to] = v
	}
	// Пустой reuse-settings не заводим: у mihomo это не «настроек нет», а
	// объект настроек, и пустой объект он разбирает по-своему.
	if len(reuse) > 0 {
		opts["reuse-settings"] = reuse
	}
	return unknown
}

// isBlank сообщает, что значение — нулевое для своего типа и переносить его
// незачем.
//
// Отсутствие ключа и явно записанное значение для mihomo разные вещи, но
// нулевое значение — это и есть его умолчание: "" не задаёт ничего, false и
// 0 совпадают с тем, что движок подставит сам. Записать их значило бы
// засорить профиль строками без смысла.
//
// Ловушка, ради которой эта функция вообще смотрит на тип: xPaddingBytes
// приходит СТРОКОЙ, и "0-0" — это осмысленное «паддинга нет», а не ноль.
// Проверка на нулевое ЧИСЛО его не заденет, и так и задумано: у двух узлов
// подписки стоит именно "0-0", и потеря этой строки включила бы им чужое
// умолчание "100-1000" — то есть 400 на каждом запросе.
func isBlank(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return t == ""
	case bool:
		return !t
	case float64:
		return t == 0
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	default:
		return false
	}
}
