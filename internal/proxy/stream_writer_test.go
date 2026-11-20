package proxy

import (
	"net/http"
)

// failWriter always returns an error on Write.
type failWriter struct{}

func (f *failWriter) Write(p []byte) (int, error) {
	return 0, http.ErrNotSupported
}
