package crm

// pg_repo_test.go exercises the pgx-backed Repo against pgxmock (CONTRACT
// §17.3, mirroring payment/pg_repo_test.go). The pool uses QueryMatcherEqual
// because the SQL text contains regex metacharacters; tests are same-package so
// they reference the unexported sql* constants and the nullString/nullInt64
// helpers. The load-bearing assertions are UpdateGroupSOP / AddGroupMembers as
// single conditional UPDATE ... RETURNING statements and the GREATEST clamp.

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pashagolub/pgxmock/v4"
	"github.com/stretchr/testify/require"
)

// Compile-time interface satisfaction.
var _ Repo = (*PGRepo)(nil)

// fixedTime is the deterministic clock used across every pgxmock row.
var fixedTime = time.Date(2026, time.July, 29, 9, 30, 0, 0, time.UTC)

// newMockPool builds a pgxmock pool with plain-equality SQL matching.
func newMockPool(t *testing.T) pgxmock.PgxPoolIface {
	t.Helper()
	mock, err := pgxmock.NewPool(pgxmock.QueryMatcherOption(pgxmock.QueryMatcherEqual))
	require.NoError(t, err)
	t.Cleanup(func() { mock.Close() })
	return mock
}

// groupColumns returns the community_groups column set in scanCommunityGroup
// order (the row mapping pgxmock relies on).
func groupColumns() []string {
	return []string{"id", "tenant_id", "store_id", "wework_chat_id", "name", "member_count", "sop_stage", "created_at", "updated_at"}
}

// codeColumns returns the wework_living_codes column set in scanLivingCode
// order.
func codeColumns() []string {
	return []string{"id", "tenant_id", "store_id", "code", "scene", "config", "created_at"}
}

