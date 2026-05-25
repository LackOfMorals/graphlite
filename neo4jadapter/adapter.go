// Package neo4jadapter wraps a neo4j.DriverWithContext so that it satisfies
// the graphlite.Driver interface. Use it when you want to call
// graphlite.DB.CopyFrom or graphlite.DB.CopyTo against a real Neo4j instance,
// or wherever application code that accepts graphlite.Driver needs to be backed
// by a live database.
//
// Usage:
//
//	neo4jDriver, err := neo4j.NewDriverWithContext(uri, neo4j.BasicAuth(user, pass, ""))
//	if err != nil {
//	    log.Fatal(err)
//	}
//	var driver graphlite.Driver = neo4jadapter.New(neo4jDriver)
//
//	// Now driver can be passed to graphlite.DB.CopyFrom / CopyTo, or used
//	// anywhere a graphlite.Driver is expected.
//	local, _ := graphlite.Open(":memory:")
//	local.CopyFrom(ctx, driver) // pull from real Neo4j into local graphlite
package neo4jadapter

import (
	"context"

	graphlite "github.com/LackOfMorals/graphlite"
	"github.com/neo4j/neo4j-go-driver/v6/neo4j"
)

// New wraps d so it satisfies graphlite.Driver. Sessions created through the
// returned driver use the default neo4j.SessionConfig (no database override,
// write access mode). Use NewWithConfig if you need custom session options.
func New(d neo4j.DriverWithContext) graphlite.Driver {
	return &driverAdapter{d: d, cfg: neo4j.SessionConfig{}}
}

// NewWithConfig wraps d and applies cfg to every session opened through the
// returned driver. This lets callers target a specific database or set an
// access mode.
func NewWithConfig(d neo4j.DriverWithContext, cfg neo4j.SessionConfig) graphlite.Driver {
	return &driverAdapter{d: d, cfg: cfg}
}

// ─────────────────────────────────────────────────────────────────────────────
// driverAdapter — graphlite.Driver
// ─────────────────────────────────────────────────────────────────────────────

type driverAdapter struct {
	d   neo4j.DriverWithContext
	cfg neo4j.SessionConfig
}

func (a *driverAdapter) NewSession(ctx context.Context) graphlite.Session {
	return &sessionAdapter{s: a.d.NewSession(ctx, a.cfg)}
}

func (a *driverAdapter) VerifyConnectivity(ctx context.Context) error {
	return a.d.VerifyConnectivity(ctx)
}

func (a *driverAdapter) Close(ctx context.Context) error {
	return a.d.Close(ctx)
}

// ─────────────────────────────────────────────────────────────────────────────
// sessionAdapter — graphlite.Session
// ─────────────────────────────────────────────────────────────────────────────

type sessionAdapter struct{ s neo4j.Session }

func (a *sessionAdapter) ExecuteRead(ctx context.Context, work graphlite.ManagedTransactionWork) (any, error) {
	return a.s.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return work(&managedTxAdapter{tx: tx})
	})
}

func (a *sessionAdapter) ExecuteWrite(ctx context.Context, work graphlite.ManagedTransactionWork) (any, error) {
	return a.s.ExecuteWrite(ctx, func(tx neo4j.ManagedTransaction) (any, error) {
		return work(&managedTxAdapter{tx: tx})
	})
}

func (a *sessionAdapter) BeginTransaction(ctx context.Context) (graphlite.Transaction, error) {
	tx, err := a.s.BeginTransaction(ctx)
	if err != nil {
		return nil, err
	}
	return &explicitTxAdapter{tx: tx}, nil
}

func (a *sessionAdapter) Run(ctx context.Context, cypher string, params map[string]any) (graphlite.Result, error) {
	res, err := a.s.Run(ctx, cypher, params)
	if err != nil {
		return nil, err
	}
	return &resultAdapter{r: res}, nil
}

func (a *sessionAdapter) Close(ctx context.Context) error {
	return a.s.Close(ctx)
}

// ─────────────────────────────────────────────────────────────────────────────
// managedTxAdapter — graphlite.ManagedTransaction
// ─────────────────────────────────────────────────────────────────────────────

type managedTxAdapter struct{ tx neo4j.ManagedTransaction }

