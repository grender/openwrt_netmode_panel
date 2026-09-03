package subs

import (
	"context"
	"errors"
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
	if !strings.Contains(string(m), "Германия") {
		t.Errorf("манифест остался старым: %q", m)
	}
}
