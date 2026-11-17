// Package templates embeds the versioned metric and dashboard templates so
// sitegen remains a single self-contained binary.
package templates

import "embed"

// FS contains templates/metrics/*.yaml and templates/dashboard/*.json.
//
//go:embed metrics/*.yaml dashboard/*.json
var FS embed.FS
