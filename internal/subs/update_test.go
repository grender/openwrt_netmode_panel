package subs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"netmoded/internal/happ"
)

// oneNode — подписка из одной пригодной записи: vless + tcp + reality.
//
// Собрана здесь, а не взята из internal/happ/testdata: этому пакету нужен
// не полный формат провайдера (его доказывают тесты happ), а факт «разбор
// дал узел». Ссылка на чужую фикстуру связала бы тесты политики с формой
// чужого JSON, который меняется без нашего участия.
const oneNode = `[
  {
    "remarks": "🇩🇪 Германия",
    "outbounds": [
      {
        "tag": "proxy",
        "protocol": "vless",
        "settings": {"vnext": [{
          "address": "203.0.113.10",
          "port": 443,
          "users": [{"id": "00000000-0000-0000-0000-000000000001", "flow": "xtls-rprx-vision"}]
        }]},
        "streamSettings": {
          "network": "tcp",
          "security": "reality",
          "realitySettings": {
            "serverName": "example.com",
            "publicKey": "pk",
            "shortId": "ab",
            "fingerprint": "chrome"
          }
        }
      }
    ]
  }
]`

// zeroNodes — подписка, которая разбирается, но узлов не даёт: одна запись
// «Авто», то есть балансировщик без своего адреса.
const zeroNodes = `[{"remarks":"Авто | Лучший сервер","outbounds":[],"routing":{"balancers":[{}]}}]`

type harness struct {
	up        *Updater
	provider  string
	manifest  string
	reloads   int
	fetchArgs []string
}

func newHarness(t *testing.T, body string, fetchErr error) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{
		provider: filepath.Join(dir, "sub.yaml"),
		manifest: filepath.Join(dir, "subscription.json"),
	}
	h.up = &Updater{
		URL:          "https://example.invalid/sub/secret-id",
		ProviderPath: h.provider,
		ManifestPath: h.manifest,
		Reload: func(context.Context) error {
			h.reloads++
			return nil
		},
		Fetch: func(_ context.Context, url string) ([]byte, error) {
			h.fetchArgs = append(h.fetchArgs, url)
			if fetchErr != nil {
				return nil, fetchErr
			}
			return []byte(body), nil
		},
	}
	return h
}

func mustNotExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s: файл %s создан (%v)", why, filepath.Base(path), err)
	}
}

