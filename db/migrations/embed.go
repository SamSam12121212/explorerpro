package migrations

import "embed"

// Files contains the embedded SQL migrations for runtime persistence.
//
//go:embed *.sql
var Files embed.FS
