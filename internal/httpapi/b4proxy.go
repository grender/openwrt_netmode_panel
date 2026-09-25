package httpapi

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"netmoded/internal/b4"
)

// Прокси для ПК на одном порту в обоих режимах (ADR-0048).
//
// В режиме nikki входящий прокси — mixed-port mihomo (HTTP и SOCKS5, логин
// из nikki.@authentication). В режиме b4 nikki остановлен, и порт молчит.
// У b4 есть свой SOCKS5-сервер; его исходящие соединения идут с роутера
// через цепочку output b4 и обходят DPI так же, как трафик LAN. Здесь он
// настраивается зеркалом mixed-port: тот же порт, тот же логин — и
// клиентам (Mac через проброс VM, браузер на ПК) нечего перенастраивать,
// кроме того, что HTTP-прокси в режиме b4 нет.
//
// Порт и логин берутся из UCI nikki, а не из литералов: источник правды
// один, и правка mixed_port в nikki доезжает до b4 при следующем
// переключении.

// b4 после старта или перезапуска поднимает API не сразу: procd, запуск
// бинаря, чтение конфига. Ждём с запасом, но ограниченно — это хвост
// джоба переключения режима. Переменные, а не константы, — ради тестов
// с лежащим b4.
var (
	b4UpWait      = 10 * time.Second
	b4RestartWait = 15 * time.Second
	b4PollEvery   = 500 * time.Millisecond
)

// syncB4Proxy приводит SOCKS5 b4 к mixed-port nikki. Идемпотентна: если
// настройки уже совпадают, b4 не перезапускается.
//
// Ошибка НЕ валит переключение режима: b4 для LAN работает и без SOCKS5,
// а прокси для ПК — удобство. Вызывающий пишет её в журнал.
func (s *Server) syncB4Proxy(ctx context.Context) error {
	if s.b4 == nil {
		return nil
	}
	want, ok, err := s.nikkiProxyWanted(ctx)
	if err != nil {
		return err
	}
	if !ok {
		s.logf("b4 socks5: у nikki не задан mixed_port — зеркалить нечего")
		return nil
	}

	if err := s.waitB4(ctx, b4UpWait); err != nil {
		return fmt.Errorf("b4 не поднялся: %w", err)
	}
	cur, err := s.b4.Socks5Config(ctx)
	if err != nil {
		return fmt.Errorf("чтение настроек: %w", err)
	}
	next := cur
	next.Enabled = true
	next.Port = want.Port
	// Все адреса: к порту приходят и с LAN, и из проброса VM (адрес
	// роутера в LAN). mixed-port nikki слушает так же.
	next.BindAddress = "0.0.0.0"
	next.Username = want.Username
	next.Password = want.Password
	if next == cur {
		return nil
	}

	if err := s.b4.SetSocks5Config(ctx, next); err != nil {
		return fmt.Errorf("запись настроек: %w", err)
	}
	if err := s.b4.Restart(ctx); err != nil {
		return fmt.Errorf("перезапуск: %w", err)
	}
	// Ответ на restart пришёл от СТАРОГО процесса, а свой перезапуск он
	// делает через полсекунды. Сразу спросить Version — получить ответ от
	// него же и решить, что всё готово.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Second):
	}
	if err := s.waitB4(ctx, b4RestartWait); err != nil {
		return fmt.Errorf("b4 не вернулся после перезапуска: %w", err)
	}
	s.logf("b4 socks5: порт %d, логин %s — как у mixed-port nikki", next.Port, loginNote(next.Username))
	return nil
}

// nikkiProxyWanted читает порт и логин mixed-port nikki. ok=false — порта
// нет: nikki не настроен, и зеркалить нечего.
func (s *Server) nikkiProxyWanted(ctx context.Context) (b4.Socks5Config, bool, error) {
	portStr, err := s.ex.UCIGet(ctx, "nikki", "mixin", "mixed_port")
	if err != nil || portStr == "" {
		return b4.Socks5Config{}, false, nil
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return b4.Socks5Config{}, false, fmt.Errorf("nikki.mixin.mixed_port=%q — не порт", portStr)
	}
	want := b4.Socks5Config{Port: port}

	// Логин только если он включён у nikki: иначе b4 требовал бы пароль,
	// которого клиенты в режиме nikki не вводили. У b4 логин и пароль —
	// оба или ни одного (socks5.go:64-67).
	if on, _ := s.ex.UCIGet(ctx, "nikki", "mixin", "authentication"); on == "1" {
		user, _ := s.ex.UCIGet(ctx, "nikki", "@authentication[0]", "username")
		pass, _ := s.ex.UCIGet(ctx, "nikki", "@authentication[0]", "password")
		if user != "" && pass != "" {
			want.Username, want.Password = user, pass
		}
	}
	return want, true, nil
}

// waitB4 ждёт, пока API b4 ответит на Version.
func (s *Server) waitB4(ctx context.Context, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		_, err := s.b4.Version(ctx)
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(b4PollEvery):
		}
	}
}

func loginNote(user string) string {
	if user == "" {
		return "не нужен"
	}
	return user
}
