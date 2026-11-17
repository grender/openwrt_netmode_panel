package subs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"netmoded/internal/atomicfile"
	"netmoded/internal/happ"
)

// ErrNotConfigured — адрес подписки не задан.
//
// Это СОСТОЯНИЕ, а не поломка: свежая установка живёт без подписки, демон
// при этом работает целиком, а панель показывает подсказку вместо кнопки
// (ADR-0031, код ответа subscription_not_configured). Отдельный сентинел
// нужен вызывающему, чтобы отличить «владелец ещё ничего не настроил» от
// «настроено, но не скачалось»: первое в журнал обновлений писать незачем,
// второе — обязательно.
var ErrNotConfigured = errors.New("subs: адрес подписки не задан")

// ErrNoNodes — разбор дал ноль узлов, файлы не тронуты.
//
// Предохранитель, унаследованный от прежнего скрипта happ2clash (SPEC §2) и
// ставший нашим вместе с конвертером. Перезапись пустым списком отняла бы у
// владельца рабочий туннель молча — движок принял бы файл без единого узла и
// не сказал бы ни слова. Сохранённый старый список даёт время разобраться.
var ErrNoNodes = errors.New("subs: подписка разобрана, но узлов в ней ноль; " +
	"старый список узлов СОХРАНЁН и продолжает работать")

// ErrProviderIgnored — движок перечитал провайдера без ошибки, но ни одного
// записанного узла не показал.
//
// 204 на PUT /providers/proxies — не «прочитал наш файл». Fetcher mihomo
// читает файл по СВОЕМУ пути, сверяет хэш с прежним и при совпадении молча
// отвечает успехом, ничего не меняя; а если файл прочитан, имена узлов может
// переписать override провайдера. В обоих случаях обновление отчиталось бы
// «ok, N узлов» по нашему разбору, а в панели не было бы ни одного из них —
// ровно то, что случилось на роутере 2026-09-06. Сверить список движка с
// записанным — единственный способ отличить это от успеха.
var ErrProviderIgnored = errors.New("subs: движок перечитал провайдера, но не показал " +
	"ни одного записанного узла")

// Updater — одно обновление подписки целиком: скачать, разобрать, положить
// файлы, попросить движок перечитать.
//
// Круг 3 (ADR-0001): решения здесь, а чужой формат — в happ, Clash API — в
// nikki. Оба внешних действия приходят функциями (Fetch, Reload), а не
// клиентами: так политика проверяется без сети и без движка.
type Updater struct {
	// URL — адрес подписки. Пусто или nil → ErrNotConfigured.
	//
	// ФУНКЦИЯ, а не строка, и по той же причине, по которой функциями
	// приходят Fetch и Reload: адрес — внешнее состояние, а не свойство
	// обновлятеля. С тех пор как его можно сменить из панели
	// (PUT /api/subscription, ADR-0034), поле-строка означало бы снимок,
	// сделанный при сборке графа: расписание держит Updater годами и
	// продолжало бы качать по старому адресу до перезапуска демона.
	// Читается в момент вызова — и читается ровно один раз за Update,
	// чтобы проверка на пустоту и само скачивание не разъехались, если
	// адрес сменят между ними.
	//
	// СЕКРЕТ: в строке лежит идентификатор владельца. Ни в один текст
	// ошибки этого пакета он не попадает — см. Update.
	URL func() string
	// ProviderPath — файл узлов для mihomo (/etc/nikki/run/providers/sub.yaml).
	ProviderPath string
	// ManifestPath — порядок и виды записей (/etc/netmoded/subscription.json).
	ManifestPath string
	// OutboundsPath — исходные outbounds узлов для подписки телефонов
	// (/etc/netmoded/outbounds.json, happ.RenderOutbounds). Пусто — не
	// пишется: обновление работает как раньше.
	OutboundsPath string
	// Reload — перечитать провайдера движком (nikki.Client.ReloadProvider).
	Reload func(ctx context.Context) error
	// Proxies — имена узлов, которые движок держит в провайдере после
	// перечитывания (nikki.Client.ProviderProxies). Пусто → сверка
	// пропускается; в боевом графе обязано быть задано — см. ErrProviderIgnored.
	Proxies func(ctx context.Context) ([]string, error)
	// Logf — журнал демона для того, что не отказ, но стоит записи:
	// частичное расхождение списка движка с записанным. Пусто → тишина.
	Logf func(format string, args ...any)
	// Fetch — скачать подписку. Пусто → happ.Fetch.
	//
	// Подменяется только тестами: боевой путь обязан ходить через happ,
	// который умеет не показывать адрес в ошибках.
	Fetch func(ctx context.Context, url string) ([]byte, error)

	// Sources — все подписки (ADR-0052). nil — одна, из URL: так собраны
	// прежние тесты и так работал демон до второй подписки.
	Sources func() []SourceConf
	// CacheDir — каталог кэшей источников. Пусто — кэша нет: отказавший
	// источник в список не попадает.
	CacheDir string
	// FetchWith — подмена скачивания, видящая ID устройства. Сильнее Fetch.
	FetchWith func(ctx context.Context, url string, opts happ.FetchOpts) ([]byte, error)
	// Now — часы для итогов источников; nil — time.Now.
	Now func() time.Time
}

