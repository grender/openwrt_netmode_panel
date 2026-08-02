// Package wireless выводит состояние внешнего канала из конфигурации UCI
// и живого ubus.
//
// Ничего не хранит. Какая сеть выбрана — вычисляемая величина: она выводится
// из опции `disabled` при каждом запросе (ADR-0004). Хранение копии означало
// бы, что она разъедется в первый же раз, когда владелец откроет LuCI.
//
// Все функции здесь чистые: вход — байты, выход — структура. Это позволяет
// проверять их на записанном выводе живого роутера (docs/recon/raw/) вообще
// без моков; рукописный мок проверял бы нашу же догадку саму против себя.
package wireless

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"

	"netmoded/internal/uci"
)

// State — состояние выбора внешней сети (docs/contracts/state-machine.md).
type State string

const (
	// Empty — станционных секций нет вовсе. Свежая установка выглядит
	// именно так, поэтому это НЕ ошибка.
	Empty State = "empty"
	// AllDisabled — сети сохранены, ни одна не включена. Тоже норма:
	// владелец отключил сеть в LuCI и не выбрал новую.
	AllDisabled State = "all_disabled"
	// Single — рабочее состояние: ровно одна включена.
	Single State = "single"
	// Ambiguous — включено ≥2 либо значение disabled не распознано.
	// Запись запрещена целиком, автопочинки нет (ADR-0010).
	Ambiguous State = "ambiguous"
)

// Selection — результат классификации.
type Selection struct {
	State    State
	Active   string   // имя секции при Single, иначе ""
	Sections []string // все наши секции в порядке файла
	Conflict []string // при Ambiguous — секции, вызвавшие конфликт
	Reason   string   // при Ambiguous — что именно не так, для панели
}

// Network — сохранённая сеть в том виде, в каком она уходит наружу.
//
// Поля с паролем здесь нет намеренно: наружу идёт только факт его наличия
// (ADR-0012). Структура без поля не может его случайно сериализовать.
type Network struct {
	// ID — имя секции UCI. Указатель, потому что у анонимной секции
	// идентификатора НЕТ, и `null` — единственное честное значение
	// (ADR-0005). Пустая строка на его месте лгала бы дважды: в JS она
	// ложно-истинна, а `encodeURIComponent("")` склеивается в валидный
	// путь — то есть «нет идентификатора» доехало бы до DELETE как адрес.
	ID         *string `json:"id"`
	SSID       string  `json:"ssid"`
	Encryption string  `json:"encryption"`
	HasKey     bool    `json:"has_key"`
	Network    string  `json:"network"`
	Enabled    bool    `json:"enabled"`
	Editable   bool    `json:"editable"`
}

// ours сообщает, наша ли это секция: станция на нужном радио.
// Всё прочее — включая домашнюю точку доступа — вне модели.
func ours(s uci.Section, radio string) bool {
	return s.Type == "wifi-iface" &&
		s.Options["device"] == radio &&
		s.Options["mode"] == "sta"
}

// Classify вычисляет состояние выбора.
func Classify(c *uci.Config, radio string) Selection {
	sel := Selection{State: Empty}
	if c == nil {
		return sel
	}

	var enabled, unparseable []string

	for _, s := range c.Sections {
		if !ours(s, radio) {
			continue
		}
		sel.Sections = append(sel.Sections, s.Name)

		if !s.DisabledValid() {
			// Как netifd прочтёт это значение, мы не знаем — значит не
			// имеем права угадывать (ADR-0010).
			unparseable = append(unparseable, s.Name)
			continue
		}
		if !s.Disabled() {
			enabled = append(enabled, s.Name)
		}
	}

	switch {
	case len(sel.Sections) == 0:
		sel.State = Empty

	case len(unparseable) > 0:
		sel.State = Ambiguous
		sel.Conflict = unparseable
		sel.Reason = fmt.Sprintf("значение disabled не распознано в секциях: %v", unparseable)

	case len(enabled) == 0:
		sel.State = AllDisabled

	case len(enabled) == 1:
		sel.State = Single
		sel.Active = enabled[0]

	default:
		sel.State = Ambiguous
		sel.Conflict = enabled
		sel.Reason = fmt.Sprintf("включено несколько станционных секций: %v", enabled)
	}

	return sel
}

