package httpserver

// M2-S2 HTTP-layer tests (CONTRACT §11.6): the reach + groupbuy + crm boss loops
// plus the cross-tenant proof. The frequency circuit (1/day for wework_group),
// content filter (敏感词 hard block), unsubscribe opt-out, and subscribe-message
// one-time quota are all wired through the same anti-corruption adapters the
// composition root uses, proving the guards fire end-to-end over HTTP.

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/zhangguijunshi/backend/internal/compliance/contentfilter"
	"github.com/zhangguijunshi/backend/internal/compliance/freqcircuit"
	"github.com/zhangguijunshi/backend/internal/crm"
	"github.com/zhangguijunshi/backend/internal/groupbuy"
	"github.com/zhangguijunshi/backend/internal/membership"
	"github.com/zhangguijunshi/backend/internal/reach"
	"github.com/zhangguijunshi/backend/pkg/config"
)

// m2s2FreqGate adapts *freqcircuit.Service to reach.FreqGate (mirrors
// cmd/api/main.go's freqGateAdapter).
type m2s2FreqGate struct{ svc *freqcircuit.Service }

func (a m2s2FreqGate) Allow(ctx context.Context, channel string) (bool, string, error) {
	return a.svc.Allow(ctx, freqcircuit.Channel(channel))
}

// m2s2ContentFilter adapts *contentfilter.Filter to reach.ContentFilter (mirrors
// cmd/api/main.go's cfFilterAdapter).
type m2s2ContentFilter struct{ f *contentfilter.Filter }

func (a m2s2ContentFilter) Do(text string) reach.ContentResult {
	r := a.f.Do(text)
	return reach.ContentResult{Text: r.Text, Passed: r.Passed, HitWords: r.HitWords, Replaced: r.Replaced}
}

// newM2S2Router builds a router with membership + reach + groupbuy + crm wired
// over in-memory repos and the dev MockTenant header enabled. reach carries the
// frequency circuit + content filter guards; groupbuy reuses the membership
// lookuper defined in m2s1_test.go.
func newM2S2Router() *gin.Engine {
	memSvc := membership.New(membership.NewMemoryMemberRepo(), membership.NewMemoryMemberCoder(nil))
	freqSvc := freqcircuit.New()
	cfFilter := contentfilter.New()
	reachSvc := reach.New(
		reach.NewMemoryReachRepo(),
		reach.WithSender(reach.NewMemorySender()),
		reach.WithFreqGate(m2s2FreqGate{freqSvc}),
		reach.WithContentFilter(m2s2ContentFilter{cfFilter}),
	)
	gbSvc := groupbuy.New(groupbuy.NewMemoryGroupbuyRepo(), groupbuy.WithMemberLookuper(m2s1MembershipLookuper{memSvc}))
	crmSvc := crm.New(crm.NewMemoryRepo())
	return New(Deps{
		Cfg:        &config.Config{App: config.AppConfig{Env: "test"}},
		Membership: memSvc,
		Reach:      reachSvc,
		Groupbuy:   gbSvc,
		CRM:        crmSvc,
		DevMock:    true,
	})
}

// joinMember enrols one member under tenant/store and returns its id.
func joinMember(t *testing.T, r *gin.Engine, tenant, store, phone, name string) int64 {
	_, mem := doM2(t, r, http.MethodPost, "/v1/boss/membership/members", tenant, store,
		gin.H{"phone_hash": phone, "name": name})
	return int64(mem["ID"].(float64))
}

// sendReach fires one reach send and returns the task data map.
func sendReach(t *testing.T, r *gin.Engine, tenant, store string, body gin.H) map[string]any {
	_, data := doM2(t, r, http.MethodPost, "/v1/boss/reach/send", tenant, store, body)
	return data
}