// SourceConf — одна подписка: откуда качать и как представляться.
type SourceConf struct {
	ID    string
	Label string
	URL   string
	// HWID — ID устройства, если этому провайдеру его надо слать; иначе пусто.
	HWID string
}

// sources — настроенные источники с непустым адресом, в порядке слияния.
func (u *Updater) sources() []SourceConf {
	if u.Sources != nil {
		var out []SourceConf
		for _, s := range u.Sources() {
			if s.URL != "" {
				out = append(out, s)
			}
		}
		return out
	}
	if u.URL != nil {
		if url := u.URL(); url != "" {
			return []SourceConf{{ID: happ.MainSource, URL: url}}
		}
	}
	return nil
}

func (u *Updater) fetchOne(ctx context.Context, s SourceConf) ([]byte, error) {
	switch {
	case u.FetchWith != nil:
		return u.FetchWith(ctx, s.URL, happ.FetchOpts{HWID: s.HWID})
	case u.Fetch != nil:
		return u.Fetch(ctx, s.URL)
	default:
		return happ.FetchWith(ctx, s.URL, happ.FetchOpts{HWID: s.HWID})
	}
}

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

// Update скачивает все подписки, сливает их в один список и отдаёт движку.
//
// Источник один — поведение прежнее, до буквы: отказ скачивания или ноль
// узлов возвращают ошибку и не трогают ни одного файла.
//
// Источников несколько (ADR-0052): каждый качается и разбирается отдельно.
// Удачный кладётся в свой кэш; неудачный берётся из кэша прошлой удачи, и
// его отказ уезжает в Summary.Partial. Ошибкой обновление становится, только
// когда не скачался НИ ОДИН: тогда файлы не трогаются, как и раньше.
func (u *Updater) Update(ctx context.Context) (happ.Summary, error) {
	srcs := u.sources()
	if len(srcs) == 0 {
		return happ.Summary{}, ErrNotConfigured
	}
	if u.Reload == nil {
		return happ.Summary{}, errors.New("subs: перезагрузка провайдера не настроена")
	}

	var (
		merged   []happ.Source
		partial  []string
		firstErr error
		firstSum happ.Summary
		unknown  []string
		fresh    int
	)
	for _, src := range srcs {
		entries, sum, err := u.fetchSource(ctx, src)
		unknown = append(unknown, sum.UnknownKeys...)
		old, cached := loadCache(u.CacheDir, src.ID)
		res := SourceResult{ID: src.ID, At: u.now().UTC()}

		if err == nil {
			fresh++
			t := res.At
			res.Status, res.Nodes, res.OKAt = "ok", sum.Nodes, &t
			if cerr := saveCache(u.CacheDir, cacheFile{SourceResult: res, Entries: toCacheEntries(entries)}); cerr != nil && u.Logf != nil {
				u.Logf("подписка %s: кэш не записан: %v", sourceName(src), cerr)
			}
			merged = append(merged, happ.Source{ID: src.ID, Label: src.Label, Entries: entries})
			continue
		}

		if firstErr == nil {
			firstErr, firstSum = err, sum
		}
		partial = append(partial, sourceName(src)+": "+err.Error())
		res.Status, res.Error = "fail", err.Error()
		if cached {
			// Узлы прошлой удачи остаются; меняется только итог попытки.
			res.Nodes, res.OKAt = old.Nodes, old.OKAt
			old.SourceResult = res
			if cerr := saveCache(u.CacheDir, old); cerr != nil && u.Logf != nil {
				u.Logf("подписка %s: итог не записан: %v", sourceName(src), cerr)
			}
			merged = append(merged, happ.Source{ID: src.ID, Label: src.Label, Entries: old.entries()})
		} else if cerr := saveCache(u.CacheDir, cacheFile{SourceResult: res}); cerr != nil && u.Logf != nil {
			u.Logf("подписка %s: итог не записан: %v", sourceName(src), cerr)
		}
	}

	if fresh == 0 {
		// Не скачалось ничего: файлы остаются как были. У единственного
		// источника ошибка уходит как есть — её текст и вид видят журнал и тесты.
		if len(srcs) == 1 {
			return firstSum, firstErr
		}
		return firstSum, fmt.Errorf("subs: не скачалась ни одна подписка: %s", strings.Join(partial, "; "))
	}

	sum, err := u.publish(ctx, merged)
	sum.UnknownKeys = uniqueSorted(unknown)
	sum.Partial = partial
	return sum, err
}

