# Nikki: обход зависит от видимости домена, sniffer включён

**Снято с роутера 2026-09-08**, сырьё —
[`raw/89-nikki-sniffer-connections.txt`](raw/89-nikki-sniffer-connections.txt).
Повод: ПК «neural» в LAN-порту при выключенном пробросе получил аренду
(`192.168.9.208`), маршрут ушёл через `wwan`, nftables-таблица `inet nikki`
перехватывала `br-lan` — а сайт из набора обхода не открывался. Первая
гипотеза «обход не покрывает LAN-порт» **не подтвердилась**: покрывает так
же, как Wi-Fi. Не работал он по другой причине.

## Что наблюдалось

`GET /connections` Clash API до включения sniffer, свёрнуто по источнику:

| источник | dnsMode | исход |
|---|---|---|
| 192.168.9.208 (ПК, LAN-порт) | normal | 115 → `MATCH,DIRECT` |
| 192.168.9.208 | fake-ip | 31 → DIRECT (Windows, Steam), 2 → RuleSet → PROXY |
| 192.168.9.238 (телефон, Wi-Fi) | fake-ip | 43 → DIRECT, 11 → RuleSet → PROXY |

Три факта из этой таблицы и из отдельных записей:

- У ПК **три четверти соединений с `dnsMode: normal` и пустым `host`**, у
  Wi-Fi-клиента — четверть. `normal` значит «адрес назначения не из
  fake-ip диапазона»: имя резолвили не через роутер.
- Среди соединений ПК живое к `mozilla.cloudflare-dns.com` (72 КБ ↑ /
  210 КБ ↓) — **Firefox ходит в DoH мимо dnsmasq**. Перехват порта 53
  (`lan_dns_hijack`) тут бессилен: это TCP 443.
- Соединения ПК к Google (`142.251.151.4`, 1885 байт ↑, 0 ↓) и Fastly
  (`151.101.129.91`, 225 ↑, 0 ↓) **висят без ответа** — прямое
  подключение к заблокированному адресу. Это и есть «сайт не открывается».

Почему так: наши наборы почти все **доменные** (`nm-geosite-*`; IP-наборы
только у facebook/telegram/twitter). Соединение, у которого mihomo не знает
домена, не совпадает ни с одним `RULE-SET` и падает в `MATCH,DIRECT`. А домен
mihomo узнаёт двумя путями — из fake-ip (клиент спросил роутер) или из
sniffer (SNI в TLS ClientHello). Второй путь был выключен: секции `sniffer`
в `/etc/nikki/run/config.yaml` не было, `sniffHost` пуст у всех записей.

## Как sniffer включается в nikki

`/etc/nikki/ucode/mixin.uc:128-150` (цитата в `raw/89`): `sniffer.enable`
читается из `nikki.mixin.sniffer`, список протоколов — из секций
`nikki.@sniff[]`, но **только при `nikki.mixin.sniffer_sniff=1`**. На роутере
ключа `sniffer` не было вовсе, `sniffer_sniff='0'`; при этом три секции
`@sniff` (HTTP 80/8080, TLS 443/8443, QUIC 443/8443, `overwrite_destination=1`)
стояли с `enabled='1'` — описаны, но не рендерились.

Включено так:

```sh
uci set nikki.mixin.sniffer=1
uci set nikki.mixin.sniffer_sniff=1
uci commit nikki
/etc/init.d/nikki restart
```

Откат — те же два ключа в `0` и рестарт. Это настройка **пакета nikki**, не
нашего демона: `netmoded` в `nikki` uci не пишет, панель этот ключ не
показывает. Перезапуск nikki рвёт обход на несколько секунд — тот же
эффект, что у `netmode-apply nikki`.

## Что стало

Через 20 с после рестарта `config.yaml:178` содержит `sniffer: enable: true`
с тремя протоколами; у ПК **13 соединений с `dnsMode: normal` ушли через
`RuleSet` → PROXY**, `sniffHost` заполнен (`youtube.com`,
`rr1---sn-….googlevideo.com`, `www.google.com`). Wi-Fi-клиент по-прежнему идёт
через `RuleSet`; `lan_inbound_device = { "br-lan" }` не изменился.
Оставшиеся `normal … DIRECT` записи ПК — домены вне наборов (Mozilla,
gstatic), это правильный `MATCH`.

## Ограничения и выводы

- **Обход зависит от видимости домена, а не от того, откуда пришёл клиент.**
  LAN-порт и Wi-Fi равны; неравны клиенты с DoH/DoT/Private DNS. Sniffer
  закрывает их для TLS с открытым SNI и для QUIC.
- **ECH** (Encrypted Client Hello) прячет SNI — такой клиент снова уйдёт в
  `MATCH,DIRECT`. Лечится только IP-наборами или запретом ECH на клиенте;
  сейчас не наблюдалось.
- `override-destination: true` подменяет адрес назначения на резолв
  снифнутого имени — для fake-ip это штатно, для `normal` соединений
  меняет IP, к которому подключился клиент. Побочных эффектов за время
  наблюдения не видно; если появятся — первый кандидат на выключение.
- Попутно закрыт кусок RQ-07: на железе mihomo `v1.19.27`, наш блок
  `rules` из `mixin.yaml` стоит **перед** правилами профиля ровно так, как
  описано в [`nikki-mixin.md`](nikki-mixin.md), провайдеры `nm-*`
  загружены (`RuleSet` срабатывает).
