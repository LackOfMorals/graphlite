//go:build ignore

// This file is a graphlite-injected shim added to the vendored neo4j package
// to allow graphlite's compatSession to implement neo4j.Session, including its
// unexported methods (executeQueryRead, executeQueryWrite, getServerInfo,
// verifyAuthentication). Without this shim, a struct embedding a nil neo4j.Session
// would panic when neo4j.ExecuteQuery calls session.executeQueryWrite via the
// interface dispatch path.
//
// This file is intentionally minimal and adds no public API surface.
package neo4j

import (
	"context"

	"github.com/neo4j/neo4j-go-driver/v6/neo4j/db"
)

// ManagedTransactionWorkFunc is the callback type for executeQueryRead/Write.
type ManagedTransactionWorkFunc func(ctx context.Context, work ManagedTransactionWork, configurers ...func(*TransactionConfig)) (any, error)

// EmbeddableSession is a concrete type that implements neo4j.Session including
// its unexported methods. It is designed to be embedded in graphlite's
// compatSession in place of a nil neo4j.Session interface, preventing nil
// pointer panics when neo4j.ExecuteQuery calls executeQueryRead/executeQueryWrite.
//
// The public methods (LastBookmarks, BeginTransaction, ExecuteRead, ExecuteWrite,
// Run, Close) are overridden by the outer compatSession embedding this type;
// only the unexported methods are dispatched to this struct's implementations.
type EmbeddableSession struct {
	// ExecQueryReadFn is called by executeQueryRead (used by neo4j.ExecuteQuery
	// with readers routing).
	ExecQueryReadFn ManagedTransactionWorkFunc
	// ExecQueryWriteFn is called by executeQueryWrite (used by neo4j.ExecuteQuery
	// with writers routing, which is the default).
	ExecQueryWriteFn ManagedTransactionWorkFunc
}

// LastBookmarks satisfies the Session interface (overridden by outer type).
func (e *EmbeddableSession) LastBookmarks() Bookmarks { return Bookmarks{} }

// BeginTransaction satisfies the Session interface (overridden by outer type).
func (e *EmbeddableSession) BeginTransaction(_ context.Context, _ ...func(*TransactionConfig)) (ExplicitTransaction, error) {
	panic("graphlite: EmbeddableSession.BeginTransaction should be overridden by embedding type")
}

// ExecuteRead satisfies the Session interface (overridden by outer type).
func (e *EmbeddableSession) ExecuteRead(ctx context.Context, work ManagedTransactionWork, configurers ...func(*TransactionConfig)) (any, error) {
	panic("graphlite: EmbeddableSession.ExecuteRead should be overridden by embedding type")
}

// ExecuteWrite satisfies the Session interface (overridden by outer type).
func (e *EmbeddableSession) ExecuteWrite(ctx context.Context, work ManagedTransactionWork, configurers ...func(*TransactionConfig)) (any, error) {
	panic("graphlite: EmbeddableSession.ExecuteWrite should be overridden by embedding type")
}

// Run satisfies the Session interface (overridden by outer type).
func (e *EmbeddableSession) Run(_ context.Context, _ string, _ map[string]any, _ ...func(*TransactionConfig)) (Result, error) {
	panic("graphlite: EmbeddableSession.Run should be overridden by embedding type")
}

// Close satisfies the Session interface (overridden by outer type).
func (e *EmbeddableSession) Close(_ context.Context) error {
	panic("graphlite: EmbeddableSession.Close should be overridden by embedding type")
}

// executeQueryRead is the unexported method called by neo4j.ExecuteQuery when
// routing is set to Read. It delegates to ExecQueryReadFn.
func (e *EmbeddableSession) executeQueryRead(ctx context.Context, work ManagedTransactionWork, configurers ...func(*TransactionConfig)) (any, error) {
	if e.ExecQueryReadFn == nil {
		panic("graphlite: EmbeddableSession.ExecQueryReadFn is nil")
	}
	return e.ExecQueryReadFn(ctx, work, configurers...)
}

// executeQueryWrite is the unexported method called by neo4j.ExecuteQuery when
// routing is set to Write (the default). It delegates to ExecQueryWriteFn.
func (e *EmbeddableSession) executeQueryWrite(ctx context.Context, work ManagedTransactionWork, configurers ...func(*TransactionConfig)) (any, error) {
	if e.ExecQueryWriteFn == nil {
		panic("graphlite: EmbeddableSession.ExecQueryWriteFn is nil")
	}
	return e.ExecQueryWriteFn(ctx, work, configurers...)
}

// getServerInfo is the unexported method called internally by the neo4j package.
// graphlite does not support server info queries; returns a stub.
func (e *EmbeddableSession) getServerInfo(_ context.Context) (ServerInfo, error) {
	return &serverInfo{address: "localhost:0", agent: "graphlite/1.0"}, nil
}

// verifyAuthentication is the unexported method for auth verification.
// graphlite accepts all auth; always returns nil.
func (e *EmbeddableSession) verifyAuthentication(_ context.Context) error { return nil }

// serverInfo is a minimal ServerInfo implementation used by getServerInfo.
type serverInfo struct {
	address string
	agent   string
}

func (s *serverInfo) Address() string                        { return s.address }
func (s *serverInfo) Agent() string                          { return s.agent }
func (s *serverInfo) ProtocolVersion() db.ProtocolVersion   { return db.ProtocolVersion{} }
