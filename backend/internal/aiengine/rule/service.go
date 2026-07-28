package rule

import (
	"sort"

	"github.com/zhangguijunshi/backend/internal/margin"
)

// Service implements Engine. Rules are filtered (Enable=true) and stably sorted
// by Priority desc at construction. guard is the common margin-floor source
// (CONTRACT §12.5.4: rule/agent/exec share one margin.Guard injected here).
type Service struct {
	rules []Rule
	guard margin.Guard
}

// Option configures a Service.
type Option func(*Service)

// NewEngine builds an Engine. rules are filtered (Enable=true) and stably
// sorted by Priority descending (larger first; ties keep seed order). guard is
// the shared margin guard used by ApplyGuardrail at emit time.
func NewEngine(rules []Rule, guard margin.Guard, _ ...Option) *Service {
	s := &Service{guard: guard}
	s.rules = sortByPriorityDesc(filterEnabled(rules))
	return s
}

func filterEnabled(rules []Rule) []Rule {
	out := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if r.Enable {
			out = append(out, r)
		}
	}
	return out
}

func sortByPriorityDesc(rules []Rule) []Rule {
	out := make([]Rule, len(rules))
	copy(out, rules)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Priority > out[j].Priority })
	return out
}
