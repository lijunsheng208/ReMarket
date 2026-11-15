-- Feed 基础查询按创建时间稳定排序；执行前评估大表在线建索引窗口。
ALTER TABLE products ADD KEY idx_products_feed (status, created_at, id);
