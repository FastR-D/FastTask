// Package protocol implements the assistant-transport wire protocol between the
// FastTask backend and the assistant-ui frontend (doc/agent-impl.md §2,
// ADR-0002). It is deliberately independent of the LLM provider and HTTP layers
// so the encoder can be unit-tested against the byte-level contract.
//
// Two facts drive every type here:
//
//  1. The client renders messages from server STATE, not from part chunks
//     (§2.6). Streaming text is therefore an `append-text` update-state
//     operation, and an `append-text` requires its target to already exist as a
//     string — so a `set` of {"type":"text","text":""} must come first.
//  2. Chunk-level `path` is an INTEGER array; update-state operation `path` is a
//     STRING array (§2.5, §2.6). They are not interchangeable.
package protocol

import (
	"encoding/json"
	"fmt"
)

// ChunkType enumerates the only chunk types the Go encoder may emit (§2.5).
// Unknown types are dropped by the decoder, or fail the stream in strict mode.
type ChunkType string

const (
	ChunkUpdateState            ChunkType = "update-state"
	ChunkPartStart              ChunkType = "part-start"
	ChunkAnnotations            ChunkType = "annotations"
	ChunkData                   ChunkType = "data"
	ChunkStepStart              ChunkType = "step-start"
	ChunkStepFinish             ChunkType = "step-finish"
	ChunkMessageFinish          ChunkType = "message-finish"
	ChunkError                  ChunkType = "error"
	ChunkTextDelta              ChunkType = "text-delta"
	ChunkPartFinish             ChunkType = "part-finish"
	ChunkToolCallArgsTextFinish ChunkType = "tool-call-args-text-finish"
	ChunkResult                 ChunkType = "result"
)

// pathRequired lists the chunk types whose `path` field is mandatory (§2.5).
var pathRequired = map[ChunkType]bool{
	ChunkTextDelta:              true,
	ChunkPartFinish:             true,
	ChunkToolCallArgsTextFinish: true,
	ChunkResult:                 true,
}

// validChunkTypes is the closed set the encoder is allowed to emit (§2.5).
var validChunkTypes = map[ChunkType]bool{
	ChunkUpdateState: true, ChunkPartStart: true, ChunkAnnotations: true,
	ChunkData: true, ChunkStepStart: true, ChunkStepFinish: true,
	ChunkMessageFinish: true, ChunkError: true, ChunkTextDelta: true,
	ChunkPartFinish: true, ChunkToolCallArgsTextFinish: true, ChunkResult: true,
}

// Chunk is one line of the SSE stream. Exactly the fields relevant to Type are
// populated; the rest are omitted so the JSON matches the decoder's expectation.
// Path is a pointer so the encoder can distinguish "omitted" (optional-path
// types) from "present but empty" (a required path of []).
type Chunk struct {
	Type ChunkType `json:"type"`
	Path *[]int    `json:"path,omitempty"`

	Operations   []Operation `json:"operations,omitempty"`
	Part         *Part       `json:"part,omitempty"`
	Annotations  []any       `json:"annotations,omitempty"`
	Data         []any       `json:"data,omitempty"`
	FinishReason string      `json:"finishReason,omitempty"`
	TextDelta    string      `json:"textDelta,omitempty"`
	IsError      *bool       `json:"isError,omitempty"`
}

// OperationType is the kind of an update-state operation (§2.6).
type OperationType string

const (
	OpSet        OperationType = "set"
	OpAppendText OperationType = "append-text"
)

// Operation is a single mutation of server state. Path is a STRING array whose
// segments are object keys or integer-form array indices ("3"); append-text
// requires the target to already hold a string (§2.6).
type Operation struct {
	Type  OperationType `json:"type"`
	Path  []string      `json:"path"`
	Value any           `json:"value"`
}

// unsafePathSegments are rejected by the client decoder (§2.6). The server never
// generates them, but guarding here keeps a future refactor from emitting one.
var unsafePathSegments = map[string]bool{"__proto__": true, "constructor": true, "prototype": true}

func validatePath(path []string) error {
	for _, segment := range path {
		if unsafePathSegments[segment] {
			return fmt.Errorf("protocol: unsafe path segment %q", segment)
		}
	}
	return nil
}

// Set builds a `set` operation assigning value at the string path.
func Set(path []string, value any) (Operation, error) {
	if err := validatePath(path); err != nil {
		return Operation{}, err
	}
	return Operation{Type: OpSet, Path: path, Value: value}, nil
}

