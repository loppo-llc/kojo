package agent

import (
	"database/sql"
	"time"

	_ "modernc.org/sqlite"
)

// MemoryResult is a search result from the FTS5 index.
type MemoryResult struct {
	Source  string `json:"source"`
	Content string `json:"content"`
	Rank    float64 `json:"rank"`
}

// MemoryIndex provides full-text search over agent memories using sqlite FTS5.
type MemoryIndex struct {
	db *sql.DB
}

// NewMemoryIndex opens or creates a FTS5 index at the given path.
func NewMemoryIndex(dbPath string) (*MemoryIndex, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}

	_, err = db.Exec(`CREATE VIRTUAL TABLE IF NOT EXISTS memories USING fts5(
		source,
		content,
		created_at
	)`)
	if err != nil {
		db.Close()
		return nil, err
	}

	return &MemoryIndex{db: db}, nil
}

// Insert adds a memory entry to the index.
func (idx *MemoryIndex) Insert(source, content string) error {
	_, err := idx.db.Exec(
		`INSERT INTO memories (source, content, created_at) VALUES (?, ?, ?)`,
		source, content, time.Now().UTC().Format(time.RFC3339),
	)
	return err
}

// Search queries the FTS5 index and returns matching results.
func (idx *MemoryIndex) Search(query string, limit int) ([]MemoryResult, error) {
	if query == "" || limit <= 0 {
		return nil, nil
	}

	rows, err := idx.db.Query(
		`SELECT source, content, rank FROM memories WHERE memories MATCH ? ORDER BY rank LIMIT ?`,
		query, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []MemoryResult
	for rows.Next() {
		var r MemoryResult
		if err := rows.Scan(&r.Source, &r.Content, &r.Rank); err != nil {
			continue
		}
		results = append(results, r)
	}
	return results, nil
}

// Close closes the database.
func (idx *MemoryIndex) Close() error {
	return idx.db.Close()
}
