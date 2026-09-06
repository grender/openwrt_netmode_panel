// Package nikki — клиент Clash API у mihomo.
//
// Файл /etc/nikki/run/config.yaml не читается и не пишется НИКОГДА: он
// генерируется из профиля, и любая наша правка будет затёрта (SPEC §12).
//
// Контракт: docs/recon/nikki.md, снят с живого роутера
// (raw/60-clash-proxies.json, raw/61-clash-groups.json).
package nikki

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	probeTimeout = 2 * time.Second
	callTimeout  = 5 * time.Second
)

// ErrUnavailable — Clash API не отвечает или отвергает секрет.
var ErrUnavailable = errors.New("nikki: Clash API недоступен")

// ErrNotSelectable — движок отказался принимать ручной выбор.
//
// mihomo приводит цель к интерфейсу SelectAble (hub/route/proxies.go) и
// при несовпадении отвечает 400 «Must be a Selector». Selector, URLTest и
// Fallback его реализуют, поэтому на практике эта ошибка означает попытку
// выбрать участника у обычного узла, а не у группы.
var ErrNotSelectable = errors.New("nikki: группа не допускает ручной выбор")

// ErrNotFound — группы или узла нет.
var ErrNotFound = errors.New("nikki: не найдено")

// ErrProbeFailed — узел не ответил на пробу задержки.
//
// Отдельно от ErrUnavailable: там не отвечает САМ движок и панели показывать
// нечего вовсе, здесь движок ответил исправно и сообщил, что конкретный узел
// мёртв. Смешать их значило бы объявить недоступным Clash API всякий раз,
// когда в подписке протух один сервер.
var ErrProbeFailed = errors.New("nikki: узел не ответил на пробу")

// ErrProviderStale — файл провайдера записан, а движок его не принял.
//
// Отдельно от ErrUnavailable, хотя mihomo отвечает пятисоткой: движок жив и
// внятно сказал, что перечитать провайдера не смог (hub/route/provider.go,
// updateProvider зовёт provider.Update()). Состояние при этом расходится —
// на диске новый список узлов, в памяти движка старый, — и владельцу это
// надо сказать прямо. Записав такое в «Clash API недоступен», мы отправили
// бы его перезапускать nikki, тогда как чинить надо содержимое файла.
var ErrProviderStale = errors.New("nikki: провайдер записан, но движком не перечитан")

// StatusError — движок ответил кодом, который разбирается по смыслу вызова.
//
// Тип, а не форматированная строка: смысл 400 у mihomo зависит от маршрута
// («Must be a Selector» у PUT, «нечего снимать» у DELETE), и определять его
// поиском подстроки в тексте ошибки значит поставить HTTP-код панели
// в зависимость от формулировки сообщения — она тихо разъедется при первой же
// правке текста.
type StatusError struct {
	Code int
	Path string
	// Message — текст из тела ответа {"message": …}. У mihomo это
	// единственное место, где названа ПРИЧИНА отказа: у /providers/proxies
	// 503 несёт «proxy 3 error: invalid REALITY public key» или «open …:
	// permission denied», и без этого поля оба случая для владельца
	// выглядят одинаково — «движок отверг». Пусто, если тела нет или оно
	// не той формы.
	Message string
}

func (e *StatusError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("nikki: %s: код %d: %s", e.Path, e.Code, e.Message)
	}
	return fmt.Sprintf("nikki: %s: код %d", e.Path, e.Code)
}

// errBodyLimit — сколько байт тела ошибки читать. Форма тела — одна строка
// message; больше килобайта здесь не бывает, а потолок нужен по той же
// причине, что в ADR-0022: чужой сервер не должен решать, сколько памяти
// займёт наш разбор его отказа.
const errBodyLimit = 4 << 10

// errMessage достаёт message из тела ошибки Clash API. Любая неудача —
// пустая строка: тело ошибки объясняет, а не решает, и терять код ответа
// из-за кривого тела нельзя.
func errMessage(r io.Reader) string {
	var body struct {
		Message string `json:"message"`
	}
	if err := json.NewDecoder(io.LimitReader(r, errBodyLimit)).Decode(&body); err != nil {
		return ""
	}
	return strings.TrimSpace(body.Message)
}

