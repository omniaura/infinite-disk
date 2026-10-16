package agent

import "embed"

// Files contains only the reviewed worker and locked manifest, never installed dependencies.
//
//go:embed bridge.mjs package.json package-lock.json
var Files embed.FS
