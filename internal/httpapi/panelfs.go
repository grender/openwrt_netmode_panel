package httpapi

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// panelFile — один файл панели, приготовленный к отдаче заранее.
type panelFile struct {
	raw   []byte
	gz    []byte
	ctype string
	etag  string
	// immutable — файл, адрес которого несёт версию (assets/*?v=…), и потому
	// его содержимое по этому адресу измениться не может.
	immutable bool
}

// panelStore — вся панель в памяти, сжатая один раз при старте.
//
// СЖАТИЕ ПРИ СТАРТЕ, А НЕ ПРИ СБОРКЕ. Предсжатые файлы рядом с исходными
// пришлось бы коммитить (артефакт панели лежит в git — ADR-0036), то есть
// класть в историю бинарные блобы и удваивать каталог. Панель весит
// десятки килобайт, и сжать её на старте — это единицы миллисекунд один
// раз за жизнь процесса.
//
// И НЕ ПРИ ЗАПРОСЕ. Сжатие на лету стоило бы CPU роутера на каждом заходе,
// а панель — единственное, что этот роутер отдаёт наружу; экономить здесь
// память ценой процессора незачем.
type panelStore struct {
	files map[string]*panelFile
	index *panelFile
}

func newPanelStore(sub fs.FS) (*panelStore, error) {
	st := &panelStore{files: map[string]*panelFile{}}

	err := fs.WalkDir(sub, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(sub, p)
		if err != nil {
			return err
		}

		ct := mime.TypeByExtension(path.Ext(p))
		if ct == "" {
			ct = "application/octet-stream"
		}

		f := preparedFile(b, ct)
		f.immutable = strings.HasPrefix(p, "assets/")

		st.files[p] = f
		return nil
	})
	if err != nil {
		return nil, err
	}
	st.index = st.files["index.html"]
	return st, nil
}

// preparedFile — тело, приготовленное к отдаче: сжатие и отпечаток
// считаются ОДИН раз, а не на каждый запрос.
//
// Отдельная функция, потому что тел таких два вида и они не родня: файлы
// панели (готовятся при старте) и каталог наборов geosite (готовится, когда
// его принесли с GitHub, — 60 КБ имён, которые панель перечитывает на каждом
// открытии вкладки). Общее у них ровно то, что здесь: сжать один раз, дать
// валидатор, чтобы повтор стоил 304.
func preparedFile(b []byte, ctype string) *panelFile {
	sum := sha256.Sum256(b)
	f := &panelFile{
		raw:   b,
		ctype: ctype,
		// Слабый ETag: тело может уехать сжатым, а по RFC 9110 сильный
		// валидатор обязан описывать ровно те байты, что в теле.
		etag: `W/"` + hex.EncodeToString(sum[:8]) + `"`,
	}

	// Сжимаем только то, что от этого выигрывает. Картинки и шрифты
	// уже сжаты, и второй проход даёт минус процент и трату CPU.
	if compressible(ctype) {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		if _, werr := zw.Write(b); werr == nil && zw.Close() == nil {
			// Оставляем, только если сжатие вообще что-то дало.
			if buf.Len() < len(b) {
				f.gz = append([]byte(nil), buf.Bytes()...)
			}
		}
	}
	return f
}

func compressible(ct string) bool {
	return strings.HasPrefix(ct, "text/") ||
		strings.Contains(ct, "javascript") ||
		strings.Contains(ct, "json") ||
		strings.Contains(ct, "svg")
}

// ServeHTTP отдаёт панель.
//
// Своя реализация вместо http.FileServer, и ради трёх вещей, которых у того
// нет: сжатия, осмысленных заголовков кэша и ETag. Без последних двух
// версия в адресе ассета (`?v=…`, ADR-0036) не покупает ничего: браузер и
// так перечитывал бы всё при каждом заходе, а deploy.sh был вынужден
// советовать владельцу жёсткую перезагрузку.
func (st *panelStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeErr(w, http.StatusMethodNotAllowed, "bad_request", "Панель отдаётся только по GET")
		return
	}

	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	f := st.files[p]
	if p == "" || p == "." {
		f = st.index
	}
	if f == nil {
		// Панель — не SPA с маршрутизацией: неизвестный путь это неизвестный
		// путь, а не «отдайте index и разберитесь на клиенте». Подмена
		// превратила бы опечатку в адресе ассета в белый экран без единой
		// ошибки в консоли.
		writeErr(w, http.StatusNotFound, "not_found", "Нет такого файла панели")
		return
	}

	// index.html — точка входа, и она обязана перечитываться всегда: именно
	// в ней лежат адреса ассетов с новой версией. Закэшируй её браузер — и
	// обновлённая панель не приедет НИКОГДА, потому что о новых адресах он
	// не узнает.
	cache := "no-store"
	if f.immutable {
		// Адрес несёт версию содержимого, поэтому по нему содержимое
		// измениться не может: год и immutable.
		cache = "public, max-age=31536000, immutable"
	}
	servePrepared(w, r, f, cache)
}

// servePrepared отдаёт приготовленное тело: ETag, 304, сжатие, длина.
//
// Срок кэша параметром, а не полем: он единственное, чем отдача панели
// отличается от отдачи каталога наборов. У index.html это no-store, у
// ассета с версией в адресе — год, у каталога — час. Всё остальное —
// условный запрос, выбор сжатого тела, Content-Length — обязано быть у
// них общим, потому что ошибиться здесь можно ровно один раз на каждую
// копию кода.
func servePrepared(w http.ResponseWriter, r *http.Request, f *panelFile, cacheControl string) {
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("ETag", f.etag)
	h.Set("Cache-Control", cacheControl)

	// Условный запрос. Отвечаем 304 до всякой записи тела: ради этого
	// ETag и заводился.
	if match := r.Header.Get("If-None-Match"); match != "" && etagMatch(match, f.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	body := f.raw
	if f.gz != nil && acceptsGzip(r.Header.Get("Accept-Encoding")) {
		h.Set("Content-Encoding", "gzip")
		// Vary обязателен: без него кэш-посредник отдал бы сжатое тело
		// клиенту, который сжатия не просил.
		h.Add("Vary", "Accept-Encoding")
		body = f.gz
	}

	// Своя запись, а не http.ServeContent: тот сам ставит Content-Length
	// и умеет Range, но заодно ставит Last-Modified и трогает ETag, а
	// у встроенной ФС времени файла нет вовсе — go:embed отдаёт нулевое,
	// и Last-Modified из него был бы враньём про 1 января первого года.
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// etagMatch — разбор If-None-Match. Список значений через запятую плюс `*`.
func etagMatch(header, etag string) bool {
	for _, part := range strings.Split(header, ",") {
		v := strings.TrimSpace(part)
		if v == "*" || v == etag {
			return true
		}
	}
	return false
}

// acceptsGzip — грубый, но достаточный разбор Accept-Encoding.
//
// Весов q здесь нет намеренно: единственный клиент панели — браузер
// владельца, и все они шлют gzip без весов. Разбирать RFC целиком ради
// случая, которого не бывает, значит завести код, который никогда не
// проверялся.
func acceptsGzip(h string) bool {
	for _, part := range strings.Split(h, ",") {
		if strings.EqualFold(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]), "gzip") {
			return true
		}
	}
	return false
}
