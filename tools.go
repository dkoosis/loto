//go:build tools

// Package tools pins build-only tool dependencies into the module graph so
// `go mod tidy` keeps them, without letting them enter the loto binary.
//
// The lone entry is the ruleguard DSL: .golangci-rules/bugclasses.go (the
// shared bugclasses pack, ccp-sbp) imports it, but that file lives in a
// dot-directory the Go toolchain ignores — so tidy never sees the import and
// would drop dsl from go.mod, leaving golangci-lint's embedded ruleguard unable
// to typecheck the rules file (its importer resolves only from the module
// graph, not $GOROOT/$GOPATH). This blank import is the bridge. The `tools`
// build tag keeps the file out of every normal build.
package tools

import _ "github.com/quasilyte/go-ruleguard/dsl"
