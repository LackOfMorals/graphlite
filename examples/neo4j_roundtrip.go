//go:build ignore

// neo4j_roundtrip shows the pull → modify → push cycle using graphlite as a
// local processing layer between two Neo4j driver instances.
//
//  1. Pull: use CopyFrom to copy a graph from a remote Neo4j driver into a
//     local in-memory graphlite instance.
//  2. Modify: run Cypher against the local copy to enrich or transform the data.
//  3. Push: use CopyTo to promote the modified graph to the destination driver.
//
// In production, swap the NewDriver calls marked "← swap for real Neo4j" with:
//
//	neo4j.NewDriverWithContext("bolt://localhost:7687",
//	    neo4j.BasicAuth("neo4j", "password", ""))
//
// Run with:
//
//	go run github.com/LackOfMorals/graphlite/examples/neo4j_roundtrip.go
//
// or from the repo root:
//
//	go run examples/neo4j_roundtrip.go
package main

import (
	"context"
	"fmt"

	graphlite "github.com/LackOfMorals/graphlite"
	neo4j "github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

func main() {
	ctx := context.Background()

	// ── Step 1: seed the remote source ───────────────────────────────────────
	// ← swap for real Neo4j in production
	remote, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	must(err)
	defer remote.Close(ctx)

	seed(ctx, remote)
	fmt.Println("=== Remote graph (before) ===")
	printGraph(ctx, remote)

	// ── Step 2: pull into a local graphlite instance ──────────────────────────
	local, err := graphlite.Open(":memory:")
	must(err)
	defer local.Close()

	must(local.CopyFrom(ctx, remote))

	// ── Step 3: modify the local copy ────────────────────────────────────────
	// Tag each department with the region it belongs to.
	for name, region := range map[string]string{
		"Engineering": "EMEA",
		"Sales":       "AMER",
		"Support":     "APAC",
	} {
		_, err = local.RunQuery(ctx,
			`MATCH (d:Department {name: $name}) SET d.region = $region`,
			map[string]any{"name": name, "region": region},
		)
		must(err)
	}

	// Promote engineers with seniority > 5 years to "Senior Engineer".
	for _, name := range []string{"Alice", "Bob"} {
		qr, err := local.RunQuery(ctx,
			`MATCH (e:Employee {name: $name}) RETURN e.yearsExp AS yrs`,
			map[string]any{"name": name},
		)
		must(err)
		eager, err := graphlite.NewEagerResult(ctx, qr)
		must(err)
		if len(eager.Records) == 0 {
			continue
		}
		yrs, _ := eager.Records[0].Get("yrs")
		if toFloat(yrs) > 5 {
			_, err = local.RunQuery(ctx,
				`MATCH (e:Employee {name: $name}) SET e.role = "Senior Engineer"`,
				map[string]any{"name": name},
			)
			must(err)
		}
	}

	// Link employees to their manager where one was not set remotely.
	_, err = local.RunQuery(ctx,
		`MATCH (mgr:Employee {name: "Alice"}), (rep:Employee {name: "Carol"})
		 CREATE (mgr)-[:MANAGES]->(rep)`,
		nil,
	)
	must(err)

	// ── Step 4: push the enriched graph to the destination ───────────────────
	// ← swap for a real Neo4j instance in production.
	// CopyTo writes every node and relationship; use a fresh database or a
	// dedicated namespace so existing data is not duplicated.
	destination, err := graphlite.NewDriver(":memory:", neo4j.NoAuth())
	must(err)
	defer destination.Close(ctx)

	must(local.CopyTo(ctx, destination))

	fmt.Println("\n=== Destination graph (after enrichment) ===")
	printGraph(ctx, destination)
	printManagers(ctx, destination)
}

// seed populates the remote driver with a small org-chart graph.
func seed(ctx context.Context, driver neo4j.Driver) {
	queries := []struct {
		cypher string
		params map[string]any
	}{
		{`CREATE (:Department {name: "Engineering"})`, nil},
		{`CREATE (:Department {name: "Sales"})`, nil},
		{`CREATE (:Department {name: "Support"})`, nil},
		{
			`CREATE (:Employee {name: "Alice", role: "Engineer", yearsExp: 8})`,
			nil,
		},
		{
			`CREATE (:Employee {name: "Bob", role: "Engineer", yearsExp: 3})`,
			nil,
		},
		{
			`CREATE (:Employee {name: "Carol", role: "Sales Rep", yearsExp: 6})`,
			nil,
		},
		{
			`MATCH (e:Employee {name: "Alice"}), (d:Department {name: "Engineering"})
			 CREATE (e)-[:WORKS_IN]->(d)`,
			nil,
		},
		{
			`MATCH (e:Employee {name: "Bob"}), (d:Department {name: "Engineering"})
			 CREATE (e)-[:WORKS_IN]->(d)`,
			nil,
		},
		{
			`MATCH (e:Employee {name: "Carol"}), (d:Department {name: "Sales"})
			 CREATE (e)-[:WORKS_IN]->(d)`,
			nil,
		},
	}
	for _, q := range queries {
		_, err := neo4j.ExecuteQuery(ctx, driver, q.cypher, q.params, neo4j.EagerResultTransformer)
		must(err)
	}
}

// printGraph prints employees, their roles, and the department they work in.
func printGraph(ctx context.Context, driver neo4j.Driver) {
	result, err := neo4j.ExecuteQuery(ctx, driver,
		`MATCH (e:Employee)-[:WORKS_IN]->(d:Department)
		 RETURN e.name AS name, e.role AS role, e.yearsExp AS exp,
		        d.name AS dept, d.region AS region
		 ORDER BY e.name`,
		nil, neo4j.EagerResultTransformer,
	)
	must(err)
	for _, rec := range result.Records {
		m := rec.AsMap()
		fmt.Printf("  %-8s  role=%-18s exp=%-3v dept=%-14s region=%v\n",
			m["name"], m["role"], m["exp"], m["dept"], m["region"])
	}
}

// printManagers prints MANAGES relationships.
func printManagers(ctx context.Context, driver neo4j.Driver) {
	result, err := neo4j.ExecuteQuery(ctx, driver,
		`MATCH (mgr:Employee)-[:MANAGES]->(rep:Employee)
		 RETURN mgr.name AS manager, rep.name AS report`,
		nil, neo4j.EagerResultTransformer,
	)
	must(err)
	for _, rec := range result.Records {
		m := rec.AsMap()
		fmt.Printf("  %s manages %s\n", m["manager"], m["report"])
	}
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case int64:
		return float64(n)
	}
	return 0
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
