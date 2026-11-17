package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"netmoded/internal/happ"
	"netmoded/internal/job"
	"netmoded/internal/subs"
	"netmoded/internal/uci"
)

// Несколько подписок (ADR-0052).
//
// Основная живёт где жила — netmode.main.subscription_url и subURLStore.
// Остальные — секции `config subscription '<id>'` с url, label и hwid.
// Все сливаются в один список узлов (internal/subs, internal/happ.Merge):
// профиль владельца про число подписок не знает.
//
// СЕКРЕТЫ. Адрес каждой подписки и ID устройства роутера наружу не уходят
// никогда — ни в ответе, ни в ошибке, ни в журнале (ADR-0012, ADR-0034).
// Панель получает маску адреса и признак «ID шлётся».

const (
	// maxSubSources — потолок числа подписок вместе с основной. Каждая —
	// отдельное скачивание под общим таймаутом джоба и ещё один список в
	// панели; четырёх хватает с запасом.
	maxSubSources = 4
	// mainSourceLabel — имя основной подписки в текстах демона (ошибки,
	// журнал). Панель подписывает её своим словарём.
	mainSourceLabel = "Основная"
	maxSourceLabel  = 24
	subSectionType  = "subscription"
)

// SubSourceConfig — дополнительная подписка, как она лежит в UCI.
type SubSourceConfig struct {
	ID    string
	Label string
	URL   string
	// HWID — слать ли этой подписке ID устройства роутера.
	HWID bool
}

// ParseSubSources достаёт дополнительные подписки и ID устройства из
// вывода `uci show netmode`. Секции с негодным id пропускаются: id уходит
// в имя файла кэша.
func ParseSubSources(raw []byte) (extra []SubSourceConfig, hwid string) {
	cfg, err := uci.ParseShow("netmode", raw)
	if err != nil || cfg == nil {
		return nil, ""
	}
	if m, ok := cfg.Section("main"); ok {
		hwid = m.Options["hwid"]
	}
	for _, s := range cfg.ByType(subSectionType) {
		if s.Anonymous || !subs.ValidSourceID(s.Name) || s.Name == happ.MainSource {
			continue
		}
		extra = append(extra, SubSourceConfig{
			ID: s.Name, Label: s.Options["label"], URL: s.Options["url"], HWID: s.Options["hwid"] == "1",
		})
		if len(extra) == maxSubSources-1 {
			break
		}
	}
	return extra, hwid
}

// subSources — дополнительные подписки и ID устройства, живые: панель
// меняет их на ходу, обновление читает на момент вызова.
type subSources struct {
	mu    sync.RWMutex
	extra []SubSourceConfig
	hwid  string
}

func (s *subSources) list() []SubSourceConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]SubSourceConfig(nil), s.extra...)
}

func (s *subSources) deviceID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hwid
}

func (s *subSources) find(id string) (SubSourceConfig, bool) {
	for _, e := range s.list() {
		if e.ID == id {
			return e, true
		}
	}
	return SubSourceConfig{}, false
}

func (s *subSources) put(c SubSourceConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.extra {
		if s.extra[i].ID == c.ID {
			s.extra[i] = c
			return
		}
	}
	s.extra = append(s.extra, c)
}

func (s *subSources) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.extra[:0]
	for _, e := range s.extra {
		if e.ID != id {
			out = append(out, e)
		}
	}
	s.extra = out
}

func (s *subSources) setDeviceID(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hwid = v
}

// updaterSources — все подписки для subs.Updater, основная первой.
func (s *Server) updaterSources() []subs.SourceConf {
	out := []subs.SourceConf{{ID: happ.MainSource, Label: mainSourceLabel, URL: s.subURL.get()}}
	hwid := s.subExtra.deviceID()
	for _, e := range s.subExtra.list() {
		c := subs.SourceConf{ID: e.ID, Label: e.Label, URL: e.URL}
		if e.HWID {
			c.HWID = hwid
		}
		out = append(out, c)
	}
	return out
}

// anySubConfigured — есть ли хоть одна подписка с адресом.
func (s *Server) anySubConfigured() bool {
	if s.subURL.configured() {
		return true
	}
	for _, e := range s.subExtra.list() {
		if e.URL != "" {
			return true
		}
	}
	return false
}

// maskSecretPath — маска адреса, у которого секрет лежит в пути, а не в
// параметрах (второй провайдер: https://host/<идентификатор>). maskSubURL
// путь показывает целиком — для такого адреса это весь секрет. Здесь
// последний сегмент пути заменяется на «…».
func maskSecretPath(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "адрес задан, но не разбирается как URL"
	}
	if u.RawQuery != "" {
		return maskSubURL(raw)
	}
	p := strings.TrimRight(u.Path, "/")
	if i := strings.LastIndex(p, "/"); i >= 0 {
		p = p[:i+1] + "…"
	}
	return u.Scheme + "://" + u.Host + p
}

type subSourceView struct {
	ID         string             `json:"id"`
	Label      string             `json:"label"`
	Configured bool               `json:"configured"`
	Masked     string             `json:"masked"`
	HWID       bool               `json:"hwid"`
	Last       *subs.SourceResult `json:"last"`
}

