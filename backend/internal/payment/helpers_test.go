package payment

// helpers_test.go shared test fixtures: a fixed clock, a tenant-bearing ctx
// builder, and a FakeWxPay notify body constructor. Shared across the
// memory_repo / pg_repo / service test files (same package).

import (
	"context"
	"fmt"
	"time"

	"github.com/zhangguijunshi/backend/pkg/multitenancy"
)

// fixedTime is the deterministic clock used everywhere a test needs a stable
// timestamp (applied_at / paid_at / ledger created_at via WithNowFunc).
var fixedTime = time.Date(2026, time.July, 28, 10, 0, 0, 0, time.UTC)

// fixedLocation is Asia/Shanghai (+08:00) for RFC3339 success_time in notify
// bodies, matching the business timezone.
var fixedLocation = time.FixedZone("CST", 8*3600)

// tenantCtx returns a ctx carrying (tenantID, storeID) as a merchant source.
func tenantCtx(tenantID, storeID int64) context.Context {
	return multitenancy.WithContext(context.Background(), multitenancy.Context{
		TenantID: tenantID,
		StoreID:  storeID,
		Source:   multitenancy.SourceMerchant,
	})
}

// fakeBody builds a FakeWxPay-shaped notify body for outTradeNo / txID.
func fakeBody(outTradeNo, txID string, amount int64, success bool) []byte {
	state := "SUCCESS"
	if !success {
		state = "PAYERROR"
	}
	t := fixedTime.In(fixedLocation)
	return []byte(fmt.Sprintf(
		`{"out_trade_no":%q,"transaction_id":%q,"amount_total":%d,"trade_state":%q,"success_time":%q}`,
		outTradeNo, txID, amount, state, t.Format(time.RFC3339),
	))
}