func (a *managedTxAdapter) Run(ctx context.Context, cypher string, params map[string]any) (graphlite.Result, error) {
	res, err := a.tx.Run(ctx, cypher, params)
	if err != nil {
		return nil, err
	}
	return &resultAdapter{r: res}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// explicitTxAdapter — graphlite.Transaction
// ─────────────────────────────────────────────────────────────────────────────

type explicitTxAdapter struct{ tx neo4j.ExplicitTransaction }

func (a *explicitTxAdapter) Run(ctx context.Context, cypher string, params map[string]any) (graphlite.Result, error) {
	res, err := a.tx.Run(ctx, cypher, params)
	if err != nil {
		return nil, err
	}
	return &resultAdapter{r: res}, nil
}

func (a *explicitTxAdapter) Commit(ctx context.Context) error   { return a.tx.Commit(ctx) }
func (a *explicitTxAdapter) Rollback(ctx context.Context) error { return a.tx.Rollback(ctx) }
func (a *explicitTxAdapter) Close(ctx context.Context) error    { return a.tx.Close(ctx) }

// ─────────────────────────────────────────────────────────────────────────────
// resultAdapter — graphlite.Result
// ─────────────────────────────────────────────────────────────────────────────

type resultAdapter struct {
	r       neo4j.Result
	current *graphlite.Record
}

// Keys returns the projection column names. neo4j.Result.Keys() returns an
// error which is discarded here; any underlying problem surfaces on Next/Err.
func (a *resultAdapter) Keys() []string {
	keys, _ := a.r.Keys()
	return keys
}

func (a *resultAdapter) Next(ctx context.Context) bool {
	if !a.r.Next(ctx) {
		return false
	}
	a.current = convertRecord(a.r.Record())
	return true
}

func (a *resultAdapter) Record() *graphlite.Record { return a.current }
func (a *resultAdapter) Err() error                { return a.r.Err() }

func (a *resultAdapter) Collect(ctx context.Context) ([]*graphlite.Record, error) {
	neo4jRecs, err := a.r.Collect(ctx)
	if err != nil {
		return nil, err
	}
	recs := make([]*graphlite.Record, len(neo4jRecs))
	for i, r := range neo4jRecs {
		recs[i] = convertRecord(r)
	}
	return recs, nil
}

func (a *resultAdapter) Consume(ctx context.Context) (graphlite.ResultSummary, error) {
	sum, err := a.r.Consume(ctx)
	if err != nil {
		return nil, err
	}
	return &resultSummaryAdapter{s: sum}, nil
}

// ─────────────────────────────────────────────────────────────────────────────
// ResultSummary / Counters adapters
// ─────────────────────────────────────────────────────────────────────────────

type resultSummaryAdapter struct{ s neo4j.ResultSummary }

func (a *resultSummaryAdapter) Counters() graphlite.Counters {
	return &countersAdapter{c: a.s.Counters()}
}

type countersAdapter struct{ c neo4j.Counters }

func (a *countersAdapter) NodesCreated() int         { return a.c.NodesCreated() }
func (a *countersAdapter) NodesDeleted() int         { return a.c.NodesDeleted() }
func (a *countersAdapter) RelationshipsCreated() int { return a.c.RelationshipsCreated() }
func (a *countersAdapter) RelationshipsDeleted() int { return a.c.RelationshipsDeleted() }
func (a *countersAdapter) PropertiesSet() int        { return a.c.PropertiesSet() }
func (a *countersAdapter) ContainsUpdates() bool     { return a.c.ContainsUpdates() }

// ─────────────────────────────────────────────────────────────────────────────
// Value conversion helpers
// ─────────────────────────────────────────────────────────────────────────────

// convertRecord converts a *neo4j.Record to a *graphlite.Record, translating
// any neo4j.Node or neo4j.Relationship values into their graphlite equivalents
// so that callers such as graphlite.DB.CopyFrom receive the expected types.
func convertRecord(r *neo4j.Record) *graphlite.Record {
	vals := make([]any, len(r.Values))
	for i, v := range r.Values {
		vals[i] = convertValue(v)
	}
	return graphlite.NewRecord(r.Keys, vals)
}

// convertValue maps neo4j graph types to graphlite graph types. Scalar values
// (strings, int64, float64, bool, nil, …) pass through unchanged.
func convertValue(v any) any {
	switch val := v.(type) {
	case neo4j.Node:
		return &graphlite.Node{
			ElementId: val.ElementId,
			Labels:    val.Labels,
			Props:     val.Props,
		}
	case neo4j.Relationship:
		return &graphlite.Relationship{
			ElementId:      val.ElementId,
			Type:           val.Type,
			StartElementId: val.StartElementId,
			EndElementId:   val.EndElementId,
			Props:          val.Props,
		}
	default:
		return v
	}
}