func (s *Server) subSourceViews() []subSourceView {
	last := map[string]subs.SourceResult{}
	if s.subUpdater != nil {
		for _, r := range s.subUpdater.Results() {
			if r.Status != "" {
				last[r.ID] = r
			}
		}
	}
	view := func(id, label, raw, masked string, hwid bool) subSourceView {
		v := subSourceView{ID: id, Label: label, Configured: raw != "", Masked: masked, HWID: hwid}
		if r, ok := last[id]; ok {
			v.Last = &r
		}
		return v
	}
	main := s.subURL.get()
	out := []subSourceView{view(happ.MainSource, "", main, maskSubURL(main), false)}
	for _, e := range s.subExtra.list() {
		out = append(out, view(e.ID, e.Label, e.URL, maskSecretPath(e.URL), e.HWID))
	}
	return out
}

func (s *Server) writeSubSources(w http.ResponseWriter, code int) {
	writeJSON(w, code, map[string]any{"max": maxSubSources, "sources": s.subSourceViews()})
}

func (s *Server) handleSubscriptionsGet(w http.ResponseWriter, r *http.Request) {
	s.writeSubSources(w, http.StatusOK)
}

// checkSubURL — те же требования, что у основной подписки.
func checkSubURL(raw string) string {
	u, err := url.Parse(raw)
	switch {
	case err != nil || u.Host == "":
		return "Адрес не разбирается как URL"
	case u.Scheme != "http" && u.Scheme != "https":
		return "Адрес подписки обязан начинаться с http:// или https://"
	}
	return ""
}

// checkSubLabel — имя подписки. Оно уходит хвостом в имена узлов
// (happ.LabelSep), а оттуда — в регулярки фильтров миксина и в строку
// состояния: поэтому коротко и без разделителей.
func (s *Server) checkSubLabel(label, selfID string) string {
	switch {
	case label == "":
		return "Имя подписки не задано"
	case utf8.RuneCountInString(label) > maxSourceLabel:
		return "Имя подписки длиннее 24 знаков"
	case strings.Contains(label, strings.TrimSpace(happ.LabelSep)):
		return "В имени подписки нельзя «·»"
	case strings.EqualFold(label, mainSourceLabel):
		return "Имя «Основная» занято первой подпиской"
	}
	for _, r := range label {
		if unicode.IsControl(r) || strings.ContainsRune(`,;|>'"\`, r) {
			return "В имени подписки недопустимый знак"
		}
	}
	for _, e := range s.subExtra.list() {
		if e.ID != selfID && strings.EqualFold(e.Label, label) {
			return "Подписка с таким именем уже есть"
		}
	}
	return ""
}

// newSourceID — id секции из имени: латиница и цифры, иначе «sub».
func (s *Server) newSourceID(label string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(label) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		if b.Len() == 12 {
			break
		}
	}
	base := b.String()
	if base == "" || base == happ.MainSource {
		base = "sub"
	}
	id := base
	for n := 2; ; n++ {
		if _, busy := s.subExtra.find(id); !busy && id != happ.MainSource {
			return id
		}
		id = base + string(rune('0'+n%10))
	}
}

func newDeviceID() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// saveSubSource пишет секцию подписки в UCI и, если ей нужен ID устройства,
// а у роутера его ещё нет, — заводит его. Под замком пакета, как запись
// основного адреса.
func (s *Server) saveSubSource(ctx context.Context, c SubSourceConfig, create bool) *httpErr {
	unlock := s.lockPkg("netmode")
	defer unlock()

	fail := func(code, msg string) *httpErr {
		s.logf("подписки: запись секции не удалась")
		return &httpErr{http.StatusInternalServerError, code, msg}
	}
	if create {
		if err := s.ex.UCIAddNamed(ctx, "netmode", c.ID, subSectionType); err != nil {
			return fail("write_failed", "Записать подписку не удалось")
		}
	}
	hw := "0"
	if c.HWID {
		hw = "1"
	}
	for _, kv := range [][2]string{{"label", c.Label}, {"url", c.URL}, {"hwid", hw}} {
		if err := s.ex.UCISet(ctx, "netmode", c.ID, kv[0], kv[1]); err != nil {
			return fail("write_failed", "Записать подписку не удалось")
		}
	}
	newID := ""
	if c.HWID && s.subExtra.deviceID() == "" {
		id, err := newDeviceID()
		if err != nil {
			return fail("write_failed", "ID устройства не создан")
		}
		if err := s.ex.UCISet(ctx, "netmode", "main", "hwid", id); err != nil {
			return fail("write_failed", "Записать ID устройства не удалось")
		}
		newID = id
	}
	if err := s.ex.UCICommit(ctx, "netmode"); err != nil {
		s.logf("подписки: commit netmode не удался, запись осталась в черновике")
		return &httpErr{http.StatusInternalServerError, "commit_failed",
			"Подписка записана, но не закоммичена. В конфигурации остался черновик — " +
				"снимите его на роутере: uci revert netmode"}
	}
	if err := os.Chmod(netmodeConfigPath, 0o600); err != nil && !os.IsNotExist(err) {
		s.logf("подписки: права на %s не доведены до 0600 — в нём секрет, проверьте руками", netmodeConfigPath)
	}
	if newID != "" {
		s.subExtra.setDeviceID(newID)
	}
	s.subExtra.put(c)
	return nil
}

func (s *Server) handleSubscriptionsPost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Label string `json:"label"`
		URL   string `json:"url"`
		HWID  bool   `json:"hwid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Тело запроса не разбирается как JSON")
		return
	}
	in.Label, in.URL = strings.TrimSpace(in.Label), strings.TrimSpace(in.URL)
	if len(s.subExtra.list())+1 >= maxSubSources {
		writeErr(w, http.StatusConflict, "too_many_subscriptions", "Подписок уже четыре — больше нельзя")
		return
	}
	if msg := s.checkSubLabel(in.Label, ""); msg != "" {
		writeErr(w, http.StatusBadRequest, "bad_request", msg)
		return
	}
	if in.URL == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "Адрес подписки не задан")
		return
	}
	if msg := checkSubURL(in.URL); msg != "" {
		writeErr(w, http.StatusBadRequest, "bad_request", msg)
		return
	}
	ctx, cancel := writeCtx(r)
	defer cancel()
	c := SubSourceConfig{ID: s.newSourceID(in.Label), Label: in.Label, URL: in.URL, HWID: in.HWID}
	if herr := s.saveSubSource(ctx, c, true); herr != nil {
		herr.send(w)
		return
	}
	s.writeSubSources(w, http.StatusCreated)
}

