package graphlite_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/LackOfMorals/graphlite"
)

// ─────────────────────────────────────────────────────────────────────────────
// CSV node import tests
// ─────────────────────────────────────────────────────────────────────────────

func TestImport_CSVNodes_Basic(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	csv := ":ID,:LABEL,name:string,age:int\n1,Person,Alice,30\n2,Person,Bob,25\n"
	if err := db.Import(ctx, strings.NewReader(csv), graphlite.FormatCSVNodes); err != nil {
		t.Fatalf("Import CSV nodes: %v", err)
	}

	if got := countNodes(t, db); got != 2 {
		t.Errorf("node count: got %d, want 2", got)
	}
}

func TestImport_CSVNodes_MissingIDColumn(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	csv := ":LABEL,name:string\nPerson,Alice\n"
	err := db.Import(ctx, strings.NewReader(csv), graphlite.FormatCSVNodes)
	if err == nil {
		t.Fatal("expected error for missing :ID column, got nil")
	}
	if !strings.Contains(err.Error(), ":ID") {
		t.Errorf("error should mention :ID, got: %v", err)
	}
}

func TestImport_CSVNodes_MissingLABELColumn(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	csv := ":ID,name:string\n1,Alice\n"
	err := db.Import(ctx, strings.NewReader(csv), graphlite.FormatCSVNodes)
	if err == nil {
		t.Fatal("expected error for missing :LABEL column, got nil")
	}
	if !strings.Contains(err.Error(), ":LABEL") {
		t.Errorf("error should mention :LABEL, got: %v", err)
	}
}

func TestImport_CSVNodes_TypedProperties(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	csv := ":ID,:LABEL,score:float,active:bool\n1,Item,9.5,true\n"
	if err := db.Import(ctx, strings.NewReader(csv), graphlite.FormatCSVNodes); err != nil {
		t.Fatalf("Import CSV typed props: %v", err)
	}
	if got := countNodes(t, db); got != 1 {
		t.Errorf("node count: got %d, want 1", got)
	}
}

