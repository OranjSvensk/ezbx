// Package catalog carries the tool catalogue inside the ezbx program: one
// file per tool (tools/<id>.json), one per tool pack (packs/<id>.json) and
// one per starter profile (profiles/<id>.json). See docs/build.md and
// docs/profiles.md.
package catalog

import "embed"

// FS holds every tools/*.json, packs/*.json and profiles/*.json file.
//
//go:embed tools/*.json packs/*.json profiles/*.json
var FS embed.FS
