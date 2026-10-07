// Package gates holds the reference reviews shipped with tardis-gate. An
// enrolled repository uses them by name unless it supplies its own directory.
package gates

import "embed"

// FS contains <name>/gate.yml for each reference gate.
//
//go:embed */gate.yml
var FS embed.FS