func (s *Server) handleSubscriptionsPut(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == happ.MainSource {
		// У основной меняется только адрес — тем же путём, что всегда.
		s.handleSubscriptionPut(w, r)
		return
	}
	cur, ok := s.subExtra.find(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "unknown_subscription", "Такой подписки нет")
		return
	}
	var in struct {
		Label *string `json:"label"`
		URL   *string `json:"url"`
		HWID  *bool   `json:"hwid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Тело запроса не разбирается как JSON")
		return
	}
	if in.Label != nil {
		cur.Label = strings.TrimSpace(*in.Label)
		if msg := s.checkSubLabel(cur.Label, id); msg != "" {
			writeErr(w, http.StatusBadRequest, "bad_request", msg)
			return
		}
	}
	// Пустой адрес — «оставить прежний»: панель его не знает и вернуть не
	// может. Убрать подписку — удалением, а не пустым адресом.
	if in.URL != nil && strings.TrimSpace(*in.URL) != "" {
		cur.URL = strings.TrimSpace(*in.URL)
		if msg := checkSubURL(cur.URL); msg != "" {
			writeErr(w, http.StatusBadRequest, "bad_request", msg)
			return
		}
	}
	if in.HWID != nil {
		cur.HWID = *in.HWID
	}
	ctx, cancel := writeCtx(r)
	defer cancel()
	if herr := s.saveSubSource(ctx, cur, false); herr != nil {
		herr.send(w)
		return
	}
	s.writeSubSources(w, http.StatusOK)
}

// handleSubscriptionsDelete убирает подписку и её узлы из общего списка.
//
// Джобом и целиком внутри него: удаление секции, кэша и пересборка списка
// из кэшей остальных — одна операция. Занято — 409, и не тронуто ничего:
// половина удаления (секции нет, узлы в пуле) хуже отказа.
func (s *Server) handleSubscriptionsDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == happ.MainSource {
		writeErr(w, http.StatusBadRequest, "main_subscription",
			"Основную подписку удалить нельзя — очистите её адрес")
		return
	}
	if _, ok := s.subExtra.find(id); !ok {
		writeErr(w, http.StatusNotFound, "unknown_subscription", "Такой подписки нет")
		return
	}
	j, err := s.jobs.Start("subscription", "", "Удаление подписки и пересборка пула", subscriptionETASec,
		func(ctx context.Context) error {
			if err := s.deleteSubSource(ctx, id); err != nil {
				return err
			}
			if s.subRefresher == nil {
				return nil
			}
			_, err := s.subRefresher.Rebuild(ctx)
			if errors.Is(err, subs.ErrNoNodes) {
				// Подписок с узлами не осталось — пересобирать не из чего.
				return nil
			}
			return err
		})
	if errors.Is(err, job.ErrBusy) {
		writeErr(w, http.StatusConflict, "job_busy", "Уже идёт другая операция. Дождитесь её завершения.")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": j})
}

func (s *Server) deleteSubSource(ctx context.Context, id string) error {
	unlock := s.lockPkg("netmode")
	defer unlock()
	if err := s.ex.UCIDelete(ctx, "netmode", id, ""); err != nil {
		return errors.New("подписка не удалена из конфигурации")
	}
	if err := s.ex.UCICommit(ctx, "netmode"); err != nil {
		return errors.New("удаление подписки не закоммичено: снимите черновик на роутере — uci revert netmode")
	}
	s.subExtra.remove(id)
	if err := subs.DropCache(s.cfg.SubsCacheDir, id); err != nil {
		s.logf("подписки: кэш удалённой подписки не убран: %v", err)
	}
	return nil
}
