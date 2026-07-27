-- ============================================================
-- 000006_reach_community_groupbuy.up.sql
-- 触达 / 社群 / 拼团域 (M2-S2)
-- 依据: 技术方案 第5章 §5.3.5 (拼团) + §5.3.6 (社群/触达)
--       CONTRACT §11.2 (M2-S2 迁移编号与裁剪)
-- 目标数据库: PostgreSQL 16
--
-- 范围(对照 CONTRACT §11.1, 7 张表全部纳入):
--   拼团(BE-B-03):      groupbuys + groupbuy_participants
--   社群/活码(BE-B-06): community_groups + wework_living_codes
--   触达(BE-B-07):      reach_tasks
--   订阅计数(BE-B-08):  subscribe_auth
--   退订(BE-B-10):      unsubscribe_records (本迁移新增, ch05 §5.3.6 未列)
--
-- 分区: ch05 无分区设计, 均普通表。
--
-- 触发器(沿用 000001/000004/000005 范式):
--   * groupbuys / community_groups 含 updated_at, 挂 BEFORE UPDATE 触发器
--     自动刷新 updated_at; 复用 000001 创建的 trg_set_updated_at() 函数,
--     此处仅 CREATE TRIGGER (名带表名后缀避免冲突)。
--   * groupbuy_participants / wework_living_codes / reach_tasks /
--     subscribe_auth / unsubscribe_records 无 updated_at, 不挂触发器。
--
-- CHECK 约束(reach_tasks 特殊, CONTRACT §11.2):
--   reach_tasks 仅 channel / template_code / status 加 CHECK;
--   blocked_reason 留弹性不加 CHECK (CONTRACT 枚举
--     freq_circuit|content_filter|unsubscribed|subscribe_quota|risk,
--     未来可能扩展, 不锁死, 仅以注释说明取值)。
--
-- 其它约定(沿用 000001/000004/000005):
--   * 时间戳统一 TIMESTAMPTZ(UTC); created_at / updated_at 默认 now()。
--   * tenant_id 全部 NOT NULL REFERENCES tenants(id), 行级隔离键。
--   * 金额 groupbuys.params.price_cents 走 JSONB 内 BIGINT 分(应用层约束)。
--   * params / config 用 JSONB 存弹性参数, 不存需检索过滤的业务键。
-- ============================================================


-- ============================================================
-- 1. groupbuys : 拼团活动主表
--    status: active(进行中) | success(成团) | failed(超时未成团) | cancelled(取消)
--    required_count: 3-4 人成团(CONTRACT §11.3.1); joined 达 required_count 即 success。
-- ============================================================
CREATE TABLE groupbuys (
    id             BIGSERIAL    PRIMARY KEY,
    tenant_id      BIGINT       NOT NULL REFERENCES tenants(id),
    store_id       BIGINT       NOT NULL REFERENCES stores(id),
    product_id     BIGINT       REFERENCES products(id),           -- 关联商品(可空, 赠品/服务类拼团)
    title          TEXT         NOT NULL,
    params         JSONB        NOT NULL DEFAULT '{}'::jsonb,       -- {group_size, price_cents, valid_hours}
    required_count INT          NOT NULL,                           -- 成团所需人数(3-4)
    joined         INT          NOT NULL DEFAULT 0,                 -- 已参团人数
    status         TEXT         NOT NULL DEFAULT 'active'
                   CHECK (status IN ('active','success','failed','cancelled')),
    start_at       TIMESTAMPTZ  NOT NULL,
    end_at         TIMESTAMPTZ  NOT NULL,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CHECK (end_at > start_at),
    CHECK (required_count > 0),
    CHECK (joined >= 0)
);

CREATE INDEX idx_groupbuys_tenant_store ON groupbuys(tenant_id, store_id);
CREATE INDEX idx_groupbuys_tenant_status ON groupbuys(tenant_id, status) WHERE status = 'active';

CREATE TRIGGER trg_groupbuys_updated_at
    BEFORE UPDATE ON groupbuys
    FOR EACH ROW EXECUTE FUNCTION trg_set_updated_at();

