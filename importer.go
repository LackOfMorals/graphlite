// This file implements the Import function for bulk-loading graph data.
package graphlite

import (
	"context"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// ImportFormat identifies the file format accepted by Import.
type ImportFormat int

const (
	// FormatJSON is the JSON bulk import format:
	//
	//	{
	//	  "nodes": [
	//	    {"id": "n1", "labels": ["Person"], "props": {"name": "Alice"}},
	//	    ...
	//	  ],
	//	  "edges": [
	//	    {"type": "KNOWS", "startId": "n1", "endId": "n2", "props": {}}
	//	  ]
	//	}
	//
	// Node "id" is a file-local string reference; it is not persisted. Edges
	// reference node "id" values via startId/endId.
	FormatJSON ImportFormat = 1
)

// importMaxBytes is the maximum number of bytes accepted from the reader.
// Imports exceeding this size return ErrImportTooLarge.
const importMaxBytes int64 = 500 * 1024 * 1024 // 500 MiB

// importMaxDepth is the maximum JSON nesting depth allowed during import.
// Inputs exceeding this depth return ErrImportDepthExceeded.
const importMaxDepth = 20

// importJSONNode is the JSON schema for a single node in the import file.
type importJSONNode struct {
	ID     string         `json:"id"`
	Labels []string       `json:"labels"`
	Props  map[string]any `json:"props"`
}

// importJSONEdge is the JSON schema for a single edge in the import file.
type importJSONEdge struct {
	Type    string         `json:"type"`
	StartID string         `json:"startId"`
	EndID   string         `json:"endId"`
	Props   map[string]any `json:"props"`
}

// importJSONDocument is the top-level JSON schema for the import file.
type importJSONDocument struct {
	Nodes []importJSONNode `json:"nodes"`
	Edges []importJSONEdge `json:"edges"`
}

// Import reads nodes and edges from r and inserts them atomically into the
// database. On any parse or constraint error the entire import is rolled back
// and an error is returned.
//
// format must be FormatJSON. Other values return an error.
//
// Security limits:
//   - Maximum input size: 500 MiB (returns ErrImportTooLarge if exceeded)
//   - Maximum JSON nesting depth: 20 (returns ErrImportDepthExceeded if exceeded)
func (d *DB) Import(ctx context.Context, r io.Reader, format ImportFormat) error {
	if format != FormatJSON {
		return fmt.Errorf("graphlite: import: unsupported format %d", format)
	}
	return d.importJSON(ctx, r)
}

// importJSON implements the JSON import path.
func (d *DB) importJSON(ctx context.Context, r io.Reader) error {
	// Enforce the 500 MiB size limit by wrapping with a counting reader.
	lr := &limitedReader{r: io.LimitReader(r, importMaxBytes+1), limit: importMaxBytes}

	doc, err := decodeImportJSON(lr)
	// Check the size limit before propagating any decode error: a truncated read
	// (EOF mid-document) caused by io.LimitReader is reported as a JSON parse
	// error, but the real root cause is the size limit being exceeded.
	if lr.exceeded {
		return &ErrImportTooLarge{MaxBytes: importMaxBytes}
	}
	if err != nil {
		return err
	}

	// Open a transaction: all inserts are atomic.
	tx, err := d.st.Begin(ctx)
	if err != nil {
		return fmt.Errorf("graphlite: import: begin transaction: %w", err)
	}

	// idMap maps the file-local node "id" string to its database integer ID.
	idMap := make(map[string]int64, len(doc.Nodes))

	// Insert nodes.
	for i, n := range doc.Nodes {
		labelsStr := strings.Join(n.Labels, ",")
		propsJSON, err := marshalProps(n.Props)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("graphlite: import: node %d (%q) props: %w", i, n.ID, err)
		}
		dbID, err := tx.InsertNode(ctx, labelsStr, propsJSON)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("graphlite: import: insert node %d (%q): %w", i, n.ID, err)
		}
		if n.ID != "" {
			if _, dup := idMap[n.ID]; dup {
				_ = tx.Rollback()
				return fmt.Errorf("graphlite: import: duplicate node id %q", n.ID)
			}
			idMap[n.ID] = dbID
		}
	}

	// Insert edges.
	for i, e := range doc.Edges {
		if e.Type == "" {
			_ = tx.Rollback()
			return fmt.Errorf("graphlite: import: edge %d: missing type", i)
		}
		startDBID, ok := idMap[e.StartID]
		if !ok {
			_ = tx.Rollback()
			return fmt.Errorf("graphlite: import: edge %d: unknown startId %q", i, e.StartID)
		}
		endDBID, ok := idMap[e.EndID]
		if !ok {
			_ = tx.Rollback()
			return fmt.Errorf("graphlite: import: edge %d: unknown endId %q", i, e.EndID)
		}
		propsJSON, err := marshalProps(e.Props)
		if err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("graphlite: import: edge %d props: %w", i, err)
		}
		if _, err := tx.InsertEdge(ctx, e.Type, startDBID, endDBID, propsJSON); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("graphlite: import: insert edge %d: %w", i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("graphlite: import: commit: %w", err)
	}
	return nil
}

// marshalProps encodes a property map as a JSON object string.
// A nil or empty map encodes as "{}".
func marshalProps(props map[string]any) (string, error) {
	if len(props) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(props)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// decodeImportJSON decodes the import document from r, enforcing the maximum
// JSON nesting depth (importMaxDepth). Returns ErrImportDepthExceeded when the
// depth limit is violated.
func decodeImportJSON(r io.Reader) (*importJSONDocument, error) {
	dec := json.NewDecoder(r)

	// Walk the token stream manually so we can track nesting depth.
	// We accumulate the raw JSON and then unmarshal it, rather than
	// trying to decode into the struct token-by-token.
	//
	// Strategy: scan for depth violations first using the token stream,
	// then re-decode the already-buffered bytes into the struct.
	// Since we need to buffer anyway (the reader may be streaming), we
	// decode into a raw json.RawMessage first, validate depth, then unmarshal.

	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("graphlite: import: JSON parse error: %w", err)
	}

	// Check depth of the decoded bytes.
	if err := checkJSONDepth(raw, importMaxDepth); err != nil {
		return nil, err
	}

	// Unmarshal into the document struct.
	var doc importJSONDocument
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("graphlite: import: JSON unmarshal error: %w", err)
	}
	return &doc, nil
}

// checkJSONDepth scans the raw JSON bytes and returns ErrImportDepthExceeded
// if the nesting depth of objects and arrays exceeds maxDepth.
func checkJSONDepth(data []byte, maxDepth int) error {
	depth := 0
	dec := json.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			// The data was already successfully decoded, so this shouldn't happen.
			return fmt.Errorf("graphlite: import: depth check error: %w", err)
		}
		switch d := tok.(type) {
		case json.Delim:
			if d == '{' || d == '[' {
				depth++
				if depth > maxDepth {
					return &ErrImportDepthExceeded{MaxDepth: maxDepth}
				}
			} else {
				depth--
			}
		}
	}
	return nil
}

// limitedReader wraps an io.Reader that has already been limited via
// io.LimitReader. It sets exceeded=true if the underlying reader delivered
// exactly limit+1 bytes (i.e. the real input was larger than limit).
type limitedReader struct {
	r        io.Reader
	limit    int64
	read     int64
	exceeded bool
}

func (l *limitedReader) Read(p []byte) (int, error) {
	n, err := l.r.Read(p)
	l.read += int64(n)
	if l.read > l.limit {
		l.exceeded = true
	}
	return n, err
}
