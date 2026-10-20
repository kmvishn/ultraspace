// Package web embeds the browser app into the binary.
package web

import "embed"

//go:embed static
var FS embed.FS
