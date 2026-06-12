package main

import (
	"strings"
)

// Mirrors packages/shared/src/fileFilters.ts. Keep symmetric — the worker
// filters again on extract, but doing it client-side avoids uploading bytes
// we'd just throw away.

var excludedDirPrefixes = []string{
	"node_modules/",
	".git/",
	"vendor/",
	"__pycache__/",
	".next/",
	"dist/",
	"build/",
	".cache/",
	".turbo/",
	"coverage/",
	".nyc_output/",
	".venv/",
	"venv/",
	".tox/",
	"target/",
}

var excludedExtensions = map[string]struct{}{
	"lock":   {},
	"min.js": {},
	"min.css": {},
	"map":    {},
	"wasm":   {},
	"png":    {},
	"jpg":    {},
	"jpeg":   {},
	"gif":    {},
	"ico":    {},
	"svg":    {},
	"webp":   {},
	"bmp":    {},
	"woff":   {},
	"woff2":  {},
	"ttf":    {},
	"eot":    {},
	"otf":    {},
	"pdf":    {},
	"zip":    {},
	"tar":    {},
	"gz":     {},
	"bz2":    {},
	"xz":     {},
	"7z":     {},
	"rar":    {},
	"pyc":    {},
	"pyo":    {},
	"class":  {},
	"o":      {},
	"so":     {},
	"dylib":  {},
	"dll":    {},
	"exe":    {},
}

// Per-file size cap. 5 MB lets candidates include sample data or fixtures
// without bundling node_modules-scale binaries. The server-side artifact
// filter has a tighter 500 KB cap for what it indexes; the tarball stores
// up to this limit so the full repo state is recoverable.
const maxBundleFileBytes = 5 * 1024 * 1024

// Total tarball uncompressed cap. Aborts the bundle if exceeded — typically
// indicates an accidentally-committed node_modules/dist.
const maxBundleTotalBytes = 100 * 1024 * 1024

func shouldBundleFile(path string, size int64) bool {
	if size > maxBundleFileBytes {
		return false
	}
	lower := strings.ToLower(path)
	for _, prefix := range excludedDirPrefixes {
		if strings.HasPrefix(lower, prefix) || strings.Contains(lower, "/"+prefix) {
			return false
		}
	}
	// Extract extension. Mirror the TS `lower.split('.').pop()` logic.
	dot := strings.LastIndex(lower, ".")
	if dot >= 0 {
		ext := lower[dot+1:]
		if _, blocked := excludedExtensions[ext]; blocked {
			return false
		}
	}
	return true
}
