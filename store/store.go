// Package store defines the storage interface and its SQLite implementation.
// The store layer works only with raw IDs, label strings, JSON blobs, and SQL
// result rows — it never imports Cypher types.
package store
