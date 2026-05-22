package graphlite

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// CopyFrom imports all nodes and relationships from src into this database,
// appending to any existing data. src can be any [Driver] — a real Neo4j
// instance wrapped in an adapter or another graphlite [*DB].
//
// The entire import runs inside a single graphlite transaction; if any error
// occurs all changes are rolled back and the database is unchanged.
func (d *DB) CopyFrom(ctx context.Context, src Driver) error {
	var nodes []*Node
	var rels []*Relationship

	sess := src.NewSession(ctx)
	defer sess.Close(ctx) //nolint:errcheck

	_, err := sess.ExecuteRead(ctx, func(tx ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, "MATCH (n) RETURN n", nil)
		if err != nil {
			return nil, err
		}
		for res.Next(ctx) {
			n, ok := res.Record().Values()[0].(*Node)
			if !ok {
				return nil, fmt.Errorf("graphlite: CopyFrom: unexpected node value type %T", res.Record().Values()[0])
			}
			nodes = append(nodes, n)
		}
		return nil, res.Err()
	})
	if err != nil {
		return fmt.Errorf("graphlite: CopyFrom: query nodes: %w", err)
	}

	_, err = sess.ExecuteRead(ctx, func(tx ManagedTransaction) (any, error) {
		res, err := tx.Run(ctx, "MATCH ()-[r]->() RETURN r", nil)
		if err != nil {
			return nil, err
		}
		for res.Next(ctx) {
			r, ok := res.Record().Values()[0].(*Relationship)
			if !ok {
				return nil, fmt.Errorf("graphlite: CopyFrom: unexpected relationship value type %T", res.Record().Values()[0])
			}
			rels = append(rels, r)
		}
		return nil, res.Err()
	})
	if err != nil {
		return fmt.Errorf("graphlite: CopyFrom: query edges: %w", err)
	}

	tx, err := d.st.Begin(ctx)
	if err != nil {
		return fmt.Errorf("graphlite: CopyFrom: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	idMap := make(map[string]int64, len(nodes))

	for _, n := range nodes {
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

	for _, r := range rels {
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
// dst can be any [Driver] — a real Neo4j instance wrapped in an adapter or
// another graphlite [*DB]. For graphlite-to-graphlite copies, [DB.Export] +
// [DB.Import] is also an option.
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
func (d *DB) CopyTo(ctx context.Context, dst Driver) error {
	nodes, err := d.st.ListNodes(ctx)
	if err != nil {
		return fmt.Errorf("graphlite: CopyTo: list nodes: %w", err)
	}
	edges, err := d.st.ListEdges(ctx)
	if err != nil {
		return fmt.Errorf("graphlite: CopyTo: list edges: %w", err)
	}

	sess := dst.NewSession(ctx)
	defer sess.Close(ctx) //nolint:errcheck

	// Phase 1: create nodes with a temporary _graphliteId for relationship matching.
	for _, n := range nodes {
		props, err := unmarshalProps(n.Props)
		if err != nil {
			return fmt.Errorf("graphlite: CopyTo: unmarshal node %d: %w", n.ID, err)
		}
		cypher, params := buildCreateNodeCypher(n.Labels, props, n.ID)
		_, err = sess.ExecuteWrite(ctx, func(tx ManagedTransaction) (any, error) {
			_, err := tx.Run(ctx, cypher, params)
			return nil, err
		})
		if err != nil {
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
		_, err = sess.ExecuteWrite(ctx, func(tx ManagedTransaction) (any, error) {
			_, err := tx.Run(ctx, cypher, params)
			return nil, err
		})
		if err != nil {
			return fmt.Errorf("graphlite: CopyTo: create edge %d: %w", e.ID, err)
		}
	}

	// Phase 3: remove temp property.
	if len(nodes) > 0 {
		_, err = sess.ExecuteWrite(ctx, func(tx ManagedTransaction) (any, error) {
			_, err := tx.Run(ctx,
				"MATCH (n) WHERE n._graphliteId IS NOT NULL REMOVE n._graphliteId", nil)
			return nil, err
		})
		if err != nil {
			return fmt.Errorf("graphlite: CopyTo: remove temp property: %w", err)
		}
	}
	return nil
}

// buildCreateNodeCypher produces a CREATE statement for a single node.
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
