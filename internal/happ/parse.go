package happ

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ErrNotArray — тело подписки разобралось как JSON, но корень не массив.
//
// Отдельно от ErrBadJSON: провайдер, ответивший объектом с полем error или
// HTML-страницей входа, — это не «битый JSON», а внятный ответ не про то.
// Слив их в одну ошибку, владелец увидел бы «подписка не разбирается» и пошёл
// бы искать поломку в нашем парсере вместо истёкшего доступа.
var ErrNotArray = errors.New("happ: ответ подписки не массив записей")

// ErrBadJSON — тело подписки не является JSON вовсе.
var ErrBadJSON = errors.New("happ: ответ подписки не разбирается как JSON")

// ErrEmpty — массив есть, записей в нём нет.
//
// Пустой массив — отказ разбора, а вот ноль УЗЛОВ при непустом массиве
// отказом не является: это факт про содержимое подписки, и вызывающий узнаёт
// о нём из Summary. Разница существенная: в первом случае писать в файл
// провайдера нечего и незачем, во втором есть что показать в панели.
var ErrEmpty = errors.New("happ: подписка не содержит записей")

// Parse разбирает тело подписки в записи, СТРОГО сохраняя порядок провайдера.
//
// Порядок здесь — не оформление, а главная ценность разбора: у провайдера он
// авторский (сначала «Авто», потом узлы по регионам, потом заголовок раздела
// и обходы белых списков), а Clash API отдаёт узлы объектом и порядок теряет
// безвозвратно. Восстановить его потом неоткуда — только отсюда.
func Parse(raw []byte) ([]Entry, error) {
	return parse(raw, xhttpSupported)
}

// parse — тело Parse с явным положением выключателя xhttp.
//
// Выключатель существует как константа (см. convert.go), а параметр здесь —
// чтобы тест мог доказать ОБА его положения, не превращая константу в
// изменяемую глобальную переменную. Глобальная переменная, которую тесты
// крутят туда-сюда, — это гонка под -race и зависимость тестов от порядка
// запуска; параметр не стоит ничего и не даёт ни того, ни другого.
func parse(raw []byte, xhttp bool) ([]Entry, error) {
	var records []json.RawMessage
	if err := json.Unmarshal(raw, &records); err != nil {
		// json.Valid отделяет «не JSON» от «JSON, но не массив». Текст
		// ошибки encoding/json для этого не годится: он разный у разных
		// корневых типов и меняется от версии к версии.
		if json.Valid(raw) {
			return nil, ErrNotArray
		}
		return nil, fmt.Errorf("%w: %v", ErrBadJSON, err)
	}
	if len(records) == 0 {
		return nil, ErrEmpty
	}

	entries := make([]Entry, len(records))
	// ids — отпечаток узла каждой записи, по которому ищется двойник
	// разделителя. Пусто у «Авто» и у записей без пригодного outbound.
	ids := make([]string, len(records))

	for i, rec := range records {
		entries[i], ids[i] = parseRecord(rec, i, xhttp)
	}

	markSeparators(entries, ids)
	dedupeNames(entries)
	return entries, nil
}

