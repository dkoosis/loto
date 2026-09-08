package store

import (
	_ "embed"
	"strings"
)

//go:embed schema.sql
var schemaSQLTemplate string

// eventKindCheckPlaceholder is the token schema.sql carries in place of a
// hand-typed events.event_kind CHECK list. schemaSQL below substitutes it
// with eventKindCheckSQL()'s render of allEventKinds (event_kinds.go) — the
// single place a fresh DB's CHECK clause and the Go event-kind declaration
// can go out of sync is removed by construction (loto-123y).
const eventKindCheckPlaceholder = "__EVENT_KIND_CHECK__"

// schemaSQL is the DDL migrate applies to a fresh (or re-opened) DB — schema.sql
// with eventKindCheckPlaceholder replaced by the live event-kind list. Computed
// once at package init: schema.sql is a compile-time embed, so the substitution
// input never changes at runtime.
var schemaSQL = strings.Replace(schemaSQLTemplate, eventKindCheckPlaceholder, eventKindCheckSQL(), 1)
