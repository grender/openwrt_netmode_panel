package happ

import (
	"encoding/base64"
	"fmt"
)

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
	case "shadowsocks":
		p, t, r := convertShadowsocks(name, o)
		return p, t, r, nil
	case "":
		return nil, "", "в outbound не указан протокол", nil
	default:
		// «Нашим конвертером», а не «в узел mihomo»: движок умеет куда больше
		// протоколов, чем мы переводим. Прежняя формулировка отправила
		// владельца чинить mihomo, когда провайдер добавил shadowsocks, —
		// а чинить надо было эту таблицу.
		return nil, "", fmt.Sprintf(
			"протокол %s не переводится нашим конвертером (движок его может уметь, перевода нет у нас)",
			o.Protocol), nil
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

// ssCiphers — шифры shadowsocks, которые переводятся в узел, и длина ключа
// в байтах у шифров 2022 (у остальных пароль — произвольная строка, ноль).
//
// БЕЛЫЙ СПИСОК, а не «передать как есть», и это не осторожность вообще.
// mihomo разбирает файл провайдера целиком: одна строка с шифром, которого
// shadowsocks.CreateMethod не знает, роняет весь провайдер, и три узла на
// экзотическом шифре уносят с собой остальные тридцать семь. Шифр вне списка
// остаётся видимой строкой с причиной — дешёвый отказ вместо дорогого.
//
// Имена сверены с sing-shadowsocks2 v0.2.7 — той версией, которую тянет
// mihomo v1.19.27 (shadowaead/method.go и shadowaead_2022/method.go). В
// список взяты только распространённые AEAD и 2022: в подписке наблюдался
// один chacha20-ietf-poly1305, и расширять таблицу на то, чего провайдер не
// даёт, значит подписываться под формами, которых никто не проверял.
var ssCiphers = map[string]int{
	"aes-128-gcm":             0,
	"aes-192-gcm":             0,
	"aes-256-gcm":             0,
	"chacha20-ietf-poly1305":  0,
	"xchacha20-ietf-poly1305": 0,
	// У 2022 пароль — base64-ключ фиксированной длины. Не той длины —
	// CreateMethod отказывает, и опять целым провайдером.
	"2022-blake3-aes-128-gcm":       16,
	"2022-blake3-aes-256-gcm":       32,
	"2022-blake3-chacha20-poly1305": 32,
}

// convertShadowsocks переводит outbound shadowsocks в узел mihomo типа ss.
//
// Эталона от провайдера здесь нет, в отличие от vless и hysteria2: его
// собственный Clash-профиль (UA clash-verge/2.0) SS-узлов не содержит вовсе.
// Поэтому таблица сверена с исходником — ShadowSocksOption в
// adapter/outbound/shadowsocks.go mihomo v1.19.27: name, server, port,
// password, cipher, udp, udp-over-tcp, udp-over-tcp-version. Форма записи —
// docs/recon/raw/93-happ-shadowsocks.json.
func convertShadowsocks(name string, o xrayOutbound) (map[string]any, string, string) {
	if len(o.Settings.Servers) == 0 {
		return nil, "", "у shadowsocks-outbound нет сервера"
	}
	v := o.Settings.Servers[0]
	if v.Address == "" || v.Port == 0 {
		return nil, "", "у shadowsocks-outbound не заполнен адрес или порт"
	}
	if v.Password == "" {
		return nil, "", "у shadowsocks-outbound нет пароля"
	}
	keyLen, ok := ssCiphers[v.Method]
	if !ok {
		return nil, "", fmt.Sprintf("шифр shadowsocks %s не переводится нашим конвертером", orNone(v.Method))
	}
	if keyLen > 0 {
		key, err := base64.StdEncoding.DecodeString(v.Password)
		if err != nil || len(key) != keyLen {
			return nil, "", fmt.Sprintf("у шифра %s ключ обязан быть base64 длиной %d байт", v.Method, keyLen)
		}
	}

	// Поверх shadowsocks Xray умеет транспорты и TLS; в подписке их нет, и
	// переводить нечего сверять. Формулировка — та же, что у vless: движок
	// это умеет, не умеем мы.
	ss := o.StreamSettings
	if ss.Network != "" && ss.Network != "tcp" {
		return nil, "", fmt.Sprintf(
			"транспорт %s у shadowsocks не переводится нашим конвертером (движок его умеет, перевода нет у нас)",
			ss.Network)
	}
	if ss.Security != "" && ss.Security != "none" {
		return nil, "", fmt.Sprintf(
			"шифрование %s поверх shadowsocks не переводится нашим конвертером", ss.Security)
	}

	p := map[string]any{
		"name":     name,
		"type":     "ss",
		"server":   v.Address,
		"port":     v.Port,
		"cipher":   v.Method,
		"password": v.Password,
		// udp: true — потому что так ведёт себя клиент, под которого
		// подписка написана: Xray у shadowsocks шлёт UDP сам. У mihomo же по
		// умолчанию udp: false, и без этого ключа QUIC и игровой UDP молча
		// шли бы мимо узла — тот самый класс поломок, что уже искали на
		// серверах WSS, где режется именно UDP.
		"udp": true,
	}
	if v.UoT {
		// UDP поверх TCP переносим, только когда он включён: выключенный —
		// это умолчание и у Xray, и у mihomo. Версий у протокола две;
		// третья значила бы, что провайдер сменил формат, и собрать из неё
		// узел вслепую нельзя — mihomo отказал бы целым провайдером.
		switch v.UoTVersion {
		case 0, 1, 2:
		default:
			return nil, "", fmt.Sprintf("версия udp-over-tcp %d у shadowsocks не переводится нашим конвертером", v.UoTVersion)
		}
		p["udp-over-tcp"] = true
		if v.UoTVersion != 0 {
			p["udp-over-tcp-version"] = v.UoTVersion
		}
	}
	return p, "ss", ""
}

// orNone — подстановка для пустого значения в тексте причины.
func orNone(v string) string {
	if v == "" {
		return "(не указан)"
	}
	return v
}
