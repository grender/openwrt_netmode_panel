package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"netmoded/internal/geosite"
	"netmoded/internal/nikki"
	"netmoded/internal/rulesets"
)

// handleRulesetsGet — что владелец выбрал и что из этого реально работает.
//
// Ответ склеен из двух источников, и они отвечают на РАЗНЫЕ вопросы: файл
// mixin.yaml говорит, какие наборы выбраны, а Clash API — какие из них
// движок скачал. Слить их в одно поле нельзя: «выбран, но не скачался» —
// самая частая жалоба владельца («включил, не работает»), и увидеть её
// можно только там, где эти два ответа стоят рядом.
func (s *Server) handleRulesetsGet(w http.ResponseWriter, r *http.Request) {
	cfg, foreign, herr := s.readMixin()
	if herr != nil {
		herr.send(w)
		return
	}
	writeJSON(w, http.StatusOK, s.rulesetsBody(r.Context(), cfg, foreign))
}

// readMixin читает выбор наборов с диска.
//
// Три исхода разделены по той же границе, что и в rulesets.Parse, и разница
// между ними не косметическая:
//
//   - файла нет — свежая установка, «правила из профиля». Это НОРМАЛЬНОЕ
//     состояние, а не отказ: nikki приезжает со своим профилем, и пока
//     наборов не выбирали, выбирать было нечего. 404 или 500 здесь показали
//     бы панели поломку там, где её нет;
//   - файл чужой — трогать нельзя, и панель обязана это показать, а не
//     предложить кнопку «Применить», которая молча затрёт чужую работу;
//   - файл наш, но испорчен — 500. Отпечаток врал бы, а PUT с ним записал
//     бы поверх непонятно чего.
func (s *Server) readMixin() (rulesets.Config, bool, *httpErr) {
	profile := rulesets.Config{Policy: rulesets.PolicyProfile, Download: rulesets.DownloadDirect}

	b, err := os.ReadFile(s.cfg.MixinPath)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return profile, false, nil
	case err != nil:
		// Не mixin_corrupt: файл не прочитан вовсе, о его содержимом мы
		// ничего не знаем. Права, каталог, ФС только для чтения — это
		// чинится не «примените заново».
		return profile, false, &httpErr{http.StatusInternalServerError, "read_failed",
			"Файл наборов " + s.cfg.MixinPath + " не читается: " + err.Error()}
	}

	cfg, foreign, err := rulesets.Parse(b)
	if err != nil {
		// Ветвления по виду ошибки нет намеренно: Parse отдаёт только
		// ErrCorrupt, и вторая ветка была бы недостижимой строкой, про
		// которую через полгода никто не скажет, чем она отличается.
		return profile, false, &httpErr{http.StatusInternalServerError, "mixin_corrupt",
			"Файл наборов " + s.cfg.MixinPath + " повреждён: " + err.Error()}
	}
	return cfg, foreign, nil
}

