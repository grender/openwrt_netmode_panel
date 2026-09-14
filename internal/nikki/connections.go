package nikki

import (
	"context"
	"net/http"
	"strconv"
	"time"
)

// Снимок соединений Clash API.
//
// Форма снята с живого роутера: docs/recon/raw/91-watch-logs-connections-hosts.txt,
// разбор — docs/recon/nikki-watch.md, «GET /connections». Без заголовка
// Upgrade: websocket mihomo отдаёт ОДИН снимок (hub/route/connections.go:24-29);
// параметр interval имеет смысл только у WebSocket, и его мы не используем.
//
// Фильтра по устройству у mihomo нет: тело приходит целиком, и отбор по
// sourceIP делает вызывающий.

// Conn — одно соединение в снимке.
//
// Поля названы по нашему, а не по проводным именам: проводные лежат в тегах
// wireConn ниже. Главное, что стоит помнить про эту запись, — три вещи,
// каждая из которых ломает наивный разбор:
//
//   - destinationPort в JSON СТРОКА, а не число;
//   - destinationIP пуст у 141 записи из 150 (у fake-ip адрес назначения не
//     резолвился для правил), а remoteDestination заполнен у всех 150 —
//     значит «куда ушло» надо брать из него;
//   - chains идут ОТ УЗЛА К ГРУППЕ: ["🇫🇷⚡Франция","PROXY","BYPASS"].
type Conn struct {
	ID string
	// Net — tcp или udp.
	Net      string
	SourceIP string
	// SourcePort — ключ склейки со строкой журнала: id соединения в журнал
	// не попадает (docs/recon/nikki-watch.md, итог 5).
	SourcePort int
	// Host — домен из fake-ip; пуст у 9 записей из 150.
	Host string
	// SniffHost — домен от sniffer; пуст у 144 из 150: при fake-ip он не нужен.
	SniffHost string
	// DestinationIP — адрес, по которому проверялись правила. Пуст у 141 из 150.
	DestinationIP string
	// RemoteDestination — настоящий адрес, куда ушло соединение. Есть всегда.
	RemoteDestination string
	DestinationPort   int
	// Geo — destinationGeoIP. null у 146 из 150: mihomo ставит метку только
	// в момент проверки правила GEOIP, а до него доходят немногие.
	Geo []string
	// Upload, Download — байты с начала соединения, не за тик.
	Upload, Download int64
	// Start — время старта по часам роутера.
	Start time.Time
	// Chains — от узла к группе, как отдаёт mihomo. Отображаемая форма —
	// ChainString.
	Chains []string
	// Rule, RulePayload — какое правило сработало: RuleSet +
	// nm-geosite-anthropic, DomainSuffix + домен, Match + пусто.
	Rule, RulePayload string
}

// SourceKey — «адрес:порт источника», ключ склейки со строкой журнала.
//
// Собирается здесь, а не у вызывающего: разойдись две сборки хоть пробелом,
// склейка перестала бы находить что-либо, и это выглядело бы как «журнал не
// приходит», а не как опечатка.
func (c Conn) SourceKey() string {
	return c.SourceIP + ":" + strconv.Itoa(c.SourcePort)
}

// Name — имя адресата: sniffHost, иначе host, иначе адрес назначения.
//
// Порядок именно такой: sniffHost приходит от sniffer и точнее fake-ip,
// а remoteDestination есть всегда и потому стоит последним.
func (c Conn) Name() string {
	if c.SniffHost != "" {
		return c.SniffHost
	}
	if c.Host != "" {
		return c.Host
	}
	return c.RemoteDestination
}

// ChainString — цепочка в том же виде, в каком её печатает журнал.
//
// Повторяет Chain.String() из mihomo (constant/adapters.go:75): одно звено
// как есть, несколько — «последнее[первое]», то есть «группа[узел]». Живёт
// рядом с проводным типом, а не у наблюдателя, потому что это форма ЭТОГО
// источника: строка журнала «using BYPASS[🇫🇷⚡Франция]» и снимок обязаны
// давать одну и ту же строку, иначе они не склеятся глазами владельца.
func ChainString(chains []string) string {
	switch len(chains) {
	case 0:
		return ""
	case 1:
		return chains[0]
	default:
		return chains[len(chains)-1] + "[" + chains[0] + "]"
	}
}