// Proxy — узел или группа.
type Proxy struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Alive говорит о последней УСПЕШНОЙ пробе, а не о последней вообще.
	Alive bool `json:"alive"`
	// DelayMS — задержка последней пробы. nil означает «пробы не было».
	//
	// Именно nil, а не 0: mihomo пишет в историю delay:0 при несостоявшейся
	// пробе (raw/60-clash-proxies.json), и превратить это в «ноль
	// миллисекунд» значило бы показать мёртвый узел самым быстрым.
	DelayMS *int `json:"delay_ms"`
	// Members заполнено только у групп.
	Members []string `json:"members,omitempty"`
	// Now — участник, через который группа работает прямо сейчас.
	Now string `json:"now,omitempty"`
	// Fixed — участник, закреплённый вручную. Пусто означает автовыбор.
	//
	// В mihomo это поле `selected` группы; при непустом значении автоподбор
	// в fast() обходится (adapter/outboundgroup/urltest.go). Именно оно
	// отличает «выбрано руками» от «движок так решил» — по одному Now это
	// неразличимо.
	Fixed string `json:"fixed,omitempty"`
	// Pinned — закреплён ли выбор вручную.
	Pinned bool `json:"pinned"`
	// Selectable — принимает ли группа ручной выбор.
	Selectable bool `json:"selectable"`
}

// IsGroup сообщает, группа ли это.
func (p Proxy) IsGroup() bool { return len(p.Members) > 0 }

// Client — интерфейс для тестируемости обработчиков.
type Client interface {
	Version(ctx context.Context) (string, error)
	Proxies(ctx context.Context) (map[string]Proxy, error)
	Select(ctx context.Context, group, member string) error
	Unfix(ctx context.Context, group string) error
	// ReloadProvider — в интерфейсе, потому что обновление подписки без
	// него не заканчивается: файл на диске новый, а список в панели старый.
	// Проверить, что демон действительно зовёт перезагрузку после записи,
	// можно только на подменённом клиенте.
	ReloadProvider(ctx context.Context, name string) error
	// ProviderProxies — имена узлов провайдера у движка. В интерфейсе по
	// той же причине, что ReloadProvider: без него проверка «файл дошёл до
	// движка» существовала бы только на живом роутере.
	ProviderProxies(ctx context.Context, name string) ([]string, error)
	// Delay — проба задержки одного узла. В интерфейсе, потому что замер
	// пачки (ProbeAll) написан против интерфейса: иначе поведение при
	// частичном отказе — половина узлов мертва, бюджет вышел — проверялось
	// бы только на живом роутере, то есть никогда.
	Delay(ctx context.Context, name string) (int, error)
	// PanelAlive — проба статики дашборда (panel.go). В интерфейсе, а не
	// только у HTTP: без неё обработчик не смог бы проверить панель на
	// подменённом клиенте, и единственный путь, отдающий секрет наружу,
	// остался бы без теста.
	PanelAlive(ctx context.Context) error
}

// HTTP — реальный клиент.
type HTTP struct {
	BaseURL string
	Secret  string
	client  *http.Client
}

func New(baseURL, secret string) *HTTP {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:9090"
	}
	return &HTTP{
		BaseURL: baseURL,
		Secret:  secret,
		client: &http.Client{
			Transport: &http.Transport{
				MaxIdleConns:        2,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     30 * time.Second,
			},
		},
	}
}

