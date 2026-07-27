package handler

// CRMHandler exposes the boss-surface community-group + living-code endpoints
// (M2-S2, CONTRACT §11.3.5 BE-B-06): group CRUD, SOP-stage advancement, member
// delta, living-code creation, and living-code store resolution (分流). Tenant /
// store come from the verified request context, never the request body.

import (
	"errors"
	"strconv"

	"github.com/gin-gonic/gin"

	"github.com/zhangguijunshi/backend/internal/crm"
	"github.com/zhangguijunshi/backend/pkg/errcode"
	"github.com/zhangguijunshi/backend/pkg/httpx"
)

// CRMHandler serves crm endpoints.
type CRMHandler struct {
	svc *crm.Service
}

// NewCRMHandler constructs a CRMHandler.
func NewCRMHandler(svc *crm.Service) *CRMHandler {
	return &CRMHandler{svc: svc}
}

type createGroupRequest struct {
	StoreID      int64  `json:"store_id"`
	Name         string `json:"name" binding:"required"`
	WeworkChatID string `json:"wework_chat_id"`
	SOPStage     string `json:"sop_stage"`
}

// CreateGroup persists a community group.
// POST /v1/boss/crm/groups
func (h *CRMHandler) CreateGroup(c *gin.Context) {
	var req createGroupRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	g, err := h.svc.CreateGroup(c.Request.Context(), &crm.CommunityGroup{
		StoreID:      storeScope(c, req.StoreID),
		Name:         req.Name,
		WeworkChatID: req.WeworkChatID,
		SOPStage:     req.SOPStage,
	})
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, g)
}

// ListGroups lists community groups (store_id<=0 = all stores).
// GET /v1/boss/crm/groups?store_id=
func (h *CRMHandler) ListGroups(c *gin.Context) {
	list, err := h.svc.ListGroups(c.Request.Context(), queryInt64(c, "store_id"))
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, list)
}

// GetGroup loads one community group.
// GET /v1/boss/crm/groups/:id
func (h *CRMHandler) GetGroup(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	g, err := h.svc.GetGroup(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, crm.ErrNotFound) {
			httpx.Fail(c, errcode.Err{Code: 40404, Message: "社群不存在", HTTP: 404})
			return
		}
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, g)
}

// AdvanceSOP advances a group one step: recruit→activate→convert→refer.
// POST /v1/boss/crm/groups/:id/sop/advance
func (h *CRMHandler) AdvanceSOP(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	g, err := h.svc.AdvanceSOP(c.Request.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, crm.ErrNotFound):
			httpx.Fail(c, errcode.Err{Code: 40404, Message: "社群不存在", HTTP: 404})
		case errors.Is(err, crm.ErrSOPTerminal):
			httpx.Fail(c, errcode.ErrSOPTerminal)
		default:
			httpx.FailError(c, err)
		}
		return
	}
	httpx.OK(c, g)
}

type addGroupMembersRequest struct {
	Delta int `json:"delta" binding:"required"`
}

// AddGroupMembers applies a signed delta to a group's member count.
// POST /v1/boss/crm/groups/:id/members
func (h *CRMHandler) AddGroupMembers(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	var req addGroupMembersRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	g, err := h.svc.AddGroupMembers(c.Request.Context(), id, req.Delta)
	if err != nil {
		if errors.Is(err, crm.ErrNotFound) {
			httpx.Fail(c, errcode.Err{Code: 40404, Message: "社群不存在", HTTP: 404})
			return
		}
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, g)
}

type createLivingCodeRequest struct {
	StoreID int64                `json:"store_id"`
	Code    string               `json:"code" binding:"required"`
	Scene   string               `json:"scene" binding:"required"`
	Config  crm.LivingCodeConfig `json:"config"`
}

// CreateCode persists a living code.
// POST /v1/boss/crm/living-codes
func (h *CRMHandler) CreateCode(c *gin.Context) {
	var req createLivingCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	lc, err := h.svc.CreateCode(c.Request.Context(), &crm.LivingCode{
		StoreID: storeScope(c, req.StoreID),
		Code:    req.Code,
		Scene:   req.Scene,
		Config:  req.Config,
	})
	if err != nil {
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, lc)
}

type resolveCodeRequest struct {
	Code string `json:"code" binding:"required"`
}

// ResolveCode resolves the destination store + welcome config for one scan.
// POST /v1/boss/crm/living-codes/resolve
func (h *CRMHandler) ResolveCode(c *gin.Context) {
	var req resolveCodeRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		httpx.Fail(c, errcode.ErrParam)
		return
	}
	storeID, cfg, err := h.svc.ResolveCode(c.Request.Context(), req.Code)
	if err != nil {
		if errors.Is(err, crm.ErrNotFound) {
			httpx.Fail(c, errcode.Err{Code: 40404, Message: "活码不存在", HTTP: 404})
			return
		}
		httpx.FailError(c, err)
		return
	}
	httpx.OK(c, gin.H{"store_id": storeID, "config": cfg})
}
