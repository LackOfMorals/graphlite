// Package graphlite_test contains property-based tests using pgregory.net/rapid.
// These tests generate random graphs and verify full round-trip fidelity via
// CREATE → MATCH cycles, exercising JSON encoding, label parsing, and the scope
// tracker.
//
// Run with:
//
//	CGO_ENABLED=0 go test -run TestRapid ./...
package graphlite_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	graphlite "github.com/LackOfMorals/graphlite/v2"
	"pgregory.net/rapid"
)

// ─────────────────────────────────────────────────────────────────────────────
// Generators
// ─────────────────────────────────────────────────────────────────────────────

// lowerLetters is the set of ASCII lowercase letters used to generate valid
// Cypher identifiers (labels and property keys).
var lowerLetters = func() []rune {
	r := make([]rune, 26)
	for i := range r {
		r[i] = rune('a' + i)
	}
	return r
}()

// cypherReservedWords is the set of lowercase Cypher keywords that the
// opencypher parser rejects when used as bare (unquoted) property key names.
// genPropKey skips any generated key that matches one of these strings so that
// property-based tests do not hit parser errors unrelated to the feature under
// test.
var cypherReservedWords = map[string]struct{}{
	"all": {}, "and": {}, "as": {}, "asc": {}, "ascending": {},
	"by": {}, "call": {}, "case": {}, "contains": {}, "create": {},
	"delete": {}, "desc": {}, "descending": {}, "detach": {}, "distinct": {},
	"do": {}, "else": {}, "end": {}, "exists": {}, "false": {},
	"for": {}, "foreach": {}, "in": {}, "is": {}, "limit": {},
	"mandatory": {}, "match": {}, "merge": {}, "not": {}, "null": {},
	"of": {}, "on": {}, "optional": {}, "or": {}, "order": {},
	"remove": {}, "return": {}, "schema": {}, "set": {}, "skip": {},
	"then": {}, "true": {}, "union": {}, "unique": {}, "unwind": {},
	"using": {}, "when": {}, "where": {}, "with": {}, "xor": {},
	"yield": {},
}

// alphanumChars is the set of ASCII letters (lower + upper) and digits used
// for the tail of identifiers.
var alphanumChars = func() []rune {
	var r []rune
	for i := 'a'; i <= 'z'; i++ {
		r = append(r, i)
	}
	for i := 'A'; i <= 'Z'; i++ {
		r = append(r, i)
	}
	for i := '0'; i <= '9'; i++ {
		r = append(r, i)
	}
	return r
}()

// genLabel generates a valid Cypher label name: starts with a lowercase letter,
// followed by 0–11 alphanumeric characters.
func genLabel(t *rapid.T) string {
	first := rapid.SampledFrom(lowerLetters).Draw(t, "labelFirst")
	restLen := rapid.IntRange(0, 11).Draw(t, "labelRestLen")
	sb := strings.Builder{}
	sb.WriteRune(first)
	for i := 0; i < restLen; i++ {
		sb.WriteRune(rapid.SampledFrom(alphanumChars).Draw(t, fmt.Sprintf("labelChar%d", i)))
	}
	return sb.String()
}

// genLabels generates between 1 and maxN unique label strings.
func genLabels(t *rapid.T, maxN int) []string {
	n := rapid.IntRange(1, maxN).Draw(t, "labelCount")
	seen := make(map[string]struct{}, n)
	var labels []string
	// Retry up to 3*n times to get unique labels; append an index suffix on
	// collision to guarantee termination.
	attempts := 0
	for len(labels) < n {
		l := genLabel(t)
		candidate := l
		if _, ok := seen[candidate]; ok {
			// Force uniqueness by appending the current label count as suffix.
			candidate = fmt.Sprintf("%s%d", l, len(labels))
		}
		if _, ok := seen[candidate]; ok {
			candidate = fmt.Sprintf("%s%d", l, attempts)
		}
		attempts++
		if _, ok := seen[candidate]; !ok {
			seen[candidate] = struct{}{}
			labels = append(labels, candidate)
		}
		if attempts > 5*n+10 {
			// Fallback: generate deterministic labels to avoid infinite loops.
			for len(labels) < n {
				lbl := fmt.Sprintf("lbl%d", len(labels))
				if _, ok := seen[lbl]; !ok {
					seen[lbl] = struct{}{}
					labels = append(labels, lbl)
				}
			}
			break
		}
	}
	return labels
}

