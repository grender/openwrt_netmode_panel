package geosite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	// defaultBaseURL — боевой корень GitHub REST API.
	//
	// Литерал подтверждён в docs/recon/evidence.json (geosite_catalog) и
	// сторожится scripts/check-evidence.sh: адрес чужой службы обязан быть
	// процитирован, а не вспомнен автором правки.
	defaultBaseURL = "https://api.github.com"

	// rootTreePath — корневое дерево ветки meta.
	//
	// Именно ветка, а не sha: GitHub принимает имя ветки как tree_sha и
	// отдаёт её текущее дерево. Иначе пришлось бы сначала спрашивать
	// коммит — лишний запрос из шестидесяти в час.
	//
	// БЕЗ ?recursive=1, и это не экономия, а необходимость: рекурсивный
	// ответ на этом репозитории обрезается (truncated) примерно на 94 000
	// записей из-за каталога asn/, то есть geosite в него не помещается
	// вовсе. Работает только пошаговый обход (замер 2026-09-07).
	rootTreePath = "/repos/MetaCubeX/meta-rules-dat/git/trees/meta"

	// geoDir — единственная запись корня, которая нам нужна.
	geoDir = "geo"

	// mrsSuffix — формат правил, который умеет читать mihomo.
	//
	// Каждое имя лежит в репозитории тремя файлами (.mrs, .yaml, .list);
	// считать все значило бы утроить список и предложить владельцу имена
	// «youtube.yaml», за которыми нет ничего, что движок скачает.
	mrsSuffix = ".mrs"

	// fetchTimeout — один запрос целиком, вместе с чтением тела.
	//
	// Пятнадцать секунд, а не шестьдесят, как у подписки: здесь запрос
	// стоит В ОЧЕРЕДИ перед панелью — владелец ждёт список на экране, и
	// бюджет побочного списка панели восемь секунд на попытку. Дольше
	// ждать бессмысленно: ответ всё равно опоздает.
	fetchTimeout = 15 * time.Second

	// maxBody — потолок тела ответа. Больше — отказ, а НЕ усечение.
	//
	// Та же логика, что у internal/happ/fetch.go и ADR-0022: тело
	// разбирается, а молча обрезанный JSON даёт загадочную жалобу парсера,
	// и владелец идёт искать поломку в нашем разборе вместо болтливого
	// сервера.
	//
	// Четыре мегабайта — примерно пятикратный запас против снятых
	// 2026-09-07 ~800 КБ на все четыре дерева, из которых самое большое
	// (geosite, 5698 записей) — около 600 КБ.
	maxBody = 4 << 20

	// userAgent — GitHub ОБЯЗЫВАЕТ его присылать: запрос без User-Agent
	// api.github.com отбивает кодом 403. Имя демона, а не браузера: по
	// журналу GitHub должно быть видно, кто ходит.
	userAgent = "netmoded"

	// acceptJSON — версия представления Git Trees API.
	//
	// Без него GitHub вправе отдать другое представление (или другую
	// версию схемы), и разбор поедет молча.
	acceptJSON = "application/vnd.github+json"
)

// ErrTruncated — GitHub обрезал дерево.
//
// Отказ, а не «сколько есть». Неполный каталог хуже отсутствующего: панель
// покажет список без части имён, а PUT отобьёт существующее имя владельца
// как несуществующее — и ошибку эту будет не отличить от опечатки.
var ErrTruncated = errors.New("geosite: GitHub обрезал дерево — каталог неполный")

// ErrTooLarge — ответ больше maxBody.
var ErrTooLarge = errors.New("geosite: ответ больше допустимого")

// ErrRedirect — GitHub ответил перенаправлением, и мы за ним не идём.
//
// Причина та же, что у подписки (internal/happ/fetch.go): проверка схемы
// смотрит только на исходный адрес, а http.Client по умолчанию сходил бы за
// «302 Location: http://…» до десяти раз и схему бы не перепроверил. Секрета
// в этих запросах нет, но подменённый ответ — это чужой список имён, который
// уедет в конфиг маршрутизации владельца.
var ErrRedirect = errors.New("geosite: GitHub отвечает перенаправлением; за ним не идём")

// errNotHTTPS — адрес каталога не по https при боевой настройке.
var errNotHTTPS = errors.New("geosite: адрес каталога обязан быть https")

// treeEntry — запись дерева в форме GitHub.
//
// Поля size и mode не читаются намеренно: имя даёт path, вид — type, а адрес
// поддерева — url. Остальное в снимок не попадает и разбирать его незачем.
type treeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"` // "blob" | "tree" | "commit"
	URL  string `json:"url"`
}

