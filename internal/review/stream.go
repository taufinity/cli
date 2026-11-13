package review

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// maxLineBytes bounds one NDJSON line. A result carries the whole review and
// can be several MiB; this only stops a runaway stream from filling memory.
const maxLineBytes = 64 << 20

// Event is a non-terminal line: accepted, progress or keepalive.
type Event struct {
	Type, Stage, RequestID string
}

// Finding is one entry of a result's findings list.
type Finding struct {
	Severity, Title, File string
	Line                  int
}

// Terminal is the line that ends the stream: a result or an error.
type Terminal struct {
	Type, Verdict, Markdown, Code, Message string
	Findings                               []Finding
	Counts                                 map[string]int
	Partial                                bool
	// Raw is the terminal line with cost_usd removed: members never see an
	// amount, so it is dropped before anything can print it.
	Raw json.RawMessage
}

// wireFinding mirrors the service's lowercase keys.
type wireFinding struct {
	Severity string `json:"severity"`
	Title    string `json:"title"`
	File     string `json:"file"`
	Line     int    `json:"line"`
}

type wireLine struct {
	Type      string         `json:"type"`
	Stage     string         `json:"stage"`
	RequestID string         `json:"request_id"`
	Verdict   string         `json:"verdict"`
	Markdown  string         `json:"markdown"`
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	Findings  []wireFinding  `json:"findings"`
	Counts    map[string]int `json:"counts"`
	Partial   bool           `json:"partial"`
}

// ReadStream reads Studio's NDJSON body line by line and returns the terminal
// line. Every line is parsed as JSON (the service's keys are alphabetical, so
// "type" is not first and a prefix match would be wrong). Lines that are not
// JSON objects and unknown types are ignored. onEvent, when set, sees each
// accepted, progress and keepalive line in order. A stream that ends without
// a terminal line is an error.
func ReadStream(r io.Reader, onEvent func(Event)) (Terminal, error) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, rerr := readLine(br)
		if t, ok := handleLine(line, onEvent); ok {
			return t, nil
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return Terminal{}, errors.New("the review stream ended without a result")
			}
			return Terminal{}, fmt.Errorf("reading the review stream: %w", rerr)
		}
	}
}

// readLine returns one line without its newline, of any length up to
// maxLineBytes. The error is io.EOF at the end of input (the final line may
// come back together with it).
func readLine(br *bufio.Reader) ([]byte, error) {
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		buf = append(buf, chunk...)
		if len(buf) > maxLineBytes {
			return nil, fmt.Errorf("a line of the stream is longer than %d bytes", maxLineBytes)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return bytes.TrimRight(buf, "\r\n"), err
	}
}

func handleLine(line []byte, onEvent func(Event)) (Terminal, bool) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return Terminal{}, false
	}
	var w wireLine
	if err := json.Unmarshal(line, &w); err != nil {
		return Terminal{}, false
	}
	switch w.Type {
	case "accepted", "progress", "keepalive":
		if onEvent != nil {
			onEvent(Event{Type: w.Type, Stage: w.Stage, RequestID: w.RequestID})
		}
		return Terminal{}, false
	case "result", "error":
		t := Terminal{
			Type: w.Type, Verdict: w.Verdict, Markdown: w.Markdown, Code: w.Code,
			Message: w.Message, Counts: w.Counts, Partial: w.Partial,
			Raw: stripCost(line),
		}
		for _, f := range w.Findings {
			t.Findings = append(t.Findings, Finding{Severity: f.Severity, Title: f.Title, File: f.File, Line: f.Line})
		}
		return t, true
	}
	return Terminal{}, false
}

// stripCost removes every key that names a cost or an amount in USD, at any
// depth, so nothing the service adds later can carry an amount to a member.
func stripCost(line []byte) json.RawMessage {
	var v any
	if err := json.Unmarshal(line, &v); err != nil {
		return json.RawMessage(line)
	}
	out, err := json.Marshal(dropCostKeys(v))
	if err != nil {
		return json.RawMessage(line)
	}
	return out
}

func dropCostKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			lk := strings.ToLower(k)
			if strings.Contains(lk, "cost") || strings.Contains(lk, "usd") {
				delete(t, k)
				continue
			}
			t[k] = dropCostKeys(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = dropCostKeys(t[i])
		}
	}
	return v
}
