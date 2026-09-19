package subs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"netmoded/internal/happ"
)

// assertUnchanged — файл побайтово прежний. «Функция вернула ошибку» и
// «файлы не тронуты» — разные утверждения, и рунбук обещает второе.
func assertUnchanged(t *testing.T, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", filepath.Base(path), err)
	}
	if string(got) != want {
		t.Errorf("%s изменился: было %q, стало %q", filepath.Base(path), want, string(got))
	}
}

// prefill кладёт «вчерашние» файлы на место.
func prefill(t *testing.T, h *harness) (oldProvider, oldManifest string) {
	t.Helper()
	oldProvider, oldManifest = `{"proxies":[{"name":"старый"}]}`+"\n", `[{"name":"старый","kind":"node"}]`+"\n"
	if err := os.WriteFile(h.provider, []byte(oldProvider), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.manifest, []byte(oldManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	return oldProvider, oldManifest
}

// TestUpdateWritesProviderBeforeManifest — порядок записи закреплён по
// последствиям частичного отказа (см. комментарий в Update). Мутация «поменять
// два writeAtomic местами» без этого теста выживала.
func TestUpdateWritesProviderBeforeManifest(t *testing.T) {
	t.Run("манифест не записался — провайдер уже новый", func(t *testing.T) {
		h := newHarness(t, oneNode, nil)
		h.up.ManifestPath = filepath.Join(t.TempDir(), "нет-такого-каталога", "subscription.json")

		_, err := h.up.Update(context.Background())
		if err == nil || !strings.Contains(err.Error(), "манифест") {
			t.Fatalf("ожидался отказ на манифесте, получено %v", err)
		}
		body, rerr := os.ReadFile(h.provider)
		if rerr != nil || !strings.Contains(string(body), "203.0.113.10") {
			t.Fatalf("файл провайдера должен быть записан ДО манифеста: %v %q", rerr, body)
		}
		if h.reloads != 0 {
			t.Errorf("движок дёрнули при незаписанном манифесте: %d", h.reloads)
		}
	})
	t.Run("провайдер не записался — манифеста нет вовсе", func(t *testing.T) {
		h := newHarness(t, oneNode, nil)
		h.up.ProviderPath = filepath.Join(t.TempDir(), "нет-такого-каталога", "sub.yaml")

		_, err := h.up.Update(context.Background())
		if err == nil || !strings.Contains(err.Error(), "провайдера") {
			t.Fatalf("ожидался отказ на файле провайдера, получено %v", err)
		}
		mustNotExist(t, h.manifest, "после отказа провайдера")
		if h.reloads != 0 {
			t.Errorf("движок дёрнули при незаписанных файлах: %d", h.reloads)
		}
	})
}

// TestUpdateKeepsFilesOnParseError — самый вероятный отказ на живом роутере:
// доступ протух, и провайдер отвечает объектом с error или страницей входа.
// Файлы обязаны остаться побайтово прежними, движок — нетронутым, а класс
// ошибки — различимым сквозь обёртку.
func TestUpdateKeepsFilesOnParseError(t *testing.T) {
	cases := []struct {
		name string
		body string
		want error
	}{
		{"обрывок", `[{"remarks":`, happ.ErrBadJSON},
		{"объект с error", `{"error":"subscription expired"}`, happ.ErrNotArray},
		{"строка", `"nope"`, happ.ErrNotArray},
		{"пустой массив", `[]`, happ.ErrEmpty},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, c.body, nil)
			oldP, oldM := prefill(t, h)

			_, err := h.up.Update(context.Background())
			if err == nil {
				t.Fatal("ожидался отказ разбора")
			}
			if !errors.Is(err, c.want) {
				t.Errorf("класс ошибки потерян сквозь обёртку: %v", err)
			}
			if strings.Contains(err.Error(), "secret-id") {
				t.Errorf("адрес подписки в тексте ошибки: %v", err)
			}
			assertUnchanged(t, h.provider, oldP)
			assertUnchanged(t, h.manifest, oldM)
			if h.reloads != 0 {
				t.Errorf("движок дёрнули при отказе разбора: %d", h.reloads)
			}
		})
	}
}

// TestUpdateKeepsFilesOnFetchErrorOverExisting — отказ скачивания поверх
// работающей установки: прежде проверялся только путь свежей установки.
func TestUpdateKeepsFilesOnFetchErrorOverExisting(t *testing.T) {
	h := newHarness(t, "", errors.New("сеть лежит"))
	oldP, oldM := prefill(t, h)
	if _, err := h.up.Update(context.Background()); err == nil {
		t.Fatal("ожидался отказ скачивания")
	}
	assertUnchanged(t, h.provider, oldP)
	assertUnchanged(t, h.manifest, oldM)
	if h.reloads != 0 {
		t.Errorf("движок дёрнули при отказе скачивания: %d", h.reloads)
	}
}

// TestUpdateReloadFailureLeavesNewContent — «файлы записаны» в тексте
// ошибки обязано быть правдой о СОДЕРЖИМОМ, а не о существовании файлов.
func TestUpdateReloadFailureLeavesNewContent(t *testing.T) {
	h := newHarness(t, oneNode, nil)
	prefill(t, h)
	h.up.Reload = func(context.Context) error { return errors.New("движок отверг") }

	_, err := h.up.Update(context.Background())
	if err == nil || !strings.Contains(err.Error(), "записаны") {
		t.Fatalf("ожидался отказ с оговоркой о записанных файлах: %v", err)
	}
	p, _ := os.ReadFile(h.provider)
	if !strings.Contains(string(p), "203.0.113.10") {
		t.Errorf("файл провайдера остался старым: %q", p)
	}
	m, _ := os.ReadFile(h.manifest)
	if !strings.Contains(string(m), "Браво") {
		t.Errorf("манифест остался старым: %q", m)
	}
}

