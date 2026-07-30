package auth

// rbac.go maps roles to admin-web menu permissions (CONTRACT §19 / BE-A-01) and
// builds the GET /v1/admin/me response. Menu strings match the admin-web route
// keys (the frontend reads permissions.menus to gate nav visibility).
//
// This is intentionally a flat allowlist of top-level menu keys, not a
// fine-grained action matrix: the backend enforces access with
// RequireScene/RequireRole middleware, while menus only drive client-side nav.
// Adding a menu for a role is therefore a UI concern, not a security boundary.

import "github.com/zhangguijunshi/backend/pkg/jwt"

// RoleMenus maps each role to its visible top-level menu keys.
var RoleMenus = map[string][]string{
	"owner":        {"dashboard", "catalog", "orders", "members", "marketing", "finance", "reach", "settings"},
	"manager":      {"dashboard", "catalog", "orders", "members", "marketing", "finance", "reach", "settings"},
	"clerk":        {"dashboard", "orders", "members"},
	"cashier":      {"dashboard", "orders", "members"},
	"regional_mgr": {"dashboard", "catalog", "orders", "marketing"},
	"platform_ops": {"platform/merchants", "platform/audit", "platform/billing", "platform/tenants"},
}

// MenusFor returns the menu keys for role, or an empty (non-nil) slice for an
// unknown role so the response shape is always stable.
func MenusFor(role string) []string {
	if m, ok := RoleMenus[role]; ok {
		return m
	}
	return []string{}
}

// MeUser is the caller-identity block of the /me response.
type MeUser struct {
	Name     string `json:"name"`
	Role     string `json:"role"`
	TenantID int64  `json:"tenant_id"`
	StoreID  int64  `json:"store_id"`
}

// MePermissions is the permissions block (menus now; a future action matrix may
// extend it without breaking the JSON shape).
type MePermissions struct {
	Menus []string `json:"menus"`
}

// MeResult is the GET /v1/admin/me payload.
type MeResult struct {
	User        MeUser        `json:"user"`
	Role        string        `json:"role"`
	Permissions MePermissions `json:"permissions"`
}

// BuildMeResult assembles the /me response from verified claims. Name is left
// empty because the JWT carries no display name; a future /me may join
// users.name by id (out of scope for M5 Phase 1).
func BuildMeResult(claims *jwt.Claims) MeResult {
	return MeResult{
		User: MeUser{
			Name:     "",
			Role:     claims.Role,
			TenantID: claims.TenantID,
			StoreID:  claims.StoreID,
		},
		Role: claims.Role,
		Permissions: MePermissions{
			Menus: MenusFor(claims.Role),
		},
	}
}