COMMENT ON TABLE  groupbuys                IS '拼团活动主表; 按 tenant_id + store_id 行级隔离; 状态机 active→success/failed/cancelled';
COMMENT ON COLUMN groupbuys.id             IS '拼团自增主键';
COMMENT ON COLUMN groupbuys.tenant_id      IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN groupbuys.store_id       IS '所属门店ID(外键 -> stores.id)';
COMMENT ON COLUMN groupbuys.product_id     IS '关联商品ID(外键 -> products.id), 可空(赠品/服务类拼团)';
COMMENT ON COLUMN groupbuys.title          IS '拼团标题';
COMMENT ON COLUMN groupbuys.params         IS '弹性参数 JSONB: {group_size, price_cents(分), valid_hours}';
COMMENT ON COLUMN groupbuys.required_count IS '成团所需人数(3-4), > 0';
COMMENT ON COLUMN groupbuys.joined         IS '已参团人数, >= 0; 达 required_count 即成团';
COMMENT ON COLUMN groupbuys.status         IS '状态: active(进行中) | success(成团) | failed(超时未成团) | cancelled(取消)';
COMMENT ON COLUMN groupbuys.start_at       IS '开团时间(UTC)';
COMMENT ON COLUMN groupbuys.end_at         IS '截团时间(UTC), 须 > start_at; FinalizeExpired 据此判定';
COMMENT ON COLUMN groupbuys.created_at     IS '创建时间(UTC)';
COMMENT ON COLUMN groupbuys.updated_at     IS '更新时间(UTC), 由触发器自动刷新';


-- ============================================================
-- 2. groupbuy_participants : 拼团参团记录(强归因, 禁软删)
--    status: joined(已参团) | paid(已支付) | refunded(已退款)
--    UNIQUE(tenant_id, groupbuy_id, member_id): 一会员同一团仅一条(ErrAlreadyJoined 的 DB 兜底)
--    无 updated_at, 不挂触发器。
-- ============================================================
CREATE TABLE groupbuy_participants (
    id           BIGSERIAL    PRIMARY KEY,
    tenant_id    BIGINT       NOT NULL REFERENCES tenants(id),
    groupbuy_id  BIGINT       NOT NULL REFERENCES groupbuys(id),
    member_id    BIGINT       NOT NULL REFERENCES members(id),
    status       TEXT         NOT NULL DEFAULT 'joined'
                 CHECK (status IN ('joined','paid','refunded')),
    joined_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, groupbuy_id, member_id)
);

CREATE INDEX idx_gbp_tenant_groupbuy ON groupbuy_participants(tenant_id, groupbuy_id);
CREATE INDEX idx_gbp_tenant_member ON groupbuy_participants(tenant_id, member_id);

COMMENT ON TABLE  groupbuy_participants             IS '拼团参团记录; 强归因禁软删; UNIQUE(tenant_id,groupbuy_id,member_id) 兜底防重复参团';
COMMENT ON COLUMN groupbuy_participants.id          IS '参团记录自增主键';
COMMENT ON COLUMN groupbuy_participants.tenant_id   IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN groupbuy_participants.groupbuy_id IS '拼团ID(外键 -> groupbuys.id)';
COMMENT ON COLUMN groupbuy_participants.member_id   IS '会员ID(外键 -> members.id)';
COMMENT ON COLUMN groupbuy_participants.status      IS '状态: joined(已参团) | paid(已支付) | refunded(已退款)';
COMMENT ON COLUMN groupbuy_participants.joined_at   IS '参团时间(UTC)';


