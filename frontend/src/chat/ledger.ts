import type { ChatMessage } from '../api/chat'

// decimal 验证服务端 BIGINT 字符串，不接受已经损失精度的 Number。
export function decimal(value: unknown, zero = true): value is string { return typeof value === 'string' && /^(0|[1-9]\d*)$/.test(value) && BigInt(value) <= 9223372036854775807n && (zero || value !== '0') }
// compareSeq 精确比较超出 Number 安全范围的序号。
export function compareSeq(a: string, b: string) { return BigInt(a) < BigInt(b) ? -1 : BigInt(a) > BigInt(b) ? 1 : 0 }
// maximum 返回单调游标，不因迟到通知回退。
export function maximum(a: string, b: string) { return compareSeq(a, b) < 0 ? b : a }
// validMessage 校验进入消息账本的最小协议，不将未知消息当作已发送。
export function validMessage(value: unknown): value is ChatMessage {
  if (!value || typeof value !== 'object') return false
  const m = value as ChatMessage
  return decimal(m.id, false) && decimal(m.seq, false) && decimal(m.conversation_id, false) && decimal(m.sender_id, false) && typeof m.content === 'string' && typeof m.client_msg_id === 'string'
}

// Ledger 在最近历史窗口基线之上推进连续位置，乱序推送不能跳过缺口。
export class Ledger {
  messages = new Map<string, ChatMessage>()
  private ids = new Map<string,string>()
  cursor = '0'
  initialized = false
  oldest = '0'
  hasOlder = false

  // put 按会话序号去重；同一序号出现不同消息 ID 时拒绝污染账本。
  put(message: ChatMessage) {
    if (!validMessage(message)) throw new Error('消息格式无效，请刷新会话。')
    if (this.ids.has(message.id) && this.ids.get(message.id)!==message.seq) throw new Error('消息顺序异常，请重新加载会话。')
    const old = this.messages.get(message.seq)
    if (old && old.id !== message.id) throw new Error('消息顺序异常，请重新加载会话。')
    this.ids.set(message.id,message.seq); this.messages.set(message.seq, message)
    if (this.initialized) this.advance()
  }
  // seed 以首次最近历史页建立明确基线；更早的内容仍可独立向前分页。
  seed(messages: ChatMessage[], more: boolean) {
    for (const message of messages) this.put(message)
    const sorted = [...messages].sort((a, b) => compareSeq(a.seq, b.seq))
    this.oldest = sorted[0]?.seq ?? '0'
    this.cursor = sorted.length ? (BigInt(sorted[0].seq) - 1n).toString() : '0'
    this.initialized = true; this.hasOlder = more; this.advance()
  }
  // advance 只跨越已经存在的连续消息，缺口由补拉恢复。
  advance() { while (this.messages.has((BigInt(this.cursor) + 1n).toString())) this.cursor = (BigInt(this.cursor) + 1n).toString() }
  // sorted 以真实序号排序，避免重复推送或 Number 精度造成错序。
  sorted() { return [...this.messages.values()].sort((a, b) => compareSeq(a.seq, b.seq)) }
}
