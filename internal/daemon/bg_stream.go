package daemon

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// StreamEventKind identifies which fields on StreamEvent are populated.
type StreamEventKind int

const (
	StreamEventOther StreamEventKind = iota
	StreamEventInit
	StreamEventAssistantText
	StreamEventToolUse
	StreamEventResult
)

// StreamEvent is one decoded item from a `claude --print --output-format=stream-json`
// subprocess. Only the fields named by Kind are meaningful.
type StreamEvent struct {
	Kind StreamEventKind

	Text string

	Tool string

	// SessionID is set on StreamEventInit; carries the CC session_id from the
	// system/init line so spawned interactive sessions can be referenced for
	// resume or status display.
	SessionID string

	OK         bool
	ResultText string
	NumTurns   int
	DurationMs int
	CostUSD    float64
	IsError    bool
}

// StreamReader parses newline-delimited JSON from r. Each call to Next returns
// one event or io.EOF. Malformed lines emit StreamEventOther and are not fatal.
type StreamReader struct {
	sc      *bufio.Scanner
	pending []StreamEvent
}

const streamScannerMax = 4 * 1024 * 1024

func NewStreamReader(r io.Reader) *StreamReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), streamScannerMax)

	return &StreamReader{sc: sc}
}

// Next returns the next parsed event or io.EOF when the underlying reader is
// drained. Lines that fail to parse yield StreamEventOther.
func (s *StreamReader) Next() (StreamEvent, error) {
	if len(s.pending) > 0 {
		ev := s.pending[0]
		s.pending = s.pending[1:]

		return ev, nil
	}

	for s.sc.Scan() {
		line := strings.TrimSpace(s.sc.Text())
		if line == "" {
			continue
		}

		evs := parseStreamLine(line)
		if len(evs) == 0 {
			return StreamEvent{Kind: StreamEventOther}, nil
		}

		s.pending = evs[1:]

		return evs[0], nil
	}

	if err := s.sc.Err(); err != nil {
		return StreamEvent{}, err
	}

	return StreamEvent{}, io.EOF
}

// streamLine is the shallow envelope shared by every stream-json line type.
type streamLine struct {
	Type    string          `json:"type"`
	Subtype string          `json:"subtype"`
	Message json.RawMessage `json:"message"`

	SessionID string `json:"session_id"`

	IsError      bool    `json:"is_error"`
	DurationMs   int     `json:"duration_ms"`
	NumTurns     int     `json:"num_turns"`
	Result       string  `json:"result"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

type streamMessage struct {
	Content []streamContent `json:"content"`
}

type streamContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
	Name string `json:"name"`
}

// parseStreamLine decodes one JSON line into zero or more StreamEvents.
// Returns a single StreamEventOther for unrecognised or malformed input so the
// reader never observes an empty slice from a non-blank line.
func parseStreamLine(line string) []StreamEvent {
	other := []StreamEvent{{Kind: StreamEventOther}}

	var head streamLine
	if err := json.Unmarshal([]byte(line), &head); err != nil {
		return other
	}

	switch head.Type {
	case "system":
		if head.Subtype == "init" {
			return []StreamEvent{{Kind: StreamEventInit, SessionID: head.SessionID}}
		}

		return other

	case "assistant":
		if len(head.Message) == 0 {
			return other
		}

		var msg streamMessage
		if err := json.Unmarshal(head.Message, &msg); err != nil {
			return other
		}

		evs := make([]StreamEvent, 0, len(msg.Content))

		for _, c := range msg.Content {
			switch c.Type {
			case "text":
				evs = append(evs, StreamEvent{Kind: StreamEventAssistantText, Text: c.Text})
			case "tool_use":
				evs = append(evs, StreamEvent{Kind: StreamEventToolUse, Tool: c.Name})
			}
		}

		if len(evs) == 0 {
			return other
		}

		return evs

	case "result":
		return []StreamEvent{{
			Kind:       StreamEventResult,
			OK:         !head.IsError,
			IsError:    head.IsError,
			ResultText: head.Result,
			NumTurns:   head.NumTurns,
			DurationMs: head.DurationMs,
			CostUSD:    head.TotalCostUSD,
		}}

	default:
		return other
	}
}
