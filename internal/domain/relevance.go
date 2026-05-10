package domain

import "time"

type RelevanceKind int

const (
	RelevanceLockAcquire RelevanceKind = iota
	RelevanceLockRelease
	RelevanceInbox
)

func RelevantTags(tags []TagRecord, forAgent string, target Target, kind RelevanceKind, now time.Time, caseInsensitive bool) []TagRecord {
	accept := relevanceAcceptor(kind, forAgent)
	out := make([]TagRecord, 0, len(tags))
	for i := range tags {
		tg := &tags[i]
		if tg.ExpiresAt != nil && !now.Before(*tg.ExpiresAt) {
			continue
		}
		if !Overlap(tg.Target, target, caseInsensitive) {
			continue
		}
		if accept(tg) {
			out = append(out, *tg)
		}
	}
	return out
}

func relevanceAcceptor(kind RelevanceKind, forAgent string) func(*TagRecord) bool {
	switch kind {
	case RelevanceLockAcquire:
		return func(tg *TagRecord) bool {
			return tg.AddresseeUUID == forAgent || tg.AddresseeUUID == ""
		}
	case RelevanceLockRelease:
		return func(tg *TagRecord) bool { return tg.AddresseeUUID != "" }
	case RelevanceInbox:
		return func(tg *TagRecord) bool { return tg.AddresseeUUID == forAgent }
	}
	return func(*TagRecord) bool { return false }
}
