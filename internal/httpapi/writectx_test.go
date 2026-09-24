package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// Оборванный клиентом запрос не обрывает начатую цепочку записи в UCI:
// иначе set проходил бы, commit — нет, и в стейджинге повисал бы секретный
// адрес подписки (а у сетей — полусекция без disabled=1).
func TestSubscriptionPutSurvivesClientCancel(t *testing.T) {
	s, f := newServer(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("PUT", "/api/subscription",
		strings.NewReader(`{"url":"https://example.invalid/sub?token=1"}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+testToken)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("код %d: %s", rec.Code, rec.Body.String())
	}
	if !slices.Contains(f.Calls, "commit netmode") {
		t.Errorf("запись не доведена до commit: %v", f.Calls)
	}
}
