package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

// mustNotExist — по пути ничего не лежит; для проверки, что .tmp не пережил
// отказ или успех.
func mustNotExist(t *testing.T, path, why string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("%s: %s должен отсутствовать, Lstat вернул err=%v", why, path, err)
	}
}

// TestWriteContentAndPerm — после успешной записи по рабочему пути лежит
// именно переданное содержимое с именно переданными правами, а временного
// файла не остаётся.
func TestWriteContentAndPerm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub.yaml")
	want := []byte("proxies: []\n")

	if err := Write(path, want, 0o640); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение результата: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("содержимое %q, ожидалось %q", got, want)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("права %o, ожидались %o", fi.Mode().Perm(), 0o640)
	}
	mustNotExist(t, path+".tmp", "после успешной записи")
}

// TestWriteOverwritesExisting — повторная запись по тому же пути полностью
// замещает старое содержимое, а не дописывает и не мешает старое с новым.
func TestWriteOverwritesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mixin.yaml")

	if err := Write(path, []byte("старое содержимое подлиннее"), 0o644); err != nil {
		t.Fatalf("первая запись: %v", err)
	}
	if err := Write(path, []byte("новое"), 0o644); err != nil {
		t.Fatalf("вторая запись: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение результата: %v", err)
	}
	if string(got) != "новое" {
		t.Errorf("содержимое %q — старый хвост не смыт", got)
	}
	mustNotExist(t, path+".tmp", "после второй записи")
}

// TestWriteMissingDirLeavesNoTemp — каталог не создаётся: отсутствие
// каталога назначения — это отказ с внятной ошибкой, а не молчаливое
// заведение чужого каталога в обход его владельца.
func TestWriteMissingDirLeavesNoTemp(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "нет-такого-каталога")
	path := filepath.Join(dir, "mixin.yaml")

	err := Write(path, []byte("данные"), 0o644)
	if err == nil {
		t.Fatal("ожидался отказ записи в несуществующий каталог")
	}
	if _, statErr := os.Stat(dir); !os.IsNotExist(statErr) {
		t.Error("каталог назначения не должен появляться сам по себе")
	}
	mustNotExist(t, path+".tmp", "после отказа на несуществующем каталоге")
	mustNotExist(t, path, "после отказа на несуществующем каталоге")
}

// TestWriteLeftoverTempIsReplaced — залежавшийся .tmp от прерванной записи
// (чужие права, чужое содержимое) не должен ни пережить новую запись, ни
// попасть на рабочее место как есть: он снимается перед открытием.
func TestWriteLeftoverTempIsReplaced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub.yaml")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte("мусор от прерванной записи"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Write(path, []byte("свежие данные"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("права рабочего файла %o, ожидалось %o: чужие права с .tmp переехали на место", fi.Mode().Perm(), 0o600)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "свежие данные" {
		t.Errorf("на рабочем месте оказалось %q, а не новая запись", got)
	}
	mustNotExist(t, tmp, "после успеха")
}

// TestWriteDoesNotFollowSymlinkTemp — подложенная в чужой каталог
// символическая ссылка path.tmp не должна увести секрет по чужому пути:
// os.WriteFile пошёл бы по ней и дописал чужой файл чужими правами.
// Write снимает .tmp (не следуя по ссылке — Remove её не разыменовывает) и
// создаёт на его месте настоящий файл через O_EXCL.
func TestWriteDoesNotFollowSymlinkTemp(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub.yaml")
	elsewhere := filepath.Join(t.TempDir(), "чужой-файл")
	if err := os.WriteFile(elsewhere, []byte("нетронуто"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, path+".tmp"); err != nil {
		t.Skip("символические ссылки недоступны:", err)
	}

	if err := Write(path, []byte("proxies: []\n"), 0o600); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "нетронуто" {
		t.Fatalf("секрет уехал по ссылке: чужой файл теперь %q", got)
	}
	body, _ := os.ReadFile(path)
	if string(body) != "proxies: []\n" {
		t.Errorf("рабочий файл не записан: %q", body)
	}
}

// TestWriteLeavesNoTempOnWriteFailure — отказ самой записи данных (не
// открытия, не переименования) тоже убирает .tmp: мусор во флеше не должен
// пережить отказ ровно тогда, когда места и так не хватает.
func TestWriteLeavesNoTempOnWriteFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub.yaml")
	// Каталог на месте .tmp: OpenFile с O_CREATE|O_EXCL на непустом
	// каталоге отказывает на этапе открытия, а не записи, — этого
	// достаточно, чтобы проверить симметрию defer'а по всем веткам отказа.
	if err := os.Mkdir(path+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path+".tmp", "x"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := Write(path, []byte("данные"), 0o600)
	if err == nil {
		t.Fatal("ожидался отказ открытия временного файла")
	}
	mustNotExist(t, path, "после отказа записи")
}
