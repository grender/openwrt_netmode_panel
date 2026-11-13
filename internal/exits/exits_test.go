package exits

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// valid — экспорт ноды в форме, которую пишет node-export.yml репо выходных
// нод (README.md там, «Формат exports/<нода>.json (schema 1)»). Значения
// выдуманы, форма — настоящая: на разбор влияет форма.
const valid = `{
  "schema": 1,
  "name": "kz-nqz-exit-01",
  "country": "kz",
  "address": "kz-nqz-exit-01.example.net",
  "vless": {
    "port": 443,
    "flow": "xtls-rprx-vision",
    "sni": "krisha.kz",
    "fingerprint": "chrome",
    "public_key": "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
    "short_id": "0123abcd",
    "users": {
      "resi": "11111111-2222-4333-8444-555555555555",
      "dc": "66666666-7777-4888-9999-aaaaaaaaaaaa"
    }
  }
}`

func TestParseValid(t *testing.T) {
	n, err := Parse([]byte(valid))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if n.Name != "kz-nqz-exit-01" || n.Country != "kz" || n.Address != "kz-nqz-exit-01.example.net" {
		t.Errorf("шапка: %+v", n)
	}
	if n.Port != 443 || n.Flow != "xtls-rprx-vision" || n.SNI != "krisha.kz" || n.Fingerprint != "chrome" {
		t.Errorf("vless: %+v", n)
	}
	if n.PublicKey != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" || n.ShortID != "0123abcd" {
		t.Errorf("reality: %+v", n)
	}
	if got := n.Exits(); len(got) != 2 || got[0] != "dc" || got[1] != "resi" {
		t.Errorf("Exits() = %v, ждали [dc resi] — порядок стабильный, по имени", got)
	}
	if n.Users["resi"] != "11111111-2222-4333-8444-555555555555" {
		t.Errorf("uuid resi: %q", n.Users["resi"])
	}
}

// Каждое поле ломается по-своему; сообщение называет поле, но никогда не
// содержит значение — в экспорте секреты клиента, а ошибка уходит в журнал
// и в ответ API.
func TestParseRejects(t *testing.T) {
	cases := []struct {
		name, from, to, field string
	}{
		{"нет schema", `"schema": 1,`, ``, "schema"},
		{"чужая schema", `"schema": 1,`, `"schema": 2,`, "schema"},
		{"имя не по правилу", `"kz-nqz-exit-01",`, `"KZ_exit",`, "name"},
		{"страна", `"country": "kz"`, `"country": "Kazakhstan"`, "country"},
		{"пустой адрес", `"address": "kz-nqz-exit-01.example.net"`, `"address": ""`, "address"},
		{"адрес с пробелом", `"address": "kz-nqz-exit-01.example.net"`, `"address": "a b"`, "address"},
		{"порт строкой", `"port": 443`, `"port": "443"`, "port"},
		{"порт вне диапазона", `"port": 443`, `"port": 70000`, "port"},
		{"чужой flow", `"xtls-rprx-vision"`, `"none"`, "flow"},
		{"пустой sni", `"sni": "krisha.kz"`, `"sni": ""`, "sni"},
		{"пустой fingerprint", `"fingerprint": "chrome"`, `"fingerprint": ""`, "fingerprint"},
		{"ключ не base64url", `"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`, `"AAAA+AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`, "public_key"},
		{"ключ не 32 байта", `"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"`, `"AAAAAAAA"`, "public_key"},
		{"short_id не hex", `"0123abcd"`, `"0123abcz"`, "short_id"},
		{"short_id нечётной длины", `"0123abcd"`, `"0123abc"`, "short_id"},
		{"short_id длиннее 16", `"0123abcd"`, `"0123456789abcdef01"`, "short_id"},
		{"uuid битый", `"11111111-2222-4333-8444-555555555555"`, `"11111111-2222-4333-8444-55555555555Z"`, "users.resi"},
		{"имя выхода", `"dc": "66666666`, `"DC!": "66666666`, "users"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			raw := strings.Replace(valid, c.from, c.to, 1)
			if raw == valid {
				t.Fatalf("замена %q не нашлась в фикстуре", c.from)
			}
			_, err := Parse([]byte(raw))
			if err == nil {
				t.Fatal("Parse принял битый экспорт")
			}
			if !strings.Contains(err.Error(), c.field) {
				t.Errorf("ошибка %q не называет поле %q", err, c.field)
			}
			for _, secret := range []string{"11111111-2222", "66666666-7777", "AAAAAAAA", "0123abc"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("ошибка %q содержит значение секрета", err)
				}
			}
		})
	}
}

func TestParseNoUsers(t *testing.T) {
	raw := `{"schema":1,"name":"kz-nqz-exit-01","country":"kz","address":"a.example.net",
	  "vless":{"port":443,"flow":"xtls-rprx-vision","sni":"krisha.kz","fingerprint":"chrome",
	  "public_key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA","short_id":"","users":{}}}`
	if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "users") {
		t.Fatalf("нода без выходов принята: %v", err)
	}
}

// Пустой short_id — законная форма Reality (shortIds: [""]), её принимаем.
func TestParseEmptyShortID(t *testing.T) {
	raw := strings.Replace(valid, `"0123abcd"`, `""`, 1)
	if _, err := Parse([]byte(raw)); err != nil {
		t.Fatalf("пустой short_id отвергнут: %v", err)
	}
}

func TestParseNotJSON(t *testing.T) {
	if _, err := Parse([]byte("proxies:\n  - name: x\n")); err == nil {
		t.Fatal("YAML вместо JSON принят")
	}
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadMissingDirIsEmpty(t *testing.T) {
	nodes, err := Load(filepath.Join(t.TempDir(), "нет"))
	if err != nil || len(nodes) != 0 {
		t.Fatalf("Load(нет каталога) = %v, %v; ждали пусто без ошибки", nodes, err)
	}
}

func TestLoadSortedAndSkipsNonJSON(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "kz-nqz-exit-01.json", valid)
	writeFile(t, dir, "kz-ala-exit-02.json", strings.Replace(valid, "kz-nqz-exit-01", "kz-ala-exit-02", 1))
	writeFile(t, dir, "README", "не нода")
	writeFile(t, dir, ".kz-nqz-exit-03.json.tmp", "{")
	nodes, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(nodes) != 2 || nodes[0].Name != "kz-ala-exit-02" || nodes[1].Name != "kz-nqz-exit-01" {
		t.Fatalf("Load = %v, ждали две ноды по имени", names(nodes))
	}
}

// Битый файл — отказ на весь каталог: нода, молча выпавшая из exits.yaml,
// неотличима от удалённой, а с ней пропадает выход.
func TestLoadFailsOnBrokenFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "kz-nqz-exit-01.json", valid)
	writeFile(t, dir, "kz-ala-exit-02.json", "{")
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "kz-ala-exit-02.json") {
		t.Fatalf("Load с битым файлом: %v; ждали ошибку с именем файла", err)
	}
}

func TestLoadFileNameMustMatchNode(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "kz-ala-exit-02.json", valid) // внутри kz-nqz-exit-01
	if _, err := Load(dir); err == nil || !strings.Contains(err.Error(), "kz-ala-exit-02.json") {
		t.Fatalf("Load с чужим именем файла: %v", err)
	}
}

func names(nodes []Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Name
	}
	return out
}
