package happ

import "fmt"

// xhttpSupported — принимает ли mihomo этой сборки транспорт xhttp.
//
// ЭТО ВЫКЛЮЧАТЕЛЬ, А НЕ МЁРТВЫЙ КОД. На роутере стоит mihomo v1.19.27, и
// поддержка xhttp у него взята из документации metacubex, а не снята с
// железа: на момент разведки ssh отсутствовал. Проверка живьём впереди.
//
// Если движок откажется принять файл провайдера с xhttp, здесь ставится
// false — и одиннадцать записей на этом транспорте уходят в панель как
// KindUnsupported с причиной, назвающей версию движка, вместо того чтобы
// уронить обновление подписки целиком. Догадка, которая может оказаться
// неверной, не должна стоить всей подписки: без выключателя единственным
// способом починиться был бы выпуск демона.
const xhttpSupported = true

// convert переводит outbound Xray в объект узла mihomo.
//
// Таблица перевода не выдумана, а сверена: собранные по ней узлы совпали с
// готовым Clash-профилем того же провайдера (ответ на User-Agent
// clash-verge/2.0) по 15 общим узлам — type, server, port, uuid, network,
// flow, tls, servername, client-fingerprint, reality-opts.public-key,
// password, sni, alpn. Расхождений по существу ноль.
//
// Возвращает либо готовый узел с типом, либо пустую причину непригодности.
// Причина — короткий русский текст: он уходит в панель как есть, и владелец
// читает его вместо того, чтобы гадать, куда делся узел.
//
// Четвёртое значение — ключи xhttpSettings.extra, которых конвертер не
// знает (см. copyXHTTPExtra). Они не мешают собрать узел, но должны дойти
// до журнала: молча потерянный ключ — это узел, который «то работает, то
// нет», без единой зацепки для владельца.
func convert(name string, o xrayOutbound, xhttp bool) (proxy map[string]any, typ string, reason string, unknown []string) {
	switch o.Protocol {
	case "vless":
		return convertVLESS(name, o, xhttp)
	case "hysteria":
		p, t, r := convertHysteria(name, o)
		return p, t, r, nil
	case "":
		return nil, "", "в outbound не указан протокол", nil
	default:
		return nil, "", fmt.Sprintf("протокол %s не переводится в узел mihomo", o.Protocol), nil
	}
}

