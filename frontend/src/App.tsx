import { useCallback, useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import { ApiError, cents, request, yuan } from './api/client'
import type { Category, Product, User } from './api/client'
import { Modal } from './components/Modal'
import './App.css'

const states: Record<string, string> = { PRODUCT_STATUS_DRAFT: '草稿', PRODUCT_STATUS_ON_SALE: '在售', PRODUCT_STATUS_OFF_SHELF: '已下架', PRODUCT_STATUS_RESERVED: '已预占', PRODUCT_STATUS_SOLD: '已售出' }
// readToken 仅在当前浏览器会话保留登录，存储不可用时退化为内存状态。
function readToken() { try { return sessionStorage.getItem('remarket.token') ?? '' } catch { return '' } }
// App 组织商品浏览、账号与编辑流程，所有业务数据从 Gateway 获取。
function App() {
  const [token, setToken] = useState(readToken)
  const [user, setUser] = useState<User | null>(null)
  const [authReady, setAuthReady] = useState(!token)
  const [tab, setTab] = useState<'browse' | 'saved'>('browse')
  const [categories, setCategories] = useState<Category[]>([])
  const [category, setCategory] = useState('')
  const [products, setProducts] = useState<Product[]>([])
  const [page, setPage] = useState(1)
  const [total, setTotal] = useState('0')
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [revision, setRevision] = useState(0)
  const [authMode, setAuthMode] = useState<'login' | 'register' | null>(null)
  const [editor, setEditor] = useState<Product | 'new' | null>(null)
  const [detail, setDetail] = useState<Product | null>(null)
  const [busy, setBusy] = useState(false)
  const [dialogError, setDialogError] = useState('')
  const [lookup, setLookup] = useState('')

  // logout 清理会话凭据及受保护页面状态；不宣称撤销已签发的 JWT。
  const logout = useCallback(() => { try { sessionStorage.removeItem('remarket.token') } catch { /* 浏览器可能禁用存储。 */ } setToken(''); setUser(null); setTab('browse'); setDetail(null); setEditor(null); setAuthReady(true) }, [])
  // message 统一解释错误，保留请求编号便于联调；401 时清理本地过期会话。
  const message = useCallback((error: unknown): string => { if (error instanceof ApiError && error.status === 401 && token) logout(); return error instanceof Error ? `${error.message}${error instanceof ApiError && error.requestId ? `（请求编号 ${error.requestId}）` : ''}` : '操作未完成，请重试。' }, [token, logout])

  useEffect(() => {
    const controller = new AbortController()
    if (!token) return
    request<{ user: User }>('/users/me', { token, signal: controller.signal }).then(result => setUser(result.user)).catch(err => { if (!controller.signal.aborted) { setError(err instanceof Error ? err.message : '无法恢复登录'); if (err instanceof ApiError && (err.status === 401 || err.status === 403)) logout() } }).finally(() => { if (!controller.signal.aborted) setAuthReady(true) })
    return () => controller.abort()
  }, [token, logout])
  useEffect(() => {
    const controller = new AbortController()
    request<{ categories?: Category[] }>('/categories', { signal: controller.signal }).then(data => setCategories(data.categories ?? [])).catch(err => { if (!controller.signal.aborted) setError(err instanceof Error ? err.message : '分类加载失败') })
    return () => controller.abort()
  }, [revision])
  useEffect(() => {
    const controller = new AbortController()
    queueMicrotask(() => { if (!controller.signal.aborted) { setLoading(true); setError('') } })
    // 浏览公开列表不附带 Token，避免过期会话影响匿名浏览。
    const work = tab === 'browse' ? request<{ products?: Product[]; total?: string }>(`/products?page=${page}&page_size=12${category ? `&category_id=${category}` : ''}`, { signal: controller.signal }) : user ? request<{ products?: Product[]; total?: string }>(`/products/mine?page=${page}&page_size=12`, { token, signal: controller.signal }) : Promise.resolve({ products: [], total: '0' })
    work.then(data => { setProducts(data.products ?? []); setTotal(data.total ?? '0') }).catch(err => { if (!controller.signal.aborted) { setProducts([]); setError(message(err)) } }).finally(() => { if (!controller.signal.aborted) setLoading(false) })
    return () => controller.abort()
  }, [tab, page, category, revision, user, token, message])

  // openProduct 读取最新详情后展示，不以卡片上的旧版本执行修改。
  async function openProduct(id: string) { setBusy(true); setError(''); try { const data = await request<{ product: Product }>(`/products/${encodeURIComponent(id)}`, { token }); setDetail(data.product); setDialogError('') } catch (err) { setError(message(err)) } finally { setBusy(false) } }
  // authenticate 注册后切换登录，不自动创建不存在的注册令牌。
  async function authenticate(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); const form = new FormData(event.currentTarget); setBusy(true); setDialogError('')
    try {
      const body = { username: String(form.get('username')), password: String(form.get('password')) }
      if (authMode === 'register') { await request('/auth/register', { method: 'POST', body: { ...body, nickname: String(form.get('nickname')) } }); setAuthMode('login'); setNotice('注册成功，请使用新账号登录。') }
      else { const result = await request<{ access_token: string; user: User }>('/auth/login', { method: 'POST', body }); try { sessionStorage.setItem('remarket.token', result.access_token) } catch { /* 仍可使用内存会话。 */ } setAuthReady(false); setToken(result.access_token); setUser(result.user); setAuthMode(null); setNotice('登录成功，欢迎回来。') }
    } catch (err) { setDialogError(message(err)) } finally { setBusy(false) }
  }
  // saveDraft 将元精确转换为分，编辑时带上当前版本，发生冲突先刷新详情再重试。
  async function saveDraft(event: FormEvent<HTMLFormElement>) {
    event.preventDefault(); const form = new FormData(event.currentTarget); setBusy(true); setDialogError('')
    try {
      const existing = editor !== 'new' ? editor : null
      const result = await request<{ product: Product }>(existing ? `/products/${existing.id}` : '/products', { method: existing ? 'PATCH' : 'POST', token, body: { category_id: String(form.get('category')), title: String(form.get('title')), description: String(form.get('description')), price_cent: cents(String(form.get('price'))), ...(existing ? { expected_version: existing.aggregate_version } : {}) } })
      setEditor(null); setDetail(result.product); setRevision(value => value + 1); setNotice(existing ? '商品信息已保存。' : '草稿已保存，确认后即可发布。')
    } catch (err) { setDialogError(message(err)) } finally { setBusy(false) }
  }
  // changeState 不自动重试写入，确认下架后才提交；成功后使用服务端新版本。
  async function changeState(product: Product, action: 'publish' | 'off-shelf') {
    if (action === 'off-shelf' && !window.confirm('确定下架这件商品？下架后将不在商品广场展示。')) return
    setBusy(true); setDialogError('')
    try { const result = await request<{ product: Product }>(`/products/${product.id}/${action}`, { method: 'POST', token, body: { expected_version: product.aggregate_version } }); setDetail(result.product); setRevision(value => value + 1); setNotice(action === 'publish' ? '商品已上架。' : '商品已下架。') } catch (err) { setDialogError(message(err)) } finally { setBusy(false) }
  }
  const owner = detail && user?.id === detail.seller_id
  const editable = detail && ['PRODUCT_STATUS_DRAFT', 'PRODUCT_STATUS_ON_SALE', 'PRODUCT_STATUS_OFF_SHELF'].includes(detail.status)
  return <>
    <header className="header"><a className="brand" href="#" onClick={() => { setTab('browse'); setPage(1) }}><span className="brand-mark">R</span>ReMarket<span className="brand-caption">二手交易平台</span></a><nav aria-label="主导航"><button className={tab === 'browse' ? 'nav-active' : ''} onClick={() => { setTab('browse'); setPage(1) }}>商品市场</button><button className={tab === 'saved' ? 'nav-active' : ''} onClick={() => { if (!user) setAuthMode('login'); else { setTab('saved'); setPage(1) } }}>商品管理</button></nav><div className="header-actions">{!authReady ? <span className="muted">恢复登录…</span> : user ? <><button className="account" onClick={() => { setNotice(`当前账号：${user.username} · ${user.role === 'ADMIN' ? '管理员' : '普通用户'}`) }}><span className="avatar">{user.nickname?.slice(0, 1) || '我'}</span><span>{user.nickname}</span></button><button className="text-button" onClick={logout}>退出</button></> : <button className="text-button" onClick={() => { setAuthMode('login'); setDialogError('') }}>登录 / 注册</button>}</div></header>
    <main>
      <section className="intro"><h1>{tab === 'browse' ? '商品市场' : '商品管理'}</h1></section>
      {notice && <div className="notice" role="status"><span>{notice}</span><button aria-label="关闭提示" onClick={() => setNotice('')}>×</button></div>}
      <div className="toolbar">{tab === 'browse' ? <div className="categories" aria-label="商品分类"><button className={!category ? 'selected' : ''} onClick={() => { setCategory(''); setPage(1) }}>全部分类</button>{categories.map(item => <button key={item.id} className={category === item.id ? 'selected' : ''} onClick={() => { setCategory(item.id); setPage(1) }}>{item.name}</button>)}</div> : <form className="lookup" onSubmit={event => { event.preventDefault(); if (/^[1-9]\d*$/.test(lookup)) void openProduct(lookup); else setError('请输入有效的商品编号。') }}><label htmlFor="lookup">商品编号</label><input id="lookup" inputMode="numeric" value={lookup} onChange={event => setLookup(event.target.value)} placeholder="输入商品 ID" /><button disabled={busy}>查找</button></form>}</div>
      <div className="section-label"><div className="list-heading"><h2>{tab === 'browse' ? '在售商品' : '我的商品'}</h2><span>{loading ? '加载中' : `${total} 件商品`}</span></div><div className="list-actions"><button className="primary small" onClick={() => { setDialogError(''); if (!user) setAuthMode('login'); else setEditor('new') }}><svg width="14" height="14" viewBox="0 0 16 16" fill="none" aria-hidden="true"><path d="M8 3v10M3 8h10" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round"/></svg><span>新建商品</span></button><button className="text-button refresh" disabled={loading || busy} onClick={() => setRevision(value => value + 1)}><svg width="14" height="14" viewBox="0 0 16 16" fill="none" aria-hidden="true"><path d="M12.8 6A5 5 0 1 0 13 9M12.8 2.8V6H9.6" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round"/></svg><span>刷新</span></button></div></div>
      {error ? <div className="empty error" role="alert"><span className="empty-symbol">!</span><h3>暂时没能加载</h3><p>{error}</p><button onClick={() => setRevision(value => value + 1)}>重新加载</button></div> : loading ? <div className="grid" aria-label="正在加载">{[0, 1, 2, 3].map(i => <div className="skeleton" key={i} />)}</div> : products.length ? <div className="table-panel"><table className="product-table"><thead><tr><th>商品信息</th><th>分类</th><th>价格</th><th>状态</th><th>发布时间</th><th className="action-cell">操作</th></tr></thead><tbody>{products.map(product => <tr key={product.id}><td><div className="goods-info"><div className="goods-thumb" aria-label="暂无商品图片"><svg width="25" height="25" viewBox="0 0 24 24" fill="none" aria-hidden="true"><rect x="3" y="3" width="18" height="18" rx="2" stroke="currentColor" strokeWidth="1.3"/><circle cx="8" cy="8" r="1.5" stroke="currentColor"/><path d="m4 18 5-5 4 3 3-5 5 7" stroke="currentColor" strokeWidth="1.3"/></svg></div><div className="goods-copy"><button className="product-link" disabled={busy} onClick={() => void openProduct(product.id)}>{product.title}</button><span className="goods-description">{product.description || '暂无描述'}</span><span className="goods-id">编号：{product.id}</span></div></div></td><td><span className="category-text">{categories.find(item => item.id === product.category_id)?.name ?? product.category_id}</span></td><td><strong className="table-price">¥ {yuan(product.price_cent)}</strong></td><td><span className={`status-badge state-${product.status.replace('PRODUCT_STATUS_', '').toLowerCase()}`}>{states[product.status] ?? '未知状态'}</span></td><td className="date-cell">{product.first_published_at ? new Date(product.first_published_at).toLocaleDateString('zh-CN') : '未发布'}</td><td className="action-cell"><button className="table-action" disabled={busy} onClick={() => void openProduct(product.id)}>查看详情</button>{user?.id === product.seller_id && ['PRODUCT_STATUS_DRAFT', 'PRODUCT_STATUS_ON_SALE', 'PRODUCT_STATUS_OFF_SHELF'].includes(product.status) && <button className="table-action" disabled={busy} onClick={() => { setDialogError(''); setEditor(product) }}>编辑</button>}</td></tr>)}</tbody></table></div> : <div className="empty"><span className="empty-symbol" aria-hidden="true">—</span><h3>{tab === 'browse' ? '暂无在售商品' : '暂无商品'}</h3><p>{tab === 'browse' ? '可以切换分类，或新建商品并发布后查看。' : '新建商品，或输入商品编号查看详情。'}</p><button className="primary" onClick={() => { setDialogError(''); if (!user) setAuthMode('login'); else setEditor('new') }}>新建商品</button></div>}
      {<div className="pagination"><button disabled={page <= 1 || loading} onClick={() => setPage(value => value - 1)}>← 上一页</button><span>第 {page} 页</span><button disabled={BigInt(page * 12) >= BigInt(total) || loading} onClick={() => setPage(value => value + 1)}>下一页 →</button></div>}
    </main><footer><span>ReMarket 二手交易平台</span><span>商品管理 · 账号服务</span></footer>
    {authMode && <Modal title={authMode === 'login' ? '账号登录' : '注册账号'} busy={busy} close={() => { setAuthMode(null); setDialogError('') }}><p className="modal-subtitle">{authMode === 'login' ? '登录后可发布商品、保存草稿和管理商品状态。' : '填写账号信息，完成注册后即可登录。'}</p><form onSubmit={authenticate} key={authMode}><label>用户名<input name="username" required autoComplete="username" pattern="[A-Za-z][A-Za-z0-9_]{2,31}" placeholder="3–32 位字母、数字或下划线" /></label>{authMode === 'register' && <label>昵称<input name="nickname" required maxLength={64} placeholder="请输入昵称" /></label>}<label>密码<input name="password" type="password" required minLength={8} autoComplete={authMode === 'login' ? 'current-password' : 'new-password'} placeholder="至少 8 个字符，最多 72 字节" /></label>{dialogError && <p className="form-error" role="alert">{dialogError}</p>}<button className="primary full" disabled={busy}>{busy ? '处理中…' : authMode === 'login' ? '登录' : '创建账号'}</button></form><p className="switch-auth">{authMode === 'login' ? '还没有账号？' : '已经有账号？'}<button disabled={busy} onClick={() => { setAuthMode(authMode === 'login' ? 'register' : 'login'); setDialogError('') }}>{authMode === 'login' ? '立即注册' : '去登录'}</button></p></Modal>}
    {detail && !editor && <Modal title="商品详情" busy={busy} close={() => setDetail(null)}><div className="detail-heading"><span className="pill">{states[detail.status]}</span><span className="muted">商品 #{detail.id}</span></div><h3 className="detail-title">{detail.title}</h3><p className="detail-price">¥ {yuan(detail.price_cent)}</p><p className="description">{detail.description || '暂无描述'}</p><dl><dt>分类</dt><dd>{categories.find(item => item.id === detail.category_id)?.name ?? detail.category_id}</dd><dt>卖家编号</dt><dd>{detail.seller_id}</dd><dt>发布时间</dt><dd>{detail.first_published_at ? new Date(detail.first_published_at).toLocaleString('zh-CN') : '尚未发布'}</dd></dl>{detail.images?.length ? <p className="scope-note">此商品有 {detail.images.length} 张图片记录，图片展示将在上传服务接入后开放。</p> : null}{dialogError && <p className="form-error" role="alert">{dialogError}<button disabled={busy} onClick={() => void openProduct(detail.id)}>刷新详情</button></p>}<div className="detail-actions">{owner && editable && <button disabled={busy} onClick={() => { setEditor(detail); setDialogError('') }}>编辑商品</button>}{owner && ['PRODUCT_STATUS_DRAFT', 'PRODUCT_STATUS_OFF_SHELF'].includes(detail.status) && <button className="primary" disabled={busy} onClick={() => void changeState(detail, 'publish')}>{busy ? '处理中…' : detail.status === 'PRODUCT_STATUS_DRAFT' ? '确认发布' : '重新上架'}</button>}{(owner || user?.role === 'ADMIN') && detail.status === 'PRODUCT_STATUS_ON_SALE' && <button disabled={busy} onClick={() => void changeState(detail, 'off-shelf')}>{busy ? '处理中…' : '下架商品'}</button>}</div></Modal>}
    {editor && <Modal title={editor === 'new' ? '新建商品' : '编辑商品'} busy={busy} close={() => { setEditor(null); setDialogError('') }}><p className="modal-subtitle">先保存草稿，检查无误后再上架。</p><form key={editor === 'new' ? 'new' : editor.aggregate_version} onSubmit={saveDraft}><label>商品名称<input name="title" required maxLength={128} defaultValue={editor === 'new' ? '' : editor.title} placeholder="例如：用了半年的机械键盘" /></label><div className="form-row"><label>分类<select name="category" required defaultValue={editor === 'new' ? '' : editor.category_id}><option value="" disabled>选择分类</option>{editor !== 'new' && !categories.some(item => item.id === editor.category_id) && <option value={editor.category_id}>原分类（可能已停用）</option>}{categories.map(item => <option key={item.id} value={item.id}>{item.name}</option>)}</select></label><label>价格 / 元<input name="price" required inputMode="decimal" defaultValue={editor === 'new' ? '' : yuan(editor.price_cent)} placeholder="0.00" /></label></div><label>商品描述<textarea name="description" rows={4} defaultValue={editor === 'new' ? '' : editor.description} placeholder="请输入商品成色、使用情况及其他说明。" /></label><p className="scope-note">目前支持文字发布，图片上传后续开放。</p>{dialogError && <p className="form-error" role="alert">{dialogError}{editor !== 'new' && <button type="button" disabled={busy} onClick={() => { const id = editor.id; setEditor(null); void openProduct(id) }}>刷新商品后重试</button>}</p>}<button className="primary full" disabled={busy || !categories.length}>{busy ? '保存中…' : editor === 'new' ? '保存草稿' : '保存修改'}</button></form></Modal>}
  </>
}
export default App
