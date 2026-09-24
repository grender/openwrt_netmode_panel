package httpapi

import (
	"os"
	"testing"

	"netmoded/internal/mixin"
)

// Повторная сверка внутри джоба ловит запись, случившуюся между проверкой
// If-Match в обработчике и самой записью.
func TestMixinStillCatchesChangeBetweenCheckAndWrite(t *testing.T) {
	s, _ := newServer(t)

	cur, _, herr := s.readMixin()
	seen := mixinState(cur, herr)
	if err := s.mixinStill(seen); err != nil {
		t.Fatalf("файл не менялся, а сверка отказала: %v", err)
	}

	next := cur
	next.Policy = mixin.PolicyDirect
	next.Sets = []mixin.Set{{Name: "youtube", Action: mixin.ActionTunnel}}
	if err := os.WriteFile(s.cfg.MixinPath, mixin.Render(next), 0o644); err != nil {
		t.Fatalf("подготовка: %v", err)
	}
	if err := s.mixinStill(seen); err == nil {
		t.Error("файл переписан между проверкой и записью, а сверка пропустила запись по старому снимку")
	}
}
