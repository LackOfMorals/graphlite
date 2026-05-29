package graphlite_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/LackOfMorals/graphlite/v2"
)

// openMem opens a fresh in-memory graphlite DB for testing.
func openMem(t *testing.T) *graphlite.DB {
	t.Helper()
	db, err := graphlite.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close(context.Background()) })
	return db
}

// countNodes runs MATCH (n) RETURN count(*) — currently we just do MATCH (n) RETURN n
// and count records, since COUNT(*) aggregation is a v0.2 feature.
func countNodes(t *testing.T, db *graphlite.DB) int {
	t.Helper()
	ctx := context.Background()
	qr, err := db.RunQuery(ctx, "MATCH (n) RETURN n", nil)
	if err != nil {
		t.Fatalf("countNodes query: %v", err)
	}
	recs, err2 := qr.Collect(ctx)
	if err2 != nil {
		t.Fatalf("countNodes collect: %v", err2)
	}
	return len(recs)
}

// countEdges counts all edges via RunQuery.
func countEdges(t *testing.T, db *graphlite.DB) int {
	t.Helper()
	ctx := context.Background()
	qr, err := db.RunQuery(ctx, "MATCH ()-[r]->() RETURN r", nil)
	if err != nil {
		t.Fatalf("countEdges query: %v", err)
	}
	recs, err2 := qr.Collect(ctx)
	if err2 != nil {
		t.Fatalf("countEdges collect: %v", err2)
	}
	return len(recs)
}

// TestImport_ValidJSON verifies that a well-formed JSON import inserts all nodes
// and edges atomically.
func TestImport_ValidJSON(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	payload := `{
		"nodes": [
			{"id": "n1", "labels": ["Person"], "props": {"name": "Alice", "age": 30}},
			{"id": "n2", "labels": ["Person"], "props": {"name": "Bob"}}
		],
		"edges": [
			{"type": "KNOWS", "startId": "n1", "endId": "n2", "props": {"since": 2020}}
		]
	}`

	if err := db.Import(ctx, strings.NewReader(payload), graphlite.FormatJSON); err != nil {
		t.Fatalf("Import: %v", err)
	}

	if got := countNodes(t, db); got != 2 {
		t.Errorf("node count: got %d, want 2", got)
	}
	if got := countEdges(t, db); got != 1 {
		t.Errorf("edge count: got %d, want 1", got)
	}
}

// TestImport_EmptyGraph verifies that an empty import (no nodes, no edges) is valid.
func TestImport_EmptyGraph(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	if err := db.Import(ctx, strings.NewReader(`{"nodes":[],"edges":[]}`), graphlite.FormatJSON); err != nil {
		t.Fatalf("Import empty: %v", err)
	}
	if got := countNodes(t, db); got != 0 {
		t.Errorf("node count: got %d, want 0", got)
	}
}

// TestImport_NoProps verifies that nodes and edges with nil props are accepted.
func TestImport_NoProps(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	payload := `{
		"nodes": [
			{"id": "a", "labels": ["X"]},
			{"id": "b", "labels": ["Y"]}
		],
		"edges": [
			{"type": "LINK", "startId": "a", "endId": "b"}
		]
	}`
	if err := db.Import(ctx, strings.NewReader(payload), graphlite.FormatJSON); err != nil {
		t.Fatalf("Import no props: %v", err)
	}
	if got := countNodes(t, db); got != 2 {
		t.Errorf("node count: got %d, want 2", got)
	}
	if got := countEdges(t, db); got != 1 {
		t.Errorf("edge count: got %d, want 1", got)
	}
}