// parseRecord разбирает одну запись и возвращает её вместе с отпечатком узла.
func parseRecord(rec json.RawMessage, index int, xhttp bool) (Entry, string) {
	var xr xrayRecord
	if err := json.Unmarshal(rec, &xr); err != nil {
		// Одна кривая запись не должна ронять всю подписку: остальные
		// двадцать девять исправны, и владельцу полезнее список с одной
		// строкой-объяснением, чем пустая панель. Имя достаём отдельной
		// попыткой — оно единственное, что делает строку узнаваемой.
		return Entry{
			Name:   recordName(rec, index),
			Kind:   KindUnsupported,
			Reason: "запись подписки не разбирается",
		}, ""
	}

	name := xr.Remarks
	if name == "" {
		name = positionalName(index)
	}

	useful, ok := xr.usefulOutbound()
	if !ok {
		// Балансировщик без wl-outbound — это «Авто | Лучший сервер»:
		// группа поверх десятка серверов, у которой своего адреса нет.
		// Подключаться к ней нечем, и в список узлов она не идёт.
		if len(xr.Routing.Balancers) > 0 {
			return Entry{Name: name, Kind: KindAuto}, ""
		}
		return Entry{
			Name:   name,
			Kind:   KindUnsupported,
			Reason: "в записи нет пригодного outbound (ожидался тег proxy или proxy-wl-*)",
		}, ""
	}

	// Отпечаток считается ДО перевода и не зависит от выключателя xhttp.
	// Так и задумано: выключатель говорит про то, что примет движок, а не
	// про то, чем запись является. Иначе погашенный xhttp превратил бы
	// заголовок раздела в строку «неподдержанный узел» — шум на ровном
	// месте, причём в единственной строке, ради которой правило и писалось.
	id := useful.identity()

	proxy, typ, reason := convert(name, useful, xhttp)
	if reason != "" {
		return Entry{Name: name, Kind: KindUnsupported, Reason: reason}, id
	}
	return Entry{Name: name, Kind: KindNode, Type: typ, Proxy: proxy}, id
}

// markSeparators опознаёт заголовки разделов — вторым проходом по всем
// записям.
//
// Правило двухступенчатое, и вторая ступень обязательна.
//
//  1. Имя не начинается с эмодзи-флага — это только КАНДИДАТ. У 29 записей
//     из 30 имя открывается парой regional indicator, у заголовка — стрелкой
//     вниз, и другого признака в данных нет: «⬇️ Обходы белых списков ⬇️» —
//     побайтовая копия узла «Германия (БС-3)» вплоть до балансировщика.
//
//  2. Кандидат становится разделителем, только если его узел ДУБЛИРУЕТ узел
//     другой записи. Это защита, а не украшение: заголовок в снятой подписке
//     копирует соседа, но безымянный-без-флага и при этом НАСТОЯЩИЙ узел
//     провайдер может завести в любой день, и спрятать его под видом
//     заголовка значило бы отнять у владельца работающий сервер. Уникальный
//     узел без флага остаётся узлом.
//
// Двойник ищется по всему массиву, и до кандидата, и после: в снятой
// подписке заголовок стоит 25-м, а его близнец — 28-м.
func markSeparators(entries []Entry, ids []string) {
	for i := range entries {
		if entries[i].Kind == KindAuto || ids[i] == "" {
			continue
		}
		if hasFlagPrefix(entries[i].Name) {
			continue
		}
		if !hasTwin(ids, i) {
			continue
		}
		// Заголовок — не узел: ни типа, ни объекта для файла провайдера,
		// ни причины непригодности у него быть не должно.
		entries[i] = Entry{Name: entries[i].Name, Kind: KindSeparator}
	}
}

// hasTwin сообщает, встречается ли отпечаток ещё где-нибудь в массиве.
func hasTwin(ids []string, self int) bool {
	for j, id := range ids {
		if j != self && id == ids[self] {
			return true
		}
	}
	return false
}

// hasFlagPrefix сообщает, открывается ли имя эмодзи-флагом.
//
// Флаг в Unicode — это ПАРА regional indicator (U+1F1E6…U+1F1FF), а не один
// символ. Проверять только первую руну нельзя: одиночный regional indicator
// рисуется буквой в рамке и флагом не является, а значит и признаком узла
// служить не может.
func hasFlagPrefix(name string) bool {
	first, size := utf8.DecodeRuneInString(name)
	if !isRegionalIndicator(first) {
		return false
	}
	second, _ := utf8.DecodeRuneInString(name[size:])
	return isRegionalIndicator(second)
}

func isRegionalIndicator(r rune) bool { return r >= 0x1F1E6 && r <= 0x1F1FF }

