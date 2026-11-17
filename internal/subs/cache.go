package subs

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"netmoded/internal/atomicfile"
	"netmoded/internal/happ"
)

// Кэш источника (ADR-0052): последний удачный разбор подписки одного
// провайдера и итог последней попытки. Из кэшей собирается общий список
// узлов; источник, не скачавшийся сегодня, остаётся в пуле вчерашними
// узлами — иначе отказ одного провайдера убирал бы его серверы у всех,
// кто на них закреплён.
//
// В файле узлы целиком, с адресами и ключами, поэтому права 0600 — как у
// файла провайдера. Наружу он не отдаётся: панель получает только итог.

// SourceResult — итог последней попытки по источнику.
type SourceResult struct {
	ID string `json:"id"`
	// At — когда пробовали в последний раз; OKAt — когда в последний раз
	// получилось (узлы в кэше — от этого момента).
	At     time.Time  `json:"at"`
	OKAt   *time.Time `json:"ok_at"`
	Status string     `json:"status"` // ok | fail
	Nodes  int        `json:"nodes"`
	Error  string     `json:"error"`
}

type cacheEntry struct {
	Name     string          `json:"name"`
	Kind     happ.Kind       `json:"kind"`
	Type     string          `json:"type,omitempty"`
	Reason   string          `json:"reason,omitempty"`
	Pool     []string        `json:"pool,omitempty"`
	Proxy    map[string]any  `json:"proxy,omitempty"`
	Outbound json.RawMessage `json:"outbound,omitempty"`
}

type cacheFile struct {
	SourceResult
	Entries []cacheEntry `json:"entries"`
}

// validSourceID — id источника попадает в имя файла: только то, что не
// может увести путь из каталога кэша.
var validSourceID = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

// ValidSourceID сообщает, годится ли строка в id источника.
func ValidSourceID(id string) bool { return validSourceID.MatchString(id) }

func cachePath(dir, id string) string { return filepath.Join(dir, id+".json") }

// loadCache читает кэш источника. Нет файла, битый файл, чужой id — «кэша
// нет»: это не ошибка обновления, источник просто скачается заново.
func loadCache(dir, id string) (cacheFile, bool) {
	if dir == "" || !ValidSourceID(id) {
		return cacheFile{}, false
	}
	b, err := os.ReadFile(cachePath(dir, id))
	if err != nil {
		return cacheFile{}, false
	}
	var c cacheFile
	// UseNumber: порт 443 обязан вернуться в файл провайдера как 443, а не
	// как 443.0 или 4.43e2 — узел пишется для движка из этого же значения.
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&c); err != nil || c.ID != id {
		return cacheFile{}, false
	}
	return c, true
}

func (c cacheFile) entries() []happ.Entry {
	out := make([]happ.Entry, 0, len(c.Entries))
	for _, e := range c.Entries {
		out = append(out, happ.Entry{Name: e.Name, Kind: e.Kind, Type: e.Type, Reason: e.Reason,
			Pool: e.Pool, Proxy: e.Proxy, Outbound: e.Outbound})
	}
	return out
}

func saveCache(dir string, c cacheFile) error {
	if dir == "" {
		return nil
	}
	if !ValidSourceID(c.ID) {
		return errors.New("subs: id источника не годится в имя файла кэша")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return atomicfile.Write(cachePath(dir, c.ID), append(b, '\n'), filePerm)
}

func toCacheEntries(entries []happ.Entry) []cacheEntry {
	out := make([]cacheEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, cacheEntry{Name: e.Name, Kind: e.Kind, Type: e.Type, Reason: e.Reason,
			Pool: e.Pool, Proxy: e.Proxy, Outbound: e.Outbound})
	}
	return out
}

// DropCache удаляет кэш источника — когда источник удалён из настроек.
func DropCache(dir, id string) error {
	if dir == "" || !ValidSourceID(id) {
		return nil
	}
	err := os.Remove(cachePath(dir, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