-- ============================================================
-- 3. wework_living_codes : 企微活码(分流加好友/进群)
--    scene: add_friend(加好友) | join_group(进群)
--    config JSONB: {auto_assign_rule(nearest_store|round_robin), welcome_msg}
--    无 updated_at, 不挂触发器。
-- ============================================================
CREATE TABLE wework_living_codes (
    id           BIGSERIAL    PRIMARY KEY,
    tenant_id    BIGINT       NOT NULL REFERENCES tenants(id),
    store_id     BIGINT       REFERENCES stores(id),              -- 可空(活码可跨店分流)
    code         TEXT         NOT NULL,
    scene        TEXT         NOT NULL
                 CHECK (scene IN ('add_friend','join_group')),
    config       JSONB        NOT NULL DEFAULT '{}'::jsonb,        -- {auto_assign_rule, welcome_msg}
    created_at   TIMESTAMPTZ  NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, code)
);

CREATE INDEX idx_living_codes_tenant_store ON wework_living_codes(tenant_id, store_id);
CREATE INDEX idx_living_codes_tenant_scene ON wework_living_codes(tenant_id, scene);

COMMENT ON TABLE  wework_living_codes             IS '企微活码; 按 tenant_id 行级隔离; UNIQUE(tenant_id,code); WX-06 真实 API 延后(M2-S2 仅数据模型)';
COMMENT ON COLUMN wework_living_codes.id          IS '活码自增主键';
COMMENT ON COLUMN wework_living_codes.tenant_id   IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN wework_living_codes.store_id    IS '归属门店ID(外键 -> stores.id), 可空(活码可跨店分流)';
COMMENT ON COLUMN wework_living_codes.code        IS '活码标识, 租户内唯一';
COMMENT ON COLUMN wework_living_codes.scene       IS '场景: add_friend(加好友) | join_group(进群)';
COMMENT ON COLUMN wework_living_codes.config      IS '配置 JSONB: {auto_assign_rule(nearest_store|round_robin), welcome_msg}';
COMMENT ON COLUMN wework_living_codes.created_at  IS '创建时间(UTC)';


-- ============================================================
-- 4. community_groups : 社群(企微客户群)
--    sop_stage: recruit(拉新) | activate(激活) | convert(转化) | refer(裂变) 四阶段 SOP
--    挂 BEFORE UPDATE 触发器刷新 updated_at。
-- ============================================================
CREATE TABLE community_groups (
    id             BIGSERIAL    PRIMARY KEY,
    tenant_id      BIGINT       NOT NULL REFERENCES tenants(id),
    store_id       BIGINT       NOT NULL REFERENCES stores(id),
    wework_chat_id TEXT,                                      -- 企微客户群 chat_id(可空, 未绑定前)
    name           TEXT         NOT NULL,
    member_count   INT          NOT NULL DEFAULT 0,
    sop_stage      TEXT         CHECK (sop_stage IN ('recruit','activate','convert','refer')),
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    updated_at     TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CHECK (member_count >= 0)
);

CREATE INDEX idx_groups_tenant_store ON community_groups(tenant_id, store_id);
CREATE INDEX idx_groups_tenant_sop ON community_groups(tenant_id, sop_stage) WHERE sop_stage IS NOT NULL;

CREATE TRIGGER trg_community_groups_updated_at
    BEFORE UPDATE ON community_groups
    FOR EACH ROW EXECUTE FUNCTION trg_set_updated_at();

COMMENT ON TABLE  community_groups                IS '社群(企微客户群); 按 tenant_id + store_id 行级隔离; sop_stage 四阶段状态机';
COMMENT ON COLUMN community_groups.id             IS '社群自增主键';
COMMENT ON COLUMN community_groups.tenant_id      IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN community_groups.store_id       IS '所属门店ID(外键 -> stores.id)';
COMMENT ON COLUMN community_groups.wework_chat_id  IS '企微客户群 chat_id, 可空(未绑定时)';
COMMENT ON COLUMN community_groups.name           IS '社群名称';
COMMENT ON COLUMN community_groups.member_count   IS '群成员数, >= 0';
COMMENT ON COLUMN community_groups.sop_stage      IS 'SOP 阶段: recruit(拉新) | activate(激活) | convert(转化) | refer(裂变, 终态); 空=未启动SOP';
COMMENT ON COLUMN community_groups.created_at     IS '创建时间(UTC)';
COMMENT ON COLUMN community_groups.updated_at     IS '更新时间(UTC), 由触发器自动刷新';


