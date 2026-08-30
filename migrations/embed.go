// Package migrations embeds the SQL schema files so the binary can apply them
// itself, rather than relying on the Postgres image's one-shot initdb hook.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