func TestUpdateWritesBothFilesAndReloads(t *testing.T) {
	h := newHarness(t, oneNode, nil)

	sum, err := h.up.Update(context.Background())
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if sum.Nodes != 1 {
		t.Errorf("узлов %d, ожидался 1 (сводка: %+v)", sum.Nodes, sum)
	}

	prov, err := os.ReadFile(h.provider)
	if err != nil {
		t.Fatalf("файл провайдера не записан: %v", err)
	}
	if !strings.Contains(string(prov), "203.0.113.10") {
		t.Errorf("в файле провайдера нет узла:\n%s", prov)
	}

	entries, err := LoadManifest(h.manifest)
	if err != nil {
		t.Fatalf("манифест не читается: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != happ.KindNode || entries[0].Name != "🇩🇪 Германия" {
		t.Errorf("манифест: %+v", entries)
	}

	// Ровно один: движок перечитывает провайдера с диска, и второй вызов
	// — это лишняя пересборка списка узлов на роутере, где она стоит
	// заметно.
	if h.reloads != 1 {
		t.Errorf("перезагрузок провайдера %d, ожидалась одна", h.reloads)
	}
}

// Предохранитель нуля узлов (SPEC §2, ADR-0031): перезапись пустым списком
// отняла бы у владельца рабочий туннель молча.
func TestUpdateRefusesZeroNodesAndTouchesNothing(t *testing.T) {
	h := newHarness(t, zeroNodes, nil)

	sum, err := h.up.Update(context.Background())
	if !errors.Is(err, ErrNoNodes) {
		t.Fatalf("ошибка %v, ожидалась ErrNoNodes", err)
	}
	if sum.Nodes != 0 {
		t.Errorf("узлов %d, ожидался 0", sum.Nodes)
	}
	// Текст обязан говорить о сохранённом старом списке: без этого он
	// читается как потеря данных, и владелец чинит то, что не ломалось.
	if !strings.Contains(err.Error(), "СОХРАНЁН") {
		t.Errorf("в тексте не сказано, что старый список цел: %v", err)
	}

	mustNotExist(t, h.provider, "ноль узлов")
	mustNotExist(t, h.manifest, "ноль узлов")
	if h.reloads != 0 {
		t.Errorf("движок дёрнут %d раз при нуле узлов", h.reloads)
	}
}

// Тот же предохранитель, но поверх уже работающей установки: важен не факт
// «файла нет», а факт «прежний файл не изменился».
func TestUpdateKeepsPreviousFilesOnZeroNodes(t *testing.T) {
	h := newHarness(t, zeroNodes, nil)
	const oldProvider = "{\"proxies\":[{\"name\":\"старый узел\"}]}\n"
	const oldManifest = `[{"name":"старый узел","kind":"node"}]`
	if err := os.WriteFile(h.provider, []byte(oldProvider), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.manifest, []byte(oldManifest), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := h.up.Update(context.Background()); !errors.Is(err, ErrNoNodes) {
		t.Fatalf("ошибка %v, ожидалась ErrNoNodes", err)
	}

	for _, tt := range []struct{ path, want string }{
		{h.provider, oldProvider},
		{h.manifest, oldManifest},
	} {
		got, err := os.ReadFile(tt.path)
		if err != nil {
			t.Fatalf("%s: %v", tt.path, err)
		}
		if string(got) != tt.want {
			t.Errorf("%s переписан:\n%s", filepath.Base(tt.path), got)
		}
	}
}

func TestUpdateKeepsFilesOnFetchError(t *testing.T) {
	boom := errors.New("happ: подписка не скачалась: connection refused")
	h := newHarness(t, "", boom)

	if _, err := h.up.Update(context.Background()); !errors.Is(err, boom) {
		t.Fatalf("ошибка %v, ожидалась ошибка скачивания как есть", err)
	}

	mustNotExist(t, h.provider, "скачивание не удалось")
	mustNotExist(t, h.manifest, "скачивание не удалось")
	if h.reloads != 0 {
		t.Errorf("движок дёрнут %d раз без новых файлов", h.reloads)
	}
}

// happ.Fetch уже вычистил адрес из текста ошибки (там *url.Error несёт
// полный URL с секретом владельца). Обёртка в этом пакете не имеет права
// вернуть его обратно.
func TestUpdateDoesNotLeakURLIntoFetchError(t *testing.T) {
	h := newHarness(t, "", errors.New("happ: подписка не скачалась: i/o timeout"))

	_, err := h.up.Update(context.Background())
	if err == nil {
		t.Fatal("ошибка скачивания проглочена")
	}
	if strings.Contains(err.Error(), "secret-id") {
		t.Errorf("адрес подписки уехал в текст ошибки: %v", err)
	}
}

// Отказ перезагрузки — это ошибка обновления, но файлы УЖЕ новые. Не сказав
// этого, мы отправим владельца чинить запись, которая прошла.
func TestUpdateReportsFilesWrittenWhenReloadFails(t *testing.T) {
	h := newHarness(t, oneNode, nil)
	h.up.Reload = func(context.Context) error {
		h.reloads++
		return errors.New("nikki: провайдер записан, но движком не перечитан")
	}

	sum, err := h.up.Update(context.Background())
	if err == nil {
		t.Fatal("отказ перезагрузки выдан за успех")
	}
	if sum.Nodes != 1 {
		t.Errorf("сводка потеряна: %+v", sum)
	}
	if !strings.Contains(err.Error(), "файлы записаны") {
		t.Errorf("в тексте не сказано, что запись прошла: %v", err)
	}

	for _, path := range []string{h.provider, h.manifest} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("%s не записан при отказе перезагрузки: %v", filepath.Base(path), err)
		}
	}
}

