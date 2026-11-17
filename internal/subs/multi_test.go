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

// Несколько подписок (ADR-0052). Вторая подписка — та же запись с другим
// именем и адресом: этому пакету важен факт слияния, а не формат.
var secondNode = strings.NewReplacer("🇧🇧 Браво", "🇫🇯 Alpha-1", "203.0.113.10", "203.0.113.20").Replace(oneNode)

const (
	urlMain   = "https://example.invalid/sub/main-secret"
	urlSecond = "https://second.example.invalid/second-secret"
	testHWID  = "5ec2e75ec2e75ec2e75ec2e75ec2e75ec2e75ec2e75ec2e75ec2e75ec2e75ec2"
)

type multi struct {
	up       *Updater
	dir      string
	bodies   map[string]string
	fail     map[string]error
	hwidSeen map[string]string
	reloads  int
	logs     []string
}

func newMulti(t *testing.T) *multi {
	t.Helper()
	dir := t.TempDir()
	m := &multi{dir: dir,
		bodies:   map[string]string{urlMain: oneNode, urlSecond: secondNode},
		fail:     map[string]error{},
		hwidSeen: map[string]string{},
	}
	m.up = &Updater{
		Sources: func() []SourceConf {
			return []SourceConf{
				{ID: happ.MainSource, Label: "Основная", URL: urlMain},
				{ID: "second", Label: "Вторая", URL: urlSecond, HWID: testHWID},
			}
		},
		CacheDir:      filepath.Join(dir, "subs"),
		ProviderPath:  filepath.Join(dir, "sub.yaml"),
		ManifestPath:  filepath.Join(dir, "subscription.json"),
		OutboundsPath: filepath.Join(dir, "outbounds.json"),
		Reload:        func(context.Context) error { m.reloads++; return nil },
		Logf:          func(f string, a ...any) { m.logs = append(m.logs, f) },
		FetchWith: func(_ context.Context, url string, o happ.FetchOpts) ([]byte, error) {
			m.hwidSeen[url] = o.HWID
			if err := m.fail[url]; err != nil {
				return nil, err
			}
			return []byte(m.bodies[url]), nil
		},
	}
	return m
}

func (m *multi) manifestNames(t *testing.T) []string {
	t.Helper()
	es, err := LoadManifest(m.up.ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name)
	}
	return out
}

