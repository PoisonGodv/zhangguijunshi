-- seed_demo.sql — 掌柜军师 admin dashboard 演示种子数据（路3）
--
-- 目的：为「冒烟租户」(tenants 第 1 行) 造一套可看的演示数据，让 admin-web
-- dashboard（GET /v1/boss/billing/summary/today + /:date）打开即有：营业额/订单数/
-- 实收/客单价 + 热销 TopProducts + 近7日趋势线。
--
-- 设计依据（已核实 internal/billing/service.go DailySummary + pg_repo.go ListByDate）：
--   * DailySummary 聚合 ListByDate 返回的订单：Σ TotalCents -> GrossCents，
--     Σ PaidCents -> NetCents，TopProducts 来自 order_items(Σ qty*price_cents)。
--   * ListByDate 的 SQL 只按 tenant_id + store_id + created_at 落在「day 自身时区的
--     自然日 [00:00, 次日00:00)」过滤，**不过滤 status**。因此本脚本只造 status='paid'
--     且 total_cents == paid_cents 的订单，保证营业额==实收、数字干净。
--   * 日期列是 created_at（不是 paid_at）。handler 用 time.Local 解析日期、time.Now()
--     取今天；backend 容器时区 = Asia/Shanghai(CST)，PG 存 UTC。故所有订单 created_at
--     锚定在「当地正午 12:00 Asia/Shanghai」，无论 UTC 如何存都落进正确的本地自然日。
--   * 日期用 (now() AT TIME ZONE 'Asia/Shanghai')::date - i 相对偏移，近7日趋势始终有数据，
--     绝不硬编码绝对日期。
--
-- 幂等：整个脚本包在一个事务里，开头先探测是否已存在 order_no LIKE 'SEED-%' 的订单；
-- 已种子过则 RAISE NOTICE 并 RETURN，什么都不插。重跑安全，不会翻倍。
-- 约束遵守：只 INSERT 演示数据；不 DROP/TRUNCATE、不改 schema、不碰 migrations/；
-- tenant_id 一律用子查询 (SELECT id FROM tenants ORDER BY id LIMIT 1)，不硬编码。
--
-- 跑法见 docs/runbooks/seed-demo.md。这是演示数据，生产慎用。

BEGIN;

DO $$
DECLARE
  v_tenant   bigint;
  v_store    bigint;
  v_existing bigint;
  v_base     timestamp with time zone;