func TestUpdateWithoutURL(t *testing.T) {
	h := newHarness(t, oneNode, nil)
	h.up.URL = ""

	if _, err := h.up.Update(context.Background()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("ошибка %v, ожидалась ErrNotConfigured", err)
	}
	if len(h.fetchArgs) != 0 {
		t.Errorf("без адреса состоялось скачивание: %v", h.fetchArgs)
	}
	mustNotExist(t, h.provider, "адрес не задан")
	mustNotExist(t, h.manifest, "адрес не задан")
}

// В файле провайдера лежит uuid абонента, в манифесте — рядом с токеном
// демона. Оба секретны, и права на них проверяются, а не подразумеваются.
func TestUpdateFilesArePrivate(t *testing.T) {
	h := newHarness(t, oneNode, nil)
	if _, err := h.up.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}

	for _, path := range []string{h.provider, h.manifest} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if perm := fi.Mode().Perm(); perm != filePerm {
			t.Errorf("%s: права %04o, ожидались %04o", filepath.Base(path), perm, filePerm)
		}
	}
}

// Временный файл не должен переживать успешную запись: .tmp рядом с
// провайдером собьёт с толку любого, кто полезет разбираться в /etc/nikki.
func TestUpdateLeavesNoTempFiles(t *testing.T) {
	h := newHarness(t, oneNode, nil)
	if _, err := h.up.Update(context.Background()); err != nil {
		t.Fatalf("Update: %v", err)
	}

	for _, path := range []string{h.provider + ".tmp", h.manifest + ".tmp"} {
		mustNotExist(t, path, "после успешной записи")
	}
}

// Отсутствие манифеста — состояние свежей установки, а не ошибка: список
// собирается из одного движка, и этот путь обязан работать.
func TestLoadManifestMissingFileIsNotAnError(t *testing.T) {
	entries, err := LoadManifest(filepath.Join(t.TempDir(), "нет-такого.json"))
	if err != nil {
		t.Fatalf("отсутствие манифеста объявлено ошибкой: %v", err)
	}
	if entries != nil {
		t.Errorf("ожидался nil, получено %+v", entries)
	}
}

