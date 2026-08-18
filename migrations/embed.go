// Package migrations exposes the immutable data-analysis schema migrations.
//
// Embedding the SQL files into the migration binaries ensures that the code and
// the schema plan are released as one versioned artifact.
package migrations

import "embed"

// Files contains every numbered SQL migration in this directory.
//
//go:embed *.sql
var Files embed.FS
