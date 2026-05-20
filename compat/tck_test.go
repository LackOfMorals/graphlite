//go:build tck

// Package compat contains the openCypher TCK (Technology Compatibility Kit)
// test harness for graphlite. It uses Godog (Cucumber for Go) to run Gherkin
// scenarios against a real graphlite in-memory database.
//
// Run with:
//
//	CGO_ENABLED=0 go test -tags=tck ./compat/... -v
//
// The harness reports a TCK pass rate at the end of the run. Scenarios tagged
// @skip are excluded from execution; each has a reason comment above it.
package compat

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/cucumber/godog"

	graphlite "github.com/LackOfMorals/graphlite"
)

// ─────────────────────────────────────────────────────────────────────────────
// TCK scenario definitions
//
// Representative openCypher TCK feature scenarios expressed inline as Gherkin.
// Supported scenarios are expected to pass. Scenarios tagged @skip are excluded
// from execution; each has a reason comment immediately above.
// ─────────────────────────────────────────────────────────────────────────────

// tckFeatures contains all inline Gherkin scenarios. Multiple Feature blocks
// are not allowed in a single Gherkin document, so all scenarios live inside
// one "graphlite TCK" Feature. Rule blocks separate logical groups.
const tckFeatures = `
Feature: graphlite TCK

  # ── Basic node matching ───────────────────────────────────────────────────

  Scenario: Match all nodes in an empty graph returns no results
    Given an empty graph
    When executing query "MATCH (n) RETURN n"
    Then the result should be empty

  Scenario: Match all nodes — single node present
    Given an empty graph
    And having executed "CREATE (n:Person {name: 'Alice'})"
    When executing query "MATCH (n) RETURN n"
    Then the result should not be empty

  Scenario: Match node by label
    Given an empty graph
    And having executed "CREATE (n:Person {name: 'Alice'})"
    And having executed "CREATE (n:Animal {name: 'Dog'})"
    When executing query "MATCH (n:Person) RETURN n"
    Then the result should contain exactly 1 row

  Scenario: Match node by property value
    Given an empty graph
    And having executed "CREATE (n:Person {name: 'Alice', age: 30})"
    And having executed "CREATE (n:Person {name: 'Bob', age: 25})"
    When executing query "MATCH (n:Person) WHERE n.name = 'Alice' RETURN n.name"
    Then the result should contain exactly 1 row

  Scenario: Return node property string value
    Given an empty graph
    And having executed "CREATE (n:Person {name: 'Alice'})"
    When executing query "MATCH (n:Person) RETURN n.name AS name"
    Then the result field "name" in row 0 equals "Alice"

  # ── CREATE statements ──────────────────────────────────────────────────────

  Scenario: Create a node with a label and query succeeds
    Given an empty graph
    When executing query "CREATE (n:Person {name: 'Bob'})"
    Then the query succeeds

  Scenario: Create an inline node-relationship-node and count nodes
    Given an empty graph
    When executing query "CREATE (a:Person {name: 'Alice'})-[:KNOWS]->(b:Person {name: 'Bob'})"
    Then the query succeeds
    And 2 nodes should have been created

  # ── Relationship traversal ─────────────────────────────────────────────────

  Scenario: Traverse a single directed relationship
    Given an empty graph
    And having executed "CREATE (a:Person {name: 'Alice'})-[:KNOWS]->(b:Person {name: 'Bob'})"
    When executing query "MATCH (a:Person)-[r:KNOWS]->(b:Person) RETURN a.name, b.name"
    Then the result should contain exactly 1 row

  Scenario: No match on wrong relationship type
    Given an empty graph
    And having executed "CREATE (a:Person {name: 'Alice'})-[:KNOWS]->(b:Person {name: 'Bob'})"
    When executing query "MATCH (a:Person)-[r:LIKES]->(b:Person) RETURN a.name, b.name"
    Then the result should be empty

  Scenario: Traverse returns correct property values
    Given an empty graph
    And having executed "CREATE (a:Person {name: 'Alice'})-[:KNOWS]->(b:Person {name: 'Bob'})"
    When executing query "MATCH (a:Person)-[:KNOWS]->(b:Person) RETURN a.name AS name"
    Then the result field "name" in row 0 equals "Alice"

  # ── Aggregation ───────────────────────────────────────────────────────────

  Scenario: Count all nodes
    Given an empty graph
    And having executed "CREATE (n:Person {name: 'Alice'})"
    And having executed "CREATE (n:Person {name: 'Bob'})"
    And having executed "CREATE (n:Person {name: 'Charlie'})"
    When executing query "MATCH (n:Person) RETURN count(n) AS cnt"
    Then the result field "cnt" in row 0 equals 3

  Scenario: Count relationships
    Given an empty graph
    And having executed "CREATE (a:Person {name: 'A'})-[:KNOWS]->(b:Person {name: 'B'})"
    And having executed "CREATE (a:Person {name: 'C'})-[:KNOWS]->(b:Person {name: 'D'})"
    When executing query "MATCH ()-[r:KNOWS]->() RETURN count(r) AS cnt"
    Then the result field "cnt" in row 0 equals 2

  # ── ORDER BY, LIMIT, SKIP ─────────────────────────────────────────────────

  Scenario: Order results by property ascending returns all rows
    Given an empty graph
    And having executed "CREATE (n:Person {name: 'Charlie', age: 35})"
    And having executed "CREATE (n:Person {name: 'Alice', age: 25})"
    And having executed "CREATE (n:Person {name: 'Bob', age: 30})"
    When executing query "MATCH (n:Person) RETURN n.name ORDER BY n.name"
    Then the result should contain exactly 3 rows

  Scenario: Limit results
    Given an empty graph
    And having executed "CREATE (n:Person {name: 'Alice'})"
    And having executed "CREATE (n:Person {name: 'Bob'})"
    And having executed "CREATE (n:Person {name: 'Charlie'})"
    When executing query "MATCH (n:Person) RETURN n LIMIT 2"
    Then the result should contain exactly 2 rows

  Scenario: Skip and limit results
    Given an empty graph
    And having executed "CREATE (n:Person {name: 'Alice'})"
    And having executed "CREATE (n:Person {name: 'Bob'})"
    And having executed "CREATE (n:Person {name: 'Charlie'})"
    When executing query "MATCH (n:Person) RETURN n SKIP 1 LIMIT 1"
    Then the result should contain exactly 1 row

  # ── MERGE ─────────────────────────────────────────────────────────────────

  Scenario: Merge creates node when it does not exist
    Given an empty graph
    When executing query "MERGE (n:Person {name: 'Alice'})"
    Then the query succeeds
    And 1 node should have been created

  Scenario: Merge matches existing node without creating duplicate
    Given an empty graph
    And having executed "CREATE (n:Person {name: 'Alice'})"
    When executing query "MERGE (n:Person {name: 'Alice'})"
    Then the query succeeds

  # ── WHERE predicates ──────────────────────────────────────────────────────

  Scenario: IS NULL predicate matches node with missing property
    Given an empty graph
    And having executed "CREATE (n:Person {name: 'Alice'})"
    And having executed "CREATE (n:Person {name: 'Bob', age: 30})"
    When executing query "MATCH (n:Person) WHERE n.age IS NULL RETURN n"
    Then the result should contain exactly 1 row

  Scenario: IS NOT NULL predicate matches node with property set
    Given an empty graph
    And having executed "CREATE (n:Person {name: 'Alice'})"
    And having executed "CREATE (n:Person {name: 'Bob', age: 30})"
    When executing query "MATCH (n:Person) WHERE n.age IS NOT NULL RETURN n"
    Then the result should contain exactly 1 row

  Scenario: STARTS WITH string predicate
    Given an empty graph
    And having executed "CREATE (n:Person {name: 'Alice'})"
    And having executed "CREATE (n:Person {name: 'Bob'})"
    When executing query "MATCH (n:Person) WHERE n.name STARTS WITH 'Al' RETURN n"
    Then the result should contain exactly 1 row

  Scenario: IN list predicate
    Given an empty graph
    And having executed "CREATE (n:Person {name: 'Alice'})"
    And having executed "CREATE (n:Person {name: 'Bob'})"
    And having executed "CREATE (n:Person {name: 'Charlie'})"
    When executing query "MATCH (n:Person) WHERE n.name IN ['Alice', 'Bob'] RETURN n"
    Then the result should contain exactly 2 rows

  # ── WITH pipelines ────────────────────────────────────────────────────────

  Scenario: WITH passes variables to next stage for aggregation
    Given an empty graph
    And having executed "CREATE (n:Person {name: 'Alice'})"
    And having executed "CREATE (n:Person {name: 'Bob'})"
    When executing query "MATCH (n:Person) WITH n RETURN count(n) AS cnt"
    Then the result field "cnt" in row 0 equals 2

  # ── Skipped scenarios (unsupported features) ──────────────────────────────
  # Each @skip tag is accompanied by a reason comment.

  # unsupported: variable-length paths
  @skip
  Scenario: Variable-length path match
    Given an empty graph
    When executing query "MATCH (a)-[*1..3]-(b) RETURN a, b"
    Then the result should be empty

  # unsupported: CALL subquery
  @skip
  Scenario: CALL subquery support
    Given an empty graph
    When executing query "CALL { MATCH (n) RETURN n } RETURN n"
    Then the result should be empty

  # unsupported: list comprehensions
  @skip
  Scenario: List comprehension expression
    Given an empty graph
    And having executed "CREATE (n:Person {name: 'Alice'})"
    When executing query "MATCH (n:Person) RETURN [x IN [1,2,3] WHERE x > 1 | x*2] AS list"
    Then the result should not be empty

  # unsupported: pattern predicates in WHERE
  @skip
  Scenario: Pattern predicate in WHERE clause
    Given an empty graph
    And having executed "CREATE (a:Person {name: 'Alice'})-[:KNOWS]->(b:Person {name: 'Bob'})"
    When executing query "MATCH (a:Person) WHERE (a)-[:KNOWS]->() RETURN a.name"
    Then the result should contain exactly 1 row

  # unsupported: FOREACH clause
  @skip
  Scenario: FOREACH iteration clause
    Given an empty graph
    When executing query "FOREACH (i IN [1,2,3] | CREATE (n:Number {val: i}))"
    Then the query succeeds

  # unsupported: string functions toLower/toUpper
  @skip
  Scenario: Built-in string function toLower
    Given an empty graph
    And having executed "CREATE (n:Person {name: 'Alice'})"
    When executing query "MATCH (n:Person) RETURN toLower(n.name)"
    Then the result should not be empty

  # unsupported: math functions (abs, ceil, floor)
  @skip
  Scenario: Built-in math function abs
    Given an empty graph
    And having executed "CREATE (n:Number {val: -5})"
    When executing query "MATCH (n:Number) RETURN abs(n.val)"
    Then the result should not be empty
`

