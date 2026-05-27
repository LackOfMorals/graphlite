package graphlite

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/LackOfMorals/graphlite/store"
)

// Format identifies the file format accepted by Import.
type Format int

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
	FormatJSON Format = 1

	// FormatCSVNodes is the CSV bulk import format for node files.
	// The header row must contain at minimum ":ID" and ":LABEL" columns.
	// Additional columns become node properties; header format is "name:type"
	// where type is one of: string (default), int, float, bool.
	// Example header: :ID,:LABEL,name:string,age:int
	FormatCSVNodes Format = 2

	// FormatCSVEdges is the CSV bulk import format for edge (relationship) files.
	// The header row must contain ":START_ID", ":END_ID", and ":TYPE" columns.
	// Additional columns become edge properties.
	// Example header: :START_ID,:END_ID,:TYPE,weight:float
	FormatCSVEdges Format = 3
)

// ExportFormat identifies the file format produced by Export.
type ExportFormat int

const (
	// ExportFormatJSON exports the full graph as a JSON document compatible with
	// FormatJSON import:
	//   {"nodes": [...], "edges": [...]}
	ExportFormatJSON ExportFormat = 1

	// ExportFormatCSV exports all nodes followed by all edges in CSV format.
	// The nodes section uses the FormatCSVNodes layout (:ID,:LABEL,<props>).
	// The edges section uses the FormatCSVEdges layout (:START_ID,:END_ID,:TYPE,<props>).
	// The two sections are written back-to-back with no separator.
	ExportFormatCSV ExportFormat = 2
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
// Supported formats: FormatJSON, FormatCSVNodes, FormatCSVEdges.
//
// Security limits (FormatJSON only):
//   - Maximum input size: 500 MiB (returns ErrImportTooLarge if exceeded)
//   - Maximum JSON nesting depth: 20 (returns ErrImportDepthExceeded if exceeded)
func (d *DB) Import(ctx context.Context, r io.Reader, format Format) error {
	switch format {
	case FormatJSON:
		return d.importJSON(ctx, r)
	case FormatCSVNodes:
		return d.importCSVNodes(ctx, r)
	case FormatCSVEdges:
		return d.importCSVEdges(ctx, r)
	default:
		return fmt.Errorf("graphlite: import: unsupported format %d", format)
	}
}

// Export writes all nodes and/or edges from the database to w in the requested
// format.
//
// Supported formats: ExportFormatJSON, ExportFormatCSV.
//
// ExportFormatCSV writes nodes in FormatCSVNodes layout followed immediately by
// edges in FormatCSVEdges layout. Use two separate Export calls (with a bytes.Buffer
// or MultiWriter) if the two sections must be consumed independently.
func (d *DB) Export(ctx context.Context, w io.Writer, format ExportFormat) error {
	switch format {
	case ExportFormatJSON:
		return d.exportJSON(ctx, w)
	case ExportFormatCSV:
		if err := d.exportCSVNodes(ctx, w); err != nil {
			return err
		}
		return d.exportCSVEdges(ctx, w)
	default:
		return fmt.Errorf("graphlite: export: unsupported format %d", format)
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// JSON import
// ─────────────────────────────────────────────────────────────────────────────

// importJSON implements the JSON import path.
func (d *DB) importJSON(ctx context.Context, r io.Reader) (retErr error) {
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
	// Rollback is a no-op after a successful Commit (task-012), so this deferred
	// guard is always safe and eliminates per-error inline rollback calls.
	defer func() {
		if retErr != nil {
			_ = tx.Rollback()
		}
	}()

	// idMap maps the file-local node "id" string to its database integer ID.
	idMap := make(map[string]int64, len(doc.Nodes))

	// Insert nodes.
	for i, n := range doc.Nodes {
		propsJSON, err := marshalProps(n.Props)
		if err != nil {
			return fmt.Errorf("graphlite: import: node %d (%q) props: %w", i, n.ID, err)
		}
		dbID, err := tx.InsertNode(ctx, store.Labels(n.Labels), propsJSON)
		if err != nil {
			return fmt.Errorf("graphlite: import: insert node %d (%q): %w", i, n.ID, err)
		}
		if n.ID != "" {
			if _, dup := idMap[n.ID]; dup {
				return fmt.Errorf("graphlite: import: duplicate node id %q", n.ID)
			}
			idMap[n.ID] = dbID
		}
	}

	// Insert edges.
	for i, e := range doc.Edges {
		if e.Type == "" {
			return fmt.Errorf("graphlite: import: edge %d: missing type", i)
		}
		startDBID, ok := idMap[e.StartID]
		if !ok {
			return fmt.Errorf("graphlite: import: edge %d: unknown startId %q", i, e.StartID)
		}
		endDBID, ok := idMap[e.EndID]
		if !ok {
			return fmt.Errorf("graphlite: import: edge %d: unknown endId %q", i, e.EndID)
		}
		propsJSON, err := marshalProps(e.Props)
		if err != nil {
			return fmt.Errorf("graphlite: import: edge %d props: %w", i, err)
		}
		if _, err := tx.InsertEdge(ctx, e.Type, startDBID, endDBID, propsJSON); err != nil {
			return fmt.Errorf("graphlite: import: insert edge %d: %w", i, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("graphlite: import: commit: %w", err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// CSV import (nodes)
// ─────────────────────────────────────────────────────────────────────────────

// csvColKind identifies the role of a CSV column.
type csvColKind int

const (
	csvColID      csvColKind = iota // :ID
	csvColLabel                     // :LABEL
	csvColStartID                   // :START_ID
	csvColEndID                     // :END_ID
	csvColType                      // :TYPE
	csvColProp                      // regular property column (name:type)
)

// csvColDef describes one parsed CSV column header.
type csvColDef struct {
	kind     csvColKind
	propName string // for csvColProp: the property name
	propType string // for csvColProp: string|int|float|bool (default: string)
}

// parseCSVHeader parses a CSV header row into column definitions.
func parseCSVHeader(headers []string) ([]csvColDef, error) {
	defs := make([]csvColDef, len(headers))
	for i, h := range headers {
		h = strings.TrimSpace(h)
		switch h {
		case ":ID":
			defs[i] = csvColDef{kind: csvColID}
		case ":LABEL":
			defs[i] = csvColDef{kind: csvColLabel}
		case ":START_ID":
			defs[i] = csvColDef{kind: csvColStartID}
		case ":END_ID":
			defs[i] = csvColDef{kind: csvColEndID}
		case ":TYPE":
			defs[i] = csvColDef{kind: csvColType}
		default:
			// Property column: "name" or "name:type"
			parts := strings.SplitN(h, ":", 2)
			propName := strings.TrimSpace(parts[0])
			if propName == "" {
				return nil, fmt.Errorf("graphlite: csv import: empty column name at position %d", i)
			}
			propType := "string"
			if len(parts) == 2 {
				propType = strings.TrimSpace(parts[1])
			}
			defs[i] = csvColDef{kind: csvColProp, propName: propName, propType: propType}
		}
	}
	return defs, nil
}

// parseCSVPropValue converts a raw CSV string to a typed Go value based on propType.
func parseCSVPropValue(raw, propType string) (any, error) {
	if raw == "" {
		return nil, nil
	}
	switch propType {
	case "int", "integer", "long":
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("cannot parse %q as int: %w", raw, err)
		}
		return v, nil
	case "float", "double":
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			return nil, fmt.Errorf("cannot parse %q as float: %w", raw, err)
		}
		return v, nil
	case "bool", "boolean":
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("cannot parse %q as bool: %w", raw, err)
		}
		return v, nil
	default: // "string" or unrecognised type
		return raw, nil
	}
}

// importCSVNodes imports a node CSV file into the database atomically.
func (d *DB) importCSVNodes(ctx context.Context, r io.Reader) (retErr error) {
	cr := csv.NewReader(io.LimitReader(r, importMaxBytes+1))
	cr.TrimLeadingSpace = true

	// Read header row.
	headers, err := cr.Read()
	if err != nil {
		return fmt.Errorf("graphlite: csv node import: read header: %w", err)
	}
	defs, err := parseCSVHeader(headers)
	if err != nil {
		return err
	}

	// Validate that :ID and :LABEL columns are present.
	hasID, hasLabel := false, false
	for _, d := range defs {
		if d.kind == csvColID {
			hasID = true
		}
		if d.kind == csvColLabel {
			hasLabel = true
		}
	}
	if !hasID {
		return fmt.Errorf("graphlite: csv node import: missing :ID column")
	}
	if !hasLabel {
		return fmt.Errorf("graphlite: csv node import: missing :LABEL column")
	}

	// Read all data rows.
	rows, err := cr.ReadAll()
	if err != nil {
		return fmt.Errorf("graphlite: csv node import: read rows: %w", err)
	}

	// Open a transaction: all inserts are atomic.
	tx, err := d.st.Begin(ctx)
	if err != nil {
		return fmt.Errorf("graphlite: csv node import: begin transaction: %w", err)
	}
	// Rollback is a no-op after a successful Commit (task-012), so this deferred
	// guard is always safe and eliminates per-error inline rollback calls.
	defer func() {
		if retErr != nil {
			_ = tx.Rollback()
		}
	}()

	for rowIdx, row := range rows {
		if len(row) != len(defs) {
			return fmt.Errorf("graphlite: csv node import: row %d: expected %d columns, got %d", rowIdx+2, len(defs), len(row))
		}

		var nodeID, labelStr string
		props := make(map[string]any)

		for colIdx, def := range defs {
			val := strings.TrimSpace(row[colIdx])
			switch def.kind {
			case csvColID:
				nodeID = val
			case csvColLabel:
				labelStr = val
			case csvColProp:
				if val == "" {
					continue
				}
				pv, err := parseCSVPropValue(val, def.propType)
				if err != nil {
					return fmt.Errorf("graphlite: csv node import: row %d col %q: %w", rowIdx+2, def.propName, err)
				}
				if pv != nil {
					props[def.propName] = pv
				}
			}
		}

		if nodeID == "" {
			return fmt.Errorf("graphlite: csv node import: row %d: empty :ID value", rowIdx+2)
		}

		propsJSON, err := marshalProps(props)
		if err != nil {
			return fmt.Errorf("graphlite: csv node import: row %d props: %w", rowIdx+2, err)
		}

		if _, err := tx.InsertNode(ctx, store.DecodeLabels(labelStr), propsJSON); err != nil {
			return fmt.Errorf("graphlite: csv node import: row %d insert: %w", rowIdx+2, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("graphlite: csv node import: commit: %w", err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// CSV import (edges) — requires a pre-existing node idMap from a prior node CSV
// import. Since Import is called per-file, edges reference :ID values that must
// already be in the database. We look them up by scanning the nodes table.
// ─────────────────────────────────────────────────────────────────────────────

// importCSVEdges imports an edge CSV file into the database atomically.
// The :START_ID and :END_ID values must match node row IDs already in the DB
// (i.e. the integer primary keys stored as ElementId strings).
func (d *DB) importCSVEdges(ctx context.Context, r io.Reader) (retErr error) {
	cr := csv.NewReader(io.LimitReader(r, importMaxBytes+1))
	cr.TrimLeadingSpace = true

	// Read header row.
	headers, err := cr.Read()
	if err != nil {
		return fmt.Errorf("graphlite: csv edge import: read header: %w", err)
	}
	defs, err := parseCSVHeader(headers)
	if err != nil {
		return err
	}

	// Validate required columns.
	hasStart, hasEnd, hasType := false, false, false
	for _, d := range defs {
		switch d.kind {
		case csvColStartID:
			hasStart = true
		case csvColEndID:
			hasEnd = true
		case csvColType:
			hasType = true
		}
	}
	if !hasStart {
		return fmt.Errorf("graphlite: csv edge import: missing :START_ID column")
	}
	if !hasEnd {
		return fmt.Errorf("graphlite: csv edge import: missing :END_ID column")
	}
	if !hasType {
		return fmt.Errorf("graphlite: csv edge import: missing :TYPE column")
	}

	// Read all data rows.
	rows, err := cr.ReadAll()
	if err != nil {
		return fmt.Errorf("graphlite: csv edge import: read rows: %w", err)
	}

	// Open a transaction: all inserts are atomic.
	tx, err := d.st.Begin(ctx)
	if err != nil {
		return fmt.Errorf("graphlite: csv edge import: begin transaction: %w", err)
	}
	// Rollback is a no-op after a successful Commit (task-012), so this deferred
	// guard is always safe and eliminates per-error inline rollback calls.
	defer func() {
		if retErr != nil {
			_ = tx.Rollback()
		}
	}()

	for rowIdx, row := range rows {
		if len(row) != len(defs) {
			return fmt.Errorf("graphlite: csv edge import: row %d: expected %d columns, got %d", rowIdx+2, len(defs), len(row))
		}

		var startIDStr, endIDStr, edgeType string
		props := make(map[string]any)

		for colIdx, def := range defs {
			val := strings.TrimSpace(row[colIdx])
			switch def.kind {
			case csvColStartID:
				startIDStr = val
			case csvColEndID:
				endIDStr = val
			case csvColType:
				edgeType = val
			case csvColProp:
				if val == "" {
					continue
				}
				pv, err := parseCSVPropValue(val, def.propType)
				if err != nil {
					return fmt.Errorf("graphlite: csv edge import: row %d col %q: %w", rowIdx+2, def.propName, err)
				}
				if pv != nil {
					props[def.propName] = pv
				}
			}
		}

		if edgeType == "" {
			return fmt.Errorf("graphlite: csv edge import: row %d: empty :TYPE value", rowIdx+2)
		}

		startID, err := strconv.ParseInt(startIDStr, 10, 64)
		if err != nil {
			return fmt.Errorf("graphlite: csv edge import: row %d: invalid :START_ID %q: %w", rowIdx+2, startIDStr, err)
		}
		endID, err := strconv.ParseInt(endIDStr, 10, 64)
		if err != nil {
			return fmt.Errorf("graphlite: csv edge import: row %d: invalid :END_ID %q: %w", rowIdx+2, endIDStr, err)
		}

		// Verify that the referenced nodes exist.
		if _, err := tx.GetNode(ctx, startID); err != nil {
			return fmt.Errorf("graphlite: csv edge import: row %d: start node %d not found: %w", rowIdx+2, startID, err)
		}
		if _, err := tx.GetNode(ctx, endID); err != nil {
			return fmt.Errorf("graphlite: csv edge import: row %d: end node %d not found: %w", rowIdx+2, endID, err)
		}

		propsJSON, err := marshalProps(props)
		if err != nil {
			return fmt.Errorf("graphlite: csv edge import: row %d props: %w", rowIdx+2, err)
		}

		if _, err := tx.InsertEdge(ctx, edgeType, startID, endID, propsJSON); err != nil {
			return fmt.Errorf("graphlite: csv edge import: row %d insert: %w", rowIdx+2, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("graphlite: csv edge import: commit: %w", err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// JSON export
// ─────────────────────────────────────────────────────────────────────────────

// exportJSONNode is the JSON schema for a single exported node.
type exportJSONNode struct {
	ID     string         `json:"id"`
	Labels []string       `json:"labels"`
	Props  map[string]any `json:"props"`
}

// exportJSONEdge is the JSON schema for a single exported edge.
type exportJSONEdge struct {
	ID      string         `json:"id"`
	Type    string         `json:"type"`
	StartID string         `json:"startId"`
	EndID   string         `json:"endId"`
	Props   map[string]any `json:"props"`
}

// exportJSONDocument is the top-level JSON export schema.
type exportJSONDocument struct {
	Nodes []exportJSONNode `json:"nodes"`
	Edges []exportJSONEdge `json:"edges"`
}

// exportJSON writes the full graph as a JSON document to w.
func (d *DB) exportJSON(ctx context.Context, w io.Writer) error {
	nodes, err := d.st.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("graphlite: export json: list nodes: %w", err)
	}
	edges, err := d.st.ListEdges(ctx)
	if err != nil {
		return fmt.Errorf("graphlite: export json: list edges: %w", err)
	}

	doc := exportJSONDocument{
		Nodes: make([]exportJSONNode, 0, len(nodes)),
		Edges: make([]exportJSONEdge, 0, len(edges)),
	}

	for _, n := range nodes {
		props, err := unmarshalProps(n.Props)
		if err != nil {
			return fmt.Errorf("graphlite: export json: node %d props: %w", n.ID, err)
		}
		labels := []string(n.Labels)
		doc.Nodes = append(doc.Nodes, exportJSONNode{
			ID:     strconv.FormatInt(n.ID, 10),
			Labels: labels,
			Props:  props,
		})
	}

	for _, e := range edges {
		props, err := unmarshalProps(e.Props)
		if err != nil {
			return fmt.Errorf("graphlite: export json: edge %d props: %w", e.ID, err)
		}
		doc.Edges = append(doc.Edges, exportJSONEdge{
			ID:      strconv.FormatInt(e.ID, 10),
			Type:    e.Type,
			StartID: strconv.FormatInt(e.StartID, 10),
			EndID:   strconv.FormatInt(e.EndID, 10),
			Props:   props,
		})
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("graphlite: export json: encode: %w", err)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// CSV export (nodes)
// ─────────────────────────────────────────────────────────────────────────────

// exportCSVNodes writes all nodes as CSV to w.
// The header is: :ID,:LABEL,<sorted property keys>
// Property values are written as their JSON representation.
func (d *DB) exportCSVNodes(ctx context.Context, w io.Writer) error {
	nodes, err := d.st.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("graphlite: export csv nodes: list nodes: %w", err)
	}

	// Collect all property keys across all nodes to build a stable header.
	propKeySet := make(map[string]struct{})
	nodeProps := make([]map[string]any, len(nodes))
	for i, n := range nodes {
		props, err := unmarshalProps(n.Props)
		if err != nil {
			return fmt.Errorf("graphlite: export csv nodes: node %d props: %w", n.ID, err)
		}
		nodeProps[i] = props
		for k := range props {
			propKeySet[k] = struct{}{}
		}
	}

	propKeys := sortedKeys(propKeySet)

	cw := csv.NewWriter(w)

	// Write header.
	header := make([]string, 0, 2+len(propKeys))
	header = append(header, ":ID", ":LABEL")
	for _, k := range propKeys {
		header = append(header, k+":string")
	}
	if err := cw.Write(header); err != nil {
		return fmt.Errorf("graphlite: export csv nodes: write header: %w", err)
	}

	// Write rows.
	for i, n := range nodes {
		row := make([]string, 0, 2+len(propKeys))
		row = append(row, strconv.FormatInt(n.ID, 10), n.Labels.Encode())
		props := nodeProps[i]
		for _, k := range propKeys {
			v, ok := props[k]
			if !ok || v == nil {
				row = append(row, "")
				continue
			}
			row = append(row, anyToString(v))
		}
		if err := cw.Write(row); err != nil {
			return fmt.Errorf("graphlite: export csv nodes: write row: %w", err)
		}
	}

	cw.Flush()
	return cw.Error()
}

// ─────────────────────────────────────────────────────────────────────────────
// CSV export (edges)
// ─────────────────────────────────────────────────────────────────────────────

// exportCSVEdges writes all edges as CSV to w.
// The header is: :START_ID,:END_ID,:TYPE,<sorted property keys>
func (d *DB) exportCSVEdges(ctx context.Context, w io.Writer) error {
	edges, err := d.st.ListEdges(ctx)
	if err != nil {
		return fmt.Errorf("graphlite: export csv edges: list edges: %w", err)
	}

	// Collect all property keys across all edges to build a stable header.
	propKeySet := make(map[string]struct{})
	edgeProps := make([]map[string]any, len(edges))
	for i, e := range edges {
		props, err := unmarshalProps(e.Props)
		if err != nil {
			return fmt.Errorf("graphlite: export csv edges: edge %d props: %w", e.ID, err)
		}
		edgeProps[i] = props
		for k := range props {
			propKeySet[k] = struct{}{}
		}
	}

	propKeys := sortedKeys(propKeySet)

	cw := csv.NewWriter(w)

	// Write header.
	header := make([]string, 0, 3+len(propKeys))
	header = append(header, ":START_ID", ":END_ID", ":TYPE")
	for _, k := range propKeys {
		header = append(header, k+":string")
	}
	if err := cw.Write(header); err != nil {
		return fmt.Errorf("graphlite: export csv edges: write header: %w", err)
	}

	// Write rows.
	for i, e := range edges {
		row := make([]string, 0, 3+len(propKeys))
		row = append(row,
			strconv.FormatInt(e.StartID, 10),
			strconv.FormatInt(e.EndID, 10),
			e.Type,
		)
		props := edgeProps[i]
		for _, k := range propKeys {
			v, ok := props[k]
			if !ok || v == nil {
				row = append(row, "")
				continue
			}
			row = append(row, anyToString(v))
		}
		if err := cw.Write(row); err != nil {
			return fmt.Errorf("graphlite: export csv edges: write row: %w", err)
		}
	}

	cw.Flush()
	return cw.Error()
}

// ─────────────────────────────────────────────────────────────────────────────
// Shared helpers
// ─────────────────────────────────────────────────────────────────────────────

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

// unmarshalProps decodes a JSON object string into a property map.
// An empty or "{}" string returns an empty (non-nil) map.
func unmarshalProps(propsJSON string) (map[string]any, error) {
	m := make(map[string]any)
	if propsJSON == "" || propsJSON == "{}" {
		return m, nil
	}
	if err := json.Unmarshal([]byte(propsJSON), &m); err != nil {
		return nil, err
	}
	return m, nil
}


// anyToString converts a property value to its string representation for CSV output.
func anyToString(v any) string {
	switch val := v.(type) {
	case string:
		return val
	case bool:
		return strconv.FormatBool(val)
	case int64:
		return strconv.FormatInt(val, 10)
	case float64:
		// Avoid scientific notation for common integer-valued floats.
		if val == float64(int64(val)) {
			return strconv.FormatInt(int64(val), 10)
		}
		return strconv.FormatFloat(val, 'f', -1, 64)
	case json.Number:
		return val.String()
	default:
		// Fallback: JSON-encode the value.
		b, err := json.Marshal(val)
		if err != nil {
			return fmt.Sprintf("%v", val)
		}
		return string(b)
	}
}

// sortedKeys returns a sorted slice of keys from a string set map.
func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ─────────────────────────────────────────────────────────────────────────────
// JSON decode helpers (used by importJSON)
// ─────────────────────────────────────────────────────────────────────────────

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
