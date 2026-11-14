import { useEffect, useRef, useState } from 'react'
import { yuan } from '../api/client'
import type { Product, User } from '../api/client'
import { createOrder, decideOrder, getOrder, listOrders, orderLabels, processing, checkoutKey, forgetCheckout } from '../api/orders'
import type { Order, OrderRole } from '../api/orders'
import { Modal } from './Modal'

// dateText 展示服务端绝对时间，不以本地时间替代服务端截止判定。
function dateText(value?: string): string { return value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '—' }
// statusTone 将真实业务状态映射为固定视觉语义，未知状态保留中性展示。
function statusTone(order: Order): string { return order.status === 'ORDER_STATUS_COMPLETED' ? 'done' : ['ORDER_STATUS_FAILED', 'ORDER_STATUS_EXPIRED', 'ORDER_STATUS_CANCELLED'].includes(order.status) ? 'closed' : 'waiting' }
// OrderBadge 使用文字和颜色共同表达状态，避免仅靠颜色区分交易结果。
function OrderBadge({ order }: { order: Order }) { return <span className={`order-badge ${statusTone(order)}`}>{orderLabels[order.status] ?? '未知状态'}</span> }

// Checkout 保持当前买家/商品的同一结算意图，网络结果未知时跨关闭及刷新复用幂等键。
export function Checkout({ product, user, token, close, created, message, inspectOrders }: { product: Product; user: User; token: string; close: () => void; created: (order: Order) => void; inspectOrders: () => void; message: (error: unknown) => string }) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [agreed, setAgreed] = useState(false)
  const active = useRef<AbortController | null>(null)
  const submitting = useRef(false)
  useEffect(() => () => active.current?.abort(), [])
  // submit 在发出请求前持久化随机键；未知结果保留原键，成功取到订单后才清理。
  async function submit() {
    if (submitting.current || !agreed) return
    submitting.current = true; setBusy(true); setError('')
    const controller = new AbortController(); active.current = controller
    try {
      const key = checkoutKey(user.id, product.id)
      const result = await createOrder(product.id, key, token, controller.signal)
      if (controller.signal.aborted) return
      forgetCheckout(user.id, product.id)
      created(result.order)
    } catch (err) { if (!controller.signal.aborted) setError(`${message(err)} 请保持原商品并重试，系统会复用同一笔请求。`) }
    finally { submitting.current = false; if (!controller.signal.aborted) setBusy(false) }
  }
  return <Modal title="确认下单" busy={busy} close={close}>
    <div className="checkout-product"><span className="order-eyebrow">本次购买</span><h3>{product.title}</h3><p className="order-amount">¥ {yuan(product.price_cent)}</p><p className="muted">卖家编号 {product.seller_id} · 商品编号 {product.id}</p></div>
    <div className="order-note"><strong>线下交易，无在线支付</strong><p>下单后等待商品预占结果。请与卖家协商交易，收货并检查后再确认成交。只有双方均确认，订单才会完成。</p><p>显示价格供下单前核对，最终金额以订单详情为准。</p></div>
    <label className="checkout-agree"><input type="checkbox" checked={agreed} onChange={event => setAgreed(event.target.checked)} disabled={busy} /><span>我已了解交易方式和双方确认规则</span></label>
    {error && <p className="form-error" role="alert">{error}</p>}
    <div className="order-actions">{error && <button disabled={busy} onClick={inspectOrders}>查看我的订单</button>}<button disabled={busy} onClick={close}>返回商品</button><button className="primary" disabled={busy || !agreed} onClick={() => void submit()}>{busy ? '正在提交…' : '确认下单'}</button></div>
  </Modal>
}

