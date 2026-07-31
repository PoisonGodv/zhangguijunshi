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
// It records GetAccessToken and Invalidate call counts so tests can assert the
// token-invalid retry path (invalidate → refetch → resend).
type fakeTokenSource struct {
	token           string
	err             error
	calls           int
	invalidateCalls int
	// secondToken, when non-empty, is returned by GetAccessToken after the first
	// Invalidate (simulating a fresh token fetched post-rotation).
	secondToken string
}

func (f *fakeTokenSource) GetAccessToken(_ context.Context) (string, error) {
	f.calls++
	if f.invalidateCalls > 0 && f.secondToken != "" {
		return f.secondToken, f.err
	}
	return f.token, f.err
}

func (f *fakeTokenSource) Invalidate(_ context.Context) {
	f.invalidateCalls++
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

// newFakeWxSeq returns a fake WeChat server that replies with resp[i] on the i-th
// send call (clamped to the last entry), recording each access_token seen.
func newFakeWxSeq(t *testing.T, resps []string, tokens *[]string) *httptest.Server {
	t.Helper()
	var n int
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tokens != nil {
			*tokens = append(*tokens, r.URL.Query().Get("access_token"))
		}
		idx := n
		if idx >= len(resps) {
			idx = len(resps) - 1
		}
		n++
		_, _ = w.Write([]byte(resps[idx]))
	}))
}

// TestWxSender_TokenInvalid_InvalidateAndRetryOnce proves the token-invalid
// recovery path: the first send returns errcode 42001 (access_token expired), the
// sender invalidates the cached token, refetches (a fresh token), and retries the
// send exactly once — which succeeds. Without this, a token WeChat invalidated
// early would fail every send for up to the ~115min cache lifetime.
func TestWxSender_TokenInvalid_InvalidateAndRetryOnce(t *testing.T) {
	var seenTokens []string
	srv := newFakeWxSeq(t, []string{
		`{"errcode":42001,"errmsg":"access_token expired"}`, // first send: stale token
		`{"errcode":0,"errmsg":"ok"}`,                       // retry: fresh token
	}, &seenTokens)
	defer srv.Close()

	tokens := &fakeTokenSource{token: "STALE", secondToken: "FRESH"}
	sender := NewWxSender(wxsdk.New(wxsdk.WithBaseURL(srv.URL)), tokens, &fakeMemberResolver{openID: "oABC"}, map[string]string{TplRecall30: "wx-tpl-30"})

	err := sender.Send(context.Background(), &SendPayload{
		TenantID: 1, MemberID: 42, Channel: ChannelSubscribeMsg, TemplateCode: TplRecall30,
		Title: "标题", Body: "正文",
	})
	if err != nil {
		t.Fatalf("Send should succeed after token refresh, got %v", err)
	}
	if tokens.invalidateCalls != 1 {
		t.Fatalf("expected exactly 1 Invalidate, got %d", tokens.invalidateCalls)
	}
	if tokens.calls != 2 {
		t.Fatalf("expected 2 token fetches (initial + post-invalidate), got %d", tokens.calls)
	}
	// The retry used the fresh token, not the stale one.
	if len(seenTokens) != 2 || seenTokens[0] != "STALE" || seenTokens[1] != "FRESH" {
		t.Fatalf("expected [STALE FRESH] send tokens, got %v", seenTokens)
	}
}

// TestWxSender_TokenInvalid_RetryStillFails proves the retry is bounded to ONE
// attempt: when the second send also returns a token-invalid errcode, Send gives
// up and returns the wrapped error (no infinite loop, no third send).
func TestWxSender_TokenInvalid_RetryStillFails(t *testing.T) {
	var seenTokens []string
	srv := newFakeWxSeq(t, []string{
		`{"errcode":42001,"errmsg":"access_token expired"}`,
		`{"errcode":42001,"errmsg":"access_token expired"}`,
	}, &seenTokens)
	defer srv.Close()

	tokens := &fakeTokenSource{token: "STALE", secondToken: "FRESH"}
	sender := NewWxSender(wxsdk.New(wxsdk.WithBaseURL(srv.URL)), tokens, &fakeMemberResolver{openID: "oABC"}, map[string]string{TplRecall30: "wx-tpl-30"})

	err := sender.Send(context.Background(), &SendPayload{
		TenantID: 1, MemberID: 42, Channel: ChannelSubscribeMsg, TemplateCode: TplRecall30,
	})
	if err == nil {
		t.Fatal("expected error when retry also fails")
	}
	var apiErr *wxsdk.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrCode != 42001 {
		t.Fatalf("expected wrapped 42001 APIError, got %v", err)
	}
	if tokens.invalidateCalls != 1 {
		t.Fatalf("retry must be bounded to one invalidate, got %d", tokens.invalidateCalls)
	}
	if len(seenTokens) != 2 {
		t.Fatalf("expected exactly 2 send attempts (no third), got %d", len(seenTokens))
	}
}

// TestWxSender_NonTokenErr_NoRetry proves a non-token send error (e.g. 43101 user
// refused) is returned immediately WITHOUT invalidate/retry — the recovery path is
// reserved for token-invalid errcodes only.
func TestWxSender_NonTokenErr_NoRetry(t *testing.T) {
	var seenTokens []string
	srv := newFakeWxSeq(t, []string{`{"errcode":43101,"errmsg":"user refuse"}`}, &seenTokens)
	defer srv.Close()

	tokens := &fakeTokenSource{token: "TOK"}
	sender := NewWxSender(wxsdk.New(wxsdk.WithBaseURL(srv.URL)), tokens, &fakeMemberResolver{openID: "oABC"}, map[string]string{TplRecall30: "wx-tpl-30"})

	err := sender.Send(context.Background(), &SendPayload{
		TenantID: 1, MemberID: 42, Channel: ChannelSubscribeMsg, TemplateCode: TplRecall30,
	})
	if err == nil {
		t.Fatal("expected error on user-refuse")
	}
	if tokens.invalidateCalls != 0 {
		t.Fatalf("non-token error must not invalidate, got %d", tokens.invalidateCalls)
	}
	if len(seenTokens) != 1 {
		t.Fatalf("non-token error must not retry, got %d sends", len(seenTokens))
	}
}
