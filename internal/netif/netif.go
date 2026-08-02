// Package netif разбирает состояние сетевого интерфейса из ubus.
//
// Отделён от wireless намеренно: там радио и эфир, здесь L3 — адрес,
// маршрут, DNS. Смешивать их значило бы связать «сеть в эфире видна»
// с «интернет работает», а это разные вещи, и их расхождение как раз
// самое полезное, что панель может показать.
package netif

import (
	"encoding/json"
	"fmt"
)

// Status — состояние интерфейса upstream.
type Status struct {
	Up        bool   `json:"up"`
	Pending   bool   `json:"pending"`
	Available bool   `json:"available"`
	Uptime    int    `json:"uptime"`
	Proto     string `json:"proto"`
	Device    string `json:"l3_device"`

	Addresses  []Addr   `json:"ipv4-address"`
	Routes     []Route  `json:"route"`
	DNSServers []string `json:"dns-server"`
}

type Addr struct {
	Address string `json:"address"`
	Mask    int    `json:"mask"`
}

type Route struct {
	Target  string `json:"target"`
	Mask    int    `json:"mask"`
	Nexthop string `json:"nexthop"`
}

// ParseStatus разбирает `ubus call network.interface.<name> status`.
func ParseStatus(b []byte) (Status, error) {
	var s Status
	if err := json.Unmarshal(b, &s); err != nil {
		return Status{}, fmt.Errorf("network.interface status: %w", err)
	}
	return s, nil
}

// Online сообщает, есть ли работающий внешний канал.
//
// Одного `up` недостаточно: интерфейс поднимается и без аренды DHCP —
// тогда up=true, адреса нет, интернета нет. Проверка одного флага дала бы
// уверенное «всё хорошо» ровно в той ситуации, ради диагностики которой
// панель и открывают.
//
// Признак — сочетание трёх условий (docs/recon/ubus.md):
// интерфейс поднят, есть адрес IPv4, есть маршрут по умолчанию с шлюзом.
func (s Status) Online() bool {
	if !s.Up || len(s.Addresses) == 0 {
		return false
	}
	return s.HasDefaultRoute()
}

// HasDefaultRoute сообщает, есть ли маршрут по умолчанию с непустым шлюзом.
func (s Status) HasDefaultRoute() bool {
	for _, r := range s.Routes {
		if r.Target == "0.0.0.0" && r.Mask == 0 && r.Nexthop != "" {
			return true
		}
	}
	return false
}

// Gateway возвращает шлюз по умолчанию, если он есть.
func (s Status) Gateway() string {
	for _, r := range s.Routes {
		if r.Target == "0.0.0.0" && r.Mask == 0 {
			return r.Nexthop
		}
	}
	return ""
}
