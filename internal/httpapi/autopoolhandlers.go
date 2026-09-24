package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"netmoded/internal/atomicfile"
	"netmoded/internal/happ"
	"netmoded/internal/job"
	"netmoded/internal/mixin"
)

// Авто-пул: чем наполняется группа AUTO.
//
// Вторая секция того же mixin.yaml, что и наборы, поэтому весь узор взят
// оттуда: состояние читается и пишется целиком, отпечаток защищает от
// гонки двух вкладок, порядок проверок идёт от «запись бессмысленна» к
// «запись невозможна» и весь он стоит ДО первой записи, а применение —
// джоб с перезапуском движка.
//
// Отпечаток у пула свой, отдельный от наборов: решения независимые, и
// правка одного не должна отбивать чужой PUT другого.

const (
	// autopoolETASec — запись файла, флаг и netmode-apply. Те же 15 с, что
	// у наборов: шаги буквально одни и те же, кроме ожидания докачки.
	autopoolETASec = 15
)

// handleAutopool — GET /api/nikki/autopool.
func (s *Server) handleAutopool(w http.ResponseWriter, r *http.Request) {
	cfg, foreign, herr := s.readMixin()
	if herr != nil {
		herr.send(w)
		return
	}
	writeJSON(w, http.StatusOK, s.autopoolBody(r.Context(), cfg.Auto, foreign))
}

// autopoolBody собирает тело ответа.
//
// Имена узлов берутся из МАНИФЕСТА, а не у движка, и это не экономия
// запроса: выбор владельца лежит в нашем файле и читается без mihomo. Будь
// список живым, экран пула гас бы вместе с движком — ровно тогда, когда
// владелец и лезет разбираться, почему обход не работает.
func (s *Server) autopoolBody(ctx context.Context, a mixin.AutoConfig, foreign bool) map[string]any {
	available, providerPool := s.manifestNodes()

	// Имена, которые владелец когда-то отметил, а провайдер с тех пор
	// переименовал или убрал. Молчать о них нельзя: в режиме «только
	// отмеченные» такое имя молча уменьшает пул, а в «все, кроме
	// отмеченных» — молча его увеличивает.
	known := make(map[string]bool, len(available))
	for _, n := range available {
		known[n] = true
	}
	missing := make([]string, 0)
	for _, n := range a.Nodes {
		if !known[n] {
			missing = append(missing, n)
		}
	}

	// Режим наружу — всегда словом. Пустой и «deny» внутри одно и то же
	// состояние (autopool.go), но панели нужен конкретный выбор для
	// переключателя, а не пустая строка.
	mode := a.Mode
	if mode == "" {
		mode = mixin.AutoDeny
	}

	return map[string]any{
		"fingerprint": mixin.AutoFingerprint(a),
		"mode":        mode,
		"nodes":       nonNil(a.Nodes),
		"foreign":     foreign,
		// available — узлы подписки в авторском порядке провайдера: из
		// них владелец и отмечает.
		"available": nonNil(available),
		// provider_pool — состав балансировщика подписки. Пуст, если
		// провайдер своего авторежима не прислал: тогда режим «как в
		// подписке» брать неоткуда, и панель обязана его не предлагать.
		"provider_pool": nonNil(providerPool),
		"missing":       missing,
		// pool_size — сколько узлов у движка в группе AUTO СЕЙЧАС. null
		// означает «спросить не смогли», а не «пул пуст»: движок может
		// быть погашен, и ноль соврал бы про пустой пул.
		"pool_size": s.livePoolSize(ctx),
	}
}

// manifestNodes — имена узлов подписки и состав балансировщика провайдера.
func (s *Server) manifestNodes() (available, providerPool []string) {
	for _, e := range s.manifestEntries() {
		switch e.Kind {
		case happ.KindNode:
			available = append(available, e.Name)
		case happ.KindAuto:
			// Пул провайдера лежит у записи «Авто» — она и есть его
			// балансировщик (internal/happ).
			providerPool = append(providerPool, e.Pool...)
		}
	}
	return available, providerPool
}

// livePoolSize — размер группы AUTO у движка или nil.
func (s *Server) livePoolSize(ctx context.Context) *int {
	if s.nikki == nil {
		return nil
	}
	all, err := s.nikki.Proxies(ctx)
	if err != nil {
		return nil
	}
	g, ok := all[mixin.AutoGroup]
	if !ok || !g.IsGroup() {
		return nil
	}
	n := len(g.Members)
	return &n
}

