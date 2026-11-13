# Свои выходные ноды: группы `EXIT-*` в профиле nikki

Как подключить свою выходную ноду (VLESS + Reality) и направлять в неё
отдельные наборы geosite и свои правила. Решение и его цена —
[ADR-0049](adr/0049-exit-groups-as-set-targets.md), формат файла —
[контракт миксина](contracts/nikki-mixin-rulesets.md).

**Демон профиль не пишет** (ADR-0002): ноды и группы добавляете вы, один раз.
Панель только предлагает в цели набора живые группы с именем `EXIT-…` и пишет
правила `RULE-SET,nm-geosite-<имя>,EXIT-…` в `mixin.yaml`.

## Раскладка

Пример — нода `kz-nqz-exit-01` с двумя выходами (выход выбирается по UUID):

| Прокси | Выход | Группа | Для чего |
|---|---|---|---|
| `kz-nqz-exit-01-resi` | ISP-прокси, домашний IP | `EXIT-RESI` | только AI: `category-ai-!cn`, `openai`, `anthropic` — прокси медленный на новые соединения ([разведка](recon/exit-chains.md)) |
| `kz-nqz-exit-01-dc` | IP самого VPS | `EXIT-DC` | то, что режется по IP: Telegram, Instagram |

```yaml
proxies:                       # из exports/kz-nqz-exit-01.mihomo.yaml — с секретами
  - name: kz-nqz-exit-01-resi
    # …
  - name: kz-nqz-exit-01-dc
    # …

proxy-groups:
  # AUTO, BYPASS — как были.
  - name: PROXY
    type: select
    proxies: [AUTO, kz-nqz-exit-01-resi, kz-nqz-exit-01-dc]   # ручной выбор
    use: [sub]
  - name: EXIT-RESI            # нода легла — отказ, а не утечка через чужой IP
    type: select
    proxies: [kz-nqz-exit-01-resi, REJECT]
  - name: EXIT-DC
    type: select
    proxies: [kz-nqz-exit-01-dc, REJECT]
```

- **`select` + `REJECT` — fail closed.** `select` сам не переключается:
  упала нода — соединения рвутся. `REJECT` вторым участником — способ закрыть
  выход руками. Ноды других стран дописываются в ту же группу участниками.
- **В `AUTO` ноды не добавлять:** url-test смешал бы их с подпиской
  (ADR-0031, ADR-0044). В `PROXY` — можно: они встанут в конец списка узлов
  панели как обычные узлы. `REJECT` в `PROXY` не добавлять — панель показала
  бы его вечно мёртвым узлом.
- **Имя группы:** `EXIT-` и 1–32 знака из `A-Za-z0-9._-`. Другие имена панель
  в цели не предлагает.
- **`udp: false`** у ноды: UDP через выход не пройдёт. QUIC браузер переведёт
  на TCP; звонки Telegram через `EXIT-DC`, скорее всего, не заработают.

## Пошагово на роутере

Файлы на роутер — только `ssh … 'cat > …' < файл`: `scp` там не работает (в
dropbear нет sftp-сервера). Команды ниже ничего секретного не печатают.

**1. Найти профиль и сделать копию.**

```sh
ssh root@192.168.9.1
P=/etc/nikki/profiles/$(uci get nikki.config.profile | sed 's/^file://')
echo "$P"
cp "$P" "$P.bak-$(date +%F)"
```

**2. Доставить ноды** — с Mac, из соседнего репозитория:

```sh
ssh root@192.168.9.1 'umask 077; cat > /tmp/kz.yaml' \
  < ~/sources/wall-breaker-exit-node/exports/kz-nqz-exit-01.mihomo.yaml
```

На роутере проверить форму, не печатая содержимого:

```sh
yq '.proxies | length' /tmp/kz.yaml          # ждём 2
yq '.proxies[].name' /tmp/kz.yaml            # имена, без секретов
```

**3. Вписать ноды и группы** (`yq` уже стоит — им же nikki склеивает конфиг):

```sh
yq -i '.proxies = ((.proxies // []) + load("/tmp/kz.yaml").proxies)' "$P"
yq -i '.proxy-groups += [
  {"name": "EXIT-RESI", "type": "select", "proxies": ["kz-nqz-exit-01-resi", "REJECT"]},
  {"name": "EXIT-DC",   "type": "select", "proxies": ["kz-nqz-exit-01-dc", "REJECT"]}
]' "$P"
yq -i '(.proxy-groups[] | select(.name == "PROXY") | .proxies) += ["kz-nqz-exit-01-resi", "kz-nqz-exit-01-dc"]' "$P"
rm /tmp/kz.yaml
```

`yq -i` переписывает файл своим форматированием; комментарии обычно
переживает. Если профиль правите руками — то же самое через `vi "$P"`.

`netmode-apply` лежит в `/usr/local/bin` — в `PATH` сессии ssh его может не
быть, поэтому ниже путь полный.

**4. Проверить разбор и применить.**

```sh
mihomo -t -d /etc/nikki/run -f "$P"   # профиль сам по себе; провайдеры не качает
/usr/local/bin/netmode-apply nikki
```

**5. Убедиться, что движок видит группы.**

```sh
S=$(uci get nikki.mixin.api_secret)
for g in EXIT-RESI EXIT-DC; do
  curl -s -H "Authorization: Bearer $S" http://127.0.0.1:9090/proxies/$g | jsonfilter -e '@.now'
done
```

Ждём `kz-nqz-exit-01-resi` и `kz-nqz-exit-01-dc`.

**6. Направить наборы в панели.** «Узлы Nikki» → «Что в туннель · наборы» →
у набора выбрать «Выход…» → `EXIT-RESI` или `EXIT-DC` → «Применить».

**Порядок решает, кто победит.** Наборы проверяются сверху вниз, и домен из
двух наборов уходит по верхнему: `geosite:google` включает `youtube`, и при
`google` → `EXIT-RESI` выше `youtube` → туннель YouTube уйдёт в домашний
выход. «Изменить порядок» в панели переставляет наборы. Свои правила всегда
стоят раньше наборов.

**7. Проверка выхода.** Набор `category-ip-geo-detect` (в нём `ipinfo.io`)
направить в `EXIT-RESI`, применить, с домашней машины:

```sh
curl -s https://ipinfo.io/json      # ждём IP и org домашнего провайдера в KZ
```

Затем перевести тот же набор в `EXIT-DC` — должен показаться IP VPS. После
снятия набора — прежний адрес туннеля или прямой, как было.

## Откат и восстановление

Вернуть профиль из копии:

```sh
cp "$P.bak-<дата>" "$P" && /usr/local/bin/netmode-apply nikki
```

**Перед удалением группы из профиля снимите её с наборов в панели.**
Профиль правится мимо демона, и правило на исчезнувшую группу не даст mihomo
подняться. Проверено на VM 13.11.2026 (mihomo 1.19.27): `netmode-apply`
выходит кодом 6 («nikki должен работать, но не работает»), в
`/var/log/nikki/core.log` —
`rules[…] [RULE-SET,nm-geosite-telegram,EXIT-DC] error: proxy [EXIT-DC] not found`. Если это уже случилось — выключить склейку (правила панели уйдут
вместе с файлом, профиль останется рабочим), поправить, включить обратно:

```sh
uci set nikki.mixin.mixin_file_content=0 && uci commit nikki && /usr/local/bin/netmode-apply nikki
# … починить профиль или вернуть группу …
uci set nikki.mixin.mixin_file_content=1 && uci commit nikki && /usr/local/bin/netmode-apply nikki
```