// do выполняет запрос.
//
// Секрет идёт заголовком Authorization: Bearer. Параметр ?secret= в строке
// запроса НЕ работает — проверено на живом роутере, отвечает Unauthorized
// (docs/recon/nikki.md). Он предназначен веб-морде LuCI, которая уже сама
// кладёт значение в заголовок.
func (c *HTTP) do(ctx context.Context, method, path string, body, out any, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var rdr *jsonReader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("nikki: сборка тела: %w", err)
		}
		rdr = newJSONReader(b)
	} else {
		rdr = newJSONReader(nil)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rdr)
	if err != nil {
		return fmt.Errorf("nikki: запрос: %w", err)
	}
	if c.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.Secret)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode == http.StatusUnauthorized:
		// Неверный секрет — для панели это та же недоступность: подбирать
		// его мы не станем, а чинится он правкой UCI.
		return fmt.Errorf("%w: секрет отвергнут", ErrUnavailable)
	case resp.StatusCode >= 500:
		// Код едет вместе с ошибкой, а не только в её тексте: у
		// /providers/proxies 503 означает «файл записан, движок его отверг»
		// и от прочих пятисоток отличается ровно числом. Двойной %w
		// оставляет ошибку недоступностью для всех, кто её так и разбирает
		// (errors.Is), и одновременно даёт добраться до кода (errors.As) —
		// поиск подстроки «код 503» в тексте разъехался бы при первой же
		// правке формулировки, ровно как это уже было с «код 400».
		return fmt.Errorf("%w: %w", ErrUnavailable,
			&StatusError{Code: resp.StatusCode, Path: path, Message: errMessage(resp.Body)})
	case resp.StatusCode >= 400:
		return &StatusError{Code: resp.StatusCode, Path: path, Message: errMessage(resp.Body)}
	}

	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("nikki: разбор ответа %s: %w", path, err)
	}
	return nil
}

// Version — проба живости.
func (c *HTTP) Version(ctx context.Context) (string, error) {
	var v struct {
		Meta    bool   `json:"meta"`
		Version string `json:"version"`
	}
	if err := c.do(ctx, http.MethodGet, "/version", nil, &v, probeTimeout); err != nil {
		return "", err
	}
	return v.Version, nil
}

// wireProxy повторяет форму ответа mihomo.
type wireProxy struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Alive   bool     `json:"alive"`
	All     []string `json:"all"`
	Now     string   `json:"now"`
	Fixed   string   `json:"fixed"`
	History []struct {
		Time  string `json:"time"`
		Delay int    `json:"delay"`
	} `json:"history"`
}

// Proxies возвращает узлы и группы.
//
// Корень ответа — объект, а не массив: {"proxies": {"имя": {...}}}.
func (c *HTTP) Proxies(ctx context.Context) (map[string]Proxy, error) {
	var wire struct {
		Proxies map[string]wireProxy `json:"proxies"`
	}
	if err := c.do(ctx, http.MethodGet, "/proxies", nil, &wire, callTimeout); err != nil {
		return nil, err
	}

	out := make(map[string]Proxy, len(wire.Proxies))
	for name, w := range wire.Proxies {
		p := Proxy{
			Name:    name,
			Type:    w.Type,
			Alive:   w.Alive,
			Members: w.All,
			Now:     w.Now,
			Fixed:   w.Fixed,
			Pinned:  w.Fixed != "",
			// Ручной выбор принимает любая группа: mihomo проверяет
			// интерфейс SelectAble, а не конкретный тип, и Selector,
			// URLTest и Fallback его реализуют.
			Selectable: len(w.All) > 0,
			DelayMS:    lastDelay(w),
		}
		if p.Name == "" {
			p.Name = w.Name
		}
		out[name] = p
	}
	return out, nil
}

// lastDelay достаёт задержку последней пробы.
//
// Значение 0 в истории означает НЕСОСТОЯВШУЮСЯ пробу, а не нулевую
// задержку (raw/60-clash-proxies.json). Возвращаем nil, чтобы панель
// показала прочерк, а не покрасила мёртвый узел зелёным как самый быстрый.
func lastDelay(w wireProxy) *int {
	for i := len(w.History) - 1; i >= 0; i-- {
		if d := w.History[i].Delay; d > 0 {
			v := d
			return &v
		}
		// Ноль — проба не прошла; ищем дальше вглубь истории только если
		// это последняя запись подряд? Нет: последняя проба и есть текущее
		// состояние. Ноль в последней записи означает «сейчас не отвечает».
		break
	}
	return nil
}