// TestM2S2_GroupbuyFullFlow proves 建团 → 参团 → 满员成团(success) → 超员拒绝(60013).
func TestM2S2_GroupbuyFullFlow(t *testing.T) {
	r := newM2S2Router()
	m1 := joinMember(t, r, "50021", "9", "h1", "甲")
	m2 := joinMember(t, r, "50021", "9", "h2", "乙")
	m3 := joinMember(t, r, "50021", "9", "h3", "丙")
	m4 := joinMember(t, r, "50021", "9", "h4", "丁")

	_, gb := doM2(t, r, http.MethodPost, "/v1/boss/groupbuys", "50021", "9",
		gin.H{"product_id": 1, "title": "芒果3人团", "required_count": 3})
	id := int64(gb["ID"].(float64))
	assert.Equal(t, "active", gb["Status"])
	assert.EqualValues(t, 0, gb["Joined"])

	// 1st + 2nd joins stay active.
	_, d1 := doM2(t, r, http.MethodPost, "/v1/boss/groupbuys/"+itoa(id)+"/join", "50021", "9", gin.H{"member_id": m1})
	assert.Equal(t, "active", d1["groupbuy"].(map[string]any)["Status"])
	_, d2 := doM2(t, r, http.MethodPost, "/v1/boss/groupbuys/"+itoa(id)+"/join", "50021", "9", gin.H{"member_id": m2})
	assert.Equal(t, "active", d2["groupbuy"].(map[string]any)["Status"])

	// 3rd join flips the squad to success (满员成团).
	_, d3 := doM2(t, r, http.MethodPost, "/v1/boss/groupbuys/"+itoa(id)+"/join", "50021", "9", gin.H{"member_id": m3})
	require.Equal(t, "success", d3["groupbuy"].(map[string]any)["Status"], "3rd join should close the squad")

	// 4th join → already full (60013).
	w, _ := doM1(t, r, http.MethodPost, "/v1/boss/groupbuys/"+itoa(id)+"/join", "50021", "9", gin.H{"member_id": m4})
	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, 60013, env.Code, "join on a full squad should 60013: %s", env.Message)

	// Duplicate join by an existing participant → 60015.
	w, _ = doM1(t, r, http.MethodPost, "/v1/boss/groupbuys/"+itoa(id)+"/join", "50021", "9", gin.H{"member_id": m1})
	env = decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, 60015, env.Code, "duplicate join should 60015: %s", env.Message)
}

// TestM2S2_ReachSendAndFreqCircuit proves a sent reach, then the WeCom daily
// cap blocks the second attempt (blocked_reason=freq_circuit); switching to the
// unlimited SMS channel sends again.
func TestM2S2_ReachSendAndFreqCircuit(t *testing.T) {
	r := newM2S2Router()
	m := joinMember(t, r, "50021", "9", "h-freq", "老王")

	t1 := sendReach(t, r, "50021", "9", gin.H{
		"member_id": m, "channel": "wework_group", "template_code": "birthday"})
	assert.Equal(t, "sent", t1["Status"])

	// 2nd wework_group same day → blocked by frequency circuit.
	t2 := sendReach(t, r, "50021", "9", gin.H{
		"member_id": m, "channel": "wework_group", "template_code": "repurchase"})
	assert.Equal(t, "blocked", t2["Status"])
	assert.Equal(t, "freq_circuit", t2["BlockedReason"])

	// SMS is unlimited → sends.
	t3 := sendReach(t, r, "50021", "9", gin.H{
		"member_id": m, "channel": "sms", "template_code": "recall_30"})
	assert.Equal(t, "sent", t3["Status"])
}

// TestM2S2_ReachUnsubscribe proves the opt-out blocks a channel, and resubscribe
// restores it. Uses SMS (no frequency cap) so the only blocker is the opt-out.
func TestM2S2_ReachUnsubscribe(t *testing.T) {
	r := newM2S2Router()
	m := joinMember(t, r, "50021", "9", "h-unsub", "老李")

	doM2(t, r, http.MethodPost, "/v1/boss/reach/unsubscribe", "50021", "9",
		gin.H{"member_id": m, "channel": "sms", "reason": "too noisy"})

	b := sendReach(t, r, "50021", "9", gin.H{
		"member_id": m, "channel": "sms", "template_code": "recall_60"})
	assert.Equal(t, "blocked", b["Status"])
	assert.Equal(t, "unsubscribed", b["BlockedReason"])

	doM2(t, r, http.MethodPost, "/v1/boss/reach/resubscribe", "50021", "9",
		gin.H{"member_id": m, "channel": "sms"})

	ok := sendReach(t, r, "50021", "9", gin.H{
		"member_id": m, "channel": "sms", "template_code": "recall_90"})
	assert.Equal(t, "sent", ok["Status"], "resubscribe should restore the channel")
}

