# 我的商品分页

GET /api/v1/products/mine?page=1&page_size=12&category_id=1，需要 Bearer Token。

Gateway 调用新增 ListMyProducts RPC，卖家 ID 从 Product 服务验签后的 Context 获取，请求不能指定或覆盖卖家。管理员也只查询自己的商品。返回当前卖家的所有状态商品，包括草稿、在售、预占、成交和下架。

分页规则与市场列表一致：page=0 默认1，page_size=0 默认20，最大100；负值拒绝，category_id=0 不过滤。总数和列表使用同一 MySQL 只读快照，按 created_at DESC、id DESC 排序。ID、金额、版本与 total 均按 Proto JSON 字符串返回。

商品管理页改为调用此接口并展示分页，不再依赖 localStorage 记录。已有商品无需迁移或重新创建，登录同一账号即可查询。商品编号查找保留，用于直接查看详情。

真实 MySQL 双库 HTTP/gRPC race 联调通过，覆盖匿名拒绝、草稿可见、分页与总数、管理员不能越权读取其他账号、非法分页及原商品生命周期；测试库已清理，业务库未修改。