// dedupeNames разводит совпавшие имена суффиксом « (2)», « (3)» и так далее.
//
// В снятой подписке дубликатов нет — проверено. Страховка нужна не от
// сегодняшних данных, а от завтрашних: имя у нас ключ и в манифесте, и в
// выборе узла через Clash API, а mihomo держит узлы объектом (map). Два узла
// с одним именем — это молча потерянный узел в файле провайдера и выбор,
// который попадает не туда, куда показывал владелец.
func dedupeNames(entries []Entry) {
	seen := make(map[string]int, len(entries))
	for i := range entries {
		base := entries[i].Name
		n := seen[base]
		seen[base] = n + 1
		if n == 0 {
			continue
		}
		name := fmt.Sprintf("%s (%d)", base, n+1)
		// Разведённое имя тоже может совпасть с чужим — например, если
		// провайдер сам прислал «Германия» и «Германия (2)». Крутим,
		// пока не найдём свободное.
		for seen[name] > 0 {
			seen[base]++
			name = fmt.Sprintf("%s (%d)", base, seen[base])
		}
		seen[name] = 1
		entries[i].Name = name
		// Имя узла в объекте для mihomo обязано ехать за именем записи:
		// именно оно попадёт в файл провайдера и в группы профиля.
		if entries[i].Proxy != nil {
			entries[i].Proxy["name"] = name
		}
	}
}

// recordName достаёт remarks из записи, которая целиком не разобралась.
func recordName(rec json.RawMessage, index int) string {
	var head struct {
		Remarks string `json:"remarks"`
	}
	if err := json.Unmarshal(rec, &head); err == nil && head.Remarks != "" {
		return head.Remarks
	}
	return positionalName(index)
}

// positionalName — имя для записи, у которой своего нет.
//
// Позиция, а не «без имени»: безымянных строк в списке может оказаться
// несколько, и одинаковые имена тут же разъедутся по dedupeNames в « (2)»,
// « (3)» — номер записи владелец хотя бы сопоставит с подпиской.
func positionalName(index int) string {
	return fmt.Sprintf("запись %d", index+1)
}

// Summarize считает записи по видам.
//
// «Авто» отдельного счётчика не имеет намеренно: это не узел и не отказ, а
// ровно одна служебная строка, которая у провайдера всегда есть и всегда
// одна. Счётчик, который во всех наблюдениях равен единице, ничего не
// сообщает — а Summary уходит в журнал обновлений, где место дорого.
func Summarize(entries []Entry) Summary {
	var s Summary
	for _, e := range entries {
		switch e.Kind {
		case KindNode:
			s.Nodes++
		case KindUnsupported:
			s.Unsupported++
		case KindSeparator:
			s.Separators++
		}
	}
	return s
}

// --- форма записи Happ-JSON -------------------------------------------------

// xrayRecord — одна запись подписки: полный конфиг Xray.
//
// Разбираем из него ровно три вещи: имя, наличие балансировщика и outbounds.
// Всё остальное (dns, inbounds, log, правила routing) описывает КЛИЕНТ, а
// клиентом является mihomo с нашим профилем, а не подписка. Перенос чужих
// правил routing дал бы чужую политику вместо своей, причём в тридцати
// одинаковых экземплярах.
type xrayRecord struct {
	Remarks   string         `json:"remarks"`
	Outbounds []xrayOutbound `json:"outbounds"`
	Routing   struct {
		Balancers []json.RawMessage `json:"balancers"`
	} `json:"routing"`
}

// usefulOutbound выбирает единственный outbound, который станет узлом.
//
// У записи «обхода белых списков» их два содержательных: proxy-decoy-* и
// proxy-wl-*. Берём ИМЕННО wl, и это не догадка по названию: в
// strategy.settings.costs штраф стоит на wl-теге, а fallbackTag указывает на
// него же — балансировщик ходит через decoy, пока тот жив, и сваливается на
// wl. При этом decoy — обычный reality-узел, который в списке УЖЕ ЕСТЬ
// отдельной строкой. Взяв decoy, мы завели бы дубликат под именем «БС-N» и
// потеряли единственный сервер, ради которого запись существует.
func (r xrayRecord) usefulOutbound() (xrayOutbound, bool) {
	for _, ob := range r.Outbounds {
		if strings.HasPrefix(ob.Tag, "proxy-wl") {
			return ob, true
		}
	}
	for _, ob := range r.Outbounds {
		if ob.Tag == "proxy" {
			return ob, true
		}
	}
	return xrayOutbound{}, false
}

