// copy_from_neo4j demonstrates seeding a local graphlite database from a remote
// Neo4j instance.
//
// Pattern: connect to both databases, read nodes and relationships from Neo4j
// using session.Run / Collect, then write them into a local graphlite database
// using BeginTx / tx.Run / tx.Commit. The local file can then be used offline or
// in tests without requiring a running Neo4j instance.
//
// Environment variables:
//
//	NEO4J_URI   — bolt or neo4j URI (default: neo4j://localhost:7687)
//	NEO4J_USER  — username (default: neo4j)
//	NEO4J_PASS  — password (default: empty)
//	GRAPHLITE_PATH — output file path (default: copy_from_neo4j.db)
//
// Run with:
//
//	NEO4J_URI=neo4j://localhost:7687 NEO4J_USER=neo4j NEO4J_PASS=secret go run .
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	graphlite "github.com/LackOfMorals/graphlite/v2"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j/dbtype"
)

func main() {
	ctx := context.Background()

	// ── Neo4j source ─────────────────────────────────────────────────────────
	uri := envOrDefault("NEO4J_URI", "neo4j://localhost:7687")
	user := envOrDefault("NEO4J_USER", "neo4j")
	pass := envOrDefault("NEO4J_PASS", "")

	driver, err := neo4j.NewDriverWithContext(uri, neo4j.BasicAuth(user, pass, ""))
	if err != nil {
		log.Fatalf("neo4j.NewDriverWithContext: %v", err)
	}
	defer driver.Close(ctx)

	// ── graphlite destination ─────────────────────────────────────────────────
	dbPath := envOrDefault("GRAPHLITE_PATH", "copy_from_neo4j.db")
	db, err := graphlite.Open(dbPath)
	if err != nil {
		log.Fatalf("graphlite.Open: %v", err)
	}
	defer db.Close(ctx)

	// ── Copy nodes ────────────────────────────────────────────────────────────
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx)

	nodeResult, err := session.Run(ctx, `MATCH (n) RETURN n`, nil)
	if err != nil {
		log.Fatalf("neo4j MATCH nodes: %v", err)
	}
	nodes, err := nodeResult.Collect(ctx)
	if err != nil {
		log.Fatalf("collect nodes: %v", err)
	}

	tx, err := db.BeginTx(ctx)
	if err != nil {
		log.Fatalf("graphlite.BeginTx: %v", err)
	}

	nodeCount := 0
	for _, rec := range nodes {
		raw, ok := rec.Get("n")
		if !ok {
			continue
		}
		node, ok := raw.(dbtype.Node)
		if !ok {
			continue
		}

		// Build a CREATE statement using the node's first label and properties.
		label := "Node"
		if len(node.Labels) > 0 {
			label = node.Labels[0]
		}
		params := make(map[string]any, len(node.Props)+1)
		for k, v := range node.Props {
			params[k] = v
		}
		// Preserve the original Neo4j element ID as a stable identifier so
		// that relationship endpoints can be resolved after copying nodes.
		params["_neo4j_id"] = node.ElementId

		cypher := fmt.Sprintf("CREATE (n:`%s` $props)", label)
		if _, err := tx.Run(ctx, cypher, map[string]any{"props": params}); err != nil {
			_ = tx.Rollback()
			log.Fatalf("graphlite CREATE node: %v", err)
		}
		nodeCount++
	}
	if err := tx.Commit(); err != nil {
		log.Fatalf("graphlite Commit nodes: %v", err)
	}
	fmt.Printf("Copied %d node(s) from Neo4j → graphlite\n", nodeCount)

	// ── Copy relationships ────────────────────────────────────────────────────
	relResult, err := session.Run(ctx, `MATCH (a)-[r]->(b) RETURN a, r, b`, nil)
	if err != nil {
		log.Fatalf("neo4j MATCH rels: %v", err)
	}
	rels, err := relResult.Collect(ctx)
	if err != nil {
		log.Fatalf("collect rels: %v", err)
	}

	tx2, err := db.BeginTx(ctx)
	if err != nil {
		log.Fatalf("graphlite.BeginTx (rels): %v", err)
	}

	relCount := 0
	for _, rec := range rels {
		rawRel, ok := rec.Get("r")
		if !ok {
			continue
		}
		rel, ok := rawRel.(dbtype.Relationship)
		if !ok {
			continue
		}
		rawA, _ := rec.Get("a")
		rawB, _ := rec.Get("b")
		nodeA, _ := rawA.(dbtype.Node)
		nodeB, _ := rawB.(dbtype.Node)

		// Match nodes by their preserved _neo4j_id property.
		params := map[string]any{
			"aid":  nodeA.ElementId,
			"bid":  nodeB.ElementId,
			"type": rel.Type,
		}
		for k, v := range rel.Props {
			params["prop_"+k] = v
		}
		cypher := fmt.Sprintf(
			"MATCH (a {_neo4j_id: $aid}), (b {_neo4j_id: $bid}) CREATE (a)-[:`%s`]->(b)",
			rel.Type,
		)
		if _, err := tx2.Run(ctx, cypher, params); err != nil {
			_ = tx2.Rollback()
			log.Fatalf("graphlite CREATE rel: %v", err)
		}
		relCount++
	}
	if err := tx2.Commit(); err != nil {
		log.Fatalf("graphlite Commit rels: %v", err)
	}
	fmt.Printf("Copied %d relationship(s) from Neo4j → graphlite\n", relCount)
	fmt.Printf("Database written to: %s\n", dbPath)
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