// Rebuild пересобирает общий список из кэшей, без сети: источник удалён
// или переименован, а качать остальные заново незачем.
func (u *Updater) Rebuild(ctx context.Context) (happ.Summary, error) {
	if u.Reload == nil {
		return happ.Summary{}, errors.New("subs: перезагрузка провайдера не настроена")
	}
	var merged []happ.Source
	for _, src := range u.sources() {
		if c, ok := loadCache(u.CacheDir, src.ID); ok && len(c.Entries) > 0 {
			merged = append(merged, happ.Source{ID: src.ID, Label: src.Label, Entries: c.entries()})
		}
	}
	return u.publish(ctx, merged)
}

// Results — итоги последней попытки по каждому настроенному источнику, в
// порядке слияния. Источник, который ещё ни разу не пробовали, — с пустым
// Status.
func (u *Updater) Results() []SourceResult {
	var all []SourceConf
	if u.Sources != nil {
		all = u.Sources()
	} else {
		all = u.sources()
	}
	out := make([]SourceResult, 0, len(all))
	for _, src := range all {
		if c, ok := loadCache(u.CacheDir, src.ID); ok {
			out = append(out, c.SourceResult)
		} else {
			out = append(out, SourceResult{ID: src.ID})
		}
	}
	return out
}

func sourceName(s SourceConf) string {
	if s.Label != "" {
		return s.Label
	}
	return s.ID
}

func uniqueSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, v := range in {
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// fetchSource — скачать и разобрать одну подписку. Ноль узлов — отказ
// источника: пустой ответ не должен вытеснять его прошлые узлы.
func (u *Updater) fetchSource(ctx context.Context, src SourceConf) ([]happ.Entry, happ.Summary, error) {
	raw, err := u.fetchOne(ctx, src)
	if err != nil {
		return nil, happ.Summary{}, err
	}
	entries, err := happ.Parse(raw)
	if err != nil {
		return nil, happ.Summary{}, fmt.Errorf("subs: подписка не разбирается: %w", err)
	}
	sum := happ.Summarize(entries)
	if sum.Nodes == 0 {
		return nil, sum, ErrNoNodes
	}
	return entries, sum, nil
}

// publish сливает источники, пишет три файла и просит движок их перечитать.
//
// Порядок записи — провайдер, манифест, outbounds, перечитка, сверка — не
// менять: он разобран в комментарии к Updater.
func (u *Updater) publish(ctx context.Context, sources []happ.Source) (happ.Summary, error) {
	entries, renamed := happ.Merge(sources)
	sum := happ.Summarize(entries)
	if sum.Nodes == 0 {
		return sum, ErrNoNodes
	}
	if len(renamed) > 0 && u.Logf != nil {
		u.Logf("подписки: имена совпали с более ранней подпиской и получили хвост: %s. "+
			"Хвост держится, пока оригинал есть в ранней подписке", strings.Join(renamed, "; "))
	}

	if err := atomicfile.Write(u.ProviderPath, happ.Render(entries), filePerm); err != nil {
		return sum, fmt.Errorf("subs: файл провайдера: %w", err)
	}

	manifest, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return sum, fmt.Errorf("subs: манифест не сериализуется: %w", err)
	}
	if err := atomicfile.Write(u.ManifestPath, append(manifest, '\n'), filePerm); err != nil {
		return sum, fmt.Errorf("subs: манифест: %w", err)
	}
	if u.OutboundsPath != "" {
		if err := atomicfile.Write(u.OutboundsPath, happ.RenderOutbounds(entries), filePerm); err != nil {
			return sum, fmt.Errorf("subs: outbounds для подписки телефонов: %w", err)
		}
	}

	if err := u.Reload(ctx); err != nil {
		return sum, fmt.Errorf(
			"subs: файлы записаны (%d узлов), но движок провайдера не перечитал: %w",
			sum.Nodes, err)
	}

	if err := u.verifyEngine(ctx, entries, sum.Nodes); err != nil {
		return sum, err
	}
	return sum, nil
}

