package reach

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// ctxWithTenant builds a context carrying tid (SourceMerchant), the same shape
// the HTTP middleware will attach after JWT/Mock auth.
func ctxWithTenant(tid int64) context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{
		TenantID: tid,
		Source:   multitenancy.SourceMerchant,
	})
}

// fixedNow is the injected service clock; tests advance it by mutating nowVar.
var nowVar = time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)

func clock() time.Time { return nowVar }

// ---- fakes for the three anti-corruption ports ----

type fakeSender struct {
	payloads []*SendPayload
	err      error
}

func (f *fakeSender) Send(_ context.Context, p *SendPayload) error {
	cp := *p
	f.payloads = append(f.payloads, &cp)
	return f.err
}

type fakeFreq struct {
	allowed bool
	reason  string
	err     error
	calls   int
}

func (f *fakeFreq) Allow(_ context.Context, _ string) (bool, string, error) {
	f.calls++
	return f.allowed, f.reason, f.err
}

type fakeCF struct {
	passed bool
	text   string
	calls  int
}

func (f *fakeCF) Do(_ string) ContentResult {
	f.calls++
	return ContentResult{Text: f.text, Passed: f.passed}
}

// fullSvc wires every guard so the pipeline order is observable.
func fullSvc(repo ReachRepo, sender *fakeSender, freq *fakeFreq, cf *fakeCF) *Service {
	return New(repo, WithSender(sender), WithFreqGate(freq), WithContentFilter(cf), WithNowFunc(clock))
}

// ===========================================================================
// Render: 8 templates + unknown code + vars/defaults
// ===========================================================================

func TestRender_AllTemplates(t *testing.T) {
	cases := []struct {
		code    string
		vars    map[string]string
		nameKey string // a var we expect to appear verbatim in body or title
		nameVal string
	}{
		{TplNewWelcome, map[string]string{"member_name": "小美", "store_name": "鲜果日记"}, "store_name", "鲜果日记"},
		{TplBirthday, map[string]string{"member_name": "阿强", "birthday": "8月8日"}, "birthday", "8月8日"},
		{TplRecall30, map[string]string{"member_name": "阿强", "recall_days": "30"}, "member_name", "阿强"},
		{TplRecall60, map[string]string{"member_name": "阿强"}, "", ""}, // recall_days defaults to 60
		{TplRecall90, map[string]string{"member_name": "阿强"}, "", ""},
		{TplRepurchase, map[string]string{"member_name": "李姐", "product_name": "洗洁精"}, "product_name", "洗洁精"},
		{TplBalanceLow, map[string]string{"member_name": "李姐", "balance": "12"}, "balance", "12"},
		{TplPointsExpire, map[string]string{"member_name": "李姐", "points": "500"}, "points", "500"},
	}
	for _, c := range cases {
		got, err := Render(RenderInput{Code: c.code, MemberName: c.vars["member_name"], StoreName: c.vars["store_name"], Vars: c.vars})
		if err != nil {
			t.Fatalf("%s: unexpected err %v", c.code, err)
		}
		if got.Code != c.code {
			t.Errorf("%s: code echo mismatch %q", c.code, got.Code)
		}
		if got.Title == "" || got.Body == "" {
			t.Errorf("%s: title/body must be non-empty (title=%q body=%q)", c.code, got.Title, got.Body)
		}
		// Body stays within the ≤120-rune copy guideline.
		if rc := runeCount(got.Body); rc > 120 {
			t.Errorf("%s: body too long: %d runes", c.code, rc)
		}
		if c.nameVal != "" && !strings.Contains(got.Body, c.nameVal) && !strings.Contains(got.Title, c.nameVal) {
			t.Errorf("%s: expected %q in title or body (title=%q body=%q)", c.code, c.nameVal, got.Title, got.Body)
		}
	}
}

