package watch

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

// rawPath — фикстура разведки RQ-09. Тесты разборщика читают записанный
// вывод роутера: рукописный мок проверяет нашу догадку саму против себя, а
// записанный вывод ловит ошибки формы (docs/recon/README.md).
const rawPath = "../../docs/recon/raw/91-watch-logs-connections-hosts.txt"

// rawSection отдаёт непустые строки между «## <header>» и следующим
// заголовком. Фикстура одна и общая; резать её копиями значило бы завести
// второй источник правды о роутере.
func rawSection(t *testing.T, header string) []string {
	t.Helper()
	f, err := os.Open(rawPath)
	if err != nil {
		t.Fatalf("фикстура не открылась: %v", err)
	}
	defer f.Close()
	var out []string
	in := false
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 64<<10)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "## "):
			in = strings.Contains(line, header)
		case strings.HasPrefix(line, "#"):
			in = false
		case in && strings.TrimSpace(line) != "":
			out = append(out, line)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("фикстура не дочиталась: %v", err)
	}
	if len(out) == 0 {
		t.Fatalf("в фикстуре нет раздела %q", header)
	}
	return out
}
