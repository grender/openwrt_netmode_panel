package httpapi

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"netmoded/internal/atomicfile"
	"netmoded/internal/exits"
	"netmoded/internal/happsub"
	"netmoded/internal/mixin"
)

// Подписка для Happ на телефонах (ADR-0050, этап «Что дальше»): свои выходы
// напрямую и через хопы плюс «Умный маршрут» с правилами панели.
//
// Свой токен, отдельный от токена панели: этот уезжает в приложение на
// телефоне, и утечка его не должна открывать панель роутера. Токен — в
// пути: Happ умеет только адрес, без заголовков. Неверный токен — пустой
// 404, ничем не отличимый от «такого пути нет». Демон слушает только LAN
// (ADR-0014), так что подписка доступна только из домашней сети.

const (
	subPathPrefix = "/sub/happ/"
	// subTitle — имя подписки в Happ.
	subTitle = "netmoded · свои выходы"
	// subUpdateHours — как часто Happ перекачивает: как обновление подписки
	// провайдера, раз в 12 часов.
	subUpdateHours = "12"
	maxSubClients  = 16
	// maxSubTunnel — сколько узлов авто-пула идёт в туннель «Умного»: их
	// observatory пробует разом раз в интервал.
	maxSubTunnel = 12
)

// subClients — кто забирал подписку: User-Agent → время. Без адресов: в
// панели достаточно видеть, что телефон обновился.
type subClients struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (c *subClients) seen(ua string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.at == nil {
		c.at = map[string]time.Time{}
	}
	if len(ua) > 200 {
		ua = ua[:200]
	}
	c.at[ua] = time.Now().UTC()
	for len(c.at) > maxSubClients {
		var oldest string
		for k, v := range c.at {
			if oldest == "" || v.Before(c.at[oldest]) {
				oldest = k
			}
		}
		delete(c.at, oldest)
	}
}

func (c *subClients) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = nil
}

func (c *subClients) list() []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]map[string]any, 0, len(c.at))
	for ua, t := range c.at {
		out = append(out, map[string]any{"user_agent": ua, "at": t.Format(time.RFC3339)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["at"].(string) > out[j]["at"].(string) })
	return out
}

// subPathAllowed — путь подписки, который идёт мимо токена панели: ровно
// GET /sub/happ/<одно непустое имя>. Всё прочее под префиксом — пустой
// 404: иначе /sub/happ/ упал бы в «GET /» и отдал статику панели без токена.
func subPathAllowed(r *http.Request) bool {
	tok := strings.TrimPrefix(r.URL.Path, subPathPrefix)
	return r.Method == http.MethodGet && tok != "" && !strings.Contains(tok, "/")
}

func (s *Server) subToken() string {
	b, err := os.ReadFile(s.cfg.SubTokenPath)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (s *Server) phoneSubURL(tok string) string {
	return fmt.Sprintf("http://%s%s%s", s.Addr(), subPathPrefix, tok)
}

// handleHappSub — GET /sub/happ/{token}.
func (s *Server) handleHappSub(w http.ResponseWriter, r *http.Request) {
	want := s.subToken()
	if want == "" || subtle.ConstantTimeCompare([]byte(r.PathValue("token")), []byte(want)) != 1 {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.subSeen.seen(r.UserAgent())

	cfg, _, herr := s.readMixin()
	if herr != nil {
		// Без миксина — без правил и хопов: прямые записи всё равно
		// рабочие, а телефон без подписки хуже телефона без «Умного».
		s.logf("подписка для телефонов: миксин не прочитан (%s) — отдана без правил и хопов", herr.code)
		cfg = mixin.Config{}
	}
	nodes, err := exits.Load(s.cfg.ExitsDir)
	if err != nil {
		s.logf("подписка для телефонов: экспорты нод не читаются: %v", err)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	obs, err := s.loadOutbounds()
	if err != nil {
		s.logf("подписка для телефонов: %v — отдана без хопов", err)
	}
	var hops []happsub.Hop
	for _, name := range cfg.Hops.Pool {
		if ob, ok := obs[name]; ok {
			hops = append(hops, happsub.Hop{Name: name, Outbound: ob})
		}
	}
	// Туннель — узлы авто-пула, как BYPASS на роутере: тот же расчёт, что у
	// движка по фильтру sub-auto (умолчание — весь список).
	available, _ := s.manifestNodes()
	autoMode := cfg.Auto.Mode
	if autoMode == "" {
		autoMode = mixin.AutoDeny
	}
	var tunnel []happsub.Hop
	for _, name := range hopPool(autoMode, cfg.Auto.Nodes, available) {
		if ob, ok := obs[name]; ok && len(tunnel) < maxSubTunnel {
			tunnel = append(tunnel, happsub.Hop{Name: name, Outbound: ob})
		}
	}
	body, err := happsub.Build(happsub.Input{Nodes: nodes, Hops: hops, Tunnel: tunnel, Rules: cfg})
	if err != nil {
		s.logf("подписка для телефонов: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("profile-title", "base64:"+base64.StdEncoding.EncodeToString([]byte(subTitle)))
	h.Set("profile-update-interval", subUpdateHours)
	_, _ = w.Write(body)
}

// loadOutbounds — исходные outbounds узлов подписки (outbounds.json).
// Файла нет — подписка ещё не обновлялась этой версией демона: хопов нет.
func (s *Server) loadOutbounds() (map[string]map[string]any, error) {
	b, err := os.ReadFile(s.cfg.OutboundsPath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("outbounds узлов подписки не прочитаны: %w", err)
	}
	var m map[string]map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("outbounds узлов подписки не разбираются: %w", err)
	}
	return m, nil
}

// handleSubLink — GET /api/sub/link: ссылка (или null, если не выпущена) и
// кто её забирал. Ссылка — секрет: панель показывает её только по
// явному нажатию и не хранит.
func (s *Server) handleSubLink(w http.ResponseWriter, r *http.Request) {
	var url any
	if tok := s.subToken(); tok != "" {
		url = s.phoneSubURL(tok)
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": url, "clients": s.subSeen.list()})
}

// handleSubRotate — POST /api/sub/rotate: выпустить новый токен. Старая
// ссылка перестаёт работать сразу на всех телефонах.
func (s *Server) handleSubRotate(w http.ResponseWriter, r *http.Request) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "Не удалось получить случайные байты: "+err.Error())
		return
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)
	if err := atomicfile.Write(s.cfg.SubTokenPath, []byte(tok+"\n"), 0o600); err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "Токен подписки не записан: "+err.Error())
		return
	}
	s.subSeen.reset()
	writeJSON(w, http.StatusOK, map[string]any{"url": s.phoneSubURL(tok)})
}
