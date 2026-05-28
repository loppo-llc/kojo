package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// NowMillis returns the current UTC time in epoch milliseconds. All timestamps
// in kojo's structured store use this representation (3.2 in the design doc).
// Callers that need to inject a fake clock should do so at the boundary of
// their handler rather than monkey-patching this function.
func NowMillis() int64 {
	return time.Now().UTC().UnixMilli()
}

// monotonicClock guarantees strictly increasing values within a single
// process even if the wall clock jumps backwards. Used for `seq` on tables
// whose partition is "global" so insertion order is preserved during a clock
// skew correction.
//
// Per-partition seq (per agent_id, per groupdm_id, etc.) is allocated by the
// table-specific store, not here.
type monotonicClock struct {
	last atomic.Int64
}

var globalSeqClock monotonicClock

// NextGlobalSeq returns a strictly increasing 64-bit value, suitable as the
// `seq` column for tables that partition seq globally. Resolution is 1ms but
// every call advances by at least 1, so within a tight loop seq still
// increments.
func NextGlobalSeq() int64 {
	now := NowMillis()
	for {
		prev := globalSeqClock.last.Load()
		next := now
		if next <= prev {
			next = prev + 1
		}
		if globalSeqClock.last.CompareAndSwap(prev, next) {
			return next
		}
	}
}

// CanonicalETag computes the etag string used by every domain table.
//
// Format: "<version>-<sha256(canonical_record)[:8]>"
//
// canonicalRecord must be a value safely JSON-encodable. Callers are
// responsible for assembling the table-specific subset of fields that the
// design doc lists for each table (3.3, "etag canonical_record" table).
//
// This helper guarantees:
//   - Map keys are sorted (canonical JSON), so re-encoding the same logical
//     record always yields the same digest regardless of struct field order.
//   - Floating-point and unsupported types fail loudly via the underlying
//     encoder rather than silently producing a divergent digest.
func CanonicalETag(version int, canonicalRecord any) (string, error) {
	if version < 1 {
		return "", fmt.Errorf("store: etag version must be >=1, got %d", version)
	}
	buf, err := canonicalJSON(canonicalRecord)
	if err != nil {
		return "", fmt.Errorf("store: canonicalize record: %w", err)
	}
	sum := sha256.Sum256(buf)
	return fmt.Sprintf("%d-%s", version, hex.EncodeToString(sum[:])[:8]), nil
}

// canonicalJSON marshals v using sorted keys at every nesting level. This is
// the same canonicalization JCS (RFC 8785) takes for object members; we don't
// need full RFC 8785 (number form, Unicode escapes) because all our etag
// inputs are strings and integers we control.
func canonicalJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	return marshalSorted(generic)
}

func marshalSorted(v any) ([]byte, error) {
	switch x := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b []byte
		b = append(b, '{')
		for i, k := range keys {
			if i > 0 {
				b = append(b, ',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			b = append(b, kb...)
			b = append(b, ':')
			vb, err := marshalSorted(x[k])
			if err != nil {
				return nil, err
			}
			b = append(b, vb...)
		}
		b = append(b, '}')
		return b, nil
	case []any:
		var b []byte
		b = append(b, '[')
		for i, el := range x {
			if i > 0 {
				b = append(b, ',')
			}
			eb, err := marshalSorted(el)
			if err != nil {
				return nil, err
			}
			b = append(b, eb...)
		}
		b = append(b, ']')
		return b, nil
	default:
		return json.Marshal(v)
	}
}

// SHA256Hex returns the lowercase hex sha256 digest of body. Used by
// agent_memory.body_sha256 and blob_refs.sha256.
func SHA256Hex(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// onceErr is a tiny utility wrapping sync.Once that propagates an error.
// Useful for one-shot initializers (e.g. lazy prepared-statement caches).
type onceErr struct {
	once sync.Once
	err  error
}

func (o *onceErr) Do(f func() error) error {
	o.once.Do(func() { o.err = f() })
	return o.err
}
