// Command netmoded — демон управления сетевыми режимами роутера.
//
// Слушает только на LAN-адресе, требует токен, отдаёт API и панель
// из одного бинаря. Наружу не выставляется ни при каких условиях.
package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"netmoded/internal/executor"
	"netmoded/internal/httpapi"
)

var version = "dev"

const (
	defaultPort      = 8088 // 8080 занят mihomo (docs/recon/raw/25-netstat.txt)
	defaultTokenFile = "/etc/netmoded/token"
)

func main() {
	showVersion := flag.Bool("version", false, "напечатать версию и выйти")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		return
	}

	if err := run(); err != nil {
		log.Fatalf("netmoded: %v", err)
	}
}

func run() error {
	ex := executor.New()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	cfg, err := loadConfig(ctx, ex)
	if err != nil {
		return err
	}

	cfg.Logf = log.Printf
	srv, err := httpapi.NewServer(cfg, ex)
	if err != nil {
		return err
	}
	defer srv.Close()

	// Расписание обновления подписки. Конвертер теперь ВНУТРИ демона:
	// подписку он скачивает и раскладывает сам, скрипта happ2clash на
	// роутере больше нет (его удаляет scripts/deploy.sh --install).
	//
	// Строку happ2clash из /etc/crontabs/root всё равно надо убрать руками —
	// чужой crontab демон не правит (SPEC §9). Причина теперь другая:
	// не «конвертер дёргается дважды», а cron раз в N часов зовёт
	// несуществующий файл и молча получает 127. Писем об этом не будет:
	// MTA в образе OpenWrt нет, а busybox crond собран без вызова
	// sendmail — отказ виден только штатной строкой crond в logread, то
	// есть практически не виден. Строка бесполезна и вводит в заблуждение
	// при следующем разборе.
	//
	// Без адреса подписки расписание НЕ ЗАПУСКАЕТСЯ вовсе, и это не
	// экономия таймера. Крутящийся цикл каждые двенадцать часов писал бы в
	// журнал обновлений отказ «адрес не задан» — то есть превращал бы
	// нормальное состояние свежей установки в историю неудач и приучал не
	// читать этот журнал. Одна строка при старте говорит ровно столько же
	// и ровно один раз.
	//
	// Кнопка «обновить сейчас» при этом остаётся рабочей: планировщик
	// собран, RunOnce доступен, и на нажатие панель получит внятное
	// subscription_not_configured вместо тишины.
	if cfg.SubscriptionURL != "" {
		go srv.Scheduler().Run(ctx)
	} else {
		log.Printf("расписание обновления подписки не запущено: " +
			"netmode.main.subscription_url не задан; " +
			"список узлов будет показан таким, каким его отдаёт mihomo")
	}

	log.Printf("netmoded %s слушает http://%s", version, srv.Addr())
	if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}

// loadConfig читает /etc/config/netmode.
//
// Отсутствие файла — валидное состояние свежей установки, а не ошибка.
// Единственное, без чего демон не стартует, — адрес прослушивания:
// отката на «слушать везде» нет (ADR-0014).
func loadConfig(ctx context.Context, ex executor.Executor) (httpapi.Config, error) {
	get := func(opt, fallback string) string {
		v, err := ex.UCIGet(ctx, "netmode", "main", opt)
		if err != nil || v == "" {
			return fallback
		}
		return v
	}

	listen := get("listen", "")
	if listen == "" {
		// Выводим из LAN-адреса роутера, отбрасывая маску.
		v, err := ex.UCIGet(ctx, "network", "lan", "ipaddr")
		if err != nil {
			return httpapi.Config{}, fmt.Errorf(
				"адрес прослушивания не задан и не выводится из network.lan.ipaddr: %w", err)
		}
		listen = strings.SplitN(v, "/", 2)[0]
	}

	port := defaultPort
	if v := get("port", ""); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return httpapi.Config{}, fmt.Errorf("порт %q не число", v)
		}
		port = n
	}

	token, err := loadOrCreateToken(get("token_file", defaultTokenFile))
	if err != nil {
		return httpapi.Config{}, err
	}

	// Адрес подписки. Пусто — валидное состояние, а не ошибка старта: демон
	// работает, расписание молчит, панель показывает подсказку.
	//
	// ОСТОРОЖНО, СЕКРЕТ. В строке лежит идентификатор подписки, поэтому её
	// нельзя печатать НИГДЕ: ни в log.Printf при старте (журнал демона
	// читает logread, а его видно из ssh), ни в тексте ошибки — текст ошибки
	// уходит владельцу в терминал и в джобы. Отсюда и нет проверки вида
	// «адрес не разбирается как URL»: она потребовала бы вписать значение в
	// сообщение, чтобы быть полезной. Разбор и все жалобы на него — в
	// internal, где ответ уже умеет не показывать секрет (ADR-0012).
	subURL := get("subscription_url", "")

	return httpapi.Config{
		Listen:          listen,
		Port:            port,
		Token:           token,
		NikkiURL:        nikkiURL(ctx, ex),
		NikkiSecret:     get2(ctx, ex, "nikki", "mixin", "api_secret"),
		SubscriptionURL: subURL,
	}, nil
}

// nikkiURL выводит адрес Clash API из nikki.mixin.api_listen.
//
// Значение вида "[::]:9090" означает «слушает везде»; ходить туда надо
// по петле, а не по literal-адресу из конфига.
func nikkiURL(ctx context.Context, ex executor.Executor) string {
	listen := get2(ctx, ex, "nikki", "mixin", "api_listen")
	if listen == "" {
		return "http://127.0.0.1:9090"
	}
	i := strings.LastIndex(listen, ":")
	if i < 0 {
		return "http://127.0.0.1:9090"
	}
	return "http://127.0.0.1:" + listen[i+1:]
}

func get2(ctx context.Context, ex executor.Executor, pkg, section, opt string) string {
	v, err := ex.UCIGet(ctx, pkg, section, opt)
	if err != nil {
		return ""
	}
	return v
}

// loadOrCreateToken читает токен, создавая его при первом запуске.
//
// Существующий не перезаписывается никогда: ротация — это осознанное
// `rm` плюс перезапуск, а не побочный эффект старта демона.
//
// Файл, а не UCI: /etc/config/wireless имеет права 644
// (docs/recon/raw/40-etc-config-perms.txt), то есть мировидимость в этом
// каталоге зависит от пакета, а не от нашей политики.
func loadOrCreateToken(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err == nil {
		tok := strings.TrimSpace(string(b))
		if tok == "" {
			return "", fmt.Errorf("%s пуст: удалите его, токен будет создан заново", path)
		}
		return tok, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("чтение токена %s: %w", path, err)
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("генерация токена: %w", err)
	}
	tok := base64.RawURLEncoding.EncodeToString(raw)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("каталог токена: %w", err)
	}
	if err := os.WriteFile(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("запись токена: %w", err)
	}
	log.Printf("создан токен %s (права 0600)", path)
	return tok, nil
}
