package protocol

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// StreamWriter frames chunks as assistant-transport SSE (doc/agent-impl.md §2.4).
//
// The wire rules are strict and the reason this package exists rather than using
// Huma's sse helper:
//
//   - Each chunk is exactly one `data: <json>\n\n` frame.
//   - The stream MUST end with `data: [DONE]\n\n`.
//   - NO `event:` line may ever be written. The decoder in strict mode requires
//     the default event name `message`; any explicit `event:` fails the whole
//     stream (§2.4, ADR-0002 §4). Huma's sse package forces an `event:` line,
//     which is why these endpoints are registered as bare Gin handlers (§9.1).
//
// A FlushFunc is invoked after every frame so a reverse proxy or browser sees
// bytes immediately instead of buffering (§9.3).
type StreamWriter struct {
	w     io.Writer
	flush func()
}

// NewStreamWriter builds a writer. flush may be nil (buffered writers in tests).
func NewStreamWriter(w io.Writer, flush func()) *StreamWriter {
	if flush == nil {
		flush = func() {}
	}
	return &StreamWriter{w: w, flush: flush}
}

// Done is the sentinel that terminates a stream (§2.4).
const Done = "[DONE]"

// WriteChunk validates and emits one chunk as a data frame, then flushes. It
// refuses to write an invalid chunk so a malformed frame can never reach the
// client's strict decoder.
func (s *StreamWriter) WriteChunk(chunk Chunk) error {
	if err := chunk.Validate(); err != nil {
		return err
	}
	encoded, err := json.Marshal(chunk)
	if err != nil {
		return fmt.Errorf("protocol: encode chunk: %w", err)
	}
	return s.writeData(string(encoded))
}

// WriteState emits a single update-state chunk carrying ops.
func (s *StreamWriter) WriteState(ops ...Operation) error {
	return s.WriteChunk(UpdateState(ops...))
}

// WriteRaw emits an already-encoded chunk JSON as one data frame. It is used to
// replay persisted chunks from agent_run_chunks verbatim on resume/reconnect
// (doc/agent-impl.md §2.8, §8), guaranteeing the replayed bytes are identical to
// what the run originally produced. The payload must be single-line JSON, which
// json.Marshal always yields.
func (s *StreamWriter) WriteRaw(chunkJSON string) error {
	return s.writeData(chunkJSON)
}

// WriteDone emits the terminal `data: [DONE]\n\n` frame (§2.4).
func (s *StreamWriter) WriteDone() error {
	return s.writeData(Done)
}

// WriteComment emits an SSE comment line (`: ...\n\n`) used as a heartbeat to
// survive proxy idle timeouts. A comment is not a data frame and never reaches
// the decoder (§9.3).
func (s *StreamWriter) WriteComment(text string) error {
	text = strings.ReplaceAll(text, "\n", " ")
	if _, err := io.WriteString(s.w, ": "+text+"\n\n"); err != nil {
		return err
	}
	s.flush()
	return nil
}

// writeData emits one `data: <payload>\n\n` frame. Payloads containing newlines
// are split into multiple data lines per the SSE spec, though our JSON is always
// single-line.
func (s *StreamWriter) writeData(payload string) error {
	var frame strings.Builder
	for _, line := range strings.Split(payload, "\n") {
		frame.WriteString("data: ")
		frame.WriteString(line)
		frame.WriteString("\n")
	}
	frame.WriteString("\n")
	if _, err := io.WriteString(s.w, frame.String()); err != nil {
		return err
	}
	s.flush()
	return nil
}
