package rulesets

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"netmoded/internal/geosite"
	"netmoded/internal/nikki"
)

// golden читает эталон из testdata.
//
// Эталоны написаны руками по §2 спеки и НЕ порождаются кодом: иначе golden
// проверял бы только то, что Render дважды подряд делает одно и то же, а не
// то, что он делает обещанное владельцу и понятное mihomo.
func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("эталон %s: %v", name, err)
	}
	return b
}

// TestRenderGolden — вывод Render байт-в-байт совпадает с эталонами.
//
// Байт-в-байт, а не «разбирается как YAML»: файл склеивает yq на роутере, и
// цена лишнего пробела в отступе — не косметика, а провал склейки, который
// виден только в core.log после перезапуска движка.
func TestRenderGolden(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		file string
	}{
		{
			name: "только эти наборы, качать напрямую",
			cfg: Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
				{Name: "youtube"}, {Name: "telegram", IP: true},
			}},
			file: "mixin-only.yaml",
		},
		{
			name: "всё кроме этих наборов, качать через туннель",
			cfg: Config{Policy: PolicyExcept, Download: DownloadTunnel, Sets: []Set{
				{Name: "github"}, {Name: "telegram", IP: true},
			}},
			file: "mixin-except-tunnel.yaml",
		},
		{
			name: "правила из профиля",
			cfg:  Config{Policy: PolicyProfile, Download: DownloadDirect},
			file: "mixin-profile.yaml",
		},
		{
			name: "имена с @ и ! плюс подсети",
			cfg: Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
				{Name: "netflix@ads"}, {Name: "category-ai-!cn", IP: true},
			}},
			file: "mixin-special.yaml",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := golden(t, tc.file)
			got := Render(tc.cfg)
			if string(got) != string(want) {
				t.Errorf("Render не совпал с %s.\n--- получено ---\n%s\n--- ожидалось ---\n%s",
					tc.file, got, want)
			}
		})
	}
}

// TestRenderKeepsPrivateBeforeMatch — строка GEOIP,PRIVATE стоит ровно перед
// MATCH при обеих политиках.
//
// Отдельная проверка сверх golden потому, что именно эта строка спасает
// локальную сеть: при политике «кроме» хвост MATCH,BYPASS уводит в туннель
// всё подряд, включая 192.168.9.0/24, и роутер перестаёт быть доступен из
// собственного LAN. Убрать её — самая дешёвая на вид правка и самая дорогая
// по последствиям.
func TestRenderKeepsPrivateBeforeMatch(t *testing.T) {
	for _, policy := range []Policy{PolicyOnly, PolicyExcept} {
		lines := strings.Split(strings.TrimRight(string(Render(Config{
			Policy: policy, Download: DownloadDirect, Sets: []Set{{Name: "youtube"}},
		})), "\n"), "\n")

		last, prev := lines[len(lines)-1], lines[len(lines)-2]
		if prev != "  - 'GEOIP,PRIVATE,DIRECT,no-resolve'" {
			t.Errorf("политика %s: перед хвостом %q, ожидалась строка GEOIP,PRIVATE", policy, prev)
		}
		if !strings.HasPrefix(last, "  - 'MATCH,") {
			t.Errorf("политика %s: последняя строка %q, ожидался MATCH", policy, last)
		}
	}
}

// TestRenderEmptySetsHasNoProviders — «только эти наборы», но наборов ноль:
// файл состоит из шапки и двух строк хвоста, без рубрики rule-providers.
//
// Пустая рубрика превратилась бы в rule-providers: null и при склейке yq
// затёрла бы провайдеров профиля — то есть безобидный на вид «ничего не
// выбрано» сломал бы чужие правила.
func TestRenderEmptySetsHasNoProviders(t *testing.T) {
	got := string(Render(Config{Policy: PolicyOnly, Download: DownloadDirect}))
	if strings.Contains(got, "rule-providers") {
		t.Errorf("при пустом выборе появилась рубрика rule-providers:\n%s", got)
	}
	want := "nikki-rules:\n" +
		"  - 'GEOIP,PRIVATE,DIRECT,no-resolve'\n" +
		"  - 'MATCH,DIRECT'\n"
	if !strings.HasSuffix(got, want) {
		t.Errorf("хвост файла:\n%s\nожидался:\n%s", got, want)
	}
}

