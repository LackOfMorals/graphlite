package graphlite

import (
	"context"
	"fmt"
	"sort"
	"strings"

	neo4j "github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// CopyFrom imports all nodes and relationships from src into this database,
// appending to any existing data. src can be any [neo4j.Driver] — a real Neo4j
// instance or another graphlite [DriverCompat].
//
// The entire import runs inside a single graphlite transaction; if any error
// occurs all changes are rolled back and the database is unchanged.
func (d *DB) CopyFrom(ctx context.Context, src neo4j.Driver) error {
	nodeRes, err := neo4j.ExecuteQuery(ctx, src,
		"MATCH (n) RETURN n", nil, neo4j.EagerResultTransformer)
	if err != nil {
		return fmt.Errorf("graphlite: CopyFrom: query nodes: %w", err)
	}
	edgeRes, err := neo4j.ExecuteQuery(ctx, src,
		"MATCH ()-[r]->() RETURN r", nil, neo4j.EagerResultTransformer)
	if err != nil {
		return fmt.Errorf("graphlite: CopyFrom: query edges: %w", err)
	}

	tx, err := d.st.Begin(ctx)
	if err != nil {
		return fmt.Errorf("graphlite: CopyFrom: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// elementId → local graphlite ID
	idMap := make(map[string]int64, len(nodeRes.Records))

	for _, rec := range nodeRes.Records {
		n, ok := rec.Values[0].(neo4j.Node)
		if !ok {
			return fmt.Errorf("graphlite: CopyFrom: unexpected node value type %T", rec.Values[0])
		}
		propsJSON, err := marshalProps(n.Props)
		if err != nil {
			return fmt.Errorf("graphlite: CopyFrom: marshal node props: %w", err)
		}
		localID, err := tx.InsertNode(ctx, strings.Join(n.Labels, ","), propsJSON)
		if err != nil {
			return fmt.Errorf("graphlite: CopyFrom: insert node: %w", err)
		}
		idMap[n.ElementId] = localID
	}

	for _, rec := range edgeRes.Records {
		r, ok := rec.Values[0].(neo4j.Relationship)
		if !ok {
			return fmt.Errorf("graphlite: CopyFrom: unexpected relationship value type %T", rec.Values[0])
		}
		startID, ok := idMap[r.StartElementId]
		if !ok {
			return fmt.Errorf("graphlite: CopyFrom: relationship references unknown start node %q", r.StartElementId)
		}
		endID, ok := idMap[r.EndElementId]
		if !ok {
			return fmt.Errorf("graphlite: CopyFrom: relationship references unknown end node %q", r.EndElementId)
		}
		propsJSON, err := marshalProps(r.Props)
		if err != nil {
			return fmt.Errorf("graphlite: CopyFrom: marshal edge props: %w", err)
		}
		if _, err := tx.InsertEdge(ctx, r.Type, startID, endID, propsJSON); err != nil {
			return fmt.Errorf("graphlite: CopyFrom: insert edge: %w", err)
		}
	}

	return tx.Commit()
}

// CopyTo exports all nodes and relationships from this database into dst.
// dst can be any [neo4j.Driver] — a real Neo4j instance or another graphlite
// [DriverCompat]. For graphlite-to-graphlite copies, [DB.Export] + [DB.Import]
// is also an option.
//
// CopyTo issues one CREATE per node and one CREATE per relationship. A
// temporary node property (_graphliteId) is used to match relationship
// endpoints and is removed before CopyTo returns.
//
// CopyTo is not atomic on the destination. If it fails partway through,
// partially created data and residual _graphliteId properties may remain.
//
// Note: label names and relationship types must be valid Cypher identifiers
// (no spaces or special characters). Data stored via graphlite's own Cypher
// parser always satisfies this constraint.
func (d *DB) CopyTo(ctx context.Context, dst neo4j.Driver) error {
	nodes, err := d.st.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("graphlite: CopyTo: list nodes: %w", err)
	}
	edges, err := d.st.ListEdges(ctx)
	if err != nil {
		return fmt.Errorf("graphlite: CopyTo: list edges: %w", err)
	}

	// Phase 1: create nodes with a temporary _graphliteId for relationship matching.
	for _, n := range nodes {
		props, err := unmarshalProps(n.Props)
		if err != nil {
			return fmt.Errorf("graphlite: CopyTo: unmarshal node %d: %w", n.ID, err)
		}
		cypher, params := buildCreateNodeCypher(n.Labels, props, n.ID)
		if _, err := neo4j.ExecuteQuery(ctx, dst, cypher, params, neo4j.EagerResultTransformer); err != nil {
			return fmt.Errorf("graphlite: CopyTo: create node %d: %w", n.ID, err)
		}
	}

	// Phase 2: create relationships.
	for _, e := range edges {
		props, err := unmarshalProps(e.Props)
		if err != nil {
			return fmt.Errorf("graphlite: CopyTo: unmarshal edge %d: %w", e.ID, err)
		}
		cypher, params := buildCreateEdgeCypher(e.Type, e.StartID, e.EndID, props)
		if _, err := neo4j.ExecuteQuery(ctx, dst, cypher, params, neo4j.EagerResultTransformer); err != nil {
			return fmt.Errorf("graphlite: CopyTo: create edge %d: %w", e.ID, err)
		}
	}

	// Phase 3: remove temp property.
	if len(nodes) > 0 {
		if _, err := neo4j.ExecuteQuery(ctx, dst,
			"MATCH (n) WHERE n._graphliteId IS NOT NULL REMOVE n._graphliteId",
			nil, neo4j.EagerResultTransformer); err != nil {
			return fmt.Errorf("graphlite: CopyTo: remove temp property: %w", err)
		}
	}
	return nil
}

// buildCreateNodeCypher produces a CREATE statement for a single node.
// Labels and property keys must be valid Cypher identifiers.
func buildCreateNodeCypher(labelsStr string, props map[string]any, id int64) (string, map[string]any) {
	params := map[string]any{"glid": id}
	propExpr := buildInlinePropExpr(props, params)

	labelPart := rawLabelExpr(labelsStr)
	var nodePattern string
	switch {
	case labelPart != "" && propExpr != "":
		nodePattern = "n:" + labelPart + " {" + propExpr + "}"
	case labelPart != "":
		nodePattern = "n:" + labelPart
	case propExpr != "":
		nodePattern = "n {" + propExpr + "}"
	default:
		nodePattern = "n"
	}
	return fmt.Sprintf("CREATE (%s) SET n._graphliteId = $glid", nodePattern), params
}

// buildCreateEdgeCypher produces a MATCH+CREATE statement for a single edge.
func buildCreateEdgeCypher(edgeType string, startID, endID int64, props map[string]any) (string, map[string]any) {
	params := map[string]any{"sid": startID, "eid": endID}
	propExpr := buildInlinePropExpr(props, params)

	var relPattern string
	if propExpr != "" {
		relPattern = "r:" + edgeType + " {" + propExpr + "}"
	} else {
		relPattern = "r:" + edgeType
	}
	return fmt.Sprintf(
		"MATCH (a {_graphliteId: $sid}), (b {_graphliteId: $eid}) CREATE (a)-[%s]->(b)",
		relPattern,
	), params
}

// buildInlinePropExpr builds "key1: $p0, key2: $p1, ..." and populates params.
// Keys are sorted for deterministic output.
func buildInlinePropExpr(props map[string]any, params map[string]any) string {
	if len(props) == 0 {
		return ""
	}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	parts := make([]string, 0, len(keys))
	for i, k := range keys {
		pname := fmt.Sprintf("p%d", i)
		params[pname] = props[k]
		parts = append(parts, k+": $"+pname)
	}
	return strings.Join(parts, ", ")
}

// rawLabelExpr converts "Person,Employee" to "Person:Employee" (unquoted).
// Labels written through graphlite's Cypher parser are always valid identifiers.
func rawLabelExpr(labelsStr string) string {
	if labelsStr == "" {
		return ""
	}
	parts := strings.Split(labelsStr, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, ":")
}
