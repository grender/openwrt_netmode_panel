package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"netmoded/internal/atomicfile"
	"netmoded/internal/exits"
	"netmoded/internal/job"
	"netmoded/internal/mixin"
)

// Свои выходы через узлы подписки: пул хопов и каталог экспортов нод.
//
// Узор — авто-пула (autopoolhandlers.go): состояние в mixin.yaml читается
// и пишется целиком, отпечаток у секции свой, все проверки до первой
// записи, применение — джоб с перезапуском движка. Отличие одно: секция
// живёт в двух файлах. Группы-однохопники — в миксине, цепочки на них —
// в файле провайдера exits, и оба строятся из одного Pool, иначе цепочка
// сослалась бы на группу, которой нет (docs/recon/exit-chains.md).

// hopsETASec — запись двух файлов, флаг и netmode-apply.
const hopsETASec = 20

// hopsModeOff — «цепочек нет». Наружу словом, внутри — нулевой HopConfig.
const hopsModeOff = "off"

func hopsMode(h mixin.HopConfig) string {
	if h.IsDefault() {
		return hopsModeOff
	}
	return string(h.Mode)
}

// exitSNIs — SNI своих нод без повторов, в порядке нод: их не должен
// переписывать sniffer роутера (hops.go пакета mixin).
func exitSNIs(nodes []exits.Node) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range nodes {
		if !seen[n.SNI] {
			seen[n.SNI] = true
			out = append(out, n.SNI)
		}
	}
	return out
}

// hopPool — состав пула по выбору, в порядке подписки.
//
// Порядок подписки, а не присланного списка: от него зависит порядок
// цепочек в exits.yaml, и он не должен зависеть от того, в каком порядке
// владелец ставил галочки.
func hopPool(mode mixin.AutoMode, nodes, available []string) []string {
	mark := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		mark[n] = true
	}
	out := make([]string, 0)
	for _, n := range available {
		if (mode == mixin.AutoDeny) != mark[n] {
			out = append(out, n)
		}
	}
	return out
}

// handleHops — GET /api/nikki/hops.
func (s *Server) handleHops(w http.ResponseWriter, r *http.Request) {
	cfg, foreign, herr := s.readMixin()
	if herr != nil {
		herr.send(w)
		return
	}
	available, providerPool := s.manifestNodes()
	known := make(map[string]bool, len(available))
	for _, n := range available {
		known[n] = true
	}
	missing := make([]string, 0)
	for _, n := range cfg.Hops.Nodes {
		if !known[n] {
			missing = append(missing, n)
		}
	}

	// Свои ноды — без единого секрета: UUID, ключ и shortId живут только в
	// файлах 0600. Битый каталог — не 500: экран пула обязан открываться,
	// чтобы владелец увидел, ЧТО сломано.
	nodes, exErr := exits.Load(s.cfg.ExitsDir)
	list := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		list = append(list, map[string]any{
			"name": n.Name, "country": n.Country, "address": n.Address,
			"port": n.Port, "sni": n.SNI, "exits": n.Exits(),
		})
	}
	var exitsError any
	if exErr != nil {
		exitsError = exErr.Error()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"fingerprint":   mixin.HopFingerprint(cfg.Hops),
		"mode":          hopsMode(cfg.Hops),
		"nodes":         nonNil(cfg.Hops.Nodes),
		"pool":          nonNil(cfg.Hops.Pool),
		"available":     nonNil(available),
		"provider_pool": nonNil(providerPool),
		"missing":       missing,
		"max_pool":      mixin.MaxHopPool,
		"foreign":       foreign,
		"exits":         list,
		"exits_error":   exitsError,
		"skip_sni":      nonNil(cfg.SkipSNI),
		// follow — через какой хоп сейчас идёт resi каждой ноды (выставляет
		// hopsync). Пусто — движок молчит; «REJECT» — живого хопа нет.
		"follow": s.followNow(r.Context(), cfg),
	})
}