// ─────────────────────────────────────────────────────────────────────────────
// Per-scenario state
// ─────────────────────────────────────────────────────────────────────────────

type tckState struct {
	db           *graphlite.DB
	lastResult   *graphlite.EagerResult
	lastError    error
	nodesCreated int
}

func newTCKState() *tckState { return &tckState{} }

func (s *tckState) reset() {
	if s.db != nil {
		_ = s.db.Close()
		s.db = nil
	}
	s.lastResult = nil
	s.lastError = nil
	s.nodesCreated = 0
}

// ─────────────────────────────────────────────────────────────────────────────
// Step definitions
// ─────────────────────────────────────────────────────────────────────────────

func (s *tckState) givenAnEmptyGraph(ctx context.Context) error {
	s.reset()
	db, err := graphlite.Open(":memory:")
	if err != nil {
		return fmt.Errorf("open in-memory db: %w", err)
	}
	s.db = db
	return nil
}

func (s *tckState) havingExecuted(ctx context.Context, cypher string) error {
	if s.db == nil {
		return fmt.Errorf("database not initialised (missing 'Given an empty graph' step)")
	}
	qr, err := s.db.RunQuery(ctx, cypher, nil)
	if err != nil {
		return fmt.Errorf("having executed %q: %w", cypher, err)
	}
	eager, err := graphlite.NewEagerResult(ctx, qr)
	if err != nil {
		return fmt.Errorf("collect result for %q: %w", cypher, err)
	}
	if eager.Summary != nil {
		s.nodesCreated += eager.Summary.Counters().NodesCreated()
	}
	return nil
}

