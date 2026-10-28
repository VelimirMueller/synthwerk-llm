// Package sse writes the answer stream of the chat contract (openapi/llm): frames
// with id "<message_id>:<sequence>", an event name and one line of JSON data. Every
// write flushes at once and has its own deadline, so a slow client cannot hold the
// handler forever and no layer in the service buffers the stream.
package sse

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// WriteTimeout is the deadline of one frame.
const WriteTimeout = 10 * time.Second

// Writer writes one stream. It is not safe for concurrent use.
type Writer struct {
	w   io.Writer
	rc  *http.ResponseController
	id  string
	seq int
}

// Start sends the stream headers and the 200 status.
func Start(w http.ResponseWriter, messageID string) (*Writer, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	s := &Writer{w: w, rc: http.NewResponseController(w), id: messageID}
	return s, s.flush()
}

// Event writes one frame. data is encoded as one line of JSON.
func (s *Writer) Event(name string, data any) error {
	raw, err := json.Marshal(data) // json.Marshal never emits a raw newline
	if err != nil {
		return fmt.Errorf("sse: encode %s: %w", name, err)
	}
	s.seq++
	if err := s.deadline(); err != nil {
		return err
	}
	frame := "id: " + s.id + ":" + strconv.Itoa(s.seq) + "\nevent: " + name + "\ndata: " + string(raw) + "\n\n"
	if _, err := io.WriteString(s.w, frame); err != nil {
		return fmt.Errorf("sse: write: %w", err)
	}
	return s.flush()
}

// Ping writes the keep-alive comment line.
func (s *Writer) Ping() error {
	if err := s.deadline(); err != nil {
		return err
	}
	if _, err := io.WriteString(s.w, ": ping\n\n"); err != nil {
		return fmt.Errorf("sse: write: %w", err)
	}
	return s.flush()
}

func (s *Writer) deadline() error {
	// httptest.ResponseRecorder has no deadlines: ignore ErrNotSupported.
	if err := s.rc.SetWriteDeadline(time.Now().Add(WriteTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return fmt.Errorf("sse: deadline: %w", err)
	}
	return nil
}

func (s *Writer) flush() error {
	if err := s.rc.Flush(); err != nil {
		return fmt.Errorf("sse: flush: %w", err)
	}
	return nil
}
