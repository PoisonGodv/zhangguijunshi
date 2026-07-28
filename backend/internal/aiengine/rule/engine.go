package rule

import (
	"context"
	"fmt"
	"strings"

	"github.com/zhangguijunshi/backend/internal/aiengine/signal"
)

// Engine routes a signal Bundle through the rule set to a Routed result.
type Engine interface {
	Route(ctx context.Context, b *signal.Bundle) (*Routed, error)
}

// inventoryLinkedActions bind to a concrete product and must pass the margin
// guardrail per matching inventory item (group_seckill / price_adj). All other
// ActionTypes are placeholder (single占位 RoutedItem, no product binding).
var inventoryLinkedActions = map[string]bool{
	ActGroupSeckill: true,
	ActPriceAdj:     true,
}

// Route never returns an error. A panic is recovered to an empty Routed; zero
// matches yield an empty Routed.Items (the Pipeline injects cold-start).
func (s *Service) Route(_ context.Context, b *signal.Bundle) (routed *Routed, err error) {
	defer func() {
		if r := recover(); r != nil {
			routed = &Routed{TenantID: bundleTenant(b), Items: []RoutedItem{}}
			err = nil
		}
	}()

	if b == nil {
		return &Routed{TenantID: 0, Items: []RoutedItem{}}, nil
	}

	items := make([]RoutedItem, 0)
	for _, rl := range s.rules {
		actionType, ok := ReasonToAction[rl.Action.ReasonType]
		if !ok {
			continue // unknown reason: skip defensively
		}
		if !businessMatch(rl.Business, b.BusinessType) {
			continue
		}
		if !condsAllMatch(rl.Conds, b) {
			continue
		}
		items = append(items, s.emit(rl, actionType, b)...)
	}
	return &Routed{TenantID: b.TenantID, Items: items}, nil
}

func bundleTenant(b *signal.Bundle) int64 {
	if b == nil {
		return 0
	}
	return b.TenantID
}

// emit produces RoutedItems for a matched rule: per-inventory-item for
// inventory-linked actions (each through ApplyGuardrail), or a single
// placeholder RoutedItem otherwise.
func (s *Service) emit(rl Rule, actionType string, b *signal.Bundle) []RoutedItem {
	if inventoryLinkedActions[actionType] {
		out := make([]RoutedItem, 0)
		for _, inv := range selectHitInventory(rl, b.Inventory) {
			item, ok := ApplyGuardrail(inv, rl, s.guard)
			if ok {
				out = append(out, item)
			}
		}
		return out
	}
	return []RoutedItem{s.emitPlaceholder(rl, actionType)}
}

func (s *Service) emitPlaceholder(rl Rule, actionType string) RoutedItem {
	return RoutedItem{
		RuleID:         rl.ID,
		ReasonType:     rl.Action.ReasonType,
		ActionType:     actionType,
		Target:         rl.Action.Target,
		ProductID:      0,
		Product:        "-",
		CostCents:      0,
		PriceCents:     0,
		CostCeilCents:  0,
		MarginFloorPct: s.guard.MarginFloorPct,
		Priority:       rl.Priority,
		Reason:         fmt.Sprintf("命中规则%s: %s (满减/触达类, 非商品绑定)", rl.ID, actionType),
		ParamRange:     rl.Action.ParamRange,
	}
}

// ===== business match =====

func businessMatch(ruleBusiness, bundleBusiness string) bool {
	rb := strings.ToLower(strings.TrimSpace(ruleBusiness))
	if rb == "" || rb == "*" {
		return true
	}
	return rb == strings.ToLower(strings.TrimSpace(bundleBusiness))
}

// ===== conds AND =====

func condsAllMatch(conds []Cond, b *signal.Bundle) bool {
	if len(conds) == 0 {
		return true
	}
	for _, c := range conds {
		if !matchCond(c, b) {
			return false
		}
	}
	return true
}

