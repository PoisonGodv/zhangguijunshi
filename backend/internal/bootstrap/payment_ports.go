package bootstrap

// payment_ports.go selects the WeChat-Pay V3 implementation for the three
// payment.Service ports (CONTRACT §15). When the merchant V3 credentials are
// configured (cfg.Payment.Configured) it builds the real V3Client (v3wire.go,
// signing via sign.go); otherwise — and on any V3 init failure — it falls back
// to the deterministic, never-network FakeWxPay so dev / tests never touch the
// network.

import (
	"go.uber.org/zap"

	"github.com/zhangguijunshi/backend/internal/payment"
	"github.com/zhangguijunshi/backend/pkg/config"
)

// paymentPorts is the triple the payment Service consumes.
type paymentPorts struct {
	applier      payment.SubMerchantApplier
	preparer     payment.Preparer
	notifyParser payment.NotifyParser
}

func buildPaymentPorts(cfg *config.Config, log *zap.Logger) paymentPorts {
	if cfg.Payment.Configured() {
		vc, err := payment.NewV3Client(cfg.Payment, nil)
		if err == nil {
			// PaymentConfig carries no appid (it is the mini-program/公众号 id,
			// owned by the WX config); the V3 client needs it for the JSAPI
			// lift-off params (appId / paySign). Thread it in at the root.
			vc.AppID = cfg.WX.AppID
			log.Info("payment: real WeChat Pay V3 client enabled",
				zap.String("mch_id", cfg.Payment.MchID))
			return paymentPorts{applier: vc, preparer: vc, notifyParser: vc}
		}
		log.Warn("payment: V3 client init failed; falling back to FakeWxPay", zap.Error(err))
	} else {
		log.Info("payment: no V3 credentials; using FakeWxPay (dev/test)")
	}
	fake := payment.NewFakeWxPay()
	return paymentPorts{applier: fake, preparer: fake, notifyParser: fake}
}
