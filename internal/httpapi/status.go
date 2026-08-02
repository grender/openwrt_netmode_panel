// Package httpapi — HTTP-слой демона.
//
// Слушает только на LAN-адресе и требует токен (ADR-0014). Наружу не
// выставляется ни при каких условиях: `0.0.0.0` отвергается при старте,
// а не логируется предупреждением.
package httpapi

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"netmoded/internal/b4"
	"netmoded/internal/executor"
	"netmoded/internal/job"
	"netmoded/internal/logs"
	"netmoded/internal/netif"
	"netmoded/internal/nikki"
	"netmoded/internal/uci"
	"netmoded/internal/wireless"
)

// Имена, выведенные разведкой. Держатся здесь, а не в конфиге: сделать их
// настраиваемыми означало бы дать способ увести вызовы на чужой объект.
const (
	stationRadio = "radio0" // станция, 2.4 ГГц (raw/10)
	apRadio      = "radio1" // домашняя точка, 5 ГГц
	upstreamIf   = "wwan"   // L3 upstream (raw/11)
)

// StatusCacheTTL — кэш дорогих чтений.
//
// Панель опрашивает /api/status раз в секунду по SPEC §7, и каждый опрос
// иначе стоил бы четырёх запусков процессов на роутере с 512 МБ. Это кэш,
// а не состояние: его отсутствие меняет только задержку, и ни один путь
// записи в него не заглядывает.
const StatusCacheTTL = 500 * time.Millisecond

// Status — тело ответа GET /api/status.
//
// Форма зафиксирована в docs/api/openapi.yaml и проверяется golden-тестами
// против docs/api/examples/: панель разрабатывается от того же контракта.
type Status struct {
	GeneratedAt string `json:"generated_at"`
	Hostname    string `json:"hostname"`
	Mode        string `json:"mode"`

	SelectionState string   `json:"selection_state"`
	ConfiguredSSID *string  `json:"configured_ssid"`
	AssociatedSSID *string  `json:"associated_ssid"`
	Conflict       []string `json:"conflict,omitempty"`
	PendingApply   bool     `json:"pending_apply"`
	Fingerprint    string   `json:"wireless_fingerprint"`

	AP     APStatus     `json:"ap"`
	Online OnlineStatus `json:"online"`
	Links  Links        `json:"links"`

	Job          *job.Job      `json:"job"`
	Subscription *Subscription `json:"subscription"`
	LastFail     *Fail         `json:"last_fail"`
	Nikki        Service       `json:"nikki"`
	B4           Service       `json:"b4"`
}

type APStatus struct {
	SSID    string `json:"ssid"`
	Band    string `json:"band"`
	Clients *int   `json:"clients"` // null, пока RQ-05 не отвечён
}

type OnlineStatus struct {
	OK        bool   `json:"ok"`
	CheckedAt string `json:"checked_at"`
}

type Links struct {
	Nikki *string `json:"nikki"`
	B4    *string `json:"b4"`
}

// Subscription — результат последнего обновления подписки.
type Subscription struct {
	LastUpdate *string `json:"last_update"`
	Status     string  `json:"status"`
	Nodes      int     `json:"nodes"`
	Error      *string `json:"error"`
}

type Fail struct {
	SSID   string `json:"ssid"`
	Reason string `json:"reason"`
	At     string `json:"at"`
}

type Service struct {
	Available bool    `json:"available"`
	Version   *string `json:"version,omitempty"`
	// Set — активный узел (Nikki) или сет (b4).
	Set string `json:"set,omitempty"`
	// Pinned — выбран ли узел вручную.
	//
	// Без этого признака панель не отличит «движок подобрал» от «закреплено
	// руками»: имя в Set в обоих случаях одинаковое.
	Pinned       bool `json:"pinned,omitempty"`
	EnabledCount int  `json:"enabled_count,omitempty"`
}

// StatusReader собирает статус, кэшируя дорогие чтения.
type StatusReader struct {
	ex    executor.Executor
	b4    b4.Client
	nikki nikki.Client
	jobs  *job.Manager
	logs  *logs.Log
	now   func() time.Time

	mu     sync.Mutex
	cached *Status
	at     time.Time
}

func NewStatusReader(ex executor.Executor) *StatusReader {
	return &StatusReader{ex: ex, b4: b4.New(b4.DefaultBaseURL), now: time.Now}
}

// NewStatusReaderWith собирает читателя с готовыми клиентами.
func NewStatusReaderWith(ex executor.Executor, b4c b4.Client, nk nikki.Client) *StatusReader {
	return &StatusReader{ex: ex, b4: b4c, nikki: nk, now: time.Now}
}

// Read возвращает статус, не старше StatusCacheTTL.
func (r *StatusReader) Read(ctx context.Context) (*Status, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.cached != nil && r.now().Sub(r.at) < StatusCacheTTL {
		// Джоб обновляем даже из кэша: он меняется чаще, чем раз в 500 мс,
		// и застывший прогресс выглядел бы как зависшая операция.
		cp := *r.cached
		if r.jobs != nil {
			cp.Job = r.jobs.Current()
		}
		return &cp, nil
	}

	s, err := r.build(ctx)
	if err != nil {
		return nil, err
	}
	r.cached, r.at = s, r.now()
	return s, nil
}

