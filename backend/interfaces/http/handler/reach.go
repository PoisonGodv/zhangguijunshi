package handler

// ReachHandler exposes the boss-surface reach endpoints (M2-S2, CONTRACT §11.3.2
// BE-B-07/08/10): the single outbound send channel plus subscribe-message quota,
// unsubscribe opt-out, and task audit listing. Tenant/store come from the
// verified request context, never the request body.
//
// A blocked send is NOT an error: reach.Service.Send returns the ReachTask with
// status=blocked + blocked_reason, and this handler passes it through as a 200
// OK data payload so the client can branch on task.status (intercepted vs sent
// vs system-failed). Only hard errors (unknown template, param, no tenant) map
// to errcode failures.

import (
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/internal/reach"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
)

// ReachHandler serves reach endpoints.
type ReachHandler struct {
	svc *reach.Service
}

// NewReachHandler constructs a ReachHandler.
func NewReachHandler(svc *reach.Service) *ReachHandler {
	return &ReachHandler{svc: svc}
}

type sendRequest struct {
	StoreID      int64             `json:"store_id"`
	MemberID     int64             `json:"member_id" binding:"required,min=1"`
	StrategyID   int64             `json:"strategy_id"`
	Channel      string            `json:"channel" binding:"required"`
	TemplateCode string            `json:"template_code" binding:"required"`
	Vars         map[string]string `json:"vars"`
}

// Send runs the full reach pipeline for one member.
// POST /v1/boss/reach/send
func (h *ReachHandler) Send(c *gin.Context) {
	var req sendRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	task, err := h.svc.Send(c.Request.Context(), reach.SendInput{
		StoreID:      storeScope(c, req.StoreID),
		MemberID:     req.MemberID,
		StrategyID:   req.StrategyID,
		Channel:      req.Channel,
		TemplateCode: req.TemplateCode,
		Vars:         req.Vars,
	})
	if err != nil {
		h.failReach(c, err)
		return
	}
	httpx.OK(c, task)
}

// ListTasks lists reach tasks for the ctx tenant.
// GET /v1/boss/reach/tasks?store_id=&status=
func (h *ReachHandler) ListTasks(c *gin.Context) {
	tasks, err := h.svc.ListTasks(c.Request.Context(), queryInt64(c, "store_id"), c.Query("status"))
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, tasks)
}

// GetTask loads one reach task.
// GET /v1/boss/reach/tasks/:id
func (h *ReachHandler) GetTask(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	task, err := h.svc.GetTask(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, reach.ErrNotFound) {
			httpx.Fail(c, errcode.Err{Code: 40404, Message: "触达任务不存在", HTTP: 404})
			return
		}
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, task)
}

type grantSubscribeRequest struct {
	MemberID   int64  `json:"member_id" binding:"required,min=1"`
	TemplateID string `json:"template_id" binding:"required"`
	Quota      int    `json:"quota" binding:"required,min=1"`
}

// GrantSubscribe idempotently accumulates subscribe-message quota for a member.
// POST /v1/boss/reach/subscribe-auth
func (h *ReachHandler) GrantSubscribe(c *gin.Context) {
	var req grantSubscribeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	auth, err := h.svc.GrantSubscribe(c.Request.Context(), req.MemberID, req.TemplateID, req.Quota)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, auth)
}

type unsubscribeRequest struct {
	MemberID int64  `json:"member_id" binding:"required,min=1"`
	Channel  string `json:"channel" binding:"required"`
	Reason   string `json:"reason"`
}

// Unsubscribe records a member's opt-out from a channel.
// POST /v1/boss/reach/unsubscribe
func (h *ReachHandler) Unsubscribe(c *gin.Context) {
	var req unsubscribeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	rec, err := h.svc.Unsubscribe(c.Request.Context(), req.MemberID, req.Channel, req.Reason)
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, rec)
}

type resubscribeRequest struct {
	MemberID int64  `json:"member_id" binding:"required,min=1"`
	Channel  string `json:"channel" binding:"required"`
}

// Resubscribe clears a member's opt-out from a channel (idempotent).
// POST /v1/boss/reach/resubscribe
func (h *ReachHandler) Resubscribe(c *gin.Context) {
	var req resubscribeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	if err := h.svc.Resubscribe(c.Request.Context(), req.MemberID, req.Channel); err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, gin.H{"ok": true})
}

// failReach maps reach hard errors to HTTP codes. Blocked sends never reach
// here — they are returned as ReachTask data (status=blocked).
func (h *ReachHandler) failReach(c *gin.Context, err error) {
	switch {
	case errors.Is(err, reach.ErrTemplateUnknown):
		httpx.Fail(c, errcode.ErrTemplateUnknown)
	default:
		httpx.FailError(c, err)
	}
}