func TestPGRepo_CreateGroup(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	cfg := &CommunityGroup{
		TenantID: 1, StoreID: 10, WeworkChatID: "chat_1", Name: "VIP群",
		MemberCount: 5, SOPStage: "", CreatedAt: fixedTime, UpdatedAt: fixedTime,
	}
	// SOPStage "" is seeded to recruit before INSERT; nullString maps the
	// nullable chat_id / stage columns exactly as the implementation encodes them.
	mock.ExpectQuery(sqlGroupCreate).
		WithArgs(int64(1), int64(10), nullString("chat_1"), "VIP群", 5, nullString(SOPRecruit), fixedTime, fixedTime).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(42)))
	g, err := repo.CreateGroup(context.Background(), cfg)
	require.NoError(t, err)
	require.Equal(t, int64(42), g.ID)
	require.Equal(t, SOPRecruit, g.SOPStage) // empty stage seeded
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_GetGroup(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	mock.ExpectQuery(sqlGroupGetByID).
		WithArgs(int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(groupColumns()).AddRow(
			int64(42), int64(1), int64(10), "chat_1", "VIP群", 7, SOPConvert, fixedTime, fixedTime,
		))
	g, err := repo.GetGroup(context.Background(), 1, 42)
	require.NoError(t, err)
	require.Equal(t, "VIP群", g.Name)
	require.Equal(t, 7, g.MemberCount)
	require.Equal(t, SOPConvert, g.SOPStage)
	require.Equal(t, "chat_1", g.WeworkChatID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_GetGroupNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	mock.ExpectQuery(sqlGroupGetByID).
		WithArgs(int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetGroup(context.Background(), 1, 99)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_ListGroups(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	mock.ExpectQuery(sqlGroupListAll).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows(groupColumns()).
			AddRow(int64(1), int64(1), int64(10), "c1", "A", 3, SOPRecruit, fixedTime, fixedTime).
			AddRow(int64(2), int64(1), int64(11), nil, "B", 10, SOPRefer, fixedTime, fixedTime))
	got, err := repo.ListGroups(context.Background(), 1, 0)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, int64(1), got[0].ID)
	require.Equal(t, "", got[1].WeworkChatID) // NULL chat_id → ""
	require.Equal(t, SOPRefer, got[1].SOPStage)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_ListGroupsByStore(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	mock.ExpectQuery(sqlGroupListStore).
		WithArgs(int64(1), int64(10)).
		WillReturnRows(pgxmock.NewRows(groupColumns()).
			AddRow(int64(1), int64(1), int64(10), "c1", "A", 3, SOPRecruit, fixedTime, fixedTime))
	got, err := repo.ListGroups(context.Background(), 1, 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, int64(10), got[0].StoreID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_UpdateGroupSOP(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	mock.ExpectQuery(sqlGroupUpdateSOP).
		WithArgs(nullString(SOPActivate), fixedTime, int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(groupColumns()).AddRow(
			int64(42), int64(1), int64(10), "chat_1", "VIP群", 5, SOPActivate, fixedTime, fixedTime,
		))
	g, err := repo.UpdateGroupSOP(context.Background(), 1, 42, SOPActivate, fixedTime)
	require.NoError(t, err)
	require.Equal(t, SOPActivate, g.SOPStage)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_UpdateGroupSOPNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	mock.ExpectQuery(sqlGroupUpdateSOP).
		WithArgs(nullString(SOPActivate), fixedTime, int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.UpdateGroupSOP(context.Background(), 1, 99, SOPActivate, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_AddGroupMembers(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	// Positive delta: GREATEST(5 + 3, 0) = 8.
	mock.ExpectQuery(sqlGroupAddMembers).
		WithArgs(3, fixedTime, int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(groupColumns()).AddRow(
			int64(42), int64(1), int64(10), "chat_1", "VIP群", 8, SOPRecruit, fixedTime, fixedTime,
		))
	g, err := repo.AddGroupMembers(context.Background(), 1, 42, 3, fixedTime)
	require.NoError(t, err)
	require.Equal(t, 8, g.MemberCount)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_AddGroupMembersClampsToZero(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	// Negative delta past zero: GREATEST(5 + (-100), 0) = 0.
	mock.ExpectQuery(sqlGroupAddMembers).
		WithArgs(-100, fixedTime, int64(1), int64(42)).
		WillReturnRows(pgxmock.NewRows(groupColumns()).AddRow(
			int64(42), int64(1), int64(10), "chat_1", "VIP群", 0, SOPRecruit, fixedTime, fixedTime,
		))
	g, err := repo.AddGroupMembers(context.Background(), 1, 42, -100, fixedTime)
	require.NoError(t, err)
	require.Equal(t, 0, g.MemberCount) // clamp ≥ 0
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_AddGroupMembersNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	mock.ExpectQuery(sqlGroupAddMembers).
		WithArgs(3, fixedTime, int64(1), int64(99)).
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.AddGroupMembers(context.Background(), 1, 99, 3, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_CreateCode(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	wantCfg := LivingCodeConfig{AutoAssignRule: RuleRoundRobin, WelcomeMsg: "欢迎光临"}
	cfgBytes, _ := json.Marshal(wantCfg)
	lc := &LivingCode{
		TenantID: 1, StoreID: 10, Code: "CODE-A", Scene: SceneAddFriend,
		Config: wantCfg, CreatedAt: fixedTime,
	}
	mock.ExpectQuery(sqlCodeCreate).
		WithArgs(int64(1), nullInt64(10), "CODE-A", SceneAddFriend, cfgBytes, fixedTime).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(7)))
	got, err := repo.CreateCode(context.Background(), lc)
	require.NoError(t, err)
	require.Equal(t, int64(7), got.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_CreateCodeNullStore(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	cfgBytes, _ := json.Marshal(LivingCodeConfig{AutoAssignRule: RuleNearestStore})
	lc := &LivingCode{
		TenantID: 1, StoreID: 0, Code: "CODE-B", Scene: SceneJoinGroup,
		Config: LivingCodeConfig{AutoAssignRule: RuleNearestStore}, CreatedAt: fixedTime,
	}
	// StoreID 0 → NULL (sql.NullInt64{} valid=false), matching a cross-store code.
	mock.ExpectQuery(sqlCodeCreate).
		WithArgs(int64(1), sql.NullInt64{}, "CODE-B", SceneJoinGroup, cfgBytes, fixedTime).
		WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(int64(8)))
	got, err := repo.CreateCode(context.Background(), lc)
	require.NoError(t, err)
	require.Equal(t, int64(8), got.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_GetCode(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	// LivingCodeConfig has no json tags, so the JSONB round-trips under the Go
	// field names — marshal the same struct the implementation decodes into.
	cfgBytes, _ := json.Marshal(LivingCodeConfig{AutoAssignRule: RuleRoundRobin, WelcomeMsg: "hi"})
	mock.ExpectQuery(sqlCodeGetByCode).
		WithArgs(int64(1), "CODE-A").
		WillReturnRows(pgxmock.NewRows(codeColumns()).AddRow(
			int64(7), int64(1), int64(10), "CODE-A", SceneAddFriend, cfgBytes, fixedTime,
		))
	lc, err := repo.GetCode(context.Background(), 1, "CODE-A")
	require.NoError(t, err)
	require.Equal(t, RuleRoundRobin, lc.Config.AutoAssignRule)
	require.Equal(t, "hi", lc.Config.WelcomeMsg)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_GetCodeNotFound(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	mock.ExpectQuery(sqlCodeGetByCode).
		WithArgs(int64(1), "nope").
		WillReturnError(pgx.ErrNoRows)
	_, err := repo.GetCode(context.Background(), 1, "nope")
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_ListCodes(t *testing.T) {
	mock := newMockPool(t)
	repo := NewPGRepo(mock)
	mock.ExpectQuery(sqlCodeListAll).
		WithArgs(int64(1)).
		WillReturnRows(pgxmock.NewRows(codeColumns()).
			AddRow(int64(7), int64(1), int64(10), "CODE-A", SceneAddFriend, []byte(`{}`), fixedTime).
			AddRow(int64(8), int64(1), nil, "CODE-B", SceneJoinGroup, []byte(`{}`), fixedTime))
	got, err := repo.ListCodes(context.Background(), 1, 0)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, int64(10), got[0].StoreID)
	require.Equal(t, int64(0), got[1].StoreID) // NULL store_id → 0
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestPGRepo_NilPoolIsSafe(t *testing.T) {
	// Nil pool degrades: single reads / writes → ErrNotFound; lists → empty.
	r1 := NewPGRepo(nil)
	_, err := r1.GetGroup(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = r1.GetCode(context.Background(), 1, "x")
	require.ErrorIs(t, err, ErrNotFound)
	_, err = r1.UpdateGroupSOP(context.Background(), 1, 1, SOPActivate, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = r1.AddGroupMembers(context.Background(), 1, 1, 1, fixedTime)
	require.ErrorIs(t, err, ErrNotFound)
	_, err = r1.CreateGroup(context.Background(), &CommunityGroup{Name: "x"})
	require.ErrorIs(t, err, ErrNotFound)
	_, err = r1.CreateCode(context.Background(), &LivingCode{Code: "x"})
	require.ErrorIs(t, err, ErrNotFound)

	got, err := r1.ListGroups(context.Background(), 1, 0)
	require.NoError(t, err)
	require.Empty(t, got)
	codes, err := r1.ListCodes(context.Background(), 1, 0)
	require.NoError(t, err)
	require.Empty(t, codes)

	// Nil receiver is safe too.
	var r2 *PGRepo
	_, err = r2.GetGroup(context.Background(), 1, 1)
	require.ErrorIs(t, err, ErrNotFound)
}