// Select закрепляет участника группы.
//
// Работает и для URLTest: mihomo хранит закреплённый узел в поле selected
// и обходит автоподбор, пока оно непусто. Правка профиля для этого не
// нужна — вопреки тому, что предполагает SPEC §7.
//
// Закрепление считается временным действием: возврат к автовыбору —
// Unfix, и панель обязана держать его на виду.
func (c *HTTP) Select(ctx context.Context, group, member string) error {
	proxies, err := c.Proxies(ctx)
	if err != nil {
		return err
	}
	g, ok := proxies[group]
	if !ok {
		return ErrNotFound
	}
	if !g.IsGroup() {
		return fmt.Errorf("%w: %q — узел, а не группа", ErrNotFound, group)
	}
	found := false
	for _, m := range g.Members {
		if m == member {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("%w: %q не входит в группу %q", ErrNotFound, member, group)
	}

	body := map[string]string{"name": member}
	err = c.do(ctx, http.MethodPut, "/proxies/"+url.PathEscape(group), body, nil, callTimeout)
	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusBadRequest {
		// mihomo отвечает 400 «Must be a Selector», когда цель не
		// реализует SelectAble (hub/route/proxies.go).
		return fmt.Errorf("%w: %q имеет тип %s", ErrNotSelectable, group, g.Type)
	}
	return err
}

// Unfix снимает ручное закрепление и возвращает группу к автовыбору.
//
// Это и есть пункт AUTO, которого требует SPEC §7: в mihomo он встроен —
// DELETE /proxies/{группа} вызывает ForceSet(""), и работает он только для
// НЕ-Selector групп, то есть ровно для URLTest и Fallback
// (hub/route/proxies.go, unfixedProxy).
//
// Для обычного Selector автовыбора не существует, поэтому там вызов
// вернёт ErrNotSelectable — и это честно: снимать нечего.
func (c *HTTP) Unfix(ctx context.Context, group string) error {
	proxies, err := c.Proxies(ctx)
	if err != nil {
		return err
	}
	g, ok := proxies[group]
	if !ok {
		return ErrNotFound
	}
	if !g.IsGroup() {
		return fmt.Errorf("%w: %q — узел, а не группа", ErrNotFound, group)
	}

	err = c.do(ctx, http.MethodDelete, "/proxies/"+url.PathEscape(group), nil, nil, callTimeout)
	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusBadRequest {
		return fmt.Errorf("%w: у группы %q типа %s автовыбора нет",
			ErrNotSelectable, group, g.Type)
	}
	return err
}

// ReloadProvider велит движку перечитать файл провайдера с диска.
//
// Без этого вызова запись файла не значит ничего видимого: mihomo держит
// список узлов в памяти и сам перечитывает провайдера по своему interval,
// то есть когда-нибудь в ближайшие часы. Обновление подписки, после
// которого в панели те же узлы, что и до него, владелец справедливо
// прочтёт как «кнопка сломана».
//
// PUT /providers/proxies/{имя}, успех — 204 без тела (hub/route/provider.go,
// updateProvider). Имя приходит из нашей же конфигурации, но экранируется
// как элемент пути: незакодированный слэш увёл бы запрос на другой маршрут,
// и вместо отказа мы получили бы тихое «204» от чего-то постороннего.
//
// Три исхода разделены намеренно:
//
//	404 — провайдера с таким именем в движке нет (ErrNotFound): либо имя
//	      разошлось с профилем, либо файл ещё ни разу не подхватывался;
//	503 — движок провайдера знает, но обновиться не смог (ErrProviderStale);
//	сеть — движка нет вовсе (ErrUnavailable), как у всех прочих вызовов.
func (c *HTTP) ReloadProvider(ctx context.Context, name string) error {
	path := "/providers/proxies/" + url.PathEscape(name)
	err := c.do(ctx, http.MethodPut, path, nil, nil, callTimeout)

	var se *StatusError
	if errors.As(err, &se) && se.Code == http.StatusServiceUnavailable {
		// StatusError едет в цепочке вторым %w: его текст несёт причину из
		// тела ответа — единственный след того, ЧТО именно не понравилось
		// движку. Раньше она выбрасывалась, и «proxy 3 error: …» и
		// «permission denied» для владельца были неотличимы. Через errors.As
		// сообщение доступно и машине.
		return fmt.Errorf("%w: файл провайдера %q уже новый, а список узлов "+
			"в mihomo остался старым — движок отверг перечитывание (%w)",
			ErrProviderStale, name, se)
	}
	return err
}

// ProviderProxies — имена узлов, которые движок держит в провайдере СЕЙЧАС.
//
// Нужен для одного: проверить после перечитывания, что записанный файл
// вообще дошёл до движка. PUT отвечает 204 и тогда, когда прочитанный файл
// не изменился (fetcher сверяет хэш и молча выходит), и тогда, когда имена
// переписаны override-ом провайдера, — в обоих случаях «успех» без единого
// нашего узла. Сверять список — единственный способ отличить это от успеха.
func (c *HTTP) ProviderProxies(ctx context.Context, name string) ([]string, error) {
	var v struct {
		Proxies []struct {
			Name string `json:"name"`
		} `json:"proxies"`
	}
	path := "/providers/proxies/" + url.PathEscape(name)
	if err := c.do(ctx, http.MethodGet, path, nil, &v, callTimeout); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(v.Proxies))
	for _, p := range v.Proxies {
		names = append(names, p.Name)
	}
	return names, nil
}

