// Package domain holds the pure-function core of loto v2: target canonicalization,
// overlap detection, staleness rules, authorization, and tag-relevance filtering.
//
// No imports of database/sql, os, path/filepath beyond Clean/ToSlash, or anything
// with side effects. Tests of this package never touch disk or shell.
package domain