// tree — ответ Git Trees API.
//
// Truncated читается ОБЯЗАТЕЛЬНО: это единственный признак того, что список
// неполон. Отсутствие поля (false) — нормальный ответ.
type tree struct {
	SHA       string      `json:"sha"`
	Tree      []treeEntry `json:"tree"`
	Truncated bool        `json:"truncated"`
}

// base — корень API с учётом настройки.
func (c *Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimSuffix(c.BaseURL, "/")
	}
	return defaultBaseURL
}

// checkURL — правило схемы.
//
// https требуется ТОЛЬКО при боевой настройке (пустой BaseURL). Проверка
// стоит не ради самого корневого адреса — он константа и всегда https, — а
// ради адресов, которые GitHub присылает В ТЕЛЕ ответа: по url поддеревьев
// клиент ходит, и «url»: «http://…» увёл бы обход на открытый провод, где
// список имён можно подменить по дороге.
//
// Явно заданный BaseURL — стенд разработчика и httptest: там http разрешён,
// иначе ни поведение кэша, ни потолок тела, ни ETag нечем проверить без
// выхода в сеть. Цена ошибки — разработчик направит боевой демон на http;
// поле в конфиг владельца не выведено.
func (c *Client) checkURL(raw string) error {
	if c.BaseURL != "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("geosite: адрес %q не разбирается", raw)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("%w (получено: %s)", errNotHTTPS, raw)
	}
	return nil
}

// client — HTTP-клиент запросов.
//
// Боевой собирается один раз на Client, а не на запрос: четыре дерева
// читаются подряд, и новый клиент на каждое означал бы новое соединение и
// новое рукопожатие TLS с роутера.
func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.def == nil {
		c.def = &http.Client{
			Timeout: fetchTimeout,
			// Ни одного перенаправления, и своя ошибка вместо
			// http.ErrUseLastResponse: последний ответ на редиректе —
			// пустое тело с кодом 30x, и разбор упал бы на нём
			// загадочной жалобой парсера вместо честной причины.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return ErrRedirect
			},
		}
	}
	return c.def
}

// fetchTree читает одно дерево.
//
// etag непуст → запрос уходит с If-None-Match. Это главная экономия пакета:
// ответ 304 не тратит лимит в 60 запросов в час, то есть продление свежести
// каталога раз в шесть часов не стоит владельцу ничего.
func (c *Client) fetchTree(ctx context.Context, raw, etag string) (*tree, string, bool, error) {
	if err := c.checkURL(raw); err != nil {
		return nil, "", false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, "", false, fmt.Errorf("geosite: запрос к GitHub не собирается: %w", err)
	}
	req.Header.Set("Accept", acceptJSON)
	req.Header.Set("User-Agent", userAgent)
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := c.client().Do(req)
	if err != nil {
		// В отличие от подписки (internal/happ), адрес здесь не секрет:
		// это публичный репозиторий. Поэтому *url.Error оставляется как
		// есть — в отказе видно, какой именно запрос не прошёл.
		return nil, "", false, fmt.Errorf("geosite: GitHub не ответил: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		return nil, etag, true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Код называется в тексте: 403 при исчерпанном лимите и 404 при
		// переименованной ветке чинятся по-разному, и владелец увидит
		// ровно эту строку.
		return nil, "", false, fmt.Errorf("geosite: GitHub ответил кодом %d", resp.StatusCode)
	}

	// Читаем на байт больше потолка: если этот байт пришёл, тело в потолок
	// не уложилось. Отличить «ровно maxBody» от «больше» иначе нечем —
	// io.LimitReader об обрыве не сообщает.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, "", false, fmt.Errorf("geosite: тело ответа GitHub не дочитано: %w", err)
	}
	if len(body) > maxBody {
		return nil, "", false, fmt.Errorf("%w (%d Б)", ErrTooLarge, maxBody)
	}

	var t tree
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, "", false, fmt.Errorf("geosite: ответ GitHub не разбирается: %w", err)
	}
	if t.Truncated {
		return nil, "", false, ErrTruncated
	}
	return &t, resp.Header.Get("ETag"), false, nil
}