// OrdersPage 分别读取当前用户买入和卖出订单；切换账号时由父组件 key 隔离页面状态。
export function OrdersPage({ user, token, role, changeRole, message, initialId, clearInitial, browse }: { user: User; token: string; role: OrderRole; changeRole: (role: OrderRole) => void; message: (error: unknown) => string; initialId: string | null; clearInitial: () => void; browse: () => void }) {
  const [orders, setOrders] = useState<Order[]>([])
  const [total, setTotal] = useState('0')
  const [page, setPage] = useState(1)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [revision, setRevision] = useState(0)
  const [detailId, setDetailId] = useState<string | null>(null)
  useEffect(() => {
    const controller = new AbortController(); let timer: number | undefined
    queueMicrotask(() => { if (!controller.signal.aborted) { setLoading(true); setError('') } })
    listOrders(role, page, token, controller.signal).then(data => {
      if (controller.signal.aborted) return
      const rows = data.orders ?? []; setOrders(rows); setTotal(data.total ?? '0')
      if (rows.some(processing)) timer = window.setTimeout(() => setRevision(value => value + 1), 5000)
    }).catch(err => { if (!controller.signal.aborted) { setOrders([]); setError(message(err)) } }).finally(() => { if (!controller.signal.aborted) setLoading(false) })
    return () => { controller.abort(); window.clearTimeout(timer) }
  }, [role, page, token, revision, message])
  // switchRole 重置列表页码，避免把买入列表的偏移带到卖出列表。
  function switchRole(next: OrderRole) { if (next === role) return; setPage(1); setOrders([]); changeRole(next) }
  // closeDetail 关闭初始下单详情并刷新列表，以服务端最新状态展示。
  function closeDetail() { setDetailId(null); clearInitial(); setRevision(value => value + 1) }
  return <section className="orders-page">
    <div className="orders-heading"><div><h1>我的订单</h1></div><button onClick={() => setRevision(value => value + 1)} disabled={loading}>刷新列表</button></div>
    <div className="orders-tabs" aria-label="订单类型"><button aria-pressed={role === 'buyer'} className={role === 'buyer' ? 'active' : ''} onClick={() => switchRole('buyer')}>我买到的</button><button aria-pressed={role === 'seller'} className={role === 'seller' ? 'active' : ''} onClick={() => switchRole('seller')}>我卖出的</button><span>{total} 笔订单</span></div>
    {error ? <div className="empty error" role="alert"><h3>订单暂时无法加载</h3><p>{error}</p><button onClick={() => setRevision(value => value + 1)}>重新加载</button></div> : loading ? <div className="order-loading" role="status">正在读取订单…</div> : orders.length ? <div className="order-list">{orders.map(order => <article className="order-card" key={order.id}>
      <div className="order-card-top"><span>订单 #{order.id}</span><OrderBadge order={order} /></div>
      <div className="order-card-body"><div><h2>商品 #{order.product_id}</h2><p>{role === 'buyer' ? `卖家编号 ${order.seller_id}` : `买家编号 ${order.buyer_id}`}</p><p>创建于 {dateText(order.created_at)}</p></div><strong className="order-card-price">¥ {yuan(order.price_cent)}</strong></div>
      <div className="order-card-bottom"><span>{order.status === 'ORDER_STATUS_RESERVED' ? '线下交易后，等待双方确认' : processing(order) ? '系统处理中，可查看最新进度' : '查看订单了解交易结果'}</span><button onClick={() => setDetailId(order.id)}>查看订单</button></div>
    </article>)}</div> : <div className="empty"><span className="empty-symbol">—</span><h3>{role === 'buyer' ? '还没有买入订单' : '还没有卖出订单'}</h3><p>{role === 'buyer' ? '从商品详情点击“立即购买”，开始你的第一笔交易。' : '有人购买你发布的商品后，会在这里显示。'}</p><button onClick={browse}>去逛逛</button></div>}
    {!error && <div className="pagination"><button disabled={loading || page <= 1} onClick={() => setPage(value => value - 1)}>← 上一页</button><span>第 {page} 页</span><button disabled={loading || BigInt(page * 12) >= BigInt(total)} onClick={() => setPage(value => value + 1)}>下一页 →</button></div>}
    {(detailId || initialId) && <OrderDetail key={detailId || initialId} id={(detailId || initialId)!} user={user} token={token} message={message} close={closeDetail} />}
  </section>
}