// Networks собирает список сохранённых сетей.
//
// Правило редактируемости — прямое следствие инварианта фазы 1 (ADR-0009):
// править можно только выключенные секции, и только когда конфигурация
// однозначна.
func Networks(c *uci.Config, sel Selection, radio string) []Network {
	if c == nil {
		return nil
	}
	out := make([]Network, 0, len(sel.Sections))

	for _, s := range c.Sections {
		if !ours(s, radio) {
			continue
		}
		enabled := s.DisabledValid() && !s.Disabled()
		named := !s.Anonymous

		var id *string
		if named {
			// Индекс наружу не отдаём: он съедет при удалении соседней
			// секции, и вкладка, открытая пять минут назад, отредактирует
			// чужую сеть (ADR-0005). Значит у анонимной секции адреса нет —
			// и поле остаётся null.
			name := s.Name
			id = &name
		}

		out = append(out, Network{
			ID:         id,
			SSID:       s.Options["ssid"],
			Encryption: s.Options["encryption"],
			// Ровно то, что написано: в секции сохранён непустой key.
			// «Открытой сети пароль не нужен» — ответ на другой вопрос
			// («можно ли подключиться»), и склеивать их нельзя: у сети с
			// encryption=none пароля нет, а не «есть» (ADR-0012).
			HasKey:   s.Options["key"] != "",
			Network:  s.Options["network"],
			Enabled:  enabled,
			Editable: named && !enabled && sel.State != Ambiguous,
		})
	}
	return out
}

// Fingerprint — отпечаток конфигурации для оптимистичной блокировки.
//
// Снимается до чтения и сверяется перед коммитом: если между ними кто-то
// (LuCI, ssh) успел поменять файл, коммит отменяется. Никакой блокировки,
// которая накрыла бы LuCI, не существует, поэтому остаётся замечать.
func Fingerprint(rawShow []byte) string {
	sum := sha256.Sum256(rawShow)
	return "sha256:" + hex.EncodeToString(sum[:8])
}

// ─────────── ubus: network.wireless status ───────────

type RadioConfig struct {
	Band    string `json:"band"`
	Channel any    `json:"channel"`
	Country string `json:"country"`
	HTMode  string `json:"htmode"`
}

type RadioIface struct {
	Section string `json:"section"`
	Ifname  string `json:"ifname"`
	Config  struct {
		Mode string `json:"mode"`
		SSID string `json:"ssid"`
	} `json:"config"`
	Stations []json.RawMessage `json:"stations"`
}

type Radio struct {
	Up         bool         `json:"up"`
	Pending    bool         `json:"pending"`
	Disabled   bool         `json:"disabled"`
	Config     RadioConfig  `json:"config"`
	Interfaces []RadioIface `json:"interfaces"`
}

// ParseStatus разбирает `ubus call network.wireless status`.
func ParseStatus(b []byte) (map[string]Radio, error) {
	var out map[string]Radio
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("network.wireless status: %w", err)
	}
	return out, nil
}

// IfnameForSection возвращает имя интерфейса по имени секции UCI.
//
// Связь дана прямо в ответе ubus, поэтому сопоставлять по порядку не нужно,
// а хардкодить `phy0.0-sta0` тем более: имя меняется с конфигурацией радио
// и на другом железе выглядит иначе.
func IfnameForSection(st map[string]Radio, section string) string {
	for _, r := range st {
		for _, i := range r.Interfaces {
			if i.Section == section {
				return i.Ifname
			}
		}
	}
	return ""
}

// IfnameForMode возвращает имя интерфейса по радио и режиму (`ap`, `sta`).
func IfnameForMode(st map[string]Radio, radio, mode string) string {
	r, ok := st[radio]
	if !ok {
		return ""
	}
	for _, i := range r.Interfaces {
		if i.Config.Mode == mode {
			return i.Ifname
		}
	}
	return ""
}

// PendingApply сообщает, есть ли изменения, не применённые к радио.
//
// В фазе 1 наши записи применения не вызывают, поэтому true после нашей
// записи — штатное состояние, а не тревога (ADR-0009).
func PendingApply(st map[string]Radio) bool {
	for _, r := range st {
		if r.Pending {
			return true
		}
	}
	return false
}

// ─────────── ubus: iwinfo scan ───────────

// ScanResult — сеть в эфире.
type ScanResult struct {
	SSID       string `json:"ssid"`
	BSSID      string `json:"bssid"`
	Hidden     bool   `json:"hidden"`
	SignalDBm  int    `json:"signal_dbm"`
	Quality    int    `json:"quality"`
	QualityMax int    `json:"quality_max"`
	Channel    int    `json:"channel"`
	Encryption string `json:"encryption"`
}

