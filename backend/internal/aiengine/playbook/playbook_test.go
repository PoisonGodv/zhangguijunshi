package playbook

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/aiengine/rule"
)

// ===== NewStore 种子 + Get/List (DoD §14.6 A2-①) =====

// TestNewStoreSeedsAllFiveBusinesses 断言 NewStore 种入恰好 5 业态, 每个都能 Get 命中.
func TestNewStoreSeedsAllFiveBusinesses(t *testing.T) {
	s := NewStore()
	require.NotNil(t, s)

	for _, b := range []string{Fruit, Convenience, Maternity, Fresh, Apparel} {
		pb, ok := s.Get(b)
		require.True(t, ok, "business %s should be seeded", b)
		require.NotNil(t, pb)
		assert.Equal(t, b, pb.Business)
	}
}

// TestNewStoreGetUnknownBusinessReturnsFalse 未知业态 → (*Playbook)(nil), false, 不 panic.
func TestNewStoreGetUnknownBusinessReturnsFalse(t *testing.T) {
	s := NewStore()

	pb, ok := s.Get("restaurant")
	assert.False(t, ok)
	assert.Nil(t, pb)

	// 空串同样未知.
	pb, ok = s.Get("")
	assert.False(t, ok)
	assert.Nil(t, pb)
}

// TestStoreListReturnsAllFive List 返回 5 业态, 覆盖全部常量, 无 nil 项.
func TestStoreListReturnsAllFive(t *testing.T) {
	s := NewStore()
	books := s.List()
	require.Len(t, books, 5)

	seen := make(map[string]bool, 5)
	for _, pb := range books {
		require.NotNil(t, pb)
		seen[pb.Business] = true
	}
	for _, b := range []string{Fruit, Convenience, Maternity, Fresh, Apparel} {
		assert.True(t, seen[b], "List should include %s", b)
	}
}

// TestStoreListOrderIsStable List 顺序固定 (allBusinesses 定义序), 便于稳定序列化.
func TestStoreListOrderIsStable(t *testing.T) {
	s := NewStore()
	books := s.List()
	require.Len(t, books, 5)
	want := []string{Fruit, Convenience, Maternity, Fresh, Apparel}
	for i, b := range want {
		assert.Equal(t, b, books[i].Business)
	}
}

// ===== 每 Playbook 字段齐 (DoD §14.6 A2-②) =====

// TestEachPlaybookHasCompleteFields 遍历 5 业态, 断言 Rules≥2 / Prompts 非空 /
// SOP[4] 各 Name+Play 非空 / ColdPriority≥2 / LossRate,CashCoef>0.
func TestEachPlaybookHasCompleteFields(t *testing.T) {
	s := NewStore()
	for _, pb := range s.List() {
		require.NotNil(t, pb, "playbook must not be nil")

		assert.GreaterOrEqual(t, len(pb.Rules), 2, "%s Rules>=2", pb.Business)
		assert.NotEmpty(t, pb.Prompts, "%s Prompts non-empty", pb.Business)
		assert.GreaterOrEqual(t, len(pb.ColdPriority), 2, "%s ColdPriority>=2", pb.Business)
		assert.Greater(t, pb.LossRate, 0.0, "%s LossRate>0", pb.Business)
		assert.Greater(t, pb.CashCoef, 0.0, "%s CashCoef>0", pb.Business)

		require.Len(t, pb.SOP, 4, "%s SOP must be 4 stages", pb.Business)
		for i, st := range pb.SOP {
			assert.NotEmpty(t, st.Name, "%s SOP[%d].Name", pb.Business, i)
			assert.NotEmpty(t, st.Play, "%s SOP[%d].Play", pb.Business, i)
		}
	}
}

// TestPlaybookRulesMatchBusiness 每业态 Rules 的 Business 字段都等于该业态 (过滤正确).
func TestPlaybookRulesMatchBusiness(t *testing.T) {
	s := NewStore()
	for _, pb := range s.List() {
		for _, r := range pb.Rules {
			assert.Equal(t, pb.Business, r.Business, "rule %s leaked into %s", r.ID, pb.Business)
		}
	}
}

// TestLossRateCashCoefTable 校验 §14.3.2 表的精确 LossRate/CashCoef 值.
func TestLossRateCashCoefTable(t *testing.T) {
	s := NewStore()
	cases := map[string]struct {
		loss float64
		cash float64
	}{
		Fruit:       {0.10, 1.2},
		Convenience: {0.015, 1.2},
		Maternity:   {0.02, 1.2},
		Fresh:       {0.10, 1.2},
		Apparel:     {0.03, 1.3},
	}
	for b, want := range cases {
		pb, ok := s.Get(b)
		require.True(t, ok, "seeded %s", b)
		assert.InDelta(t, want.loss, pb.LossRate, 1e-9, "%s LossRate", b)
		assert.InDelta(t, want.cash, pb.CashCoef, 1e-9, "%s CashCoef", b)
	}
}

