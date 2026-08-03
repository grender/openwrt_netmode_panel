package nikki

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
)

// PanelPath — путь, по которому mihomo отдаёт статику дашборда.
//
// Подтверждено разведкой (raw/73-nikki-ui-probe.txt): `external-ui: ui` в
// /etc/nikki/run/config.yaml разворачивается в путь /ui/, статика отдаётся
// с кодом 200 БЕЗ единого заголовка авторизации, дашборд — zashboard.
// Отдельного порта у панели нет: `external-controller: '[::]:9090'` —
// единственный слушатель, то есть панель живёт на порту Clash API.
//
// Литерал стоит в internal/nikki, а не в обработчике, намеренно.
// scripts/check-evidence.sh сканирует клиентские пакеты и НЕ заглядывает в
// internal/httpapi (там наши собственные маршруты, их сверяет check-routes).
// Тот же «/ui/», написанный в обработчике, был бы внешним путём без
// механической защиты — единственным таким в проекте.
const PanelPath = "/ui/"

// ErrPanelMissing — по PanelPath не оказалось статики.
//
// Отдельная ошибка, а не ErrUnavailable: Clash API может отвечать прекрасно,
// а дашборд быть не скачан (external-ui-url тянет zashboard с GitHub при
// первом запуске, и без интернета этого не происходит). Для владельца это
// разные починки: «поднять nikki» против «докачать панель».
var ErrPanelMissing = errors.New("nikki: панель не отвечает")

// PanelPort достаёт порт Clash API из базового адреса.
//
// Разбором, а не вторым литералом: адрес выводится из nikki.mixin.api_listen
// при старте демона, и порт обязан приезжать оттуда же. Второй литерал
// разошёлся бы с конфигурацией роутера молча — ссылка вела бы в никуда
// ровно у того владельца, который порт менял.
func PanelPort(baseURL string) (string, bool) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", false
	}
	p := u.Port()
	if p == "" {
		return "", false
	}
	return p, true
}

// PanelBaseURL — адрес панели без параметров: то, КУДА идти.
//
// Именно он уезжает в /api/status: секрету незачем ездить в ответе, который
// панель опрашивает раз в секунду. У ссылки при этом остаётся настоящий
// href — работают средний клик, «открыть в новой вкладке», «копировать
// адрес», — а секрет добавляет отдельный запрос по клику (PanelURL).
//
// Хост берётся из заголовка Host запроса, а НЕ из адреса Clash API: там
// стоит 127.0.0.1, по которому ходит демон. В браузере владельца петля
// указывает на его собственный ноутбук.
func PanelBaseURL(host, port string) (string, bool) {
	if host == "" || port == "" {
		return "", false
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(host, port), Path: PanelPath}
	return u.String(), true
}

// PanelURL — полный адрес дашборда вместе с секретом.
//
// Форма снята с самого LuCI (raw/71-luci-nikki-open-dashboard.txt,
// tools/nikki.js openDashboard):
//
//	http://<хост>:<порт>/ui/?host=…&hostname=…&port=…&secret=…
//
// Четыре параметра и именно query, а не hash. `host` и `hostname` дублируют
// друг друга не по недосмотру: разные дашборды читают разное имя, и LuCI
// кладёт оба. Схема http, потому что api_tls_listen на снимке не задан;
// подкаталога после /ui/ нет, потому что не задан ui_name.
//
// Это НЕ противоречит записи «?secret= не работает» в docs/recon/nikki.md:
// там секрет предъявляли самому Clash API, который принимает только
// заголовок Authorization. Здесь параметр уходит статике дашборда, а она уже
// сама кладёт значение в заголовок при обращении к API.
//
// Сборка идёт через url.Values, а не конкатенацией: секрет — произвольная
// строка из UCI, и знак `&`, пробел или непечатный символ в ней порвали бы
// склеенный руками адрес, причём тихо — браузер получил бы усечённый секрет
// и молча не вошёл бы.
func PanelURL(host, port, secret string) (string, bool) {
	if host == "" || port == "" {
		return "", false
	}
	q := url.Values{}
	q.Set("host", host)
	q.Set("hostname", host)
	q.Set("port", port)
	q.Set("secret", secret)
	u := url.URL{
		Scheme:   "http",
		Host:     net.JoinHostPort(host, port),
		Path:     PanelPath,
		RawQuery: q.Encode(),
	}
	return u.String(), true
}

// PanelAlive — живая проба статики дашборда по петле.
//
// Нужна перед тем, как отдать адрес с секретом. Без неё секрет уезжает в
// адресную строку браузера — и оседает в истории, в автодополнении и в
// синхронизации профиля — ради страницы 404. Проба локальная и дешёвая,
// делается только по клику владельца, поэтому не кэшируется.
//
// Заголовок Authorization НЕ ставится намеренно: проверяем ровно то, что
// сделает браузер, а он заголовков не носит. Разведка говорит, что статика
// авторизации не требует (raw/73); если это когда-нибудь изменится, отказ
// придёт здесь, а не в виде пустой страницы у владельца.
func (c *HTTP) PanelAlive(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+PanelPath, nil)
	if err != nil {
		return ErrUnavailable
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	// Тело дочитывается с потолком: соединение переиспользуется только после
	// полного чтения, а index.html дашборда — полтора килобайта. Потолок на
	// случай, если по этому пути когда-нибудь окажется не он.
	_, _ = io.CopyN(io.Discard, resp.Body, 64<<10)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return ErrPanelMissing
	}
	return nil
}
