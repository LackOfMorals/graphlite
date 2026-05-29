// backend_switch demonstrates how to choose between a local graphlite database
// and a remote Neo4j instance at application startup based on an environment
// variable.
//
// Pattern: read GRAPHLITE_BACKEND from the environment.
// - When GRAPHLITE_BACKEND=local (default), open a graphlite in-memory database.
// - When GRAPHLITE_BACKEND=neo4j, connect to Neo4j using NEO4J_URI / NEO4J_USER /
//   NEO4J_PASS environment variables.
//
// Both backends run the same MATCH query, demonstrating that application logic
// can be written once against graphlite's native API and switched to Neo4j for
// production without changing the query layer.
//
// Run with:
//
//	GRAPHLITE_BACKEND=local go run .
//	GRAPHLITE_BACKEND=neo4j NEO4J_URI=neo4j://localhost:7687 NEO4J_USER=neo4j NEO4J_PASS=secret go run .
package main

import (
	"context"
	"fmt"
	"log"
	"os"

	graphlite "github.com/LackOfMorals/graphlite/v2"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func main() {
	ctx := context.Background()

	backend := os.Getenv("GRAPHLITE_BACKEND")
	if backend == "" {
		backend = "local"
	}

	switch backend {
	case "local":
		runWithGraphlite(ctx)
	case "neo4j":
		runWithNeo4j(ctx)
	default:
		log.Fatalf("unknown GRAPHLITE_BACKEND %q: expected \"local\" or \"neo4j\"", backend)
	}
}

// runWithGraphlite opens a local in-memory graphlite database, seeds it with a
// small graph, and runs a MATCH query using the graphlite native API.
func runWithGraphlite(ctx context.Context) {
	fmt.Println("Backend: graphlite (local)")

	db, err := graphlite.Open(":memory:")
	if err != nil {
		log.Fatalf("graphlite.Open: %v", err)
	}
	defer db.Close(ctx)

	// Seed with sample data.
	_, err = db.RunQuery(ctx, `CREATE (:Person {name: "Alice", age: 30})`, nil)
	if err != nil {
		log.Fatalf("seed: %v", err)
	}
	_, err = db.RunQuery(ctx, `CREATE (:Person {name: "Bob", age: 25})`, nil)
	if err != nil {
		log.Fatalf("seed: %v", err)
	}

	// Query people.
	result, err := db.RunQuery(ctx, `MATCH (p:Person) RETURN p.name AS name, p.age AS age`, nil)
	if err != nil {
		log.Fatalf("query: %v", err)
	}
	printResult(ctx, result)
}

// runWithNeo4j connects to a remote Neo4j instance using NEO4J_URI, NEO4J_USER,
// and NEO4J_PASS environment variables, then runs the same MATCH query.
func runWithNeo4j(ctx context.Context) {
	fmt.Println("Backend: Neo4j (remote)")

	uri := envOrDefault("NEO4J_URI", "neo4j://localhost:7687")
	user := envOrDefault("NEO4J_USER", "neo4j")
	pass := envOrDefault("NEO4J_PASS", "")

	driver, err := neo4j.NewDriverWithContext(uri, neo4j.BasicAuth(user, pass, ""))
	if err != nil {
		log.Fatalf("neo4j.NewDriverWithContext: %v", err)
	}
	defer driver.Close(ctx)

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx)

	// Run the same query against Neo4j. The column names match the graphlite path.
	result, err := session.Run(ctx, `MATCH (p:Person) RETURN p.name AS name, p.age AS age`, nil)
	if err != nil {
		log.Fatalf("session.Run: %v", err)
	}
	for result.Next(ctx) {
		rec := result.Record()
		name, _ := rec.Get("name")
		age, _ := rec.Get("age")
		fmt.Printf("  name=%v  age=%v\n", name, age)
	}
	if err := result.Err(); err != nil {
		log.Fatalf("result iteration: %v", err)
	}
}

// printResult drains a graphlite *Result and prints each record's name and age columns.
func printResult(ctx context.Context, result *graphlite.Result) {
	for result.Next(ctx) {
		rec := result.Record()
		name, _ := rec.Get("name")
		age, _ := rec.Get("age")
		fmt.Printf("  name=%v  age=%v\n", name, age)
	}
	if _, err := result.Consume(ctx); err != nil {
		log.Fatalf("consume: %v", err)
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
