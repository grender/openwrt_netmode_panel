package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"netmoded/internal/atomicfile"
	"netmoded/internal/geosite"
	"netmoded/internal/job"
	"netmoded/internal/nikki"
	"netmoded/internal/rulesets"
	"netmoded/internal/uci"
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

	f := s.catalogFile(cat, s.catalog.Stale(cat))
	if f == nil {
		// Сборка тела отказать не может (см. catalogFile), но если это
		// всё-таки случилось, 500 честнее пустого каталога: тот приехал
		// бы под тем же ETag и с тем же часом кэша, и вкладка показывала
		// бы «наборов нет» до конца часа, ничего не переспросив.
		writeErr(w, http.StatusInternalServerError, "internal",
			"Список наборов geosite не собрался в ответ — подробности в журнале демона.")
		return
	}
	// Час кэша, а не no-store: тело большое, меняется несколько раз в
	// сутки, и повторное открытие вкладки обязано стоить 304, а не 60 КБ.
	servePrepared(w, r, f, "private, max-age=3600")
}

// catalogFile — готовое тело каталога, собранное не чаще раза на снимок.
// nil — собрать не удалось.
//
// Ключ включает признак просроченности, а не только версию снимка. Иначе
// при лежащем GitHub тело, собранное свежим, отдавалось бы со stale:false и
// через сутки — то есть врало бы ровно в том поле, ради которого оно
// заведено: владелец смотрит на stale, когда не находит в списке имя,
// которое в репозитории уже есть.
//
// Время снимка в ключе — по той же причине, и цена его невелика. Меняется
// оно не на запрос, а на снимок: продление через 304 стоит одной пересборки
// раз в TTL, то есть раз в шесть часов. Убрать его из ключа значило бы
// сэкономить эту пересборку и показывать владельцу fetched_at, отставшее от
// действительности на те же шесть часов, — в поле, которое он читает именно
// затем, чтобы понять, насколько список отстал.
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
		// nil, а не пустой «{}»: обработчик ответит 500, и вкладка
		// переспросит, а не запомнит пустой каталог на час.
		s.logf("каталог geosite: тело не собралось: %v", err)
		return nil
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

const (
	// rulesetsETASec — сколько примерно занимает применение: запись файла,
	// коммит флага и netmode-apply укладываются в те же 15 с, что и смена
	// режима (SPEC §5).
	//
	// Это ОЦЕНКА для полосы в панели, а не потолок: ожидание докачки
	// наборов (rulesetsWait) может добавить сверху до двадцати секунд, и
	// джоб на медленном канале честно идёт дольше своего eta_sec. Врать в
	// другую сторону — ставить сюда 35 — хуже: обычное применение, когда
	// правила уже лежат в /etc/nikki/run/rules, укладывается в первые
	// секунды, и полоска ползла бы вхолостую при каждом нажатии.
	rulesetsETASec = 15
	// rulesetsWait — сколько ждать докачки наборов после перезапуска.
	// Двадцати секунд хватает mihomo, чтобы поднять Clash API и забрать
	// несколько .mrs с GitHub; что не успело — досчитается само по
	// суточному циклу провайдеров.
	rulesetsWait = 20 * time.Second
	// rulesetsPoll — шаг опроса.
	rulesetsPoll = time.Second
)

// mixinFlagOpt — nikki.mixin.mixin_file_content, то есть «склеивать ли
// mixin.yaml с профилем».
//
// Файл без этого флага лежит на диске мёртвым грузом: nikki.init его просто
// не читает. Поэтому запись файла и переключение флага — одна операция, а
// не две настройки, и разъехаться им нельзя.
const mixinFlagOpt = "mixin_file_content"

