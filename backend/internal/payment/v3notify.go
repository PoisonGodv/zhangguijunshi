package payment

// v3notify.go implements V3Client.ParseNotify (CONTRACT §15.5.4 命门 +
// §15.5.8): the real notify path verifies the WeChat V3 signature against the
// platform certificate, then AES-256-GCM-decrypts the resource body and
// normalises the transaction into NotifyResult. Signature/clock failure is
// reported as ErrSignInvalid so HandleNotify knows to leave the DB untouched;
// any later decrypt/parse failure also returns an error (DB still untouched —
// no mutation happens in this method).
//
// FakeWxPay.ParseNotify bypasses all of this (JSON decode of a flat body); the
// two implementations are interchangeable behind the NotifyParser port.

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

// ParseNotify verifies + decrypts a WeChat Pay V3 notify.
//
// Flow:
//  1. Resolve the Wechatpay-Serial → platform public key (cert cache; miss fetches
//     /v3/certificates). Cert failure → ErrSignInvalid (fail closed: we cannot
//     verify, so we must not trust).
//  2. VerifyV3Notify checks the four Wechatpay-* headers, the ±5min freshness
//     (replay protection) and the RSA-SHA256 signature. Failure → ErrSignInvalid.
//  3. Unmarshal the envelope, AES-256-GCM-decrypt resource.ciphertext with the
//     APIv3 key and parse the plaintext transaction.
//  4. Map trade_state→NotifyResult.Status (SUCCESS→success, else failed).
//
// The method performs no DB I/O; HandleNotify does, only after a nil error.
func (c *V3Client) ParseNotify(ctx context.Context, headers map[string]string, body []byte) (NotifyResult, error) {
	// 1. resolve platform public key for the serial the notify claims.
	serial := headerGet(headers, v3HeaderSerial)
	var pub *rsa.PublicKey
	if serial != "" {
		p, err := c.certCache.get(ctx, c, serial)
		if err != nil {
			// Cannot fetch the matching cert → cannot verify → fail closed as a
			// signature error so HandleNotify leaves the ledger untouched.
			return NotifyResult{}, ErrSignInvalid
		}
		pub = p
	}

	// 2. verify signature + freshness. Any failure is already ErrSignInvalid.
	if err := VerifyV3Notify(headers, body, pub); err != nil {
		return NotifyResult{}, err
	}

	// 3. decrypt the resource.
	var env v3NotifyEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return NotifyResult{}, fmt.Errorf("payment: v3 notify parse envelope: %w", err)
	}
	ciphertext, err := base64.StdEncoding.DecodeString(env.Resource.Ciphertext)
	if err != nil {
		return NotifyResult{}, fmt.Errorf("payment: v3 notify resource ciphertext base64: %w", err)
	}
	plain, err := aesGCMDecrypt([]byte(c.cfg.APiv3Key), env.Resource.Nonce, env.Resource.AssociatedData, ciphertext)
	if err != nil {
		return NotifyResult{}, fmt.Errorf("payment: v3 notify resource decrypt: %w", err)
	}

	// 4. normalise transaction.
	var tx v3Transaction
	if err := json.Unmarshal(plain, &tx); err != nil {
		return NotifyResult{}, fmt.Errorf("payment: v3 notify plaintext parse: %w", err)
	}
	res := NotifyResult{
		OutTradeNo:   tx.OutTradeNo,
		ProviderTxID: tx.TransactionID,
		AmountCents:  tx.Amount.PayerTotal,
		Status:       PaymentStatusFailed,
		RawPayload:   body,
	}
	// payer_total (实付) is authoritative; fall back to total when upstream omits it
	// (e.g. failed / non-paid callbacks).
	if res.AmountCents == 0 {
		res.AmountCents = tx.Amount.Total
	}
	if tx.TradeState == "SUCCESS" {
		res.Status = PaymentStatusSuccess
	}
	if tx.SuccessTime != "" {
		if t, err := time.Parse(time.RFC3339, tx.SuccessTime); err == nil {
			res.PaidAt = t
		}
	}
	return res, nil
}

// v3NotifyEnvelope models the outer notify body WeChat POSTs to notify_url.
type v3NotifyEnvelope struct {
	ID           string `json:"id"`
	EventType    string `json:"event_type"`
	ResourceType string `json:"resource_type"`
	EventID      string `json:"event_id"`
	Resource     struct {
		Algorithm      string `json:"algorithm"`
		Ciphertext     string `json:"ciphertext"`
		AssociatedData string `json:"associated_data"`
		Nonce          string `json:"nonce"`
		OriginalType   string `json:"original_type"`
	} `json:"resource"`
}

// v3Transaction models the decrypted notify plaintext (a payment transaction).
// amount.payer_total is the 实付 amount (after coupons); amount.total is the
// order total. We prefer payer_total for the ledger.
type v3Transaction struct {
	TransactionID string `json:"transaction_id"`
	OutTradeNo    string `json:"out_trade_no"`
	TradeType     string `json:"trade_type"`
	TradeState    string `json:"trade_state"`
	SuccessTime   string `json:"success_time"`
	Amount        struct {
		PayerTotal int64 `json:"payer_total"`
		Total      int64 `json:"total"`
	} `json:"amount"`
}