func matchCond(c Cond, b *signal.Bundle) bool {
	field := strings.TrimSpace(c.Field)
	op := strings.ToLower(strings.TrimSpace(c.Op))
	if field == "" {
		return false
	}
	head, tail, _ := strings.Cut(strings.ToLower(field), ".")
	switch head {
	case "weather":
		return compareScalar(weatherField(b.Weather, tail), op, c.Value)
	case "holiday":
		return compareScalar(holidayField(b.Holiday, tail), op, c.Value)
	case "members":
		return matchMembersCond(b.Members, tail, op, c.Value)
	case "inventory":
		return matchInventoryAny(b.Inventory, tail, op, c.Value)
	}
	return false // unknown prefix: conservative miss
}

// ===== field accessors =====

func weatherField(w signal.Weather, tail string) any {
	switch tail {
	case "condition":
		return w.Condition
	case "temp_c":
		return w.TempC
	case "forecast":
		return w.Forecast
	}
	return nil
}

func holidayField(h signal.Holiday, tail string) any {
	switch tail {
	case "name":
		return h.Name
	case "days_to":
		return h.DaysTo
	case "is_festival":
		return h.IsFestival
	}
	return nil
}

func inventoryField(inv signal.InventorySig, tail string) any {
	switch tail {
	case "product_id":
		return inv.ProductID
	case "name":
		return inv.Name
	case "cost_cents":
		return inv.CostCents
	case "price_cents":
		return inv.PriceCents
	case "stock":
		return inv.Stock
	case "days_to_expiry":
		return inv.DaysToExpiry
	case "slow_moving":
		return inv.SlowMoving
	case "high_margin":
		return inv.HighMargin
	}
	return nil
}

// ===== members: scalar or list =====

func matchMembersCond(m signal.MemberStats, tail, op string, value any) bool {
	switch tail {
	case "sleep_30":
		return compareScalar(m.Sleep30, op, value)
	case "total":
		return compareScalar(m.Total, op, value)
	case "high_value":
		return matchListCond(m.HighValue, op, value)
	case "birthday_today":
		return matchListCond(m.BirthdayToday, op, value)
	}
	return false
}

// matchListCond handles list-typed member fields. op "in" + value "nonempty"
// (or op "exists") means the list is non-empty.
func matchListCond(list []int64, op string, value any) bool {
	if s, ok := value.(string); ok && s == "nonempty" {
		return len(list) > 0
	}
	switch op {
	case "in":
		return int64InList(list, value)
	case "exists":
		return len(list) > 0
	}
	return false
}

func int64InList(list []int64, value any) bool {
	switch v := value.(type) {
	case []int64:
		for _, x := range list {
			for _, y := range v {
				if x == y {
					return true
				}
			}
		}
	case []int:
		for _, x := range list {
			for _, y := range v {
				if x == int64(y) {
					return true
				}
			}
		}
	case []any:
		for _, x := range list {
			for _, y := range v {
				if f, ok := toFloat(y); ok && f == float64(x) {
					return true
				}
			}
		}
	}
	return false
}

// ===== inventory: any item matching =====

func matchInventoryAny(inv []signal.InventorySig, tail, op string, value any) bool {
	if len(inv) == 0 {
		return false
	}
	for _, item := range inv {
		if compareScalar(inventoryField(item, tail), op, value) {
			return true
		}
	}
	return false
}

// selectHitInventory returns the inventory subset that matches ALL of the
// rule's inventory.* conds (AND). If the rule has no inventory.* cond, fall
// back to the full list (defensive — linked rules should carry one).
func selectHitInventory(rl Rule, inventory []signal.InventorySig) []signal.InventorySig {
	var invConds []Cond
	for _, c := range rl.Conds {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(c.Field)), "inventory.") {
			invConds = append(invConds, c)
		}
	}
	if len(invConds) == 0 {
		return inventory
	}
	out := make([]signal.InventorySig, 0)
	for _, inv := range inventory {
		match := true
		for _, c := range invConds {
			_, tail, _ := strings.Cut(strings.ToLower(strings.TrimSpace(c.Field)), ".")
			op := strings.ToLower(strings.TrimSpace(c.Op))
			if !compareScalar(inventoryField(inv, tail), op, c.Value) {
				match = false
				break
			}
		}
		if match {
			out = append(out, inv)
		}
	}
	return out
}

