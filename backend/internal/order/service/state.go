// Package service 定义订单业务规则，不依赖 HTTP、gRPC 或数据库实现。
package service

import "remarket/backend/internal/order/model"

// CanTransition 仅检查状态边是否合法，不执行转换；调用方仍须在本地事务中
// 校验身份、截止时间、双方确认和远程结果，原子更新版本及操作记录和 Outbox。
// RESERVED 自环仅用于一方首次确认；重复请求应直接返回既有结果。
func CanTransition(from, to model.OrderStatus) bool {
	switch from {
	case model.StatusPending:
		return to == model.StatusReserved || to == model.StatusFailed
	case model.StatusReserved:
		return to == model.StatusReserved || to == model.StatusConfirming || to == model.StatusCancelling || to == model.StatusExpiring
	case model.StatusConfirming:
		return to == model.StatusCompleted
	case model.StatusCancelling:
		return to == model.StatusCancelled
	case model.StatusExpiring:
		return to == model.StatusExpired
	default:
		return false
	}
}