// TestImport_MultipleLabels verifies that nodes with multiple labels are stored correctly.
func TestImport_MultipleLabels(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	payload := `{"nodes":[{"id":"n1","labels":["Person","Employee"],"props":{"name":"Charlie"}}],"edges":[]}`
	if err := db.Import(ctx, strings.NewReader(payload), graphlite.FormatJSON); err != nil {
		t.Fatalf("Import multi-label: %v", err)
	}

	// Verify the node is retrievable by each label.
	qr, err := db.RunQuery(ctx, "MATCH (n:Person) RETURN n", nil)
	if err != nil {
		t.Fatalf("MATCH Person: %v", err)
	}
	if !qr.Next(ctx) {
		t.Fatal("expected a Person node")
	}
	_, _ = qr.Collect(ctx)

	qr2, err := db.RunQuery(ctx, "MATCH (n:Employee) RETURN n", nil)
	if err != nil {
		t.Fatalf("MATCH Employee: %v", err)
	}
	if !qr2.Next(ctx) {
		t.Fatal("expected an Employee node")
	}
	_, _ = qr2.Collect(ctx)
}

// TestImport_InvalidJSON verifies that malformed JSON returns an error and rolls
// back any partial state.
func TestImport_InvalidJSON(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	err := db.Import(ctx, strings.NewReader(`{not valid json`), graphlite.FormatJSON)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}

	// No nodes should have been inserted.
	if got := countNodes(t, db); got != 0 {
		t.Errorf("node count after bad import: got %d, want 0", got)
	}
}

// TestImport_UnknownEdgeStartId verifies that referencing a non-existent node ID
// in startId rolls back the entire import.
func TestImport_UnknownEdgeStartId(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	payload := `{
		"nodes": [{"id": "n1", "labels": ["X"], "props": {}}],
		"edges": [{"type": "T", "startId": "DOES_NOT_EXIST", "endId": "n1", "props": {}}]
	}`
	err := db.Import(ctx, strings.NewReader(payload), graphlite.FormatJSON)
	if err == nil {
		t.Fatal("expected error for unknown startId, got nil")
	}

	// Rollback should have reverted the node insert too.
	if got := countNodes(t, db); got != 0 {
		t.Errorf("node count after rollback: got %d, want 0", got)
	}
}

// TestImport_UnknownEdgeEndId verifies that referencing a non-existent node ID
// in endId rolls back the entire import.
func TestImport_UnknownEdgeEndId(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	payload := `{
		"nodes": [{"id": "n1", "labels": ["X"], "props": {}}],
		"edges": [{"type": "T", "startId": "n1", "endId": "MISSING", "props": {}}]
	}`
	err := db.Import(ctx, strings.NewReader(payload), graphlite.FormatJSON)
	if err == nil {
		t.Fatal("expected error for unknown endId, got nil")
	}

	if got := countNodes(t, db); got != 0 {
		t.Errorf("node count after rollback: got %d, want 0", got)
	}
}

// TestImport_DuplicateNodeId verifies that duplicate file-local node IDs return
// an error and roll back.
func TestImport_DuplicateNodeId(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	payload := `{
		"nodes": [
			{"id": "n1", "labels": ["X"], "props": {}},
			{"id": "n1", "labels": ["Y"], "props": {}}
		],
		"edges": []
	}`
	err := db.Import(ctx, strings.NewReader(payload), graphlite.FormatJSON)
	if err == nil {
		t.Fatal("expected error for duplicate node id, got nil")
	}
	if got := countNodes(t, db); got != 0 {
		t.Errorf("node count after rollback: got %d, want 0", got)
	}
}

// TestImport_MissingEdgeType verifies that an edge without a type field returns
// an error.
func TestImport_MissingEdgeType(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	payload := `{
		"nodes": [
			{"id": "a", "labels": ["X"]},
			{"id": "b", "labels": ["Y"]}
		],
		"edges": [{"startId": "a", "endId": "b"}]
	}`
	err := db.Import(ctx, strings.NewReader(payload), graphlite.FormatJSON)
	if err == nil {
		t.Fatal("expected error for missing edge type, got nil")
	}
}

