package reach

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/zhangguijunshi/backend/pkg/wxsdk"
)

// fakeTokenSource is a stub reach.TokenSource returning a fixed token / error.
type fakeTokenSource struct {
	token string
	err   error
	calls int
}

func (f *fakeTokenSource) GetAccessToken(_ context.Context) (string, error) {
	f.calls++
	return f.token, f.err
}

// fakeMemberResolver is a stub reach.MemberResolver returning a fixed openid.
type fakeMemberResolver struct {
	openID string
	err    error
}

func (f *fakeMemberResolver) ResolveOpenID(_ context.Context, _, _ int64) (string, error) {
	return f.openID, f.err
}

// wxSendPayload captures what the fake WeChat server received.
type wxSendPayload struct {
	accessToken string
	body        string
}

func newFakeWx(t *testing.T, resp string, got *wxSendPayload) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got != nil {
			got.accessToken = r.URL.Query().Get("access_token")
			raw, _ := io.ReadAll(r.Body)
			got.body = string(raw)
		}
		_, _ = w.Write([]byte(resp))
	}))
}

func TestWxSender_SubscribeSuccess(t *testing.T) {
	var got wxSendPayload
	srv := newFakeWx(t, `{"errcode":0,"errmsg":"ok"}`, &got)
	defer srv.Close()

	tokens := &fakeTokenSource{token: "TOK"}
	sender := NewWxSender(
		wxsdk.New(wxsdk.WithBaseURL(srv.URL)),
		tokens,
		&fakeMemberResolver{openID: "oABC"},
		map[string]string{TplRecall30: "wx-tpl-30"},
		WithWxPage("pages/index"),
	)

	err := sender.Send(context.Background(), &SendPayload{
		TenantID:     1,
		MemberID:     42,
		Channel:      ChannelSubscribeMsg,
		TemplateCode: TplRecall30,
		Title:        "好久不见",
		Body:         "老顾客回馈券已到账",
	})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got.accessToken != "TOK" {
		t.Fatalf("access_token not forwarded: %q", got.accessToken)
	}
	if !strings.Contains(got.body, `"touser":"oABC"`) ||
		!strings.Contains(got.body, `"template_id":"wx-tpl-30"`) ||
		!strings.Contains(got.body, `"thing1"`) || !strings.Contains(got.body, "好久不见") ||
		!strings.Contains(got.body, `"thing2"`) || !strings.Contains(got.body, "老顾客回馈券已到账") {
		t.Fatalf("unexpected send body: %s", got.body)
	}
	if tokens.calls != 1 {
		t.Fatalf("expected 1 token fetch, got %d", tokens.calls)
	}
}

func TestWxSender_NonSubscribeChannelNotImplemented(t *testing.T) {
	sender := NewWxSender(wxsdk.New(), &fakeTokenSource{token: "TOK"}, &fakeMemberResolver{openID: "o"}, map[string]string{TplRecall30: "x"})
	for _, ch := range []string{ChannelWeworkGroup, ChannelWeworkOneV, ChannelSMS} {
		err := sender.Send(context.Background(), &SendPayload{Channel: ch, TemplateCode: TplRecall30})
		if err == nil || !strings.Contains(err.Error(), "not implemented") {
			t.Fatalf("channel %q: expected not-implemented error, got %v", ch, err)
		}
	}
}

func TestWxSender_EmptyOpenID(t *testing.T) {
	sender := NewWxSender(wxsdk.New(), &fakeTokenSource{token: "TOK"}, &fakeMemberResolver{openID: ""}, map[string]string{TplRecall30: "x"})
	err := sender.Send(context.Background(), &SendPayload{Channel: ChannelSubscribeMsg, TemplateCode: TplRecall30})
	if err == nil || !strings.Contains(err.Error(), "no wechat openid") {
		t.Fatalf("expected empty-openid error, got %v", err)
	}
}

func TestWxSender_ResolverError(t *testing.T) {
	sender := NewWxSender(wxsdk.New(), &fakeTokenSource{token: "TOK"}, &fakeMemberResolver{err: ErrNotFound}, map[string]string{TplRecall30: "x"})
	err := sender.Send(context.Background(), &SendPayload{Channel: ChannelSubscribeMsg, TemplateCode: TplRecall30})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected wrapped ErrNotFound, got %v", err)
	}
}

func TestWxSender_MissingTemplateMapping(t *testing.T) {
	sender := NewWxSender(wxsdk.New(), &fakeTokenSource{token: "TOK"}, &fakeMemberResolver{openID: "o"}, map[string]string{})
	err := sender.Send(context.Background(), &SendPayload{Channel: ChannelSubscribeMsg, TemplateCode: TplBirthday})
	if err == nil || !strings.Contains(err.Error(), "no wechat template_id mapped") {
		t.Fatalf("expected missing-mapping error, got %v", err)
	}
}

func TestWxSender_TokenError(t *testing.T) {
	sender := NewWxSender(wxsdk.New(), &fakeTokenSource{err: errors.New("boom")}, &fakeMemberResolver{openID: "o"}, map[string]string{TplRecall30: "x"})
	err := sender.Send(context.Background(), &SendPayload{Channel: ChannelSubscribeMsg, TemplateCode: TplRecall30})
	if err == nil || !strings.Contains(err.Error(), "get access_token") {
		t.Fatalf("expected token error, got %v", err)
	}
}

func TestWxSender_WxErrCodeFails(t *testing.T) {
	srv := newFakeWx(t, `{"errcode":43101,"errmsg":"user refuse"}`, nil)
	defer srv.Close()

	sender := NewWxSender(wxsdk.New(wxsdk.WithBaseURL(srv.URL)), &fakeTokenSource{token: "TOK"}, &fakeMemberResolver{openID: "o"}, map[string]string{TplRecall30: "x"})
	err := sender.Send(context.Background(), &SendPayload{Channel: ChannelSubscribeMsg, TemplateCode: TplRecall30})
	var apiErr *wxsdk.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected wrapped *wxsdk.APIError, got %T %v", err, err)
	}
	if apiErr.ErrCode != 43101 {
		t.Fatalf("errcode mismatch: %+v", apiErr)
	}
}
