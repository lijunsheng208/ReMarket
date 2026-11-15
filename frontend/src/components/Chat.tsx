import { useEffect, useLayoutEffect, useRef, useState } from 'react'
import type { FormEvent } from 'react'
import { request } from '../api/client'
import type { Product } from '../api/client'
import type { Conversation } from '../api/chat'
import { compareSeq } from '../chat/ledger'
import type { ChatSession, ChatSnapshot } from '../chat/session'
import './Chat.css'

// timeLabel 用消息实际时间显示标签，非法或缺省时间不伪造日期。
function timeLabel(value?: string) { if (!value || !Number.isFinite(Date.parse(value))) return ''; return new Date(value).toLocaleString('zh-CN', { month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' }) }
// ConversationName 查询真实昵称与商品标题；商品已不可见时保留明确编号入口。
function ConversationName({ conversation, userId, token, compact = false }: { conversation: Conversation; userId: string; token: string; compact?: boolean }) {
  const [name, setName] = useState('')
  const [title, setTitle] = useState('')
  const otherId = conversation.buyer_id === userId ? conversation.seller_id : conversation.buyer_id
  useEffect(() => {
    const controller = new AbortController()
    request<{ nickname: string }>(`/users/${otherId}/profile`, { token, signal: controller.signal }).then(p => { if (!controller.signal.aborted) setName(p.nickname) }).catch(() => {})
    if (!compact) request<{ product: Product }>(`/products/${conversation.product_id}`, { token, signal: controller.signal }).then(p => { if (!controller.signal.aborted) setTitle(p.product.title) }).catch(() => {})
    return () => controller.abort()
  }, [otherId, conversation.product_id, token, compact])
  return <span className="chat-person-copy"><strong>{name || `用户 #${otherId}`}</strong>{!compact && <small>{title || `商品 #${conversation.product_id}`}</small>}</span>
}
// ProductContext 用真实商品标题保留咨询语境，不让内部编号占据主要界面。
function ProductContext({ id, token, open }: { id: string; token: string; open: (id: string) => void }) {
  const [title, setTitle] = useState('')
  useEffect(() => { const controller = new AbortController(); request<{ product: Product }>(`/products/${id}`, { token, signal: controller.signal }).then(result => { if (!controller.signal.aborted) setTitle(result.product.title) }).catch(() => {}); return () => controller.abort() }, [id, token])
  return <button className="chat-product-context" onClick={() => open(id)}><span>正在聊这件商品</span><strong>{title || '查看商品详情'}</strong><span aria-hidden="true">↗</span></button>
}
// Chat 展示单账号的会话与真实消息，渲染后且用户停留底部才推进已读。
export function Chat({ session, snapshot, userId, token, readable, openProduct, browse, initialConversation }: { session: ChatSession; snapshot: ChatSnapshot; userId: string; token: string; readable: boolean; openProduct: (id: string) => void; browse: () => void; initialConversation?: string }) {
  const [drafts, setDrafts] = useState<Record<string, string>>({})
  const [error, setError] = useState('')
  const [olderBusy, setOlderBusy] = useState(false)
  const [mobileThread, setMobileThread] = useState(!!initialConversation)
  const [atBottom, setAtBottom] = useState(true)
  const [focused, setFocused] = useState(document.hasFocus())
  const [visible, setVisible] = useState(document.visibilityState === 'visible')
  const feed = useRef<HTMLDivElement>(null)
  const position = useRef<{ id: string; previousHeight: number; preserving: boolean }>({ id: '', previousHeight: 0, preserving: false })
  const conversation = snapshot.conversations.find(c => c.id === snapshot.selected)
  const draft = drafts[snapshot.selected] ?? ''
  useEffect(() => {
    // updateVisibility 独立跟踪浏览器焦点与可见性，后台页面不提交已读。
    const updateVisibility = () => { setFocused(document.hasFocus()); setVisible(document.visibilityState === 'visible') }
    window.addEventListener('focus', updateVisibility); window.addEventListener('blur', updateVisibility); document.addEventListener('visibilitychange', updateVisibility)
    return () => { window.removeEventListener('focus', updateVisibility); window.removeEventListener('blur', updateVisibility); document.removeEventListener('visibilitychange', updateVisibility) }
  }, [])
  useLayoutEffect(() => {
    const element = feed.current
    if (!element) return
    if (position.current.id !== snapshot.selected) { element.scrollTop = element.scrollHeight; position.current = { id: snapshot.selected, previousHeight: element.scrollHeight, preserving: false } }
    else if (!position.current.preserving && atBottom) element.scrollTop = element.scrollHeight
  }, [snapshot.selected, snapshot.messages, snapshot.pending, atBottom, mobileThread])
  useEffect(() => {
    if (!conversation || !readable || !atBottom || !focused || !visible || (!mobileThread && window.matchMedia('(max-width: 700px)').matches)) return
    const frame = requestAnimationFrame(() => { void session.markVisible(conversation.id) })
    return () => cancelAnimationFrame(frame)
  }, [session, snapshot, conversation, readable, atBottom, focused, visible, mobileThread])
  // select 保留未发草稿，切换会话时恢复到底部并将移动端焦点移入聊天区。
  function select(id: string) { setError(''); setAtBottom(true); setMobileThread(true); session.select(id) }
  // submit 先保存待发键，失败时保留输入，中文输入法组合态不会通过 Enter 误发。
  function submit(event: FormEvent) {
    event.preventDefault(); setError('')
    try { session.send(draft); setDrafts(values => ({ ...values, [snapshot.selected]: '' })); setAtBottom(true) } catch (err) { setError(err instanceof Error ? err.message : '发送未完成') }
  }
  // older 保留阅读位置，新增旧消息时不把用户滚动到最新消息。
  async function older() {
    if (olderBusy) return
    setOlderBusy(true)
    if (feed.current) position.current = { id: snapshot.selected, previousHeight: feed.current.scrollHeight, preserving: true }
    const id = snapshot.selected
    const element = feed.current
    const anchor = element ? [...element.querySelectorAll<HTMLElement>('[data-message-id]')].find(node => node.getBoundingClientRect().bottom > element.getBoundingClientRect().top) : undefined
    const top = anchor?.getBoundingClientRect().top
    await session.older()
    setOlderBusy(false)
    requestAnimationFrame(() => { if (position.current.id !== id || !feed.current) return; feed.current.scrollTop += anchor?.isConnected && top !== undefined ? anchor.getBoundingClientRect().top - top : feed.current.scrollHeight - position.current.previousHeight; position.current.preserving = false })
  }
  // action 显示待发箱存储错误，不吞掉用户的重试或移除失败。
  function action(work: () => void) { try { work(); setError('') } catch (err) { setError(err instanceof Error ? err.message : '操作未完成') } }
  const status = snapshot.status === 'online' ? '已连接' : snapshot.status === 'connecting' ? '连接中…' : snapshot.status === 'reconnecting' ? '正在重新连接…' : '未连接'
  return <section className={`chat-page ${mobileThread ? 'show-thread' : ''}`} aria-label="聊天消息">
    <div className="chat-page-heading"><div><h1>消息</h1><p>聊聊成色、价格与交接方式</p></div><span className={`chat-connection ${snapshot.status === 'online' ? 'connected' : ''}`} role="status"><i />{status}</span></div>
    {(snapshot.error || error) && <div className="form-error" role="alert">{error || snapshot.error}<button onClick={() => { setError(''); session.resume() }}>重试加载</button></div>}
    <div className="chat-workspace">
      <aside className="chat-sidebar" aria-label="会话列表"><div className="chat-sidebar-heading"><h2>我的会话</h2><button className="text-button" disabled={snapshot.loading} onClick={() => void session.refreshList()}>刷新</button></div>
        <div className="chat-conversations">{snapshot.conversations.map(c => <button key={c.id} className={`chat-conversation ${c.id === snapshot.selected ? 'active' : ''}`} onClick={() => select(c.id)} aria-current={c.id === snapshot.selected ? 'true' : undefined}><span className="chat-avatar" aria-hidden="true">{c.buyer_id === userId ? '卖' : '买'}</span><span className="chat-conversation-body"><ConversationName key={c.id} conversation={c} userId={userId} token={token} /><span className="chat-preview">{c.last_message_content || (c.last_message_id ? '查看最近消息' : '还没有消息，打个招呼吧')}</span><time>{timeLabel(c.last_message_at ?? c.created_at)}</time></span>{BigInt(c.unread_count ?? '0') > 0n && <span className="chat-unread" aria-label={`${c.unread_count} 条未读`}>{BigInt(c.unread_count ?? '0') > 99n ? '99+' : c.unread_count}</span>}</button>)}
          {!snapshot.conversations.length && <div className="chat-empty"><span aria-hidden="true">↗</span><h3>{snapshot.loading ? '正在加载会话' : '从一件好物开始聊'}</h3><p>在商品详情中选择“联系卖家”</p><button onClick={browse}>去逛逛</button></div>}
          {snapshot.nextCursor && <button className="chat-more" disabled={snapshot.loading} onClick={() => void session.refreshList(true)}>更多会话</button>}
        </div>
      </aside>
      <section className="chat-thread" aria-label="聊天窗口">
        {conversation ? <><header className="chat-thread-heading"><button className="chat-back text-button" onClick={() => setMobileThread(false)}>← 会话</button><span className="chat-avatar" aria-hidden="true">{conversation.buyer_id === userId ? '卖' : '买'}</span><ConversationName key={conversation.id} conversation={conversation} userId={userId} token={token} compact /><span className="chat-consultation">商品咨询</span></header>
          <ProductContext key={conversation.product_id} id={conversation.product_id} token={token} open={openProduct} />
          <div ref={feed} className="chat-message-feed" role="log" aria-label="消息记录" aria-live="polite" onScroll={event => { const node = event.currentTarget; setAtBottom(node.scrollHeight - node.scrollTop - node.clientHeight < 48) }}>
            {snapshot.hasOlder && <button className="chat-history" disabled={olderBusy} onClick={() => void older()}>{olderBusy ? '加载中…' : '查看更早消息'}</button>}
            {snapshot.loading && !snapshot.messages.length && <p className="chat-feed-note" role="status">正在加载消息…</p>}
            {!snapshot.loading && !snapshot.messages.length && !snapshot.pending.length && <div className="chat-empty"><h3>你好，好物还在吗？</h3><p>发送第一条消息，和对方聊聊吧。</p></div>}
            {snapshot.messages.map(m => <article className={`chat-message ${m.sender_id === userId ? 'mine' : ''}`} key={m.id} data-message-id={m.id}><div className="chat-bubble">{m.content}</div><small><time>{timeLabel(m.created_at)}</time>{m.sender_id === userId && <span>{compareSeq(m.seq, snapshot.peerRead) <= 0 ? '对方已读' : '已发送'}</span>}</small></article>)}
            {snapshot.pending.map(m => <article className="chat-message mine pending" key={m.client_msg_id}><div className="chat-bubble">{m.content}</div><small><span>{m.status === 'queued' ? '等待连接后发送' : m.status === 'sending' ? '发送中…' : m.status === 'unknown' ? '待确认 · 将安全重试' : '未发送'}</span>{(m.status === 'unknown' || m.status === 'failed') && <button onClick={() => action(() => session.retryMessage(m.client_msg_id))}>重试</button>}<button onClick={() => action(() => session.discard(m.client_msg_id))} aria-label="移除本地待发消息">移除</button></small>{m.error && <span className="chat-pending-error">{m.error}</span>}</article>)}
          </div>
          {!atBottom && <button className="chat-jump" onClick={() => { setAtBottom(true); if (feed.current) feed.current.scrollTop = feed.current.scrollHeight }}>回到最新消息 ↓</button>}
          <form className="chat-composer" onSubmit={submit}><label className="sr-only" htmlFor="chat-content">消息内容</label><textarea id="chat-content" rows={3} value={draft} placeholder="问问商品成色，约好交接时间…" onChange={event => setDrafts(values => ({ ...values, [snapshot.selected]: event.target.value }))} onKeyDown={event => { if (event.key === 'Enter' && !event.shiftKey && !event.nativeEvent.isComposing && event.keyCode !== 229) { event.preventDefault(); event.currentTarget.form?.requestSubmit() } }} /><div className="chat-compose-actions"><span>{snapshot.syncing ? '正在同步消息…' : 'Enter 发送 · Shift + Enter 换行'}<small>{[...draft].length}/2000</small></span><button className="primary" type="submit" disabled={!draft.trim() || [...draft].length > 2000}>发送 <span aria-hidden="true">↑</span></button></div></form></> : <div className="chat-empty chat-welcome"><span aria-hidden="true">↗</span><h2>让好物有个新去处</h2><p>选择一段会话，继续聊聊。</p></div>}
      </section>
    </div>
  </section>
}