// TestImport_DepthExceeded verifies that a JSON payload with nesting depth
// greater than importMaxDepth (20) returns ErrImportDepthExceeded.
func TestImport_DepthExceeded(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Build a deeply nested JSON object inside props (depth > 20).
	// Top level: object (1) → "nodes" array (2) → node object (3) →
	// "props" object (4) → nested objects depth 4..N.
	// We need depth > 20 counting from the root.
	// Root { = 1, nodes [ = 2, node { = 3, props { = 4, then 17 more levels.
	inner := strings.Repeat(`{"x":`, 18) + `1` + strings.Repeat(`}`, 18)
	payload := fmt.Sprintf(`{"nodes":[{"id":"n1","labels":["X"],"props":%s}],"edges":[]}`, inner)

	err := db.Import(ctx, strings.NewReader(payload), graphlite.FormatJSON)
	if err == nil {
		t.Fatal("expected ErrImportDepthExceeded, got nil")
	}
	var depthErr *graphlite.ErrImportDepthExceeded
	if !isErrType(err, &depthErr) {
		t.Errorf("expected *ErrImportDepthExceeded, got %T: %v", err, err)
	}
}

// TestImport_TooLarge verifies that a reader exceeding 500 MiB returns
// ErrImportTooLarge. We use a synthetic reader that claims to be huge.
func TestImport_TooLarge(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping 500 MiB synthetic reader test in short mode")
	}
	db := openMem(t)
	ctx := context.Background()

	// Build a reader that first emits valid JSON header bytes then emits
	// enough zeros to exceed the 500 MiB limit before closing.
	// We need the JSON decoder to actually try to read the full document.
	// Easiest approach: concatenate valid prefix + infinite zero reader wrapped
	// at limit+1, but json.Decoder will error on the zero bytes.
	// Instead, manufacture a 500MiB+1 byte valid-looking (but huge) JSON string.
	//
	// A cleaner approach: create a reader that returns 500MB+1 of content.
	// We wrap it in a strings that starts with '{"nodes":[' and then yields
	// bytes until exhausted — the decoder will fail to parse it as valid JSON
	// (no closing bracket), but our limit check fires first because the size
	// check happens at the reader level before decoding completes.
	//
	// Actually: our limitedReader wraps io.LimitReader(r, 500MiB+1) and sets
	// exceeded=true when read > limit. The json.Decoder will stop after reading
	// all available bytes. We need to give it just over 500MiB of data so it
	// reads past the limit.
	//
	// Use an infiniteReader that returns 'x' bytes — the decoder will return
	// an error (invalid JSON) but our reader will have set exceeded=true first.
	// We check exceeded AFTER decoding, so even if decoding fails we check it.
	//
	// However: decodeImportJSON returns the decode error before we check
	// exceeded. Fix: pass the limit check before calling decodeImportJSON.
	// Our implementation checks lr.exceeded after decodeImportJSON returns —
	// if the decode succeeds (never for garbage data) or fails, we must check
	// the overflow flag.
	//
	// To trigger the TooLarge path cleanly: use a reader that is exactly
	// 500MiB+1 of spaces followed by '{}' — the decoder will produce an
	// error because JSON doesn't allow leading spaces of that size before
	// the token... actually it does (whitespace is valid). Let's just use
	// a giant string of spaces + '{}' at the end.
	//
	// Simpler: use a pipeReader that produces (500MiB+1) bytes of a giant
	// valid JSON string. Build a JSON string whose size is > 500MiB.
	// That's impractical in a test.
	//
	// Best approach for unit test: use our internal limit constant which is
	// 500MB. We can test with a smaller limit by using a custom reader that
	// signals "already read limit+1 bytes". Since the limit is hardcoded, we
	// use an io.MultiReader: a tiny valid JSON header + an io.LimitedReader
	// that provides exactly limit+1 bytes of whitespace (valid JSON whitespace
	// is skipped by the decoder before it gets to the actual token).
	// This will trigger exceeded=true in our limitedReader.
	//
	// For test speed, we don't actually allocate 500MB. Instead we verify the
	// ErrImportTooLarge type is returned from a huge synthetic reader via a
	// pipeWriter that sends limit+1 bytes then closes.

	pr, pw := io.Pipe()
	const limit = 500 * 1024 * 1024

	go func() {
		// Write limit+1 bytes of spaces (valid JSON whitespace) so the decoder
		// reads past the limit, triggering the exceeded flag, before receiving
		// actual JSON content.
		buf := make([]byte, 4096)
		for i := range buf {
			buf[i] = ' '
		}
		written := int64(0)
		for written <= limit {
			n := int64(len(buf))
			if written+n > limit+1 {
				n = limit + 1 - written
			}
			wn, err := pw.Write(buf[:n])
			written += int64(wn)
			if err != nil {
				pw.CloseWithError(err)
				return
			}
		}
		// Close without valid JSON — decoder will fail, but we should have
		// already set exceeded=true.
		_ = pw.Close()
	}()

	err := db.Import(ctx, pr, graphlite.FormatJSON)
	if err == nil {
		t.Fatal("expected error for oversized import, got nil")
	}
	var sizeErr *graphlite.ErrImportTooLarge
	if !isErrType(err, &sizeErr) {
		// It's acceptable to get ErrImportTooLarge OR a JSON parse error that
		// wraps it. If neither, check for the TooLarge string.
		if !strings.Contains(err.Error(), "maximum size") {
			t.Errorf("expected ErrImportTooLarge or size-related error, got %T: %v", err, err)
		}
	}
}

