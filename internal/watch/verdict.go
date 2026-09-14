package watch

import "time"

// joinWindow — окно склейки строки журнала с записью снимка.
//
// Порт источника переиспользуется, поэтому склейка ищет только среди
// соединений, стартовавших рядом со строкой. Без окна метка от давно
// закрытого соединения однажды приклеила бы чужое.
const joinWindow = 5 * time.Second

// verdictOf — таблица вердиктов ADR-0043, и порядок проверок в ней значим.
//
//	unreachable — за последние 60 с была неудача дозвона и после неё не было
//	              удачи: рукопожатие с целью или с узлом туннеля не прошло;
//	silent      — живое соединение старше 5 с отправило запрос и не получило
//	              ответа, и ушло оно напрямую;
//	ok          — остальное.
//
// «Не отвечает» проверяется первым: адресат, который и не дозвонился, и
// молчит по другому соединению, — это прежде всего недозвон, и лечится он
// иначе.
func verdictOf(e *entry, now time.Time) Verdict {
	if !e.lastDialAt.IsZero() &&
		now.Sub(e.lastDialAt) <= unreachableFor &&
		e.lastOKAt.Before(e.lastDialAt) {
		return VerdictUnreachable
	}
	if e.silentTick {
		return VerdictSilent
	}
	return VerdictOK
}
