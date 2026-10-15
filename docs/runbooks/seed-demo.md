# Runbook：admin dashboard 演示种子数据（seed_demo）

> ⚠️ **这是演示数据，生产慎用。** 脚本只 `INSERT`，不 `DROP`/`TRUNCATE`、不改 schema、
> 不碰 `migrations/`（那是 golang-migrate 管的）。它往「第一个 tenant」里造一套可看的
> 门店/商品/会员/订单，让 admin-web dashboard 打开即有营业额、订单数、热销榜、近7日趋势。

## 它造了什么

| 对象 | 量级 | 说明 |
|---|---|---|
| store | 1 | 「演示门店（种子）」，convenience |
| product | 8 | 有价格/成本/单位（分 cents） |
| inventory_batch | 8 | 每商品一个批次（quantity 999），让库存页也有货 |
| member | 12 | 2 个 vip + 10 个 normal，phone_hash/wx_openid 用 `seed_` 前缀 |
| order | 21 | 覆盖**最近 7 天**、每天 3 单，金额有起伏 |
| order_item | 21 | 每单 1 个明细，含几个高频热销品（苹果/香蕉/鸡蛋） |

订单全部 `status='paid'` 且 `total_cents == paid_cents`，`created_at` 锚在**当地正午
12:00（Asia/Shanghai）**，所以无论 PG 以 UTC 存储都落进正确的本地自然日。日期用
`now() AT TIME ZONE 'Asia/Shanghai'` 相对偏移，**不硬编码绝对日期** —— 任何时候重跑，
「近7日趋势」始终有数据。

## 前置

- prod PG 容器在跑：`zg-postgres-prod`（库 `zhanggui`，用户 `zg`）。
- 已跑过 migrate，`tenants` 至少有 1 行（脚本取 `SELECT id FROM tenants ORDER BY id LIMIT 1`，
  不新建 tenant）。

## 怎么跑

仓库根目录下：

```bash
# 方式 A：直接管道喂进去（推荐）
docker exec -i zg-postgres-prod psql -U zg -d zhanggui -v ON_ERROR_STOP=1 \
  < backend/migrations_seed/seed_demo.sql

# 方式 B：先 docker cp 再跑
docker cp backend/migrations_seed/seed_demo.sql zg-postgres-prod:/tmp/seed_demo.sql
docker exec zg-postgres-prod psql -U zg -d zhanggui -v ON_ERROR_STOP=1 -f /tmp/seed_demo.sql
```

成功输出（首次）：

```
NOTICE:  seed_demo: 完成。tenant=1 store=4 products=8 members=12 orders=21 items=21 batches=8
```

**幂等**：重跑会打印 `已存在 N 条 SEED 订单，跳过`，什么都不插，不会翻倍。

## 怎么验证

### 1. 行数非 0

```bash
docker exec zg-postgres-prod psql -U zg -d zhanggui -c "
SELECT (SELECT count(*) FROM orders) orders,
       (SELECT count(*) FROM order_items) items,
       (SELECT count(*) FROM members) members,
       (SELECT count(*) FROM products) products,
       (SELECT count(*) FROM stores) stores;"
```

期望：orders=21、items=21、members=12、products=8、stores≥2（含原有「冒烟门店」）。

### 2. 复刻 billing 聚合（近7日，按 CST 自然日）

```bash
docker exec zg-postgres-prod psql -U zg -d zhanggui -c "
WITH days AS (SELECT generate_series(6,0,-1) off)
SELECT ((now() AT TIME ZONE 'Asia/Shanghai')::date - d.off) day_cst,
       count(DISTINCT o.id) orders, sum(o.total_cents) gross_cents
FROM days d LEFT JOIN orders o
  ON o.tenant_id=1 AND o.status='paid'
 AND o.created_at >= ((((now() AT TIME ZONE 'Asia/Shanghai')::date - d.off)::timestamp) AT TIME ZONE 'Asia/Shanghai')
 AND o.created_at <  ((((now() AT TIME ZONE 'Asia/Shanghai')::date - d.off + 1)::timestamp) AT TIME ZONE 'Asia/Shanghai')
GROUP BY d.off ORDER BY d.off;"
```

期望：7 行、每天 orders=3、gross_cents 有起伏。

### 3. 直接打 dashboard 用的接口（最直观）

backend 容器 `zg-backend-prod` 映射到宿主 `:18080`。当前 prod 配置
`multitenancy.dev_mock_header=false`，但本机这套 compose 的 backend 实测仍接受
`X-Mock-Tenant` 头（若你的环境已严格关 mock 头，改用真 boss JWT + tenant，或退回到上面第 2 步）：

```bash
# 今日
curl -s "http://localhost:18080/v1/boss/billing/summary/today" -H "X-Mock-Tenant: 1"

# 指定某天
curl -s "http://localhost:18080/v1/boss/billing/summary/$(date +%F)" -H "X-Mock-Tenant: 1"
```

期望 `data.OrderCount>0`、`GrossCents>0`、`TopProducts` 非空。然后打开 admin-web
（`http://localhost:8088`）dashboard 页确认条形图 + 趋势线有数据。

## 怎么清（重置）

脚本没有提供反向 migration（演示数据不该混进迁移链）。要清就按 `order_no LIKE 'SEED-%'`
精确删，**只删种子、不碰真实数据**：

```bash
docker exec zg-postgres-prod psql -U zg -d zhanggui -v ON_ERROR_STOP=1 <<'SQL'
BEGIN;
DELETE FROM order_items WHERE tenant_id = (SELECT id FROM tenants ORDER BY id LIMIT 1)
  AND order_id IN (SELECT id FROM orders WHERE order_no LIKE 'SEED-%');
DELETE FROM orders WHERE order_no LIKE 'SEED-%';
DELETE FROM inventory_batches WHERE product_id IN
  (SELECT id FROM products WHERE name IN ('红富士苹果','香蕉','巨峰葡萄','土鸡蛋30枚','纯牛奶250ml*12','五花肉','大白菜','可乐330ml*6'));
DELETE FROM members WHERE wx_openid LIKE 'seed_openid_%';
DELETE FROM products WHERE name IN ('红富士苹果','香蕉','巨峰葡萄','土鸡蛋30枚','纯牛奶250ml*12','五花肉','大白菜','可乐330ml*6');
DELETE FROM stores WHERE name = '演示门店（种子）';
COMMIT;
SQL
```

清完可重跑 `seed_demo.sql` 再生成一套。

## 注意

- 演示数据，**生产环境慎用**；真要上生产请先 review 金额/品类是否符合该租户实际。
- 脚本锚定「第一个 tenant」。多租户环境下确认那是你想填的租户，否则改子查询条件。
- `created_at` 用 Asia/Shanghai 正午，与 backend 容器时区一致；若 backend 时区改变，
  「今天」的归属可能偏移，需相应调整 `make_interval`。