// TestParseRoundTrip — Parse(Render(c)) возвращает ровно c.
//
// Это и есть смысл всей затеи: второго хранилища выбора нет, состояние
// читается из того же файла, который демон написал. Если пара не сходится,
// панель после перезапуска покажет не то, что применено.
func TestParseRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"только эти", Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
			{Name: "youtube"}, {Name: "telegram", IP: true},
		}}},
		{"кроме этих через туннель", Config{Policy: PolicyExcept, Download: DownloadTunnel, Sets: []Set{
			{Name: "github"}, {Name: "telegram", IP: true},
		}}},
		{"из профиля", Config{Policy: PolicyProfile, Download: DownloadDirect}},
		{"имена с @ и !", Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
			{Name: "netflix@ads"}, {Name: "category-ai-!cn", IP: true},
		}}},
		{"ничего не выбрано", Config{Policy: PolicyExcept, Download: DownloadTunnel}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, foreign, err := Parse(Render(tc.cfg))
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if foreign {
				t.Error("свой же файл опознан как чужой")
			}
			if !reflect.DeepEqual(got, tc.cfg) {
				t.Errorf("прочитано %+v, записано %+v", got, tc.cfg)
			}
		})
	}
}

// TestParseTreatsMissingAndCommentsAsProfile — пустого файла нет, файл из
// поставки nikki комментарный: и то и другое — «правила из профиля», и НЕ
// чужой файл.
//
// Иначе первое же открытие вкладки на свежем роутере отвечало бы
// «файл чужой, ничего не трогаю», и наборы нельзя было бы применить вовсе.
func TestParseTreatsMissingAndCommentsAsProfile(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
	}{
		{"файла нет", nil},
		{"файл пуст", []byte{}},
		{"одни переводы строк", []byte("\n\n   \n")},
		{"комментарный из поставки nikki", golden(t, "mixin-nikki-default.yaml")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, foreign, err := Parse(tc.in)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if foreign {
				t.Error("опознан как чужой, ожидалось «наш, но пустой»")
			}
			if want := (Config{Policy: PolicyProfile, Download: DownloadDirect}); !reflect.DeepEqual(cfg, want) {
				t.Errorf("прочитано %+v, ожидалось %+v", cfg, want)
			}
		})
	}
}

// TestParseForeignFile — владелец написал правила сам: шапки нет, содержимое
// есть. Такой файл переписывать нельзя, и отличить его надо от комментарного.
func TestParseForeignFile(t *testing.T) {
	cfg, foreign, err := Parse(golden(t, "mixin-foreign.yaml"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !foreign {
		t.Fatal("чужой файл не опознан — панель бы его затёрла")
	}
	if cfg.Policy != PolicyProfile || len(cfg.Sets) != 0 {
		t.Errorf("у чужого файла состояние %+v, ожидалось пустое profile", cfg)
	}
}

// TestParseCorrupt — шапка наша, но тело не сходится с состоянием.
//
// Расхождение значит, что файл правили руками либо запись оборвалась: верить
// строке состояния больше нельзя, и честнее сказать «примените заново», чем
// показать владельцу выбор, которого в правилах нет.
func TestParseCorrupt(t *testing.T) {
	head := "# netmoded: шапка\n# вторая строка\n"

	cases := []struct {
		name string
		in   []byte
	}{
		{"правил меньше, чем наборов", golden(t, "mixin-corrupt.yaml")},
		{"строки состояния нет вовсе", []byte(head + "rule-providers:\n")},
		{"неизвестная политика", []byte(head + stateMark + " policy=maybe download=direct sets=\n")},
		{"неизвестное скачивание", []byte(head + stateMark + " policy=only download=carrier sets=\n")},
		{"неизвестное поле состояния", []byte(head + stateMark + " policy=only mood=good sets=\n")},
		{"пустое имя набора", []byte(head + stateMark + " policy=only download=direct sets=youtube,\n")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Parse(tc.in)
			if !errors.Is(err, ErrCorrupt) {
				t.Errorf("получено %v, ожидалось ErrCorrupt", err)
			}
		})
	}
}

// TestParseDownloadDefaultsToDirect — старый файл без поля download читается
// как «качать напрямую», а не как повреждённый: умолчание меняться не должно.
func TestParseDownloadDefaultsToDirect(t *testing.T) {
	in := []byte("# netmoded: шапка\n# вторая строка\n" +
		stateMark + " policy=only sets=youtube\n" +
		"nikki-rules:\n  - 'RULE-SET,nm-geosite-youtube,BYPASS'\n")

	cfg, _, err := Parse(in)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Download != DownloadDirect {
		t.Errorf("скачивание %q, ожидалось %q", cfg.Download, DownloadDirect)
	}
}

// TestFingerprintSensitive — отпечаток различает всё, из-за чего файл надо
// переписать, и совпадает у одинакового выбора.
//
// На нём держится If-Match: если отпечаток не заметит перестановку наборов,
// две открытые вкладки молча затрут выбор друг друга.
func TestFingerprintSensitive(t *testing.T) {
	base := Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
		{Name: "youtube"}, {Name: "telegram", IP: true},
	}}
	same := Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
		{Name: "youtube"}, {Name: "telegram", IP: true},
	}}
	if Fingerprint(base) != Fingerprint(same) {
		t.Error("одинаковый выбор дал разные отпечатки")
	}
	if !strings.HasPrefix(Fingerprint(base), "sha256:") {
		t.Errorf("отпечаток %q без приставки sha256:", Fingerprint(base))
	}

	others := []struct {
		name string
		cfg  Config
	}{
		{"другой порядок наборов", Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
			{Name: "telegram", IP: true}, {Name: "youtube"},
		}}},
		{"другое скачивание", Config{Policy: PolicyOnly, Download: DownloadTunnel, Sets: base.Sets}},
		{"другая политика", Config{Policy: PolicyExcept, Download: DownloadDirect, Sets: base.Sets}},
		{"снят признак подсетей", Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
			{Name: "youtube"}, {Name: "telegram"},
		}}},
		{"набор убран", Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
			{Name: "youtube"},
		}}},
	}
	for _, tc := range others {
		if Fingerprint(tc.cfg) == Fingerprint(base) {
			t.Errorf("%s: отпечаток не изменился", tc.name)
		}
	}
}

