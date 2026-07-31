package wxsdk

// subscribe.go adds the Mini Program subscribe-message (订阅消息) send call to
// the minimal WeChat client (tech plan §4.3 reach delivery). It POSTs to
// /cgi-bin/message/subscribe/send?access_token=… with a JSON body of
// {touser, template_id, page, data}. The access_token is obtained separately
// via TokenCache.GetAccessToken and passed in per call, so this function stays
// stateless and the same client serves every tenant.
//
// The style mirrors client.go: net/http directly, injectable baseURL /
// *http.Client so tests run against an httptest fake.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// subscribeSendPath is the subscribe-message send endpoint.
const subscribeSendPath = "/cgi-bin/message/subscribe/send"

// DataItem is one value in a subscribe-message data map. WeChat requires each
// field to be wrapped as {"value": "..."}, hence the struct rather than a bare
// string.
type DataItem struct {
	Value string `json:"value"`
}

// SubscribeMessageInput is one subscribe-message delivery. Data keys must match
// the template's field names as configured in the WeChat console (e.g. thing1,
// thing2, amount3 …); the values are the rendered strings.
type SubscribeMessageInput struct {
	OpenID     string              // touser: the recipient's openid
	TemplateID string              // template_id: the WeChat-side template id
	Page       string              // page: optional mini-program page to open on tap
	Data       map[string]DataItem // data: template field -> value
}

// subscribeSendRequest mirrors the JSON body WeChat expects.
type subscribeSendRequest struct {
	ToUser     string              `json:"touser"`
	TemplateID string              `json:"template_id"`
	Page       string              `json:"page,omitempty"`
	Data       map[string]DataItem `json:"data"`
}

// subscribeSendResponse mirrors the raw WeChat JSON. On success errcode is 0
// (WeChat returns {"errcode":0,"errmsg":"ok"} for this endpoint).
type subscribeSendResponse struct {
	ErrCode int    `json:"errcode"`
	ErrMsg  string `json:"errmsg"`
}

// SendSubscribeMessage delivers one subscribe message using accessToken for
// auth. A non-zero errcode is returned as *APIError (Op="send_subscribe_message");
// callers can branch on APIError.IsTokenInvalid to invalidate the cached token
// and retry once.
//
// See https://developers.weixin.qq.com/miniprogram/dev/OpenApiDoc/subscribe-message/subscribeMessage-send.html
func (c *Client) SendSubscribeMessage(ctx context.Context, accessToken string, in SubscribeMessageInput) error {
	if accessToken == "" {
		return fmt.Errorf("wxsdk: access_token is required")
	}
	if in.OpenID == "" || in.TemplateID == "" {
		return fmt.Errorf("wxsdk: subscribe message requires openid and template_id")
	}

	payload := subscribeSendRequest{
		ToUser:     in.OpenID,
		TemplateID: in.TemplateID,
		Page:       in.Page,
		Data:       in.Data,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("wxsdk: encode subscribe message: %w", err)
	}

	endpoint := c.baseURL + subscribeSendPath + "?access_token=" + accessToken
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("wxsdk: build subscribe request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("wxsdk: call send_subscribe_message: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16)) // 64 KiB cap; response is tiny
	if err != nil {
		return fmt.Errorf("wxsdk: read subscribe response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("wxsdk: send_subscribe_message HTTP %d: %s", resp.StatusCode, string(body))
	}

	var parsed subscribeSendResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return fmt.Errorf("wxsdk: decode subscribe response: %w (body=%q)", err, string(body))
	}
	if parsed.ErrCode != 0 {
		return &APIError{ErrCode: parsed.ErrCode, ErrMsg: parsed.ErrMsg, Op: "send_subscribe_message"}
	}
	return nil
}
