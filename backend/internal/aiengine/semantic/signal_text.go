package semantic

// signal_text.go builds the text key that the L2 semantic cache embeds
// (CONTRACT §13.3.4 / ch07 §7.6.2). The key is NOT the raw advice text — it is
// a compact "{business}|{signal dimensions}" string whose embedding captures
// the semantic shape of the tenant's "today". Two days that differ only in
// which specific SKU is nearing expiry ("临期葡萄" vs "临期苹果") map to
// near-identical signal texts and therefore near-identical embeddings, so they
// share one advice draft (§7.6.2).

import (
	"strconv"
	"strings"

	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
)

// BuildSignalText renders a signal.Bundle as the cache key text
// (CONTRACT §13.3.4 / §7.6.2):
//
//	"fruit|weather=rain|days_to_expiry=2|sleep_30=18|holiday=none"
//
// Dimensions, in order:
//  1. business — the tenant's business type (fruit|convenience|maternity|...).
//  2. weather — the weather condition (rain|hot|cold|snow|normal), "none" if
//     unset; this is the key weather axis (rain delivery vs clearance etc.).
//  3. days_to_expiry — the minimum days_to_expiry among nearing-expiry inventory
//     (>= 0), "none" if no item is nearing expiry; this is the urgency axis.
//  4. sleep_30 — the count of members who have not visited in 30 days; this is
//     the dormant-wake axis.
//  5. holiday — the upcoming holiday name, "none" if no festival within range;
//     this is the festival-discount axis.
//
// The dimensions are deliberately the SIGNAL axes the rule router branches on
// (clearance / rain delivery / wake dormant / festival discount), not the
// advice output, so semantically equivalent signals collapse onto one key. A
// nil bundle yields "" (the cache treats empty signal text as a guaranteed
// miss, which is the safe default).
func BuildSignalText(b *signal.Bundle) string {
	if b == nil {
		return ""
	}
	parts := make([]string, 0, 5)
	parts = append(parts, normBusiness(b.BusinessType))
	parts = append(parts, "weather="+normWeather(b.Weather.Condition))
	parts = append(parts, "days_to_expiry="+minDaysToExpiry(b.Inventory))
	parts = append(parts, "sleep_30="+strconv.Itoa(b.Members.Sleep30))
	parts = append(parts, "holiday="+normHoliday(b.Holiday))
	return strings.Join(parts, "|")
}

// normBusiness lowercases/trims the business type; empty → "unknown" so a
// misconfigured tenant does not collapse onto another business's cache space.
func normBusiness(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return "unknown"
	}
	return s
}

// normWeather canonicalises the weather condition; empty/whitespace → "none".
// The value is already an enum from signal.Weather (rain|hot|cold|snow|normal),
// but defensive trimming guards against future adapters.
func normWeather(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return "none"
	}
	return s
}

// minDaysToExpiry returns the string form of the minimum days_to_expiry among
// nearing-expiry inventory items (DaysToExpiry >= 0). -1 means "not nearing
// expiry" in the signal contract (signal.InventorySig), so it is excluded.
// Returns "none" when no item is nearing expiry.
func minDaysToExpiry(inv []signal.InventorySig) string {
	min := -1
	for _, it := range inv {
		if it.DaysToExpiry < 0 {
			continue // -1 == not nearing expiry (signal contract)
		}
		if min == -1 || it.DaysToExpiry < min {
			min = it.DaysToExpiry
		}
	}
	if min < 0 {
		return "none"
	}
	return strconv.Itoa(min)
}

// normHoliday renders the holiday dimension: the holiday name when a festival
// is within range, else "none". signal.Holiday uses DaysTo == -1 and
// IsFestival == false (or an empty Name) to mean "no upcoming festival".
func normHoliday(h signal.Holiday) string {
	name := strings.TrimSpace(h.Name)
	if name == "" || !h.IsFestival && h.DaysTo < 0 {
		return "none"
	}
	return strings.ToLower(name)
}