func convertVLESS(name string, o xrayOutbound, xhttp bool) (map[string]any, string, string, []string) {
	if len(o.Settings.VNext) == 0 || len(o.Settings.VNext[0].Users) == 0 {
		return nil, "", "у vless-outbound нет адреса или пользователя", nil
	}
	v := o.Settings.VNext[0]
	if v.Address == "" || v.Port == 0 {
		return nil, "", "у vless-outbound не заполнен адрес или порт", nil
	}
	if v.Users[0].ID == "" {
		return nil, "", "у vless-outbound нет uuid", nil
	}

	ss := o.StreamSettings
	switch ss.Network {
	case "tcp":
	case "xhttp":
		if !xhttp {
			return nil, "", "транспорт xhttp не принят движком mihomo этой сборки", nil
		}
	default:
		// Формулировка важна: ws, grpc и h2 движок УМЕЕТ — их не умеет
		// наш конвертер, потому что в подписке они не встречались и
		// эталона для сверки нет. Написав «не поддержан», мы отправили бы
		// владельца менять версию mihomo, где чинить нечего.
		return nil, "", fmt.Sprintf(
			"транспорт %s не переводится нашим конвертером (движок его умеет, перевода нет у нас)",
			orNone(ss.Network)), nil
	}

	p := map[string]any{
		"name":    name,
		"type":    "vless",
		"server":  v.Address,
		"port":    v.Port,
		"uuid":    v.Users[0].ID,
		"network": ss.Network,
		// Провайдер в своём же Clash-профиле ставит udp: true у всех vless.
		"udp": true,
	}
	// Пустой flow не переносим — но не потому, что mihomo отличал бы его
	// от отсутствующего: не отличает. Он считает флоу заданным только при
	// длине не меньше 16 символов (adapter/outbound/vless.go:419-429), а
	// всё короче, включая пустую строку, молча означает «без flow».
	// То есть ключ здесь опускается ради читаемости профиля, а не ради
	// поведения движка. У xhttp-узлов flow пуст всегда.
	if f := v.Users[0].Flow; f != "" {
		p["flow"] = f
	}

	switch ss.Security {
	case "reality":
		r := ss.RealitySettings
		if r.PublicKey == "" {
			return nil, "", "у reality-узла нет публичного ключа", nil
		}
		// Отпечаток у reality обязателен так же, как публичный ключ:
		// без него подделывать нечего, и mihomo скажет об этом только в
		// свой лог, а в панели останется узел, который просто не
		// подключается. Отказ здесь превращает молчание в строку с
		// причиной.
		if r.Fingerprint == "" {
			return nil, "", "у reality-узла нет отпечатка (client-fingerprint)", nil
		}
		p["tls"] = true
		p["servername"] = r.ServerName
		p["client-fingerprint"] = r.Fingerprint
		p["reality-opts"] = map[string]any{
			"public-key": r.PublicKey,
			"short-id":   r.ShortID,
		}
	case "tls":
		t := ss.TLSSettings
		p["tls"] = true
		p["servername"] = t.ServerName
		// В отличие от reality, обычному TLS отпечаток не обязателен:
		// пустой означает «без подделки uTLS», и это рабочий режим, а не
		// поломка. Поэтому здесь ключ опускается, а не приводит к отказу.
		if t.Fingerprint != "" {
			p["client-fingerprint"] = t.Fingerprint
		}
		if len(t.ALPN) > 0 {
			p["alpn"] = append([]string(nil), t.ALPN...)
		}
	default:
		// Голый vless без шифрования в подписке не встречался, и молча
		// собрать из него узел значило бы отправить трафик открытым,
		// решив за владельца. Пусть строка останется видимой.
		return nil, "", fmt.Sprintf("режим шифрования %s у vless не поддержан", orNone(ss.Security)), nil
	}

	var unknown []string
	if ss.Network == "xhttp" {
		x := ss.XHTTPSettings
		opts := map[string]any{
			"path": x.Path,
			"host": x.Host,
			"mode": x.Mode,
		}
		unknown = copyXHTTPExtra(opts, x.Extra)
		p["xhttp-opts"] = opts
	}
	return p, "vless", "", unknown
}

func convertHysteria(name string, o xrayOutbound) (map[string]any, string, string) {
	// Hysteria 2 приезжает под именем hysteria: protocol — строка
	// "hysteria", а двойка живёт в поле version, причём сразу в двух местах
	// (settings и hysteriaSettings). По имени протокола версию не отличить
	// вовсе, поэтому смотрим оба поля и хватаемся за любое.
	version := o.Settings.Version
	if version == 0 {
		version = o.StreamSettings.HysteriaSettings.Version
	}
	if version != 2 {
		// Версия 1 в подписке не встречалась, и в mihomo она называется
		// иначе (тип hysteria, другой набор полей). Собирать её вслепую
		// не по чему — эталона нет.
		return nil, "", fmt.Sprintf("hysteria версии %d не переводится в узел mihomo", version)
	}

	s := o.Settings
	if s.Address == "" || s.Port == 0 {
		return nil, "", "у hysteria-outbound не заполнен адрес или порт"
	}
	auth := o.StreamSettings.HysteriaSettings.Auth
	if auth == "" {
		return nil, "", "у hysteria-outbound нет пароля"
	}

	p := map[string]any{
		"name":     name,
		"type":     "hysteria2",
		"server":   s.Address,
		"port":     s.Port,
		"password": auth,
		"sni":      o.StreamSettings.TLSSettings.ServerName,
	}
	if alpn := o.StreamSettings.TLSSettings.ALPN; len(alpn) > 0 {
		p["alpn"] = append([]string(nil), alpn...)
	}
	// client-fingerprint у hysteria2 не ставим: провайдер в своём
	// Clash-профиле его не ставит тоже, хотя в Xray-конфиге fingerprint
	// есть. Эталон здесь весомее симметрии.
	return p, "hysteria2", ""
}

// orNone — подстановка для пустого значения в тексте причины.
func orNone(v string) string {
	if v == "" {
		return "(не указан)"
	}
	return v
}
