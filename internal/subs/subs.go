// Package subs — политика обновления подписки: что скачать, куда положить,
// в каком порядке показать.
//
// Круг 3 по ADR-0001. Разбор чужого формата лежит в internal/happ, доступ
// к Clash API — в internal/nikki; здесь только решения, которые не диктует
// ни тот, ни другой: писать ли новый файл провайдера, чем считать неудачу,
// как склеить порядок подписки с живым состоянием mihomo.
package subs

import (
	"netmoded/internal/happ"
	"netmoded/internal/nikki"
)

// Member — строка списка узлов, как её видит панель.
//
// Встроенный nikki.Proxy, а не копия его полей: форма узла описана в одном
// месте (openapi, схема Proxy), и разъехаться они не могут. Два поля сверху
// — то, чего mihomo про запись не знает и знать не может: вид записи и
// причина непригодности приходят из подписки, а не из Clash API.
type Member struct {
	nikki.Proxy
	Kind happ.Kind `json:"kind"`
	// Reason непуст только у happ.KindUnsupported.
	Reason string `json:"reason,omitempty"`
}