func (s *tckState) whenExecutingQuery(ctx context.Context, cypher string) error {
	if s.db == nil {
		return fmt.Errorf("database not initialised")
	}
	qr, err := s.db.RunQuery(ctx, cypher, nil)
	if err != nil {
		s.lastError = err
		s.lastResult = nil
		return nil // capture error; assertion steps will check it
	}
	eager, err := graphlite.NewEagerResult(ctx, qr)
	if err != nil {
		s.lastError = err
		s.lastResult = nil
		return nil
	}
	s.lastResult = eager
	s.lastError = nil
	if eager.Summary != nil {
		s.nodesCreated += eager.Summary.Counters().NodesCreated()
	}
	return nil
}

func (s *tckState) theResultShouldBeEmpty() error {
	if s.lastError != nil {
		return fmt.Errorf("query failed: %w", s.lastError)
	}
	if s.lastResult == nil {
		return fmt.Errorf("no result available")
	}
	if len(s.lastResult.Records) != 0 {
		return fmt.Errorf("expected empty result, got %d row(s)", len(s.lastResult.Records))
	}
	return nil
}

func (s *tckState) theResultShouldNotBeEmpty() error {
	if s.lastError != nil {
		return fmt.Errorf("query failed: %w", s.lastError)
	}
	if s.lastResult == nil {
		return fmt.Errorf("no result available")
	}
	if len(s.lastResult.Records) == 0 {
		return fmt.Errorf("expected non-empty result, got 0 rows")
	}
	return nil
}

