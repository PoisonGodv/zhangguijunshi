package main

// payment_adapters.go adapts the three fulfilment domains (billing /
// membership / subscription) into the single payment.Fulfiller port the
// payment Service calls after a genuine pending→success transition (CONTRACT
// §15.7). The notify handler injects the resolved tenant into ctx first, so
// each domain's EnforceTenant sees the payment's owning tenant.

import (
	"context"

	"github.com/zhangguijunshi/backend/internal/billing"
	"github.com/zhangguijunshi/backend/internal/membership"
	"github.com/zhangguijunshi/backend/internal/payment"
	"github.com/zhangguijunshi/backend/internal/subscription"
)

// paymentFulfiller routes a successful payment to the domain its Subject names.
// nil domains are skipped (dev may wire only a subset), so a missing service
// never panics — the payment is still recorded as success.
type paymentFulfiller struct {
	bill *billing.Service
	mbr  *membership.Service
	sub  *subscription.Service
}

// Compile-time assertion that paymentFulfiller satisfies the port.
var _ payment.Fulfiller = (*paymentFulfiller)(nil)

// Fulfill dispatches by payment.Subject. Best-effort: HandleNotify swallows the
// returned error after logging — the payment stays success and the
// reconciliation job reconciles later (反二清: the fund already cleared to the
// sub-merchant, fulfilment is book-keeping only).
func (f paymentFulfiller) Fulfill(ctx context.Context, p *payment.Payment) error {
	switch p.Subject {
	case payment.SubjectOrder:
		if f.bill == nil || p.PaidAt == nil {
			return nil
		}
		_, err := f.bill.MarkOrderPaid(ctx, p.RefID, p.OutTradeNo, *p.PaidAt)
		return err
	case payment.SubjectRecharge:
		if f.mbr == nil {
			return nil
		}
		_, err := f.mbr.Recharge(ctx, membership.RechargeInput{
			MemberID:     p.RefID,
			AmountCents:  p.AmountCents,
			RefPaymentID: p.ID,
		})
		return err
	case payment.SubjectSubscription:
		if f.sub == nil {
			return nil
		}
		_, err := f.sub.ConfirmPayment(ctx, p.TenantID, p.AmountCents, p.Provider, p.OutTradeNo)
		return err
	}
	return nil
}
