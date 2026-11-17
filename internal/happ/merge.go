package happ

import "fmt"

// Source — разобранная подписка одного провайдера перед слиянием.
type Source struct {
	// ID — устойчивый идентификатор источника (имя секции UCI); у основной
	// подписки — MainSource.
	ID string
	// Label — имя для человека. Им помечаются имена, совпавшие с более
	// ранним источником.
	Label   string
	Entries []Entry
}

// MainSource — id основной подписки (netmode.main.subscription_url). Её
// записи идут первыми и в манифесте остаются без поля source: так выглядел
// манифест до появления второй подписки.
const MainSource = "main"

// LabelSep отделяет имя источника в имени узла: «Германия · SecondVPN».
const LabelSep = " · "

// Merge сливает подписки в один список — порядок источников сохраняется,
// внутри источника порядок записей прежний (ADR-0052).
//
// Имена обязаны быть уникальны на весь список: по имени узел выбирают в
// группе, по имени он попадает в авто-пул и пул хопов, от имени считается
// группа-однохопник (exits.HopGroupName). Внутри источника их развёл Parse.
// Имя, уже занятое БОЛЕЕ РАННИМ источником, получает хвост с именем своего
// источника; имена раннего источника не меняются никогда — поэтому
// добавление второй подписки не сдвигает ничего закреплённого в первой.
//
// renamed — пары «было → стало» для журнала: хвост держится, только пока
// оригинал есть в раннем источнике. Исчезнет он — имя вернётся без хвоста,
// и закреплённый по нему выбор укажет уже на другой сервер. Сливающий
// обязан об этом сказать.
//
// Входные записи не изменяются: Merge работает на копиях, потому что его
// вход — кэш источника, который переживает это слияние.
func Merge(sources []Source) (entries []Entry, renamed []string) {
	taken := map[string]bool{}
	for _, src := range sources {
		moved := map[string]string{}
		start := len(entries)
		for _, e := range src.Entries {
			name := e.Name
			if taken[name] {
				name = e.Name + LabelSep + src.Label
				for n := 2; taken[name]; n++ {
					name = fmt.Sprintf("%s%s%s (%d)", e.Name, LabelSep, src.Label, n)
				}
				moved[e.Name] = name
				renamed = append(renamed, e.Name+" → "+name)
			}
			taken[name] = true

			c := e
			c.Name = name
			if src.ID != MainSource {
				c.Source = src.ID
			}
			if e.Proxy != nil {
				c.Proxy = make(map[string]any, len(e.Proxy))
				for k, v := range e.Proxy {
					c.Proxy[k] = v
				}
				c.Proxy["name"] = name
			}
			if len(e.Pool) > 0 {
				c.Pool = append([]string(nil), e.Pool...)
			}
			entries = append(entries, c)
		}
		// Пул балансировщика называет узлы СВОЕГО источника: переименованные
		// обязаны переименоваться и в нём, иначе пул укажет на тёзку из
		// чужой подписки.
		if len(moved) > 0 {
			for i := start; i < len(entries); i++ {
				for j, n := range entries[i].Pool {
					if to, ok := moved[n]; ok {
						entries[i].Pool[j] = to
					}
				}
			}
		}
	}
	return entries, renamed
}
