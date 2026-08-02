package nikki

import "bytes"

// jsonReader — тело запроса. Отдельный тип, чтобы http.NewRequest мог
// переотправить тело при редиректе; bytes.Reader это умеет, а io.Reader нет.
type jsonReader = bytes.Reader

func newJSONReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
