package semantic

// signal_text.go builds the text key that the L2 semantic cache embeds
// (CONTRACT §13.3.4 / ch07 §7.6.2). The key is NOT the raw advice text — it is
// a compact "{business}|{signal dimensions}" string whose embedding captures
// the semantic shape of the tenant's "today". Two days that differ only in
// which specific SKU is nearing expiry ("临期葡萄" vs "临期苹果") map to
// near-identical signal texts and therefore near-identical embeddings, so they
// share one advice draft (§7.6.2).

import (
	"sort"
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
//  6. sales (M8-A, CONTRACT §21.3) — APPENDED ONLY when the bundle carries a
//     historical-sales signal (len(Bundle.Sales) > 0): a stable set/bucket-level
//     summary of the slow-moving / best-seller product sets and the up/down/flat
//     trend counts, e.g. "|sales=slow:1,5,9;best:3;up:2,down:1,flat:4". This is
//     the sales/clearance axis the rule router branches on (滞销清仓 vs 畅销补货).
//     When no sales signal is present the dimension is omitted entirely, so the
//     key is byte-for-byte identical to the pre-M8 form — callers that do not
//     supply sales data suffer no spurious cache invalidation.
//
// The dimensions are deliberately the SIGNAL axes the rule router branches on
// (clearance / rain delivery / wake dormant / festival discount / slow-moving
// clearance), not the advice output, so semantically equivalent signals collapse
// onto one key. A nil bundle yields "" (the cache treats empty signal text as a
// guaranteed miss, which is the safe default).
func BuildSignalText(b *signal.Bundle) string {
	if b == nil {
		return ""
	}
	parts := make([]string, 0, 6)
	parts = append(parts, normBusiness(b.BusinessType))
	parts = append(parts, "weather="+normWeather(b.Weather.Condition))
	parts = append(parts, "days_to_expiry="+minDaysToExpiry(b.Inventory))
	parts = append(parts, "sleep_30="+strconv.Itoa(b.Members.Sleep30))
	parts = append(parts, "holiday="+normHoliday(b.Holiday))
	// M8-A (§21.3): fold the sales/slow-moving dimension into the key, but only
	// when a sales signal exists — an empty summary leaves the legacy key intact.
	if s := salesSummary(b.Sales); s != "" {
		parts = append(parts, "sales="+s)
	}
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

// salesSummary renders the M8-A sales/slow-moving dimension of the cache key
// (CONTRACT §21.3) as a STABLE, set/bucket-level summary of the bundle's
// historical-sales signal:
//
//	"slow:1,5,9;best:3;up:2,down:1,flat:4"
//
// It deliberately avoids the jittery exact quantities (TotalQty/RecentQty/
// PriorQty/ActiveDays) — those shift day to day and would make the key churn so
// the semantic cache never hits. Instead it captures the axes the rule router
// actually branches on:
//
//   - slow — the sorted set of slow-moving product ids (滞销清仓 axis);
//   - best — the sorted set of best-seller product ids (畅销补货 axis);
//   - up/down/flat — the COUNT of products in each trend bucket (direction of
//     the store's sales momentum, not its magnitude).
//
// Product-id sets are sorted ascending for reproducibility (the same input
// always yields the same text regardless of slice order). An empty/nil sales
// slice yields "" so BuildSignalText omits the dimension entirely and the key
// stays byte-for-byte identical to the pre-M8 form (backward compatibility: no
// sales signal ⇒ no spurious cache invalidation).
func salesSummary(sales []signal.SalesSig) string {
	if len(sales) == 0 {
		return ""
	}
	slow := make([]int64, 0)
	best := make([]int64, 0)
	var up, down, flat int
	for _, s := range sales {
		if s.SlowMoving {
			slow = append(slow, s.ProductID)
		}
		if s.BestSeller {
			best = append(best, s.ProductID)
		}
		switch s.Trend {
		case signal.TrendUp:
			up++
		case signal.TrendDown:
			down++
		default: // TrendFlat or any unknown value → flat bucket
			flat++
		}
	}
	sort.Slice(slow, func(i, j int) bool { return slow[i] < slow[j] })
	sort.Slice(best, func(i, j int) bool { return best[i] < best[j] })

	var sb strings.Builder
	sb.WriteString("slow:")
	sb.WriteString(joinIDs(slow))
	sb.WriteString(";best:")
	sb.WriteString(joinIDs(best))
	sb.WriteString(";up:")
	sb.WriteString(strconv.Itoa(up))
	sb.WriteString(",down:")
	sb.WriteString(strconv.Itoa(down))
	sb.WriteString(",flat:")
	sb.WriteString(strconv.Itoa(flat))
	return sb.String()
}

// joinIDs renders a sorted product-id set as a comma-joined string, or "none"
// for an empty set (keeps the dimension unambiguous and non-empty).
func joinIDs(ids []int64) string {
	if len(ids) == 0 {
		return "none"
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}