// OrderDetail 查询真实订单与双方确认情况，处理中持续刷新，终态停止轮询并保留错误重试入口。
function OrderDetail({ id, user, token, message, close }: { id: string; user: User; token: string; message: (error: unknown) => string; close: () => void }) {
  const [order, setOrder] = useState<Order | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [revision, setRevision] = useState(0)
  const [now, setNow] = useState(() => Date.now())
  const active = useRef<AbortController | null>(null)
  const deciding = useRef(false)
  useEffect(() => () => active.current?.abort(), [])
  useEffect(() => {
    const timer = window.setInterval(() => setNow(Date.now()), 1000)
    return () => window.clearInterval(timer)
  }, [])
  useEffect(() => {
    const controller = new AbortController(); let timer: number | undefined
    // 刷新与写操作互不覆盖；写入前会使当前读取失效。
    if (busy) return
    queueMicrotask(() => { if (!controller.signal.aborted) setError('') })
    getOrder(id, token, controller.signal).then(data => {
      if (controller.signal.aborted) return
      setOrder(data.order)
      if (processing(data.order) || data.order.status === 'ORDER_STATUS_RESERVED') timer = window.setTimeout(() => setRevision(value => value + 1), 2500)
    }).catch(err => { if (!controller.signal.aborted) setError(message(err)) })
    return () => { controller.abort(); window.clearTimeout(timer) }
  }, [id, token, revision, message, busy])
  // decide 二次确认后才写入，不乐观变更状态；失败后仍允许刷新或重复原操作恢复。
  async function decide(action: 'cancel' | 'confirm') {
    if (deciding.current) return
    const prompt = action === 'confirm' ? '已完成线下交易并检查商品？确认后，双方都确认时订单将完成。' : '确定取消订单并释放商品？即使已有一方确认，取消成功后仍会关闭交易。'
    if (!window.confirm(prompt)) return
    deciding.current = true; setBusy(true); setError('')
    const controller = new AbortController(); active.current = controller
    try { const result = await decideOrder(id, action, token, controller.signal); if (!controller.signal.aborted) setOrder(result.order) }
    catch (err) { if (!controller.signal.aborted) setError(`${message(err)} 请刷新确认当前结果。`) }
    finally { deciding.current = false; if (!controller.signal.aborted) setBusy(false) }
  }
  const expired = order ? now >= Date.parse(order.expires_at) : false
  const participant = order && (user.id === order.buyer_id || user.id === order.seller_id)
  const confirmed = order && (user.id === order.buyer_id ? order.buyer_confirmed : order.seller_confirmed)
  const actionable = order?.status === 'ORDER_STATUS_RESERVED' && participant && !expired && !error
  return <Modal title="订单详情" busy={busy} close={close}>
    {!order ? <p role="status">{error ? '暂时无法读取订单。' : '正在读取订单…'}</p> : <>
      <div className="order-detail-status"><OrderBadge order={order} /><p>{processing(order) ? '正在确认处理结果，请稍候。' : order.status === 'ORDER_STATUS_RESERVED' ? expired ? '已到截止时间，等待系统关闭。' : '请在线下完成交易后，再由双方确认。' : order.status === 'ORDER_STATUS_COMPLETED' ? '双方已确认交易完成。此记录不代表在线付款。' : order.status === 'ORDER_STATUS_FAILED' ? '商品未能成功预占，本次下单未完成。' : '订单已关闭。'}</p></div>
      <p className="order-amount">¥ {yuan(order.price_cent)}</p>
      <dl className="order-facts"><dt>订单编号</dt><dd>{order.id}</dd><dt>商品编号</dt><dd>{order.product_id}</dd><dt>买家编号</dt><dd>{order.buyer_id}{user.id === order.buyer_id ? '（我）' : ''}</dd><dt>卖家编号</dt><dd>{order.seller_id}{user.id === order.seller_id ? '（我）' : ''}</dd><dt>创建时间</dt><dd>{dateText(order.created_at)}</dd><dt>确认截止</dt><dd>{dateText(order.expires_at)}</dd>{order.closed_at && <><dt>关闭时间</dt><dd>{dateText(order.closed_at)}</dd></>}</dl>
      <div className="confirmation-pair"><div className={order.buyer_confirmed ? 'confirmed' : ''}><span>买家确认</span><strong>{order.buyer_confirmed ? '已确认' : '未确认'}</strong></div><div className={order.seller_confirmed ? 'confirmed' : ''}><span>卖家确认</span><strong>{order.seller_confirmed ? '已确认' : '未确认'}</strong></div></div>
      <p className="scope-note">无在线支付。待双方确认的订单在截止后自动关闭，最终结果以服务器处理为准。</p>
      <div className="order-actions"><button disabled={busy} onClick={() => setRevision(value => value + 1)}>刷新结果</button>{actionable && <button disabled={busy} onClick={() => void decide('cancel')}>取消订单</button>}{actionable && !confirmed && <button className="primary" disabled={busy} onClick={() => void decide('confirm')}>{busy ? '处理中…' : '确认成交'}</button>}</div>
    </>}
    {error && <div className="form-error" role="alert">{error}{!order && <button onClick={() => setRevision(value => value + 1)}>重新加载</button>}</div>}
  </Modal>
}
