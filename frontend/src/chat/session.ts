import { ApiError, request } from '../api/client'
import { chatPath, socketURL } from '../api/chat'
import type { ChatMessage, ChatRequest, Conversation, ConversationPage, MessagePage, ReadState } from '../api/chat'
import { Ledger, compareSeq, decimal, maximum, validMessage } from './ledger'

export interface PendingMessage { client_msg_id: string; conversation_id: string; content: string; created_at: string; status: 'queued' | 'sending' | 'unknown' | 'failed'; error?: string; requestId?: string; sentAt?: number }
export interface ChatSnapshot {
  conversations: Conversation[]; nextCursor: string; selected: string; messages: ChatMessage[]
  pending: PendingMessage[]; status: 'connecting' | 'online' | 'reconnecting' | 'stopped'
  loading: boolean; syncing: boolean; hasOlder: boolean; error: string; peerRead: string
}
interface StorageLike { getItem(key: string): string | null; setItem(key: string, value: string): void }
interface Dependencies { api: ChatRequest; socket: (url: string) => WebSocket; origin: string; storage?: StorageLike; now: () => number; uuid: () => string; unauthorized: () => void; tickMs: number; retryMs: number }
export const emptyChat: ChatSnapshot = { conversations: [], nextCursor: '', selected: '', messages: [], pending: [], status: 'stopped', loading: false, syncing: false, hasOlder: false, error: '', peerRead: '0' }

// browserStorage 允许读取已有消息；存储禁用时发送入口明确拒绝，避免未知结果后丢失幂等键。
function browserStorage() { try { return sessionStorage } catch { return undefined } }
// errorText 为聊天页面保留可操作的错误信息，不展示 Token。
function errorText(error: unknown) { return error instanceof Error ? error.message : '消息暂不可用，请重试。' }

// ChatSession 管理单账号的连接、连续账本和待发箱；切换账号时整实例释放。
export class ChatSession {
  userId: string
  token: string
  deps: Dependencies
  private controller: AbortController | null = null
  private socket: WebSocket | null = null
  private retry: ReturnType<typeof setTimeout> | undefined
  private handshake: ReturnType<typeof setTimeout> | undefined
  private ticker: ReturnType<typeof setInterval> | undefined
  private listeners = new Set<() => void>()
  private books = new Map<string, Ledger>()
  private conversations = new Map<string, Conversation>()
  private outbox = new Map<string, PendingMessage>()
  private peerReads = new Map<string, string>()
  private syncing = new Set<string>()
  private syncAgain = new Set<string>()
  private loadingBooks = new Set<string>()
  private loadingOlder = new Set<string>()
  private readJobs = new Set<string>()
  private failedReadUntil = new Map<string, number>()
  private listJob = false
  private listAgain = false
  private listVersion = 0
  private listInitialized = false
  private dataRevision = 0
  private connecting = false
  private selected = ''
  private nextCursor = ''
  private status: ChatSnapshot['status'] = 'stopped'
  private error = ''
  private attempts = 0
  private pong = 0
  private readyRequest = ''
  private restored = false
  private storageError = ''
  private snapshot: ChatSnapshot = emptyChat