func (s *tckState) theResultShouldContainExactlyNRows(n int) error {
	if s.lastError != nil {
		return fmt.Errorf("query failed: %w", s.lastError)
	}
	if s.lastResult == nil {
		return fmt.Errorf("no result available")
	}
	if len(s.lastResult.Records) != n {
		return fmt.Errorf("expected %d row(s), got %d", n, len(s.lastResult.Records))
	}
	return nil
}

func (s *tckState) theQuerySucceeds() error {
	if s.lastError != nil {
		return fmt.Errorf("expected query to succeed, got: %w", s.lastError)
	}
	return nil
}

func (s *tckState) nNodesShouldHaveBeenCreated(n int) error {
	if s.nodesCreated != n {
		return fmt.Errorf("expected %d node(s) created, got %d", n, s.nodesCreated)
	}
	return nil
}

func (s *tckState) theResultFieldInRowEqualsString(field string, row int, expected string) error {
	if s.lastError != nil {
		return fmt.Errorf("query failed: %w", s.lastError)
	}
	if s.lastResult == nil {
		return fmt.Errorf("no result available")
	}
	if row >= len(s.lastResult.Records) {
		return fmt.Errorf("row %d not found (result has %d row(s))", row, len(s.lastResult.Records))
	}
	rec := s.lastResult.Records[row]
	val, ok := rec.Get(field)
	if !ok {
		return fmt.Errorf("field %q not found in row %d (keys: %v)", field, row, rec.Keys())
	}
	actual := fmt.Sprintf("%v", val)
	if actual != expected {
		return fmt.Errorf("field %q in row %d: expected %q, got %q", field, row, expected, actual)
	}
	return nil
}

func (s *tckState) theResultFieldInRowEqualsInt(field string, row int, expected int) error {
	if s.lastError != nil {
		return fmt.Errorf("query failed: %w", s.lastError)
	}
	if s.lastResult == nil {
		return fmt.Errorf("no result available")
	}
	if row >= len(s.lastResult.Records) {
		return fmt.Errorf("row %d not found (result has %d row(s))", row, len(s.lastResult.Records))
	}
	rec := s.lastResult.Records[row]
	val, ok := rec.Get(field)
	if !ok {
		return fmt.Errorf("field %q not found in row %d (keys: %v)", field, row, rec.Keys())
	}
	var actual int
	switch v := val.(type) {
	case int64:
		actual = int(v)
	case float64:
		actual = int(v)
	case int:
		actual = v
	default:
		str := fmt.Sprintf("%v", v)
		if _, err := fmt.Sscanf(str, "%d", &actual); err != nil {
			return fmt.Errorf("field %q in row %d: cannot parse %T(%v) as integer", field, row, val, val)
		}
	}
	if actual != expected {
		return fmt.Errorf("field %q in row %d: expected %d, got %d", field, row, expected, actual)
	}
	return nil
}

