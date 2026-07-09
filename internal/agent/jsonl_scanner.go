package agent

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/loppo-llc/kojo/internal/chathistory"
)

// jsonlLineScanner reads newline-delimited JSON from an io.Reader with a
// per-line cap large enough for real CLI stream output. bufio.Scanner caps
// each token at MaxScanTokenSize (and the Buffer max we set), failing with
// "token too long" when a backend emits a single multi-MB line — e.g. a
// claude tool_result carrying a base64-encoded image Read easily exceeds
// 1MB. bufio.Reader.ReadSlice avoids that fixed scanner cap, while the
// explicit MaxJSONLLineBytes bound prevents a corrupted/adversarial stream
// from growing one line without limit.
//
// Two modes for a line over the cap:
//   - strict (skipOversized=false): the scanner stops with
//     chathistory.ErrLineTooLarge, matching the original codex behaviour
//     where the RPC framing is broken and continuing is unsafe.
//   - skip (skipOversized=true): the oversized line is discarded, Skipped()
//     is incremented, and scanning CONTINUES with the next line. This is
//     what long-lived event streams want: dropping one event is strictly
//     better than silently killing the whole stream while the backend
//     process keeps running (the turn then dies of timeout and every
//     tool_use after the bad line is lost from the record).
type jsonlLineScanner struct {
	r    *bufio.Reader
	line []byte
	err  error
	buf  []byte
	// skipOversized selects skip mode (see type docs).
	skipOversized bool
	// discarding is set mid-oversized-line in skip mode: read and throw
	// away chunks until the terminating newline.
	discarding bool
	// skipped counts oversized lines dropped in skip mode.
	skipped int
}

// newCodexLineScanner returns a strict-mode scanner: an oversized line is a
// fatal stream error (codex app-server JSON-RPC framing).
func newCodexLineScanner(r io.Reader) *jsonlLineScanner {
	return &jsonlLineScanner{r: bufio.NewReaderSize(r, 64*1024)}
}

// newSkippingLineScanner returns a skip-mode scanner: an oversized line is
// dropped (counted in Skipped()) and scanning continues.
func newSkippingLineScanner(r io.Reader) *jsonlLineScanner {
	return &jsonlLineScanner{r: bufio.NewReaderSize(r, 64*1024), skipOversized: true}
}

// Skipped reports how many oversized lines have been dropped so far
// (always 0 in strict mode).
func (s *jsonlLineScanner) Skipped() int { return s.skipped }

// Scan advances to the next line, stripping the trailing CR/LF. It returns
// false at EOF (after yielding any final unterminated line) or on a read error.
func (s *jsonlLineScanner) Scan() bool {
	if s.err != nil {
		return false
	}
	for {
		chunk, err := s.r.ReadSlice('\n')

		if s.discarding {
			// Mid-oversized-line: throw chunks away until the line ends.
			switch {
			case err == nil:
				s.discarding = false
				s.skipped++
				continue
			case errors.Is(err, bufio.ErrBufferFull):
				continue
			case errors.Is(err, io.EOF):
				s.skipped++
				s.err = io.EOF
				return false
			default:
				s.skipped++
				s.err = err
				return false
			}
		}

		switch {
		case err == nil:
			if len(s.buf) > 0 {
				if !s.appendChunk(chunk) {
					if s.err != nil {
						return false
					}
					// Skip mode: the newline was consumed with this
					// chunk, so the whole oversized line is gone.
					s.skipped++
					continue
				}
				s.line = bytes.TrimRight(s.buf, "\r\n")
				s.buf = nil
				return true
			}
			if len(chunk) > chathistory.MaxJSONLLineBytes {
				if s.skipOversized {
					s.skipped++
					continue
				}
				s.err = jsonlLineTooLargeErr()
				return false
			}
			s.line = bytes.TrimRight(chunk, "\r\n")
			return true

		case errors.Is(err, bufio.ErrBufferFull):
			if !s.appendChunk(chunk) {
				if s.err != nil {
					return false
				}
				// Skip mode: line continues past this chunk — keep
				// discarding until its newline.
				s.discarding = true
			}
			continue

		case errors.Is(err, io.EOF):
			if len(chunk) > 0 && !s.appendChunk(chunk) {
				if s.err != nil {
					return false
				}
				s.skipped++
				s.err = io.EOF
				return false
			}
			s.err = io.EOF
			if len(s.buf) == 0 {
				return false
			}
			s.line = bytes.TrimRight(s.buf, "\r\n")
			s.buf = nil
			return true

		default:
			if len(chunk) > 0 && !s.appendChunk(chunk) {
				if s.err == nil {
					s.skipped++
					s.err = err
				}
				return false
			}
			s.err = err
			if len(s.buf) == 0 {
				return false
			}
			s.line = bytes.TrimRight(s.buf, "\r\n")
			s.buf = nil
			return true
		}
	}
}

func (s *jsonlLineScanner) Text() string { return string(s.line) }

// Bytes returns the current line without the trailing newline. Like
// bufio.Scanner.Bytes, the slice may be overwritten by the next Scan.
func (s *jsonlLineScanner) Bytes() []byte { return s.line }

// Err returns the first non-EOF read error, or nil. EOF is the normal
// termination and is not reported as an error (matching bufio.Scanner).
func (s *jsonlLineScanner) Err() error {
	if s.err == io.EOF {
		return nil
	}
	return s.err
}

// appendChunk grows the pending line. On overflow it clears the buffer and
// returns false; in strict mode it also sets the fatal error (skip mode
// leaves err nil so the caller can discard the line and continue).
func (s *jsonlLineScanner) appendChunk(chunk []byte) bool {
	if len(s.buf)+len(chunk) > chathistory.MaxJSONLLineBytes {
		s.buf = nil
		if !s.skipOversized {
			s.err = jsonlLineTooLargeErr()
		}
		return false
	}
	s.buf = append(s.buf, chunk...)
	return true
}

func jsonlLineTooLargeErr() error {
	return fmt.Errorf("stream JSONL line exceeds %d bytes: %w", chathistory.MaxJSONLLineBytes, chathistory.ErrLineTooLarge)
}