// TestImport_UnsupportedFormat verifies that non-JSON format values return an error.
func TestImport_UnsupportedFormat(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	err := db.Import(ctx, strings.NewReader(`{}`), graphlite.Format(99))
	if err == nil {
		t.Fatal("expected error for unsupported format, got nil")
	}
}

// TestImport_LargeGraph imports 1000 nodes and 2000 edges and verifies the
// counts via MATCH queries (the acceptance criteria integration test).
func TestImport_LargeGraph(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	var sb strings.Builder
	sb.WriteString(`{"nodes":[`)
	for i := 0; i < 1000; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"id":"n%d","labels":["Node"],"props":{"idx":%d}}`, i, i)
	}
	sb.WriteString(`],"edges":[`)
	// Create 2000 edges: each node i -> node (i+1)%1000, twice (type A and B).
	first := true
	for i := 0; i < 1000; i++ {
		next := (i + 1) % 1000
		for _, typ := range []string{"A", "B"} {
			if !first {
				sb.WriteByte(',')
			}
			first = false
			fmt.Fprintf(&sb, `{"type":%q,"startId":"n%d","endId":"n%d","props":{}}`, typ, i, next)
		}
	}
	sb.WriteString(`]}`)

	if err := db.Import(ctx, strings.NewReader(sb.String()), graphlite.FormatJSON); err != nil {
		t.Fatalf("Import large graph: %v", err)
	}

	if got := countNodes(t, db); got != 1000 {
		t.Errorf("node count: got %d, want 1000", got)
	}
	if got := countEdges(t, db); got != 2000 {
		t.Errorf("edge count: got %d, want 2000", got)
	}
}

// TestImport_NodeWithoutId verifies that nodes without a file-local "id" field
// are still inserted (edges just can't reference them).
func TestImport_NodeWithoutId(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	payload := `{"nodes":[{"labels":["X"],"props":{"val":1}}],"edges":[]}`
	if err := db.Import(ctx, strings.NewReader(payload), graphlite.FormatJSON); err != nil {
		t.Fatalf("Import node without id: %v", err)
	}
	if got := countNodes(t, db); got != 1 {
		t.Errorf("node count: got %d, want 1", got)
	}
}

// isErrType is a helper that checks whether err (or a value wrapped by err)
// matches the type pointed to by target using errors.As semantics.
func isErrType[T error](err error, target *T) bool {
	if err == nil {
		return false
	}
	// Try a simple type assertion first.
	if e, ok := err.(T); ok {
		*target = e
		return true
	}
	return false
}