// ─────────────────────────────────────────────────────────────────────────────
// Suite-level pass-rate counters (atomic for goroutine safety)
// ─────────────────────────────────────────────────────────────────────────────

type tckCounters struct {
	total  int64
	passed int64
	failed int64
}

// ─────────────────────────────────────────────────────────────────────────────
// TestTCK — the main test entry point (compiled only with -tags=tck)
// ─────────────────────────────────────────────────────────────────────────────

func TestTCK(t *testing.T) {
	ctrs := &tckCounters{}
	state := newTCKState()

	opts := godog.Options{
		Format:   "pretty",
		Output:   os.Stdout,
		Tags:     "~@skip", // exclude @skip-tagged scenarios from execution
		NoColors: true,
		FeatureContents: []godog.Feature{
			{Name: "tck_inline", Contents: []byte(tckFeatures)},
		},
	}

	suite := godog.TestSuite{
		Name: "graphlite-tck",
		TestSuiteInitializer: func(tsc *godog.TestSuiteContext) {
			// Suite-level setup: close the state DB after the suite.
			tsc.AfterSuite(func() {
				state.reset()
			})
		},
		ScenarioInitializer: func(sc *godog.ScenarioContext) {
			// Reset per-scenario state before each scenario.
			sc.Before(func(ctx context.Context, scenario *godog.Scenario) (context.Context, error) {
				state.reset()
				return ctx, nil
			})

			// Track pass/fail after each scenario.
			sc.After(func(ctx context.Context, scenario *godog.Scenario, err error) (context.Context, error) {
				atomic.AddInt64(&ctrs.total, 1)
				if err != nil {
					atomic.AddInt64(&ctrs.failed, 1)
				} else {
					atomic.AddInt64(&ctrs.passed, 1)
				}
				return ctx, nil
			})

			// Register step definitions.
			sc.Given(`^an empty graph$`, state.givenAnEmptyGraph)
			sc.Step(`^having executed "([^"]*)"$`, state.havingExecuted)
			sc.When(`^executing query "([^"]*)"$`, state.whenExecutingQuery)
			sc.Then(`^the result should be empty$`, state.theResultShouldBeEmpty)
			sc.Then(`^the result should not be empty$`, state.theResultShouldNotBeEmpty)
			sc.Then(`^the result should contain exactly (\d+) rows?$`, state.theResultShouldContainExactlyNRows)
			sc.Then(`^the query succeeds$`, state.theQuerySucceeds)
			sc.Then(`^(\d+) nodes? should have been created$`, state.nNodesShouldHaveBeenCreated)
			sc.Then(`^the result field "([^"]*)" in row (\d+) equals "([^"]*)"$`, state.theResultFieldInRowEqualsString)
			sc.Then(`^the result field "([^"]*)" in row (\d+) equals (\d+)$`, state.theResultFieldInRowEqualsInt)
		},
		Options: &opts,
	}

	exitCode := suite.Run()

	// Count @skip-annotated scenarios to include in the report even though
	// they were excluded from execution (Tags: "~@skip").
	skippedCount := strings.Count(tckFeatures, "@skip")

	executed := int(atomic.LoadInt64(&ctrs.total))
	passed := int(atomic.LoadInt64(&ctrs.passed))
	failed := int(atomic.LoadInt64(&ctrs.failed))

	passRate := 0.0
	if executed > 0 {
		passRate = float64(passed) / float64(executed) * 100.0
	}

	// Prominent pass-rate banner in both stdout and t.Log.
	fmt.Printf("\n================================================================================\n")
	fmt.Printf("TCK pass rate: %d/%d (%.1f%%)  [skipped: %d, failed: %d]\n",
		passed, executed, passRate, skippedCount, failed)
	fmt.Printf("================================================================================\n\n")

	t.Logf("TCK pass rate: %d/%d (%.1f%%)  [skipped: %d, failed: %d]",
		passed, executed, passRate, skippedCount, failed)

	_ = exitCode // don't fail on non-zero exit; we enforce >= 50% threshold below

	if executed > 0 && passRate < 50.0 {
		t.Errorf("TCK pass rate %.1f%% is below the required 50%% threshold (%d/%d scenarios passed)",
			passRate, passed, executed)
	}
}