// Snapshot — весь ответ /connections.
type Snapshot struct {
	// UploadTotal, DownloadTotal — счётчики движка с его запуска. Уменьшение
	// UploadTotal между снимками означает, что движок перезапустился
	// (ADR-0043): другого признака у нас нет.
	UploadTotal, DownloadTotal int64
	Memory                     int64
	Connections                []Conn
	// At — момент получения по НАШИМ часам. В теле времени нет, а скорость
	// считается делением на прошедшее время, и брать его с часов роутера
	// значило бы считать частное двух разных часов.
	At time.Time
}

// wireSnapshot — проводная форма. Отдельно от Conn, потому что проводные
// имена и типы (порт строкой) — чужой контракт, и тащить их в наблюдатель
// значит тащить туда же и его особенности.
type wireSnapshot struct {
	DownloadTotal int64      `json:"downloadTotal"`
	UploadTotal   int64      `json:"uploadTotal"`
	Memory        int64      `json:"memory"`
	Connections   []wireConn `json:"connections"`
}

type wireConn struct {
	ID       string `json:"id"`
	Metadata struct {
		Network           string   `json:"network"`
		SourceIP          string   `json:"sourceIP"`
		SourcePort        string   `json:"sourcePort"`
		DestinationIP     string   `json:"destinationIP"`
		DestinationPort   string   `json:"destinationPort"`
		DestinationGeoIP  []string `json:"destinationGeoIP"`
		Host              string   `json:"host"`
		SniffHost         string   `json:"sniffHost"`
		RemoteDestination string   `json:"remoteDestination"`
	} `json:"metadata"`
	Upload      int64    `json:"upload"`
	Download    int64    `json:"download"`
	Start       string   `json:"start"`
	Chains      []string `json:"chains"`
	Rule        string   `json:"rule"`
	RulePayload string   `json:"rulePayload"`
}

// connectionsPath — путь Clash API. Литерал отдельной константой: гейт
// scripts/check-evidence.sh сверяет его с docs/recon/evidence.json дословно.
const connectionsPath = "/connections"

// Connections — разовый снимок соединений.
func (c *HTTP) Connections(ctx context.Context) (Snapshot, error) {
	var w wireSnapshot
	if err := c.doLimited(ctx, http.MethodGet, connectionsPath, nil, &w, snapshotTimeout, connectionsBodyLimit); err != nil {
		return Snapshot{}, err
	}
	s := Snapshot{
		UploadTotal:   w.UploadTotal,
		DownloadTotal: w.DownloadTotal,
		Memory:        w.Memory,
		At:            time.Now(),
		Connections:   make([]Conn, 0, len(w.Connections)),
	}
	for _, x := range w.Connections {
		m := x.Metadata
		conn := Conn{
			ID:                x.ID,
			Net:               m.Network,
			SourceIP:          m.SourceIP,
			SourcePort:        atoiSafe(m.SourcePort),
			Host:              m.Host,
			SniffHost:         m.SniffHost,
			DestinationIP:     m.DestinationIP,
			RemoteDestination: m.RemoteDestination,
			DestinationPort:   atoiSafe(m.DestinationPort),
			Geo:               m.DestinationGeoIP,
			Upload:            x.Upload,
			Download:          x.Download,
			Chains:            x.Chains,
			Rule:              x.Rule,
			RulePayload:       x.RulePayload,
		}
		// Неразобранное время — не повод выбрасывать запись: без него
		// потеряется только возраст соединения, то есть подсказка «молчит»,
		// а байты и правило останутся верными.
		if t, err := time.Parse(time.RFC3339Nano, x.Start); err == nil {
			conn.Start = t
		}
		s.Connections = append(s.Connections, conn)
	}
	return s, nil
}

// atoiSafe — порт строкой у mihomo. Мусор даёт 0, а не отказ: порт нужен
// для склейки и подписи, и запись без него всё ещё несёт байты и правило.
func atoiSafe(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
