package main

import (
	"strings"
)

// Mirrors packages/shared/src/fileFilters.ts in promptster-hire-backend.
// Keep symmetric — the worker filters again on extract, but doing it
// client-side avoids uploading bytes we'd just throw away. The secret-file
// lists below are pinned to the server's by TestSecretFileListsMatchServer.

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

// Secret-bearing files: the server's EXCLUDED_FILE_NAMES,
// EXCLUDED_SECRET_EXTENSIONS, EXCLUDED_SECRET_DIR_PREFIXES and isSecretFile,
// ported one-for-one. This is the ONE list every staging/upload path uses:
// gitAddExcludePathspecs (git add in submit, snapshot, git watcher) and
// bundleWorkspace (the tarball).

// excludedSecretFileNames: exact basenames that are credentials, never source.
var excludedSecretFileNames = map[string]struct{}{
	".npmrc": {}, ".yarnrc": {}, ".netrc": {}, "_netrc": {}, ".pgpass": {},
	".htpasswd": {}, ".dockercfg": {},
	".mcp.json":           {}, // MCP server configs routinely inline third-party API keys
	"settings.local.json": {}, // where the CLI writes apiKeyHelper / proxy wiring
	"credentials": {}, "credentials.json": {}, "service-account.json": {},
	"id_rsa": {}, "id_dsa": {}, "id_ecdsa": {}, "id_ed25519": {},
	"secrets.yml": {}, "secrets.yaml": {}, "secrets.json": {},
	// CLI-only: not in the server list (yet). Kept from the CLI's earlier
	// git-add exclusions so this change never narrows coverage.
	".git-credentials": {},
}

// cliOnlySecretFileNames are the entries above the server doesn't have.
var cliOnlySecretFileNames = []string{".git-credentials"}

// excludedSecretExtensions: key material regardless of where it sits.
var excludedSecretExtensions = map[string]struct{}{
	"pem": {}, "key": {}, "p12": {}, "pfx": {}, "jks": {},
	"keystore": {}, "ppk": {}, "asc": {}, "gpg": {}, "kdbx": {},
}

// excludedSecretDirPrefixes: directories that hold nothing but credentials.
var excludedSecretDirPrefixes = []string{".ssh/", ".gnupg/", ".aws/", ".gcloud/"}

// envTemplateSuffixes: `.env` variants that are documentation — KEPT.
var envTemplateSuffixes = []string{"example", "sample", "template", "dist", "defaults"}

// isSecretFile reports whether a repo-relative path names a file that can
// carry a credential. Same logic as the server's isSecretFile.
func isSecretFile(path string) bool {
	lower := strings.ToLower(path)
	for _, prefix := range excludedSecretDirPrefixes {
		if strings.HasPrefix(lower, prefix) || strings.Contains(lower, "/"+prefix) {
			return true
		}
	}
	base := lower[strings.LastIndex(lower, "/")+1:]
	if base == "" {
		return false
	}
	if _, ok := excludedSecretFileNames[base]; ok {
		return true
	}
	if dot := strings.LastIndex(base, "."); dot > 0 {
		if _, ok := excludedSecretExtensions[base[dot+1:]]; ok {
			return true
		}
	}
	// `.env`, `.env.local`, `.env.production.local` — but not `.env.example`.
	if base == ".env" || strings.HasPrefix(base, ".env.") {
		for _, s := range envTemplateSuffixes {
			if base == ".env."+s || strings.HasSuffix(base, "."+s) || strings.HasPrefix(base, ".env."+s+".") {
				return false
			}
		}
		return true
	}
	// `local.env`, `prod.env` — the same file with the parts swapped.
	if strings.HasSuffix(base, ".env") {
		for _, s := range envTemplateSuffixes {
			if base == s+".env" {
				return false
			}
		}
		return true
	}
	return false
}