// delayProbeTimeout — сколько mihomo ждёт ответа узла.
//
// 1,5 с, а не 3 с из команды разведки: замер идёт по ВСЕМ участникам группы
// (на живом роутере их 26), и худший случай складывается из мёртвых узлов —
// каждый занимает ровно timeout целиком. Панель ждёт ответа синхронно, так
// что цена медленного замера — не «дольше», а «оборвалось таймаутом клиента,
// и владелец не узнал ничего».
const delayProbeTimeout = 1500 * time.Millisecond

// delayTestURL — цель пробы, ровно та, которой снимался RQ-06.
//
// Адрес запрашивается ЧЕРЕЗ проверяемый узел, поэтому «gstatic заблокирован
// напрямую» здесь не довод: способность дотянуться до него в обход блокировки
// и есть то, что мы измеряем.
const delayTestURL = "https://www.gstatic.com/generate_204"

// Delay замеряет задержку одного узла и попутно обновляет history у mihomo.
//
// Форма ответа измерена (RQ-06, живой роутер): РОВНО одно поле {"delay":25}.
// Несуществующее имя даёт непустое тело {"message":"Resource not found"} и
// код не 200 — какой именно, разведка не записала, поэтому здесь на код никто
// не смотрит: любой неуспех означает «про этот узел мы ничего не узнали», и
// различать причины нам не для чего.
//
// Групповой эндпоинт /group/{имя}/delay НЕ ИСПОЛЬЗУЕТСЯ: его форма не
// снималась. А /proxies/{группа}/delay отдаёт задержку выбранного члена, а не
// всех, — обновить всех можно только вызовом по каждому имени.
func (c *HTTP) Delay(ctx context.Context, name string) (int, error) {
	q := url.Values{
		"timeout": {strconv.Itoa(int(delayProbeTimeout / time.Millisecond))},
		"url":     {delayTestURL},
	}
	path := "/proxies/" + url.PathEscape(name) + "/delay?" + q.Encode()

	var out struct {
		Delay int `json:"delay"`
	}
	// Свой дедлайн заведомо больше того, что просим у движка: иначе «узел не
	// ответил» и «мы не дождались самого mihomo» слились бы в одну ошибку,
	// и первое молча записалось бы во второе.
	if err := c.do(ctx, http.MethodGet, path, nil, &out, delayProbeTimeout+time.Second); err != nil {
		return 0, err
	}
	if out.Delay <= 0 {
		// Нуля в успешном ответе на роутере не наблюдалось. Проверка стоит
		// потому, что ноль в history у mihomo означает несостоявшуюся пробу
		// (см. lastDelay), и отдать его как «0 мс» — ровно тот дефект,
		// который там уже чинился: мёртвый узел выглядит самым быстрым.
		return 0, fmt.Errorf("%w: %q вернул delay=%d", ErrProbeFailed, name, out.Delay)
	}
	return out.Delay, nil
}

