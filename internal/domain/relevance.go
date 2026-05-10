package domain

import "time"

type RelevanceKind int

const (
	RelevanceLockAcquire RelevanceKind = iota
	RelevanceLockRelease
	RelevanceInbox
)

func RelevantTags(tags []TagRecord, forAgent string, target Target, kind RelevanceKind, now time.Time, caseInsensitive bool) []TagRecord {
	out := make([]TagRecord, 0, len(tags))
	for _, tg := range tags {
		if tg.ExpiresAt != nil && !now.Before(*tg.ExpiresAt) {
			continue
		}
		if !Overlap(tg.Target, target, caseInsensitive) {
			continue
		}
		switch kind {
		case RelevanceLockAcquire:
			if tg.AddresseeUUID == forAgent || tg.AddresseeUUID == "" {
				out = append(out, tg)
			}
		case RelevanceLockRelease:
			if tg.AddresseeUUID != "" {
				out = append(out, tg)
			}
		case RelevanceInbox:
			if tg.AddresseeUUID == forAgent {
				out = append(out, tg)
			}
		}
	}
	return out
}
