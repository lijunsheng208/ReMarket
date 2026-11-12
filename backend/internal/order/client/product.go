// Package client 将下游 Product RPC 转换为 Order 自有业务接口。
package client

import (
	"context"
	"errors"
	productv1 "remarket/backend/gen/product/v1"
	"remarket/backend/internal/order/service"
)

type ProductReader struct {
	products productv1.ProductServiceClient
}

// NewProductReader 注入商品 RPC Client，连接生命周期由入口负责。
func NewProductReader(products productv1.ProductServiceClient) *ProductReader {
	return &ProductReader{products: products}
}

// GetForOrder 使用上游派生 Context 读取可信商品快照，后续预占仍须条件校验。
func (r *ProductReader) GetForOrder(ctx context.Context, id int64) (*service.ProductSnapshot, error) {
	result, err := r.products.GetProduct(ctx, &productv1.GetProductRequest{Id: id})
	if err != nil {
		return nil, err
	}
	if result == nil || result.Product == nil {
		return nil, errors.New("商品 RPC 响应无效")
	}
	product := result.Product
	return &service.ProductSnapshot{ID: product.Id, SellerID: product.SellerId, PriceCent: product.PriceCent, Version: product.AggregateVersion, OnSale: product.Status == productv1.ProductStatus_PRODUCT_STATUS_ON_SALE}, nil
}
