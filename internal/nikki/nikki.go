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
	"net/http"
	"net/url"
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
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("nikki: %s: код %d", e.Path, e.Code)
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
		return fmt.Errorf("%w: код %d", ErrUnavailable, resp.StatusCode)
	case resp.StatusCode >= 400:
		return &StatusError{Code: resp.StatusCode, Path: path}
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
