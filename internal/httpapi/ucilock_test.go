package httpapi

import (
	"net/http"
	"testing"
	"time"
)

// Синхронная запись сети ждёт замка пакета wireless: пока джоб аплинка
// держит его от сверки до коммита, в стейджинг не попадает ни одной
// строки, и его commit не публикует чужую полусекцию.
func TestWifiWriteWaitsForWirelessLock(t *testing.T) {
	s, _ := newServer(t)
	fp := etag(t, s)

	unlock := s.lockPkg("wireless")
	done := make(chan int, 1)
	go func() {
		done <- post(t, s, "/api/wifi/networks", `{"ssid":"Дача","encryption":"psk2","key":"password123"}`, fp).Code
	}()

	select {
	case code := <-done:
		unlock()
		t.Fatalf("запись прошла мимо замка (код %d)", code)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()

	select {
	case code := <-done:
		if code != http.StatusOK && code != http.StatusCreated {
			t.Errorf("после снятия замка запись вернула %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("запись не завершилась после снятия замка")
	}
}