// scanWire повторяет форму ответа iwinfo. Поле ssid — указатель, потому что
// у скрытых сетей оно ОТСУТСТВУЕТ (в снимке 3 из 14, raw/23). Со строкой
// вместо указателя скрытая сеть молча превратилась бы в сеть с пустым
// именем и потерялась бы при дедупликации по SSID.
type scanWire struct {
	SSID       *string `json:"ssid"`
	BSSID      string  `json:"bssid"`
	Signal     int     `json:"signal"`
	Quality    int     `json:"quality"`
	QualityMax int     `json:"quality_max"`
	Channel    int     `json:"channel"`
	Encryption struct {
		Enabled bool     `json:"enabled"`
		WPA     []int    `json:"wpa"`
		Auth    []string `json:"authentication"`
	} `json:"encryption"`
}

// ParseScan разбирает `ubus call iwinfo scan`.
func ParseScan(b []byte) ([]ScanResult, error) {
	var wire struct {
		Results []scanWire `json:"results"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return nil, fmt.Errorf("iwinfo scan: %w", err)
	}

	out := make([]ScanResult, 0, len(wire.Results))
	for _, r := range wire.Results {
		res := ScanResult{
			BSSID:      r.BSSID,
			SignalDBm:  r.Signal,
			Quality:    r.Quality,
			QualityMax: r.QualityMax,
			Channel:    r.Channel,
			Encryption: encName(r.Encryption.Enabled, r.Encryption.WPA, r.Encryption.Auth),
		}
		if r.SSID != nil {
			res.SSID = *r.SSID
		} else {
			res.Hidden = true
		}
		out = append(out, res)
	}
	return out, nil
}

// encName сводит форму iwinfo к именам, которыми оперирует UCI.
func encName(enabled bool, wpa []int, auth []string) string {
	if !enabled {
		return "none"
	}
	has := func(n int) bool {
		for _, v := range wpa {
			if v == n {
				return true
			}
		}
		return false
	}
	sae := false
	for _, a := range auth {
		if a == "sae" {
			sae = true
		}
	}

	switch {
	case has(3) && sae && has(2):
		return "sae-mixed" // WPA2 + WPA3
	case has(3) || sae:
		return "sae"
	case has(2) && has(1):
		return "psk-mixed"
	case has(2):
		return "psk2"
	case has(1):
		return "psk"
	default:
		return "unknown"
	}
}

// ─────────── ubus: iwinfo info ───────────

// Info — состояние ассоциации станции.
type Info struct {
	SSID       string `json:"ssid"`
	Associated bool   `json:"associated"`
	SignalDBm  int    `json:"signal_dbm"`
	NoiseDBm   int    `json:"noise_dbm"`
	Quality    int    `json:"quality"`
	QualityMax int    `json:"quality_max"`
	Channel    int    `json:"channel"`
	Mode       string `json:"mode"`
}

// ParseInfo разбирает `ubus call iwinfo info`.
//
// Поле bssid из ответа сознательно НЕ используется: в режиме station оно
// содержит MAC самого роутера, а не точки доступа (см. docs/recon/ubus.md).
// Поле выглядит осмысленным и даёт неверное — худший вид данных.
func ParseInfo(b []byte) (Info, error) {
	var wire struct {
		SSID       *string `json:"ssid"`
		Signal     int     `json:"signal"`
		Noise      int     `json:"noise"`
		Quality    int     `json:"quality"`
		QualityMax int     `json:"quality_max"`
		Channel    int     `json:"channel"`
		Mode       string  `json:"mode"`
	}
	if err := json.Unmarshal(b, &wire); err != nil {
		return Info{}, fmt.Errorf("iwinfo info: %w", err)
	}

	out := Info{
		SignalDBm:  wire.Signal,
		NoiseDBm:   wire.Noise,
		Quality:    wire.Quality,
		QualityMax: wire.QualityMax,
		Channel:    wire.Channel,
		Mode:       wire.Mode,
	}
	if wire.SSID != nil && *wire.SSID != "" {
		out.SSID = *wire.SSID
		out.Associated = true
	}
	return out, nil
}

// ─────────── помощники тестов ───────────

// containsSecret проверяет, что в сериализованной сети нет пароля.
// Живёт рядом с типом намеренно: проверка должна переехать вместе с ним,
// если структура когда-нибудь обзаведётся новыми полями.
func containsSecret(n Network) bool {
	b, err := json.Marshal(n)
	if err != nil {
		return true
	}
	var m map[string]any
	if json.Unmarshal(b, &m) != nil {
		return true
	}
	for _, bad := range []string{"key", "pass", "password", "psk", "secret"} {
		if _, ok := m[bad]; ok {
			return true
		}
	}
	return false
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