func TestMultiMergesBothSources(t *testing.T) {
	m := newMulti(t)
	sum, err := m.up.Update(context.Background())
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if sum.Nodes != 2 || len(sum.Partial) != 0 {
		t.Fatalf("итог %+v", sum)
	}
	if got := strings.Join(m.manifestNames(t), "|"); got != "🇧🇧 Браво|🇫🇯 Alpha-1" {
		t.Errorf("манифест: %s", got)
	}
	// ID устройства уходит только тому источнику, которому задан.
	if m.hwidSeen[urlMain] != "" || m.hwidSeen[urlSecond] != testHWID {
		t.Errorf("ID устройства: %v", m.hwidSeen)
	}
	prov, _ := os.ReadFile(m.up.ProviderPath)
	if !strings.Contains(string(prov), "203.0.113.10") || !strings.Contains(string(prov), "203.0.113.20") {
		t.Error("в файле провайдера нет узлов обеих подписок")
	}
	var outb map[string]json.RawMessage
	b, _ := os.ReadFile(m.up.OutboundsPath)
	if json.Unmarshal(b, &outb) != nil || len(outb) != 2 {
		t.Errorf("outbounds телефонов: %d записей", len(outb))
	}
	// Кэш — секрет того же рода, что файл провайдера.
	st, err := os.Stat(filepath.Join(m.up.CacheDir, "second.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("кэш источника: %v %v", st, err)
	}
}

// Вторая подписка не скачалась — её узлы остаются из прошлой удачи.
func TestMultiKeepsFailedSourceFromCache(t *testing.T) {
	m := newMulti(t)
	if _, err := m.up.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.fail[urlSecond] = &happ.StatusError{Code: 403}
	sum, err := m.up.Update(context.Background())
	if err != nil {
		t.Fatalf("отказ одной подписки стал отказом обновления: %v", err)
	}
	if sum.Nodes != 2 {
		t.Errorf("узлов %d — узлы отказавшей подписки пропали", sum.Nodes)
	}
	if len(sum.Partial) != 1 || !strings.Contains(sum.Partial[0], "Вторая") || !strings.Contains(sum.Partial[0], "403") {
		t.Errorf("Partial = %v", sum.Partial)
	}
	res := m.up.Results()
	if len(res) != 2 || res[0].Status != "ok" || res[1].Status != "fail" || res[1].Nodes != 1 || res[1].OKAt == nil {
		t.Errorf("итоги: %+v", res)
	}
	for _, s := range append(sum.Partial, res[1].Error) {
		if strings.Contains(s, "second-secret") || strings.Contains(s, testHWID) {
			t.Errorf("секрет в тексте: %q", s)
		}
	}
}

// Первый запуск, вторая подписка не скачалась: первая всё равно выходит.
func TestMultiFirstRunWithOneFailed(t *testing.T) {
	m := newMulti(t)
	m.fail[urlSecond] = happ.ErrNeedsDeviceID
	sum, err := m.up.Update(context.Background())
	if err != nil || sum.Nodes != 1 || len(sum.Partial) != 1 {
		t.Fatalf("итог %+v, ошибка %v", sum, err)
	}
	if res := m.up.Results(); res[1].Status != "fail" || res[1].Nodes != 0 || res[1].OKAt != nil {
		t.Errorf("итог второй: %+v", res[1])
	}
}

// Не скачалась ни одна — файлы не трогаются, обновление — отказ.
func TestMultiAllFailedTouchesNothing(t *testing.T) {
	m := newMulti(t)
	if _, err := m.up.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(m.up.ProviderPath)
	reloads := m.reloads
	m.fail[urlMain] = errors.New("сеть лежит")
	m.fail[urlSecond] = errors.New("сеть лежит")
	_, err := m.up.Update(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Основная") || !strings.Contains(err.Error(), "Вторая") {
		t.Fatalf("ошибка %v", err)
	}
	after, _ := os.ReadFile(m.up.ProviderPath)
	if string(before) != string(after) || m.reloads != reloads {
		t.Error("при полном отказе файл провайдера или движок тронуты")
	}
}

// Пустой ответ источника не вытесняет его прошлые узлы.
func TestMultiZeroNodesIsSourceFailure(t *testing.T) {
	m := newMulti(t)
	if _, err := m.up.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.bodies[urlSecond] = zeroNodes
	sum, err := m.up.Update(context.Background())
	if err != nil || sum.Nodes != 2 || len(sum.Partial) != 1 {
		t.Fatalf("итог %+v, ошибка %v", sum, err)
	}
}

// Источник убрали из настроек: Rebuild собирает список из кэшей без сети.
func TestMultiRebuildWithoutNetwork(t *testing.T) {
	m := newMulti(t)
	if _, err := m.up.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.up.Sources = func() []SourceConf {
		return []SourceConf{{ID: happ.MainSource, Label: "Основная", URL: urlMain}}
	}
	m.up.FetchWith = func(context.Context, string, happ.FetchOpts) ([]byte, error) {
		t.Fatal("Rebuild пошёл в сеть")
		return nil, nil
	}
	if err := DropCache(m.up.CacheDir, "second"); err != nil {
		t.Fatal(err)
	}
	sum, err := m.up.Rebuild(context.Background())
	if err != nil || sum.Nodes != 1 {
		t.Fatalf("итог %+v, ошибка %v", sum, err)
	}
	if got := strings.Join(m.manifestNames(t), "|"); got != "🇧🇧 Браво" {
		t.Errorf("манифест: %s", got)
	}
}

// Кэш переживает круг: узел возвращается в файл провайдера тем же.
func TestCacheRoundTripKeepsNodeForEngine(t *testing.T) {
	m := newMulti(t)
	if _, err := m.up.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	fresh, _ := os.ReadFile(m.up.ProviderPath)
	m.fail[urlMain] = errors.New("сеть лежит") // main — из кэша, second — свежий
	if _, err := m.up.Update(context.Background()); err != nil {
		t.Fatal(err)
	}
	cached, _ := os.ReadFile(m.up.ProviderPath)
	if string(fresh) != string(cached) {
		t.Errorf("файл провайдера из кэша отличается от свежего:\n%s\n---\n%s", fresh, cached)
	}
}

// Битый кэш — «кэша нет», а не отказ обновления.
func TestBrokenCacheIsNoCache(t *testing.T) {
	m := newMulti(t)
	if err := os.MkdirAll(m.up.CacheDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(m.up.CacheDir, "second.json"), []byte("{не json"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.fail[urlSecond] = errors.New("сеть лежит")
	sum, err := m.up.Update(context.Background())
	if err != nil || sum.Nodes != 1 {
		t.Fatalf("итог %+v, ошибка %v", sum, err)
	}
	if ValidSourceID("../x") || ValidSourceID("") || !ValidSourceID("second2") {
		t.Error("ValidSourceID")
	}
}
