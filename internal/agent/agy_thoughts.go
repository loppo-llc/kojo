package agent

import (
	"database/sql"
	"encoding/binary"
	"log/slog"
	"os"
	"path/filepath"
	"unicode/utf8"
)

// agy's stream-json output strips thinking text from step_update events
// (thinking_delta never arrives), but agy writes every step to its local
// conversation store as it runs:
//
//	~/.gemini/antigravity-cli/conversations/<conversation_id>.db
//	steps(idx PRIMARY KEY, step_type, ..., step_payload BLOB)
//
// An agent_response step (step_type 15) carries the model's thought
// summary in its protobuf payload at top-level field 20, subfield 3
// (both length-delimited; the inner one a UTF-8 string). kojo reads it
// from there so the UI can show thinking like other backends.

func agyConversationsDir() string {
	root := agyCLIDir()
	if root == "" {
		return ""
	}
	return filepath.Join(root, "conversations")
}

// protoLenField returns the payload of the first length-delimited field
// num in msg, or nil. Malformed input yields nil, never a panic.
func protoLenField(msg []byte, num uint64) []byte {
	for len(msg) > 0 {
		key, n := binary.Uvarint(msg)
		if n <= 0 {
			return nil
		}
		msg = msg[n:]
		switch key & 7 {
		case 0:
			_, n = binary.Uvarint(msg)
			if n <= 0 {
				return nil
			}
			msg = msg[n:]
		case 1:
			if len(msg) < 8 {
				return nil
			}
			msg = msg[8:]
		case 5:
			if len(msg) < 4 {
				return nil
			}
			msg = msg[4:]
		case 2:
			l, n := binary.Uvarint(msg)
			if n <= 0 || l > uint64(len(msg)-n) {
				return nil
			}
			v := msg[n : n+int(l)]
			msg = msg[n+int(l):]
			if key>>3 == num {
				return v
			}
		default:
			return nil
		}
	}
	return nil
}

// extractAgyThoughtFromPayload returns the thought summary stored in an
// agent_response step payload (field 20 -> subfield 3), or "".
func extractAgyThoughtFromPayload(payload []byte) string {
	inner := protoLenField(payload, 20)
	if inner == nil {
		return ""
	}
	t := protoLenField(inner, 3)
	if t == nil || !utf8.Valid(t) {
		return ""
	}
	return string(t)
}

// agyThoughtReader looks up thought summaries in agy's conversation store.
// The database is opened read-only on first use and reused for the turn.
type agyThoughtReader struct {
	dir    string
	logger *slog.Logger
	convID string
	db     *sql.DB
	failed bool
}

func newAgyThoughtReader(logger *slog.Logger) *agyThoughtReader {
	return &agyThoughtReader{dir: agyConversationsDir(), logger: logger}
}

// Lookup returns the thought of step idx in conversation convID. ok is
// false when the step is not readable yet (worth retrying later).
func (r *agyThoughtReader) Lookup(convID string, idx int) (thought string, ok bool) {
	if r == nil || r.dir == "" || !agyConversationIDPattern.MatchString(convID) || r.failed && r.convID == convID {
		return "", false
	}
	if r.db == nil || r.convID != convID {
		r.Close()
		r.convID = convID
		path := filepath.Join(r.dir, convID+".db")
		if _, err := os.Stat(path); err != nil {
			return "", false
		}
		db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(200)")
		if err != nil {
			r.failed = true
			r.logger.Debug("agy thoughts: open store", "err", err)
			return "", false
		}
		db.SetMaxOpenConns(1)
		r.db = db
	}
	var payload []byte
	if err := r.db.QueryRow(`SELECT step_payload FROM steps WHERE idx = ? AND step_type = 15`, idx).Scan(&payload); err != nil {
		if err != sql.ErrNoRows {
			r.logger.Debug("agy thoughts: read step", "idx", idx, "err", err)
		}
		return "", false
	}
	return extractAgyThoughtFromPayload(payload), true
}

func (r *agyThoughtReader) Close() {
	if r != nil && r.db != nil {
		_ = r.db.Close()
		r.db = nil
	}
	if r != nil {
		r.failed = false
	}
}
