// Package ui embeds the operator dashboard so the binary serves it without a
// separate asset directory, matching how migrations are shipped.
package ui

import "embed"

//go:embed index.html
var FS embed.FS
