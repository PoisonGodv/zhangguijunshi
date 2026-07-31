package reach

// Service orchestrates the reach pipeline (BE-B-07+08+10, CONTRACT §11.3.2).
// Every method first resolves the tenant from ctx via
// multitenancy.EnforceTenant — tenant is NEVER trusted from a request field.
//
// The Service depends only on the ReachRepo interface plus three optional
// anti-corruption ports (Sender / FreqGate / ContentFilter), so the whole flow
// is deterministic in unit tests (in-memory repo + injected fake guards + fixed
// clock). A port left nil means that pipeline step is skipped (dev renders +
// audits only).
//
// Send runs its guards in a FIXED order (see Send doc). A guard rejection
// persists a blocked ReachTask and returns (task, nil) — NOT an error — so the
// caller can branch on "intercepted" vs "system failure".

import (
	"context"
	"errors"
	"time"

	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// Service is the reach orchestrator.
type Service struct {
	repo   ReachRepo
	sender Sender        // nil = skip send step
	freq   FreqGate      // nil = skip frequency check
	cf     ContentFilter // nil = skip content check
	// scheduledProducer defers future sends to the async job queue (nil =
	// scheduled sends fall back to immediate delivery). CONTRACT §19.
	scheduledProducer ScheduledSendProducer
	now               func() time.Time
}

// Render delegates to the pure package-level Render so it is callable even when
// no Sender / FreqGate / ContentFilter is wired. Unknown code ⇒
// ErrTemplateUnknown.
func (s *Service) Render(in RenderInput) (Rendered, error) {
	return Render(in)
}

// Send runs the full reach pipeline for one attempt and persists a NEW audit
// row. It is the user/merchant-facing entry point and is exactly
// SendFinalize(ctx, in, 0). See SendFinalize for the fixed guard order and the
// (task, nil)-on-block vs (nil, err)-on-failure contract.
func (s *Service) Send(ctx context.Context, in SendInput) (*ReachTask, error) {
	return s.SendFinalize(ctx, in, 0)
}

// SendFinalize runs the full reach pipeline for one attempt. Guard order is
// FIXED (CONTRACT §11.3.2 / §11.5.6 — do not reorder):
//
//  1. EnforceTenant (no tenant ⇒ hard error)
//  2. Render (unknown template code ⇒ ErrTemplateUnknown, a HARD error not a
//     block; otherwise title + body resolved with sensible defaults)
//  3. ContentFilter (if wired).Do(body) ⇒ !Passed ⇒ blocked(content_filter)
//  4. recheckBeforeSend — the DELIVERY-TIME compliance recheck (CONTRACT §20.3
//     follow-up): FreqGate (if wired).Allow(channel) ⇒ !allowed ⇒
//     blocked(freq_circuit), then IsUnsubscribed(channel) ⇒ true ⇒
//     blocked(unsubscribed). It runs on every attempt immediately before the
//     sender so a deferred send honors a late unsubscribe / a frequency cap
//     tripped between booking and delivery. See recheckBeforeSend for the
//     degradation tradeoffs (freq optional ⇒ skipped when unwired; unsub strict
//     ⇒ an error fails the attempt rather than delivering).
//  5. channel==subscribe_msg ⇒ GetSubscribe precheck ⇒ absent/exhausted ⇒
//     blocked(subscribe_quota)
//  6. Sender (if wired).Send ⇒ failure status=failed (quota NOT consumed)
//  7. channel==subscribe_msg ⇒ ConsumeSubscribe (only after successful send)
//
// taskID selects the persistence mode at every terminal point (block / fail /
// sent):
//
//	taskID == 0 ⇒ CreateTask a NEW row (user-facing Send, and legacy jobs).
//	taskID >  0 ⇒ UpdateTask the EXISTING row with that id (the deferred-send
//	              worker finalizes the pending row persisted at enqueue time
//	              instead of creating a duplicate). The tenant is still resolved
//	              from ctx, and UpdateTask matches on tenant_id + id, so a
//	              cross-tenant finalize is ErrNotFound — isolation holds.
//
// A blocked outcome is persisted as a ReachTask (status=blocked + reason) and
// returned as (task, nil). A real system failure is returned as (nil, err). A
// blocked attempt consumes no count beyond the subscribe quota (and the
// frequency circuit rolls its own Incr back inside Allow on its own block).
func (s *Service) SendFinalize(ctx context.Context, in SendInput, taskID int64) (*ReachTask, error) {
	// ① tenant from ctx, never from the request body.
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	// StoreID is validated alongside the other identity fields (bug fix: the PG
	// reach_tasks.store_id FK rejects store_id=0 while the memory repo accepted
	// it, so an unvalidated StoreID=0 diverged across the two implementations).
	// This single guard sits BEFORE both the deferred-send branch and persist, so
	// it covers the immediate AND the scheduled paths in one place.
	if in.StoreID <= 0 || in.MemberID <= 0 || in.TemplateCode == "" || in.Channel == "" {
		return nil, errcode.ErrParam
	}

	now := s.now()

	// Deferred send (CONTRACT §19 / §19.9): a future ScheduledAt with a wired
	// producer first persists a PENDING ReachTask row (so a scheduled send is
	// visible via ListTasks immediately — the audit trail must not be silent),
	// then enqueues the job with that row's id in the payload. The worker
	// re-drives it through SendFinalize (ScheduledAt cleared, TaskID set) which
	// finalizes THIS row rather than inserting a duplicate. The row is persisted
	// BEFORE enqueueing; if the enqueue then fails the row is best-effort flipped
	// to failed (never left dangling as a pending that will never fire) and the
	// enqueue error is returned. This branch is only reachable with taskID==0 —
	// the worker clears ScheduledAt before calling — so it always creates a NEW
	// row. Without a producer (dev, no Redis) the branch is skipped and the send
	// is immediate.
	if !in.ScheduledAt.IsZero() && in.ScheduledAt.After(now) && s.scheduledProducer != nil {
		created, cerr := s.repo.CreateTask(ctx, &ReachTask{
			TenantID:     tenantID,
			StoreID:      in.StoreID,
			StrategyID:   in.StrategyID,
			Channel:      in.Channel,
			TemplateCode: in.TemplateCode,
			TargetCount:  1,
			Status:       StatusPending,
			ScheduledAt:  in.ScheduledAt,
			CreatedAt:    now,
		})
		if cerr != nil {
			return nil, cerr
		}
		if err := s.scheduledProducer.EnqueueScheduledSend(ctx, in, in.ScheduledAt, created.ID); err != nil {
			// Best-effort: mark the pre-persisted row failed so it is not left as
			// a pending row that nothing will ever deliver. Surface the enqueue
			// error regardless (the update is fire-and-forget).
			created.Status = StatusFailed
			_, _ = s.repo.UpdateTask(ctx, created)
			return nil, err
		}
		return created, nil
	}

	// ② render (unknown code = hard error, not a block).
	rendered, err := Render(RenderInput{
		Code:       in.TemplateCode,
		MemberName: varOr(in.Vars, "member_name"),
		StoreName:  varOr(in.Vars, "store_name"),
		Vars:       in.Vars,
	})
	if err != nil {
		return nil, err
	}

	// Seed the audit row; TargetCount=1 for a single-member send. StrategyID is
	// the AI attribution link (0 = non-AI / manual).
	task := &ReachTask{
		TenantID:     tenantID,
		StoreID:      in.StoreID,
		StrategyID:   in.StrategyID,
		Channel:      in.Channel,
		TemplateCode: in.TemplateCode,
		TargetCount:  1,
		Status:       StatusPending,
		ScheduledAt:  now,
		CreatedAt:    now,
	}
	title, body := rendered.Title, rendered.Body

	// ③ content filter (optional). Use the cleaned body (absolutism words
	// replaced) downstream when it passes.
	if s.cf != nil {
		res := s.cf.Do(body)
		if !res.Passed {
			return s.block(ctx, task, BlockContentFilter, taskID)
		}
		if res.Text != "" {
			body = res.Text
		}
	}

	// ④+⑤ delivery-time compliance recheck (frequency circuit + unsubscribe
	// opt-out). This single call IS the "投递时复检" (CONTRACT §20.3 follow-up):
	// it runs on EVERY send attempt immediately before the sender, so a deferred
	// send — booked earlier with NO guards (the deferred branch above persists +
	// enqueues and returns before any check) and delivered later by the worker via
	// SendFinalize — honors a LATE unsubscribe or a frequency cap tripped between
	// booking and delivery. The message is blocked (status≠sent), never delivered
	// to an opted-out member. See recheckBeforeSend for the degradation tradeoffs.
	if reason, rerr := s.recheckBeforeSend(ctx, tenantID, in); rerr != nil {
		return nil, rerr
	} else if reason != "" {
		return s.block(ctx, task, reason, taskID)
	}

	// ⑥ subscribe-message one-time quota PRECHECK. Bug fix: the actual
	// ConsumeSubscribe was moved to AFTER a successful delivery (step ⑧) so a
	// failed send no longer burns the member's one-time authorization with no
	// rollback. We still reject up front when there is nothing to consume —
	// absent auth (ErrNotFound) or used>=granted ⇒ blocked(subscribe_quota) — so
	// an exhausted member never reaches the sender. Any other GetSubscribe error
	// is a real system fault and is surfaced.
	if in.Channel == ChannelSubscribeMsg {
		auth, gerr := s.repo.GetSubscribe(ctx, tenantID, in.MemberID, in.TemplateCode)
		switch {
		case errors.Is(gerr, ErrNotFound):
			return s.block(ctx, task, BlockSubscribeQuota, taskID)
		case gerr != nil:
			return nil, gerr
		case auth.UsedQuota >= auth.GrantedQuota:
			return s.block(ctx, task, BlockSubscribeQuota, taskID)
		}
	}

	// ⑦ actual delivery (optional). On failure record status=failed but still
	// return the task (the attempt happened; it just did not land) — and, for
	// subscribe_msg, the one-time quota is NOT consumed, because the consume now
	// happens only after a successful send (step ⑧).
	if s.sender != nil {
		if serr := s.sender.Send(ctx, &SendPayload{
			TenantID:     tenantID,
			MemberID:     in.MemberID,
			Channel:      in.Channel,
			TemplateCode: in.TemplateCode,
			Title:        title,
			Body:         body,
		}); serr != nil {
			task.Status = StatusFailed
			return s.persist(ctx, task, taskID)
		}
	}

	// ⑧ consume the one-time subscribe quota now that delivery has succeeded (or
	// the sender is unwired — dev renders + audits only, which preserves the
	// prior behaviour of recording the consumption). A concurrent drain between
	// the ⑥ precheck and this consume can surface ErrSubscribeExhausted; the
	// message already landed, so we must NOT relabel it blocked/failed nor
	// trigger a worker redelivery — we tolerate that rare race and persist sent.
	// Any other consume error is a real system fault and is surfaced.
	if in.Channel == ChannelSubscribeMsg {
		if _, cerr := s.repo.ConsumeSubscribe(ctx, tenantID, in.MemberID, in.TemplateCode, now); cerr != nil && !errors.Is(cerr, ErrSubscribeExhausted) {
			return nil, cerr
		}
	}

	task.Status = StatusSent
	task.SentAt = now
	return s.persist(ctx, task, taskID)
}

// recheckBeforeSend is the delivery-time compliance recheck (CONTRACT §20.3
// follow-up — "投递时复检"). It runs the frequency circuit then the unsubscribe
// opt-out, in that order (matching the pipeline's ④→⑤ order), immediately before
// the sender. It is invoked on EVERY send attempt — immediate AND deferred — so a
// deferred send (booked earlier with no guards and delivered later by the worker
// via SendFinalize) honors a LATE unsubscribe or a frequency cap tripped between
// booking and delivery instead of messaging an opted-out / over-frequency member.
//
// Return contract:
//
//	("", nil)     — clear to deliver
//	(reason, nil) — blocked; reason is BlockFreqCircuit or BlockUnsubscribed
//	("", err)     — system failure; the caller surfaces it (worker redelivers)
//
// Degradation tradeoffs (availability vs compliance):
//   - Frequency: the FreqGate port is OPTIONAL. nil (dev / unwired) ⇒ the recheck
//     skips it, so a missing gate never stalls delivery. A wired gate that errors
//     is a real system failure and is surfaced (identical to the immediate-send
//     path) — a live gate's error is never silently swallowed.
//   - Unsubscribe: STRICT. The repo is a mandatory Service dependency, so an
//     IsUnsubscribed error is surfaced as a hard error (the attempt fails and, on
//     the worker path, redelivers) rather than delivering to a possibly opted-out
//     member. Compliance wins over availability: we would rather delay a send than
//     message someone who opted out.
//
// Quota (subscribe_msg) and the content filter are intentionally NOT rechecked:
//   - Quota is now consumed exactly once, at step ⑧ — AFTER a successful send
//     (bug fix: a failed delivery no longer burns the one-time authorization).
//     Step ⑥ is only a PRECHECK (GetSubscribe) that rejects an exhausted member
//     before reaching the sender; it does not mutate quota, so there is nothing
//     for recheckBeforeSend to double-count. The deferred branch returns BEFORE
//     step ⑥, so a deferred subscribe_msg send's quota is consumed at delivery
//     on its first and only pass through ⑧.
//   - The content filter is a pure function of the rendered body, which is byte
//     identical at booking and delivery; re-running it adds cost with no new signal.
func (s *Service) recheckBeforeSend(ctx context.Context, tenantID int64, in SendInput) (string, error) {
	// Frequency circuit (optional port; nil ⇒ skipped, never stalls delivery).
	if s.freq != nil {
		allowed, _, ferr := s.freq.Allow(ctx, in.Channel)
		if ferr != nil {
			return "", ferr
		}
		if !allowed {
			return BlockFreqCircuit, nil
		}
	}
	// Unsubscribe opt-out (strict: an error blocks delivery, never a pass).
	unsub, uerr := s.repo.IsUnsubscribed(ctx, tenantID, in.MemberID, in.Channel)
	if uerr != nil {
		return "", uerr
	}
	if unsub {
		return BlockUnsubscribed, nil
	}
	return "", nil
}

// persist writes the finalized task: taskID>0 updates the pre-persisted pending
// row in place (worker delivery — no duplicate row); taskID==0 creates a new row
// (user-facing send). Every terminal branch of SendFinalize funnels through here.
func (s *Service) persist(ctx context.Context, task *ReachTask, taskID int64) (*ReachTask, error) {
	if taskID > 0 {
		task.ID = taskID
		return s.repo.UpdateTask(ctx, task)
	}
	return s.repo.CreateTask(ctx, task)
}

// block persists task as blocked with the given reason and returns (task, nil).
// Used by every guard-rejection branch in SendFinalize. Honours taskID (see
// persist): a worker-driven block updates the pending row rather than inserting
// a new blocked row.
func (s *Service) block(ctx context.Context, task *ReachTask, reason string, taskID int64) (*ReachTask, error) {
	task.Status = StatusBlocked
	task.BlockedReason = reason
	return s.persist(ctx, task, taskID)
}

// varOr returns vars[key] or "" — a small helper to bridge SendInput.Vars into
// RenderInput's explicit name fields.
func varOr(vars map[string]string, key string) string {
	if vars == nil {
		return ""
	}
	return vars[key]
}

// GrantSubscribe idempotently accumulates subscribe quota for (ctx tenant,
// memberID, tplID). The repo makes an existing auth's GrantedQuota += quota.
func (s *Service) GrantSubscribe(ctx context.Context, memberID int64, tplID string, quota int) (*SubscribeAuth, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if memberID <= 0 || tplID == "" || quota <= 0 {
		return nil, errcode.ErrParam
	}
	return s.repo.GrantSubscribe(ctx, tenantID, memberID, tplID, quota, s.now())
}

// Unsubscribe records a member's opt-out from a channel (one row per member +
// channel; repeat calls refresh reason/timestamp).
func (s *Service) Unsubscribe(ctx context.Context, memberID int64, channel, reason string) (*UnsubscribeRecord, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	if memberID <= 0 || channel == "" {
		return nil, errcode.ErrParam
	}
	return s.repo.Unsubscribe(ctx, tenantID, memberID, channel, reason, s.now())
}

// Resubscribe clears a member's opt-out from a channel (idempotent).
func (s *Service) Resubscribe(ctx context.Context, memberID int64, channel string) error {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return err
	}
	if memberID <= 0 || channel == "" {
		return errcode.ErrParam
	}
	return s.repo.Resubscribe(ctx, tenantID, memberID, channel)
}

// ListTasks returns tasks for the ctx tenant, optionally narrowed by storeID
// (>0) and status (non-empty). Cross-tenant rows are invisible.
func (s *Service) ListTasks(ctx context.Context, storeID int64, status string) ([]*ReachTask, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.ListTasks(ctx, tenantID, storeID, status)
}

// GetTask loads one task for the ctx tenant. Cross-tenant ⇒ ErrNotFound.
func (s *Service) GetTask(ctx context.Context, id int64) (*ReachTask, error) {
	tenantID, err := multitenancy.EnforceTenant(ctx)
	if err != nil {
		return nil, err
	}
	return s.repo.GetTask(ctx, tenantID, id)
}
