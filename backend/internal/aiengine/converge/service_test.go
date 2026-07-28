package converge

import (
	"context"
	"testing"

	"github.com/zhangguijunshi/backend/internal/aiengine/agent"
	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
)

func ei(product, effect string, priority int, marginOK bool) agent.EnhancedItem {
	return agent.EnhancedItem{
		Routed:         rule.RoutedItem{Product: product, Priority: priority, ActionType: rule.ActGroupSeckill},
		ExpectedEffect: effect,
		MarginOK:       marginOK,
	}
}

// TestPickTop3_FiveIntoThree: 5 items, distinct scores → top 3 by score wins.
func TestPickTop3_FiveIntoThree(t *testing.T) {
	enhanced := &agent.Enhanced{TenantID: 1, Items: []agent.EnhancedItem{
		ei("A", "核销10份", 10, true),
		ei("B", "核销50份", 20, true),
		ei("C", "核销30份", 30, true),
		ei("D", "核销5份", 40, true),
		ei("E", "核销100份", 5, true),
	}}
	got := NewTop3().PickTop3(context.Background(), enhanced)
	if len(got) != 3 {
		t.Fatalf("expected 3, got %d", len(got))
	}
	// scores: B=50, E=100, C=30 → top3 by score = E(100), B(50), C(30)
	want := []string{"E", "B", "C"}
	for i, w := range want {
		if got[i].Routed.Product != w {
			t.Errorf("rank %d = %q, want %q", i+1, got[i].Routed.Product, w)
		}
	}
}

// TestPickTop3_SamePriorityStable: equal score + equal priority keeps input order.
func TestPickTop3_SamePriorityStable(t *testing.T) {
	enhanced := &agent.Enhanced{Items: []agent.EnhancedItem{
		ei("first", "核销10份", 50, true),
		ei("second", "核销10份", 50, true),
		ei("third", "核销10份", 50, true),
	}}
	got := NewTop3().PickTop3(context.Background(), enhanced)
	if len(got) != 3 {
		t.Fatalf("expected 3, got %d", len(got))
	}
	want := []string{"first", "second", "third"}
	for i, w := range want {
		if got[i].Routed.Product != w {
			t.Errorf("rank %d = %q, want %q (stable)", i+1, got[i].Routed.Product, w)
		}
	}
}

// TestPickTop3_PriorityTiebreak: equal score, different priority → higher priority first.
func TestPickTop3_PriorityTiebreak(t *testing.T) {
	enhanced := &agent.Enhanced{Items: []agent.EnhancedItem{
		ei("low", "核销10份", 10, true),
		ei("high", "核销10份", 90, true),
	}}
	got := NewTop3().PickTop3(context.Background(), enhanced)
	if len(got) != 2 || got[0].Routed.Product != "high" {
		t.Errorf("priority tiebreak failed: %+v", got)
	}
}

// TestPickTop3_MarginFalseSinksTail: a margin_ok=false item with a high score
// still sinks below any margin_ok=true item (排尾不丢).
func TestPickTop3_MarginFalseSinksTail(t *testing.T) {
	enhanced := &agent.Enhanced{Items: []agent.EnhancedItem{
		ei("blocked", "核销1000份", 99, false), // huge score but margin_ok=false
		ei("ok1", "核销1份", 1, true),
		ei("ok2", "核销2份", 1, true),
	}}
	got := NewTop3().PickTop3(context.Background(), enhanced)
	if len(got) != 3 {
		t.Fatalf("expected 3 (blocked not dropped), got %d", len(got))
	}
	if got[2].MarginOK {
		t.Errorf("margin_ok=false item must be ranked last, got order %+v", got)
	}
	if got[2].Routed.Product != "blocked" {
		t.Errorf("blocked item should be at rank 3, got %+v", got)
	}
}

// TestPickTop3_EmptyAndNil: nil or empty Enhanced → nil (no panic).
func TestPickTop3_EmptyAndNil(t *testing.T) {
	c := NewTop3()
	if got := c.PickTop3(context.Background(), nil); got != nil {
		t.Errorf("nil input -> %+v, want nil", got)
	}
	if got := c.PickTop3(context.Background(), &agent.Enhanced{}); got != nil {
		t.Errorf("empty input -> %+v, want nil", got)
	}
}

// TestPickTop3_FewerThanThree: 2 inputs return 2.
func TestPickTop3_FewerThanThree(t *testing.T) {
	enhanced := &agent.Enhanced{Items: []agent.EnhancedItem{
		ei("only", "核销5份", 5, true),
		ei("two", "核销8份", 6, true),
	}}
	got := NewTop3().PickTop3(context.Background(), enhanced)
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
}

// TestScore_WeightedKeywords covers each keyword weight + range lower-bound.
func TestScore_WeightedKeywords(t *testing.T) {
	cases := []struct {
		text string
		want float64
	}{
		{"核销35份 挽回损耗¥180", 35*1.0 + 180*0.01},
		{"到店6人", 6 * 0.5},
		{"召回4-6人", 4 * 0.8}, // range → lower bound
		{"", 0},             // empty
		{"无关键词", 0},         // unparsable
		{"核销10份 到店4人 挽回¥200 召回2人", 10*1.0 + 4*0.5 + 200*0.01 + 2*0.8},
	}
	for _, tc := range cases {
		if got := score(&agent.EnhancedItem{ExpectedEffect: tc.text}); got != tc.want {
			t.Errorf("score(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}
