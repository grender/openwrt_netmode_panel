package httpapi

import (
	"context"

	"netmoded/internal/b4"
)

// fakeB4Client — b4 в тестах HTTP-слоя.
//
// Без него тесты стучались бы в 127.0.0.1:7000 на машине разработчика:
// на ноуте там никого нет, но зависимость от чужого порта делает прогон
// невоспроизводимым — а однажды и медленным, если порт кто-то займёт.
type fakeB4Client struct {
	sets []b4.Set
	err  error
	// selected запоминает последний выбор, чтобы проверить эксклюзивность.
	selected string
}

func newFakeB4Client() *fakeB4Client {
	return &fakeB4Client{sets: []b4.Set{
		{ID: "909a6fb1-b9b1-4af1-8ee0-bdce82e3d8ff", Name: "workki", Enabled: false},
		{ID: "d98efbc7-96b6-401c-90c4-744f3ec3ccbd", Name: "HomeSet", Enabled: true},
	}}
}

func (f *fakeB4Client) Version(context.Context) (b4.Version, error) {
	if f.err != nil {
		return b4.Version{}, f.err
	}
	return b4.Version{Version: "1.74.1"}, nil
}

func (f *fakeB4Client) Sets(context.Context) ([]b4.Set, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.sets, nil
}

func (f *fakeB4Client) SelectOnly(_ context.Context, id string) error {
	if f.err != nil {
		return f.err
	}
	found := false
	for i := range f.sets {
		on := f.sets[i].ID == id
		f.sets[i].Enabled = on
		if on {
			found = true
			f.selected = f.sets[i].Name
		}
	}
	if !found {
		return b4.ErrNotFound
	}
	return nil
}