// rulesetsBody собирает тело ответа.
//
// live — отвечал ли движок. Это ОТВЕТ, а не отказ: выбор владельца лежит в
// нашем файле и читается без mihomo, поэтому 503 здесь означал бы, что при
// упавшем движке панель не показывает даже того, что сама записала, — и
// владелец решил бы, что потерял настройку.
//
// Отсюда же трёхзначные loaded/rules/updated_at: при live:false они строго
// null, а не false и не ноль. «Набор не загружен» и «неизвестно, загружен
// ли» — разные утверждения, и подменять второе первым значит соврать ровно
// там, где владелец ищет причину неработающего обхода.
func (s *Server) rulesetsBody(ctx context.Context, cfg rulesets.Config, foreign bool) map[string]any {
	var live map[string]nikki.RuleProvider
	haveLive := false
	if s.nikki != nil {
		got, err := s.nikki.RuleProviders(ctx)
		if err == nil {
			live, haveLive = got, true
		} else {
			// Единственный след причины. Наружу уезжает только
			// live:false, и по нему «движок лёг» неотличимо от
			// «ответил не тем»; без этой строки владелец, у которого
			// наборы вечно «неизвестно», не узнает даже, куда смотреть.
			s.logf("наборы geosite: провайдеры правил не прочитаны: %v", err)
		}
	}

	// «Загружен» считает rulesets.Verify, а не этот файл: правило
	// («загружены ВСЕ провайдеры набора, у каждого ненулевое время») живёт
	// в пакете, который эти провайдеры и порождает. Второй экземпляр
	// правила здесь разошёлся бы с ним от первой правки — и панель
	// показывала бы готовым набор, который движок считает недокачанным.
	loadedNames := make(map[string]bool, len(cfg.Sets))
	if haveLive {
		loaded, _ := rulesets.Verify(cfg.Sets, live)
		for _, set := range loaded {
			loadedNames[set.Name] = true
		}
	}

	// Пустой массив, а не nil: панель перебирает sets без проверки на null.
	sets := make([]map[string]any, 0, len(cfg.Sets))
	for _, set := range cfg.Sets {
		item := map[string]any{
			"name": set.Name,
			"ip":   set.IP,
			// Ключи присутствуют ВСЕГДА, пусть и с null: отсутствие ключа
			// панель не отличила бы от отсутствия набора.
			"loaded":     nil,
			"rules":      nil,
			"updated_at": nil,
		}
		if haveLive {
			ok := loadedNames[set.Name]
			item["loaded"] = ok
			// Правила суммируются по всем провайдерам набора, которые
			// движок показывает, — включая недокачанные. Число это
			// справка, а не критерий: три набора в репозитории пусты и
			// скачиваются с нулём правил.
			rules := 0
			for _, name := range rulesets.ProviderNames(set) {
				if p, found := live[name]; found {
					rules += p.RuleCount
				}
			}
			item["rules"] = rules
			if ok {
				// Время доменного провайдера: он есть у любого набора, а
				// у набора с подсетями оба времени всё равно ставит одно
				// и то же скачивание.
				item["updated_at"] = live[rulesets.ProviderSitePrefix+set.Name].UpdatedAt.UTC().Format(time.RFC3339)
			}
		}
		sets = append(sets, item)
	}

	return map[string]any{
		// Отпечаток есть и у пустого выбора: без него первый же PUT панели
		// нечем сопроводить в If-Match.
		"fingerprint": rulesets.Fingerprint(cfg),
		"policy":      string(cfg.Policy),
		"download":    string(cfg.Download),
		// Имя группы «в туннель» отдаётся, а не зашивается в панель: оно
		// одно на весь профиль mihomo, и разъехаться эти два места не
		// должны.
		"tunnel_group": rulesets.TunnelGroup,
		"sets":         sets,
		"live":         haveLive,
		"foreign":      foreign,
	}
}

// RulesetProviderNames — имена провайдеров правил, которые порождает нынешний
// выбор.
//
// Экспортируется ради дев-стенда (cmd/netmoded-dev): его подделка Clash API
// обязана показывать те же имена, что записаны в mixin.yaml, иначе на стенде
// каждый набор вечно «не загрузился» и вкладку наборов невозможно ни
// нарисовать, ни отладить.
//
// Ошибка чтения даёт nil, а не отказ: стенд не место для разбора причин, а
// пустой список провайдеров он переживает — это состояние свежей установки.
//
// Контекст в сигнатуре, хотя чтение файла его не спрашивает: стенд зовёт
// этот метод рядом с вызовами Clash API, и метод без контекста пришлось бы
// звать по-особому — ради экономии одного параметра.
func (s *Server) RulesetProviderNames(context.Context) []string {
	cfg, foreign, herr := s.readMixin()
	if herr != nil || foreign {
		return nil
	}
	var names []string
	for _, set := range cfg.Sets {
		names = append(names, rulesets.ProviderNames(set)...)
	}
	return names
}

