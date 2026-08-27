// Package migrations carries the platform's SQL schema as an embedded filesystem.
//
// The files live at the repository root rather than inside the Postgres package because
// //go:embed cannot reach a parent directory, and because a schema is a first-class artifact:
// it is reviewed by people who do not read Go, it is applied by platformctl and by an ArgoCD
// sync-wave Job, and it is diffed against production by tooling that has no Go toolchain. A
// directory of plain .sql files serves all of those; a directory of Go string constants serves
// none of them.
//
// The only thing this package adds is the embed, so that a binary carries the exact schema it
// was built against and a pod can migrate itself without a sidecar, a ConfigMap, or a container
// image that has to be kept in step with the application image.
package migrations

import (
	"embed"
	"io/fs"
)

// FS holds every migration file, named `NNNN_<slug>.<up|down>.sql`.
//
// README.md is deliberately not embedded: it is documentation for humans, and an embedded copy
// would be one more thing the migration runner has to learn to ignore.
//
//go:embed *.sql
var FS embed.FS

// Files returns the embedded migrations as an fs.FS.
//
// It exists so that callers take the narrow interface rather than the concrete embed.FS, which
// is what lets a test hand the runner a synthetic set of migrations — an out-of-order pair, a
// missing down file, a checksum that changed — without writing files to disk.
func Files() fs.FS { return FS }
