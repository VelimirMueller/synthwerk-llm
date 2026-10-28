// Package sse reads a Server-Sent Events answer stream in tests.
package sse

import (
	"bufio"
	"io"
	"strings"
	"time"
)

// Event is one SSE event. Comment lines (": ping") are Event "comment".
type Event struct {
	ID    string
	Event string
	Data  string
	// At is when the event arrived.
	At time.Time
}

// Read parses body until it ends and sends every event on the channel.
func Read(body io.Reader) <-chan Event {
	out := make(chan Event, 256)
	go func() {
		defer close(out)
		sc := bufio.NewScanner(body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		var ev Event
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if ev.Event != "" || ev.Data != "" {
					ev.At = time.Now()
					out <- ev
				}
				ev = Event{}
			case strings.HasPrefix(line, ":"):
				out <- Event{Event: "comment", Data: strings.TrimSpace(line[1:]), At: time.Now()}
			case strings.HasPrefix(line, "id: "):
				ev.ID = line[4:]
			case strings.HasPrefix(line, "event: "):
				ev.Event = line[7:]
			case strings.HasPrefix(line, "data: "):
				ev.Data += line[6:]
			}
		}
	}()
	return out
}

// All collects every event until the stream ends.
func All(body io.Reader) []Event {
	var out []Event
	for ev := range Read(body) {
		out = append(out, ev)
	}
	return out
}
