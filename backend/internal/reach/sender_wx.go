package reach

// sender_wx.go is the REAL WeChat Mini Program subscribe-message (订阅消息)
// Sender — the first production channel adapter behind the reach.Sender port
// (tech plan §4.3, CONTRACT §11.3.2). It delivers the rendered title/body of a
// subscribe_msg reach attempt to a member's openid via the WeChat
// /cgi-bin/message/subscribe/send API.
//
// Anti-corruption: WxSender depends only on TWO narrow ports defined in this
// package — TokenSource (access_token supply) and MemberResolver (openid
// lookup) — plus a *wxsdk.Client for the actual HTTP send. It imports no domain
// package; bootstrap adapts membership.Service onto MemberResolver and a
// wxsdk.TokenCache onto TokenSource (see internal/bootstrap/adapters.go). The
// other three channels (wework_group / wework_1v1 / sms) are NOT implemented
// yet: Send returns an explicit "not implemented" error for them so the task
// lands as status=failed rather than panicking or silently dropping.
//
// DATA-FIELD CONVENTION (IMPORTANT, must be reconciled with the WeChat console
// template): the rendered Title is mapped to template field "thing1" and the
// rendered Body to "thing2". This is a CONVENTION, not a WeChat mandate — each
// subscribe-message template configured in the WeChat MP console declares its
// own field keys (thing1/thing2/amount3/date4/…). Whoever registers the
// templates must ensure the template bound to each TemplateCode exposes at
// least thing1 (title) and thing2 (body); adjust buildData / the template
// registration if a template uses different keys.

import (
	"context"
	"fmt"

	"github.com/zhangguijunshi/backend/pkg/wxsdk"
)

// Title / body subscribe-message field keys (see package data-field convention).
const (
	wxDataTitleField = "thing1"
	wxDataBodyField  = "thing2"
)

// TokenSource supplies a WeChat global access_token. Satisfied by a
// wxsdk.TokenCache adapter wired at the composition root (the adapter closes
// over the tenant's AppID/Secret so reach never sees credentials). An error
// means no token could be obtained — Send surfaces it and the task fails.
type TokenSource interface {
	GetAccessToken(ctx context.Context) (string, error)
}

// MemberResolver resolves a member's WeChat openid for delivery. Satisfied by a
// membership.Service adapter wired at the composition root (tenant is passed
// explicitly so the adapter can build a tenant-scoped context). An empty openid
// (with nil error) means the member has no WeChat identity — Send treats that
// as a failure, not a silent skip.
type MemberResolver interface {
	ResolveOpenID(ctx context.Context, tenantID, memberID int64) (string, error)
}

// WxSender delivers subscribe_msg reach attempts over the real WeChat API. It
// implements Sender. Zero-value fields are invalid; construct with NewWxSender.
type WxSender struct {
	client    *wxsdk.Client     // subscribe-message HTTP sender
	tokens    TokenSource       // access_token supply (adapter over wxsdk.TokenCache)
	members   MemberResolver    // openid lookup (adapter over membership.Service)
	templates map[string]string // our TemplateCode -> WeChat template_id
	page      string            // default mini-program page opened on tap (optional)
}

// WxSenderOption customises a WxSender.
type WxSenderOption func(*WxSender)

// WithWxPage sets the default mini-program page a tapped notification opens.
// Empty (the default) sends no page, which WeChat treats as "open the home
// page".
func WithWxPage(page string) WxSenderOption {
	return func(s *WxSender) { s.page = page }
}

// NewWxSender constructs a WxSender. client / tokens / members are required;
// templates maps our reach TemplateCode (e.g. "recall_30") to the WeChat-side
// template_id registered in the MP console. A nil templates map is allowed but
// makes every send fail with a missing-mapping error (useful only in tests).
func NewWxSender(client *wxsdk.Client, tokens TokenSource, members MemberResolver, templates map[string]string, opts ...WxSenderOption) *WxSender {
	s := &WxSender{
		client:    client,
		tokens:    tokens,
		members:   members,
		templates: templates,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// Send delivers p over the WeChat subscribe-message API. Only
// ChannelSubscribeMsg is implemented; every other channel returns an explicit
// "not implemented" error (so the ReachTask lands as status=failed rather than
// panicking or being silently dropped).
//
// Pipeline: channel gate → resolve openid (empty ⇒ error) → map TemplateCode to
// a WeChat template_id (missing ⇒ error) → build the data map (title→thing1,
// body→thing2) → fetch access_token → POST the message. Every failure is
// wrapped with %w so callers can errors.As into *wxsdk.APIError for the
// WeChat-side errcode.
func (s *WxSender) Send(ctx context.Context, p *SendPayload) error {
	if p == nil {
		return fmt.Errorf("reach: wxsender: nil payload")
	}
	if p.Channel != ChannelSubscribeMsg {
		return fmt.Errorf("reach: wxsender: channel %q not implemented", p.Channel)
	}

	openID, err := s.members.ResolveOpenID(ctx, p.TenantID, p.MemberID)
	if err != nil {
		return fmt.Errorf("reach: wxsender: resolve openid (tenant=%d member=%d): %w", p.TenantID, p.MemberID, err)
	}
	if openID == "" {
		return fmt.Errorf("reach: wxsender: member %d has no wechat openid", p.MemberID)
	}

	templateID, ok := s.templates[p.TemplateCode]
	if !ok || templateID == "" {
		return fmt.Errorf("reach: wxsender: no wechat template_id mapped for code %q", p.TemplateCode)
	}

	token, err := s.tokens.GetAccessToken(ctx)
	if err != nil {
		return fmt.Errorf("reach: wxsender: get access_token: %w", err)
	}

	if err := s.client.SendSubscribeMessage(ctx, token, wxsdk.SubscribeMessageInput{
		OpenID:     openID,
		TemplateID: templateID,
		Page:       s.page,
		Data:       buildSubscribeData(p.Title, p.Body),
	}); err != nil {
		return fmt.Errorf("reach: wxsender: send subscribe message: %w", err)
	}
	return nil
}

// buildSubscribeData maps the rendered title/body onto the WeChat template
// fields per the package data-field convention (title→thing1, body→thing2). See
// the package doc for the reconciliation caveat with console-registered
// templates.
func buildSubscribeData(title, body string) map[string]wxsdk.DataItem {
	return map[string]wxsdk.DataItem{
		wxDataTitleField: {Value: title},
		wxDataBodyField:  {Value: body},
	}
}

var _ Sender = (*WxSender)(nil)
