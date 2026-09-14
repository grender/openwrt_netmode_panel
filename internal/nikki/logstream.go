package nikki

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Поток журнала Clash API.
//
// Снято с живого роутера: docs/recon/raw/91-watch-logs-connections-hosts.txt,
// разбор — docs/recon/nikki-watch.md, «GET /logs?level=info». Без заголовка
// Upgrade: websocket mihomo пишет события прямо в тело и делает Flush после
// каждого (hub/route/server.go:474-560), то есть обычный chunked-HTTP, и
// клиента WebSocket заводить не надо.
//
// Уровень потока не зависит от log-level в конфиге: на роутере стоит
// warning, а ?level=info отдаёт info. И отдаёт он НЕ только info: level —
// это пол, а не фильтр, и при level=info в теле идут и строки warning —
// именно ими и приходят неудавшиеся дозвоны (raw/91: 22 info + 11 warning).
// Читатель, оставляющий только type=="info", потерял бы весь вердикт
// «не отвечает».

const (
	// logsPath — путь Clash API. Литерал БЕЗ «?level=»: гейт
	// scripts/check-evidence.sh ищет в docs/recon/evidence.json дословную
	// подстроку, а там записан путь, а не запрос с параметром.
	logsPath = "/logs"

	// lineLimit — потолок строки журнала (ADR-0022, ADR-0043). Наблюдённый
	// максимум за 20 секунд — 161 символ.
	lineLimit = 4 << 10

	// logLevelDefault — уровень по умолчанию. debug за те же 3 секунды дал
	// 17 строк против 3: он добавляет [DNS], [Process] и [Rule], которые
	// наблюдателю не нужны.
	logLevelDefault = "info"
)

// logLevels — то, что принимает mihomo. Проверяем у себя, чтобы опечатка в
// вызывающем не уехала в чужой сервис параметром.
var logLevels = map[string]bool{
	"debug": true, "info": true, "warning": true, "error": true, "silent": true,
}

// LogLine — одно событие журнала.
//
// Payload уже прошёл json.Unmarshal, поэтому стрелка в нём — «-->», а не
// «-->»: разбирать её по сырому байту нельзя.
//
// Пустой Type означает, что строка не была JSON вовсе. Это не ошибка потока,
// а смена формата у движка, и считает её вызывающий — там же, где считаются
// строки, не поддавшиеся грамматике.
type LogLine struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
}

// LogStream — построчное чтение потока.
//
// Итератор, а не канал: канал потребовал бы горутины внутри этого пакета, а
// с ней — владения отменой и восстановлением после паники. И то и другое
// принадлежит наблюдателю, который крутит её под safe.Do (ADR-0021).
type LogStream struct {
	rc       io.ReadCloser
	br       *bufio.Reader
	line     LogLine
	err      error
	oversize int
}

// LogStream открывает поток журнала.
//
// Ошибкой заканчивается только УСТАНОВКА соединения. Дальше сроков нет, и
// это не небрежность: заголовки mihomo отдаёт только с первым событием
// (raw/91: curl -si -m 2 не получил ничего), а у тихого устройства события
// может не быть минутами. Останавливает поток отмена ctx и ничто иное.
func (c *HTTP) LogStream(ctx context.Context, level string) (*LogStream, error) {
	if level == "" {
		level = logLevelDefault
	}
	if !logLevels[level] {
		return nil, fmt.Errorf("nikki: неизвестный уровень журнала %q", level)
	}
	q := url.Values{"level": []string{level}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+logsPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("nikki: запрос журнала: %w", err)
	}
	if c.Secret != "" {
		req.Header.Set("Authorization", "Bearer "+c.Secret)
	}

	resp, err := c.streamClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		resp.Body.Close()
		return nil, fmt.Errorf("%w: секрет отвергнут", ErrUnavailable)
	case resp.StatusCode != http.StatusOK:
		msg := errMessage(resp.Body)
		resp.Body.Close()
		return nil, &StatusError{Code: resp.StatusCode, Path: logsPath, Message: msg}
	}
	return NewLogStreamFromReader(resp.Body), nil
}

// NewLogStreamFromReader — поток поверх готового тела.
//
// Существует ради подделок: LogStream — конкретный тип, и без этого шва
// всякая проверка сессии поднимала бы httptest.Server, то есть проверяла бы
// заодно и сеть.
func NewLogStreamFromReader(rc io.ReadCloser) *LogStream {
	return &LogStream{rc: rc, br: bufio.NewReaderSize(rc, lineLimit)}
}

// streamClient — отдельный клиент под поток.
//
// Клиент из New не годится дважды. Во-первых, его пул на два соединения
// (MaxIdleConns: 2) поток занял бы на часы. Во-вторых и главное: do
// выражает всякий срок через context.WithTimeout, а дедлайн контекста
// отменяет и ЧТЕНИЕ ТЕЛА — поток умирал бы через callTimeout на исправном
// роутере. Единственный срок здесь стоит на наборе номера.
func (c *HTTP) streamClient() *http.Client {
	c.streamOnce.Do(func() {
		c.stream = &http.Client{
			// Timeout НЕ ставится: он накрывает и чтение тела.
			Transport: &http.Transport{
				DialContext: (&net.Dialer{Timeout: probeTimeout}).DialContext,
				// Заголовки приходят только с первым событием, поэтому
				// любое ненулевое значение рвало бы поток у тихого
				// устройства.
				ResponseHeaderTimeout: 0,
				DisableKeepAlives:     true,
			},
		}
	})
	return c.stream
}

// Next читает следующую строку. Блокирует, пока она не придёт.
func (s *LogStream) Next() bool {
	for {
		raw, err := s.br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			// Слишком длинная строка ПРОПУСКАЕТСЯ, а не кончает поток.
			//
			// bufio.Scanner тут не годится вовсе: его ErrTooLong
			// терминальна, и одна длинная строка закрыла бы поток. Если бы
			// она ещё и повторялась, наблюдатель ушёл бы в вечный цикл
			// переподключений с паузами 1, 2, 5 с — а владелец прочитал бы
			// это как «движок перезапускается».
			if !s.drain() {
				return false
			}
			s.oversize++
			continue
		}
		if err != nil {
			if err != io.EOF {
				s.err = err
			}
			return false
		}
		line := strings.TrimSpace(string(raw))
		if line == "" {
			continue
		}
		var l LogLine
		if json.Unmarshal([]byte(line), &l) != nil {
			// Не JSON — это смена формата, а не порча потока. Отдаём как
			// есть: считать такие строки будет тот же счётчик, что и строки,
			// не поддавшиеся грамматике.
			s.line = LogLine{Payload: line}
			return true
		}
		s.line = l
		return true
	}
}

// drain дочитывает хвост слишком длинной строки. false — поток кончился.
func (s *LogStream) drain() bool {
	for {
		_, err := s.br.ReadSlice('\n')
		if err == bufio.ErrBufferFull {
			continue
		}
		if err != nil {
			if err != io.EOF {
				s.err = err
			}
			return false
		}
		return true
	}
}

// Line — последняя прочитанная строка.
func (s *LogStream) Line() LogLine { return s.line }

// Oversize — сколько строк пропущено по длине.
func (s *LogStream) Oversize() int { return s.oversize }

// Err — почему чтение кончилось. nil при чистом EOF и при отмене.
func (s *LogStream) Err() error { return s.err }

// Close закрывает тело.
func (s *LogStream) Close() error { return s.rc.Close() }
