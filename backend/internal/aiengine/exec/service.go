package exec

// service.go holds the executor's small helpers + the local channel / template
// / status constants. Channel and template strings mirror reach's values as
// bare local constants so exec stays decoupled from the reach package (CONTRACT
// §12.5.5: exec imports neither marketing / reach / catalog / groupbuy). The
// reach CHECK / template authority remains in reach; the values are frozen.

import (
	"github.com/zhangguijunshi/backend/internal/strategy"
)

// Reach channel codes (mirror reach.ChannelWework*). Used by the seckill /
// coupon_wake handlers when calling MessageSender.Send.
const (
	channelWeworkGroup = "wework_group"
	channelWeworkOneV  = "wework_1v1"
)

// Reach template codes (mirror reach.TplXxx). group_seckill uses recall_30;
// coupon_wake picks by ReasonType (birthday_care→birthday, wake_dormant→
// recall_30, festival_discount→repurchase, high_value_push→new_welcome).
const (
	tplRecall30   = "recall_30"
	tplBirthday   = "birthday"
	tplRepurchase = "repurchase"
	tplNewWelcome = "new_welcome"
)

// SendRef.Status values (mirror reach.StatusSent / StatusFailed). Used by
// classifySend.
const (
	statusSent   = "sent"
	statusFailed = "failed"
)

// couponTypeDiscount is the default coupon type when item.Params omits an
// explicit coupon_type / type (the most common seckill / wake coupon shape).
const couponTypeDiscount = "discount"

// ReasonType bare values consulted by coupon_wake's tpl selection (mirror the
// strategy_items.reason_type CHECK). exec reads ReasonType ONLY here, never for
// dispatch (CONTRACT §12.5.9).
const (
	reasonBirthdayCare     = "birthday_care"
	reasonWakeDormant      = "wake_dormant"
	reasonFestivalDiscount = "festival_discount"
	reasonHighValuePush    = "high_value_push"
)

// tplForWake maps a coupon_wake ReasonType to its reach template. Unknown
// ReasonType defaults to recall_30 (the safest wake-up template).
func tplForWake(reasonType string) string {
	switch reasonType {
	case reasonBirthdayCare:
		return tplBirthday
	case reasonWakeDormant:
		return tplRecall30
	case reasonFestivalDiscount:
		return tplRepurchase
	case reasonHighValuePush:
		return tplNewWelcome
	default:
		return tplRecall30
	}
}

// groupbuyTitle returns the group-buy title from Params["title"], falling back
// to the item Copy (≤40 chars, already content-filtered at output stage).
func groupbuyTitle(item *strategyItem) string {
	if item != nil {
		if t, ok := item.Params["title"].(string); ok && t != "" {
			return t
		}
		return item.Copy
	}
	return ""
}

// sendVars builds the reach render-vars map. It passes through Params["vars"]
// when the pipeline supplied a map[string]string; otherwise it seeds a minimal
// {"copy": item.Copy} so the template renders the boss-facing copy.
func sendVars(item *strategyItem) map[string]string {
	out := map[string]string{}
	if item != nil {
		if v, ok := item.Params["vars"]; ok {
			if m, ok := v.(map[string]string); ok {
				for k, v := range m {
					out[k] = v
				}
			}
		}
		if item.Copy != "" {
			out["copy"] = item.Copy
		}
	}
	return out
}

// paramInt64 extracts an int64 from a Params map. JSONB unmarshal produces
// float64 for numbers; in-memory callers may use int / int64. Returns 0 for
// missing / unparseable values.
func paramInt64(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch x := m[key].(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case int32:
		return int64(x)
	case float64:
		return int64(x)
	case float32:
		return int64(x)
	default:
		return 0
	}
}

// paramHours is paramInt64 narrowed to int (expire_hours / valid_hours etc.).
func paramHours(m map[string]any, key string) int {
	return int(paramInt64(m, key))
}

// stringParam extracts a string from Params, falling back to fallback when
// absent or empty.
func stringParam(m map[string]any, key, fallback string) string {
	if m == nil {
		return fallback
	}
	if v, ok := m[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

// strategyItem is a local alias for strategy.StrategyItem so service.go's
// helpers avoid repeating the full import path; the alias is resolved at the
// call sites (executor.go) where *strategy.StrategyItem is passed.
type strategyItem = strategy.StrategyItem