// TestColdPriorityMatchesContract 校验 §14.3.2 各业态 ColdPriority 精确内容.
func TestColdPriorityMatchesContract(t *testing.T) {
	s := NewStore()
	cases := map[string][]string{
		Fruit:       {rule.ActMomentsPost, rule.ActGroupSeckill, rule.ActGroupbuy},
		Convenience: {rule.ActCouponWake, rule.ActMomentsPost},
		Maternity:   {rule.ActCouponWake, rule.ActRechargePromo},
		Fresh:       {rule.ActGroupbuy, rule.ActGroupSeckill},
		Apparel:     {rule.ActCouponWake, rule.ActGroupbuy},
	}
	for b, want := range cases {
		pb, ok := s.Get(b)
		require.True(t, ok, "seeded %s", b)
		assert.Equal(t, want, pb.ColdPriority, "%s ColdPriority", b)
	}
}

// TestSOPStageNames 校验 SOP 四阶段名依次为 拉群/促活/转化/裂变.
func TestSOPStageNames(t *testing.T) {
	s := NewStore()
	wantNames := []string{"拉群", "促活", "转化", "裂变"}
	for _, pb := range s.List() {
		require.Len(t, pb.SOP, 4)
		for i, name := range wantNames {
			assert.Equal(t, name, pb.SOP[i].Name, "%s SOP[%d]", pb.Business, i)
		}
	}
}

// TestPromptsKeyedByActionTypeWithPlaceholders Prompts 的 key 是合法 action_type,
// 且模板含 {product}/{price} 占位 (§14.3.2 变量占位约定).
func TestPromptsKeyedByActionTypeWithPlaceholders(t *testing.T) {
	s := NewStore()
	validActs := map[string]bool{
		rule.ActGroupSeckill:  true,
		rule.ActCouponWake:    true,
		rule.ActMomentsPost:   true,
		rule.ActPriceAdj:      true,
		rule.ActGroupbuy:      true,
		rule.ActRechargePromo: true,
	}
	for _, pb := range s.List() {
		for act, tpl := range pb.Prompts {
			assert.True(t, validActs[act], "%s prompt key %s must be a valid action_type", pb.Business, act)
			assert.Contains(t, tpl, "{product}", "%s/%s template must contain {product}", pb.Business, act)
			assert.Contains(t, tpl, "{price}", "%s/%s template must contain {price}", pb.Business, act)
		}
	}
}

// TestPromptsCoverColdPriority 每业态 Prompts 覆盖其全部 ColdPriority 动作
// (冷启动占位文案必须能取出来).
func TestPromptsCoverColdPriority(t *testing.T) {
	s := NewStore()
	for _, pb := range s.List() {
		for _, act := range pb.ColdPriority {
			_, ok := pb.Prompts[act]
			assert.True(t, ok, "%s must have prompt for cold-priority action %s", pb.Business, act)
		}
	}
}

// ===== IsColdPriority (DoD §14.6 A2-③) =====

// TestIsColdPriority 命中/未命中; nil 接收者安全返 false.
func TestIsColdPriority(t *testing.T) {
	s := NewStore()
	fruit, ok := s.Get(Fruit)
	require.True(t, ok)

	assert.True(t, fruit.IsColdPriority(rule.ActMomentsPost))
	assert.True(t, fruit.IsColdPriority(rule.ActGroupSeckill))
	assert.True(t, fruit.IsColdPriority(rule.ActGroupbuy))
	assert.False(t, fruit.IsColdPriority(rule.ActCouponWake))
	assert.False(t, fruit.IsColdPriority(rule.ActRechargePromo))
	assert.False(t, fruit.IsColdPriority(""))

	// nil 接收者: 不 panic, 返 false.
	var nilPB *Playbook
	assert.False(t, nilPB.IsColdPriority(rule.ActMomentsPost))
}

// ===== ColdStartFilter (DoD §14.6 A2-④) =====

// mkItem 构造一个最小 RoutedItem, 只填 ActionType (过滤只看 ActionType).
func mkItem(actionType string) rule.RoutedItem {
	return rule.RoutedItem{ActionType: actionType}
}