// testCatalog — каталог для проверок имён.
func testCatalog() *geosite.Catalog {
	return geosite.NewForTest(
		[]string{"youtube", "telegram", "github", "netflix@ads"},
		[]string{"telegram", "netflix@ads"},
	)
}

// TestValidate — таблица отказов и разрешений.
func TestValidate(t *testing.T) {
	cat := testCatalog()
	applied := []Set{{Name: "было-применено", IP: true}}

	cases := []struct {
		name    string
		cfg     Config
		cat     *geosite.Catalog
		wantErr bool
		// wantIs — какой сентинел обязан опознаваться (nil — любой отказ).
		wantIs error
		// wantUnknown — отказ обязан быть *UnknownSetsError.
		wantUnknown bool
		// wantIn — что обязано быть в тексте ошибки.
		wantIn []string
	}{
		{name: "только эти, имена из каталога", cat: cat,
			cfg: Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{{Name: "youtube"}}}},
		{name: "кроме этих, наборов ноль", cat: cat,
			cfg: Config{Policy: PolicyExcept, Download: DownloadTunnel}},
		{name: "из профиля без наборов", cat: cat,
			cfg: Config{Policy: PolicyProfile, Download: DownloadDirect}},
		{name: "уже применённое имя валидно и без каталога", cat: nil,
			cfg: Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{{Name: "было-применено"}}}},

		{name: "неизвестная политика", cat: cat, wantErr: true,
			cfg: Config{Policy: "maybe", Download: DownloadDirect}},
		{name: "неизвестное скачивание", cat: cat, wantErr: true,
			cfg: Config{Policy: PolicyOnly, Download: "carrier"}},
		{name: "из профиля с наборами", cat: cat, wantErr: true,
			cfg: Config{Policy: PolicyProfile, Download: DownloadDirect, Sets: []Set{{Name: "youtube"}}}},
		{name: "дубль имени", cat: cat, wantErr: true, wantIn: []string{"youtube"},
			cfg: Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
				{Name: "youtube"}, {Name: "telegram"}, {Name: "youtube"},
			}}},
		{name: "имён нет в каталоге", cat: cat, wantErr: true,
			wantUnknown: true, wantIn: []string{"нетакого", "итакого"},
			cfg: Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
				{Name: "youtube"}, {Name: "нетакого"}, {Name: "итакого"},
			}}},
		{name: "каталога нет, а имя новое", cat: nil, wantErr: true, wantIs: ErrNoCatalog,
			cfg: Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{{Name: "youtube"}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.cfg, tc.cat, applied)
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("Validate: %v, ожидалось разрешение", err)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate разрешил, ожидался отказ")
			}
			if tc.wantUnknown {
				var got *UnknownSetsError
				if !errors.As(err, &got) {
					t.Fatalf("ошибка %v не *UnknownSetsError", err)
				}
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("ошибка %v не опознана как %v", err, tc.wantIs)
			}
			for _, s := range tc.wantIn {
				if !strings.Contains(err.Error(), s) {
					t.Errorf("в тексте %q нет %q", err.Error(), s)
				}
			}
		})
	}
}

// TestValidateListsUnknownNames — неизвестные имена перечислены все и в
// порядке выбора: владелец должен увидеть, какие именно чипы убрать.
func TestValidateListsUnknownNames(t *testing.T) {
	err := Validate(Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
		{Name: "зет"}, {Name: "youtube"}, {Name: "аз"},
	}}, testCatalog(), nil)

	var unknown *UnknownSetsError
	if !errors.As(err, &unknown) {
		t.Fatalf("ошибка %v не *UnknownSetsError", err)
	}
	if want := []string{"зет", "аз"}; !reflect.DeepEqual(unknown.Names, want) {
		t.Errorf("перечислено %v, ожидалось %v", unknown.Names, want)
	}
}

