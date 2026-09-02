package builder

import (
	"bufio"
	"io"
	"strings"
)

// sseEvent is a parsed Server-Sent Event. Only the fields the builder cares
// about are kept: the event name and the concatenated data payload.
type sseEvent struct {
	event string
	data  string
}

// readSSEEvent reads a single SSE event from r. It accumulates lines until the
// blank-line separator, joins multi-line "data:" fields in order (per the SSE
// spec), ignores comment lines (": ...") and unknown fields, and drops
// partial events when the stream ends without a terminator. A keep-alive
// comment or any event without data yields an event with empty data.
func readSSEEvent(r *bufio.Reader) (*sseEvent, error) {
	var (
		eventName string
		dataLines []string
		lines     int
	)
	for {
		line, err := r.ReadString('\n')
		if err != nil && len(line) == 0 {
			if err == io.EOF {
				return nil, io.EOF
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			// Blank line terminates the event (SSE spec). Stray blank lines
			// between events (keep-alives) are skipped.
			if lines == 0 {
				continue
			}
			break
		}
		lines++
		switch {
		case strings.HasPrefix(line, "event:"):
			eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		default:
			// Comment lines (": ping") and unknown fields are ignored.
		}
		if err == io.EOF {
			// Stream ended without an event terminator: drop the partial
			// event rather than parsing truncated JSON.
			return nil, io.EOF
		}
	}
	return &sseEvent{event: eventName, data: strings.Join(dataLines, "\n")}, nil
}