// handleRulesetsCatalog — список имён наборов из памяти демона.
//
// Список ЖИВОЙ (читается с GitHub), потому что имена живут в чужом
// репозитории и меняются без нашего участия. Отсюда два состояния, которых
// у прочих ответов этого API не бывает:
//
//   - stale — снимок старше TTL, обновление уже ушло в фон. Список
//     отдаётся сразу: отставание в именах на несколько часов никого не
//     трогает, а секунда ожидания — трогает;
//   - 503 catalog_unavailable — снимка нет вовсе и получить не удалось.
//     Отдельный код, потому что чинится это аплинком или ожиданием, а не
//     нами; уже применённые наборы при этом продолжают показываться —
//     они читаются из файла и каталога не спрашивают.
func (s *Server) handleRulesetsCatalog(w http.ResponseWriter, r *http.Request) {
	cat, err := s.catalog.Get(r.Context())
	if cat == nil {
		// Причина в тексте: 403 при исчерпанном лимите GitHub и таймаут
		// аплинка чинятся по-разному, и владелец пойдёт разбираться
		// именно с этой строкой.
		writeErr(w, http.StatusServiceUnavailable, "catalog_unavailable",
			"Список наборов geosite не загружен: "+err.Error())
		return
	}

	// Час кэша, а не no-store: тело большое, меняется несколько раз в
	// сутки, и повторное открытие вкладки обязано стоить 304, а не 60 КБ.
	servePrepared(w, r, s.catalogFile(cat, s.catalog.Stale(cat)), "private, max-age=3600")
}

// catalogFile — готовое тело каталога, собранное не чаще раза на снимок.
//
// Ключ включает признак просроченности, а не только версию снимка. Иначе
// при лежащем GitHub тело, собранное свежим, отдавалось бы со stale:false и
// через сутки — то есть врало бы ровно в том поле, ради которого оно
// заведено: владелец смотрит на stale, когда не находит в списке имя,
// которое в репозитории уже есть.
func (s *Server) catalogFile(cat *geosite.Catalog, stale bool) *panelFile {
	key := cat.Commit + "@" + cat.FetchedAt.UTC().Format(time.RFC3339Nano)
	if stale {
		key += "@stale"
	}

	s.catalogBody.mu.Lock()
	defer s.catalogBody.mu.Unlock()
	if s.catalogBody.f != nil && s.catalogBody.key == key {
		return s.catalogBody.f
	}

	ip := cat.IPNames()
	if ip == nil {
		// Пустой массив, а не null: панель перебирает поле как список.
		ip = []string{}
	}
	body := map[string]any{
		// Коммит и время — единственный способ понять, насколько список
		// отстал; панель показывает их в подписи вкладки.
		"commit":     cat.Commit,
		"fetched_at": cat.FetchedAt.UTC().Format(time.RFC3339),
		"stale":      stale,
		"names":      cat.Names,
		"ip":         ip,
		"packs":      packsIn(cat),
	}

	// Отступ тот же, что у writeJSON: тело каталога читают глазами в
	// devtools ровно так же, как остальные ответы.
	b, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		// Сериализация карты строк и срезов строк отказать не может;
		// если это всё-таки случилось, отдать полтела хуже, чем ничего.
		s.logf("каталог geosite: тело не собралось: %v", err)
		return preparedFile([]byte("{}\n"), "application/json; charset=utf-8")
	}
	f := preparedFile(append(b, '\n'), "application/json; charset=utf-8")

	s.catalogBody.key = key
	s.catalogBody.f = f
	return f
}

// packsIn — подборки, очищенные по каталогу.
//
// Имя из пака могло исчезнуть из репозитория: предлагать его на первом
// экране значило бы предлагать набор, который отобьётся при сохранении.
// Пак, от которого ничего не осталось, исчезает целиком — пустая рубрика в
// панели хуже, чем её отсутствие.
func packsIn(cat *geosite.Catalog) []geosite.Pack {
	out := make([]geosite.Pack, 0, len(geosite.Packs))
	for _, p := range geosite.Packs {
		sets := make([]string, 0, len(p.Sets))
		for _, name := range p.Sets {
			if cat.Has(name) {
				sets = append(sets, name)
			}
		}
		if len(sets) == 0 {
			continue
		}
		out = append(out, geosite.Pack{ID: p.ID, Sets: sets})
	}
	return out
}
