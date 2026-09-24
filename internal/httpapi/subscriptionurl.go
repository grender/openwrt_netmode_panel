package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
)

// netmodeConfigPath — файл, в котором uci хранит netmode.main.subscription_url.
//
// Нужен ровно для одного: довести права до 0600 после нашего commit. Путь
// зашит, а не настраивается, по той же причине, что и остальные пути демона:
// это факт системы, а не выбор владельца.
const netmodeConfigPath = "/etc/config/netmode"

// subURLStore — адрес подписки, который теперь можно сменить на ходу.
//
// До ADR-0034 адрес читался из UCI один раз при старте и лежал в Config
// неизменяемой строкой. Читателей у него три, и все три обязаны меняться
// вместе: гейт в handleSubscriptionUpdate, subs.Updater внутри расписания
// и признак Configured в статусе. Разъехавшись, они дают панели самый
// неприятный сорт вранья — кнопка «Обновить» рабочая, а нажатие отбивается
// кодом subscription_not_configured, или наоборот.
//
// Мьютекс, а не atomic.Value: расписание читает адрес из своей горутины,
// панель пишет из обработчика, и гонка здесь не гипотетическая.
type subURLStore struct {
	mu sync.RWMutex
	v  string
}

func (s *subURLStore) get() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.v
}

func (s *subURLStore) set(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.v = v
}

func (s *subURLStore) configured() bool { return s.get() != "" }

// maskSubURL — что от адреса подписки можно показать владельцу.
//
// ADR-0012 запрещает возвращать учётные данные, а status.go обещает прямым
// текстом: «сам адрес наружу не уходит НИКОГДА». Панели при этом нужно
// ответить на два вопроса — «задан ли адрес» и «какого провайдера», — и оба
// закрываются схемой, хостом и путём. Значения параметров не показываются
// НИ ОДНИМ символом: токен подписки может лежать в любом из них, а «хвостик
// из четырёх знаков» — это утечка четырёх знаков, а не мера защиты.
//
// Имена параметров сохраняются: по ним владелец узнаёт свой адрес, а секрета
// в них нет — секрет в значениях.
func maskSubURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	// Не разобралось — не показываем НИЧЕГО, кроме факта непустоты. Отдать
	// «как есть» здесь значило бы отдать секрет целиком ровно в том случае,
	// когда мы не поняли, где он лежит.
	if err != nil || u.Host == "" {
		return "адрес задан, но не разбирается как URL"
	}

	out := u.Scheme + "://" + u.Host + u.Path
	if u.RawQuery != "" {
		keys := make([]string, 0, 4)
		for k := range u.Query() {
			keys = append(keys, k+"=…")
		}
		// Порядок карты случаен, а ответ обязан быть стабильным: панель
		// опрашивает демона раз в секунду, и мигающая строка читалась бы
		// как «адрес меняется сам».
		sortStrings(keys)
		out += "?" + strings.Join(keys, "&")
	}
	if u.Fragment != "" {
		out += "#…"
	}
	return out
}

// sortStrings — вставками. Параметров в адресе подписки единицы, а тянуть
// sort ради четырёх строк незачем: пакет и так обходится без зависимостей.
func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

// handleSubscriptionGet отдаёт состояние подписки вместе с МАСКОЙ адреса.
//
// Отдельный маршрут, а не поле в /api/status, по той же причине, что и у
// GET /api/nikki/panel (ADR-0023): статус опрашивается раз в секунду, и всё,
// что в нём лежит, оседает в кэше браузера и в любом снимке панели. Маска
// секрета не содержит, но и ездить шестьдесят раз в минуту ей незачем.
func (s *Server) handleSubscriptionGet(w http.ResponseWriter, r *http.Request) {
	raw := s.subURL.get()
	writeJSON(w, http.StatusOK, map[string]any{
		"configured": raw != "",
		"masked":     maskSubURL(raw),
	})
}

