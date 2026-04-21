package agent

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
)

const (
	// DefaultEmbeddingModel is the Gemini embedding model used when no model
	// has been explicitly configured. Exported so HTTP handlers can present
	// the same fallback without duplicating the string.
	DefaultEmbeddingModel = "gemini-embedding-001"
	// defaultEmbeddingModel is kept for internal readability.
	defaultEmbeddingModel = DefaultEmbeddingModel
	embeddingAPI          = "https://generativelanguage.googleapis.com/v1beta/models/%s:embedContent?key=%s"
	batchEmbedAPI         = "https://generativelanguage.googleapis.com/v1beta/models/%s:batchEmbedContents?key=%s"
	maxEmbedInputChars    = 8000 // ~2048 tokens safe limit
	maxBatchSize          = 100  // Gemini batch limit
)

// getEmbedding generates an embedding vector for the given text using Gemini API.
func getEmbedding(apiKey, model, text string) ([]float32, error) {
	vecs, err := getBatchEmbeddings(apiKey, model, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("empty embedding response")
	}
	return vecs[0], nil
}

// getBatchEmbeddings generates embeddings for multiple texts in one API call.
// Automatically splits into batches of maxBatchSize.
func getBatchEmbeddings(apiKey, model string, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	all := make([][]float32, 0, len(texts))
	for i := 0; i < len(texts); i += maxBatchSize {
		end := i + maxBatchSize
		if end > len(texts) {
			end = len(texts)
		}
		batch, err := batchEmbedCall(apiKey, model, texts[i:end])
		if err != nil {
			return nil, err
		}
		all = append(all, batch...)
	}
	return all, nil
}

func batchEmbedCall(apiKey, model string, texts []string) ([][]float32, error) {
	url := fmt.Sprintf(batchEmbedAPI, model, apiKey)

	requests := make([]map[string]any, len(texts))
	for i, text := range texts {
		// Truncate long text to stay within token limit
		if len([]rune(text)) > maxEmbedInputChars {
			text = string([]rune(text)[:maxEmbedInputChars])
		}
		requests[i] = map[string]any{
			"model": "models/" + model,
			"content": map[string]any{
				"parts": []map[string]string{
					{"text": text},
				},
			},
		}
	}

	body, _ := json.Marshal(map[string]any{"requests": requests})
	// Use the shared Gemini HTTP client so embedding requests inherit a
	// timeout instead of inheriting http.DefaultClient's (no-timeout) default
	// and hanging indefinitely on a stuck upstream.
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, fmt.Errorf("embedding API build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := geminiHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("embedding API request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		data, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedding API error %d: %s", resp.StatusCode, data)
	}

	var result struct {
		Embeddings []struct {
			Values []float32 `json:"values"`
		} `json:"embeddings"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("embedding API decode: %w", err)
	}

	out := make([][]float32, len(result.Embeddings))
	for i, e := range result.Embeddings {
		out[i] = e.Values
	}
	return out, nil
}

// encodeEmbedding converts a float32 slice to a little-endian byte slice.
func encodeEmbedding(v []float32) []byte {
	buf := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// decodeEmbedding converts a little-endian byte slice to a float32 slice.
func decodeEmbedding(data []byte) []float32 {
	n := len(data) / 4
	v := make([]float32, n)
	for i := 0; i < n; i++ {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(data[i*4:]))
	}
	return v
}

// cosineSimilarity computes the cosine similarity between two vectors.
func cosineSimilarity(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB))
}

// contentHash returns a hex-encoded SHA-256 prefix for dedup.
func contentHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:16])
}
