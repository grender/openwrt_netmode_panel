package main

// Фикстура наблюдателя для стенда.
//
// Данные — из docs/recon/raw/91-watch-logs-connections-hosts.txt: те же
// адресаты, те же цепочки, тот же неотвечающий 194.221.250.50. Числа растут
// от вызова к вызову, иначе экран, у которого главное свойство — «числа
// меняются раз в секунду, а раскладка не прыгает», на ноуте не проверяется
// вовсе.

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"time"

	"netmoded/internal/nikki"
)

// watchDevIP — устройство, за которым стенд даёт наблюдать. Совпадает с тем,
// что снято в raw/91.
const watchDevIP = "192.168.9.219"

type devConn struct {
	id      string
	name    string // sniffHost/host; пусто — адресат назван адресом
	addr    string
	port    int
	net     string
	chains  []string
	rule    string
	payload string
	geo     []string
	// upRate, downRate — байт в секунду; 0 у молчащего адресата.
	upRate, downRate int64
	// silent — рукопожатие прошло, ответа нет: upload растёт, download стоит.
	silent bool
	ageSec int
}

var devConns = []devConn{
	{id: "3f10a1d4-4913-4793-82a4-f63b664e7808", name: "api.anthropic.com", addr: "154.222.132.99", port: 443, net: "tcp",
		chains: []string{"🇫🇷⚡Франция", "PROXY", "BYPASS"}, rule: "RuleSet", payload: "nm-geosite-anthropic",
		upRate: 60, downRate: 140, ageSec: 240},
	{id: "b1a2c3d4-0001-4000-8000-000000000001", name: "www.youtube.com", addr: "142.250.150.91", port: 443, net: "tcp",
		chains: []string{"🇫🇷⚡Франция", "PROXY", "BYPASS"}, rule: "RuleSet", payload: "nm-geosite-youtube",
		upRate: 3600, downRate: 220000, ageSec: 720},
	{id: "b1a2c3d4-0001-4000-8000-000000000002", name: "rr1---sn-5hne6n6z.googlevideo.com", addr: "173.194.135.72", port: 443, net: "udp",
		chains: []string{"🇫🇷⚡Франция", "PROXY", "BYPASS"}, rule: "RuleSet", payload: "nm-geosite-youtube",
		upRate: 12288, downRate: 1468006, ageSec: 660},
	{id: "b1a2c3d4-0001-4000-8000-000000000003", name: "www.google.com", addr: "142.250.74.100", port: 443, net: "tcp",
		chains: []string{"DIRECT"}, rule: "Match", upRate: 400, downRate: 2600, ageSec: 700},
	{id: "b1a2c3d4-0001-4000-8000-000000000004", name: "cdn.discordapp.com", addr: "162.159.135.233", port: 443, net: "tcp",
		chains: []string{"DIRECT"}, rule: "Match", upRate: 30, silent: true, ageSec: 40},
	{id: "b1a2c3d4-0001-4000-8000-000000000005", name: "", addr: "151.101.129.91", port: 443, net: "tcp",
		chains: []string{"DIRECT"}, rule: "Match", upRate: 12, silent: true, ageSec: 90},
	{id: "f26ddbd2-639b-4729-b30c-cca96279102f", name: "", addr: "13.222.111.224", port: 554, net: "tcp",
		chains: []string{"DIRECT"}, rule: "Match", geo: []string{"us"}, upRate: 210, downRate: 340, ageSec: 400},
	{id: "b1a2c3d4-0001-4000-8000-000000000006", name: "netbird.io", addr: "3.68.42.11", port: 443, net: "tcp",
		chains: []string{"DIRECT"}, rule: "DomainSuffix", payload: "netbird.io", upRate: 40, downRate: 90, ageSec: 480},
}

// devWatch — состояние фикстуры наблюдателя.
type devWatch struct {
	mu    sync.Mutex
	start time.Time
	tick  int
}

func (f *devNikki) watch() *devWatch {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.w == nil {
		f.w = &devWatch{start: time.Now()}
	}
	return f.w
}

// Connections — снимок, в котором числа растут от вызова к вызову.
func (f *devNikki) Connections(context.Context) (nikki.Snapshot, error) {
	w := f.watch()
	w.mu.Lock()
	w.tick++
	n := int64(w.tick)
	w.mu.Unlock()

	now := time.Now()
	s := nikki.Snapshot{At: now, Memory: 73535488}
	for _, d := range devConns {
		up := d.upRate * n
		down := d.downRate * n
		if d.silent {
			down = 0
		}
		s.UploadTotal += up
		s.DownloadTotal += down
		s.Connections = append(s.Connections, nikki.Conn{
			ID:                d.id,
			Net:               d.net,
			SourceIP:          watchDevIP,
			SourcePort:        50000 + len(s.Connections),
			Host:              d.name,
			RemoteDestination: d.addr,
			DestinationPort:   d.port,
			Geo:               d.geo,
			Upload:            up + 1024,
			Download:          down,
			Start:             now.Add(-time.Duration(d.ageSec) * time.Second),
			Chains:            d.chains,
			Rule:              d.rule,
			RulePayload:       d.payload,
		})
	}
	// Чужое устройство в снимке есть всегда: отбор по sourceIP обязан быть
	// виден и на стенде, иначе он проверяется только на роутере.
	s.Connections = append(s.Connections, nikki.Conn{
		ID: "c0ffee00-0000-4000-8000-000000000000", Net: "tcp",
		SourceIP: "192.168.9.163", SourcePort: 54094,
		RemoteDestination: "17.57.144.22", DestinationPort: 5223,
		Upload: 77824, Download: 34816, Start: now.Add(-30 * time.Minute),
		Chains: []string{"DIRECT"}, Rule: "Match",
	})
	return s, nil
}

// LogStream — поток, который пишет строку раз в полторы секунды.
//
// Дозвон до 194.221.250.50 не удаётся, как и на роутере: без него вердикт
// «не отвечает» на стенде не увидеть, а он на экране главный.
func (f *devNikki) LogStream(ctx context.Context, level string) (*nikki.LogStream, error) {
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		t := time.NewTicker(1500 * time.Millisecond)
		defer t.Stop()
		port := 50500
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				port++
				var line string
				if port%4 == 0 {
					line = fmt.Sprintf(
						`{"type":"warning","payload":"[TCP] dial DIRECT (match Match/) %s:%d --> 194.221.250.50:5222 error: dial tcp 194.221.250.50:5222: i/o timeout"}`,
						watchDevIP, port)
				} else {
					d := devConns[rand.Intn(len(devConns))]
					name := d.name
					if name == "" {
						name = d.addr
					}
					rule := d.rule
					if d.payload != "" {
						rule = d.rule + "(" + d.payload + ")"
					}
					line = fmt.Sprintf(
						`{"type":"info","payload":"[%s] %s:%d --> %s:%d match %s using %s"}`,
						upper(d.net), watchDevIP, port, name, d.port, rule, nikki.ChainString(d.chains))
				}
				if _, err := io.WriteString(pw, line+"\n"); err != nil {
					return
				}
			}
		}
	}()
	return nikki.NewLogStreamFromReader(pr), nil
}

func upper(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}
