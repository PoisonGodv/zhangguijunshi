package converge

import (
	"context"
	"sort"

	"github.com/zhangguijunshi/backend/internal/aiengine/agent"
)

// Service implements Top3. It is stateless; NewTop3 exists for symmetric
// construction and future option injection.
type Service struct{}

// NewTop3 builds a Converge Top3.
func NewTop3() *Service { return &Service{} }

// PickTop3 sorts the EnhancedItems by (margin_ok DESC, score DESC, priority
// DESC) and returns the first 3. margin_ok=false items sink to the tail but
// remain eligible (CONTRACT §12.3.6 "排尾不丢"). Fewer than 3 inputs return as
// many as exist. The returned slice holds pointers into the caller's items; the
// pipeline does not mutate Enhanced after Converge, so this is safe.
func (s *Service) PickTop3(_ context.Context, e *agent.Enhanced) []*agent.EnhancedItem {
	if e == nil || len(e.Items) == 0 {
		return nil
	}
	out := make([]*agent.EnhancedItem, len(e.Items))
	for i := range e.Items {
		out[i] = &e.Items[i]
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		// margin_ok=true group first.
		if a.MarginOK != b.MarginOK {
			return a.MarginOK
		}
		// then higher expected-effect score.
		sa, sb := score(a), score(b)
		if sa != sb {
			return sa > sb
		}
		// then higher rule priority.
		return a.Routed.Priority > b.Routed.Priority
	})
	if len(out) > 3 {
		out = out[:3]
	}
	return out
}
