package sse_test

import (
	"net/http/httptest"
	"testing"

	"github.com/VelimirMueller/synthwerk-llm/internal/sse"
)

func TestWriterOneDeltaExactFrameBytes(t *testing.T) {
	rec := httptest.NewRecorder()
	w, err := sse.Start(rec, "m1")
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Event("delta", map[string]string{"text": "a\nb"}); err != nil {
		t.Fatal(err)
	}
	if err := w.Event("done", map[string]string{"x": "y"}); err != nil {
		t.Fatal(err)
	}
	want := "id: m1:1\nevent: delta\ndata: {\"text\":\"a\\nb\"}\n\n" + "id: m1:2\nevent: done\ndata: {\"x\":\"y\"}\n\n"
	if got := rec.Body.String(); got != want {
		t.Fatalf("body\n%q\nwant\n%q", got, want)
	}
	for k, v := range map[string]string{"Content-Type": "text/event-stream", "Cache-Control": "no-store", "X-Accel-Buffering": "no"} {
		if rec.Header().Get(k) != v {
			t.Errorf("%s = %q, want %q", k, rec.Header().Get(k), v)
		}
	}
	if !rec.Flushed {
		t.Error("not flushed")
	}
}

func TestWriterHeartbeatCommentLineOnly(t *testing.T) {
	rec := httptest.NewRecorder()
	w, _ := sse.Start(rec, "m1")
	if err := w.Ping(); err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != ": ping\n\n" {
		t.Fatalf("body %q", got)
	}
}
