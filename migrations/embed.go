// Package migrations embeds the ordered SQL schema migrations.
package migrations

import "embed"

// FS holds every *.sql file in this directory. Files are applied in lexical
// order, so names start with a zero-padded sequence number.
//
//go:embed *.sql
var FS embed.FS
