package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// sseWriter formats and flushes Server-Sent Events. It exists so handlers
// don't have to fumble with the wire format (event:/data:/blank-line) on
// every emit.
type sseWriter struct {
	w       io.Writer
	flusher http.Flusher
	enc     *json.Encoder
}

// newSSEWriter sets the standard SSE response headers and returns a writer.
// Returns an error if the response writer doesn't support flushing (which
// would silently buffer events until the audit ended — useless for SSE).
func newSSEWriter(w http.ResponseWriter) (*sseWriter, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return nil, fmt.Errorf("response writer doesn't implement http.Flusher")
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // hint to nginx not to buffer
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	return &sseWriter{w: w, flusher: flusher, enc: json.NewEncoder(w)}, nil
}

// emit writes one named SSE event with v marshalled as the data payload.
// SSE requires data to be one line (no raw newlines) — we encode then strip
// the trailing newline Encoder appends.
func (s *sseWriter) emit(event string, v any) error {
	if _, err := fmt.Fprintf(s.w, "event: %s\ndata: ", event); err != nil {
		return err
	}
	// json.Encoder appends '\n'; that becomes our event terminator.
	if err := s.enc.Encode(v); err != nil {
		return err
	}
	// SSE event terminator is a blank line — we already have one '\n' from
	// Encode, add a second.
	if _, err := io.WriteString(s.w, "\n"); err != nil {
		return err
	}
	s.flusher.Flush()
	return nil
}
