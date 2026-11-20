package proxy

import (
	"bytes"
	"errors"
	"testing"
)

func TestScanStreamChunks(t *testing.T) {
	streamData := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: [DONE]\n\n")
	buf := bytes.NewBuffer(streamData)

	var chunks [][]byte
	err := ScanStream(buf, func(chunk []byte) {
		chunks = append(chunks, chunk)
	})

	if err != nil {
		t.Fatalf("ScanStream failed: %v", err)
	}

	if len(chunks) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(chunks))
	}
	if !bytes.Equal(chunks[1], []byte("[DONE]")) {
		t.Errorf("expected last chunk to be [DONE], got %s", string(chunks[1]))
	}
}

func TestScanStreamAccumulatesEvents(t *testing.T) {
	// Multi-line data payload, keep-alive, comment, and CRLF endings.
	streamData := []byte(": keep-alive\r\n" +
		"data: {\"a\":1,\r\n" +
		"data: \"b\":2}\r\n" +
		"\r\n" +
		"data: \r\n" +
		"\r\n" +
		"data: [DONE]\r\n")
	buf := bytes.NewBuffer(streamData)

	var chunks [][]byte
	err := ScanStream(buf, func(chunk []byte) {
		chunks = append(chunks, chunk)
	})

	if err != nil {
		t.Fatalf("ScanStream failed: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %q", len(chunks), chunks)
	}
	// Multi-line payload is joined with a newline.
	want := "{\"a\":1,\n\"b\":2}"
	if string(chunks[0]) != want {
		t.Errorf("expected %q, got %q", want, string(chunks[0]))
	}
	if string(chunks[1]) != "[DONE]" {
		t.Errorf("expected last chunk [DONE], got %q", string(chunks[1]))
	}
}

type mockFlusher struct {
	bytes.Buffer
	flushed bool
}

func (f *mockFlusher) Flush() {
	f.flushed = true
}

type errorWriter struct{}

func (ew *errorWriter) Write(p []byte) (n int, err error) {
	return 0, errors.New("write error")
}