// build читает систему заново.
//
// Ни одно чтение не считается обязательным: недоступность любого источника
// деградирует соответствующее поле, но не валит весь статус. Панель,
// которая перестала отвечать целиком из-за упавшего b4, бесполезна ровно
// тогда, когда нужна.
func (r *StatusReader) build(ctx context.Context) (*Status, error) {
	now := r.now().UTC().Format(time.RFC3339)
	s := &Status{
		GeneratedAt: now,
		Mode:        r.mode(ctx),
		Online:      OnlineStatus{CheckedAt: now},
		Nikki:       Service{Available: false},
		B4:          Service{Available: false},
	}

	// Конфигурация wireless: выбор, отпечаток.
	rawWireless, err := r.ex.UCIShow(ctx, "wireless")
	if err == nil {
		s.Fingerprint = wireless.Fingerprint(rawWireless)
		if cfg, perr := uci.ParseShow("wireless", rawWireless); perr == nil {
			sel := wireless.Classify(cfg, stationRadio)
			s.SelectionState = string(sel.State)
			s.Conflict = sel.Conflict
			if sel.State == wireless.Single {
				if sec, ok := cfg.Section(sel.Active); ok {
					v := sec.Options["ssid"]
					s.ConfiguredSSID = &v
				}
			}
			if ap := apSection(cfg); ap != nil {
				s.AP.SSID = ap.Options["ssid"]
			}
		}
	}
	if s.SelectionState == "" {
		s.SelectionState = string(wireless.Empty)
	}

	// Живое состояние радио: имена интерфейсов и pending.
	var stationIf, apIf string
	if b, err := r.ex.UbusCall(ctx, "network.wireless", "status", nil); err == nil {
		if st, perr := wireless.ParseStatus(b); perr == nil {
			s.PendingApply = wireless.PendingApply(st)
			stationIf = wireless.IfnameForMode(st, stationRadio, "sta")
			apIf = wireless.IfnameForMode(st, apRadio, "ap")
			if r0, ok := st[apRadio]; ok {
				s.AP.Band = r0.Config.Band
			}
		}
	}
	_ = apIf // RQ-05: число клиентов не подтверждено, поле остаётся null

	// Ассоциация. Имя интерфейса выведено, а не захардкожено.
	if stationIf != "" {
		if b, err := r.ex.UbusCall(ctx, "iwinfo", "info",
			map[string]any{"device": stationIf}); err == nil {
			if info, perr := wireless.ParseInfo(b); perr == nil && info.Associated {
				v := info.SSID
				s.AssociatedSSID = &v
			}
		}
	}

	// Внешний канал.
	if b, err := r.ex.UbusCall(ctx, "network.interface."+upstreamIf, "status", nil); err == nil {
		if ifs, perr := netif.ParseStatus(b); perr == nil {
			s.Online.OK = ifs.Online()
		}
	}

	// Nikki: недоступность гасит список узлов, но не валит статус.
	if r.nikki != nil {
		if all, err := r.nikki.Proxies(ctx); err == nil {
			s.Nikki.Available = true
			if g, ok := all["PROXY"]; ok {
				s.Nikki.Set = g.Now
				s.Nikki.Pinned = g.Pinned
			}
			if v, verr := r.nikki.Version(ctx); verr == nil {
				s.Nikki.Version = &v
			}
		}
	}

	// b4: недоступность гасит чипы сетов, но не валит статус.
	if r.b4 != nil {
		if sets, err := r.b4.Sets(ctx); err == nil {
			s.B4.Available = true
			s.B4.Set = b4.Selected(sets)
			s.B4.EnabledCount = b4.EnabledCount(sets)
			if v, verr := r.b4.Version(ctx); verr == nil {
				s.B4.Version = &v.Version
			}
		}
	}

	// Текущая операция. Кэш её не задерживает: джоб меняется чаще, чем
	// раз в 500 мс, и панель обязана видеть прогресс сразу.
	if r.jobs != nil {
		s.Job = r.jobs.Current()
	}

	// Последнее обновление подписки — из журнала, а не из памяти: после
	// перезапуска демона картина обязана восстанавливаться (SPEC §12).
	if r.logs != nil {
		if e, ok, lerr := r.logs.Last(); lerr == nil && ok {
			ts := e.TS.UTC().Format(time.RFC3339)
			sub := &Subscription{LastUpdate: &ts, Status: e.Status, Nodes: e.Nodes}
			if e.Err != "" {
				msg := e.Err
				sub.Error = &msg
			}
			s.Subscription = sub
		} else {
			s.Subscription = &Subscription{Status: "never"}
		}
	}

	s.Hostname = r.hostname(ctx)
	return s, nil
}

// mode читает намерение владельца.
//
// Значение вне набора не исправляется: отдаём "unknown" и оставляем файл
// как есть (ADR-0010). Отсутствие записи — это "off", а не сбой: свежая
// установка выглядит именно так.
func (r *StatusReader) mode(ctx context.Context) string {
	v, err := r.ex.UCIGet(ctx, "netmode", "main", "mode")
	if err != nil {
		return "off"
	}
	switch v {
	case "nikki", "b4", "off":
		return v
	default:
		return "unknown"
	}
}

func (r *StatusReader) hostname(ctx context.Context) string {
	if v, err := r.ex.UCIGet(ctx, "system", "@system[0]", "hostname"); err == nil && v != "" {
		return v
	}
	return "OpenWrt"
}

// apSection находит домашнюю точку доступа — только чтобы показать её имя.
// Мы её не трогаем: она не наша (ADR-0003).
func apSection(c *uci.Config) *uci.Section {
	for i := range c.Sections {
		s := &c.Sections[i]
		if s.Type == "wifi-iface" && s.Options["device"] == apRadio && s.Options["mode"] == "ap" {
			return s
		}
	}
	return nil
}

// MarshalStatus — сериализация с отступами, как в golden-фикстурах.
func MarshalStatus(s *Status) ([]byte, error) {
	return json.MarshalIndent(s, "", "  ")
}
