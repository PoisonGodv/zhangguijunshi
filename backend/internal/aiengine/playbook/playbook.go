// Package playbook 提供 5 业态(fruit/convenience/maternity/fresh/apparel)的冷启动
// 剧本库 (AI-11, CONTRACT §14.3.2)。新店首日无销售数据、signal 为空或数据质量
// 不达标时, pipeline 用对应业态的 Playbook 注入 ColdPriority 占位动作, 让老板
// 「今日 3 件事」永远 ≥1 条 (计划书 §6.6 / CONTRACT §12.3.8)。
//
// 本期剧本为 in-code seed (NewStore 硬编码); DB 化 (ai_rule 表) + 后台可视化
// 编辑延后到 M4 (CONTRACT §14.8)。本包按反腐端口约定只 import rule + stdlib,
// 不 import strategy/attribution/bandit (§14.3.0 无环 DAG)。
package playbook

import "github.com/zhangguijunshi/backend/internal/aiengine/rule"

// 5 业态常量 — 取值与 rule.Business / rule.Rule.Business 完全一致 (§14.3.2).
const (
	Fruit       = "fruit"
	Convenience = "convenience"
	Maternity   = "maternity"
	Fresh       = "fresh"
	Apparel     = "apparel"
)

// SOPStage 社群四阶段之一 (拉群/促活/转化/裂变, ch07 §7.10.1).
type SOPStage struct {
	Name string `json:"name"` // 阶段名: 拉群 | 促活 | 转化 | 裂变
	Play string `json:"play"` // 该阶段的标准打法描述
}

// Playbook 一个业态的冷启动运营手册 (ch07 §7.10.1).
type Playbook struct {
	Business     string            `json:"business"`      // 业态, 取值同上 5 常量
	Rules        []rule.Rule       `json:"rules"`         // 该业态默认规则集 (复用 rule.DefaultSeeds 同 Business 子集)
	Prompts      map[string]string `json:"prompts"`       // action_type → 文案模板, 变量用 {product}/{price}/{member_tag}/{qty} 占位
	SOP          [4]SOPStage       `json:"sop"`           // 社群四阶段: 拉群/促活/转化/裂变
	LossRate     float64           `json:"loss_rate"`     // 损耗率 (fruit≈0.10, convenience≈0.015)
	CashCoef     float64           `json:"cash_coef"`     // 现金系数 (客流反推, 默认 1.2)
	ColdPriority []string          `json:"cold_priority"` // 冷启动优先 action_type (不依赖销售反推)
}

// IsColdPriority 报告 actionType 是否在该业态的 ColdPriority 中 (大小写敏感,
// 调用方传 rule.Act* 常量).
func (p *Playbook) IsColdPriority(actionType string) bool {
	if p == nil {
		return false
	}
	for _, a := range p.ColdPriority {
		if a == actionType {
			return true
		}
	}
	return false
}

// Store 持有 5 业态剧本库 (本期 in-code seed; DB/后台延后到 M4).
// Store 在构造后只读, 无锁, 并发 Get/List 安全 (-race 友好).
type Store struct {
	books map[string]*Playbook
}

// NewStore 构造并种入 5 业态剧本. Rules 复用 rule.DefaultSeeds() 中同 Business
// 的规则 (按 Business 过滤), 保证每业态 ≥2 条; 不启用业务的业态 (convenience/
// maternity 在 DefaultSeeds 中各 2 条, fruit 3 条, fresh 1 条, apparel 2 条)
// 若该业态在 DefaultSeeds 不足 2 条, 用本业态特化规则补齐到 ≥2 (DoD §14.6 A2).
func NewStore() *Store {
	s := &Store{books: make(map[string]*Playbook, 5)}
	for _, b := range allBusinesses() {
		s.books[b] = buildPlaybook(b)
	}
	return s
}

// Get 返回该业态剧本. 未知业态 bool false (不返回 nil,ok 对, 调用方按 ok 判空).
func (s *Store) Get(business string) (*Playbook, bool) {
	pb, ok := s.books[business]
	return pb, ok
}

// List 返回全部 5 业态剧本 (顺序固定为 allBusinesses 定义序, 便于稳定测试/序列化).
func (s *Store) List() []*Playbook {
	out := make([]*Playbook, 0, len(s.books))
	for _, b := range allBusinesses() {
		if pb, ok := s.books[b]; ok {
			out = append(out, pb)
		}
	}
	return out
}

// ColdStartFilter 冷启动期过滤: 移除非 ColdPriority 的 item. 纯函数, 不写库.
//
// 规则 (§14.3.2): 逐个判定 pb.IsColdPriority(item.ActionType), 保留为 true 的
// item. 若过滤后为空 (全非冷启动动作, 或 pb 无 ColdPriority), 原样返回 items —
// 老板永远 ≥1 条 (计划书 §6.6 / CONTRACT §12.3.8). pb==nil 时原样返回.
func ColdStartFilter(items []rule.RoutedItem, pb *Playbook) []rule.RoutedItem {
	if pb == nil {
		return items
	}
	kept := make([]rule.RoutedItem, 0, len(items))
	for _, it := range items {
		if pb.IsColdPriority(it.ActionType) {
			kept = append(kept, it)
		}
	}
	if len(kept) == 0 {
		return items
	}
	return kept
}
