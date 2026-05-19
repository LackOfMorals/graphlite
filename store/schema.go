package store

// schemaDDL is the complete DDL for the graphlite SQLite schema.
// It is executed once when a new database is opened or created.
//
// Design notes:
//   - labels is stored as comma-separated text so that the instr/LIKE-based
//     label lookup can use the idx_nodes_labels index.
//   - props is stored as a JSON column so that json_extract() can project and
//     filter individual property keys.
//   - All four indexes described in the architecture are created unconditionally
//     (IF NOT EXISTS guards make this safe to re-run on an existing database).
const schemaDDL = `
CREATE TABLE IF NOT EXISTS nodes (
    id     INTEGER PRIMARY KEY AUTOINCREMENT,
    labels TEXT    NOT NULL DEFAULT '',
    props  JSON    NOT NULL DEFAULT '{}'
);

CREATE TABLE IF NOT EXISTS edges (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    type     TEXT    NOT NULL,
    start_id INTEGER NOT NULL REFERENCES nodes(id),
    end_id   INTEGER NOT NULL REFERENCES nodes(id),
    props    JSON    NOT NULL DEFAULT '{}'
);

CREATE INDEX IF NOT EXISTS idx_nodes_labels ON nodes(labels);
CREATE INDEX IF NOT EXISTS idx_edges_start  ON edges(start_id);
CREATE INDEX IF NOT EXISTS idx_edges_end    ON edges(end_id);
CREATE INDEX IF NOT EXISTS idx_edges_type   ON edges(type);
`
