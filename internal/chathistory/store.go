package chathistory

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

// MaxJSONLLineBytes caps the size of a single JSONL line read through
// ScanJSONLLines below (the backend stream scanners). Without a cap the per-line
// accumulator buffer (filled across repeated ReadSlice+ErrBufferFull chunks
// when a line exceeds bufio's default buffer) could grow without bound, so a
// corrupted/adversarial extremely-long line could allocate arbitrarily large
// amounts of memory. 10 MiB is far above realistic message payloads (a Slack
// message is < 40 KiB, a Claude transcript turn rarely exceeds 1 MiB), but
// small enough that hitting it almost certainly indicates corruption or
// attack. ScanJSONLLines surfaces ErrLineTooLarge once accumulated bytes
// would cross this threshold and stops reading.
const MaxJSONLLineBytes = 10 << 20

// ErrLineTooLarge is returned by ScanJSONLLines when a single line exceeds
// MaxJSONLLineBytes before a newline is found.
var ErrLineTooLarge = errors.New("jsonl line exceeds max size")

// ScanJSONLLines reads r line-by-line, invoking onLine for each non-empty
// line. The internal buffer is bounded at MaxJSONLLineBytes — once that many
// bytes accumulate without seeing '\n', ErrLineTooLarge is returned and no
// more data is consumed. Other I/O errors are returned wrapped; io.EOF on a
// trailing line without a final newline is treated as a clean end.
func ScanJSONLLines(r io.Reader, onLine func(line []byte)) error {
	br := bufio.NewReader(r)
	var buf []byte
	for {
		chunk, err := br.ReadSlice('\n')
		if err == nil {
			// Complete line found via the internal buffer. If we've been
			// accumulating in `buf` because earlier ReadSlice calls hit
			// ErrBufferFull, the final tail chunk could push the total over
			// the cap — check here too so onLine never receives a
			// > MaxJSONLLineBytes slice.
			if len(buf) > 0 {
				if len(buf)+len(chunk) > MaxJSONLLineBytes {
					return ErrLineTooLarge
				}
				buf = append(buf, chunk...)
				onLine(buf)
				buf = nil
			} else {
				if len(chunk) > MaxJSONLLineBytes {
					return ErrLineTooLarge
				}
				// Copy because ReadSlice returns a view into the bufio buffer
				// that is invalidated on the next read.
				line := append([]byte(nil), chunk...)
				onLine(line)
			}
			continue
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			// Line is longer than bufio's default buffer (4 KiB); accumulate.
			if len(buf)+len(chunk) > MaxJSONLLineBytes {
				return ErrLineTooLarge
			}
			buf = append(buf, chunk...)
			continue
		}
		if errors.Is(err, io.EOF) {
			// Trailing line without final '\n'.
			if len(chunk) > 0 || len(buf) > 0 {
				buf = append(buf, chunk...)
				if len(buf) > MaxJSONLLineBytes {
					return ErrLineTooLarge
				}
				onLine(buf)
			}
			return nil
		}
		return fmt.Errorf("read jsonl: %w", err)
	}
}
