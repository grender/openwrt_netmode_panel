// Package uci разбирает вывод `uci show <package>`.
//
// Разбор отделён от исполнения намеренно: Executor отдаёт сырой текст,
// а парсер — чистая функция над строкой. Благодаря этому он проверяется
// на записанном выводе живого роутера (docs/recon/raw/), без единого мока.
//
// Формат вывода `uci show`:
//
//	wireless.wifinet0=wifi-iface          объявление секции
//	wireless.wifinet0.ssid='John24'       опция
//	wireless.wifinet0.list='a' 'b'        список
//	network.@device[0]=device             анонимная секция
//
// Значения приходят в одинарных кавычках. Одинарную кавычку внутри значения
// uci экранирует так (закрыть, backslash-кавычка, открыть):
//
//	p.s.k='it'\''s'   →   it's
//
// Последовательность приведена внутри блока кода намеренно: в обычной строке
// doc-комментария gofmt заменил бы соседние кавычки на типографские.
package uci

import (
	"fmt"
	"strings"
)

// Section — одна секция UCI.
//
// Options и Lists не пересекаются: значение с несколькими токенами попадает
// только в Lists, иначе вызывающий незаметно прочитал бы первый элемент
// списка как скаляр.
type Section struct {
	Name      string // "wifinet0" либо "@device[0]" у анонимной
	Type      string // "wifi-iface"
	Anonymous bool
	Options   map[string]string
	Lists     map[string][]string
}

// Config — разобранный пакет. Порядок секций сохраняет порядок файла:
// в UCI он значим, а для панели это ещё и порядок показа.
type Config struct {
	Package  string
	Sections []Section
}

// Значения, которые UCI считает истиной. Всё остальное — ложь;
// нераспознанное — ложь плюс DisabledValid() == false.
var truthy = map[string]bool{
	"1": true, "on": true, "true": true, "yes": true, "enabled": true,
}

var falsy = map[string]bool{
	"0": true, "off": true, "false": true, "no": true, "disabled": true,
}

// Disabled сообщает, выключена ли секция.
//
// Отсутствие опции означает ВКЛЮЧЕНО — это подтверждено на живом роутере:
// у активной станции wifinet0 опции disabled нет вовсе. Отсюда правило,
// которое обязан соблюдать пишущий код: секция никогда не создаётся без
// явного disabled, иначе она немедленно поднимется.
func (s Section) Disabled() bool {
	return truthy[strings.ToLower(s.Options["disabled"])]
}

// DisabledValid сообщает, разобрано ли значение disabled однозначно.
//
// Нераспознанное значение нельзя молча считать «включено»: вызывающий
// обязан увидеть неоднозначность и отказаться от записи, а не угадывать
// намерение пользователя.
func (s Section) DisabledValid() bool {
	v, present := s.Options["disabled"]
	if !present {
		return true
	}
	v = strings.ToLower(v)
	return truthy[v] || falsy[v]
}

// Section возвращает секцию по имени.
func (c *Config) Section(name string) (*Section, bool) {
	for i := range c.Sections {
		if c.Sections[i].Name == name {
			return &c.Sections[i], true
		}
	}
	return nil, false
}

// ByType возвращает секции указанного типа в порядке файла.
func (c *Config) ByType(typ string) []Section {
	var out []Section
	for _, s := range c.Sections {
		if s.Type == typ {
			out = append(out, s)
		}
	}
	return out
}

// HasStagedChanges сообщает, есть ли незакоммиченные правки в пакете —
// по выводу `uci changes <pkg>`.
//
// Непустой результат означает, что кто-то (LuCI, ssh) держит черновик.
// Писать в этот момент нельзя: `uci commit` публикует ВЕСЬ стейджинг пакета,
// то есть мы опубликовали бы чужую работу под своим именем и в момент,
// который её автор не выбирал.
func HasStagedChanges(raw []byte) bool {
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return true
		}
	}
	return false
}

// ChangedSections — адреса секций из вывода `uci changes <pkg>`, в порядке
// появления и без повторов.
//
// Нужен ровно для отмены СВОЕГО черновика (ADR-0028): адресовать секцию
// именем, под которым её видит uci, а не тем, под которым её видели мы.
// Разница не косметическая, и она измерена на живом роутере 2026-09-01:
//
//	uci delete network.@bridge-vlan[0]   → changes: -network.cfg08a1b0
//	uci revert network.@bridge-vlan[0]   → код 0, и НИЧЕГО не отменено
//	uci revert network.cfg08a1b0         → отменено
//
// Анонимная секция адресуется индексом только пока существует; удалённая
// живёт в стейджинге под внутренним именем, и revert по индексу молча
// промахивается — возвращая при этом успех. Демонтаж после такого «успеха»
// оставлял застрявший черновик и докладывал apply_failed вместо
// stale_draft, то есть советовал повторить там, где повтор упрётся в
// foreign_staged_changes.
//
// Формы строк (все три встречаются в одном выводе):
//
//	-network.cfg08a1b0                  удалена секция
//	-network.cfg09a1b0.ports            удалена опция
//	network.homelan=interface           создана секция
//	network.homelan.ipaddr='192.168.0.85'  записана опция
//
// Возвращается ровно «пакет.секция»: revert адресуется секции целиком —
// отменять отдельную опцию, оставляя соседние, uci не умеет.
func ChangedSections(pkg string, raw []byte) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "-")
		// Значение опции отбрасывается до разбора адреса: в нём бывают и
		// точки, и знак равенства (ssid, пароли, CIDR с отрицанием).
		if i := strings.IndexByte(line, '='); i >= 0 {
			line = line[:i]
		}
		parts := strings.SplitN(line, ".", 3)
		if len(parts) < 2 || parts[0] != pkg || parts[1] == "" {
			continue
		}
		sec := parts[1]
		if !seen[sec] {
			seen[sec] = true
			out = append(out, sec)
		}
	}
	return out
}