// TestColdStartFilterKeepsOnlyColdPriority 混合动作 → 只保留 ColdPriority 项, 顺序保持.
func TestColdStartFilterKeepsOnlyColdPriority(t *testing.T) {
	s := NewStore()
	fruit, ok := s.Get(Fruit) // ColdPriority: moments_post, group_seckill, groupbuy
	require.True(t, ok)

	items := []rule.RoutedItem{
		mkItem(rule.ActMomentsPost),   // keep
		mkItem(rule.ActCouponWake),    // drop
		mkItem(rule.ActGroupSeckill),  // keep
		mkItem(rule.ActPriceAdj),      // drop
		mkItem(rule.ActGroupbuy),      // keep
		mkItem(rule.ActRechargePromo), // drop
	}
	got := ColdStartFilter(items, fruit)
	require.Len(t, got, 3)
	assert.Equal(t, rule.ActMomentsPost, got[0].ActionType)
	assert.Equal(t, rule.ActGroupSeckill, got[1].ActionType)
	assert.Equal(t, rule.ActGroupbuy, got[2].ActionType)
}

// TestColdStartFilterAllNonColdReturnsOriginal 全部非冷启动 → 原样返回 (老板永远 ≥1 条).
func TestColdStartFilterAllNonColdReturnsOriginal(t *testing.T) {
	s := NewStore()
	fruit, ok := s.Get(Fruit)
	require.True(t, ok)

	items := []rule.RoutedItem{
		mkItem(rule.ActCouponWake),
		mkItem(rule.ActPriceAdj),
	}
	got := ColdStartFilter(items, fruit)
	// 原样返回: 内容等价, 且不被清空.
	require.Len(t, got, 2)
	assert.Equal(t, items, got)
}

// TestColdStartFilterNilPlaybookReturnsOriginal pb==nil → 原样返回, 不 panic.
func TestColdStartFilterNilPlaybookReturnsOriginal(t *testing.T) {
	items := []rule.RoutedItem{
		mkItem(rule.ActCouponWake),
		mkItem(rule.ActPriceAdj),
	}
	got := ColdStartFilter(items, nil)
	assert.Equal(t, items, got)
}

// TestColdStartFilterEmptyInputReturnsEmpty 空输入 → 空输出 (过滤后为空回原样=空).
func TestColdStartFilterEmptyInputReturnsEmpty(t *testing.T) {
	s := NewStore()
	fruit, ok := s.Get(Fruit)
	require.True(t, ok)

	got := ColdStartFilter(nil, fruit)
	assert.Empty(t, got)

	got = ColdStartFilter([]rule.RoutedItem{}, fruit)
	assert.Empty(t, got)
}

// TestColdStartFilterEmptyColdPriorityReturnsOriginal 构造无 ColdPriority 的 pb →
// 任何 item 都被过滤 → 过滤后为空 → 原样返回.
func TestColdStartFilterEmptyColdPriorityReturnsOriginal(t *testing.T) {
	pb := &Playbook{Business: Fruit, ColdPriority: nil}
	items := []rule.RoutedItem{mkItem(rule.ActMomentsPost)}
	got := ColdStartFilter(items, pb)
	assert.Equal(t, items, got)
}

// TestColdStartFilterDoesNotMutateInput 过滤不修改入参 slice 内容 (纯函数).
func TestColdStartFilterDoesNotMutateInput(t *testing.T) {
	s := NewStore()
	fruit, ok := s.Get(Fruit)
	require.True(t, ok)

	items := []rule.RoutedItem{
		mkItem(rule.ActCouponWake),
		mkItem(rule.ActMomentsPost),
	}
	snapshot := make([]rule.RoutedItem, len(items))
	copy(snapshot, items)

	_ = ColdStartFilter(items, fruit)
	assert.Equal(t, snapshot, items, "input slice must not be mutated")
}

// ===== 并发安全 (-race, DoD §14.6 A2-⑤) =====

// TestStoreConcurrentGetList 并发 Get/List 不 data race (Store 构造后只读).
func TestStoreConcurrentGetList(t *testing.T) {
	s := NewStore()
	done := make(chan struct{})
	for i := 0; i < 8; i++ {
		go func(i int) {
			defer func() { done <- struct{}{} }()
			b := allBusinesses()[i%len(allBusinesses())]
			pb, ok := s.Get(b)
			require.True(t, ok)
			_ = pb.IsColdPriority(rule.ActMomentsPost)
			_ = s.List()
			_ = ColdStartFilter([]rule.RoutedItem{mkItem(rule.ActMomentsPost)}, pb)
		}(i)
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}
