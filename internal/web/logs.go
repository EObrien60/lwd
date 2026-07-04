package web

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
)

// handleLogs streams a container's live logs to the browser as
// Server-Sent Events. It adapts the raw byte stream returned by
// s.client.Logs into SSE frames, one `data: <line>\n\n` frame per line,
// flushing after each frame so the browser receives lines as they arrive.
//
// The stream runs until the client disconnects: r.Context() is passed to
// Logs so the daemon call unblocks and returns when the request context is
// canceled.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("streaming not supported by this response writer"))
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sseW := &sseWriter{w: w, flusher: flusher}
	_ = s.client.Logs(r.Context(), name, true, sseW)
}

// sseWriter is an io.Writer that reframes an arbitrary byte stream into
// Server-Sent Events data frames, one per line. Bytes are buffered until a
// newline is seen; each complete line is emitted as `data: <line>\n\n` and
// flushed immediately so the browser sees it without delay. Any trailing
// partial line left in the buffer when the stream ends is dropped (the
// underlying stream is line-oriented in practice, so this is not expected to
// lose data).
type sseWriter struct {
	w       io.Writer
	flusher http.Flusher
	buf     bytes.Buffer
}

func (s *sseWriter) Write(p []byte) (int, error) {
	n := len(p)
	s.buf.Write(p)

	for {
		line, err := s.buf.ReadString('\n')
		if err != nil {
			// No complete line yet; put back the partial data and wait for
			// more bytes.
			s.buf.WriteString(line)
			break
		}
		line = line[:len(line)-1] // strip trailing \n
		if _, err := fmt.Fprintf(s.w, "data: %s\n\n", line); err != nil {
			return n, err
		}
		s.flusher.Flush()
	}

	return n, nil
}
