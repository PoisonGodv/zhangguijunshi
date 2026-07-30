package payment

// v3client.go implements the three WeChat Pay V3 request methods on *V3Client
// (CONTRACT §15.5.8): Apply (进件), Query (查询进件), Prepay (下单 +
// JSAPI 调起参数). All three route through the do() outbound helper which
// signs every request with SignV3Authorization (sign.go) and rejects non-2xx
// upstream responses.
//
// Path constants follow the V3 service-provider (服务商 / ecommerce) contract:
//   - POST /v3/ecommerce/applyments/        — 二级商户进件
//   - GET  /v3/ecommerce/applyments/{id}    — 查询进件状态
//   - POST /v3/pay/transactions/jsapi       — JSAPI 下单 (服务商特约)
//
// No third-party SDK is used; requests/responses are stdlib JSON.

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// V3 service-provider path constants (signed verbatim by SignV3Authorization —
// the signature covers the path, not the full URL).
const (
	v3PathApplyments = "/v3/ecommerce/applyments/"
	v3PathPayJSAPI   = "/v3/pay/transactions/jsapi"
	v3PathCerts      = "/v3/certificates"
)

// Apply submits a sub-merchant onboarding application to the V3 ecommerce
// applyment endpoint and returns the upstream applyment_id (微信申请单号). The
// service stores it on the SubMerchant row for later QuerySubMerchant polls.
//
// NOTE: production V3 requires RSA-OAEP-encrypting sensitive contact_info fields
// with the platform certificate; this structural client sends contact_info in
// the clear and is correct for the contract-level / FakeWxPay-parity path. A
// production deployment must layer field-level encryption before going live.
func (c *V3Client) Apply(ctx context.Context, in ApplySubMerchantInput) (string, error) {
	body := buildApplyBody(in)
	data, err := c.do(ctx, http.MethodPost, v3PathApplyments, body)
	if err != nil {
		return "", err
	}
	var resp struct {
		ApplymentID string `json:"applyment_id"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("payment: v3 apply parse response: %w", err)
	}
	if resp.ApplymentID == "" {
		return "", fmt.Errorf("payment: v3 apply returned empty applyment_id")
	}
	return resp.ApplymentID, nil
}

// Query polls an applyment by id and maps the V3 applyment_state onto the
// SubMerchantStatus state machine (pending/reviewing/active/rejected/frozen).
func (c *V3Client) Query(ctx context.Context, _ int64, applymentID string) (SubMerchantStatus, string, error) {
	if applymentID == "" {
		return "", "", errors.New("payment: v3 query requires applyment_id")
	}
	path := v3PathApplyments + applymentID
	data, err := c.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", "", err
	}
	var resp struct {
		ApplymentState string `json:"applyment_state"`
		SubMchID       string `json:"sub_mch_id"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", "", fmt.Errorf("payment: v3 query parse response: %w", err)
	}
	return mapApplyState(resp.ApplymentState), resp.SubMchID, nil
}