// AppendText builds an `append-text` operation. The caller must have previously
// `set` a string at path or the client decoder throws (§2.6).
func AppendText(path []string, text string) (Operation, error) {
	if err := validatePath(path); err != nil {
		return Operation{}, err
	}
	return Operation{Type: OpAppendText, Path: path, Value: text}, nil
}

// UpdateState builds an update-state chunk carrying the given operations. This
// is the authoritative render path (§2.6).
func UpdateState(ops ...Operation) Chunk {
	return Chunk{Type: ChunkUpdateState, Operations: ops}
}

// StepStart and StepFinish bracket one model step (§2.5).
func StepStart() Chunk { return Chunk{Type: ChunkStepStart} }

func StepFinish(reason string) Chunk {
	return Chunk{Type: ChunkStepFinish, FinishReason: reason}
}

// MessageFinish terminates an assistant message with a finish reason (§2.5).
func MessageFinish(reason string) Chunk {
	return Chunk{Type: ChunkMessageFinish, FinishReason: reason}
}

// ErrorChunk signals a stream-level error (§2.5). The path is optional.
func ErrorChunk() Chunk { return Chunk{Type: ChunkError} }

// PartStart announces a message part object (§2.5). Path is optional.
func PartStart(part Part) Chunk { return Chunk{Type: ChunkPartStart, Part: &part} }

// Annotations and Data carry auxiliary arrays (§2.5). Path is optional.
func Annotations(values []any) Chunk { return Chunk{Type: ChunkAnnotations, Annotations: values} }
func Data(values []any) Chunk        { return Chunk{Type: ChunkData, Data: values} }

// The four constructors below produce chunks whose integer path is REQUIRED
// (§2.5). They always set a non-nil Path so it marshals even when empty.

// TextDelta appends model text at an integer path (§2.5). FastTask streams via
// append-text instead (§2.6); this exists for protocol completeness.
func TextDelta(path []int, delta string) Chunk {
	p := append([]int(nil), path...)
	return Chunk{Type: ChunkTextDelta, Path: &p, TextDelta: delta}
}

// PartFinish marks the part at an integer path complete (§2.5).
func PartFinish(path []int) Chunk {
	p := append([]int(nil), path...)
	return Chunk{Type: ChunkPartFinish, Path: &p}
}

// ToolCallArgsTextFinish marks streamed tool-call arguments complete (§2.5).
func ToolCallArgsTextFinish(path []int) Chunk {
	p := append([]int(nil), path...)
	return Chunk{Type: ChunkToolCallArgsTextFinish, Path: &p}
}

// Result terminates a tool call at an integer path; isError is optional (§2.5).
func Result(path []int, isError bool) Chunk {
	p := append([]int(nil), path...)
	return Chunk{Type: ChunkResult, Path: &p, IsError: &isError}
}

// Validate reports whether the chunk is well-formed for its type (§2.5). The
// StreamWriter refuses to emit an invalid chunk so a malformed frame can never
// silently break the client's strict decoder.
func (c Chunk) Validate() error {
	if !validChunkTypes[c.Type] {
		return fmt.Errorf("protocol: unknown chunk type %q", c.Type)
	}
	if pathRequired[c.Type] && c.Path == nil {
		return fmt.Errorf("protocol: chunk type %q requires a path", c.Type)
	}
	if c.Path != nil {
		for _, segment := range *c.Path {
			if segment < 0 {
				return fmt.Errorf("protocol: chunk path segment must be non-negative, got %d", segment)
			}
		}
	}
	switch c.Type {
	case ChunkUpdateState:
		if c.Operations == nil {
			return fmt.Errorf("protocol: update-state requires operations")
		}
		for _, op := range c.Operations {
			if op.Type != OpSet && op.Type != OpAppendText {
				return fmt.Errorf("protocol: unknown operation type %q", op.Type)
			}
			if len(op.Path) == 0 {
				return fmt.Errorf("protocol: %s operation requires a path", op.Type)
			}
		}
	case ChunkPartStart:
		if c.Part == nil {
			return fmt.Errorf("protocol: part-start requires a part")
		}
	case ChunkStepFinish, ChunkMessageFinish:
		if c.FinishReason == "" {
			return fmt.Errorf("protocol: %s requires finishReason", c.Type)
		}
	}
	return nil
}

// MarshalJSON implements json.Marshaler. It exists only to keep the exported
// shape explicit; the struct tags already produce the wire format.
func (c Chunk) MarshalJSON() ([]byte, error) {
	type wire Chunk
	return json.Marshal(wire(c))
}