// TestResolveTakesIPFromCatalog — признак подсетей проставляет каталог, а не
// панель: только он знает, есть ли имя в дереве geoip.
func TestResolveTakesIPFromCatalog(t *testing.T) {
	in := Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
		{Name: "youtube", IP: true}, // в geoip его нет — признак обязан слететь
		{Name: "telegram"},          // в geoip есть — признак обязан появиться
		{Name: "netflix@ads", IP: true},
	}}

	got := Resolve(in, testCatalog(), nil)
	want := []Set{{Name: "youtube"}, {Name: "telegram", IP: true}, {Name: "netflix@ads", IP: true}}
	if !reflect.DeepEqual(got.Sets, want) {
		t.Errorf("наборы %+v, ожидались %+v", got.Sets, want)
	}
	if !in.Sets[0].IP {
		t.Error("Resolve испортил исходную конфигурацию — она общая с вызывающим")
	}
}

// TestResolveFallsBackToApplied — каталога нет, но имя уже применено: признак
// подсетей берётся из файла. Иначе повторное применение без интернета молча
// потеряло бы половину набора telegram.
func TestResolveFallsBackToApplied(t *testing.T) {
	applied := []Set{{Name: "telegram", IP: true}, {Name: "youtube"}}
	in := Config{Policy: PolicyOnly, Download: DownloadDirect, Sets: []Set{
		{Name: "telegram"}, {Name: "youtube", IP: true},
	}}

	got := Resolve(in, nil, applied)
	want := []Set{{Name: "telegram", IP: true}, {Name: "youtube"}}
	if !reflect.DeepEqual(got.Sets, want) {
		t.Errorf("наборы %+v, ожидались %+v", got.Sets, want)
	}
}

// TestProviderNames — имена провайдеров: одно без подсетей, два с ними.
func TestProviderNames(t *testing.T) {
	if got, want := ProviderNames(Set{Name: "youtube"}), []string{"nm-geosite-youtube"}; !reflect.DeepEqual(got, want) {
		t.Errorf("без подсетей %v, ожидалось %v", got, want)
	}
	got := ProviderNames(Set{Name: "telegram", IP: true})
	want := []string{"nm-geosite-telegram", "nm-geoip-telegram"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("с подсетями %v, ожидалось %v", got, want)
	}
}

// TestVerify — что считать загруженным.
func TestVerify(t *testing.T) {
	at := time.Date(2026, 9, 7, 10, 0, 0, 0, time.UTC)
	live := map[string]nikki.RuleProvider{
		"nm-geosite-youtube":  {Name: "nm-geosite-youtube", RuleCount: 1284, UpdatedAt: at},
		"nm-geosite-telegram": {Name: "nm-geosite-telegram", RuleCount: 42, UpdatedAt: at},
		// Подсети telegram движок ни разу не скачал: updatedAt нулевой.
		"nm-geoip-telegram": {Name: "nm-geoip-telegram"},
		// Набор пуст в самом репозитории — но скачан. Это загруженный набор.
		"nm-geosite-пустой": {Name: "nm-geosite-пустой", RuleCount: 0, UpdatedAt: at},
	}

	sets := []Set{
		{Name: "youtube"},
		{Name: "telegram", IP: true},
		{Name: "пустой"},
		{Name: "которого нет"},
	}

	loaded, missing := Verify(sets, live)
	wantLoaded := []Set{{Name: "youtube"}, {Name: "пустой"}}
	wantMissing := []Set{{Name: "telegram", IP: true}, {Name: "которого нет"}}
	if !reflect.DeepEqual(loaded, wantLoaded) {
		t.Errorf("загружены %+v, ожидались %+v", loaded, wantLoaded)
	}
	if !reflect.DeepEqual(missing, wantMissing) {
		t.Errorf("не загружены %+v, ожидались %+v", missing, wantMissing)
	}
}

// TestVerifyNeverReturnsNil — пустые срезы, а не nil: вызывающий кладёт их в
// JSON, и nil стал бы там null вместо [].
func TestVerifyNeverReturnsNil(t *testing.T) {
	loaded, missing := Verify(nil, nil)
	if loaded == nil || missing == nil {
		t.Errorf("Verify вернул nil: loaded=%v missing=%v", loaded, missing)
	}
	if len(loaded) != 0 || len(missing) != 0 {
		t.Errorf("Verify на пустом входе вернул %v/%v", loaded, missing)
	}
}