// refresh — одна загрузка каталога целиком.
//
// Все запросы под общим ctx и любой отказ отменяет всю загрузку: половина
// каталога — это неполный каталог, а он опаснее отсутствующего (см.
// ErrTruncated). Кэш трогается ТОЛЬКО при полном успехе.
func (c *Client) refresh(ctx context.Context) (*Catalog, error) {
	start := time.Now()

	// Снимок предыдущего состояния берётся один раз: ETag и то, что
	// сохранять при 304, обязаны быть из одной версии кэша.
	prev := c.Current()
	etag := ""
	if prev != nil {
		etag = prev.ETag
	}

	root, rootETag, notModified, err := c.fetchTree(ctx, c.base()+rootTreePath, etag)
	if err != nil {
		return nil, err
	}
	if notModified {
		if prev == nil {
			// 304 без нашего If-None-Match — сервер не тот, за кого
			// себя выдаёт: продлевать нечего, а отдать пустой каталог
			// значило бы соврать панели.
			return nil, errors.New("geosite: GitHub ответил 304 на запрос без ETag")
		}
		// Дерево не двигалось: имена и коммит те же, обновилось только
		// время — ради этого 304 и запрашивается.
		fresh := *prev
		fresh.FetchedAt = time.Now()
		c.store(&fresh)
		c.logf("каталог geosite: без изменений (304), %d имён, коммит %s, за %d мс",
			len(fresh.Names), shortSHA(fresh.Commit), time.Since(start).Milliseconds())
		return &fresh, nil
	}

	geoURL, err := subtreeURL(root, geoDir)
	if err != nil {
		return nil, err
	}
	geo, _, _, err := c.fetchTree(ctx, geoURL, "")
	if err != nil {
		return nil, err
	}

	siteURL, err := subtreeURL(geo, string(KindSite))
	if err != nil {
		return nil, err
	}
	site, _, _, err := c.fetchTree(ctx, siteURL, "")
	if err != nil {
		return nil, err
	}

	ipURL, err := subtreeURL(geo, string(KindIP))
	if err != nil {
		return nil, err
	}
	ipTree, _, _, err := c.fetchTree(ctx, ipURL, "")
	if err != nil {
		return nil, err
	}

	names := mrsNames(site)
	if len(names) == 0 {
		// Ноль имён — это не пустой каталог, это сломанный ответ: в
		// geo/geosite их 1899. Принять ноль значило бы отбить при
		// следующем PUT все имена владельца как несуществующие.
		return nil, errors.New("geosite: в дереве geosite нет ни одного файла " + mrsSuffix)
	}

	// ip — ПЕРЕСЕЧЕНИЕ: подсети не самостоятельный выбор, они лишь
	// дополняют набор доменов. Имя вроде «cn», которое есть только в
	// geoip, предлагать нечего — доменов за ним нет.
	ipAll := make(map[string]bool, len(ipTree.Tree))
	for _, n := range mrsNames(ipTree) {
		ipAll[n] = true
	}
	ip := make(map[string]bool)
	for _, n := range names {
		if ipAll[n] {
			ip[n] = true
		}
	}

	cat := newCatalog(root.SHA, rootETag, time.Now(), names, ip)
	c.store(cat)
	c.logf("каталог geosite: %d имён, %d с geoip, коммит %s, за %d мс",
		len(cat.Names), len(ip), shortSHA(cat.Commit), time.Since(start).Milliseconds())
	return cat, nil
}

// subtreeURL — адрес поддерева по имени записи.
//
// Берётся именно url из ответа, а не собирается путь: GitHub адресует
// поддеревья по их sha, и угадать этот адрес нельзя — он меняется с каждым
// изменением содержимого каталога.
func subtreeURL(t *tree, name string) (string, error) {
	for _, e := range t.Tree {
		if e.Type == "tree" && e.Path == name {
			if e.URL == "" {
				return "", fmt.Errorf("geosite: у дерева %q нет адреса", name)
			}
			return e.URL, nil
		}
	}
	// Пропавший каталог — это смена раскладки репозитория, и чинится она
	// правкой кода, а не повтором запроса. Поэтому отказ внятный.
	return "", fmt.Errorf("geosite: в дереве нет каталога %q — раскладка репозитория изменилась", name)
}

// mrsNames — имена наборов из дерева.
//
// Записи type != "blob" (подкаталоги) и файлы с другими суффиксами
// отбрасываются: за ними нет файла правил, который скачает mihomo.
func mrsNames(t *tree) []string {
	out := make([]string, 0, len(t.Tree)/3+1)
	for _, e := range t.Tree {
		if e.Type != "blob" || !strings.HasSuffix(e.Path, mrsSuffix) {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Path, mrsSuffix))
	}
	return out
}

// shortSHA — sha для журнала.
//
// Двенадцать знаков: столько же показывает git по умолчанию, и этого хватает,
// чтобы сличить каталог с состоянием ветки meta глазами.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
