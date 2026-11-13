// Package exits — свои выходные ноды: экспорт из репо выходных нод
// (wall-breaker-exit-node, `node-export.yml` → exports/<нода>.json) и
// его проверка.
//
// Экспорт — контракт двух репозиториев, описанный в README репо нод
// («Формат exports/<нода>.json (schema 1)»). Здесь он читается строго:
// чужая schema — отказ, а не попытка угадать. Неизвестные поля внутри
// schema 1 пропускаются — так туда можно дописывать, не ломая роутер.
//
// В экспорте секреты клиента (UUID выходов, ключ и shortId Reality).
// Поэтому ни одна ошибка этого пакета не содержит значения поля — только
// его имя: ошибки уходят в журнал и в ответы API.
package exits

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Schema — единственная понимаемая версия формата экспорта.
const Schema = 1

// Node — одна выходная нода из экспорта.
type Node struct {
	Name        string
	Country     string
	Address     string // DNS-имя ноды: клиенты ходят на имя, не на IP
	Port        int
	Flow        string
	SNI         string
	Fingerprint string
	PublicKey   string // base64url, 32 байта
	ShortID     string // hex, 0–16 знаков чётной длины
	// Users — выход → UUID: «resi», «dc». Выход выбирается по UUID.
	Users map[string]string
}

// Exits — имена выходов ноды в стабильном порядке.
func (n Node) Exits() []string {
	out := make([]string, 0, len(n.Users))
	for k := range n.Users {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type wire struct {
	Schema  *int   `json:"schema"`
	Name    string `json:"name"`
	Country string `json:"country"`
	Address string `json:"address"`
	Vless   struct {
		Port        int               `json:"port"`
		Flow        string            `json:"flow"`
		SNI         string            `json:"sni"`
		Fingerprint string            `json:"fingerprint"`
		PublicKey   string            `json:"public_key"`
		ShortID     string            `json:"short_id"`
		Users       map[string]string `json:"users"`
	} `json:"vless"`
}

var (
	// То же правило, что vars/naming.yml репо нод.
	nameRe    = regexp.MustCompile(`^[a-z]{2}-[a-z]{3}-(exit|relay)-[0-9]{2}$`)
	countryRe = regexp.MustCompile(`^[a-z]{2}$`)
	// Имя хоста или IPv4 — без пробелов, схемы и порта: адрес уходит в
	// поле server узла mihomo и в vnext.address Xray как есть.
	hostRe    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	tokenRe   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	exitRe    = regexp.MustCompile(`^[a-z]{1,16}$`)
	uuidRe    = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	shortIDRe = regexp.MustCompile(`^([0-9a-f]{2}){0,8}$`)
)

// knownFlows — flow, которые понимают и mihomo, и Xray-клиенты подписки.
var knownFlows = map[string]bool{"xtls-rprx-vision": true}

// Parse разбирает и проверяет экспорт одной ноды.
func Parse(raw []byte) (Node, error) {
	var w wire
	if err := json.Unmarshal(raw, &w); err != nil {
		// Ошибки encoding/json называют позицию и тип, но не значение.
		return Node{}, fmt.Errorf("exits: экспорт не разбирается как JSON: %w", err)
	}
	if w.Schema == nil {
		return Node{}, errors.New("exits: нет поля schema — файл не из node-export.yml или старый экспорт")
	}
	if *w.Schema != Schema {
		return Node{}, fmt.Errorf("exits: schema %d не поддерживается (понимаю %d)", *w.Schema, Schema)
	}

	v := w.Vless
	switch {
	case !nameRe.MatchString(w.Name):
		return Node{}, errors.New("exits: name не по правилу <cc>-<iata>-<exit|relay>-<NN>")
	case !countryRe.MatchString(w.Country):
		return Node{}, fmt.Errorf("exits: %s: country — две строчные буквы ISO 3166-1", w.Name)
	case !hostRe.MatchString(w.Address):
		return Node{}, fmt.Errorf("exits: %s: address — имя хоста без схемы, порта и пробелов", w.Name)
	case v.Port < 1 || v.Port > 65535:
		return Node{}, fmt.Errorf("exits: %s: vless.port вне 1–65535", w.Name)
	case !knownFlows[v.Flow]:
		return Node{}, fmt.Errorf("exits: %s: vless.flow не поддерживается", w.Name)
	case !hostRe.MatchString(v.SNI):
		return Node{}, fmt.Errorf("exits: %s: vless.sni — имя хоста", w.Name)
	case !tokenRe.MatchString(v.Fingerprint):
		return Node{}, fmt.Errorf("exits: %s: vless.fingerprint пуст или с лишними знаками", w.Name)
	case !validPublicKey(v.PublicKey):
		return Node{}, fmt.Errorf("exits: %s: vless.public_key — не base64url от 32 байт", w.Name)
	case !shortIDRe.MatchString(v.ShortID):
		return Node{}, fmt.Errorf("exits: %s: vless.short_id — hex чётной длины до 16 знаков", w.Name)
	case len(v.Users) == 0:
		return Node{}, fmt.Errorf("exits: %s: vless.users пуст — у ноды нет ни одного выхода", w.Name)
	}
	users := make(map[string]string, len(v.Users))
	for exit, id := range v.Users {
		if !exitRe.MatchString(exit) {
			return Node{}, fmt.Errorf("exits: %s: vless.users — имя выхода из 1–16 строчных латинских букв", w.Name)
		}
		if !uuidRe.MatchString(id) {
			return Node{}, fmt.Errorf("exits: %s: vless.users.%s — не UUID", w.Name, exit)
		}
		users[exit] = id
	}

	return Node{
		Name: w.Name, Country: w.Country, Address: w.Address,
		Port: v.Port, Flow: v.Flow, SNI: v.SNI, Fingerprint: v.Fingerprint,
		PublicKey: v.PublicKey, ShortID: v.ShortID, Users: users,
	}, nil
}

func validPublicKey(s string) bool {
	b, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil && len(b) == 32
}

// Load читает каталог экспортов (/etc/netmoded/exits): файлы <нода>.json,
// по имени ноды. Прочее — временные файлы atomicfile (с точкой в начале),
// заметки владельца — пропускается.
//
// Каталога нет — нод нет, это не ошибка: своих выходов может не быть
// вовсе. Битый файл — отказ на весь каталог: нода, молча выпавшая из
// exits.yaml, неотличима от удалённой, а с ней пропадает и выход.
func Load(dir string) ([]Node, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("exits: каталог %s: %w", dir, err)
	}
	var nodes []Node
	for _, e := range entries {
		file := e.Name()
		if e.IsDir() || strings.HasPrefix(file, ".") || !strings.HasSuffix(file, ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, file))
		if err != nil {
			return nil, fmt.Errorf("exits: %s: %w", file, err)
		}
		n, err := Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file, err)
		}
		if file != n.Name+".json" {
			return nil, fmt.Errorf("exits: %s: внутри нода %s — файл должен называться по ноде", file, n.Name)
		}
		nodes = append(nodes, n)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	return nodes, nil
}
