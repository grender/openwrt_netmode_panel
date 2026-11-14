package httpapi

import (
	"context"
	"sort"
	"strings"
	"time"

	"netmoded/internal/exits"
	"netmoded/internal/nikki"
)

// Автоподбор хопа для resi (ADR-0050, «Автоподбор хопа»).
//
// Цепочки dc меряет url-test группы …-DC-HOP профиля — дёшево, через VPS в
// freedom. Цепочка resi одна на ноду, «<нода>-resi ⇢ авто», и ходит через
// группу-повторитель nm-follow-*; здесь демон держит в повторителе хоп с
// лучшим свежим замером цепочки dc той же ноды. Участок «хоп → VPS» у
// выходов общий, поэтому годный для dc хоп годен и для resi, а проверки
// resi — новые соединения через ISP-прокси, который отказывает на частых
// соединениях, — не нужны вовсе.

const (
	// hopSyncEvery — как часто сверять повторители с замерами. url-test
	// меряет раз в 300 с; полминуты — чтобы после перезапуска движка resi
	// недолго стоял на первом хопе пула.
	hopSyncEvery = 30 * time.Second
	// hopSyncTolerance — гистерезис: живой текущий хоп остаётся, пока он
	// хуже лучшего не больше чем на столько. Без порога resi переключался
	// бы между хопами на каждом замере, обрывая долгие соединения.
	hopSyncTolerance = 150
	// hopSyncLogEvery — как часто повторять в журнал одну и ту же ошибку
	// Clash API: молчащий движок не должен засорять logread.
	hopSyncLogEvery = 10 * time.Minute
)

// rankedHop — хоп и лучшая задержка цепочки dc через него.
type rankedHop struct {
	Hop   string
	Delay int
}

// hopRanking — живые хопы пула по возрастанию задержки цепочки dc. Для
// нескольких нод у хопа берётся лучшая из их цепочек: так же ранжирует и
// подписка телефонов (один список хопов на все ноды).
func hopRanking(all map[string]nikki.Proxy, nodes, pool []string) []rankedHop {
	var out []rankedHop
	for _, h := range pool {
		best := -1
		for _, n := range nodes {
			p, ok := all[n+"-"+exits.MeasuredExit+exits.ChainSep+h]
			if !ok || !p.Alive || p.DelayMS == nil || *p.DelayMS <= 0 {
				continue
			}
			if best < 0 || *p.DelayMS < best {
				best = *p.DelayMS
			}
		}
		if best > 0 {
			out = append(out, rankedHop{Hop: h, Delay: best})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Delay < out[j].Delay })
	return out
}

// RunHopSync — фоновый цикл; запускается из main рядом с планировщиком.
func (s *Server) RunHopSync(ctx context.Context) {
	t := time.NewTicker(hopSyncEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.syncHopsOnce(ctx)
		}
	}
}

// syncHopsOnce — один проход: для каждой ноды с повторителем выставить
// лучший хоп по её цепочкам dc. Select — только при смене.
func (s *Server) syncHopsOnce(ctx context.Context) {
	if s.nikki == nil {
		return
	}
	if mode, err := s.ex.UCIGet(ctx, "netmode", "main", "mode"); err != nil || mode != "nikki" {
		return
	}
	cfg, foreign, herr := s.readMixin()
	if foreign || herr != nil || len(cfg.FollowNodes) == 0 {
		return
	}
	all, err := s.nikki.Proxies(ctx)
	if err != nil {
		s.hopSyncLog("автоподбор хопа: Clash API не ответил: %v", err)
		return
	}
	byGroup := make(map[string]string, len(cfg.Hops.Pool))
	for _, h := range cfg.Hops.Pool {
		byGroup[exits.HopGroupName(h)] = h
	}
	for _, node := range cfg.FollowNodes {
		fg := exits.FollowGroupName(node)
		g, ok := all[fg]
		if !ok || !g.IsGroup() {
			// Склейка выключена или движок ещё не собрал группы — цепочка
			// «⇢ авто» и так отказывает; чинить тут нечего.
			continue
		}
		ranked := hopRanking(all, []string{node}, cfg.Hops.Pool)
		target := "REJECT"
		if len(ranked) > 0 {
			target = exits.HopGroupName(ranked[0].Hop)
			if cur, ok := byGroup[g.Now]; ok {
				for _, r := range ranked {
					if r.Hop == cur && r.Delay-ranked[0].Delay <= hopSyncTolerance {
						target = g.Now
					}
				}
			}
		}
		if target == g.Now {
			continue
		}
		if err := s.nikki.Select(ctx, fg, target); err != nil {
			s.hopSyncLog("автоподбор хопа: %s не переключён на %s: %v", node, hopLabel(byGroup, target), err)
			continue
		}
		s.logf("автоподбор хопа: %s → %s (было %s)", node, hopLabel(byGroup, target), hopLabel(byGroup, g.Now))
	}
}

func hopLabel(byGroup map[string]string, group string) string {
	if h, ok := byGroup[group]; ok {
		return h
	}
	return group
}

// hopSyncLog — запись в журнал не чаще hopSyncLogEvery для одного текста.
func (s *Server) hopSyncLog(format string, args ...any) {
	key := strings.SplitN(format, ":", 2)[0]
	s.hopSyncMu.Lock()
	last := s.hopSyncLogged[key]
	now := time.Now()
	if !last.IsZero() && now.Sub(last) < hopSyncLogEvery {
		s.hopSyncMu.Unlock()
		return
	}
	if s.hopSyncLogged == nil {
		s.hopSyncLogged = map[string]time.Time{}
	}
	s.hopSyncLogged[key] = now
	s.hopSyncMu.Unlock()
	s.logf(format, args...)
}