// Битый манифест — ошибка. Считать его пустым значило бы бесшумно потерять
// порядок: список остался бы рабочим, но перетасованным, и причину искали
// бы у провайдера.
func TestLoadManifestBrokenFileIsAnError(t *testing.T) {
	for _, tt := range []struct{ name, body string }{
		{"обрывок", `[{"name":"узел",`},
		{"пустой файл", ""},
		{"не массив", `{"name":"узел"}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "subscription.json")
			if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadManifest(path); err == nil {
				t.Error("битый манифест принят молча")
			}
		})
	}
}

// Круговой проход: что записало Update, то читает LoadManifest — включая
// вид записи и причину непригодности, которых в Clash API нет вовсе.
func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subscription.json")

	want := []happ.Entry{
		{Name: "Авто | Лучший сервер", Kind: happ.KindAuto},
		{Name: "🇩🇪 Германия", Kind: happ.KindNode, Type: "vless"},
		{Name: "⬇️ Обходы ⬇️", Kind: happ.KindSeparator},
		{Name: "🇯🇵 Япония", Kind: happ.KindUnsupported, Reason: "транспорт не поддержан"},
	}
	b, err := json.MarshalIndent(want, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("записей %d, ожидалось %d", len(got), len(want))
	}
	for i := range want {
		// Поля перечислены поимённо, а не сравнением структур: у Entry
		// есть карта Proxy, и структуру с ней сравнить нельзя. Заодно
		// видно, что именно проверяется, — порядок, имя, вид и причина.
		if got[i].Name != want[i].Name || got[i].Kind != want[i].Kind ||
			got[i].Type != want[i].Type || got[i].Reason != want[i].Reason {
			t.Errorf("запись %d: %+v, ожидалась %+v", i, got[i], want[i])
		}
		// Proxy в манифест не пишется (json:"-"): две копии одних и тех
		// же серверов разъехались бы при первом частичном сбое записи.
		if got[i].Proxy != nil {
			t.Errorf("запись %d: узел уехал в манифест", i)
		}
	}
}

// Без Reload обновление закончилось бы новым файлом на диске и старым
// списком в движке — «успехом», после которого ничего не изменилось.
func TestUpdateRefusesWithoutReload(t *testing.T) {
	h := newHarness(t, oneNode, nil)
	h.up.Reload = nil

	if _, err := h.up.Update(context.Background()); err == nil {
		t.Fatal("несобранный граф принят молча")
	}
	mustNotExist(t, h.provider, "перезагрузка не настроена")
}

// TestWriteAtomicReplacesLeftoverTemp — остаток прерванной записи с чужими
// правами и чужим содержимым не должен ни пережить обновление, ни попасть на
// рабочее место как есть.
func TestWriteAtomicReplacesLeftoverTemp(t *testing.T) {
	h := newHarness(t, oneNode, nil)
	tmp := h.provider + ".tmp"
	if err := os.WriteFile(tmp, []byte("мусор от прерванной записи"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := h.up.Update(context.Background()); err != nil {
		t.Fatal(err)
	}

	st, err := os.Stat(h.provider)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != filePerm {
		t.Errorf("права рабочего файла %o, ожидалось %o: чужие права с .tmp переехали на место", st.Mode().Perm(), filePerm)
	}
	body, _ := os.ReadFile(h.provider)
	if strings.Contains(string(body), "мусор") {
		t.Error("на рабочем месте оказалось содержимое остатка, а не новый файл")
	}
	mustNotExist(t, tmp, "после успеха")
}

// TestWriteAtomicDoesNotFollowSymlinkTemp — подложенная ссылка sub.yaml.tmp
// не должна увести секрет по чужому пути: os.WriteFile пошёл бы по ней,
// O_EXCL после снятия ссылки создаёт свой файл.
func TestWriteAtomicDoesNotFollowSymlinkTemp(t *testing.T) {
	h := newHarness(t, oneNode, nil)
	elsewhere := filepath.Join(t.TempDir(), "чужой-файл")
	if err := os.WriteFile(elsewhere, []byte("нетронуто"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, h.provider+".tmp"); err != nil {
		t.Skip("символические ссылки недоступны:", err)
	}

	if _, err := h.up.Update(context.Background()); err != nil {
		t.Fatal(err)
	}

	got, _ := os.ReadFile(elsewhere)
	if string(got) != "нетронуто" {
		t.Fatalf("секрет уехал по ссылке: чужой файл теперь %q", got)
	}
	body, _ := os.ReadFile(h.provider)
	if !strings.Contains(string(body), `"proxies"`) {
		t.Error("рабочий файл провайдера не записан")
	}
}

// TestUpdateLeavesNoTempOnWriteError — отказ самой записи (не Chmod, не
// Rename) тоже убирает .tmp: асимметрия трёх веток была бы мусором во
// флеше ровно тогда, когда места нет.
func TestUpdateLeavesNoTempOnWriteError(t *testing.T) {
	h := newHarness(t, oneNode, nil)
	// Провайдер — каталог: OpenFile с O_CREATE|O_EXCL на нём отказывает.
	if err := os.Mkdir(h.provider+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	// Remove снимет пустой каталог и запись пройдёт — значит, нужен
	// непустой, чтобы отказ был именно на создании временного файла.
	if err := os.WriteFile(filepath.Join(h.provider+".tmp", "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := h.up.Update(context.Background())
	if err == nil {
		t.Fatal("ожидался отказ записи временного файла")
	}
	mustNotExist(t, h.provider, "после отказа записи")
	mustNotExist(t, h.manifest, "после отказа записи")
	if h.reloads != 0 {
		t.Errorf("движок дёрнули при незаписанных файлах: %d", h.reloads)
	}
}