-- ============================================================
-- 5. reach_tasks : 触达任务记录(频次熔断在 Redis/内存计数, 此处留痕)
--    channel: wework_group | wework_1v1 | subscribe_msg | sms
--    template_code: 8 类触达模板(CONTRACT §11.3.2)
--    status: pending | sent | blocked | failed
--    blocked_reason: 留弹性不加 CHECK(CONTRACT §11.2), 取值见注释。
--    无 updated_at, 不挂触发器。
-- ============================================================
CREATE TABLE reach_tasks (
    id             BIGSERIAL    PRIMARY KEY,
    tenant_id      BIGINT       NOT NULL REFERENCES tenants(id),
    store_id       BIGINT       NOT NULL REFERENCES stores(id),
    channel        TEXT         NOT NULL
                   CHECK (channel IN ('wework_group','wework_1v1','subscribe_msg','sms')),
    template_code  TEXT         NOT NULL
                   CHECK (template_code IN ('new_welcome','birthday','recall_30','recall_60','recall_90','repurchase','balance_low','points_expire')),
    target_count   INT          NOT NULL,                       -- 目标触达人数
    status         TEXT         NOT NULL DEFAULT 'pending'
                   CHECK (status IN ('pending','sent','blocked','failed')),
    blocked_reason TEXT,                                        -- freq_circuit|content_filter|unsubscribed|subscribe_quota|risk (弹性, 不加 CHECK)
    strategy_id    BIGINT,                                      -- 触发自哪条AI策略(归因, M3 建表后补 FK)
    scheduled_at   TIMESTAMPTZ  NOT NULL,
    sent_at        TIMESTAMPTZ,
    created_at     TIMESTAMPTZ  NOT NULL DEFAULT now()
);

CREATE INDEX idx_reach_tenant_status ON reach_tasks(tenant_id, status, scheduled_at);
CREATE INDEX idx_reach_tenant_strategy ON reach_tasks(tenant_id, strategy_id) WHERE strategy_id IS NOT NULL;
CREATE INDEX idx_reach_tenant_created ON reach_tasks(tenant_id, created_at);

COMMENT ON TABLE  reach_tasks              IS '触达任务记录; 按 tenant_id 行级隔离; blocked_reason 留弹性不加 CHECK; strategy_id 是 AI 归因关键链路';
COMMENT ON COLUMN reach_tasks.id           IS '任务自增主键';
COMMENT ON COLUMN reach_tasks.tenant_id    IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN reach_tasks.store_id     IS '所属门店ID(外键 -> stores.id)';
COMMENT ON COLUMN reach_tasks.channel      IS '触达通道: wework_group(企微群发) | wework_1v1(企微1v1) | subscribe_msg(订阅消息) | sms(短信)';
COMMENT ON COLUMN reach_tasks.template_code IS '触达模板码(8 类): new_welcome | birthday | recall_30 | recall_60 | recall_90 | repurchase | balance_low | points_expire';
COMMENT ON COLUMN reach_tasks.target_count IS '目标触达人数';
COMMENT ON COLUMN reach_tasks.status       IS '状态: pending(待发) | sent(已发) | blocked(已拦截) | failed(发送失败)';
COMMENT ON COLUMN reach_tasks.blocked_reason IS '拦截原因(弹性, 无 CHECK): freq_circuit | content_filter | unsubscribed | subscribe_quota | risk; NULL=非拦截';
COMMENT ON COLUMN reach_tasks.strategy_id  IS 'AI策略项ID(归因), NULL=非AI来源; M3 建表后补外键';
COMMENT ON COLUMN reach_tasks.scheduled_at IS '计划发送时间(UTC)';
COMMENT ON COLUMN reach_tasks.sent_at      IS '实际发送时间(UTC), pending/blocked 时为 NULL';
COMMENT ON COLUMN reach_tasks.created_at   IS '创建时间(UTC)';