// ParseShow разбирает вывод `uci show pkg`.
//
// Строка из чужого пакета — ошибка, а не повод её пропустить: молчаливый
// пропуск при опечатке в имени пакета выглядел бы как «на роутере пусто».
//
// Отклонение от docs/contracts/executor.md: контракт рисует сигнатуру как
// ParseShow([]byte), без имени пакета. Параметр оставлен намеренно — он
// стоит одной проверки и ловит целый класс ошибок, при котором результат
// выглядит правдоподобно пустым. Отклонение зафиксировано здесь, а не
// молча.
func ParseShow(pkg string, raw []byte) (*Config, error) {
	c := &Config{Package: pkg}
	prefix := pkg + "."

	for n, line := range strings.Split(string(raw), "\n") {
		lineno := n + 1
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("строка %d: нет '=': %q", lineno, line)
		}
		key, val := line[:eq], line[eq+1:]

		if !strings.HasPrefix(key, prefix) {
			return nil, fmt.Errorf("строка %d: ключ %q не из пакета %q", lineno, key, pkg)
		}
		rest := key[len(prefix):]
		if rest == "" {
			return nil, fmt.Errorf("строка %d: пустой ключ", lineno)
		}

		name, option := splitSectionOption(rest)

		if option == "" {
			// Объявление секции: pkg.name=type
			if val == "" {
				return nil, fmt.Errorf("строка %d: у секции %q пустой тип", lineno, name)
			}
			c.Sections = append(c.Sections, Section{
				Name:      name,
				Type:      val,
				Anonymous: strings.HasPrefix(name, "@"),
				Options:   map[string]string{},
				Lists:     map[string][]string{},
			})
			continue
		}

		// Опция: pkg.name.option='value' [ 'value'... ]
		sec, ok := c.Section(name)
		if !ok {
			return nil, fmt.Errorf("строка %d: опция %q раньше объявления секции %q", lineno, option, name)
		}
		values, err := unquote(val)
		if err != nil {
			return nil, fmt.Errorf("строка %d: %w", lineno, err)
		}
		switch len(values) {
		case 0:
			sec.Options[option] = ""
		case 1:
			sec.Options[option] = values[0]
		default:
			sec.Lists[option] = values
		}
	}

	return c, nil
}

// splitSectionOption делит остаток ключа на имя секции и имя опции.
//
// Имя анонимной секции содержит точку внутри скобок (@device[0]), поэтому
// делить нужно по последней точке за пределами скобок, а не по первой.
func splitSectionOption(rest string) (section, option string) {
	depth := 0
	last := -1
	for i, r := range rest {
		switch r {
		case '[':
			depth++
		case ']':
			depth--
		case '.':
			if depth == 0 {
				last = i
			}
		}
	}
	if last < 0 {
		return rest, ""
	}
	return rest[:last], rest[last+1:]
}

// unquote разбирает правую часть строки — один или несколько токенов
// в одинарных кавычках, разделённых пробелами.
//
// Значение без кавычек тоже принимается: некоторые сборки uci так выводят
// простые значения, и падать на этом было бы неоправданно строго.
func unquote(s string) ([]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	if !strings.HasPrefix(s, "'") {
		return []string{s}, nil
	}

	var out []string
	var b strings.Builder
	inQuote := false

	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case !inQuote && ch == '\'':
			inQuote = true
			b.Reset()

		case inQuote && ch == '\'':
			// Экранированная кавычка: '\'' — закрыли, backslash, кавычка, открыли.
			if strings.HasPrefix(s[i:], `'\''`) {
				b.WriteByte('\'')
				i += 3
				continue
			}
			inQuote = false
			out = append(out, b.String())

		case inQuote:
			b.WriteByte(ch)

		case ch == ' ' || ch == '\t':
			// разделитель между токенами списка

		default:
			return nil, fmt.Errorf("мусор вне кавычек: %q", s[i:])
		}
	}

	if inQuote {
		return nil, fmt.Errorf("незакрытая кавычка в %q", s)
	}
	return out, nil
}