// engineFrom — «честный движок»: имена из файла провайдера, который только
// что записали. С ним обновление обязано пройти; подмена этого замыкания
// моделирует движок, читающий другой файл или переписывающий имена.
func engineFrom(path string) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var f struct {
			Proxies []struct {
				Name string `json:"name"`
			} `json:"proxies"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, err
		}
		var names []string
		for _, p := range f.Proxies {
			names = append(names, p.Name)
		}
		return names, nil
	}
}

// TestUpdateVerifiesEngineList — 204 от движка не значит «прочитал наш
// файл»: fetcher молча выходит на совпавшем хэше, override переписывает
// имена. Успех обновления измеряется списком движка, а не нашим разбором.
func TestUpdateVerifiesEngineList(t *testing.T) {
	t.Run("движок показал всё — успех", func(t *testing.T) {
		h := newHarness(t, oneNode, nil)
		h.up.Proxies = engineFrom(h.provider)
		if _, err := h.up.Update(context.Background()); err != nil {
			t.Fatalf("честный движок не должен давать отказ: %v", err)
		}
	})

	t.Run("движок показал чужие имена — отказ с обеими тройками", func(t *testing.T) {
		h := newHarness(t, oneNode, nil)
		h.up.Proxies = func(context.Context) ([]string, error) {
			return []string{"🇧🇧⚡Браво 1", "🇨🇻⚡Чарли 2"}, nil
		}
		_, err := h.up.Update(context.Background())
		if !errors.Is(err, ErrProviderIgnored) {
			t.Fatalf("ожидался ErrProviderIgnored, получено %v", err)
		}
		for _, want := range []string{"204", "🇧🇧⚡Браво 1", "🇧🇧 Браво", "path", "override"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("в тексте %q нет %q", err.Error(), want)
			}
		}
		if h.reloads != 1 {
			t.Errorf("PUT должен был пройти ровно раз: %d", h.reloads)
		}
		body, _ := os.ReadFile(h.provider)
		if !strings.Contains(string(body), "203.0.113.10") {
			t.Error("файлы обязаны остаться новыми — отказ не про запись")
		}
	})

	t.Run("движок показал часть — не отказ, а строка в журнал", func(t *testing.T) {
		h := newHarness(t, oneNode, nil)
		var logged []string
		h.up.Logf = func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
		h.up.Proxies = func(context.Context) ([]string, error) {
			// Один записанный узел есть, и ещё чужой — частичное совпадение.
			return []string{"🇧🇧 Браво", "чужой"}, nil
		}
		if _, err := h.up.Update(context.Background()); err != nil {
			t.Fatalf("частичное совпадение — не отказ: %v", err)
		}
		// В oneNode один узел, и он найден: расхождения нет, журнал молчит.
		if len(logged) != 0 {
			t.Errorf("журнал не должен получить запись при полном совпадении: %v", logged)
		}
	})

	t.Run("запрос списка не удался — отказ с оговоркой", func(t *testing.T) {
		h := newHarness(t, oneNode, nil)
		h.up.Proxies = func(context.Context) ([]string, error) { return nil, errors.New("движок молчит") }
		_, err := h.up.Update(context.Background())
		if err == nil {
			t.Fatal("ожидался отказ")
		}
		for _, want := range []string{"записаны", "перечитал", "проверить"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("в тексте %q нет %q", err.Error(), want)
			}
		}
		if errors.Is(err, ErrProviderIgnored) {
			t.Error("недоступный список — не то же, что список без наших узлов")
		}
	})

	t.Run("Proxies не задан — сверки нет", func(t *testing.T) {
		h := newHarness(t, oneNode, nil)
		h.up.Proxies = nil
		if _, err := h.up.Update(context.Background()); err != nil {
			t.Fatalf("без сверки обновление проходит как раньше: %v", err)
		}
	})
}

// TestUpdatePartialEngineListIsLogged — два узла записаны, один пропал:
// не отказ, но строка в журнале с именем пропавшего.
func TestUpdatePartialEngineListIsLogged(t *testing.T) {
	two := strings.Replace(oneNode, `"remarks": "🇧🇧 Браво"`, `"remarks": "🇧🇧 Браво"`, 1)
	two = "[" + strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(two), "["), "]") + "," +
		strings.Replace(strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(oneNode), "["), "]"),
			`"remarks": "🇧🇧 Браво"`, `"remarks": "🇫🇯 Фокстрот"`, 1) + "]"
	h := newHarness(t, two, nil)
	var logged []string
	h.up.Logf = func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
	h.up.Proxies = func(context.Context) ([]string, error) { return []string{"🇧🇧 Браво"}, nil }

	if _, err := h.up.Update(context.Background()); err != nil {
		t.Fatalf("частичное совпадение — не отказ: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "🇫🇯 Фокстрот") || !strings.Contains(logged[0], "1 узлов из 2") {
		t.Fatalf("ожидалась одна запись с пропавшим узлом и счётом: %v", logged)
	}
}