// genPropKey generates a valid Cypher property key: 1–10 lowercase letters,
// excluding Cypher reserved words that the parser rejects as bare identifiers.
// Any generated key that is a reserved word gets a "k" prefix so it is never
// mistaken for a keyword (no Cypher keyword starts with "kk…" or "k<keyword>").
func genPropKey(t *rapid.T, suffix string) string {
	chars := rapid.SliceOfN(rapid.SampledFrom(lowerLetters), 1, 10).Draw(t, "propKeyChars"+suffix)
	sb := strings.Builder{}
	for _, r := range chars {
		sb.WriteRune(r)
	}
	key := sb.String()
	if _, reserved := cypherReservedWords[key]; reserved {
		key = "k" + key
	}
	return key
}

// genPropValue generates one of the supported property value types:
// string, int64, float64, bool, or nil (null).
// Avoids NaN/Inf floats and floats that lose precision through JSON round-trip.
func genPropValue(t *rapid.T, suffix string) any {
	choice := rapid.IntRange(0, 4).Draw(t, "propValueKind"+suffix)
	switch choice {
	case 0:
		return rapid.StringN(0, 20, -1).Draw(t, "propString"+suffix)
	case 1:
		return rapid.Int64Range(-1_000_000, 1_000_000).Draw(t, "propInt64"+suffix)
	case 2:
		// Generate a float64 that survives JSON serialization losslessly.
		// Use an integer-valued float (e.g. 3.0, -7.0) or a 2-decimal fraction
		// (e.g. 1.25 = 1 + 1/4) to avoid the JSON precision truncation issue.
		// We use an integer in [-9999, 9999] divided by 4 to get exact floats.
		iv := rapid.Int64Range(-9999, 9999).Draw(t, "propFloatInt"+suffix)
		return float64(iv) * 0.25
	case 3:
		return rapid.Bool().Draw(t, "propBool"+suffix)
	default: // case 4
		return nil
	}
}

// genProps generates a map[string]any with 0–5 entries. Keys are unique
// lowercase-letter identifiers; values cover all supported property types.
func genProps(t *rapid.T, prefix string) map[string]any {
	n := rapid.IntRange(0, 5).Draw(t, prefix+"propCount")
	props := make(map[string]any, n)
	usedKeys := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		// Generate a unique key.
		suffix := fmt.Sprintf("%s_%d", prefix, i)
		key := genPropKey(t, suffix)
		// Ensure uniqueness by appending index if needed.
		if _, ok := usedKeys[key]; ok {
			key = fmt.Sprintf("%s%d", key, i)
		}
		usedKeys[key] = struct{}{}
		props[key] = genPropValue(t, suffix)
	}
	return props
}

// nodeSpec is the input descriptor used to generate a node and then verify it
// after a round-trip through CREATE → MATCH.
type nodeSpec struct {
	labels []string
	props  map[string]any
}

