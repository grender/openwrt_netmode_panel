package nikki

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// rawPath — фикстура разведки. Тесты читают записанный вывод роутера, а не
// рукописную догадку: рукописный мок проверяет нашу догадку саму против
// себя (docs/recon/README.md).
const rawPath = "../../docs/recon/raw/91-watch-logs-connections-hosts.txt"

// rawSection отдаёт строки фикстуры между заголовком «## <header>» и
// следующим заголовком. Разделитель, а не отдельные файлы: фикстура одна,
// и резать её копиями значило бы завести второй источник правды о роутере.
func rawSection(t *testing.T, header string) []string {
	t.Helper()
	f, err := os.Open(rawPath)
	if err != nil {
		t.Fatalf("фикстура не открылась: %v", err)
	}
	defer f.Close()
	var out []string
	in := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<10)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "## "):
			in = strings.Contains(line, header)
		case strings.HasPrefix(line, "#"):
			in = false
		case in && strings.TrimSpace(line) != "":
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("фикстура не дочиталась: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("в фикстуре нет раздела %q", header)
	}
	return out
}

func serveJSON(t *testing.T, body string) *HTTP {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != connectionsPath {
			t.Errorf("запрошен %s, ожидался %s", r.URL.Path, connectionsPath)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "s3cret")
}

// TestConnectionsParsesRouterRecords держит форму записи снимка.
//
// Обе записи — дословные строки роутера, и ловят они ровно те три места, где
// наивный разбор ломается: порт назначения приходит СТРОКОЙ, адрес
// назначения у fake-ip пуст (а remoteDestination заполнен), цепочка идёт от
// узла к группе.
func TestConnectionsParsesRouterRecords(t *testing.T) {
	recs := rawSection(t, "одна запись fake-ip и одна normal")
	if len(recs) != 2 {
		t.Fatalf("в фикстуре %d записей, ожидалось 2", len(recs))
	}
	body := `{"downloadTotal":26887948512,"uploadTotal":9866090047,"memory":73535488,"connections":[` +
		recs[0] + "," + recs[1] + `]}`

	snap, err := serveJSON(t, body).Connections(context.Background())
	if err != nil {
		t.Fatalf("Connections: %v", err)
	}
	if snap.UploadTotal != 9866090047 || snap.DownloadTotal != 26887948512 {
		t.Errorf("счётчики движка = %d/%d", snap.UploadTotal, snap.DownloadTotal)
	}
	if snap.At.IsZero() {
		t.Error("At пуст: время снимка берётся с НАШИХ часов, в теле его нет")
	}
	if len(snap.Connections) != 2 {
		t.Fatalf("соединений %d, ожидалось 2", len(snap.Connections))
	}

	fake := snap.Connections[0]
	if fake.DestinationPort != 443 {
		t.Errorf("destinationPort = %d; в JSON он СТРОКА, и без разбора был бы 0", fake.DestinationPort)
	}
	if fake.SourcePort != 50317 || fake.SourceKey() != "192.168.9.219:50317" {
		t.Errorf("ключ склейки = %q", fake.SourceKey())
	}
	if fake.DestinationIP != "" {
		t.Errorf("destinationIP у fake-ip = %q, ожидалась пустота", fake.DestinationIP)
	}
	if fake.RemoteDestination != "154.222.132.99" {
		t.Errorf("remoteDestination = %q", fake.RemoteDestination)
	}
	if fake.Name() != "api.anthropic.com" {
		t.Errorf("имя адресата = %q, ожидался host при пустом sniffHost", fake.Name())
	}
	if got := ChainString(fake.Chains); got != "BYPASS[🇫🇯⚡Фокстрот]" {
		t.Errorf("цепочка = %q, а журнал пишет BYPASS[🇫🇯⚡Фокстрот]", got)
	}
	if fake.Rule != "RuleSet" || fake.RulePayload != "nm-geosite-anthropic" {
		t.Errorf("правило = %q/%q", fake.Rule, fake.RulePayload)
	}
	if fake.Geo != nil {
		t.Errorf("destinationGeoIP = %v, у fake-ip его не бывает", fake.Geo)
	}
	if fake.Start.IsZero() || fake.Start.Nanosecond() == 0 {
		t.Errorf("start = %v, ожидалось RFC3339 с наносекундами", fake.Start)
	}

	norm := snap.Connections[1]
	if norm.Name() != "13.222.111.224" {
		t.Errorf("имя адресата без домена = %q, ожидался адрес назначения", norm.Name())
	}
	if len(norm.Geo) != 1 || norm.Geo[0] != "us" {
		t.Errorf("destinationGeoIP = %v", norm.Geo)
	}
	if got := ChainString(norm.Chains); got != "DIRECT" {
		t.Errorf("прямая цепочка = %q", got)
	}
}

// TestChainStringMatchesLogLine держит вторую половину склейки: снимок и
// журнал обязаны называть цепочку ОДИНАКОВО, иначе владелец видит два
// разных имени одного и того же узла.
func TestChainStringMatchesLogLine(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"DIRECT"}, "DIRECT"},
		{[]string{"🇫🇯⚡Фокстрот", "PROXY", "BYPASS"}, "BYPASS[🇫🇯⚡Фокстрот]"},
		{[]string{"узел", "группа"}, "группа[узел]"},
	}
	for _, c := range cases {
		if got := ChainString(c.in); got != c.want {
			t.Errorf("ChainString(%v) = %q, ожидалось %q", c.in, got, c.want)
		}
	}
}

// TestConnectionsBodyLimitRefuses: снимок с потолком, а не без него.
//
// Потолок назван явно в тексте отказа: усечённое тело декодер вернул бы как
// «неожиданный конец», то есть неотличимо от оборванного соединения.
func TestConnectionsBodyLimitRefuses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"connections":[`)
		rec := `{"id":"x","metadata":{"sourceIP":"192.168.9.219","sourcePort":"1","destinationPort":"443"}},`
		for n := 0; n < connectionsBodyLimit/len(rec)+16; n++ {
			if _, err := fmt.Fprint(w, rec); err != nil {
				return
			}
		}
		fmt.Fprint(w, `{"id":"last"}]}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "").Connections(context.Background())
	if err == nil {
		t.Fatal("тело больше потолка прошло молча")
	}
	if !strings.Contains(err.Error(), "потолок") {
		t.Errorf("ошибка = %v, ожидалось упоминание потолка", err)
	}
}

// TestConnectionsUnauthorizedIsUnavailable: отвергнутый секрет — та же
// недоступность, что и молчащий движок. Подбирать его мы не станем.
func TestConnectionsUnauthorizedIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "wrong").Connections(context.Background())
	if err == nil || !strings.Contains(err.Error(), "недоступен") {
		t.Errorf("401 дал %v, ожидалась ErrUnavailable", err)
	}
}