const (
	// probeParallel — сколько узлов проверяется одновременно.
	//
	// Каждая проба — настоящий запрос наружу через свой узел, поэтому число
	// ограничивает не CPU роутера, а желание не устраивать себе всплеск из
	// 26 туннелей разом.
	probeParallel = 6
	// probeBudget — общий потолок замера.
	//
	// Он тут не «на всякий случай»: 26 мёртвых узлов по 1,5 с при шестерых
	// работниках дали бы около 7 с, и это ещё до чтения /proxies. Панель
	// ждёт синхронно, и обрыв по её таймауту выглядел бы как «кнопка сломана».
	// Лучше вернуть честное «столько-то не успели».
	probeBudget = 9 * time.Second
)

// ProbeSummary — что на самом деле случилось при замере пачки узлов.
//
// Тип существует ради Skipped и Failed. Без них ответ «замер прошёл» ничем не
// отличается от «ни один узел не ответил, и мы промолчали»: числа задержек
// панель показывает и без всякого замера (mihomo сам обновляет history раз в
// несколько минут, RQ-06), так что по одному только списку узлов понять,
// сработала кнопка или нет, нельзя в принципе.
type ProbeSummary struct {
	Total     int `json:"total"`
	Measured  int `json:"measured"`
	Failed    int `json:"failed"`
	Skipped   int `json:"skipped"`
	ElapsedMS int `json:"elapsed_ms"`
}

// ProbeAll замеряет узлы по именам, не больше probeParallel одновременно.
//
// Отказ одного узла не прерывает остальных — в этом весь смысл вызова:
// подписка на два десятка серверов, часть из которых заведомо мертва, и
// «первый мёртвый обрывает замер» означало бы, что кнопка бесполезна ровно
// тогда, когда нужна.
//
// Ошибок не возвращает намеренно: неуспех отдельного узла — это результат
// замера, а не сбой операции. Единственное, что вызывающий обязан проверить,
// — Measured: ноль при непустом Total значит, что наружу не выбрался никто.
func ProbeAll(ctx context.Context, c Client, names []string) ProbeSummary {
	start := time.Now()
	sum := ProbeSummary{Total: len(names)}
	if len(names) == 0 {
		return sum
	}

	ctx, cancel := context.WithTimeout(ctx, probeBudget)
	defer cancel()

	workers := probeParallel
	if workers > len(names) {
		workers = len(names)
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	next := 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				mu.Lock()
				if next >= len(names) {
					mu.Unlock()
					return
				}
				name := names[next]
				next++
				mu.Unlock()

				var err error
				if ctx.Err() == nil {
					_, err = c.Delay(ctx, name)
				} else {
					err = ctx.Err()
				}

				mu.Lock()
				switch {
				case err == nil:
					sum.Measured++
				case ctx.Err() != nil:
					// Бюджет кончился — про узел мы не узнали ничего, и
					// записать это в «не ответил» значило бы оболгать узел
					// собственным таймаутом.
					sum.Skipped++
				default:
					sum.Failed++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	sum.ElapsedMS = int(time.Since(start) / time.Millisecond)
	return sum
}

// Groups возвращает только группы, в стабильном порядке имён.
func Groups(all map[string]Proxy) []Proxy {
	var out []Proxy
	for _, p := range all {
		if p.IsGroup() {
			out = append(out, p)
		}
	}
	sortByName(out)
	return out
}

// Members разворачивает участников группы в узлы.
//
// Участники, которых нет в ответе, пропускаются: это разделители вроде
// «⬇️ Обходы белых списков ⬇️», которые провайдер кладёт в подписку.
func Members(all map[string]Proxy, group string) []Proxy {
	g, ok := all[group]
	if !ok {
		return nil
	}
	out := make([]Proxy, 0, len(g.Members))
	for _, name := range g.Members {
		if p, ok := all[name]; ok {
			out = append(out, p)
		}
	}
	return out
}

func sortByName(ps []Proxy) {
	for i := 1; i < len(ps); i++ {
		for j := i; j > 0 && ps[j].Name < ps[j-1].Name; j-- {
			ps[j], ps[j-1] = ps[j-1], ps[j]
		}
	}
}