// genNodeSpec generates a random node specification with 1–3 labels and 0–5
// properties.
func genNodeSpec(t *rapid.T, idx int) nodeSpec {
	pfx := fmt.Sprintf("node%d_", idx)
	return nodeSpec{
		labels: genLabels(t, 3),
		props:  genProps(t, pfx),
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// Helper: build CREATE Cypher from a nodeSpec
// ─────────────────────────────────────────────────────────────────────────────

// buildCreateCypher returns a CREATE statement and a params map for the given
// nodeSpec. Properties are passed as $pN parameters to avoid Cypher injection
// issues with special characters in string values.
func buildCreateCypher(spec nodeSpec) (string, map[string]any) {
	// Labels: :label1:label2:...
	sb := strings.Builder{}
	sb.WriteString("CREATE (n")
	for _, l := range spec.labels {
		sb.WriteString(":")
		sb.WriteString(l)
	}

	params := make(map[string]any)
	if len(spec.props) == 0 {
		sb.WriteString(")")
		return sb.String(), params
	}

	sb.WriteString(" {")
	first := true
	i := 0
	for k, v := range spec.props {
		if !first {
			sb.WriteString(", ")
		}
		first = false
		pname := fmt.Sprintf("p%d", i)
		sb.WriteString(k)
		sb.WriteString(": $")
		sb.WriteString(pname)
		params[pname] = v
		i++
	}
	sb.WriteString("})")
	return sb.String(), params
}

// ─────────────────────────────────────────────────────────────────────────────
// Helper: normalise a retrieved property map for comparison
// ─────────────────────────────────────────────────────────────────────────────

// normaliseProps converts a property map from the input spec to a canonical
// form for comparison. All values (including nil/null) are preserved because
// graphlite stores nil params as JSON null and retrieves them as nil.
func normaliseProps(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for k, v := range input {
		out[k] = v
	}
	return out
}

// propsMatch checks whether the retrieved property map matches the expected
// map after accounting for JSON-number normalisation. Returns empty string if
// they match, or a description of the first mismatch.
func propsMatch(expected, got map[string]any) string {
	for k, expVal := range expected {
		gotVal, ok := got[k]
		if !ok {
			return fmt.Sprintf("key %q missing from retrieved props", k)
		}
		if !propValEqual(expVal, gotVal) {
			return fmt.Sprintf("key %q: expected %T(%v), got %T(%v)", k, expVal, expVal, gotVal, gotVal)
		}
	}
	// Check for extra keys in got that are not in expected.
	for k := range got {
		if _, ok := expected[k]; !ok {
			return fmt.Sprintf("unexpected extra key %q in retrieved props", k)
		}
	}
	return ""
}

// propValEqual compares two property values with JSON-number normalisation.
// int64 and bool inputs may come back as float64(n) from JSON decoding because
// SQLite stores them as numbers.
func propValEqual(a, b any) bool {
	if a == b {
		return true
	}
	switch av := a.(type) {
	case int64:
		if bv, ok := b.(float64); ok {
			return float64(av) == bv
		}
	case float64:
		if bv, ok := b.(float64); ok {
			return av == bv
		}
		if bv, ok := b.(int64); ok {
			return av == float64(bv)
		}
	case bool:
		// SQLite stores bool as integer 0/1; JSON decodes integers as float64.
		if bv, ok := b.(float64); ok {
			if av {
				return bv == 1
			}
			return bv == 0
		}
	}
	return false
}

// ─────────────────────────────────────────────────────────────────────────────
// Property-based tests
// ─────────────────────────────────────────────────────────────────────────────

// TestRapid_NodeRoundTrip generates 1–20 random node specs, CREATEs each one,
// then MATCHes all nodes and verifies that every created node appears in the
// result with matching labels and properties.
func TestRapid_NodeRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ctx := context.Background()

		db, err := graphlite.Open(":memory:")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer db.Close(context.Background()) //nolint:errcheck

		n := rapid.IntRange(1, 20).Draw(t, "nodeCount")
		specs := make([]nodeSpec, n)
		for i := range specs {
			specs[i] = genNodeSpec(t, i)
		}

		// CREATE each node.
		for i, spec := range specs {
			cypher, params := buildCreateCypher(spec)
			qr, err := db.RunQuery(ctx, cypher, params)
			if err != nil {
				t.Fatalf("CREATE node[%d] %q: %v", i, cypher, err)
			}
			if _, err := qr.Consume(ctx); err != nil {
				t.Fatalf("consume CREATE[%d]: %v", i, err)
			}
		}

		// MATCH all nodes and collect them.
		qr, err := db.RunQuery(ctx, "MATCH (n) RETURN n", nil)
		if err != nil {
			t.Fatalf("MATCH (n): %v", err)
		}
		records, err := qr.Collect(ctx)
		if err != nil {
			t.Fatalf("collect MATCH result: %v", err)
		}

		if len(records) != n {
			t.Fatalf("expected %d nodes, got %d", n, len(records))
		}

		// Verify each spec against the retrieved nodes. Since SQLite returns nodes
		// in insertion order (AUTOINCREMENT), we can match by index.
		nodes := make([]*graphlite.Node, 0, len(records))
		for _, rec := range records {
			v, ok := rec.Get("n")
			if !ok {
				t.Fatalf("record missing key 'n'")
			}
			node, ok := v.(*graphlite.Node)
			if !ok {
				t.Fatalf("expected *graphlite.Node, got %T", v)
			}
			nodes = append(nodes, node)
		}

		for i, spec := range specs {
			node := nodes[i]

			// Check labels (order-independent set equality).
			if !labelSetsEqual(spec.labels, node.Labels) {
				t.Fatalf("node[%d] labels mismatch: expected %v, got %v", i, spec.labels, node.Labels)
			}

			// Check properties (with JSON-number normalisation).
			expected := normaliseProps(spec.props)
			if msg := propsMatch(expected, node.Props); msg != "" {
				t.Fatalf("node[%d] props mismatch: %s (input: %v, retrieved: %v)",
					i, msg, spec.props, node.Props)
			}
		}
	})
}