// handleSubscriptionPut записывает новый адрес подписки.
//
// Пишет в UCI и коммитит — глаголами, которые у исполнителя уже есть
// (ADR-0015): своего глагола этой операции не нужно, она выражается
// UCISet + UCICommit.
func (s *Server) handleSubscriptionPut(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "Тело запроса не разбирается как JSON")
		return
	}

	// Пробелы по краям срезаются молча: адрес приезжает из буфера обмена,
	// и хвостовой перевод строки — не ошибка владельца, а свойство копирования.
	in.URL = strings.TrimSpace(in.URL)

	// Пустая строка — ВАЛИДНОЕ значение: так адрес стирают. Отдельная
	// проверка на пустоту тут была бы запретом отменить настройку.
	if in.URL != "" {
		u, err := url.Parse(in.URL)
		switch {
		case err != nil || u.Host == "":
			writeErr(w, http.StatusBadRequest, "bad_request",
				"Адрес не разбирается как URL")
			return
		case u.Scheme != "http" && u.Scheme != "https":
			// Схема проверяется здесь, а не в happ.Fetch: там отказ пришёл бы
			// джобом, то есть строкой «fail» в журнале обновлений, и опечатка
			// в схеме копила бы историю неудач вместо ответа в форме.
			writeErr(w, http.StatusBadRequest, "bad_request",
				"Адрес подписки обязан начинаться с http:// или https://")
			return
		}
	}

	// Отвязано от отмены запроса: оборванный между set и commit запрос
	// оставлял бы секретный адрес висеть в стейджинге (writeCtx).
	ctx, cancel := writeCtx(r)
	defer cancel()
	unlock := s.lockPkg("netmode")
	defer unlock()
	if err := s.ex.UCISet(ctx, "netmode", "main", "subscription_url", in.URL); err != nil {
		// Текст ошибки исполнителя наружу НЕ уходит: в неудачной команде
		// uci set есть само значение, то есть секрет (ADR-0012). По той же
		// причине его нет и в журнале демона.
		s.logf("подписка: запись адреса не удалась")
		writeErr(w, http.StatusInternalServerError, "write_failed",
			"Записать адрес не удалось")
		return
	}
	// commit_failed, а не write_failed, и разница не косметическая: запись
	// прошла, значит в конфиге остался НЕЗАКОММИЧЕННЫЙ адрес — он виден
	// в LuCI и уедет в систему при первом чужом коммите пакета netmode.
	// Один код на оба отказа отправил бы владельца искать неудавшуюся
	// запись, которой не было (docs/contracts/errors.md).
	if err := s.ex.UCICommit(ctx, "netmode"); err != nil {
		s.logf("подписка: commit netmode не удался, адрес остался в черновике")
		writeErr(w, http.StatusInternalServerError, "commit_failed",
			"Адрес записан, но не закоммичен. В конфигурации остался черновик — "+
				"снимите его на роутере: uci revert netmode")
		return
	}

	// Права доводятся ПОСЛЕ коммита, и это не перестраховка. uci commit
	// пишет через временный файл и переименование, то есть создаёт файл по
	// umask (0644 у root), а не наследует прежние 0600. deploy.sh доводит
	// права при каждой установке именно потому, что «в нём лежит
	// subscription_url» (scripts/deploy.sh) — но установка бывает раз в
	// месяц, а эта запись случается по нажатию кнопки.
	//
	// Отказ chmod не отменяет записи: адрес уже в конфиге, и делать вид,
	// что операция не состоялась, значило бы врать. Но и промолчать нельзя —
	// секрет лежит с мировидимыми правами.
	if err := os.Chmod(netmodeConfigPath, 0o600); err != nil && !os.IsNotExist(err) {
		s.logf("подписка: права на %s не доведены до 0600 — в нём секрет, проверьте руками",
			netmodeConfigPath)
	}

	// Живое значение меняется ПОСЛЕДНИМ: до этой строки демон работает по
	// старому адресу, и это правильный порядок. Обратный оставил бы окно,
	// в котором расписание качает по новому адресу, а в конфиге ещё старый —
	// то есть перезапуск демона молча откатил бы работающую настройку.
	s.subURL.set(in.URL)

	writeJSON(w, http.StatusOK, map[string]any{
		"configured": in.URL != "",
		"masked":     maskSubURL(in.URL),
	})
}