func TestImport_CSVNodes_Rollback(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Row 3 has a bad int value — should rollback node from row 1 and 2.
	csv := ":ID,:LABEL,age:int\n1,Person,30\n2,Person,25\n3,Person,notanint\n"
	err := db.Import(ctx, strings.NewReader(csv), graphlite.FormatCSVNodes)
	if err == nil {
		t.Fatal("expected error for bad int, got nil")
	}
	if got := countNodes(t, db); got != 0 {
		t.Errorf("node count after rollback: got %d, want 0", got)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CSV edge import tests
// ─────────────────────────────────────────────────────────────────────────────

func TestImport_CSVEdges_Basic(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// First import nodes so we have IDs 1 and 2 in the DB.
	nodeCsv := ":ID,:LABEL,name:string\n1,Person,Alice\n2,Person,Bob\n"
	if err := db.Import(ctx, strings.NewReader(nodeCsv), graphlite.FormatCSVNodes); err != nil {
		t.Fatalf("Import CSV nodes: %v", err)
	}

	// Now import edges referencing those IDs (1 and 2 are the auto-assigned IDs).
	// After importing with FormatCSVNodes the DB assigns IDs 1 and 2.
	edgeCsv := fmt.Sprintf(":START_ID,:END_ID,:TYPE\n%d,%d,KNOWS\n", 1, 2)
	if err := db.Import(ctx, strings.NewReader(edgeCsv), graphlite.FormatCSVEdges); err != nil {
		t.Fatalf("Import CSV edges: %v", err)
	}

	if got := countEdges(t, db); got != 1 {
		t.Errorf("edge count: got %d, want 1", got)
	}
}

func TestImport_CSVEdges_MissingStartID(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	csv := ":END_ID,:TYPE\n1,KNOWS\n"
	err := db.Import(ctx, strings.NewReader(csv), graphlite.FormatCSVEdges)
	if err == nil {
		t.Fatal("expected error for missing :START_ID column, got nil")
	}
}

func TestImport_CSVEdges_MissingEndID(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	csv := ":START_ID,:TYPE\n1,KNOWS\n"
	err := db.Import(ctx, strings.NewReader(csv), graphlite.FormatCSVEdges)
	if err == nil {
		t.Fatal("expected error for missing :END_ID column, got nil")
	}
}

func TestImport_CSVEdges_MissingType(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	csv := ":START_ID,:END_ID\n1,2\n"
	err := db.Import(ctx, strings.NewReader(csv), graphlite.FormatCSVEdges)
	if err == nil {
		t.Fatal("expected error for missing :TYPE column, got nil")
	}
}

func TestImport_CSVEdges_InvalidNodeRef(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// No nodes in DB — edge ref should fail with a clear error that
	// does not expose the raw SQLite FOREIGN KEY constraint message.
	csv := ":START_ID,:END_ID,:TYPE\n99,100,KNOWS\n"
	err := db.Import(ctx, strings.NewReader(csv), graphlite.FormatCSVEdges)
	if err == nil {
		t.Fatal("expected error for nonexistent node ref, got nil")
	}
	// The error must identify the bad node IDs without exposing the raw
	// SQLite constraint string.
	if !strings.Contains(err.Error(), "node not found") {
		t.Errorf("error should mention 'node not found', got: %v", err)
	}
	if strings.Contains(err.Error(), "FOREIGN KEY constraint") {
		t.Errorf("error should not expose raw SQLite constraint text, got: %v", err)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// JSON export tests
// ─────────────────────────────────────────────────────────────────────────────

func TestExport_JSON_RoundTrip(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	// Seed data.
	seed := `{"nodes":[
		{"id":"n1","labels":["Person"],"props":{"name":"Alice"}},
		{"id":"n2","labels":["Person"],"props":{"name":"Bob"}}
	],"edges":[
		{"type":"KNOWS","startId":"n1","endId":"n2","props":{"since":2020}}
	]}`
	if err := db.Import(ctx, strings.NewReader(seed), graphlite.FormatJSON); err != nil {
		t.Fatalf("seed import: %v", err)
	}

	// Export to JSON.
	var buf bytes.Buffer
	if err := db.Export(ctx, &buf, graphlite.ExportFormatJSON); err != nil {
		t.Fatalf("Export JSON: %v", err)
	}

	// Re-import into a fresh DB and verify counts.
	db2 := openMem(t)
	if err := db2.Import(ctx, &buf, graphlite.FormatJSON); err != nil {
		t.Fatalf("re-import from exported JSON: %v", err)
	}
	if got := countNodes(t, db2); got != 2 {
		t.Errorf("re-imported node count: got %d, want 2", got)
	}
	if got := countEdges(t, db2); got != 1 {
		t.Errorf("re-imported edge count: got %d, want 1", got)
	}
}

func TestExport_JSON_EmptyGraph(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	var buf bytes.Buffer
	if err := db.Export(ctx, &buf, graphlite.ExportFormatJSON); err != nil {
		t.Fatalf("Export JSON empty: %v", err)
	}

	// Decode and verify structure.
	var doc struct {
		Nodes []json.RawMessage `json:"nodes"`
		Edges []json.RawMessage `json:"edges"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("decode exported JSON: %v", err)
	}
	if len(doc.Nodes) != 0 {
		t.Errorf("nodes: got %d, want 0", len(doc.Nodes))
	}
	if len(doc.Edges) != 0 {
		t.Errorf("edges: got %d, want 0", len(doc.Edges))
	}
}

func TestExport_JSON_FieldStructure(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	seed := `{"nodes":[{"id":"n1","labels":["Person"],"props":{"name":"Alice"}}],"edges":[]}`
	if err := db.Import(ctx, strings.NewReader(seed), graphlite.FormatJSON); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var buf bytes.Buffer
	if err := db.Export(ctx, &buf, graphlite.ExportFormatJSON); err != nil {
		t.Fatalf("Export JSON: %v", err)
	}

	var doc struct {
		Nodes []struct {
			ID     string         `json:"id"`
			Labels []string       `json:"labels"`
			Props  map[string]any `json:"props"`
		} `json:"nodes"`
		Edges []json.RawMessage `json:"edges"`
	}
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(doc.Nodes) != 1 {
		t.Fatalf("node count: got %d, want 1", len(doc.Nodes))
	}
	n := doc.Nodes[0]
	if n.ID == "" {
		t.Error("node id should not be empty")
	}
	if len(n.Labels) != 1 || n.Labels[0] != "Person" {
		t.Errorf("labels: got %v, want [Person]", n.Labels)
	}
	if v, ok := n.Props["name"]; !ok || v != "Alice" {
		t.Errorf("props.name: got %v, want Alice", v)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// CSV export tests
// ─────────────────────────────────────────────────────────────────────────────

// TestExport_CSV_Basic verifies that ExportFormatCSV writes the node section
// (header :ID,:LABEL,...) followed by the edge section (header :START_ID,...).
func TestExport_CSV_Basic(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	seed := `{"nodes":[
		{"id":"n1","labels":["Person"],"props":{"name":"Alice"}},
		{"id":"n2","labels":["Person"],"props":{"name":"Bob"}}
	],"edges":[
		{"type":"KNOWS","startId":"n1","endId":"n2","props":{}}
	]}`
	if err := db.Import(ctx, strings.NewReader(seed), graphlite.FormatJSON); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var buf bytes.Buffer
	if err := db.Export(ctx, &buf, graphlite.ExportFormatCSV); err != nil {
		t.Fatalf("Export CSV: %v", err)
	}

	output := buf.String()
	// Nodes section headers.
	if !strings.Contains(output, ":ID") {
		t.Errorf("CSV output missing :ID column, got:\n%s", output)
	}
	if !strings.Contains(output, ":LABEL") {
		t.Errorf("CSV output missing :LABEL column, got:\n%s", output)
	}
	// Edges section headers.
	if !strings.Contains(output, ":START_ID") {
		t.Errorf("CSV output missing :START_ID column, got:\n%s", output)
	}
	if !strings.Contains(output, ":END_ID") {
		t.Errorf("CSV output missing :END_ID column, got:\n%s", output)
	}
	if !strings.Contains(output, ":TYPE") {
		t.Errorf("CSV output missing :TYPE column, got:\n%s", output)
	}
	if !strings.Contains(output, "KNOWS") {
		t.Errorf("CSV output missing KNOWS edge type, got:\n%s", output)
	}
	// node header + 2 node rows + edge header + 1 edge row = 5 lines (ignoring trailing newline).
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) != 5 {
		t.Errorf("expected 5 lines (node-header+2 nodes+edge-header+1 edge), got %d:\n%s", len(lines), output)
	}
}

// TestExport_CSV_NodesOnly verifies that ExportFormatCSV on a graph with no
// edges still includes both CSV sections (the edges section being header-only).
func TestExport_CSV_NodesOnly(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	seed := `{"nodes":[
		{"id":"n1","labels":["Person"],"props":{"name":"Alice"}},
		{"id":"n2","labels":["Person"],"props":{"name":"Bob"}}
	],"edges":[]}`
	if err := db.Import(ctx, strings.NewReader(seed), graphlite.FormatJSON); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var buf bytes.Buffer
	if err := db.Export(ctx, &buf, graphlite.ExportFormatCSV); err != nil {
		t.Fatalf("Export CSV nodes-only: %v", err)
	}

	output := buf.String()
	if !strings.Contains(output, ":ID") {
		t.Errorf("CSV output missing :ID column, got:\n%s", output)
	}
	if !strings.Contains(output, ":LABEL") {
		t.Errorf("CSV output missing :LABEL column, got:\n%s", output)
	}
	// node header + 2 node rows + edge header (no edge rows) = 4 lines.
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) != 4 {
		t.Errorf("expected 4 lines (node-header+2 nodes+edge-header), got %d:\n%s", len(lines), output)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Round-trip integration test (500-node graph)
// ─────────────────────────────────────────────────────────────────────────────

func TestExport_RoundTrip_500Nodes(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()

	const nodeCount = 500
	const edgeCount = 499

	// Build a JSON import payload: 500 nodes, 499 edges forming a chain.
	var sb strings.Builder
	sb.WriteString(`{"nodes":[`)
	for i := 0; i < nodeCount; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"id":"n%d","labels":["Node"],"props":{"idx":%d}}`, i, i)
	}
	sb.WriteString(`],"edges":[`)
	for i := 0; i < edgeCount; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"type":"NEXT","startId":"n%d","endId":"n%d","props":{}}`, i, i+1)
	}
	sb.WriteString(`]}`)

	if err := db.Import(ctx, strings.NewReader(sb.String()), graphlite.FormatJSON); err != nil {
		t.Fatalf("seed import: %v", err)
	}

	// Export as JSON.
	var exportBuf bytes.Buffer
	if err := db.Export(ctx, &exportBuf, graphlite.ExportFormatJSON); err != nil {
		t.Fatalf("Export JSON: %v", err)
	}

	// Re-import into a fresh DB and verify counts.
	db2 := openMem(t)
	if err := db2.Import(ctx, &exportBuf, graphlite.FormatJSON); err != nil {
		t.Fatalf("re-import from exported JSON: %v", err)
	}

	if got := countNodes(t, db2); got != nodeCount {
		t.Errorf("node count after round-trip: got %d, want %d", got, nodeCount)
	}
	if got := countEdges(t, db2); got != edgeCount {
		t.Errorf("edge count after round-trip: got %d, want %d", got, edgeCount)
	}
}

// TestExport_UnsupportedFormat verifies that Export returns an error for unknown formats.
func TestExport_UnsupportedFormat(t *testing.T) {
	db := openMem(t)
	ctx := context.Background()
	var buf bytes.Buffer
	err := db.Export(ctx, &buf, graphlite.ExportFormat(99))
	if err == nil {
		t.Fatal("expected error for unsupported export format, got nil")
	}
}
