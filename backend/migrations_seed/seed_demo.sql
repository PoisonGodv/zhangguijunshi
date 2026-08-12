-- seed_demo.sql — 掌柜军师 admin dashboard 演示种子数据（路3）
--
-- 目的：为「冒烟租户」(tenants 第 1 行) 造一套可看的演示数据，让 admin-web
-- dashboard（GET /v1/boss/billing/summary/today + /:date）打开即有：营业额/订单数/
-- 实收/客单价 + 热销 TopProducts + 近7日趋势线；库存页有批次（含临期/低库存）、
-- 营销页有券/进行中拼团，让演示店更像真实三四线便利店。
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
-- 各实体 INSERT 用 SEED- 前缀订单号/固定名字段走 ON CONFLICT 兜底，双保险。
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
  v_today    date;
  v_gb       bigint;
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

  -- 当地（CST）今天正午，作为 7 天窗口的基准时刻；v_today 供批次临期日锚定。
  v_base := (((now() AT TIME ZONE 'Asia/Shanghai')::date)::timestamp + make_interval(hours => 12))
            AT TIME ZONE 'Asia/Shanghai';
  v_today := (now() AT TIME ZONE 'Asia/Shanghai')::date;

  -- ① 一个演示门店（ON CONFLICT 走 idx_stores_tenant_name 部分唯一索引，重跑不报错）。
  INSERT INTO stores (tenant_id, name, business_type, province, city, district, address, status, active)
  VALUES (v_tenant, '演示门店（种子）', 'convenience', '四川省', '绵阳市', '涪城区', '临园路东段1号', 'active', true)
  ON CONFLICT (tenant_id, name) WHERE deleted_at IS NULL DO UPDATE SET name = EXCLUDED.name
  RETURNING id INTO v_store;

  -- ② 17 个商品，覆盖 水果/蔬菜/肉蛋/乳品/饮料/零食/日化 七类，毛利率约 20-45%。
  --    ON CONFLICT 走 name+unit 部分唯一索引，RETURNING 始终给 id。
  --    生鲜给 shelf_life_days（天），日化/饮料 NULL；临期品打 tags 让商品页有筛选数据。
  CREATE TEMP TABLE seed_prod (pid bigint, ord int) ON COMMIT DROP;
  WITH ins AS (
    INSERT INTO products (tenant_id, store_id, name, category, unit, cost_cents, price_cents, shelf_life_days, tags, status)
    VALUES
      -- 水果（4）
      (v_tenant, v_store, '红富士苹果',   '水果', '斤', 400, 799,  7,   ARRAY[]::TEXT[], 'active'),
      (v_tenant, v_store, '香蕉',         '水果', '斤', 250, 499,  3,   ARRAY[]::TEXT[], 'active'),
      (v_tenant, v_store, '巨峰葡萄',     '水果', '斤', 600, 1199, 5,   ARRAY[]::TEXT[], 'active'),
      (v_tenant, v_store, '麒麟西瓜',     '水果', '斤', 180, 399,  4,   ARRAY[]::TEXT[], 'active'),
      -- 蔬菜（2）
      (v_tenant, v_store, '大白菜',       '蔬菜', '斤', 150, 359,  5,   ARRAY[]::TEXT[], 'active'),
      (v_tenant, v_store, '西红柿',       '蔬菜', '斤', 280, 599,  4,   ARRAY['临期'],   'active'),
      -- 肉蛋（3）
      (v_tenant, v_store, '土鸡蛋30枚',   '肉蛋', '盒', 1500, 2599, 20,  ARRAY[]::TEXT[], 'active'),
      (v_tenant, v_store, '五花肉',       '肉蛋', '斤', 1400, 2299, 2,   ARRAY[]::TEXT[], 'active'),
      (v_tenant, v_store, '鸡胸肉',       '肉蛋', '斤', 800, 1499, 2,   ARRAY[]::TEXT[], 'active'),
      -- 乳品（2）
      (v_tenant, v_store, '纯牛奶250ml*12','乳品','箱', 3200, 4999, 180, ARRAY[]::TEXT[], 'active'),
      (v_tenant, v_store, '鲜牛奶950ml',  '乳品', '瓶', 750, 1299, 5,    ARRAY['临期'],   'active'),
      -- 饮料（2）
      (v_tenant, v_store, '可乐330ml*6',  '饮料', '组', 900, 1599, 270,  ARRAY[]::TEXT[], 'active'),
      (v_tenant, v_store, '冰红茶500ml',  '饮料', '瓶', 220, 400,  300,  ARRAY[]::TEXT[], 'active'),
      -- 零食（2）
      (v_tenant, v_store, '乐事薯片75g',  '零食', '袋', 400, 650,  240,  ARRAY[]::TEXT[], 'active'),
      (v_tenant, v_store, '奥利奥97g',    '零食', '盒', 380, 699,  300,  ARRAY[]::TEXT[], 'active'),
      -- 日化（2）
      (v_tenant, v_store, '蓝月亮洗衣液1kg','日化','瓶', 1500, 2499, NULL, ARRAY[]::TEXT[], 'active'),
      (v_tenant, v_store, '抽纸3包装',    '日化', '提', 600, 1099, NULL, ARRAY[]::TEXT[], 'active')
    ON CONFLICT (tenant_id, store_id, name, unit) WHERE deleted_at IS NULL
      DO UPDATE SET name = EXCLUDED.name
    RETURNING id, name
  )
  INSERT INTO seed_prod (pid, ord)
  SELECT id, CASE name
    WHEN '红富士苹果'    THEN 1  WHEN '香蕉'         THEN 2  WHEN '巨峰葡萄'     THEN 3
    WHEN '麒麟西瓜'      THEN 4  WHEN '大白菜'       THEN 5  WHEN '西红柿'       THEN 6
    WHEN '土鸡蛋30枚'    THEN 7  WHEN '五花肉'       THEN 8  WHEN '鸡胸肉'       THEN 9
    WHEN '纯牛奶250ml*12' THEN 10 WHEN '鲜牛奶950ml' THEN 11 WHEN '可乐330ml*6'  THEN 12
    WHEN '冰红茶500ml'   THEN 13 WHEN '乐事薯片75g'  THEN 14 WHEN '奥利奥97g'    THEN 15
    WHEN '蓝月亮洗衣液1kg' THEN 16 WHEN '抽纸3包装'  THEN 17 END
  FROM ins;

  -- ②b 库存批次：每商品一个正常批次；个别商品加第二批次制造「临期/低库存」感。
  --    列：tenant_id, store_id, product_id, quantity, in_price_cents,
  --        expiry_date, expiry_warn_date, status（active|nearing|expired|cleared）。
  --    - 香蕉/西红柿/鲜牛奶 第二批次设为临近到期(status='nearing', 到期日+2~3天, 余量低)；
  --    - 五花肉/冰红茶 数量压到个位数制造「库存紧张」；
  --    - 生鲜批次都带 expiry_date，日化/饮料留 NULL。
  CREATE TEMP TABLE seed_batch (bid bigint, ord int, is_primary boolean) ON COMMIT DROP;
  WITH b AS (
    INSERT INTO inventory_batches (tenant_id, store_id, product_id, quantity, in_price_cents, expiry_date, expiry_warn_date, status)
    SELECT v_tenant, v_store, p.pid,
           -- 余量：紧张品给个位数，其余按品类给真实量级
           CASE p.ord
             WHEN 8  THEN 6    -- 五花肉 库存紧张
             WHEN 13 THEN 8    -- 冰红茶 库存紧张
             WHEN 2  THEN 40 WHEN 6  THEN 25 WHEN 11 THEN 18   -- 生鲜中等
             WHEN 1  THEN 120 WHEN 5  THEN 80 WHEN 7  THEN 60
             ELSE 999                                    -- 标品/日化充足
           END,
           (SELECT cost_cents FROM products WHERE id = p.pid),
           -- 生鲜批次给到期日（按 shelf_life 量级），其余 NULL
           CASE WHEN (SELECT shelf_life_days FROM products WHERE id = p.pid) IS NOT NULL
                THEN v_today + ((SELECT shelf_life_days FROM products WHERE id = p.pid) / 2)
                ELSE NULL END,
           CASE WHEN (SELECT shelf_life_days FROM products WHERE id = p.pid) IS NOT NULL
                THEN v_today + ((SELECT shelf_life_days FROM products WHERE id = p.pid) / 2) - 1
                ELSE NULL END,
           'active'
    FROM seed_prod p
    RETURNING id, product_id
  )
  INSERT INTO seed_batch (bid, ord, is_primary)
  SELECT b.id, sp.ord, true FROM b JOIN seed_prod sp ON sp.pid = b.product_id;

  -- 临期/低库存第二批次（香蕉2/西红柿6/鲜牛奶11），到期日锚定 v_today+2~3 天。
  INSERT INTO inventory_batches (tenant_id, store_id, product_id, quantity, in_price_cents, expiry_date, expiry_warn_date, status)
  SELECT v_tenant, v_store, sp.pid,
         CASE sp.ord WHEN 2 THEN 12 WHEN 6 THEN 9 WHEN 11 THEN 7 END,
         (SELECT cost_cents FROM products WHERE id = sp.pid),
         CASE sp.ord WHEN 2 THEN v_today + 2 WHEN 6 THEN v_today + 2 WHEN 11 THEN v_today + 3 END,
         CASE sp.ord WHEN 2 THEN v_today + 1 WHEN 6 THEN v_today + 1 WHEN 11 THEN v_today + 2 END,
         'nearing'
  FROM seed_prod sp
  WHERE sp.ord IN (2, 6, 11);

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

  -- ④ 营销数据：3 张券（满减/折扣/代金）+ 1 个进行中的拼团。
  --    coupons 列：tenant_id, store_id, name, type, params(jsonb), valid_from, valid_to,
  --                total_quota, issued, status；name 租户内唯一(未删除)。
  INSERT INTO coupons (tenant_id, store_id, name, type, params, valid_from, valid_to, total_quota, issued, status)
  VALUES
    (v_tenant, v_store, '满30减5',       'full_reduction',
       '{"threshold_cents":3000,"off_cents":500}'::jsonb,
       v_base - make_interval(days => 6), v_base + make_interval(days => 30), 200, 18, 'active'),
    (v_tenant, v_store, '全场9折券',     'discount',
       '{"discount_permille":900}'::jsonb,
       v_base - make_interval(days => 3), v_base + make_interval(days => 20), 100, 7,  'active'),
    (v_tenant, v_store, '新人3元代金券', 'cash',
       '{"off_cents":300}'::jsonb,
       v_base - make_interval(days => 10), v_base + make_interval(days => 60), NULL, 25, 'active')
  ON CONFLICT (tenant_id, name) WHERE deleted_at IS NULL DO UPDATE SET name = EXCLUDED.name;

  -- 拼团：进行中(active)，3人成团已来2人，关联土鸡蛋商品；
  -- 带 B2 加的 description / original_price_cents（原价 vs 团购价划线对比）。
  -- groupbuys 无唯一索引，用 NOT EXISTS 兜底幂等（外层 SEED 闸门已挡，双保险）。
  INSERT INTO groupbuys (tenant_id, store_id, product_id, title, description, params, required_count, joined, status, start_at, end_at, original_price_cents)
  SELECT v_tenant, v_store,
         (SELECT pid FROM seed_prod WHERE ord = 7),
         '土鸡蛋30枚 3人拼团',
         '3人成团立享拼团价，原价25.99元拼团只要19.99元，散养土鸡蛋，新鲜直达。',
         '{"group_size":3,"price_cents":1999,"valid_hours":48}'::jsonb,
         3, 2, 'active',
         v_base - make_interval(days => 1), v_base + make_interval(days => 2),
         2599
  WHERE NOT EXISTS (
    SELECT 1 FROM groupbuys g WHERE g.tenant_id = v_tenant AND g.title = '土鸡蛋30枚 3人拼团'
  )
  RETURNING id INTO v_gb;

  -- 拼团参团记录（2 人已参，对应 joined=2）；UNIQUE(tenant,groupbuy,member) 兜底。
  IF v_gb IS NOT NULL THEN
    INSERT INTO groupbuy_participants (tenant_id, groupbuy_id, member_id, status, joined_at)
    SELECT v_tenant, v_gb, (SELECT mid FROM seed_mem WHERE ord = x), 'paid',
           v_base - make_interval(hours => 20)
    FROM (VALUES (3), (5)) AS t(x)
    ON CONFLICT (tenant_id, groupbuy_id, member_id) DO NOTHING;
  END IF;

  -- ⑤ 覆盖最近 7 天的订单 + 明细（扩到 ~30 单，平日3-4单、周末5-6单有起伏）。
  --    plan(day_offset 0=今天 .. 6=六天前, seq, product_ord, qty)。
  --    day_offset 0=今天 .. 6=六天前；今天/昨天视作周末(单多)，其余平日(单少)，金额随之起伏。
  --    含高频热销品（苹果/香蕉/鸡蛋/牛奶）让 TopProducts 有意义，外加日化/零食长单子。
  CREATE TEMP TABLE seed_plan (day_off int, seq int, prod_ord int, qty int) ON COMMIT DROP;
  INSERT INTO seed_plan VALUES
    -- 今天（周末，6 单，高）
    (0,1,1,3),(0,2,7,1),(0,3,2,2),(0,4,10,1),(0,5,12,2),(0,6,17,1),
    -- 昨天（周末，5 单）
    (1,1,10,1),(1,2,1,2),(1,3,5,4),(1,4,14,3),(1,5,3,2),
    -- 前天（平日，4 单）
    (2,1,2,3),(2,2,8,1),(2,3,3,2),(2,4,16,1),
    -- 3天前（平日，4 单）
    (3,1,12,2),(3,2,1,4),(3,3,7,1),(3,4,11,1),
    -- 4天前（平日，4 单）
    (4,1,3,1),(4,2,2,2),(4,3,10,1),(4,4,13,3),
    -- 5天前（平日，3 单）
    (5,1,1,2),(5,2,5,3),(5,3,8,1),
    -- 6天前（平日，4 单）
    (6,1,7,2),(6,2,12,1),(6,3,2,1),(6,4,15,2);

  -- 每单金额 = Σ qty*price；订单号 SEED-<dayoff>-<seq> 唯一可重放。
  CREATE TEMP TABLE seed_order_amt (day_off int, seq int, amount bigint) ON COMMIT DROP;
  INSERT INTO seed_order_amt
  SELECT pl.day_off, pl.seq, SUM(pl.qty * pr.price_cents)
  FROM seed_plan pl JOIN products pr ON pr.id = (SELECT pid FROM seed_prod WHERE ord = pl.prod_ord)
  GROUP BY pl.day_off, pl.seq;

  -- 插订单（status='paid', total==paid, created_at=当地正午偏移），拿回 (day_off,seq)->oid。
  -- payment_method 混合 wxpay/cash 更真实（订单表无 CHECK，自由文本）。
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
           src.amount, src.amount, 0,
           CASE WHEN (src.seq % 3) = 0 THEN 'cash' ELSE 'wxpay' END,
           v_base - make_interval(days => src.day_off),
           v_base - make_interval(days => src.day_off),
           v_base - make_interval(days => src.day_off)
    FROM src
    RETURNING id, order_no
  )
  INSERT INTO seed_order_id (day_off, seq, oid)
  SELECT split_part(order_no, '-', 2)::int, split_part(order_no, '-', 3)::int, id
  FROM ins;

  -- 插明细：name/unit/price/cost 从商品快照，batch_id 挂该商品主批次（is_primary=true 的正常批次）。
  INSERT INTO order_items (tenant_id, order_id, product_id, batch_id, name_snapshot, unit, qty, price_cents, cost_cents)
  SELECT v_tenant,
         (SELECT oid FROM seed_order_id o WHERE o.day_off = pl.day_off AND o.seq = pl.seq),
         pr.id,
         (SELECT sb.bid FROM seed_batch sb WHERE sb.ord = pl.prod_ord AND sb.is_primary LIMIT 1),
         pr.name, pr.unit, pl.qty, pr.price_cents, pr.cost_cents
  FROM seed_plan pl
  JOIN products pr ON pr.id = (SELECT pid FROM seed_prod WHERE ord = pl.prod_ord);

  RAISE NOTICE 'seed_demo: 完成。tenant=% store=% products=17 members=12 orders=30 batches=20 coupons=3 groupbuy=%',
    v_tenant, v_store, COALESCE(v_gb::text, 'skip(existed)');
END $$;

COMMIT;
