import { request } from './client'

export interface Order {
  id: string; buyer_id: string; seller_id: string; product_id: string; price_cent: string
  status: string; buyer_confirmed?: boolean; seller_confirmed?: boolean; aggregate_version: string
  expires_at: string; closed_at?: string; created_at: string; updated_at?: string
}
export type OrderRole = 'buyer' | 'seller'
export const orderLabels: Record<string, string> = {
  ORDER_STATUS_PENDING: '下单处理中', ORDER_STATUS_RESERVED: '待双方确认', ORDER_STATUS_CONFIRMING: '成交处理中',
  ORDER_STATUS_CANCELLING: '取消处理中', ORDER_STATUS_EXPIRING: '超时关闭中', ORDER_STATUS_COMPLETED: '交易完成',
  ORDER_STATUS_CANCELLED: '已取消', ORDER_STATUS_EXPIRED: '已过期', ORDER_STATUS_FAILED: '下单失败',
}
// processing 判断是否需要继续查询最终结果，不能把已受理当作已成交。
export function processing(order: Order): boolean { return ['ORDER_STATUS_PENDING', 'ORDER_STATUS_CONFIRMING', 'ORDER_STATUS_CANCELLING', 'ORDER_STATUS_EXPIRING'].includes(order.status) }
// listOrders 仅查询当前用户的买入或卖出订单，身份由服务端验证。
export function listOrders(role: OrderRole, page: number, token: string, signal: AbortSignal) { return request<{ orders?: Order[]; total?: string }>(`/orders?role=${role}&page=${page}&page_size=12`, { token, signal }) }
// getOrder 读取最新状态，所有 ID 始终按字符串传递。
export function getOrder(id: string, token: string, signal: AbortSignal) { return request<{ order: Order }>(`/orders/${encodeURIComponent(id)}`, { token, signal }) }
// createOrder 复用当前结算意图的幂等键；取消或超时后不得自动生成新键重试。
export function createOrder(productId: string, key: string, token: string, signal: AbortSignal) { return request<{ order: Order }>('/orders', { method: 'POST', token, signal, idempotencyKey: key, body: { product_id: productId } }) }
// decideOrder 发送空请求体，不把客户端身份或状态作为业务参数传给后端。
export function decideOrder(id: string, action: 'cancel' | 'confirm', token: string, signal: AbortSignal) { return request<{ order: Order }>(`/orders/${encodeURIComponent(id)}/${action}`, { method: 'POST', token, signal }) }

// checkoutKey 发请求前保存结算意图，网络未知、关闭对话框或刷新后仍返回同一键，按用户和商品隔离。
export function checkoutKey(userId: string, productId: string): string {
  try {
    const storageKey = `remarket.checkout.v1.${userId}.${productId}`
    const key = sessionStorage.getItem(storageKey) || crypto.randomUUID()
    sessionStorage.setItem(storageKey, key)
    return key
  } catch { throw new Error('请允许此页面使用会话存储后再下单，以便网络中断后安全重试。') }
}
// forgetCheckout 取得真实订单后才清理当前意图；清理失败保留旧键依然不会创建第二笔订单。
export function forgetCheckout(userId: string, productId: string): void { try { sessionStorage.removeItem(`remarket.checkout.v1.${userId}.${productId}`) } catch { /* 留存旧键时仍安全复用原订单。 */ } }