// handleAutopoolPut — PUT /api/nikki/autopool.
//
// Порядок проверок — от «запись бессмысленна» к «запись невозможна», и все
// они до первой записи: отката нет (ADR-0006).
func (s *Server) handleAutopoolPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Mode string `json:"mode"`
		// Nodes необязателен: у режима «как в подписке» состав берёт
		// демон, и присланный список там нечему соответствовать.
		Nodes []string `json:"nodes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Тело запроса не разбирается как JSON")
		return
	}
	mode := mixin.AutoMode(in.Mode)
	switch mode {
	case mixin.AutoDeny, mixin.AutoAllow, mixin.AutoProvider:
	default:
		writeErr(w, http.StatusBadRequest, "bad_request",
			"Неизвестный режим авто-пула «"+in.Mode+"»: ожидались deny, allow или provider.")
		return
	}

	// 1. Что лежит на диске сейчас.
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

	// 2. Отпечаток. У испорченного файла он бессмыслен — его содержимое
	//    нам неизвестно, а PUT как раз чинит эту поломку.
	if !corrupt {
		switch got := strings.TrimSpace(r.Header.Get("If-Match")); {
		case got == "":
			writeErr(w, http.StatusConflict, "stale_autopool",
				"Нужен заголовок If-Match с отпечатком из GET /api/nikki/autopool: "+
					"без него запись не докажет, что видела нынешний выбор.")
			return
		case got != mixin.AutoFingerprint(cur.Auto):
			writeErr(w, http.StatusConflict, "stale_autopool",
				"Авто-пул изменился, пока вы его правили: перечитайте "+
					"GET /api/nikki/autopool и повторите с новым отпечатком.")
			return
		}
	}

	// 3. Состав. У «как в подписке» его даёт манифест, а не клиент: в этом
	//    весь смысл режима — список пересобирается сам при каждом
	//    обновлении подписки.
	available, providerPool := s.manifestNodes()
	want := mixin.AutoConfig{Mode: mode, Nodes: in.Nodes}
	if mode == mixin.AutoProvider {
		if len(providerPool) == 0 {
			writeErr(w, http.StatusConflict, "no_provider_pool",
				"В подписке нет своего авторежима: балансировщика у провайдера не нашлось, "+
					"и брать состав пула неоткуда. Выберите узлы вручную.")
			return
		}
		want.Nodes = providerPool
	}

	// 4. Форма.
	if err := mixin.ValidateAuto(want); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_autopool", "Авто-пул не принят: "+err.Error())
		return
	}

	// 5. Имена. Панель отмечает из того, что сама же показала, поэтому
	//    незнакомое имя — это устаревшая вкладка или ручной запрос, и
	//    молча применить его значило бы записать фильтр, который ни с чем
	//    не совпадёт.
	if unknown := unknownNames(want.Nodes, available); len(unknown) > 0 {
		writeErr(w, http.StatusBadRequest, "unknown_node",
			"В подписке нет таких узлов: "+firstThree(unknown)+
				". Список узлов мог измениться — перечитайте GET /api/nikki/autopool.")
		return
	}

	// 6. Непустой пул. empty-fallback в профиле спасёт движок, но
	//    записывать заведомо пустую группу нельзя: владелец увидит мёртвое
	//    «Авто» и не поймёт, чем он его опустошил.
	if left := poolLeft(want, available); left == 0 {
		writeErr(w, http.StatusBadRequest, "empty_pool",
			"После такого выбора в авто-пуле не остаётся ни одного узла. "+
				"Снимите часть отметок или смените режим.")
		return
	}

	// 7. Джоб.
	next := cur
	next.Auto = want
	seen := mixinState(cur, herr)
	j, err := s.jobs.Start("autopool", "", "Применение авто-пула", autopoolETASec,
		func(ctx context.Context) error {
			if err := s.mixinStill(seen); err != nil {
				return err
			}
			return s.applyAutopool(ctx, next)
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

// applyAutopool выполняет применение.
//
// Шаги те же и в том же порядке, что у наборов: файл, флаг, перезапуск —
// от того, что переживает перезагрузку, к тому, что живёт до неё. Отката
// нет ни на одном (ADR-0006).
func (s *Server) applyAutopool(ctx context.Context, want mixin.Config) error {
	if err := atomicfile.Write(s.cfg.MixinPath, mixin.Render(want), 0o644); err != nil {
		return fmt.Errorf("авто-пул не записан, ничего не изменено: %w", err)
	}
	if err := s.switchMixinFlag(ctx, want); err != nil {
		return err
	}

	mode, err := s.ex.UCIGet(ctx, "netmode", "main", "mode")
	if err != nil {
		return fmt.Errorf("авто-пул записан в %s и склейка включена, но текущий режим не прочитался, "+
			"и Nikki не перезапущен — выбор вступит в силу при следующем включении режима. "+
			"Повторить применение безопасно: %w", s.cfg.MixinPath, err)
	}
	if mode != "nikki" {
		return nil
	}
	if err := s.ex.ApplyMode(ctx, "nikki"); err != nil {
		return fmt.Errorf("авто-пул записан, но Nikki не перезапустился: %w", s.modeApplyFailed("nikki", err))
	}

	// Сверка у движка, а не у себя. Фильтр применяет mihomo, и «файл
	// записан» с «пул собрался» не совпадает: регулярка могла не совпасть
	// ни с одним именем, и тогда группа AUTO живёт на empty-fallback, то
	// есть обход молча не работает.
	if s.nikki == nil {
		return nil
	}
	names, err := s.nikki.ProviderProxies(ctx, mixin.AutoProviderName)
	if err != nil {
		// Движок мог не успеть поднять Clash API. Это не повод объявлять
		// провал: файл записан и вступит в силу сам.
		s.logf("авто-пул: состав провайдера %s не сверен: %v", mixin.AutoProviderName, err)
		return nil
	}
	if len(names) == 0 {
		return fmt.Errorf("авто-пул записан и Nikki перезапущен, но в провайдере %s "+
			"не осталось ни одного узла: фильтр не совпал ни с одним именем. "+
			"Группа %s работает на empty-fallback из профиля, то есть обход через неё не идёт",
			mixin.AutoProviderName, mixin.AutoGroup)
	}
	return nil
}

// unknownNames — имена, которых нет среди известных.
func unknownNames(want, known []string) []string {
	have := make(map[string]bool, len(known))
	for _, n := range known {
		have[n] = true
	}
	out := make([]string, 0)
	for _, n := range want {
		if !have[n] {
			out = append(out, n)
		}
	}
	return out
}

// poolLeft — сколько узлов останется в пуле.
//
// Считается по манифесту, а не у движка: решение принимается до записи, а
// движок к этому моменту ещё работает по старому фильтру.
func poolLeft(a mixin.AutoConfig, available []string) int {
	switch a.Mode {
	case mixin.AutoAllow, mixin.AutoProvider:
		return len(a.Nodes)
	default:
		drop := make(map[string]bool, len(a.Nodes))
		for _, n := range a.Nodes {
			drop[n] = true
		}
		left := 0
		for _, n := range available {
			if !drop[n] {
				left++
			}
		}
		return left
	}
}

// nonNil — пустой срез вместо nil: в JSON это «[]», а не «null».
func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// resyncProviderPool пересобирает пул после обновления подписки.
//
// Режим «как в подписке» тем и отличается от ручного, что список
// пересобирается сам: провайдер добавил сервер в свой балансировщик —
// он появился и у нас. Иначе режим врал бы названием.
//
// Перезапись и перезапуск — ТОЛЬКО при изменившемся составе. Обновление
// подписки идёт раз в двенадцать часов, и гасить обход на каждое из них
// ради файла, который не поменялся, значило бы платить перерывом связи за
// ничто.
//
// Отказ не роняет обновление подписки: узлы уже записаны и перечитаны
// движком, а несобранный пул — отдельная беда, о которой честнее сказать в
// журнал, чем объявить провалом всё обновление.
func (s *Server) resyncProviderPool(ctx context.Context) {
	cur, foreign, herr := s.readMixin()
	if foreign || herr != nil || cur.Auto.Mode != mixin.AutoProvider {
		return
	}
	_, pool := s.manifestNodes()
	if len(pool) == 0 {
		// Провайдер убрал балансировщик. Прежний состав оставляем как
		// есть: он хотя бы рабочий, а пустой пул — мёртвая группа.
		s.logf("авто-пул: подписка обновилась, но балансировщика в ней больше нет — состав оставлен прежним")
		return
	}
	if sameNames(pool, cur.Auto.Nodes) {
		return
	}
	next := cur
	next.Auto.Nodes = pool
	if err := s.applyAutopool(ctx, next); err != nil {
		s.logf("авто-пул: состав подписки изменился, но не применён: %v", err)
		return
	}
	s.logf("авто-пул: состав пересобран из подписки, узлов %d", len(pool))
}

// sameNames — совпадают ли списки имён с точностью до порядка.
//
// Порядок участников группы решает mihomo, а не мы: для url-test он
// значения не имеет. Считать перестановку изменением значило бы
// перезапускать движок из-за того, что провайдер иначе отсортировал свой
// балансировщик.
func sameNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, n := range a {
		seen[n]++
	}
	for _, n := range b {
		seen[n]--
		if seen[n] < 0 {
			return false
		}
	}
	return true
}
