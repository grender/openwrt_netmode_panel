// Command netmoded-dev поднимает НАСТОЯЩИЕ обработчики демона против
// записанного вывода роутера (docs/recon/raw/).
//
// Отличие от web/mock-server.mjs принципиальное: там фикстуры отдаёт
// node, повторяя контракт руками, — а здесь работает тот же код, который
// поедет на роутер. Расхождение между «мок отвечает» и «демон отвечает»
// тут невозможно по построению.
//
// В бинарь для роутера этот код не попадает: отдельная команда, а не флаг.
//
//	go run ./cmd/netmoded-dev
package main

import (
	"context"
	"flag"
	"log"
	"os/signal"
	"syscall"

	"netmoded/internal/executor"
	"netmoded/internal/httpapi"
)

func main() {
	addr := flag.String("listen", "127.0.0.1", "адрес (LAN-адрес или loopback)")
	port := flag.Int("port", 8099, "порт")
	fixtures := flag.String("fixtures", "docs/recon/raw", "каталог с выводом роутера")
	token := flag.String("token", "dev-token-not-for-production-use", "токен API")
	flag.Parse()

	f := executor.NewFake()
	if err := f.LoadFixturesDir(*fixtures); err != nil {
		log.Fatalf("фикстуры: %v", err)
	}
	f.UCIValues["netmode.main.mode"] = "nikki"
	f.UCIValues["system.@system[0].hostname"] = "grenderRouter"

	srv, err := httpapi.NewServer(httpapi.Config{
		Listen: *addr, Port: *port, Token: *token,
	}, f)
	if err != nil {
		log.Fatalf("сервер: %v", err)
	}

	// Клиенты Nikki и b4 подменяются: на ноуте на 9090 и 7000 никого нет,
	// и без подмены панель показывала бы только «недоступно».
	srv.SetNikkiClient(newDevNikki())
	srv.SetB4Client(newDevB4())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("настоящие обработчики на http://%s/?token=%s", srv.Addr(), *token)
	log.Printf("фикстуры: %s", *fixtures)
	if err := srv.ListenAndServe(ctx); err != nil {
		log.Printf("остановлен: %v", err)
	}
}