BEGIN
  -- 取第一个租户作为种子 tenant（别新建 tenant）。
  SELECT id INTO v_tenant FROM tenants ORDER BY id LIMIT 1;
  IF v_tenant IS NULL THEN
    RAISE EXCEPTION 'seed_demo: tenants 表为空，无法种子（先跑 migrate 建租户）';
  END IF;

  -- 幂等闸门：已种子过就整体跳过。
  SELECT count(*) INTO v_existing FROM orders WHERE order_no LIKE 'SEED-%';
  IF v_existing > 0 THEN
    RAISE NOTICE 'seed_demo: 已存在 % 条 SEED 订单，跳过（幂等，不重复插入）', v_existing;
    RETURN;
  END IF;

  -- 当地（CST）今天正午，作为 7 天窗口的基准时刻。
  v_base := (((now() AT TIME ZONE 'Asia/Shanghai')::date)::timestamp + make_interval(hours => 12))
            AT TIME ZONE 'Asia/Shanghai';

  -- ① 一个演示门店（ON CONFLICT 走 idx_stores_tenant_name 部分唯一索引，重跑不报错）。
  INSERT INTO stores (tenant_id, name, business_type, province, city, district, address, status, active)
  VALUES (v_tenant, '演示门店（种子）', 'convenience', '四川省', '绵阳市', '涪城区', '临园路东段1号', 'active', true)
  ON CONFLICT (tenant_id, name) WHERE deleted_at IS NULL DO UPDATE SET name = EXCLUDED.name
  RETURNING id INTO v_store;

  -- ② 8 个商品（价格/成本/单位）。ON CONFLICT 走 name+unit 部分唯一索引，RETURNING 始终给 id。
  CREATE TEMP TABLE seed_prod (pid bigint, ord int) ON COMMIT DROP;
  WITH ins AS (
    INSERT INTO products (tenant_id, store_id, name, category, unit, cost_cents, price_cents, status)
    VALUES
      (v_tenant, v_store, '红富士苹果',   '水果', '斤', 400, 799,  'active'),
      (v_tenant, v_store, '香蕉',         '水果', '斤', 250, 499,  'active'),
      (v_tenant, v_store, '巨峰葡萄',     '水果', '斤', 600, 1199, 'active'),
      (v_tenant, v_store, '土鸡蛋30枚',   '蛋品', '盒', 1500, 2599,'active'),
      (v_tenant, v_store, '纯牛奶250ml*12','乳品','箱', 3200, 4999,'active'),
      (v_tenant, v_store, '五花肉',       '肉类', '斤', 1400, 2299,'active'),
      (v_tenant, v_store, '大白菜',       '蔬菜', '斤', 150, 359,  'active'),
      (v_tenant, v_store, '可乐330ml*6',  '饮料', '组', 900, 1599, 'active')
    ON CONFLICT (tenant_id, store_id, name, unit) WHERE deleted_at IS NULL
      DO UPDATE SET name = EXCLUDED.name
    RETURNING id, name
  )
  INSERT INTO seed_prod (pid, ord)
  SELECT id, CASE name
    WHEN '红富士苹果'    THEN 1 WHEN '香蕉'       THEN 2 WHEN '巨峰葡萄'   THEN 3
    WHEN '土鸡蛋30枚'    THEN 4 WHEN '纯牛奶250ml*12' THEN 5 WHEN '五花肉'  THEN 6
    WHEN '大白菜'        THEN 7 WHEN '可乐330ml*6' THEN 8 END
  FROM ins;

  -- ②b 每个商品一个库存批次（让库存页也有货；order_items.batch_id 可选，这里挂上更真实）。
  INSERT INTO inventory_batches (tenant_id, store_id, product_id, quantity, in_price_cents, status)
  SELECT v_tenant, v_store, p.pid, 999,
         (SELECT cost_cents FROM products WHERE id = p.pid), 'active'
  FROM seed_prod p;

  -- ③ 12 个会员（phone_hash / wx_openid 用 SEED 前缀保证唯一且可重放）。
  CREATE TEMP TABLE seed_mem (mid bigint, ord int) ON COMMIT DROP;
  WITH m AS (
    INSERT INTO members (tenant_id, store_id, name, level, points, total_spent_cents, last_visit_at, status, phone_hash, wx_openid)
    SELECT v_tenant, v_store,
           '种子会员' || g,
           CASE WHEN g <= 2 THEN 'vip' ELSE 'normal' END,
           g * 37,
           g * 12345,
           v_base - make_interval(days => (g % 7)),
           'active',
           'seed_phone_hash_' || g,
           'seed_openid_' || g
    FROM generate_series(1, 12) g
    RETURNING id
  )
  INSERT INTO seed_mem (mid, ord)
  SELECT id, row_number() OVER (ORDER BY id) FROM m;

  -- ④ 覆盖最近 7 天的订单 + 明细。
  --    plan(day_offset 0=今天 .. 6=六天前, seq, product_ord, qty)。每天 3 单、金额有起伏，
  --    含几个高频热销品（苹果/香蕉/鸡蛋）让 TopProducts 有意义。
  CREATE TEMP TABLE seed_plan (day_off int, seq int, prod_ord int, qty int) ON COMMIT DROP;
  INSERT INTO seed_plan VALUES
    (0,1,1,3),(0,2,4,1),(0,3,2,2),
    (1,1,5,1),(1,2,1,2),(1,3,7,4),
    (2,1,2,3),(2,2,6,1),(2,3,3,2),
    (3,1,8,2),(3,2,1,4),(3,3,4,1),
    (4,1,3,1),(4,2,2,2),(4,3,5,1),
    (5,1,1,2),(5,2,7,3),(5,3,6,1),
    (6,1,4,2),(6,2,8,1),(6,3,2,1);

  -- 每单金额 = Σ qty*price；订单号 SEED-<dayoff>-<seq> 唯一可重放。
  CREATE TEMP TABLE seed_order_amt (day_off int, seq int, amount bigint) ON COMMIT DROP;
  INSERT INTO seed_order_amt
  SELECT pl.day_off, pl.seq, SUM(pl.qty * pr.price_cents)
  FROM seed_plan pl JOIN products pr ON pr.id = (SELECT pid FROM seed_prod WHERE ord = pl.prod_ord)
  GROUP BY pl.day_off, pl.seq;

  -- 插订单（status='paid', total==paid, created_at=当地正午偏移），拿回 (day_off,seq)->oid。
  CREATE TEMP TABLE seed_order_id (day_off int, seq int, oid bigint) ON COMMIT DROP;
  WITH src AS (
    SELECT a.day_off, a.seq, a.amount,
           -- 每单挂一个会员（散客也可，这里都挂会员让会员消费统计有数据）
           (SELECT mid FROM seed_mem WHERE ord = ((a.day_off * 3 + a.seq) % 12) + 1 LIMIT 1) AS member_id
    FROM seed_order_amt a
  ),
  ins AS (
    INSERT INTO orders
      (tenant_id, store_id, member_id, order_no, channel, status,
       total_cents, paid_cents, discount_cents, payment_method,
       created_at, paid_at, updated_at)
    SELECT v_tenant, v_store, src.member_id,
           'SEED-' || src.day_off || '-' || src.seq,
           'in_store', 'paid',
           src.amount, src.amount, 0, 'wechat',
           v_base - make_interval(days => src.day_off),
           v_base - make_interval(days => src.day_off),
           v_base - make_interval(days => src.day_off)
    FROM src
    RETURNING id, order_no
  )
  INSERT INTO seed_order_id (day_off, seq, oid)
  SELECT split_part(order_no, '-', 2)::int, split_part(order_no, '-', 3)::int, id
  FROM ins;

  -- 插明细：name/unit/price/cost 从商品快照，batch_id 挂该商品批次。
  INSERT INTO order_items (tenant_id, order_id, product_id, batch_id, name_snapshot, unit, qty, price_cents, cost_cents)
  SELECT v_tenant,
         (SELECT oid FROM seed_order_id o WHERE o.day_off = pl.day_off AND o.seq = pl.seq),
         pr.id,
         (SELECT id FROM inventory_batches b WHERE b.product_id = pr.id AND b.tenant_id = v_tenant LIMIT 1),
         pr.name, pr.unit, pl.qty, pr.price_cents, pr.cost_cents
  FROM seed_plan pl
  JOIN products pr ON pr.id = (SELECT pid FROM seed_prod WHERE ord = pl.prod_ord);

  RAISE NOTICE 'seed_demo: 完成。tenant=% store=% products=8 members=12 orders=21 items=21 batches=8',
    v_tenant, v_store;
END $$;

COMMIT;
