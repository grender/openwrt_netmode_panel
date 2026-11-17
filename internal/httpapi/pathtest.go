package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"netmoded/internal/exits"
	"netmoded/internal/nikki"
	"netmoded/internal/safe"
)

// Проверка путей до своих выходов (ADR-0051): по нажатию в панели демон
// пробует каждое звено «роутер → сервер подписки → нода → выход» и отдаёт,
// какое ответило. Экран «Детектор отказов» красит по этому схему.
//
// Что именно пробуется и что из этого следует:
//
//	роутер → сервер подписки   узел пула хопов (и текущий узел туннеля)
//	роутер → нода              «<нода>-dc» — выход с IP самого VPS
//	хоп → нода                 «<нода>-dc ⇢ <хоп>»
//	нода → ISP-прокси          «<нода>-resi» — ОДНО соединение на ноду
//
// Выход dc — freedom на VPS, поэтому его проба и есть проба дороги до ноды.
// Остальные выходы (resi) идут дальше ноды, и их отказ при живом dc той же
// ноды означает отказ за нодой. Делать этот вывод — дело панели: демон
// отдаёт замеры, а не диагноз.
const (
	// slowExitTimeout — сколько ждать выход через ISP-прокси: он открывает
	// новое соединение 2–5 с (замер 2026-11-14, репо выходных нод).
	slowExitTimeout = 8 * time.Second
	// pathTestBudget — потолок всей проверки. Панель ждёт синхронно
	// (T.TEST, 15 с): быстрые пробы и одна медленная обязаны уложиться.
	pathTestBudget = 12 * time.Second
	// pathTestParallel — как probeParallel у замера узлов.
	pathTestParallel = 6
)

// pathProbe — исход одной пробы.
//
//	ok       ответил, delay_ms заполнен
//	fail     не ответил
//	absent   такого узла у движка нет (провайдер не перечитан, пул выключен)
//	skipped  не успели: кончился бюджет. Про звено не узнали ничего
type pathProbe struct {
	State   string `json:"state"`
	DelayMS *int   `json:"delay_ms"`
}

type pathHop struct {
	Name string `json:"name"`
	// Pool — хоп пула цепочек; Tunnel — через него сейчас идёт туннель.
	Pool   bool `json:"pool"`
	Tunnel bool `json:"tunnel"`
	pathProbe
}

type pathChain struct {
	Hop string `json:"hop"`
	pathProbe
}

type pathExit struct {
	Exit string `json:"exit"`
	// Via — каким путём до ноды шла проба: «direct» или «hop» (тогда Hop —
	// хоп повторителя ноды).
	Via string `json:"via"`
	Hop string `json:"hop,omitempty"`
	pathProbe
}

type pathNode struct {
	Name string `json:"name"`
	// Direct — nil, если у ноды нет измеряемого выхода (dc).
	Direct *pathProbe  `json:"direct"`
	Chains []pathChain `json:"chains"`
	Exits  []pathExit  `json:"exits"`
}

// pathTask — одна проба: кого, через какой провайдер, с каким таймаутом и
// куда положить исход. Всё, что нужно от карты движка, вынимается ДО запуска
// горутин: карту в это время может менять сам замер.
type pathTask struct {
	name, provider string
	known          bool
	timeout        time.Duration
	out            *pathProbe
}