// TestRapid_LabelRoundTrip generates nodes with up to 5 labels, CREATEs them,
// then MATCHes by each individual label to confirm multi-label semantics and
// round-trip fidelity.
func TestRapid_LabelRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ctx := context.Background()

		db, err := graphlite.Open(":memory:")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer db.Close(context.Background()) //nolint:errcheck

		// Generate a node with 1–5 labels.
		spec := nodeSpec{
			labels: genLabels(t, 5),
			props:  genProps(t, "lbl_"),
		}

		// CREATE the node.
		cypher, params := buildCreateCypher(spec)
		qr, err := db.RunQuery(ctx, cypher, params)
		if err != nil {
			t.Fatalf("CREATE %q: %v", cypher, err)
		}
		if _, err := qr.Consume(ctx); err != nil {
			t.Fatalf("consume CREATE: %v", err)
		}

		// MATCH the node back and verify labels survive the round-trip.
		qr2, err := db.RunQuery(ctx, "MATCH (n) RETURN n", nil)
		if err != nil {
			t.Fatalf("MATCH (n): %v", err)
		}
		records2, err := qr2.Collect(ctx)
		if err != nil {
			t.Fatalf("collect MATCH result: %v", err)
		}
		if len(records2) != 1 {
			t.Fatalf("expected 1 record, got %d", len(records2))
		}

		v, ok := records2[0].Get("n")
		if !ok {
			t.Fatalf("record missing key 'n'")
		}
		node, ok := v.(*graphlite.Node)
		if !ok {
			t.Fatalf("expected *graphlite.Node, got %T", v)
		}

		// All labels in the spec must be present on the retrieved node.
		if !labelSetsEqual(spec.labels, node.Labels) {
			t.Fatalf("label round-trip failed: expected %v, got %v", spec.labels, node.Labels)
		}

		// MATCH by each individual label: the node must be found every time.
		for _, lbl := range spec.labels {
			matchByLabel := fmt.Sprintf("MATCH (n:%s) RETURN n", lbl)
			qr3, err := db.RunQuery(ctx, matchByLabel, nil)
			if err != nil {
				t.Fatalf("MATCH by label %q: %v", lbl, err)
			}
			recs3, err := qr3.Collect(ctx)
			if err != nil {
				t.Fatalf("collect MATCH by label %q: %v", lbl, err)
			}
			if len(recs3) == 0 {
				t.Fatalf("MATCH (n:%s) returned no results; node has labels %v", lbl, node.Labels)
			}
		}
	})
}