func TestRender_RecallDaysDefault(t *testing.T) {
	// recall_60 without recall_days var ⇒ body contains "60".
	got, err := Render(RenderInput{Code: TplRecall60, MemberName: "阿强"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(got.Body, "60") {
		t.Fatalf("recall_60 default days should be 60, body=%q", got.Body)
	}
	// recall_90 ⇒ "90".
	got90, _ := Render(RenderInput{Code: TplRecall90})
	if !strings.Contains(got90.Body, "90") {
		t.Fatalf("recall_90 default days should be 90, body=%q", got90.Body)
	}
}

func TestRender_NameStoreDefaults(t *testing.T) {
	// No vars at all ⇒ name falls back to 朋友, store to 我们店.
	got, err := Render(RenderInput{Code: TplNewWelcome})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(got.Body, "朋友") {
		t.Errorf("default name 朋友 missing: %q", got.Body)
	}
	if !strings.Contains(got.Body, "我们店") {
		t.Errorf("default store 我们店 missing: %q", got.Body)
	}
}

func TestRender_UnknownCode(t *testing.T) {
	if _, err := Render(RenderInput{Code: "no_such_template"}); !errors.Is(err, ErrTemplateUnknown) {
		t.Fatalf("expected ErrTemplateUnknown, got %v", err)
	}
}

func TestServiceRender_Delegates(t *testing.T) {
	s := New(NewMemoryReachRepo())
	got, err := s.Render(RenderInput{Code: TplBirthday, MemberName: "小美", Vars: map[string]string{"birthday": "今天"}})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !strings.Contains(got.Body, "小美") {
		t.Errorf("service Render should delegate to package Render: %q", got.Body)
	}
}

func runeCount(s string) int {
	return len([]rune(s))
}

// ===========================================================================
// Send pipeline: sent / blocked / failed / dev-mode
// ===========================================================================

func TestSend_SentSuccess(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	freq := &fakeFreq{allowed: true}
	cf := &fakeCF{passed: true, text: ""}
	s := fullSvc(repo, snd, freq, cf)

	task, err := s.Send(ctxWithTenant(50021), SendInput{
		StoreID: 7, MemberID: 100, StrategyID: 555,
		Channel: ChannelWeworkGroup, TemplateCode: TplRecall30,
		Vars: map[string]string{"member_name": "阿强", "store_name": "鲜果日记"},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if task == nil || task.Status != StatusSent {
		t.Fatalf("expected sent task, got %+v", task)
	}
	if task.BlockedReason != "" {
		t.Errorf("sent task should have empty blocked reason, got %q", task.BlockedReason)
	}
	if task.StrategyID != 555 {
		t.Errorf("strategy attribution lost: %d", task.StrategyID)
	}
	if task.TenantID != 50021 || task.StoreID != 7 {
		t.Errorf("tenant/store not stamped: tenant=%d store=%d", task.TenantID, task.StoreID)
	}
	if len(snd.payloads) != 1 {
		t.Fatalf("sender should record 1 payload, got %d", len(snd.payloads))
	}
	if snd.payloads[0].MemberID != 100 || snd.payloads[0].Channel != ChannelWeworkGroup {
		t.Errorf("payload fields wrong: %+v", snd.payloads[0])
	}
	if snd.payloads[0].Title == "" || !strings.Contains(snd.payloads[0].Body, "阿强") {
		t.Errorf("payload should carry rendered title/body: %+v", snd.payloads[0])
	}
	if cf.calls != 1 || freq.calls != 1 {
		t.Errorf("both guards should run once: cf=%d freq=%d", cf.calls, freq.calls)
	}
}

func TestSend_BlockedContentFilter(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	freq := &fakeFreq{allowed: true}
	cf := &fakeCF{passed: false} // content rejects
	s := fullSvc(repo, snd, freq, cf)

	task, err := s.Send(ctxWithTenant(50021), SendInput{
		MemberID: 100, Channel: ChannelWeworkOneV, TemplateCode: TplRepurchase,
	})
	if err != nil {
		t.Fatalf("blocked must return nil error, got %v", err)
	}
	if task == nil || task.Status != StatusBlocked || task.BlockedReason != BlockContentFilter {
		t.Fatalf("expected blocked(content_filter), got %+v", task)
	}
	// Order: content blocks BEFORE freq ⇒ freq never called.
	if freq.calls != 0 {
		t.Errorf("freq must not run when content blocks: %d", freq.calls)
	}
	// Nothing delivered.
	if len(snd.payloads) != 0 {
		t.Errorf("no payload on block: %d", len(snd.payloads))
	}
}

func TestSend_BlockedFreqCircuit(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	freq := &fakeFreq{allowed: false, reason: "freq_daily"}
	cf := &fakeCF{passed: true}
	s := fullSvc(repo, snd, freq, cf)

	task, err := s.Send(ctxWithTenant(50021), SendInput{
		MemberID: 100, Channel: ChannelWeworkGroup, TemplateCode: TplNewWelcome,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if task.Status != StatusBlocked || task.BlockedReason != BlockFreqCircuit {
		t.Fatalf("expected blocked(freq_circuit), got %+v", task)
	}
	// Content was checked first (and passed); freq was called and denied.
	if cf.calls != 1 || freq.calls != 1 {
		t.Errorf("content should pass then freq deny: cf=%d freq=%d", cf.calls, freq.calls)
	}
	if len(snd.payloads) != 0 {
		t.Errorf("no payload on freq block: %d", len(snd.payloads))
	}
}

func TestSend_BlockedUnsubscribed_OrderPreservesQuota(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	freq := &fakeFreq{allowed: true}
	cf := &fakeCF{passed: true}
	s := fullSvc(repo, snd, freq, cf)

	// Grant subscribe quota, then opt the member out of subscribe_msg.
	if _, err := s.GrantSubscribe(ctxWithTenant(50021), 100, TplPointsExpire, 3); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := s.Unsubscribe(ctxWithTenant(50021), 100, ChannelSubscribeMsg, "用户主动退订"); err != nil {
		t.Fatalf("unsub: %v", err)
	}

	task, err := s.Send(ctxWithTenant(50021), SendInput{
		MemberID: 100, Channel: ChannelSubscribeMsg, TemplateCode: TplPointsExpire,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if task.Status != StatusBlocked || task.BlockedReason != BlockUnsubscribed {
		t.Fatalf("expected blocked(unsubscribed), got %+v", task)
	}
	// Order: unsubscribe (step 5) runs BEFORE subscribe-quota consume (step 6),
	// so the granted quota must be untouched.
	auth, gerr := s.repo.GetSubscribe(ctxWithTenant(50021), 50021, 100, TplPointsExpire)
	if gerr != nil {
		t.Fatalf("get subscribe: %v", gerr)
	}
	if auth.UsedQuota != 0 {
		t.Errorf("unsubscribe block must not consume subscribe quota: used=%d", auth.UsedQuota)
	}
}

func TestSend_BlockedSubscribeQuota(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	freq := &fakeFreq{allowed: true}
	cf := &fakeCF{passed: true}
	s := fullSvc(repo, snd, freq, cf)

	// No GrantSubscribe ⇒ exhausted ⇒ blocked(subscribe_quota).
	task, err := s.Send(ctxWithTenant(50021), SendInput{
		MemberID: 100, Channel: ChannelSubscribeMsg, TemplateCode: TplPointsExpire,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if task.Status != StatusBlocked || task.BlockedReason != BlockSubscribeQuota {
		t.Fatalf("expected blocked(subscribe_quota), got %+v", task)
	}
	if len(snd.payloads) != 0 {
		t.Errorf("no payload on quota block: %d", len(snd.payloads))
	}
}

func TestSend_SubscribeMsgConsumesQuota(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	freq := &fakeFreq{allowed: true}
	cf := &fakeCF{passed: true}
	s := fullSvc(repo, snd, freq, cf)

	if _, err := s.GrantSubscribe(ctxWithTenant(50021), 100, TplPointsExpire, 2); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// First send consumes one quota unit and succeeds.
	if _, err := s.Send(ctxWithTenant(50021), SendInput{
		MemberID: 100, Channel: ChannelSubscribeMsg, TemplateCode: TplPointsExpire,
	}); err != nil {
		t.Fatalf("send1: %v", err)
	}
	auth, _ := s.repo.GetSubscribe(ctxWithTenant(50021), 50021, 100, TplPointsExpire)
	if auth.UsedQuota != 1 {
		t.Errorf("after 1 send used should be 1, got %d", auth.UsedQuota)
	}
	// Second send exhausts exactly.
	if _, err := s.Send(ctxWithTenant(50021), SendInput{
		MemberID: 100, Channel: ChannelSubscribeMsg, TemplateCode: TplPointsExpire,
	}); err != nil {
		t.Fatalf("send2: %v", err)
	}
	auth, _ = s.repo.GetSubscribe(ctxWithTenant(50021), 50021, 100, TplPointsExpire)
	if auth.UsedQuota != 2 {
		t.Errorf("after 2 sends used should be 2, got %d", auth.UsedQuota)
	}
	// Third send ⇒ exhausted ⇒ blocked.
	task, err := s.Send(ctxWithTenant(50021), SendInput{
		MemberID: 100, Channel: ChannelSubscribeMsg, TemplateCode: TplPointsExpire,
	})
	if err != nil {
		t.Fatalf("send3 err: %v", err)
	}
	if task.Status != StatusBlocked || task.BlockedReason != BlockSubscribeQuota {
		t.Fatalf("third send should block(subscribe_quota), got %+v", task)
	}
	auth, _ = s.repo.GetSubscribe(ctxWithTenant(50021), 50021, 100, TplPointsExpire)
	if auth.UsedQuota != 2 {
		t.Errorf("exhausted attempt must not over-consume: used=%d", auth.UsedQuota)
	}
}

func TestSend_FailedDelivery(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{err: errors.New("upstream timeout")}
	freq := &fakeFreq{allowed: true}
	cf := &fakeCF{passed: true}
	s := fullSvc(repo, snd, freq, cf)

	task, err := s.Send(ctxWithTenant(50021), SendInput{
		MemberID: 100, Channel: ChannelSMS, TemplateCode: TplRecall90,
	})
	if err != nil {
		t.Fatalf("send failure is recorded on the task, not returned as err: %v", err)
	}
	if task.Status != StatusFailed {
		t.Fatalf("expected failed status, got %+v", task)
	}
}

func TestSend_DevModeNoGuards(t *testing.T) {
	// No guards wired: render + sender only. A subscribe_msg channel without a
	// sender still needs quota (ConsumeSubscribe runs unconditionally for that
	// channel), so use a non-subscribe channel to isolate dev-mode behaviour.
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	s := New(repo, WithSender(snd), WithNowFunc(clock))

	task, err := s.Send(ctxWithTenant(50021), SendInput{
		MemberID: 100, Channel: ChannelWeworkOneV, TemplateCode: TplBirthday,
		Vars: map[string]string{"member_name": "小美"},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if task.Status != StatusSent {
		t.Fatalf("dev mode should reach sent, got %+v", task)
	}
	if len(snd.payloads) != 1 {
		t.Errorf("dev sender should record payload: %d", len(snd.payloads))
	}
}

func TestSend_UnknownTemplateHardError(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	freq := &fakeFreq{allowed: true}
	cf := &fakeCF{passed: true}
	s := fullSvc(repo, snd, freq, cf)

	// Unknown code ⇒ hard error (ErrTemplateUnknown), NOT a blocked task.
	task, err := s.Send(ctxWithTenant(50021), SendInput{
		MemberID: 100, Channel: ChannelWeworkGroup, TemplateCode: "bogus_tpl",
	})
	if !errors.Is(err, ErrTemplateUnknown) {
		t.Fatalf("expected ErrTemplateUnknown hard error, got err=%v task=%+v", err, task)
	}
	if task != nil {
		t.Errorf("hard error should not persist a task, got %+v", task)
	}
}

func TestSend_NoTenantHardError(t *testing.T) {
	repo := NewMemoryReachRepo()
	s := New(repo, WithNowFunc(clock))
	if _, err := s.Send(context.Background(), SendInput{
		MemberID: 100, Channel: ChannelSMS, TemplateCode: TplRecall30,
	}); !errors.Is(err, multitenancy.ErrNoTenant) {
		t.Fatalf("expected ErrNoTenant, got %v", err)
	}
}

func TestSend_ParamValidation(t *testing.T) {
	s := New(NewMemoryReachRepo(), WithNowFunc(clock))
	cases := []SendInput{
		{MemberID: 0, Channel: ChannelSMS, TemplateCode: TplRecall30},
		{MemberID: 1, Channel: "", TemplateCode: TplRecall30},
		{MemberID: 1, Channel: ChannelSMS, TemplateCode: ""},
	}
	for i, c := range cases {
		if _, err := s.Send(ctxWithTenant(50021), c); err == nil {
			t.Errorf("case %d: expected param error", i)
		}
	}
}

// ===========================================================================
// Subscribe Grant/Consume atomicity
// ===========================================================================

func TestGrantSubscribe_Accumulate(t *testing.T) {
	repo := NewMemoryReachRepo()
	s := New(repo, WithNowFunc(clock))

	a1, err := s.GrantSubscribe(ctxWithTenant(50021), 100, TplPointsExpire, 3)
	if err != nil {
		t.Fatalf("grant1: %v", err)
	}
	if a1.GrantedQuota != 3 || a1.UsedQuota != 0 {
		t.Errorf("grant1 wrong: %+v", a1)
	}
	a2, err := s.GrantSubscribe(ctxWithTenant(50021), 100, TplPointsExpire, 2)
	if err != nil {
		t.Fatalf("grant2: %v", err)
	}
	if a2.GrantedQuota != 5 {
		t.Errorf("grant2 should accumulate to 5, got %d", a2.GrantedQuota)
	}
	if a2.UsedQuota != 0 {
		t.Errorf("accumulate must not reset used: %d", a2.UsedQuota)
	}
}

func TestConsumeSubscribe_AtomicExhausted(t *testing.T) {
	repo := NewMemoryReachRepo()
	s := New(repo, WithNowFunc(clock))
	if _, err := s.GrantSubscribe(ctxWithTenant(50021), 200, TplPointsExpire, 1); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// First consume OK.
	if _, err := s.repo.ConsumeSubscribe(ctxWithTenant(50021), 50021, 200, TplPointsExpire, clock()); err != nil {
		t.Fatalf("consume1: %v", err)
	}
	// Second consume ⇒ exhausted, no state change.
	_, err := s.repo.ConsumeSubscribe(ctxWithTenant(50021), 50021, 200, TplPointsExpire, clock())
	if !errors.Is(err, ErrSubscribeExhausted) {
		t.Fatalf("expected ErrSubscribeExhausted, got %v", err)
	}
	auth, _ := s.repo.GetSubscribe(ctxWithTenant(50021), 50021, 200, TplPointsExpire)
	if auth.UsedQuota != 1 {
		t.Errorf("exhausted consume must not bump used: %d", auth.UsedQuota)
	}
	// Absent auth ⇒ exhausted.
	if _, err := s.repo.ConsumeSubscribe(ctxWithTenant(50021), 50021, 999, TplPointsExpire, clock()); !errors.Is(err, ErrSubscribeExhausted) {
		t.Fatalf("absent auth should be exhausted, got %v", err)
	}
}

func TestGrantSubscribe_ParamValidation(t *testing.T) {
	s := New(NewMemoryReachRepo(), WithNowFunc(clock))
	for _, c := range []struct {
		member int64
		tpl    string
		quota  int
	}{
		{0, TplPointsExpire, 1},
		{1, "", 1},
		{1, TplPointsExpire, 0},
		{1, TplPointsExpire, -1},
	} {
		if _, err := s.GrantSubscribe(ctxWithTenant(50021), c.member, c.tpl, c.quota); err == nil {
			t.Errorf("member=%d tpl=%q quota=%d should reject", c.member, c.tpl, c.quota)
		}
	}
}

// ===========================================================================
// Unsubscribe block + Resubscribe recovery
// ===========================================================================

func TestUnsubscribe_ResubscribeCycle(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	freq := &fakeFreq{allowed: true}
	cf := &fakeCF{passed: true}
	s := fullSvc(repo, snd, freq, cf)

	// Baseline: send succeeds.
	if _, err := s.Send(ctxWithTenant(50021), SendInput{MemberID: 300, Channel: ChannelWeworkOneV, TemplateCode: TplRecall30}); err != nil {
		t.Fatalf("send1: %v", err)
	}
	// Opt out → next send blocked(unsubscribed).
	if _, err := s.Unsubscribe(ctxWithTenant(50021), 300, ChannelWeworkOneV, "太吵了"); err != nil {
		t.Fatalf("unsub: %v", err)
	}
	task, err := s.Send(ctxWithTenant(50021), SendInput{MemberID: 300, Channel: ChannelWeworkOneV, TemplateCode: TplRecall30})
	if err != nil {
		t.Fatalf("send2 err: %v", err)
	}
	if task.Status != StatusBlocked || task.BlockedReason != BlockUnsubscribed {
		t.Fatalf("expected blocked(unsubscribed), got %+v", task)
	}
	// Resubscribe → send succeeds again.
	if err := s.Resubscribe(ctxWithTenant(50021), 300, ChannelWeworkOneV); err != nil {
		t.Fatalf("resubscribe: %v", err)
	}
	task3, err := s.Send(ctxWithTenant(50021), SendInput{MemberID: 300, Channel: ChannelWeworkOneV, TemplateCode: TplRecall30})
	if err != nil {
		t.Fatalf("send3 err: %v", err)
	}
	if task3.Status != StatusSent {
		t.Errorf("after resubscribe send should succeed, got %+v", task3)
	}
}

func TestUnsubscribe_ChannelScoped(t *testing.T) {
	repo := NewMemoryReachRepo()
	s := New(repo, WithNowFunc(clock))
	// Opt out of wework_1v1 only; wework_group unaffected.
	if _, err := s.Unsubscribe(ctxWithTenant(50021), 400, ChannelWeworkOneV, ""); err != nil {
		t.Fatalf("unsub: %v", err)
	}
	b1, _ := s.repo.IsUnsubscribed(ctxWithTenant(50021), 50021, 400, ChannelWeworkOneV)
	b2, _ := s.repo.IsUnsubscribed(ctxWithTenant(50021), 50021, 400, ChannelWeworkGroup)
	if !b1 || b2 {
		t.Errorf("opt-out must be channel-scoped: 1v1=%v group=%v", b1, b2)
	}
}

func TestResubscribe_Idempotent(t *testing.T) {
	repo := NewMemoryReachRepo()
	s := New(repo, WithNowFunc(clock))
	// Resubscribe with no prior opt-out ⇒ no-op nil.
	if err := s.Resubscribe(ctxWithTenant(50021), 500, ChannelSMS); err != nil {
		t.Errorf("idempotent resubscribe should be nil, got %v", err)
	}
}

// ===========================================================================
// Cross-tenant isolation (串户矩阵)
// ===========================================================================

func TestCrossTenant_TaskInvisible(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	s := New(repo, WithSender(snd), WithFreqGate(&fakeFreq{allowed: true}), WithContentFilter(&fakeCF{passed: true}), WithNowFunc(clock))

	task, err := s.Send(ctxWithTenant(50021), SendInput{MemberID: 1, Channel: ChannelSMS, TemplateCode: TplRecall30})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	// Tenant B cannot read tenant A's task.
	if _, err := s.GetTask(ctxWithTenant(99999), task.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant GetTask must be ErrNotFound, got %v", err)
	}
	// Tenant B's listing is empty.
	list, err := s.ListTasks(ctxWithTenant(99999), 0, "")
	if err != nil {
		t.Fatalf("list err: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("tenant B should see 0 tasks, got %d", len(list))
	}
	// Tenant A sees its own task.
	if _, err := s.GetTask(ctxWithTenant(50021), task.ID); err != nil {
		t.Errorf("same-tenant GetTask failed: %v", err)
	}
}

func TestCrossTenant_SubscribeIsolated(t *testing.T) {
	repo := NewMemoryReachRepo()
	s := New(repo, WithNowFunc(clock))
	// Tenant A grants quota.
	if _, err := s.GrantSubscribe(ctxWithTenant(50021), 1, TplPointsExpire, 5); err != nil {
		t.Fatalf("grant: %v", err)
	}
	// Tenant B consuming the "same" (member, tpl) ⇒ exhausted (different key).
	if _, err := s.repo.ConsumeSubscribe(ctxWithTenant(99999), 99999, 1, TplPointsExpire, clock()); !errors.Is(err, ErrSubscribeExhausted) {
		t.Errorf("cross-tenant consume must be exhausted, got %v", err)
	}
	// Tenant A's quota untouched.
	auth, _ := s.repo.GetSubscribe(ctxWithTenant(50021), 50021, 1, TplPointsExpire)
	if auth.GrantedQuota != 5 || auth.UsedQuota != 0 {
		t.Errorf("tenant A quota should be pristine: %+v", auth)
	}
}

func TestCrossTenant_UnsubscribeIsolated(t *testing.T) {
	repo := NewMemoryReachRepo()
	s := New(repo, WithNowFunc(clock))
	// Tenant A opts out.
	if _, err := s.Unsubscribe(ctxWithTenant(50021), 1, ChannelSMS, ""); err != nil {
		t.Fatalf("unsub: %v", err)
	}
	// Tenant B's view: not unsubscribed (different key).
	b, _ := s.repo.IsUnsubscribed(ctxWithTenant(99999), 99999, 1, ChannelSMS)
	if b {
		t.Error("cross-tenant IsUnsubscribed must be false")
	}
}

func TestCrossTenant_ResubscribeNoOpOnForeign(t *testing.T) {
	repo := NewMemoryReachRepo()
	s := New(repo, WithNowFunc(clock))
	if _, err := s.Unsubscribe(ctxWithTenant(50021), 1, ChannelSMS, ""); err != nil {
		t.Fatalf("unsub: %v", err)
	}
	// Tenant B resubscribing its (non-existent) row ⇒ nil, and does NOT clear A's row.
	if err := s.Resubscribe(ctxWithTenant(99999), 1, ChannelSMS); err != nil {
		t.Errorf("foreign resubscribe should be nil: %v", err)
	}
	a, _ := s.repo.IsUnsubscribed(ctxWithTenant(50021), 50021, 1, ChannelSMS)
	if !a {
		t.Error("tenant B resubscribe must not clear tenant A's opt-out")
	}
}

func TestCrossTenant_ListTasksStoreScoped(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	s := New(repo, WithSender(snd), WithFreqGate(&fakeFreq{allowed: true}), WithContentFilter(&fakeCF{passed: true}), WithNowFunc(clock))
	// Two tasks under same tenant, different stores.
	if _, err := s.Send(ctxWithTenant(50021), SendInput{StoreID: 7, MemberID: 1, Channel: ChannelSMS, TemplateCode: TplRecall30}); err != nil {
		t.Fatalf("send1: %v", err)
	}
	if _, err := s.Send(ctxWithTenant(50021), SendInput{StoreID: 8, MemberID: 1, Channel: ChannelSMS, TemplateCode: TplRecall30}); err != nil {
		t.Fatalf("send2: %v", err)
	}
	all, _ := s.ListTasks(ctxWithTenant(50021), 0, "")
	if len(all) != 2 {
		t.Errorf("expected 2 tasks, got %d", len(all))
	}
	store7, _ := s.ListTasks(ctxWithTenant(50021), 7, "")
	if len(store7) != 1 || store7[0].StoreID != 7 {
		t.Errorf("store-scoped list wrong: %+v", store7)
	}
	sentOnly, _ := s.ListTasks(ctxWithTenant(50021), 0, StatusSent)
	if len(sentOnly) != 2 {
		t.Errorf("status filter wrong: %d", len(sentOnly))
	}
}

// ===========================================================================
// Concurrency: ConsumeSubscribe under -race (parallel quota drain)
// ===========================================================================

func TestConsumeSubscribe_Concurrent(t *testing.T) {
	repo := NewMemoryReachRepo()
	s := New(repo, WithNowFunc(clock))
	const granted = 50
	if _, err := s.GrantSubscribe(ctxWithTenant(50021), 700, TplPointsExpire, granted); err != nil {
		t.Fatalf("grant: %v", err)
	}
	ctx := ctxWithTenant(50021)
	var (
		successes int
		mu        sync.Mutex
		wg        sync.WaitGroup
	)
	for i := 0; i < granted; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.repo.ConsumeSubscribe(ctx, 50021, 700, TplPointsExpire, clock()); err == nil {
				mu.Lock()
				successes++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	// Exactly `granted` concurrent consumes may succeed; none over-consume.
	if successes != granted {
		t.Errorf("expected exactly %d successful consumes, got %d", granted, successes)
	}
	auth, _ := s.repo.GetSubscribe(ctx, 50021, 700, TplPointsExpire)
	if auth.UsedQuota != granted {
		t.Errorf("used should equal granted (%d), got %d", granted, auth.UsedQuota)
	}
	// One more ⇒ exhausted.
	if _, err := s.repo.ConsumeSubscribe(ctx, 50021, 700, TplPointsExpire, clock()); !errors.Is(err, ErrSubscribeExhausted) {
		t.Errorf("over-consume must be exhausted, got %v", err)
	}
}

// ---- scheduled (deferred) send: CONTRACT §19 ----

// fakeScheduledProducer records deferred-send enqueues.
type fakeScheduledProducer struct {
	calls     int
	gotIn     SendInput
	gotAt     time.Time
	gotTaskID int64
	retErr    error
}

func (f *fakeScheduledProducer) EnqueueScheduledSend(_ context.Context, in SendInput, sendAt time.Time, taskID int64) error {
	f.calls++
	f.gotIn = in
	f.gotAt = sendAt
	f.gotTaskID = taskID
	return f.retErr
}

func TestSend_ScheduledFuture_PersistsPendingAndEnqueues(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	prod := &fakeScheduledProducer{}
	s := New(repo, WithSender(snd), WithScheduledProducer(prod), WithNowFunc(clock))

	future := clock().Add(2 * time.Hour)
	in := SendInput{StoreID: 1, MemberID: 42, Channel: ChannelSubscribeMsg, TemplateCode: TplBirthday, ScheduledAt: future}
	task, err := s.Send(ctxWithTenant(7), in)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if prod.calls != 1 {
		t.Fatalf("producer calls = %d, want 1", prod.calls)
	}
	if prod.gotAt != future {
		t.Errorf("producer sendAt = %v, want %v", prod.gotAt, future)
	}
	if prod.gotIn.MemberID != 42 {
		t.Errorf("producer got memberID = %d", prod.gotIn.MemberID)
	}
	// A pending audit row is persisted at enqueue time (the scheduled send must
	// be visible immediately) and carries the future ScheduledAt.
	if task == nil || task.Status != StatusPending {
		t.Fatalf("task status = %+v, want pending", task)
	}
	if task.ID == 0 {
		t.Fatal("pending task must have an allocated id")
	}
	if !task.ScheduledAt.Equal(future) {
		t.Errorf("task.ScheduledAt = %v, want %v", task.ScheduledAt, future)
	}
	// The enqueued payload carries that row's id so the worker finalizes the
	// SAME row rather than duplicating it.
	if prod.gotTaskID != task.ID {
		t.Errorf("payload TaskID = %d, want the persisted row id %d", prod.gotTaskID, task.ID)
	}
	if len(snd.payloads) != 0 {
		t.Errorf("sender must NOT be called for a deferred send, got %d payloads", len(snd.payloads))
	}
	// The pending row is visible via ListTasks (status + ScheduledAt).
	tasks, _ := repo.ListTasks(ctxWithTenant(7), 7, 1, StatusPending)
	if len(tasks) != 1 {
		t.Fatalf("expected exactly 1 pending row at enqueue, got %d", len(tasks))
	}
	if tasks[0].ID != task.ID || !tasks[0].ScheduledAt.Equal(future) {
		t.Errorf("listed pending row mismatch: %+v", tasks[0])
	}
}

// TestSendFinalize_UpdatesExistingPendingRow proves the worker path (TaskID>0)
// flips the pre-persisted pending row to sent IN PLACE — no duplicate row.
func TestSendFinalize_UpdatesExistingPendingRow(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	prod := &fakeScheduledProducer{}
	s := New(repo, WithSender(snd), WithScheduledProducer(prod), WithNowFunc(clock))

	future := clock().Add(2 * time.Hour)
	in := SendInput{StoreID: 1, MemberID: 42, Channel: ChannelWeworkGroup, TemplateCode: TplBirthday, ScheduledAt: future}
	pending, err := s.Send(ctxWithTenant(7), in)
	if err != nil {
		t.Fatalf("enqueue Send: %v", err)
	}
	if pending.Status != StatusPending || pending.ID == 0 {
		t.Fatalf("precondition pending row with id, got %+v", pending)
	}

	// Worker path: ScheduledAt cleared (forces delivery) + TaskID set.
	deliverIn := in
	deliverIn.ScheduledAt = time.Time{}
	finalized, err := s.SendFinalize(ctxWithTenant(7), deliverIn, pending.ID)
	if err != nil {
		t.Fatalf("SendFinalize: %v", err)
	}
	if len(snd.payloads) != 1 {
		t.Errorf("worker delivery should send exactly 1 payload, got %d", len(snd.payloads))
	}
	if finalized.ID != pending.ID {
		t.Errorf("finalize must reuse the pending row id: got %d want %d", finalized.ID, pending.ID)
	}
	if finalized.Status != StatusSent {
		t.Errorf("finalized status = %q, want sent", finalized.Status)
	}
	if finalized.SentAt.IsZero() {
		t.Errorf("finalized sent row must have SentAt set")
	}
	// Exactly ONE row exists: the worker updated, it did not create a duplicate.
	tasks, _ := repo.ListTasks(ctxWithTenant(7), 7, 0, "")
	if len(tasks) != 1 {
		t.Fatalf("worker must update not duplicate: got %d rows", len(tasks))
	}
	if tasks[0].ID != pending.ID || tasks[0].Status != StatusSent {
		t.Errorf("row not flipped in place: %+v", tasks[0])
	}
}

// TestSendFinalize_BlockedUpdatesPendingRow proves a worker delivery that a guard
// intercepts flips the SAME pending row to blocked and still returns (task, nil)
// so the job ACKs.
func TestSendFinalize_BlockedUpdatesPendingRow(t *testing.T) {
	repo := NewMemoryReachRepo()
	prod := &fakeScheduledProducer{}
	snd := &fakeSender{}
	cf := &fakeCF{passed: false} // content rejects on delivery
	s := New(repo, WithSender(snd), WithContentFilter(cf), WithScheduledProducer(prod), WithNowFunc(clock))

	future := clock().Add(2 * time.Hour)
	in := SendInput{StoreID: 1, MemberID: 42, Channel: ChannelWeworkGroup, TemplateCode: TplBirthday, ScheduledAt: future}
	pending, err := s.Send(ctxWithTenant(7), in)
	if err != nil {
		t.Fatalf("enqueue Send: %v", err)
	}

	deliverIn := in
	deliverIn.ScheduledAt = time.Time{}
	blocked, err := s.SendFinalize(ctxWithTenant(7), deliverIn, pending.ID)
	if err != nil {
		t.Fatalf("a blocked delivery returns (task, nil), got err %v", err)
	}
	if blocked.ID != pending.ID || blocked.Status != StatusBlocked || blocked.BlockedReason != BlockContentFilter {
		t.Errorf("expected the same row flipped to blocked(content_filter), got %+v", blocked)
	}
	// No duplicate row; nothing was delivered.
	tasks, _ := repo.ListTasks(ctxWithTenant(7), 7, 0, "")
	if len(tasks) != 1 || tasks[0].Status != StatusBlocked {
		t.Errorf("exactly one blocked row expected, got %+v", tasks)
	}
	if len(snd.payloads) != 0 {
		t.Errorf("blocked delivery must not send, got %d payloads", len(snd.payloads))
	}
}

// TestSendFinalize_CrossTenantUpdateIsolated proves a finalize carrying a
// foreign tenant's ctx cannot touch another tenant's pending row (UpdateTask
// matches tenant_id + id ⇒ ErrNotFound ⇒ the worker error is surfaced).
func TestSendFinalize_CrossTenantUpdateIsolated(t *testing.T) {
	repo := NewMemoryReachRepo()
	prod := &fakeScheduledProducer{}
	s := New(repo, WithSender(&fakeSender{}), WithScheduledProducer(prod), WithNowFunc(clock))

	future := clock().Add(2 * time.Hour)
	in := SendInput{StoreID: 1, MemberID: 42, Channel: ChannelWeworkGroup, TemplateCode: TplBirthday, ScheduledAt: future}
	pending, err := s.Send(ctxWithTenant(7), in)
	if err != nil {
		t.Fatalf("enqueue Send: %v", err)
	}
	// Attacker tenant 99999 tries to finalize tenant 7's pending row id.
	deliverIn := in
	deliverIn.ScheduledAt = time.Time{}
	if _, err := s.SendFinalize(ctxWithTenant(99999), deliverIn, pending.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant finalize must be ErrNotFound, got %v", err)
	}
	// The row is untouched (still pending, still tenant 7's).
	got, _ := repo.GetTask(ctxWithTenant(7), 7, pending.ID)
	if got.Status != StatusPending {
		t.Errorf("foreign finalize must not touch the row, got %q", got.Status)
	}
}

// TestUpdateTask_MemoryAndTenantIsolation exercises the repo method directly:
// same-tenant update flips status/sent_at; a row claiming a foreign tenant is
// ErrNotFound and leaves the stored row unchanged.
func TestUpdateTask_MemoryAndTenantIsolation(t *testing.T) {
	repo := NewMemoryReachRepo()
	created, err := repo.CreateTask(context.Background(), &ReachTask{
		TenantID: 7, StoreID: 1, Channel: ChannelSMS, TemplateCode: TplRecall30,
		TargetCount: 1, Status: StatusPending,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Same-tenant update flips status + sent_at.
	created.Status = StatusSent
	created.SentAt = clock()
	upd, err := repo.UpdateTask(context.Background(), created)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Status != StatusSent || !upd.SentAt.Equal(clock()) {
		t.Errorf("update mismatch: %+v", upd)
	}
	// A task claiming a foreign tenant cannot update tenant 7's row.
	foreign := *created
	foreign.TenantID = 99999
	foreign.Status = StatusBlocked
	if _, err := repo.UpdateTask(context.Background(), &foreign); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-tenant update must be ErrNotFound, got %v", err)
	}
	got, _ := repo.GetTask(context.Background(), 7, created.ID)
	if got.Status != StatusSent {
		t.Errorf("foreign update must not touch the row, got %q", got.Status)
	}
}

func TestSend_ScheduledFuture_NoProducer_FallsBackImmediate(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	s := New(repo, WithSender(snd), WithNowFunc(clock)) // no producer wired

	future := clock().Add(2 * time.Hour)
	in := SendInput{StoreID: 1, MemberID: 42, Channel: ChannelWeworkGroup, TemplateCode: TplBirthday, ScheduledAt: future}
	task, err := s.Send(ctxWithTenant(7), in)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(snd.payloads) != 1 {
		t.Errorf("without a producer the send must be immediate, got %d payloads", len(snd.payloads))
	}
	if task.Status == StatusPending {
		t.Errorf("immediate send must not stay pending, got %q", task.Status)
	}
}

func TestSend_ScheduledPast_SendsImmediately(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	prod := &fakeScheduledProducer{}
	s := New(repo, WithSender(snd), WithScheduledProducer(prod), WithNowFunc(clock))

	past := clock().Add(-time.Hour)
	in := SendInput{StoreID: 1, MemberID: 42, Channel: ChannelWeworkGroup, TemplateCode: TplBirthday, ScheduledAt: past}
	if _, err := s.Send(ctxWithTenant(7), in); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if prod.calls != 0 {
		t.Errorf("past schedule must not enqueue, producer calls = %d", prod.calls)
	}
	if len(snd.payloads) != 1 {
		t.Errorf("past schedule must send immediately, got %d payloads", len(snd.payloads))
	}
}

func TestSend_ScheduledEnqueueError_Propagates(t *testing.T) {
	repo := NewMemoryReachRepo()
	prod := &fakeScheduledProducer{retErr: errors.New("queue down")}
	s := New(repo, WithScheduledProducer(prod), WithNowFunc(clock))

	in := SendInput{StoreID: 1, MemberID: 42, Channel: ChannelSubscribeMsg, TemplateCode: TplBirthday, ScheduledAt: clock().Add(time.Hour)}
	if _, err := s.Send(ctxWithTenant(7), in); err == nil {
		t.Fatal("enqueue error must propagate")
	}
	// The row persisted before the (failed) enqueue must not be left dangling as
	// pending — it is best-effort flipped to failed so the audit trail is honest.
	if pend, _ := repo.ListTasks(ctxWithTenant(7), 7, 0, StatusPending); len(pend) != 0 {
		t.Errorf("failed enqueue must not leave a pending row, got %d", len(pend))
	}
	if failed, _ := repo.ListTasks(ctxWithTenant(7), 7, 0, StatusFailed); len(failed) != 1 {
		t.Errorf("the pre-persisted row should be marked failed, got %d", len(failed))
	}
}

// ===========================================================================
// Delivery-time recheck (CONTRACT §20.3 follow-up — "投递时复检")
//
// A deferred send is booked with NO guard checks (the deferred branch persists a
// pending row + enqueues and returns before any compliance step). The worker
// later re-drives it through SendFinalize, which MUST re-check unsubscribe +
// frequency immediately before the sender so a LATE opt-out / freq trip between
// booking and delivery blocks the message instead of delivering it.
// ===========================================================================

// TestSendFinalize_LateUnsubscribe_BlocksDelivery: book a deferred send, then the
// member opts out BEFORE delivery. The worker finalize must block(unsubscribed),
// flip the SAME pending row (no duplicate), send nothing, and return (task, nil)
// so the job ACKs.
func TestSendFinalize_LateUnsubscribe_BlocksDelivery(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	freq := &fakeFreq{allowed: true}
	cf := &fakeCF{passed: true}
	prod := &fakeScheduledProducer{}
	s := New(repo, WithSender(snd), WithFreqGate(freq), WithContentFilter(cf), WithScheduledProducer(prod), WithNowFunc(clock))

	future := clock().Add(2 * time.Hour)
	in := SendInput{StoreID: 1, MemberID: 42, Channel: ChannelWeworkGroup, TemplateCode: TplBirthday, ScheduledAt: future}
	pending, err := s.Send(ctxWithTenant(7), in)
	if err != nil {
		t.Fatalf("enqueue Send: %v", err)
	}
	if pending.Status != StatusPending {
		t.Fatalf("precondition: pending row, got %+v", pending)
	}

	// LATE opt-out: after booking, before the worker delivers.
	if _, err := s.Unsubscribe(ctxWithTenant(7), 42, ChannelWeworkGroup, "迟到退订"); err != nil {
		t.Fatalf("unsub: %v", err)
	}

	deliverIn := in
	deliverIn.ScheduledAt = time.Time{} // worker clears ScheduledAt to force delivery
	blocked, err := s.SendFinalize(ctxWithTenant(7), deliverIn, pending.ID)
	if err != nil {
		t.Fatalf("a blocked delivery returns (task, nil), got err %v", err)
	}
	if blocked.ID != pending.ID || blocked.Status != StatusBlocked || blocked.BlockedReason != BlockUnsubscribed {
		t.Fatalf("expected same row flipped to blocked(unsubscribed), got %+v", blocked)
	}
	if len(snd.payloads) != 0 {
		t.Errorf("late-unsubscribed delivery must NOT send, got %d payloads", len(snd.payloads))
	}
	// Exactly one row, flipped in place to blocked (not sent, no duplicate).
	tasks, _ := repo.ListTasks(ctxWithTenant(7), 7, 0, "")
	if len(tasks) != 1 || tasks[0].Status != StatusBlocked {
		t.Errorf("exactly one blocked row expected, got %+v", tasks)
	}
}

// TestSendFinalize_LateFreqTrip_BlocksDelivery: the frequency cap is tripped
// BETWEEN booking and delivery. The worker finalize must block(freq_circuit).
func TestSendFinalize_LateFreqTrip_BlocksDelivery(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	freq := &fakeFreq{allowed: true} // clear at booking (booking checks nothing anyway)
	cf := &fakeCF{passed: true}
	prod := &fakeScheduledProducer{}
	s := New(repo, WithSender(snd), WithFreqGate(freq), WithContentFilter(cf), WithScheduledProducer(prod), WithNowFunc(clock))

	future := clock().Add(2 * time.Hour)
	in := SendInput{StoreID: 1, MemberID: 42, Channel: ChannelWeworkGroup, TemplateCode: TplBirthday, ScheduledAt: future}
	pending, err := s.Send(ctxWithTenant(7), in)
	if err != nil {
		t.Fatalf("enqueue Send: %v", err)
	}

	// LATE freq trip: the channel cap is hit after booking, before delivery.
	freq.allowed = false

	deliverIn := in
	deliverIn.ScheduledAt = time.Time{}
	blocked, err := s.SendFinalize(ctxWithTenant(7), deliverIn, pending.ID)
	if err != nil {
		t.Fatalf("a blocked delivery returns (task, nil), got err %v", err)
	}
	if blocked.ID != pending.ID || blocked.Status != StatusBlocked || blocked.BlockedReason != BlockFreqCircuit {
		t.Fatalf("expected same row flipped to blocked(freq_circuit), got %+v", blocked)
	}
	if len(snd.payloads) != 0 {
		t.Errorf("over-frequency delivery must NOT send, got %d payloads", len(snd.payloads))
	}
}

// TestSendFinalize_RecheckPasses_SendsNormally: with freq allowed and no opt-out,
// the delivery-time recheck does NOT over-block — a deferred send still lands as
// sent (the recheck is a guard, not a new failure mode on the happy path).
func TestSendFinalize_RecheckPasses_SendsNormally(t *testing.T) {
	repo := NewMemoryReachRepo()
	snd := &fakeSender{}
	freq := &fakeFreq{allowed: true}
	cf := &fakeCF{passed: true}
	prod := &fakeScheduledProducer{}
	s := New(repo, WithSender(snd), WithFreqGate(freq), WithContentFilter(cf), WithScheduledProducer(prod), WithNowFunc(clock))

	future := clock().Add(2 * time.Hour)
	in := SendInput{StoreID: 1, MemberID: 42, Channel: ChannelWeworkGroup, TemplateCode: TplBirthday, ScheduledAt: future}
	pending, err := s.Send(ctxWithTenant(7), in)
	if err != nil {
		t.Fatalf("enqueue Send: %v", err)
	}

	deliverIn := in
	deliverIn.ScheduledAt = time.Time{}
	sent, err := s.SendFinalize(ctxWithTenant(7), deliverIn, pending.ID)
	if err != nil {
		t.Fatalf("SendFinalize: %v", err)
	}
	if sent.ID != pending.ID || sent.Status != StatusSent {
		t.Fatalf("expected same row flipped to sent, got %+v", sent)
	}
	if len(snd.payloads) != 1 {
		t.Errorf("clean delivery should send exactly 1 payload, got %d", len(snd.payloads))
	}
	// The recheck consulted the freq gate exactly once (on the delivery attempt).
	if freq.calls != 1 {
		t.Errorf("freq recheck should run once at delivery, got %d calls", freq.calls)
	}
}

// errUnsubRepo wraps a ReachRepo and injects an error into IsUnsubscribed when
// fail is set — used to prove the delivery-time recheck fails CLOSED on an
// unsubscribe-probe error (strict: compliance over availability).
type errUnsubRepo struct {
	ReachRepo
	fail bool
}

func (r *errUnsubRepo) IsUnsubscribed(ctx context.Context, tenantID, memberID int64, channel string) (bool, error) {
	if r.fail {
		return false, errors.New("unsub probe down")
	}
	return r.ReachRepo.IsUnsubscribed(ctx, tenantID, memberID, channel)
}

// TestSendFinalize_RecheckDegradation covers the two recheck dependencies failing:
//   - freq gate UNWIRED (nil) ⇒ the recheck skips it and delivery proceeds
//     (availability: a missing optional gate never stalls a send);
//   - unsubscribe probe ERROR ⇒ delivery fails with a hard error and sends nothing
//     (strict: we would rather delay than message a possibly opted-out member).
func TestSendFinalize_RecheckDegradation(t *testing.T) {
	// (a) No freq gate wired: the recheck must skip freq and still deliver.
	t.Run("freq gate absent proceeds", func(t *testing.T) {
		repo := NewMemoryReachRepo()
		snd := &fakeSender{}
		prod := &fakeScheduledProducer{}
		// NOTE: no WithFreqGate — the optional port is nil.
		s := New(repo, WithSender(snd), WithScheduledProducer(prod), WithNowFunc(clock))

		future := clock().Add(2 * time.Hour)
		in := SendInput{StoreID: 1, MemberID: 42, Channel: ChannelWeworkGroup, TemplateCode: TplBirthday, ScheduledAt: future}
		pending, err := s.Send(ctxWithTenant(7), in)
		if err != nil {
			t.Fatalf("enqueue Send: %v", err)
		}
		deliverIn := in
		deliverIn.ScheduledAt = time.Time{}
		sent, err := s.SendFinalize(ctxWithTenant(7), deliverIn, pending.ID)
		if err != nil {
			t.Fatalf("a nil freq gate must not stall delivery: %v", err)
		}
		if sent.Status != StatusSent || len(snd.payloads) != 1 {
			t.Errorf("nil freq gate should still deliver, got %+v payloads=%d", sent, len(snd.payloads))
		}
	})

	// (b) Unsubscribe probe errors: delivery must fail closed (hard error, no send).
	t.Run("unsub probe error fails closed", func(t *testing.T) {
		base := NewMemoryReachRepo()
		repo := &errUnsubRepo{ReachRepo: base, fail: true}
		snd := &fakeSender{}
		freq := &fakeFreq{allowed: true}
		prod := &fakeScheduledProducer{}
		s := New(repo, WithSender(snd), WithFreqGate(freq), WithScheduledProducer(prod), WithNowFunc(clock))

		future := clock().Add(2 * time.Hour)
		in := SendInput{StoreID: 1, MemberID: 42, Channel: ChannelWeworkGroup, TemplateCode: TplBirthday, ScheduledAt: future}
		pending, err := s.Send(ctxWithTenant(7), in)
		if err != nil {
			t.Fatalf("enqueue Send: %v", err)
		}
		deliverIn := in
		deliverIn.ScheduledAt = time.Time{}
		task, err := s.SendFinalize(ctxWithTenant(7), deliverIn, pending.ID)
		if err == nil {
			t.Fatalf("an unsubscribe-probe error must surface as a hard error, got task=%+v", task)
		}
		if len(snd.payloads) != 0 {
			t.Errorf("strict recheck must NOT deliver on probe error, got %d payloads", len(snd.payloads))
		}
		// The pending row is left untouched (still pending) so the worker redelivers
		// once the probe recovers — it is neither silently sent nor wrongly blocked.
		got, _ := base.GetTask(ctxWithTenant(7), 7, pending.ID)
		if got.Status != StatusPending {
			t.Errorf("probe error must leave the row pending for redelivery, got %q", got.Status)
		}
	})
}