// handlePathTest — POST /api/nikki/pathtest.
//
// Синхронно и без джоба, как handleNikkiTest, и по той же причине: ничего
// не меняет, повтор безвреден, цена — время. Запускается только нажатием:
// проба resi — новое соединение через ISP-прокси, который отказывает на
// частых соединениях.
func (s *Server) handlePathTest(w http.ResponseWriter, r *http.Request) {
	if s.nikki == nil {
		writeErr(w, http.StatusServiceUnavailable, "nikki_unavailable", "Клиент Nikki не настроен")
		return
	}
	cfg, _, herr := s.readMixin()
	if herr != nil {
		herr.send(w)
		return
	}
	all, err := s.nikki.Proxies(r.Context())
	if err != nil {
		if errors.Is(err, nikki.ErrUnavailable) {
			writeErr(w, http.StatusServiceUnavailable, "nikki_unavailable",
				"Clash API не отвечает. Проверять нечего.")
			return
		}
		writeErr(w, http.StatusBadGateway, "nikki_error", err.Error())
		return
	}
	// Битый каталог экспортов — не отказ: серверы подписки проверить можно
	// и без своих нод, а что с экспортами — показывает экран пула.
	nodes, _ := exits.Load(s.cfg.ExitsDir)
	follow := s.followNow(r.Context(), cfg)

	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), pathTestBudget)
	defer cancel()

	task := func(name string, timeout time.Duration, out *pathProbe) pathTask {
		p, ok := all[name]
		return pathTask{name: name, provider: p.Provider, known: ok, timeout: timeout, out: out}
	}

	// ── Фаза 1: серверы подписки, прямой путь до нод, цепочки ──
	pool := cfg.Hops.Pool
	hops := make([]pathHop, 0, len(pool)+1)
	inPool := map[string]bool{}
	for _, h := range pool {
		inPool[h] = true
		hops = append(hops, pathHop{Name: h, Pool: true})
	}
	if tn := tunnelNode(all); tn != "" {
		if inPool[tn] {
			for i := range hops {
				if hops[i].Name == tn {
					hops[i].Tunnel = true
				}
			}
		} else {
			hops = append(hops, pathHop{Name: tn, Tunnel: true})
		}
	}

	res := make([]pathNode, len(nodes))
	var tasks []pathTask
	for i := range hops {
		tasks = append(tasks, task(hops[i].Name, 0, &hops[i].pathProbe))
	}
	for i, n := range nodes {
		res[i] = pathNode{Name: n.Name, Chains: []pathChain{}, Exits: []pathExit{}}
		if _, ok := n.Users[exits.MeasuredExit]; !ok {
			continue
		}
		base := n.Name + "-" + exits.MeasuredExit
		res[i].Direct = &pathProbe{}
		tasks = append(tasks, task(base, 0, res[i].Direct))
		res[i].Chains = make([]pathChain, len(pool))
		for j, h := range pool {
			res[i].Chains[j].Hop = h
			tasks = append(tasks, task(base+exits.ChainSep+h, 0, &res[i].Chains[j].pathProbe))
		}
	}
	s.runPathTasks(ctx, tasks)

	// ── Фаза 2: выходы за нодой — по одной пробе на выход ──
	//
	// После фазы 1, а не вместе с ней: путь до ноды выбирается по её исходу.
	// Прямой путь мёртв, а цепочка через хоп повторителя жива — пробуем
	// через «⇢ авто»; иначе «не дошли до ноды» читалось бы как «отказал
	// ISP-прокси».
	tasks = tasks[:0]
	for i, n := range nodes {
		for _, ex := range n.Exits() {
			if ex == exits.MeasuredExit {
				continue
			}
			res[i].Exits = append(res[i].Exits, pathExit{Exit: ex, Via: "direct"})
		}
		for j := range res[i].Exits {
			e := &res[i].Exits[j]
			name := n.Name + "-" + e.Exit
			if d := res[i].Direct; d != nil && d.State != "ok" {
				if h := follow[n.Name]; inPool[h] && chainOK(res[i].Chains, h) {
					e.Via, e.Hop = "hop", h
					name += exits.ChainSep + exits.AutoHop
				}
			}
			tasks = append(tasks, task(name, slowExitTimeout, &e.pathProbe))
		}
	}
	s.runPathTasks(ctx, tasks)

	elapsed := int(time.Since(start) / time.Millisecond)
	s.logf("nikki: проверка путей — серверов подписки %d, своих нод %d, %d мс", len(hops), len(res), elapsed)
	writeJSON(w, http.StatusOK, map[string]any{
		"hops":       hops,
		"nodes":      res,
		"elapsed_ms": elapsed,
	})
}

// chainOK — ответила ли цепочка dc через этот хоп.
func chainOK(chains []pathChain, hop string) bool {
	for _, c := range chains {
		if c.Hop == hop {
			return c.State == "ok"
		}
	}
	return false
}

// tunnelNode — узел подписки, через который сейчас идёт туннель: выбранный
// в PROXY, а если там группа («Авто») — выбранный в ней. Пусто, если туннель
// стоит не на узле провайдера (REJECT, DIRECT, пустая группа).
func tunnelNode(all map[string]nikki.Proxy) string {
	g, ok := all[ProxyGroup]
	if !ok {
		return ""
	}
	now := g.Now
	if a, ok := all[now]; ok && a.IsGroup() {
		now = a.Now
	}
	p, ok := all[now]
	if !ok || p.IsGroup() || p.Provider == "" {
		return ""
	}
	return now
}

// runPathTasks выполняет пробы, не больше pathTestParallel одновременно.
// Отказ одной не прерывает остальных — как в nikki.ProbeAll.
func (s *Server) runPathTasks(ctx context.Context, tasks []pathTask) {
	sem := make(chan struct{}, pathTestParallel)
	var wg sync.WaitGroup
	for i := range tasks {
		t := tasks[i]
		if !t.known {
			t.out.State = "absent"
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			*t.out = s.pathProbeOne(ctx, t)
		}()
	}
	wg.Wait()
}

func (s *Server) pathProbeOne(ctx context.Context, t pathTask) pathProbe {
	if ctx.Err() != nil {
		return pathProbe{State: "skipped"}
	}
	var delay int
	// safe.Do (ADR-0021): паника клиента в безымянной горутине уронила бы
	// демон целиком, а не одну пробу.
	err := safe.Do(nil, "проверка звена пути", func() error {
		var derr error
		if tp, ok := s.nikki.(nikki.TimedProber); ok && t.timeout > 0 {
			delay, derr = tp.DelayTimeout(ctx, t.provider, t.name, t.timeout)
		} else {
			delay, derr = s.nikki.Delay(ctx, t.provider, t.name)
		}
		return derr
	})
	switch {
	case err == nil:
		return pathProbe{State: "ok", DelayMS: &delay}
	case ctx.Err() != nil:
		// Бюджет кончился: записать «не ответил» значило бы оболгать звено
		// собственным таймаутом.
		return pathProbe{State: "skipped"}
	case errors.Is(err, nikki.ErrNotFound):
		return pathProbe{State: "absent"}
	default:
		return pathProbe{State: "fail"}
	}
}