  // constructor 只配置依赖；网络和定时器均由 start/stop 生命周期管理。
  constructor(userId: string, token: string, overrides: Partial<Dependencies> = {}) {
    this.userId = userId; this.token = token
    this.deps = { api: request, socket: url => new WebSocket(url), origin: globalThis.location?.origin ?? 'http://localhost', storage: browserStorage(), now: Date.now, uuid: () => crypto.randomUUID(), unauthorized: () => {}, tickMs: 10000, retryMs: 1000, ...overrides }
  }
  // subscribe 为 React 外部存储提供精确注销，避免卸载后继续写状态。
  subscribe = (listener: () => void) => { this.listeners.add(listener); return () => { this.listeners.delete(listener) } }
  // getSnapshot 返回稳定快照，仅在实际状态更新时替换。
  getSnapshot = () => this.snapshot
  // book 为会话保留独立序号窗口，不能把其他会话的同步位置复用过来。
  private book(id: string) { let book = this.books.get(id); if (!book) { book = new Ledger(); this.books.set(id, book) } return book }
  // alive 用生命周期标识拒绝 StrictMode 重启、退出或换账号后的迟到响应。
  private alive(controller: AbortController) { return this.controller === controller && !controller.signal.aborted }
  // emit 生成不可变渲染快照，计数来自服务器，不随重复通知盲目加减。
  private emit() {
    const book = this.books.get(this.selected)
    const conversations = [...this.conversations.values()].sort((a, b) => (b.last_message_at ?? b.created_at ?? '').localeCompare(a.last_message_at ?? a.created_at ?? '') || compareSeq(b.id, a.id))
    this.snapshot = { conversations, nextCursor: this.nextCursor, selected: this.selected, messages: book?.sorted() ?? [], pending: [...this.outbox.values()].filter(m => m.conversation_id === this.selected).map(m => ({ ...m })), status: this.status, loading: this.listJob || this.loadingBooks.has(this.selected), syncing: this.syncing.has(this.selected), hasOlder: book?.hasOlder ?? false, error: this.error, peerRead: this.peerReads.get(this.selected) ?? '0' }
    for (const listener of this.listeners) listener()
  }
  // storageKey 按已验证账号隔离待发箱，里面不保存 JWT。
  private storageKey() { return `remarket.chat.outbox.v1.${this.userId}` }
  // persist 发送前同步保存原始键与正文；失败时不会发出新的网络消息。
  private persist() {
    if (!this.deps.storage) throw new Error('请允许页面使用会话存储后发送，以便断线后安全重试。')
    this.deps.storage.setItem(this.storageKey(), JSON.stringify([...this.outbox.values()].map(({ client_msg_id, conversation_id, content, created_at, status, error }) => ({ client_msg_id, conversation_id, content, created_at, status, error }))))
  }
  // restore 只恢复当前账号的有界待发箱，刷新后未知消息仍沿用原键。
  private restore() {
    if (this.restored) return
    this.restored = true
    try {
      const raw = this.deps.storage?.getItem(this.storageKey())
      if (!raw) return
      const rows: unknown = JSON.parse(raw)
      if (!Array.isArray(rows) || rows.length > 100) throw new Error('待发消息记录异常，请检查浏览器会话存储。')
      const restored: PendingMessage[] = []
      for (const value of rows) {
        const m = value as PendingMessage
        if (!decimal(m.conversation_id, false) || !/^[a-zA-Z0-9_.:-]{1,64}$/.test(m.client_msg_id) || typeof m.content !== 'string' || !m.content.trim() || [...m.content].length > 2000) throw new Error('待发消息记录异常。')
        restored.push({ ...m, status: m.status === 'failed' ? 'failed' : 'unknown', requestId: undefined, sentAt: undefined })
      }
      for (const m of restored) this.outbox.set(m.client_msg_id, m)
    } catch (error) { this.storageError = errorText(error); this.error = this.storageError }
  }
  // start 启动连接和周期校验；同实例在 StrictMode 清理后可安全重新启动。
  start() {
    if (this.controller) return
    this.controller = new AbortController(); this.restore()
    this.status = 'connecting'; this.emit()
    void this.refreshList(); void this.connect()
    this.ticker = setInterval(() => this.resume(), this.deps.tickMs)
  }
  // stop 先取消所有 HTTP，再清理计时器与长连接；待发箱保留原键供同账号恢复。
  stop() {
    this.controller?.abort(); this.controller = null
    clearTimeout(this.retry); clearTimeout(this.handshake); clearInterval(this.ticker)
    const ws = this.socket; this.socket = null
    if (ws) { ws.onopen = null; ws.onmessage = null; ws.onclose = null; ws.onerror = null; ws.close() }
    this.connecting = false; this.listVersion++; this.listJob = false; this.listAgain = false; this.syncing.clear(); this.syncAgain.clear(); this.loadingBooks.clear(); this.loadingOlder.clear(); this.readJobs.clear()
    this.status = 'stopped'; this.emit()
  }
  // fail 在认证失效时停止重连，其余错误保留页面与待发消息。
  private fail(error: unknown) {
    this.error = errorText(error)
    if (error instanceof ApiError && error.status === 401) { this.stop(); this.deps.unauthorized() }
    this.emit()
  }
  // connect 每次重连重新获取一次性票据，收到应用 pong 后才允许重试消息。
  private async connect() {
    const controller = this.controller
    if (!controller || controller.signal.aborted || this.connecting) return
    this.connecting = true; clearTimeout(this.retry)
    this.status = this.attempts ? 'reconnecting' : 'connecting'; this.emit()
    try {
      const ticket = await this.deps.api<{ ticket: string }>('/chat/tickets', { method: 'POST', token: this.token, signal: controller.signal })
      if (!this.alive(controller)) return
      const ws = this.deps.socket(socketURL(this.deps.origin, ticket.ticket)); this.socket = ws
      this.handshake = setTimeout(() => { if (this.socket === ws) ws.close() }, 8000)
      ws.onopen = () => { if (this.socket !== ws || !this.alive(controller)) return; this.readyRequest = this.deps.uuid(); ws.send(JSON.stringify({ type: 'ping', request_id: this.readyRequest })) }
      ws.onmessage = event => { if (this.socket === ws && this.alive(controller)) this.receive(String(event.data)) }
      ws.onerror = () => { if (this.socket === ws) ws.close() }
      ws.onclose = () => {
        if (this.socket !== ws || !this.alive(controller)) return
        this.socket = null; clearTimeout(this.handshake)
        for (const pending of this.outbox.values()) if (pending.status === 'sending') { pending.status = 'unknown'; pending.sentAt = undefined }
        this.schedule(); this.emit()
      }
    } catch (error) { if (this.alive(controller)) { this.fail(error); if (this.controller) this.schedule() } }
    finally { if (this.controller === controller) this.connecting = false }
  }
  // schedule 使用有上限的指数退避，避免离线时密集申请票据。
  private schedule() { clearTimeout(this.retry); this.status = 'reconnecting'; const delay = Math.min(30000, this.deps.retryMs * 2 ** Math.min(this.attempts++, 5)); this.retry = setTimeout(() => { void this.connect() }, delay) }
  // resume 周期、网络恢复和页面恢复均触发状态查询，覆盖长连接未断开的通知遗漏。
  resume() {
    if (!this.controller) return
    if (this.status === 'reconnecting' && !this.socket) void this.connect()
    if (this.status === 'online') {
      if (this.deps.now() - this.pong > 40000) { this.socket?.close(); return }
      this.socket?.send(JSON.stringify({ type: 'ping', request_id: this.deps.uuid() })); this.flush()
    }
    void this.refreshList()
    if (this.selected && this.status === 'online') void this.sync(this.selected)
  }
  // refreshList 刷新第一页时保留已分页会话，但对新增与未读使用服务器快照。
  async refreshList(more = false) {
    const controller = this.controller
    if (!controller) return
    if (this.listJob) { if (!more) this.listAgain = true; return }
    const version = ++this.listVersion; const revision = this.dataRevision; this.listJob = true; this.emit()
    try {
      const params = new URLSearchParams({ page_size: '20' }); if (more && this.nextCursor) params.set('cursor', this.nextCursor)
      const result = await this.deps.api<ConversationPage>(`/conversations?${params}`, { token: this.token, signal: controller.signal })
      if (!this.alive(controller) || this.listVersion !== version) return
      if (revision !== this.dataRevision) { this.listAgain = true; return }
      const hasNew = (result.conversations ?? []).some(c => !this.conversations.has(c.id))
      for (const c of result.conversations ?? []) {
        if (!decimal(c.id, false) || !decimal(c.product_id, false) || !decimal(c.buyer_id, false) || !decimal(c.seller_id, false)) throw new Error('会话信息格式无效。')
        const previous = this.conversations.get(c.id)
        this.conversations.set(c.id, { ...c, last_read_seq: maximum(previous?.last_read_seq ?? '0', c.last_read_seq ?? '0') })
      }
      if (more || !this.listInitialized || hasNew) this.nextCursor = result.next_cursor ?? ''
      this.listInitialized = true
      if (!this.selected && this.conversations.size) this.select(this.conversations.keys().next().value!)
    } catch (error) { if (this.alive(controller)) this.fail(error) }
    finally { if (this.alive(controller) && this.listVersion === version) { this.listJob = false; this.emit(); if (this.listAgain) { this.listAgain = false; void this.refreshList() } } }
  }
  // open 注册刚创建的会话，避免只能从已加载第一页中选择。
  open(conversation: Conversation) { this.conversations.set(conversation.id, conversation); this.select(conversation.id) }
  // select 切换时保留各会话独立账本，首次打开先读取最近历史。
  select(id: string) { this.selected = id; this.error = ''; this.emit(); if (!this.book(id).initialized) void this.loadInitial(id); else if (this.status === 'online') void this.sync(id) }
  // loadInitial 以最近一页建立历史基线，实时订阅中到来的消息仍保留在同一账本中。
  private async loadInitial(id: string) {
    const controller = this.controller
    if (!controller || this.loadingBooks.has(id)) return
    this.loadingBooks.add(id); this.emit()
    try {
      const page = await this.deps.api<MessagePage>(`${chatPath(id)}/messages?page_size=20`, { token: this.token, signal: controller.signal })
      if (!this.alive(controller)) return
      const rows = page.messages ?? []; for (const m of rows) { if (m.conversation_id !== id) throw new Error('消息与当前会话不匹配。'); this.confirm(m) }
      this.book(id).seed(rows, !!page.has_more)
      if (this.status === 'online') void this.sync(id)
    } catch (error) { if (this.alive(controller)) this.fail(error) }
    finally { if (this.alive(controller)) { this.loadingBooks.delete(id); this.emit() } }
  }
  // older 用排他 before_seq 查询，不复用补拉游标；旧历史不会降低实时连续位置。
  async older() {
    const id = this.selected; const book = this.book(id); const controller = this.controller
    if (!controller || !book.hasOlder || this.loadingOlder.has(id)) return
    this.loadingOlder.add(id); this.emit()
    try {
      const page = await this.deps.api<MessagePage>(`${chatPath(id)}/messages?${new URLSearchParams({ before_seq: book.oldest, page_size: '20' })}`, { token: this.token, signal: controller.signal })
      if (!this.alive(controller)) return
      for (const m of page.messages ?? []) { if(m.conversation_id!==id) throw new Error('消息与当前会话不匹配。'); this.confirm(m) }
      book.oldest = page.next_seq ?? book.oldest; book.hasOlder = !!page.has_more
    } catch (error) { if (this.alive(controller)) this.fail(error) }
    finally { if (this.alive(controller)) { this.loadingOlder.delete(id); this.emit() } }
  }
  // sync 固定一轮上界，收到实时高序号也不能提前跳过缺失消息。
  async sync(id: string) {
    const controller = this.controller; const book = this.book(id)
    if (!controller || !book.initialized) return
    if (this.syncing.has(id)) { this.syncAgain.add(id); return }
    this.syncing.add(id); this.emit()
    try {
      let after = book.cursor; let until = '0'
      for (let pageNumber = 0; pageNumber < 100; pageNumber++) {
        const params = new URLSearchParams({ after_seq: after, sync_until_seq: until, page_size: '100' })
        const page = await this.deps.api<MessagePage>(`${chatPath(id)}/messages?${params}`, { token: this.token, signal: controller.signal })
        if (!this.alive(controller)) return
        const upper = page.sync_until_seq ?? '0'; const next = page.next_seq ?? after
        if (!decimal(upper) || !decimal(next) || compareSeq(next, after) < 0 || (until !== '0' && upper !== until)) throw new Error('补拉响应位置无效。')
        until = upper
        let expected = after
        for (const m of page.messages ?? []) { if (!validMessage(m) || m.conversation_id!==id || m.seq!==(BigInt(expected)+1n).toString() || compareSeq(m.seq, until)>0) throw new Error('补拉消息超出当前范围或不连续。'); this.confirm(m); expected=m.seq }
        if(next!==expected || (!page.has_more && next!==until)) throw new Error('补拉响应未覆盖请求范围，请重试。')
        if (!page.has_more) break
        if (compareSeq(next, after) <= 0) throw new Error('补拉未前进，请重试。')
        after = next
      }
      this.error = this.storageError
    } catch (error) { if (this.alive(controller)) this.fail(error) }
    finally { if (this.alive(controller)) { this.syncing.delete(id); this.emit(); if (this.syncAgain.delete(id)) void this.sync(id) } }
  }
  // confirm 以已保存消息消除对应的待发项，解决 ACK 丢失但补拉已找到消息的情况。
  private confirm(message: ChatMessage) {
    if (!validMessage(message)) throw new Error('消息格式无效。')
    const book = this.book(message.conversation_id)
    if (!book.messages.has(message.seq)) this.dataRevision++
    book.put(message)
    const pending = this.outbox.get(message.client_msg_id)
    if (message.sender_id === this.userId && pending && pending.conversation_id===message.conversation_id && pending.content===message.content) {
      this.outbox.delete(message.client_msg_id); try { this.persist() } catch { /* 留存旧键只会再次幂等查询，不会重复写入。 */ }
    }
  }
  // send 写入待发箱成功后才尝试网络发送，离线时保留相同键。
  send(content: string) {
    if (this.storageError) throw new Error(this.storageError)
    if (!this.selected || !content.trim() || [...content].length > 2000 || new TextEncoder().encode(content).length > 8000) throw new Error('请输入 1～2000 字的消息。')
    if (this.outbox.size >= 100) throw new Error('待发消息较多，请先处理已有消息。')
    const message: PendingMessage = { conversation_id: this.selected, client_msg_id: this.deps.uuid(), content, created_at: new Date(this.deps.now()).toISOString(), status: 'queued' }
    this.outbox.set(message.client_msg_id, message)
    try { this.persist() } catch { this.outbox.delete(message.client_msg_id); throw new Error('无法保存待发消息，请允许页面使用会话存储后重试。') }
    this.flush(); this.emit()
  }
  // retryMessage 用户重试时复用原键与正文，明确失败的消息不会自动重发。
  retryMessage(key: string) { const m = this.outbox.get(key); if (!m) return; m.status = 'unknown'; m.sentAt = undefined; m.error = ''; this.persist(); this.flush(); this.emit() }
  // discard 仅移除本地待发项；未知结果可能已入库，之后仍可通过补拉看到。
  discard(key: string) { const old = this.outbox.get(key); this.outbox.delete(key); try { this.persist() } catch { if (old) this.outbox.set(key, old); throw new Error('无法更新待发箱，请重试。') } this.emit() }
  // flush 限制在途请求数量，超时只表示结果未知，不生成新消息键。
  private flush() {
    const ws = this.socket
    if (!ws || ws.readyState !== 1 || this.status !== 'online') return
    let inFlight = [...this.outbox.values()].filter(m => m.status === 'sending' && this.deps.now() - (m.sentAt ?? 0) < 8000).length
    for (const m of this.outbox.values()) {
      if (m.status === 'sending' && this.deps.now() - (m.sentAt ?? 0) < 8000) { continue }
      if (m.status === 'sending') m.status = 'unknown'
      if (m.status === 'failed' || (m.sentAt !== undefined && this.deps.now() - m.sentAt < 8000) || inFlight >= 5) continue
      m.requestId = this.deps.uuid(); m.status = 'sending'; m.sentAt = this.deps.now(); inFlight++
      try { ws.send(JSON.stringify({ type: 'chat.send', request_id: m.requestId, data: { conversation_id: m.conversation_id, client_msg_id: m.client_msg_id, content: m.content } })) } catch { m.status = 'unknown'; ws.close(); break }
    }
  }
  // receive 合并 ACK/推送/已读，失败信息不能把未保存消息伪装成已发送。
  private receive(raw: string) {
    try {
      const f = JSON.parse(raw); const d = f.data
      if (f.type === 'pong') {
        this.pong = this.deps.now()
        if (f.request_id === this.readyRequest && this.status !== 'online') {
          clearTimeout(this.handshake); this.attempts = 0; this.status = 'online'; this.error = this.storageError
          for (const m of this.outbox.values()) if (m.status !== 'failed') { m.status = 'unknown'; m.sentAt = undefined }
          this.flush(); if (this.selected) { if (this.book(this.selected).initialized) void this.sync(this.selected); else void this.loadInitial(this.selected) } void this.refreshList()
        }
      } else if (f.type === 'chat.ack') {
        const pending = this.outbox.get(d?.client_msg_id)
        if (pending && pending.conversation_id === d.conversation_id && decimal(d.message_id, false) && decimal(d.seq, false)) this.confirm({ id: d.message_id, conversation_id: d.conversation_id, sender_id: this.userId, client_msg_id: pending.client_msg_id, seq: d.seq, content: pending.content, created_at: pending.created_at, message_type: 'TEXT' })
        this.flush(); void this.refreshList(); if (this.selected) void this.sync(this.selected)
      } else if (f.type === 'chat.message') {
        this.confirm({ ...d, id: d?.message_id })
        void this.refreshList()
        if (this.selected === d.conversation_id && this.book(this.selected).initialized) void this.sync(this.selected)
      } else if (f.type === 'chat.read' && decimal(d?.conversation_id, false) && decimal(d?.user_id, false) && decimal(d?.last_read_seq)) {
        if (d.user_id === this.userId) { const c = this.conversations.get(d.conversation_id); if (c) { const next = maximum(c.last_read_seq ?? '0', d.last_read_seq); if (next !== c.last_read_seq) this.dataRevision++; c.last_read_seq = next } }
        else this.peerReads.set(d.conversation_id, maximum(this.peerReads.get(d.conversation_id) ?? '0', d.last_read_seq))
        void this.refreshList()
      } else if (f.type === 'chat.error') {
        if (d?.code === 'Unauthenticated') { this.fail(new ApiError('登录已失效，请重新登录。', 401)); return }
        const pending = [...this.outbox.values()].find(m => m.requestId === f.request_id)
        if (pending) { pending.status = ['Unavailable', 'DeadlineExceeded', 'Internal', 'Canceled'].includes(d?.code) ? 'unknown' : 'failed'; pending.error = typeof d?.message === 'string' ? d.message : '发送未完成'; try { this.persist() } catch { /* 原键已在发出前保存。 */ } }
      }
      this.emit()
    } catch (error) { this.fail(error) }
  }
  // markVisible 仅由已渲染且处于底部的窗口调用；同步完成不等于用户阅读。
  async markVisible(id: string) {
    const controller = this.controller; const book = this.book(id); const c = this.conversations.get(id)
    if (!controller || this.selected !== id || !book.initialized || !c || this.readJobs.has(id) || (this.failedReadUntil.get(id) ?? 0) > this.deps.now() || compareSeq(book.cursor, c.last_read_seq ?? '0') <= 0) return
    this.readJobs.add(id)
    try {
      const state = await this.deps.api<ReadState>(`${chatPath(id)}/read`, { method: 'POST', token: this.token, signal: controller.signal, body: { last_read_seq: book.cursor } })
      if (!this.alive(controller)) return
      const current = this.conversations.get(id) ?? c
      current.last_read_seq = maximum(current.last_read_seq ?? '0', state.last_read_seq ?? '0'); current.unread_count = state.unread_count ?? '0'; this.dataRevision++
      this.failedReadUntil.delete(id); this.emit(); void this.refreshList()
    } catch (error) { if (this.alive(controller)) { this.failedReadUntil.set(id, this.deps.now() + 10000); this.fail(error) } }
    finally { if (this.alive(controller)) this.readJobs.delete(id) }
  }
}