// xrayOutbound — outbound Xray в объёме, который нужен переводу.
//
// settings у vless и у hysteria — разные объекты (vnext против плоских
// address/port), но ключи не пересекаются, поэтому одна структура покрывает
// обе формы без танцев с json.RawMessage.
type xrayOutbound struct {
	Tag      string `json:"tag"`
	Protocol string `json:"protocol"`
	Settings struct {
		VNext []struct {
			Address string `json:"address"`
			Port    int    `json:"port"`
			Users   []struct {
				ID   string `json:"id"`
				Flow string `json:"flow"`
			} `json:"users"`
		} `json:"vnext"`

		// Плоские поля — форма hysteria.
		Address string `json:"address"`
		Port    int    `json:"port"`
		Version int    `json:"version"`
	} `json:"settings"`
	StreamSettings struct {
		Network         string `json:"network"`
		Security        string `json:"security"`
		RealitySettings struct {
			ServerName  string `json:"serverName"`
			PublicKey   string `json:"publicKey"`
			ShortID     string `json:"shortId"`
			Fingerprint string `json:"fingerprint"`
		} `json:"realitySettings"`
		TLSSettings struct {
			ServerName  string   `json:"serverName"`
			Fingerprint string   `json:"fingerprint"`
			ALPN        []string `json:"alpn"`
		} `json:"tlsSettings"`
		XHTTPSettings struct {
			Mode string `json:"mode"`
			Host string `json:"host"`
			Path string `json:"path"`
			// Extra разбирается картой, а не структурой, намеренно:
			// значения в нём разнородны (строки, булевы, числа и
			// вложенный объект headers), состав меняется от узла к
			// узлу, а главное — неизвестный ключ обязан доехать до
			// таблицы перевода целым, чтобы решение о нём принимал
			// convert.go, а не молчаливый пропуск при разборе.
			Extra map[string]any `json:"extra"`
		} `json:"xhttpSettings"`
		HysteriaSettings struct {
			Version int    `json:"version"`
			Auth    string `json:"auth"`
		} `json:"hysteriaSettings"`
	} `json:"streamSettings"`
}

// identity — отпечаток сервера, по которому ищется двойник разделителя.
//
// Сравниваются ровно те поля, которые делают узел ТЕМ ЖЕ САМЫМ: протокол,
// транспорт, адрес, порт и секрет (uuid у vless, auth у hysteria). Имя в
// отпечаток не входит по определению задачи — разделитель от своего близнеца
// только именем и отличается.
//
// Пустая строка означает «сравнивать не с чем»: запись без адреса или без
// секрета не должна случайно совпасть с другой такой же и объявить себя
// заголовком.
func (o xrayOutbound) identity() string {
	server, port, secret := o.endpoint()
	if server == "" || secret == "" {
		return ""
	}
	return fmt.Sprintf("%s|%s|%s|%d|%s",
		o.Protocol, o.StreamSettings.Network, server, port, secret)
}

// endpoint возвращает адрес, порт и секрет outbound-а в единой форме.
func (o xrayOutbound) endpoint() (server string, port int, secret string) {
	if len(o.Settings.VNext) > 0 {
		v := o.Settings.VNext[0]
		if len(v.Users) > 0 {
			secret = v.Users[0].ID
		}
		return v.Address, v.Port, secret
	}
	return o.Settings.Address, o.Settings.Port, o.StreamSettings.HysteriaSettings.Auth
}