// ===== scalar comparison =====

func compareScalar(actual any, op string, value any) bool {
	switch op {
	case "eq":
		return scalarEqual(actual, value)
	case "exists":
		return scalarExists(actual)
	case "in":
		return scalarIn(actual, value)
	case "between":
		lo, hi, ok := unpackPair(value)
		if !ok {
			return false
		}
		a, ok := toFloat(actual)
		if !ok {
			return false
		}
		return a >= lo && a <= hi
	case "lt", "gt", "gte", "lte":
		a, ok1 := toFloat(actual)
		v, ok2 := toFloat(value)
		if !ok1 || !ok2 {
			return false
		}
		switch op {
		case "lt":
			return a < v
		case "gt":
			return a > v
		case "gte":
			return a >= v
		case "lte":
			return a <= v
		}
	}
	return false
}

func scalarEqual(actual, value any) bool {
	if ab, ok := actual.(bool); ok {
		if vb, ok2 := value.(bool); ok2 {
			return ab == vb
		}
		return false
	}
	if as, ok := actual.(string); ok {
		if vs, ok2 := value.(string); ok2 {
			return as == vs
		}
		return false
	}
	if af, ok := toFloat(actual); ok {
		if vf, ok2 := toFloat(value); ok2 {
			return af == vf
		}
	}
	return false
}

func scalarExists(actual any) bool {
	if actual == nil {
		return false
	}
	switch x := actual.(type) {
	case string:
		return x != ""
	case bool:
		return x
	}
	if f, ok := toFloat(actual); ok {
		return f != 0
	}
	return false
}

func scalarIn(actual, value any) bool {
	switch v := value.(type) {
	case []string:
		if as, ok := actual.(string); ok {
			for _, x := range v {
				if x == as {
					return true
				}
			}
		}
		return false
	case []int:
		if f, ok := toFloat(actual); ok {
			for _, x := range v {
				if float64(x) == f {
					return true
				}
			}
		}
		return false
	case []int64:
		if f, ok := toFloat(actual); ok {
			for _, x := range v {
				if float64(x) == f {
					return true
				}
			}
		}
		return false
	case []float64:
		if f, ok := toFloat(actual); ok {
			for _, x := range v {
				if x == f {
					return true
				}
			}
		}
		return false
	case []any:
		for _, x := range v {
			if scalarEqual(actual, x) {
				return true
			}
		}
		return false
	}
	return false
}

func unpackPair(value any) (float64, float64, bool) {
	switch v := value.(type) {
	case []int:
		if len(v) < 2 {
			return 0, 0, false
		}
		return float64(v[0]), float64(v[1]), true
	case []int64:
		if len(v) < 2 {
			return 0, 0, false
		}
		return float64(v[0]), float64(v[1]), true
	case []float64:
		if len(v) < 2 {
			return 0, 0, false
		}
		return v[0], v[1], true
	case []any:
		if len(v) < 2 {
			return 0, 0, false
		}
		lo, ok1 := toFloat(v[0])
		hi, ok2 := toFloat(v[1])
		if !ok1 || !ok2 {
			return 0, 0, false
		}
		return lo, hi, true
	}
	return 0, 0, false
}

// toFloat coerces numeric kinds to float64; bool and nil are rejected (a bool
// is an int sub-kind in Go and must be excluded explicitly).
func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case int32:
		return float64(x), true
	case bool:
		return 0, false
	case nil:
		return 0, false
	}
	return 0, false
}
