package converge

import (
	"context"
	"regexp"
	"strconv"

	"github.com/zhangguijunshi/backend/internal/aiengine/agent"
)

// Top3 is the Converge stage port. PickTop3 never panics; on a nil/empty
// Enhanced it returns nil. It does not mutate the caller's Enhanced.Items.
type Top3 interface {
	PickTop3(ctx context.Context, e *agent.Enhanced) []*agent.EnhancedItem
}

// weightedPattern binds an expected-effect keyword to its score weight and the
// regex that lifts the first number following the keyword. Ranges ("召回4-6人")
// yield the lower bound via the first match, matching the demo's conservative
// read.
type weightedPattern struct {
	weight float64
	re     *regexp.Regexp
}

var weightedPatterns = []weightedPattern{
	{1.0, regexp.MustCompile(`核销\D*?(\d+\.?\d*)`)},
	{0.5, regexp.MustCompile(`到店\D*?(\d+\.?\d*)`)},
	{0.01, regexp.MustCompile(`挽回\D*?(\d+\.?\d*)`)},
	{0.8, regexp.MustCompile(`召回\D*?(\d+\.?\d*)`)},
}

// score parses an EnhancedItem.ExpectedEffect and returns the weighted sum of
// the numbers found after each keyword. An empty / unparseable effect scores 0.
func score(ei *agent.EnhancedItem) float64 {
	if ei == nil {
		return 0
	}
	text := ei.ExpectedEffect
	if text == "" {
		return 0
	}
	total := 0.0
	for _, wp := range weightedPatterns {
		for _, m := range wp.re.FindAllStringSubmatch(text, -1) {
			if len(m) < 2 {
				continue
			}
			if n, err := strconv.ParseFloat(m[1], 64); err == nil {
				total += n * wp.weight
			}
		}
	}
	return total
}
