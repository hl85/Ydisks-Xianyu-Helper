-- +goose Up
-- 拼团小刀自动免拼的商品名单：卖家把需要「买家下刀单后系统直接免拼发货」的
-- 商品 ID 登记在这里。订单同步发现带 SKIP_PIN（直接免拼）按钮的订单时，若其
-- 商品在名单内则调用免拼接口，实现买家付款后无需人工点击「直接免拼」。
-- 与登录凭证无关的运营配置；cookie_id 对应 cookies.id。
CREATE TABLE skip_pin_items (
    cookie_id TEXT NOT NULL,
    item_id TEXT NOT NULL,
    enabled INTEGER NOT NULL DEFAULT 1,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (cookie_id, item_id)
);

-- +goose Down
DROP TABLE IF EXISTS skip_pin_items;