// TestRapid_JSONImportRoundTrip generates a small graph (1–30 nodes, 0–20
// edges), imports it via graphlite.Import(FormatJSON), then verifies node and
// edge counts via MATCH queries.
func TestRapid_JSONImportRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ctx := context.Background()

		db, err := graphlite.Open(":memory:")
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer db.Close(context.Background()) //nolint:errcheck

		nodeCount := rapid.IntRange(1, 30).Draw(t, "nodeCount")
		edgeCount := rapid.IntRange(0, 20).Draw(t, "edgeCount")

		// Build the JSON import document.
		type importNode struct {
			ID     string         `json:"id"`
			Labels []string       `json:"labels"`
			Props  map[string]any `json:"props"`
		}
		type importEdge struct {
			Type    string         `json:"type"`
			StartID string         `json:"startId"`
			EndID   string         `json:"endId"`
			Props   map[string]any `json:"props"`
		}
		type importGraph struct {
			Nodes []importNode `json:"nodes"`
			Edges []importEdge `json:"edges"`
		}

		jnodes := make([]importNode, nodeCount)
		for i := range jnodes {
			labels := genLabels(t, 3)
			props := genPropsJSONSafe(t, fmt.Sprintf("imp%d_", i))
			jnodes[i] = importNode{
				ID:     fmt.Sprintf("n%d", i),
				Labels: labels,
				Props:  props,
			}
		}

		jedges := make([]importEdge, edgeCount)
		for i := range jedges {
			startIdx := rapid.IntRange(0, nodeCount-1).Draw(t, fmt.Sprintf("edgeStart%d", i))
			endIdx := rapid.IntRange(0, nodeCount-1).Draw(t, fmt.Sprintf("edgeEnd%d", i))
			jedges[i] = importEdge{
				Type:    "RELATES",
				StartID: fmt.Sprintf("n%d", startIdx),
				EndID:   fmt.Sprintf("n%d", endIdx),
				Props:   map[string]any{},
			}
		}

		graph := importGraph{Nodes: jnodes, Edges: jedges}

		// Serialise to JSON.
		jsonBytes, err := json.Marshal(graph)
		if err != nil {
			t.Fatalf("marshal test graph: %v", err)
		}

		// Import.
		if err := db.Import(ctx, strings.NewReader(string(jsonBytes)), graphlite.FormatJSON); err != nil {
			t.Fatalf("Import: %v", err)
		}

		// Verify node count.
		qr, err := db.RunQuery(ctx, "MATCH (n) RETURN n", nil)
		if err != nil {
			t.Fatalf("MATCH (n): %v", err)
		}
		nodeRecs, err := qr.Collect(ctx)
		if err != nil {
			t.Fatalf("collect MATCH result: %v", err)
		}
		if len(nodeRecs) != nodeCount {
			t.Fatalf("node count: expected %d, got %d", nodeCount, len(nodeRecs))
		}

		// Verify edge count.
		qr2, err := db.RunQuery(ctx, "MATCH (a)-[r]->(b) RETURN r", nil)
		if err != nil {
			t.Fatalf("MATCH ()-[r]->(): %v", err)
		}
		edgeRecs, err := qr2.Collect(ctx)
		if err != nil {
			t.Fatalf("collect edge MATCH result: %v", err)
		}
		if len(edgeRecs) != edgeCount {
			t.Fatalf("edge count: expected %d, got %d", edgeCount, len(edgeRecs))
		}
	})
}

// ─────────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────────

// labelSetsEqual returns true if a and b contain the same label strings
// (order-independent).
func labelSetsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]int, len(a))
	for _, l := range a {
		set[l]++
	}
	for _, l := range b {
		set[l]--
		if set[l] < 0 {
			return false
		}
	}
	return true
}

// genPropsJSONSafe generates a property map with only JSON-safe values (no
// nil/null, no NaN/Inf). Used for JSON import where null values need special
// handling.
func genPropsJSONSafe(t *rapid.T, prefix string) map[string]any {
	n := rapid.IntRange(0, 3).Draw(t, prefix+"safePropCount")
	props := make(map[string]any, n)
	usedKeys := make(map[string]struct{}, n)
	for i := 0; i < n; i++ {
		suffix := fmt.Sprintf("%s%d", prefix, i)
		key := genPropKey(t, suffix)
		if _, ok := usedKeys[key]; ok {
			key = fmt.Sprintf("%s%d", key, i)
		}
		usedKeys[key] = struct{}{}
		// Only safe values: string, int64, float64 (non-NaN/Inf), bool.
		choice := rapid.IntRange(0, 3).Draw(t, suffix+"safePropKind")
		switch choice {
		case 0:
			props[key] = rapid.StringN(0, 20, -1).Draw(t, suffix+"safeStr")
		case 1:
			props[key] = rapid.Int64Range(-1_000_000, 1_000_000).Draw(t, suffix+"safeInt")
		case 2:
			// Use exact quarter-integer floats to avoid JSON precision issues.
			iv := rapid.Int64Range(-9999, 9999).Draw(t, suffix+"safeFloat")
			props[key] = float64(iv) * 0.25
		case 3:
			props[key] = rapid.Bool().Draw(t, suffix+"safeBool")
		}
	}
	return props
}
