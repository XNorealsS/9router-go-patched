package proxy

import (
	"io"
	"net/http"
)

// StreamWriter wraps an io.Writer and handles flushing if the writer supports it.
type StreamWriter struct {
	w       io.Writer
	flusher http.Flusher
}
