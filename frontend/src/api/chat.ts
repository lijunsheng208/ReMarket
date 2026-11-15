import { request } from './client'

export interface Conversation {
  id: string; product_id: string; buyer_id: string; seller_id: string
  last_seq?: string; last_message_id?: string; last_message_at?: string; created_at?: string
  last_read_seq?: string; unread_count?: string; last_message_content?: string
}
export interface ChatMessage {
  id: string; conversation_id: string; sender_id: string; client_msg_id: string
  seq: string; content: string; message_type?: string; created_at?: string
}
export interface MessagePage { messages?: ChatMessage[]; has_more?: boolean; next_seq?: string; sync_until_seq?: string }
export interface ReadState { conversation_id: string; user_id: string; last_read_seq?: string; unread_count?: string; last_seq?: string }
export interface ConversationPage { conversations?: Conversation[]; next_cursor?: string }
export type ChatRequest = typeof request

// chatPath 始终按十进制字符串编码实体 ID，不转换为 JavaScript Number。
export function chatPath(id: string) { return `/conversations/${encodeURIComponent(id)}` }
// createConversation 只提交商品 ID，参与者由后端从可信身份和商品信息决定。
export function createConversation(productId: string, token: string, signal?: AbortSignal) { return request<Conversation>('/conversations', { method: 'POST', body: { product_id: productId }, token, signal }) }
// socketURL 仅把短期一次性票据放入同源 WebSocket URL，不传长期 JWT。
export function socketURL(origin: string, ticket: string) { const url = new URL('/api/v1/chat/ws', origin); url.protocol = url.protocol === 'https:' ? 'wss:' : 'ws:'; url.searchParams.set('ticket', ticket); return url.toString() }
