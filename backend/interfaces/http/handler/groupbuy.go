package handler

// GroupbuyHandler exposes the boss-surface group-buy endpoints (M2-S2, CONTRACT
// §11.3.1 BE-B-03): create / list-active / get / join / list-participants /
// cancel. Tenant/store come from the verified request context, never the
// request body.

import (
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/internal/groupbuy"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
)

// GroupbuyHandler serves groupbuy endpoints.
type GroupbuyHandler struct {
	svc *groupbuy.Service
}

// NewGroupbuyHandler constructs a GroupbuyHandler.
func NewGroupbuyHandler(svc *groupbuy.Service) *GroupbuyHandler {
	return &GroupbuyHandler{svc: svc}
}

type createGroupbuyRequest struct {
	StoreID       int64                   `json:"store_id"`
	ProductID     int64                   `json:"product_id" binding:"required,min=1"`
	Title         string                  `json:"title" binding:"required"`
	Params        groupbuy.GroupbuyParams `json:"params"`
	RequiredCount int                     `json:"required_count"`
	ValidHours    int                     `json:"valid_hours"`
}

// Create opens a new active squad.
// POST /v1/boss/groupbuys
func (h *GroupbuyHandler) Create(c *gin.Context) {
	var req createGroupbuyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	g, err := h.svc.Create(c.Request.Context(), groupbuy.CreateInput{
		StoreID:       req.StoreID,
		ProductID:     req.ProductID,
		Title:         req.Title,
		Params:        req.Params,
		RequiredCount: req.RequiredCount,
		ValidHours:    req.ValidHours,
	})
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, g)
}

// ListActive returns active squads (store_id<=0 = all stores in tenant).
// GET /v1/boss/groupbuys/active?store_id=
func (h *GroupbuyHandler) ListActive(c *gin.Context) {
	list, err := h.svc.ListActive(c.Request.Context(), queryInt64(c, "store_id"))
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, list)
}

// Get loads one squad.
// GET /v1/boss/groupbuys/:id
func (h *GroupbuyHandler) Get(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	g, err := h.svc.Get(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, groupbuy.ErrNotFound) {
			httpx.Fail(c, errcode.Err{Code: 40404, Message: "拼团不存在", HTTP: 404})
			return
		}
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, g)
}

type joinGroupbuyRequest struct {
	MemberID int64 `json:"member_id" binding:"required,min=1"`
}

// Join adds a member to a squad (atomic; full ⇒ success flip).
// POST /v1/boss/groupbuys/:id/join
func (h *GroupbuyHandler) Join(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	var req joinGroupbuyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	p, g, err := h.svc.Join(c.Request.Context(), id, req.MemberID)
	if err != nil {
		h.failGroupbuy(c, err)
		return
	}
	httpx.OK(c, gin.H{"participant": p, "groupbuy": g})
}

// ListParticipants lists a squad's participants.
// GET /v1/boss/groupbuys/:id/participants
func (h *GroupbuyHandler) ListParticipants(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	list, err := h.svc.ListParticipants(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, groupbuy.ErrNotFound) {
			httpx.Fail(c, errcode.Err{Code: 40404, Message: "拼团不存在", HTTP: 404})
			return
		}
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, list)
}

// Cancel transitions a squad active → cancelled.
// POST /v1/boss/groupbuys/:id/cancel
func (h *GroupbuyHandler) Cancel(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	g, err := h.svc.Cancel(c.Request.Context(), id)
	if err != nil {
		h.failGroupbuy(c, err)
		return
	}
	httpx.OK(c, g)
}

// failGroupbuy maps groupbuy domain errors to HTTP codes.
func (h *GroupbuyHandler) failGroupbuy(c *gin.Context, err error) {
	switch {
	case errors.Is(err, groupbuy.ErrNotFound):
		httpx.Fail(c, errcode.Err{Code: 40404, Message: "拼团不存在", HTTP: 404})
	case errors.Is(err, groupbuy.ErrGroupbuyFull):
		httpx.Fail(c, errcode.ErrGroupbuyFull)
	case errors.Is(err, groupbuy.ErrGroupbuyClosed):
		httpx.Fail(c, errcode.ErrGroupbuyClosed)
	case errors.Is(err, groupbuy.ErrAlreadyJoined):
		httpx.Fail(c, errcode.ErrAlreadyJoined)
	default:
		httpx.FailError(c, err)
	}
}