// handleRulesetsPut применяет выбор наборов.
//
// Медленная операция: файл на флеш, коммит UCI, перезапуск движка —
// значит джоб и 202, а не 200 с результатом (ADR-0013).
//
// Порядок проверок идёт от «запись бессмысленна» к «запись невозможна», и
// все они стоят ДО первой записи. Это не аккуратность, а единственный
// доступный нам вид отката: отката нет (ADR-0006), после uci commit
// отказаться уже нечем, а перезаписанный mixin.yaml не вернуть.
func (s *Server) handleRulesetsPut(w http.ResponseWriter, r *http.Request) {
	// 1. Форма тела.
	//
	// ip панель не присылает: знать, есть ли имя в дереве geoip, может
	// только каталог, и признак проставляет демон (Resolve ниже). Дай мы
	// его клиенту — вкладка, открытая до обновления каталога, теряла бы
	// половину набора молча.
	var in struct {
		Policy   string   `json:"policy"`
		Download string   `json:"download"`
		Sets     []string `json:"sets"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Тело запроса не разбирается как JSON")
		return
	}
	if in.Download == "" {
		// Умолчание, а не отказ: «откуда качать» спрашивают редко, и
		// требовать поле на каждой записи значило бы заставлять панель
		// помнить его ради одного случая из ста.
		in.Download = string(rulesets.DownloadDirect)
	}
	want := rulesets.Config{
		Policy:   rulesets.Policy(in.Policy),
		Download: rulesets.Download(in.Download),
		// Пустой срез, а не nil: дальше он идёт в Render и в отпечаток, и
		// разница «наборов нет» / «поле не пришло» там ничего не значит.
		Sets: make([]rulesets.Set, 0, len(in.Sets)),
	}
	for _, name := range in.Sets {
		want.Sets = append(want.Sets, rulesets.Set{Name: name})
	}

	// 2. Что лежит на диске сейчас.
	cur, foreign, herr := s.readMixin()
	if foreign {
		writeErr(w, http.StatusConflict, "foreign_mixin",
			"Файл "+s.cfg.MixinPath+" написан не нами. Перезаписать его значило бы "+
				"молча уничтожить чужую работу: уберите файл на роутере и повторите.")
		return
	}
	// Испорченный файл записи НЕ мешает, и это не поблажка. Его содержимое
	// нам неизвестно, значит и отпечаток его бессмыслен; а PUT как раз
	// переписывает файл целиком — то есть чинит ровно ту поломку, о которой
	// GET докладывает 500. Отбить здесь значило бы запереть владельца:
	// применить нельзя, потому что применённое нечитаемо.
	corrupt := herr != nil && herr.code == "mixin_corrupt"
	if herr != nil && !corrupt {
		herr.send(w)
		return
	}

	// 3. Отпечаток: не устарело ли представление клиента.
	//
	// У испорченного файла его не спрашиваем — сверять было бы не с чем, а
	// применённых наборов у него нет (cur пуст), и в проверке имён ниже он
	// участвует как свежая установка.
	//
	// Пробелы по краям срезаются: заголовок приезжает и из curl по ssh, где
	// хвостовой перевод строки — свойство копирования, а не ошибка (так же
	// поступают записи в wireless и в мост).
	if !corrupt {
		// Отсутствие заголовка и несовпадение — ОДИН код (лечится и то и
		// другое перечитыванием GET), но разные тексты: сказать про
		// «изменился, пока вы правили» тому, кто отпечатка не прислал
		// вовсе, значило бы отправить его искать вторую вкладку, которой
		// не было.
		got := strings.TrimSpace(r.Header.Get("If-Match"))
		switch {
		case got == "":
			writeErr(w, http.StatusConflict, "stale_rulesets",
				"Нужен заголовок If-Match с отпечатком из GET /api/nikki/rulesets: "+
					"без него запись не докажет, что видела нынешний выбор.")
			return
		case got != rulesets.Fingerprint(cur):
			writeErr(w, http.StatusConflict, "stale_rulesets",
				"Выбор наборов изменился, пока вы его правили: перечитайте "+
					"GET /api/nikki/rulesets и повторите с новым отпечатком.")
			return
		}
	}

	// 4. Каталог — только если он действительно нужен.
	//
	// Уже применённое имя валидно и без него: иначе повторное применение
	// без интернета — скажем, одна лишь смена «откуда качать» — отбивалось
	// бы на именах, которые сам же демон и записал. Синхронная загрузка
	// стоит секунд, и платить их за запись, которую проверять не по чему,
	// незачем.
	cat := s.catalog.Current()
	if needsCatalog(want.Sets, cat, cur.Sets) {
		// Get отдаёт снимок и при неудачном походе в сеть — его мог
		// принести кто-то другой, пока мы ходили; отказ означает, что
		// отдавать нечего вовсе.
		got, err := s.catalog.Get(r.Context())
		if err != nil && got == nil {
			// Причина в тексте: 403 при исчерпанном лимите GitHub и
			// отсутствие аплинка чинятся по-разному.
			writeErr(w, http.StatusServiceUnavailable, "catalog_unavailable",
				"Список наборов geosite не загружен, проверить новые имена нечем: "+err.Error())
			return
		}
		if got != nil {
			cat = got
		}
	}

	// 5. Форма выбора и существование имён.
	if err := rulesets.Validate(want, cat, cur.Sets); err != nil {
		var unknown *rulesets.UnknownSetsError
		if errors.As(err, &unknown) {
			// Имена перечислены: панель подсвечивает именно эти чипы, а
			// «неверный запрос» не сказало бы владельцу, какое из тридцати
			// имён он написал с опечаткой.
			writeErr(w, http.StatusBadRequest, "unknown_set",
				"Таких наборов нет в списке: "+strings.Join(unknown.Names, ", ")+
					". Имена берутся из репозитория MetaCubeX/meta-rules-dat, ветка meta.")
			return
		}
		// Сюда же попал бы rulesets.ErrNoCatalog, но попасть не может:
		// новые имена без каталога отбиты шагом 4, а без новых имён этот
		// сентинел не рождается.
		//
		// Приставка пакета срезается: «rulesets:» — метка для журнала и
		// греп, а владельцу она сообщает только то, что у нас есть файл с
		// таким именем.
		writeErr(w, http.StatusBadRequest, "bad_request",
			"Выбор наборов не принят: "+strings.TrimPrefix(err.Error(), "rulesets: "))
		return
	}
	want = rulesets.Resolve(want, cat, cur.Sets)

	// 6. Чужие незакоммиченные правки в nikki.
	//
	// `uci commit` публикует ВЕСЬ стейджинг пакета — своего и чужого не
	// различает. Закоммитив поверх чужого черновика, мы опубликовали бы
	// чужую работу под своим именем и в момент, который её автор не
	// выбирал (ADR-0011).
	changes, err := s.ex.UCIChanges(r.Context(), "nikki")
	if err != nil {
		writeErr(w, http.StatusServiceUnavailable, "uci_unavailable", err.Error())
		return
	}
	if uci.HasStagedChanges(changes) {
		writeErr(w, http.StatusConflict, "foreign_staged_changes",
			"В /etc/config/nikki есть незакоммиченные правки — вероятно, открыт LuCI. "+
				"Примените или отмените их, затем повторите.")
		return
	}

	// 7. Группа «в туннель».
	//
	// Правила RULE-SET,…,BYPASS без неё не применятся, и mihomo не
	// поднимется вовсе — то есть отказ обнаружился бы уже после записи
	// файла и перезапуска, обрывом обхода. Молчание движка при этом
	// пропускается: он имеет право лежать, и тогда проверить нечего, а
	// запретить из-за этого запись значило бы поставить выбор наборов в
	// зависимость от работающего mihomo. При profile группа не нужна
	// вовсе — правил мы не пишем.
	if want.Policy != rulesets.PolicyProfile && s.nikki != nil {
		if all, err := s.nikki.Proxies(r.Context()); err == nil {
			if _, ok := all[rulesets.TunnelGroup]; !ok {
				writeErr(w, http.StatusServiceUnavailable, "group_missing",
					"В профиле mihomo нет группы "+rulesets.TunnelGroup+
						" — правила наборов применить не к чему. Поправьте профиль.")
				return
			}
		}
	}

	// 8. Джоб. Arg пустой: применение наборов одно, уточнять в нём нечего.
	j, err := s.jobs.Start("rulesets", "", "Применение наборов geosite", rulesetsETASec,
		func(ctx context.Context) error { return s.applyRulesets(ctx, want) })
	if errors.Is(err, job.ErrBusy) {
		writeErr(w, http.StatusConflict, "job_busy",
			"Уже идёт другая операция. Дождитесь её завершения.")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"job": j})
}

// needsCatalog — есть ли среди присланных имён такие, которых нет ни в
// каталоге, ни среди уже применённых.
//
// Отдельная функция, а не догадка по ошибке Validate: там «нечего сверять»
// и «сверили, не нашли» уже слиты в один проход, а решение «идти ли в
// GitHub» принимается ДО него.
func needsCatalog(want []rulesets.Set, cat *geosite.Catalog, applied []rulesets.Set) bool {
	known := make(map[string]bool, len(applied))
	for _, set := range applied {
		known[set.Name] = true
	}
	for _, set := range want {
		if known[set.Name] {
			continue
		}
		if cat != nil && cat.Has(set.Name) {
			continue
		}
		return true
	}
	return false
}

// applyRulesets выполняет применение.
//
// Порядок шагов — от того, что переживает перезагрузку, к тому, что живёт
// до неё: файл, флаг, перезапуск. Обратный порядок означал бы перезапуск
// движка под старым файлом, то есть «Применено» в панели над правилами,
// которых mihomo не видел.
//
// Отката нет ни на одном шаге (ADR-0006): вернуть прежний файл — это вторая
// запись на флеш ради состояния, которого владелец не просил, а вернуть
// прежний флаг после успешного коммита нечем. Единственная отмена здесь —
// снятие СВОЕГО черновика UCI, который в систему ещё не уехал.
//
// Светодиод джоб не трогает: режим не меняется, и мигание «применяю» рядом
// с работающим туннелем означало бы поломку связи, которой нет (ADR-0013).
// Индикацию правит только разбор отказа netmode-apply (modeApplyFailed) —
// там она часть разбора кодов скрипта, и заводить ради неё второй экземпляр
// этого разбора было бы хуже.
func (s *Server) applyRulesets(ctx context.Context, want rulesets.Config) error {
	// a. Файл. Атомарно: читатель (nikki.init при склейке) не должен
	//    увидеть половину записи, а обрыв питания — оставить пустой файл.
	if err := atomicfile.Write(s.cfg.MixinPath, rulesets.Render(want), 0o644); err != nil {
		return fmt.Errorf("наборы не записаны, ничего не изменено: %w", err)
	}

	// b. Флаг.
	if err := s.switchMixinFlag(ctx, want.Policy); err != nil {
		return err
	}

	// c. Режим. netmode-apply nikki поднял бы движок, который владелец
	//    выключил намеренно. Выбор при этом уже на диске и вступит в силу
	//    при включении Nikki — на то он и лежит в файле, который nikki
	//    читает сам, без нашего участия.
	//
	//    Отказ чтения — именно отказ, а не «режим не nikki». Проглоченная
	//    ошибка дала бы пустую строку, то есть молчаливый пропуск
	//    перезапуска и «Применено» в панели над правилами, которых движок
	//    не перечитывал. Отличить это от настоящего успеха владельцу
	//    нечем: файл на месте, флаг на месте, туннель работает по-старому.
	mode, err := s.ex.UCIGet(ctx, "netmode", "main", "mode")
	if err != nil {
		return fmt.Errorf("наборы записаны в %s и склейка включена, но текущий режим не прочитался, "+
			"и Nikki не перезапущен — выбор вступит в силу при следующем включении режима. "+
			"Повторить применение безопасно: %w", s.cfg.MixinPath, err)
	}
	if mode != "nikki" {
		return nil
	}

	// d. Перезапуск. Своей таксономии отказов у наборов нет — берётся
	//    разбор кодов netmode-apply целиком: причины и советы там те же,
	//    а второй экземпляр этого разбора разошёлся бы с первым.
	if err := s.ex.ApplyMode(ctx, "nikki"); err != nil {
		return fmt.Errorf("наборы записаны, но Nikki не перезапустился: %w", s.modeApplyFailed("nikki", err))
	}

	// e. Сверять нечего: правил мы не писали.
	if want.Policy == rulesets.PolicyProfile || len(want.Sets) == 0 {
		return nil
	}

	// f. Движок после перезапуска отвечает не сразу, а ответив — качает.
	wait := s.awaitRuleProviders(ctx, want.Sets)
	if !wait.answered {
		// Сверять нечего вовсе. Три разных «нечего», и путать их нельзя:
		// чинятся они в трёх разных местах.
		switch {
		case errors.Is(wait.err, errNoNikkiClient):
			return fmt.Errorf("наборы записаны и Nikki перезапущен, но проверить их некому: %w", wait.err)
		case wait.stopped:
			return fmt.Errorf("наборы записаны и Nikki перезапущен, но ожидание движка прервано "+
				"(срок джоба или остановка демона) — загрузились ли наборы, неизвестно: %w", wait.err)
		default:
			// Причина последнего отказа уезжает целиком: 401 на неверном
			// секрете Clash API и connection refused чинятся в разных
			// местах, а «не ответил» одинаково подходит обоим.
			return fmt.Errorf("наборы записаны и Nikki перезапущен, но Clash API не ответил за %s — "+
				"загрузились ли наборы, неизвестно: %w", s.rulesetsWait, wait.err)
		}
	}

	// g. Сверка с движком — по ПОСЛЕДНЕМУ снимку ожидания.
	//
	// «Ни один не скачался» — это отказ, а не успех: правила есть, файлов
	// правил нет, и обход не работает. Часть — успех: mihomo докачает
	// остальное сам по своему циклу, а какие наборы недокачаны, видно в
	// GET полем loaded.
	loaded, missing := rulesets.Verify(want.Sets, wait.live)
	if len(loaded) == 0 {
		return fmt.Errorf("наборы записаны, но движок не скачал ни одного из %d (%s): "+
			"проверьте, доступен ли с роутера raw.githubusercontent.com, "+
			"подробности — в /var/log/nikki/core.log",
			len(want.Sets), firstThree(setNames(missing)))
	}
	if len(missing) > 0 {
		// Единственный след: наружу это уезжает состоянием done, и без
		// строки в журнале «скачалось не всё» не отличить от «скачалось
		// всё» нигде, кроме глаз владельца на вкладке.
		s.logf("наборы geosite: скачались не все, недостаёт %d из %d: %s",
			len(missing), len(want.Sets), firstThree(setNames(missing)))
	}
	return nil
}

// switchMixinFlag доводит nikki.mixin.mixin_file_content до нужного
// значения — и только если оно отличается.
//
// Запись тем же значением стоила бы коммита пакета nikki на флеше и заодно
// опубликовала бы всё, что накопил в стейджинге кто-то ещё.
func (s *Server) switchMixinFlag(ctx context.Context, p rulesets.Policy) error {
	want := "1"
	if p == rulesets.PolicyProfile {
		// Правила целиком из профиля: файл из одной шапки, и склеивать его
		// не нужно вовсе.
		want = "0"
	}

	// Опции нет (ErrNotFound) — nikki считает её выключенной, и мы считаем
	// так же. Отдельной ветки на отказ uci нет намеренно: тогда мы просто
	// попробуем записать, и настоящий отказ приедет ниже — с текстом.
	cur, err := s.ex.UCIGet(ctx, "nikki", "mixin", mixinFlagOpt)
	if err != nil {
		cur = "0"
	}
	if cur == want {
		return nil
	}

	err = s.ex.UCISet(ctx, "nikki", "mixin", mixinFlagOpt, want)
	if err == nil {
		err = s.ex.UCICommit(ctx, "nikki")
	}
	if err == nil {
		return nil
	}

	// Своя правка снимается: незакоммиченный черновик уехал бы в систему
	// при первом чужом `uci commit nikki`, то есть в момент, который никто
	// не выбирал.
	if rerr := s.ex.UCIRevert(ctx, "nikki", "mixin"); rerr != nil {
		s.logf("наборы geosite: черновик nikki.mixin не снят: %v", rerr)
	}

	// Текст обязан сказать, действует ли записанный файл ПРЯМО СЕЙЧАС:
	// от этого зависит, что владелец увидит в обходе до починки. Флаг
	// стоял в 1 — движок читает новый файл (и при profile тоже: он читает
	// файл из одной шапки, то есть правил не добавляет).
	effect := "новый файл пока не действует — движок его не читает"
	if cur == "1" {
		effect = "новый файл уже действует — флаг и так стоял в 1"
	}
	return fmt.Errorf("наборы записаны в %s, но флаг %s не переключён (%s): %w",
		s.cfg.MixinPath, mixinFlagOpt, effect, err)
}

// errNoNikkiClient — клиента Clash API нет вовсе.
//
// В штатной сборке недостижимо (NewServer заводит его всегда), но исход
// свой: «спросить некому» и «спросили, не ответил» чинятся в разных местах,
// и один текст на оба отправил бы владельца поднимать исправный mihomo.
var errNoNikkiClient = errors.New("клиент Clash API не настроен")

// ruleProvidersWait — чем кончилось ожидание движка.
//
// Структура, а не bool: сверке нужен снимок, докладу об отказе — причина, а
// разбору текста — знание, кончилось ли окно само или ожидание оборвали.
// Три разных ответа в одном флаге не помещаются.
type ruleProvidersWait struct {
	// live — ПОСЛЕДНИЙ удачный снимок провайдеров. Последний, а не первый:
	// пока идёт докачка, каждый следующий полнее предыдущего.
	live map[string]nikki.RuleProvider
	// answered — отвечал ли Clash API хоть раз. Без этого «движок молчит»
	// неотличимо от «движок ответил, что не скачал ничего», а это разные
	// доклады владельцу.
	answered bool
	// err — последний отказ движка, целиком.
	err error
	// stopped — ожидание оборвал контекст джоба, а не наше окно.
	stopped bool
}

// awaitRuleProviders ждёт, пока движок докачает выбранные наборы.
//
// Ждать ОТВЕТА Clash API было бы рано: mihomo поднимает API до того, как
// заканчивает initial-загрузку провайдеров, и сверка по первому же ответу
// докладывала бы «не скачалось ни одного» про наборы, которые приезжают
// секундой позже. Поэтому опрос идёт до тех пор, пока не загрузятся ВСЕ
// запрошенные наборы либо не выйдет окно.
//
// Дольше окна не ждём и после него не считаем случившееся отказом: суточный
// цикл провайдеров докачает остальное сам, а панель всё это время под
// замком джоба. Поэтому по истечении окна сверка идёт по последнему снимку —
// часть наборов это «применено», ноль наборов это отказ.
func (s *Server) awaitRuleProviders(ctx context.Context, want []rulesets.Set) ruleProvidersWait {
	if s.nikki == nil {
		return ruleProvidersWait{err: errNoNikkiClient}
	}

	out := ruleProvidersWait{}
	deadline := time.Now().Add(s.rulesetsWait)
	for {
		live, err := s.nikki.RuleProviders(ctx)
		if err != nil {
			out.err = err
		} else {
			out.live, out.answered, out.err = live, true, nil
			// Всё на месте — ждать больше нечего.
			if loaded, _ := rulesets.Verify(want, live); len(loaded) == len(want) {
				return out
			}
		}
		if time.Now().After(deadline) {
			return out
		}
		if !sleepCtx(ctx, s.rulesetsPoll) {
			// Контекст джоба мёртв: досиживать окно в нём значило бы
			// докладывать о сроке, которого никто не выдерживал.
			out.stopped = true
			if out.err == nil {
				out.err = ctx.Err()
			}
			return out
		}
	}
}

// setNames — имена наборов в порядке выбора.
func setNames(sets []rulesets.Set) []string {
	out := make([]string, 0, len(sets))
	for _, set := range sets {
		out = append(out, set.Name)
	}
	return out
}

// firstThree — первые три имени через запятую.
//
// Три, а не все: в выборе их бывает три десятка, а строка ошибки читается
// в панели одним абзацем. Полный перечень владелец и так видит на вкладке —
// здесь имена нужны, чтобы узнать доклад, а не чтобы по нему работать.
func firstThree(names []string) string {
	if len(names) == 0 {
		// Пустая строка в скобках выглядела бы как обрыв сообщения.
		return "(пусто)"
	}
	if len(names) > 3 {
		names = names[:3]
	}
	return strings.Join(names, ", ")
}
