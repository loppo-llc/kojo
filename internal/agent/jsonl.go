package agent

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
)

// jsonlAppend appends a JSON-encodable value as a line to a JSONL file.
func jsonlAppend(path string, v any) error {
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = f.Write(data)
	return err
}

// jsonlLoadPaginated reads a JSONL file with cursor-based pagination.
// idFunc extracts the ID from each record for cursor matching.
// If before is non-empty, returns only records before that ID.
// If limit > 0, returns the last `limit` records.
func jsonlLoadPaginated[T any](path string, limit int, before string, idFunc func(*T) string) ([]*T, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer f.Close()

	var all []*T
	r := bufio.NewReader(f)
	for {
		// ReadBytes has no fixed cap, so a single record can be arbitrarily
		// large. The returned slice includes the trailing '\n' when present;
		// json.Unmarshal tolerates trailing whitespace, so we pass it as-is.
		line, readErr := r.ReadBytes('\n')
		if len(line) > 0 {
			var v T
			if jerr := json.Unmarshal(line, &v); jerr == nil {
				all = append(all, &v)
			}
			// malformed/empty lines are skipped
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return all, false, readErr
		}
	}

	if before != "" {
		idx := -1
		for i, v := range all {
			if idFunc(v) == before {
				idx = i
				break
			}
		}
		if idx >= 0 {
			all = all[:idx]
		}
	}

	hasMore := false
	if limit > 0 && len(all) > limit {
		hasMore = true
		all = all[len(all)-limit:]
	}
	return all, hasMore, nil
}