// Prepay calls the JSAPI unified-order endpoint for a service-provider 特约
// sub-merchant, then assembles the JSAPI lift-off payload (appId / timeStamp /
// nonceStr / package / signType / paySign) the mini-program hands to
// wx.requestPayment. paySign is a fresh SHA256withRSA signature over
// appId\ntimeStamp\nnonceStr\npackage\n with the merchant private key.
func (c *V3Client) Prepay(ctx context.Context, in PrepayInput) (PrepayResult, error) {
	body := v3PrepayBody{
		AppID:       c.AppID,
		SubMchID:    strconv.FormatInt(in.SubMerchantID, 10),
		OutTradeNo:  in.OutTradeNo,
		Description: in.Description,
		NotifyURL:   c.cfg.NotifyURL,
		Amount: v3Amount{
			Total:    in.AmountCents,
			Currency: "CNY",
		},
	}
	if in.OpenID != "" {
		body.Payer = &v3Payer{OpenID: in.OpenID}
	}
	data, err := c.do(ctx, http.MethodPost, v3PathPayJSAPI, body)
	if err != nil {
		return PrepayResult{}, err
	}
	var resp struct {
		PrepayID string `json:"prepay_id"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return PrepayResult{}, fmt.Errorf("payment: v3 prepay parse response: %w", err)
	}
	if resp.PrepayID == "" {
		return PrepayResult{}, fmt.Errorf("payment: v3 prepay returned empty prepay_id")
	}
	timeStamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonceStr := randomHex(16)
	pkg := "prepay_id=" + resp.PrepayID
	paySign, err := c.signJSAPILift(c.AppID, timeStamp, nonceStr, pkg)
	if err != nil {
		return PrepayResult{}, err
	}
	return PrepayResult{
		PrepayID:  resp.PrepayID,
		TimeStamp: timeStamp,
		NonceStr:  nonceStr,
		Package:   pkg,
		PaySign:   paySign,
		SignType:  "RSA",
	}, nil
}

// do is the single outbound V3 helper: marshal bodyObj (nil → empty GET body),
// sign method\npath\nts\nnonce\nbody\n with SignV3Authorization, issue the
// request through the injected *http.Client and return the response body bytes
// on 2xx. Non-2xx is parsed into a V3 error (code + message).
func (c *V3Client) do(ctx context.Context, method, path string, bodyObj any) ([]byte, error) {
	var (
		bodyStr    string
		bodyReader io.Reader
	)
	if bodyObj != nil {
		b, err := json.Marshal(bodyObj)
		if err != nil {
			return nil, fmt.Errorf("payment: v3 marshal body: %w", err)
		}
		bodyStr = string(b)
		bodyReader = bytes.NewReader(b)
	}
	nonce := randomHex(16)
	ts := time.Now().Unix()
	auth, err := SignV3Authorization(method, path, bodyStr, nonce, ts, c.cfg.MchID, c.cfg.CertSerial, c.priv)
	if err != nil {
		return nil, fmt.Errorf("payment: v3 sign request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("payment: v3 build request: %w", err)
	}
	req.Header.Set("Authorization", auth)
	req.Header.Set("Accept", "application/json")
	if bodyObj != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("payment: v3 http: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("payment: v3 read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, parseV3Error(data, resp.StatusCode)
	}
	return data, nil
}

// parseV3Error turns a non-2xx V3 error body into a descriptive error. WeChat
// returns {"code":"SIGN_ERROR","message":"..."}; if we cannot parse it we fall
// back to the bare status code.
func parseV3Error(data []byte, status int) error {
	var ve struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(data, &ve); err == nil && ve.Code != "" {
		return fmt.Errorf("payment: v3 error %d %s: %s", status, ve.Code, ve.Message)
	}
	return fmt.Errorf("payment: v3 http status %d", status)
}

// signJSAPILift signs the JSAPI lift-off message
// appId\ntimeStamp\nnonceStr\npackage\n with SHA256withRSA + base64 → paySign.
// This is a distinct signing from SignV3Authorization (different message
// format), but reuses the same merchant private key.
func (c *V3Client) signJSAPILift(appID, timeStamp, nonceStr, pkg string) (string, error) {
	if c.priv == nil {
		return "", errors.New("payment: jsapi sign requires private key")
	}
	msg := appID + "\n" + timeStamp + "\n" + nonceStr + "\n" + pkg + "\n"
	digest := sha256.Sum256([]byte(msg))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.priv, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("payment: jsapi sign: %w", err)
	}
	return base64.StdEncoding.EncodeToString(sig), nil
}

// randomHex returns nBytes of crypto-strong randomness as a hex string (2*nBytes
// chars). Used for nonce_str / Wechatpay-Nonce.
func randomHex(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand should not fail; fall back to time-seeded hex so the request
		// still goes out with a (weak) unique nonce rather than panicking.
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b)
}

// mapApplyState maps a V3 applyment_state onto the SubMerchantStatus state
// machine. Unknown states default to pending (the safest non-terminal).
func mapApplyState(state string) SubMerchantStatus {
	switch state {
	case "FINISH":
		return SubMerchantActive
	case "CHECKING", "NEED_ACTION", "NEED_SIGN":
		return SubMerchantReviewing
	case "REJECTED":
		return SubMerchantRejected
	case "FROZEN":
		return SubMerchantFrozen
	default:
		return SubMerchantPending
	}
}

// buildApplyBody assembles the V3 ecommerce applyment request body from
// ApplySubMerchantInput. out_request_no is made unique per (tenant, store,
// nanosecond); business_code defaults to the supplied license_no. Fields not
// derivable from ApplyInput are omitted (omitempty) — production materials
// require more (bank accounts, settlement, encrypted id cards) and are layered
// by the wiring layer / a future enrichment before going live.
func buildApplyBody(in ApplySubMerchantInput) v3ApplyBody {
	return v3ApplyBody{
		OutRequestNo: fmt.Sprintf("zg_apply_%d_%d_%d", in.TenantID, in.StoreID, time.Now().UnixNano()),
		BusinessCode: in.LicenseNo,
		ContactInfo: v3ApplyContactInfo{
			ContactName:  in.ContactName,
			ContactPhone: in.ContactPhone,
		},
		BusinessInfo: v3ApplyBusinessInfo{
			MerchantShortname: in.BusinessName,
			LicenseType:       in.LicenseType,
		},
	}
}

// v3ApplyBody models the request body for POST /v3/ecommerce/applyments/.
type v3ApplyBody struct {
	OutRequestNo string              `json:"out_request_no"`
	BusinessCode string              `json:"business_code,omitempty"`
	ContactInfo  v3ApplyContactInfo  `json:"contact_info"`
	BusinessInfo v3ApplyBusinessInfo `json:"business_info"`
}

type v3ApplyContactInfo struct {
	ContactName  string `json:"contact_name,omitempty"`
	ContactPhone string `json:"contact_phone,omitempty"`
}

type v3ApplyBusinessInfo struct {
	MerchantShortname string `json:"merchant_shortname,omitempty"`
	LicenseType       string `json:"business_license_info_type,omitempty"`
}

// v3PrepayBody models the request body for POST /v3/pay/transactions/jsapi
// (service-provider 特约商户 path: sub_mchid + sp_appid).
type v3PrepayBody struct {
	AppID       string   `json:"appid"`
	SubMchID    string   `json:"sub_mchid"`
	OutTradeNo  string   `json:"out_trade_no"`
	Description string   `json:"description,omitempty"`
	NotifyURL   string   `json:"notify_url"`
	Amount      v3Amount `json:"amount"`
	Payer       *v3Payer `json:"payer,omitempty"`
}

type v3Amount struct {
	Total    int64  `json:"total"`
	Currency string `json:"currency,omitempty"`
}

type v3Payer struct {
	OpenID string `json:"openid"`
}