-- ============================================================
-- 6. subscribe_auth : 订阅消息授权计数(一次性授权, 原子消耗)
--    granted_quota: 用户授权的可用次数; used_quota: 已消耗次数。
--    CHECK (used_quota <= granted_quota): 不可超额消耗(DB 兜底 ConsumeSubscribe 原子性)。
--    无 updated_at, 不挂触发器。
-- ============================================================
CREATE TABLE subscribe_auth (
    id            BIGSERIAL    PRIMARY KEY,
    tenant_id     BIGINT       NOT NULL REFERENCES tenants(id),
    member_id     BIGINT       NOT NULL REFERENCES members(id),
    template_id   TEXT         NOT NULL,                        -- 微信订阅消息模板id
    granted_quota INT          NOT NULL DEFAULT 1,              -- 授权次数
    used_quota    INT          NOT NULL DEFAULT 0,
    granted_at    TIMESTAMPTZ  NOT NULL DEFAULT now(),
    CHECK (used_quota <= granted_quota)
);

CREATE INDEX idx_subauth_tenant_member ON subscribe_auth(tenant_id, member_id, template_id);

COMMENT ON TABLE  subscribe_auth              IS '订阅消息授权计数; 原子消耗 used_quota<=granted_quota; subscribe_msg 通道发送前强制校验';
COMMENT ON COLUMN subscribe_auth.id           IS '授权记录自增主键';
COMMENT ON COLUMN subscribe_auth.tenant_id    IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN subscribe_auth.member_id    IS '会员ID(外键 -> members.id)';
COMMENT ON COLUMN subscribe_auth.template_id  IS '微信订阅消息模板id';
COMMENT ON COLUMN subscribe_auth.granted_quota IS '已授权次数(Grant 时累加)';
COMMENT ON COLUMN subscribe_auth.used_quota   IS '已消耗次数(Consume 时 +1), <= granted_quota';
COMMENT ON COLUMN subscribe_auth.granted_at   IS '最近一次授权时间(UTC)';


-- ============================================================
-- 7. unsubscribe_records : 退订记录(一会员一通道一条退订态)
--    channel: wework_group | wework_1v1 | subscribe_msg | sms
--    UNIQUE(tenant_id, member_id, channel): 退订后重订阅走 UPDATE 清态(不新增行)。
--    INDEX(tenant_id, member_id): IsUnsubscribed 高频点查。
--    无 updated_at, 不挂触发器。
-- ============================================================
CREATE TABLE unsubscribe_records (
    id             BIGSERIAL    PRIMARY KEY,
    tenant_id      BIGINT       NOT NULL REFERENCES tenants(id),
    member_id      BIGINT       NOT NULL REFERENCES members(id),
    channel        TEXT         NOT NULL
                   CHECK (channel IN ('wework_group','wework_1v1','subscribe_msg','sms')),
    reason         TEXT,                                        -- 退订原因(可空)
    unsubscribed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (tenant_id, member_id, channel)
);

CREATE INDEX idx_unsub_tenant_member ON unsubscribe_records(tenant_id, member_id);

COMMENT ON TABLE  unsubscribe_records              IS '退订记录; 一会员一通道一条 UNIQUE(tenant_id,member_id,channel); 退订阻断后续发送, 重订阅 UPDATE 清态';
COMMENT ON COLUMN unsubscribe_records.id           IS '退订记录自增主键';
COMMENT ON COLUMN unsubscribe_records.tenant_id    IS '所属租户ID(外键 -> tenants.id), 租户行级隔离键';
COMMENT ON COLUMN unsubscribe_records.member_id    IS '会员ID(外键 -> members.id)';
COMMENT ON COLUMN unsubscribe_records.channel      IS '退订通道: wework_group | wework_1v1 | subscribe_msg | sms';
COMMENT ON COLUMN unsubscribe_records.reason       IS '退订原因, 可空';
COMMENT ON COLUMN unsubscribe_records.unsubscribed_at IS '退订时间(UTC); 重订阅时由应用层清态或更新';