// handleHopsPut — PUT /api/nikki/hops.
func (s *Server) handleHopsPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode  string   `json:"mode"`
		Nodes []string `json:"nodes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Тело запроса не разбирается как JSON")
		return
	}
	switch in.Mode {
	case hopsModeOff, string(mixin.AutoDeny), string(mixin.AutoAllow), string(mixin.AutoProvider):
	default:
		writeErr(w, http.StatusBadRequest, "bad_request",
			"Неизвестный режим пула хопов «"+in.Mode+"»: ожидались off, deny, allow или provider.")
		return
	}

	// 1. Что на диске.
	cur, foreign, herr := s.readMixin()
	if foreign {
		writeErr(w, http.StatusConflict, "foreign_mixin",
			"Файл "+s.cfg.MixinPath+" написан не нами. Перезаписать его значило бы "+
				"молча уничтожить чужую работу: уберите файл на роутере и повторите.")
		return
	}
	corrupt := herr != nil && herr.code == "mixin_corrupt"
	if herr != nil && !corrupt {
		herr.send(w)
		return
	}

	// 2. Отпечаток.
	if !corrupt {
		switch got := strings.TrimSpace(r.Header.Get("If-Match")); {
		case got == "":
			writeErr(w, http.StatusConflict, "stale_hops",
				"Нужен заголовок If-Match с отпечатком из GET /api/nikki/hops.")
			return
		case got != mixin.HopFingerprint(cur.Hops):
			writeErr(w, http.StatusConflict, "stale_hops",
				"Пул хопов изменился, пока вы его правили: перечитайте GET /api/nikki/hops и повторите.")
			return
		}
	}

	// 3. Состав.
	available, providerPool := s.manifestNodes()
	var want mixin.HopConfig
	if in.Mode != hopsModeOff {
		want = mixin.HopConfig{Mode: mixin.AutoMode(in.Mode), Nodes: in.Nodes}
		if want.Mode == mixin.AutoProvider {
			if len(providerPool) == 0 {
				writeErr(w, http.StatusConflict, "no_provider_pool",
					"В подписке нет своего авторежима: брать состав пула неоткуда. Выберите узлы вручную.")
				return
			}
			want.Nodes = providerPool
		}
		if unknown := unknownNames(want.Nodes, available); len(unknown) > 0 {
			writeErr(w, http.StatusBadRequest, "unknown_node",
				"В подписке нет таких узлов: "+firstThree(unknown)+
					". Список узлов мог измениться — перечитайте GET /api/nikki/hops.")
			return
		}
		want.Pool = hopPool(want.Mode, want.Nodes, available)
	}
	if err := mixin.ValidateHops(want); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_hops", "Пул хопов не принят: "+err.Error())
		return
	}

	// 4. Свои ноды. Битый экспорт — отказ до записи: нода выпала бы из
	//    exits.yaml молча, вместе со своим выходом.
	nodes, err := exits.Load(s.cfg.ExitsDir)
	if err != nil {
		writeErr(w, http.StatusConflict, "exits_broken",
			"Экспорты своих нод не читаются, цепочки строить не из чего: "+err.Error())
		return
	}

	next := cur
	next.Hops = want
	next.SkipSNI = exitSNIs(nodes)
	next.FollowNodes = exits.FollowNodes(nodes)
	seen := mixinState(cur, herr)
	s.startExitsJob(w, "hops", "Применение пула хопов", func(ctx context.Context) error {
		if err := s.mixinStill(seen); err != nil {
			return err
		}
		return s.applyExits(ctx, next, nodes, true)
	})
}

// handleExitsReload — POST /api/exits/reload: перечитать каталог экспортов
// после scripts/exits-push.sh.
//
// Перезапуск движка — только если изменился набор SNI (он в миксине). Иначе
// хватает записи файла провайдера и его перечитки: связь не рвётся.
func (s *Server) handleExitsReload(w http.ResponseWriter, r *http.Request) {
	cur, foreign, herr := s.readMixin()
	if foreign {
		writeErr(w, http.StatusConflict, "foreign_mixin",
			"Файл "+s.cfg.MixinPath+" написан не нами: SNI своих нод записать некуда.")
		return
	}
	if herr != nil && herr.code != "mixin_corrupt" {
		herr.send(w)
		return
	}
	nodes, err := exits.Load(s.cfg.ExitsDir)
	if err != nil {
		writeErr(w, http.StatusConflict, "exits_broken", "Экспорты своих нод не читаются: "+err.Error())
		return
	}
	next := cur
	next.SkipSNI = exitSNIs(nodes)
	next.FollowNodes = exits.FollowNodes(nodes)
	// Перезапуск — если в миксине что-то меняется: SNI или повторители.
	restart := herr != nil || strings.Join(next.SkipSNI, ",") != strings.Join(cur.SkipSNI, ",") ||
		strings.Join(next.FollowNodes, ",") != strings.Join(cur.FollowNodes, ",")
	seen := mixinState(cur, herr)
	s.startExitsJob(w, "exits", "Перечитка своих нод", func(ctx context.Context) error {
		if err := s.mixinStill(seen); err != nil {
			return err
		}
		return s.applyExits(ctx, next, nodes, restart)
	})
}

func (s *Server) startExitsJob(w http.ResponseWriter, kind, title string, run func(context.Context) error) {
	j, err := s.jobs.Start(kind, "", title, hopsETASec, run)
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

// applyExits пишет файл провайдера exits и — при restart — миксин, флаг
// склейки и перезапуск движка.
//
// Провайдер — первым: до перезапуска движок его не перечитывает, а после
// перезапуска оба файла уже согласованы. В обратном порядке между двумя
// записями прожил бы миксин с группами без цепочек — безвредно, — но при
// сбое второй записи осталась бы ровно та рассинхронизация, ради
// избежания которой Pool один на оба файла.
func (s *Server) applyExits(ctx context.Context, want mixin.Config, nodes []exits.Node, restart bool) error {
	if err := atomicfile.Write(s.cfg.ExitsProviderPath, exits.RenderProvider(nodes, want.Hops.Pool), 0o600); err != nil {
		return fmt.Errorf("файл провайдера %s не записан, ничего не изменено: %w", s.cfg.ExitsProviderPath, err)
	}
	mode, err := s.ex.UCIGet(ctx, "netmode", "main", "mode")
	if !restart {
		if err != nil || mode != "nikki" || s.nikki == nil {
			return nil
		}
		if err := s.nikki.ReloadProvider(ctx, exitsProviderName); err != nil {
			return fmt.Errorf("файл провайдера %s записан, но движок его не перечитал "+
				"(есть ли провайдер %s в профиле?): %w", s.cfg.ExitsProviderPath, exitsProviderName, err)
		}
		return nil
	}

	if err := atomicfile.Write(s.cfg.MixinPath, mixin.Render(want), 0o644); err != nil {
		return fmt.Errorf("файл провайдера записан, а %s — нет: перезапуск не делался, повторите: %w", s.cfg.MixinPath, err)
	}
	if err := s.switchMixinFlag(ctx, want); err != nil {
		return err
	}
	if err != nil {
		return fmt.Errorf("пул хопов записан, но текущий режим не прочитался, и Nikki не перезапущен: %w", err)
	}
	if mode != "nikki" {
		return nil
	}
	if err := s.ex.ApplyMode(ctx, "nikki"); err != nil {
		return fmt.Errorf("пул хопов записан, но Nikki не перезапустился: %w", s.modeApplyFailed("nikki", err))
	}

	// Сверка у движка: группы-однохопники должны быть. Нет — склейка не
	// доехала, и каждая цепочка отказывает на каждом соединении.
	if s.nikki == nil || (len(want.Hops.Pool) == 0 && len(want.FollowNodes) == 0) {
		return nil
	}
	all, err := s.nikki.Proxies(ctx)
	if err != nil {
		s.logf("пул хопов: группы у движка не сверены: %v", err)
		return nil
	}
	// Повторители — так же: без группы «⇢ авто» отказывает на каждом
	// соединении, то есть resi через хоп не работает вовсе.
	var lostFollow []string
	for _, n := range want.FollowNodes {
		if g, ok := all[exits.FollowGroupName(n)]; !ok || !g.IsGroup() {
			lostFollow = append(lostFollow, n)
		}
	}
	if len(lostFollow) > 0 {
		return fmt.Errorf("пул хопов записан и Nikki перезапущен, но у движка нет повторителей хопа для нод %s: "+
			"resi через хоп отказывает. Проверьте, что склейка включена (nikki.mixin.%s=1)",
			firstThree(lostFollow), mixinFlagOpt)
	}
	var lost []string
	for _, h := range want.Hops.Pool {
		if g, ok := all[exits.HopGroupName(h)]; !ok || !g.IsGroup() {
			lost = append(lost, h)
		}
	}
	if len(lost) > 0 {
		return fmt.Errorf("пул хопов записан и Nikki перезапущен, но у движка нет групп для хопов %s: "+
			"цепочки через них отказывают. Проверьте, что склейка включена (nikki.mixin.%s=1)",
			firstThree(lost), mixinFlagOpt)
	}
	return nil
}

// resyncHops пересобирает пул после обновления подписки — как
// resyncProviderPool: только при изменившемся составе, отказ — в журнал.
func (s *Server) resyncHops(ctx context.Context) {
	cur, foreign, herr := s.readMixin()
	if foreign || herr != nil || cur.Hops.IsDefault() {
		return
	}
	available, providerPool := s.manifestNodes()
	want := cur.Hops
	if want.Mode == mixin.AutoProvider && len(providerPool) > 0 {
		want.Nodes = providerPool
	}
	want.Pool = hopPool(want.Mode, want.Nodes, available)
	if strings.Join(want.Pool, "\n") == strings.Join(cur.Hops.Pool, "\n") &&
		strings.Join(want.Nodes, "\n") == strings.Join(cur.Hops.Nodes, "\n") {
		return
	}
	if err := mixin.ValidateHops(want); err != nil {
		s.logf("пул хопов: подписка обновилась, новый состав не годится (%v) — оставлен прежний", err)
		return
	}
	nodes, err := exits.Load(s.cfg.ExitsDir)
	if err != nil {
		s.logf("пул хопов: подписка обновилась, но экспорты нод не читаются (%v) — состав оставлен прежним", err)
		return
	}
	next := cur
	next.Hops = want
	next.FollowNodes = exits.FollowNodes(nodes)
	if err := s.applyExits(ctx, next, nodes, true); err != nil {
		s.logf("пул хопов: состав подписки изменился, но не применён: %v", err)
		return
	}
	s.logf("пул хопов: состав пересобран из подписки, хопов %d", len(want.Pool))
}

// followNow — текущий хоп повторителя каждой ноды у живого движка.
func (s *Server) followNow(ctx context.Context, cfg mixin.Config) map[string]string {
	out := map[string]string{}
	if s.nikki == nil || len(cfg.FollowNodes) == 0 {
		return out
	}
	all, err := s.nikki.Proxies(ctx)
	if err != nil {
		return out
	}
	byGroup := map[string]string{}
	for _, h := range cfg.Hops.Pool {
		byGroup[exits.HopGroupName(h)] = h
	}
	for _, n := range cfg.FollowNodes {
		if g, ok := all[exits.FollowGroupName(n)]; ok {
			out[n] = hopLabel(byGroup, g.Now)
		}
	}
	return out
}