// TestM2S2_ReachSubscribeQuota proves the one-time subscribe quota: after one
// grant of 1, the first subscribe_msg sends and the second is blocked.
func TestM2S2_ReachSubscribeQuota(t *testing.T) {
	r := newM2S2Router()
	m := joinMember(t, r, "50021", "9", "h-sub", "老陈")

	doM2(t, r, http.MethodPost, "/v1/boss/reach/subscribe-auth", "50021", "9",
		gin.H{"member_id": m, "template_id": "repurchase", "quota": 1})

	s1 := sendReach(t, r, "50021", "9", gin.H{
		"member_id": m, "channel": "subscribe_msg", "template_code": "repurchase"})
	assert.Equal(t, "sent", s1["Status"])

	s2 := sendReach(t, r, "50021", "9", gin.H{
		"member_id": m, "channel": "subscribe_msg", "template_code": "repurchase"})
	assert.Equal(t, "blocked", s2["Status"])
	assert.Equal(t, "subscribe_quota", s2["BlockedReason"])
}

// TestM2S2_ReachContentFilter proves a DFA sensitive-word hit blocks the send
// before the frequency check. member_name="诈骗" is interpolated into the body.
func TestM2S2_ReachContentFilter(t *testing.T) {
	r := newM2S2Router()
	m := joinMember(t, r, "50021", "9", "h-cf", "老赵")

	b := sendReach(t, r, "50021", "9", gin.H{
		"member_id": m, "channel": "wework_group", "template_code": "birthday",
		"vars": gin.H{"member_name": "诈骗"}})
	assert.Equal(t, "blocked", b["Status"])
	assert.Equal(t, "content_filter", b["BlockedReason"])
}

// TestM2S2_CRM_SOPAdvance proves the recruit→activate→convert→refer funnel and
// that advancing past refer yields 60016 (terminal).
func TestM2S2_CRM_SOPAdvance(t *testing.T) {
	r := newM2S2Router()
	_, g := doM2(t, r, http.MethodPost, "/v1/boss/crm/groups", "50021", "9",
		gin.H{"name": "水果福利群"})
	id := int64(g["ID"].(float64))
	require.Equal(t, "recruit", g["SOPStage"], "new group defaults to recruit")

	// recruit → activate → convert → refer.
	for _, stage := range []string{"activate", "convert", "refer"} {
		_, g = doM2(t, r, http.MethodPost, "/v1/boss/crm/groups/"+itoa(id)+"/sop/advance", "50021", "9", nil)
		require.Equal(t, stage, g["SOPStage"], "advance should reach %s", stage)
	}

	// refer is terminal → 60016.
	w, _ := doM1(t, r, http.MethodPost, "/v1/boss/crm/groups/"+itoa(id)+"/sop/advance", "50021", "9", nil)
	env := decodeEnvelope(t, w.Body.Bytes())
	assert.Equal(t, 60016, env.Code, "advance past refer should 60016: %s", env.Message)
}

// TestM2S2_CrossTenant proves tenant B cannot read tenant A's groupbuy or reach
// task.
func TestM2S2_CrossTenant(t *testing.T) {
	r := newM2S2Router()
	mA := joinMember(t, r, "50021", "9", "h-a", "租户A会员")

	// Tenant A opens a squad.
	_, gb := doM2(t, r, http.MethodPost, "/v1/boss/groupbuys", "50021", "9",
		gin.H{"product_id": 1, "title": "A的团", "required_count": 3})
	gbID := int64(gb["ID"].(float64))

	// Tenant B cannot read it.
	w, _ := doM1(t, r, http.MethodGet, "/v1/boss/groupbuys/"+itoa(gbID), "50022", "9", nil)
	assert.Equal(t, 40404, decodeEnvelope(t, w.Body.Bytes()).Code, "cross-tenant groupbuy get should 40404")

	// Tenant A sends a reach task; tenant B cannot read it.
	_, tk := doM2(t, r, http.MethodPost, "/v1/boss/reach/send", "50021", "9",
		gin.H{"member_id": mA, "channel": "sms", "template_code": "birthday"})
	taskID := int64(tk["ID"].(float64))
	w, _ = doM1(t, r, http.MethodGet, "/v1/boss/reach/tasks/"+itoa(taskID), "50022", "9", nil)
	assert.Equal(t, 40404, decodeEnvelope(t, w.Body.Bytes()).Code, "cross-tenant reach task get should 40404")
}
