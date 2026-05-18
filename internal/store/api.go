// Package-level type compile-time assertions used to live here as
// LockOps / Health interfaces. Within an internal/ package with a
// single impl (*Store) and no test fakes, ISP interfaces were pure
// overhead — callers now hold *store.Store directly.
//
// EventLog stays out of api.go for the same reason: its only consumer
// today is *Store itself (lock ops emit audit events transactionally).
// Reintroduce a role interface here when an external consumer appears.

package store