// verifyEngine сверяет список узлов движка с тем, что записано.
//
// Ни одного совпадения — отказ ErrProviderIgnored с двумя тройками имён:
// по ним разница видна сразу (префикс от override? старые имена из другого
// файла?). Часть совпала — не отказ, а строка в журнал: узлы мог убрать
// filter провайдера, и это предмет разбора, а не поломка. Запрос не удался —
// отказ с оговоркой, что файлы новые и PUT прошёл: чинить надо не запись.
func (u *Updater) verifyEngine(ctx context.Context, entries []happ.Entry, nodes int) error {
	if u.Proxies == nil {
		return nil
	}
	got, err := u.Proxies(ctx)
	if err != nil {
		return fmt.Errorf("subs: файлы записаны (%d узлов) и движок их перечитал, "+
			"а проверить его список не удалось: %w", nodes, err)
	}

	engine := make(map[string]bool, len(got))
	for _, n := range got {
		engine[n] = true
	}
	var written, missing []string
	for _, e := range entries {
		if e.Kind != happ.KindNode {
			continue
		}
		written = append(written, e.Name)
		if !engine[e.Name] {
			missing = append(missing, e.Name)
		}
	}

	switch {
	case len(written) == 0:
		return nil
	case len(missing) == len(written):
		return fmt.Errorf("%w: PUT прошёл (204) и файлы новые, но у движка %d узлов, "+
			"первые: %s; мы записали %d, первые: %s — он читает другой файл "+
			"(path провайдера в профиле) или переписывает имена (override)",
			ErrProviderIgnored, len(got), firstThree(got), len(written), firstThree(written))
	case len(missing) > 0:
		if u.Logf != nil {
			u.Logf("движок показал %d узлов из %d записанных; нет, например: %s "+
				"(filter провайдера? проверьте профиль)",
				len(written)-len(missing), len(written), firstThree(missing))
		}
	}
	return nil
}

// firstThree — до трёх имён через запятую; для текста ошибки, а не для машины.
func firstThree(names []string) string {
	if len(names) == 0 {
		return "(пусто)"
	}
	if len(names) > 3 {
		names = names[:3]
	}
	return strings.Join(names, ", ")
}

// filePerm — права на оба файла: 0600.
//
// Манифест лежит рядом с токеном демона, файл провайдера — рядом с конфигом
// nikki, у которого на этом роутере тоже 0600
// (docs/recon/raw/40-etc-config-perms.txt), и внутри него uuid абонента,
// то есть тот же секрет, что и в адресе подписки.
//
// НЕ ПРОВЕРЕНО НА ЖЕЛЕЗЕ: mihomo читает файл провайдера сам, и если на этом
// роутере он ходит не под root, 0600 закроет ему доступ — узлы не
// поднимутся, а причина будет выглядеть как ошибка разбора. Права придётся
// ослабить до 0640/0644 с группой nikki; проверяется одним обновлением на
// живом роутере (ADR-0031, «Чего не снимало железо»).
const filePerm = 0o600

// LoadManifest читает порядок и виды записей подписки.
//
// Отсутствие файла — НЕ ошибка, а валидное состояние свежей установки:
// обновление ещё ни разу не проходило. Отдаётся nil, nil, и subs.Order на
// пустом манифесте собирает список из одного движка — путь, который обязан
// работать, иначе демон выглядит сломанным ровно у того, у кого подписка не
// качается.
//
// Битый файл — ошибка. Считать его пустым значило бы бесшумно потерять
// порядок: список остался бы рабочим, но перетасованным, и владелец искал
// бы причину в провайдере.
func LoadManifest(path string) ([]happ.Entry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("subs: чтение манифеста: %w", err)
	}

	var entries []happ.Entry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("subs: манифест %s не разбирается: %w", path, err)
	}
	return entries, nil
}
