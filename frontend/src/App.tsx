import { useCallback, useEffect, useState } from 'react'
import type { FormEvent } from 'react'
import { ApiError, cents, request, yuan } from './api/client'
import type { Category, Product, User } from './api/client'
import { Modal } from './components/Modal'
import { Checkout, OrdersPage } from './components/Orders'
import type { OrderRole } from './api/orders'
import { AccountStats, ProfilePage, SellerCard } from './components/Profiles'
import type { ProfileSection } from './components/Profiles'
import { ProductImages } from './components/ProductImages'
import { SearchMarket } from './components/SearchMarket'
import './App.css'
import './components/Orders.css'

interface ProductStatistics { total?: string; on_sale?: string; sold?: string; off_shelf?: string }
const states: Record<string, string> = { PRODUCT_STATUS_DRAFT: '草稿', PRODUCT_STATUS_ON_SALE: '在售', PRODUCT_STATUS_OFF_SHELF: '已下架', PRODUCT_STATUS_RESERVED: '已预占', PRODUCT_STATUS_SOLD: '已售出' }
// readToken 仅在当前浏览器会话保留登录，存储不可用时退化为内存状态。
function readToken() { try { return sessionStorage.getItem('remarket.token') ?? '' } catch { return '' } }
// CategoryIcon 使用统一线性图标区分分类，不依赖外部图片请求。
function CategoryIcon({ index }: { index: number }) {
  const paths = ['M3 3h6v6H3z M15 3h6v6h-6z M3 15h6v6H3z M15 15h6v6h-6z','M5 4h14v12H5z M2 20h20 M8 17h8','M12 5c-3-2-6-2-10-1v15c4-1 7-1 10 1 3-2 6-2 10-1V4c-4-1-7-1-10 1v15','M5 7h12v9a5 5 0 0 1-5 5h-2a5 5 0 0 1-5-5V7z M17 8h2a3 3 0 0 1 0 6h-2 M8 3h6','M5 8h14l2 13H3L5 8z M8 10V6a4 4 0 0 1 8 0v4','m4 15 4-6 4 3 4-6 M7 21l4-7 5 3 4-6 M13 3h.01','M4 12h.01 M12 12h.01 M20 12h.01']
  return <svg className="category-icon" width="28" height="28" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth={index === 6 ? 4 : 1.6} strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={paths[index % paths.length]}/></svg>
}
// ManagementIcon 使用一致的线性图标表达商品统计状态。
function ManagementIcon({ state }: { state: string }) { const path = state === 'ON_SALE' ? 'M9 11l2 2 4-4 M5 4h14v16H5z' : state === 'SOLD' ? 'M4 7h16v13H4z M8 7V4h8v3 M9 13h6 M12 10v6' : state === 'OFF_SHELF' ? 'M4 5h16v4H4z M6 9v11h12V9 M9 13h6' : 'M3 3h7v7H3z M14 3h7v7h-7z M3 14h7v7H3z M14 14h7v7h-7z'; return <svg width="23" height="23" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d={path}/></svg> }
// App 组织商品浏览、账号与编辑流程，所有业务数据从 Gateway 获取。
function App() {
  const [token, setToken] = useState(readToken)
  const [user, setUser] = useState<User | null>(null)
  const [authReady, setAuthReady] = useState(!token)
  const [tab, setTab] = useState<'browse' | 'saved' | 'orders' | 'publish' | 'profile'>('browse')
  const [categories, setCategories] = useState<Category[]>([])
  const [profileId, setProfileId] = useState('')
  const [profileSection, setProfileSection] = useState<ProfileSection>('products')
  const [socialRevision, setSocialRevision] = useState(0)
  const [category, setCategory] = useState('')
  const [products, setProducts] = useState<Product[]>([])
  const [page, setPage] = useState(1)
  const [total, setTotal] = useState('0')
  const [statistics, setStatistics] = useState<ProductStatistics | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [notice, setNotice] = useState('')
  const [revision, setRevision] = useState(0)
  const [authMode, setAuthMode] = useState<'login' | 'register' | null>(null)
  const [editor, setEditor] = useState<Product | 'new' | null>(null)
  const [detail, setDetail] = useState<Product | null>(null)
  const [busy, setBusy] = useState(false)
  const [dialogError, setDialogError] = useState('')
  const [search, setSearch] = useState('')
  const [statusFilter, setStatusFilter] = useState('')
  const [orderRole, setOrderRole] = useState<OrderRole>('buyer')
  const [checkout, setCheckout] = useState<Product | null>(null)
  const [initialOrderId, setInitialOrderId] = useState<string | null>(null)

  // logout 清理会话凭据及受保护页面状态；不宣称撤销已签发的 JWT。
  const logout = useCallback(() => { try { sessionStorage.removeItem('remarket.token') } catch { /* 浏览器可能禁用存储。 */ } setToken(''); setUser(null); setStatistics(null); setCheckout(null); setInitialOrderId(null); setTab('browse'); setDetail(null); setEditor(null); setAuthReady(true) }, [])
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
    if (tab !== 'saved') return () => controller.abort()
    queueMicrotask(() => { if (!controller.signal.aborted) { setLoading(true); setError('') } })
    // 商品管理只读取当前账号商品，商城搜索由独立组件负责。
    const work = user ? request<{ products?: Product[]; total?: string; statistics?: ProductStatistics }>(`/products/mine?page=${page}&page_size=12`, { token, signal: controller.signal }) : Promise.resolve({ products: [], total: '0' })
    work.then(data => { if (controller.signal.aborted) return; if (tab === 'saved') setStatistics('statistics' in data ? data.statistics ?? null : null); setProducts(data.products ?? []); setTotal(data.total ?? '0') }).catch(err => { if (!controller.signal.aborted) { setProducts([]); if (tab === 'saved') setStatistics(null); setError(message(err)) } }).finally(() => { if (!controller.signal.aborted) setLoading(false) })
    return () => controller.abort()
  }, [tab, page, category, revision, user, token, message])

  // 成功通知在三秒后自动清理，新通知出现时重置计时，卸载时释放定时器。
  useEffect(() => {
    if (!notice) return
    const timer = window.setTimeout(() => setNotice(''), 3000)
    return () => window.clearTimeout(timer)
  }, [notice])

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
    event.preventDefault(); const form = new FormData(event.currentTarget); const publishNow = (event.nativeEvent as SubmitEvent).submitter instanceof HTMLButtonElement && ((event.nativeEvent as SubmitEvent).submitter as HTMLButtonElement).value === 'publish'; setBusy(true); setDialogError('')
    try {
      const files = form.getAll('photos').filter((value): value is File => value instanceof File && value.size > 0)
      const existing = editor !== 'new' ? editor : null
      if (files.length + (existing?.images?.length ?? 0) > 9) throw new Error('每件商品最多 9 张图片。')
      for (const file of files) { if (!['image/jpeg', 'image/png'].includes(file.type) || file.size > 5 * 1024 * 1024) throw new Error('图片仅支持 JPEG/PNG，每张不超过 5MB。') }
      let result = await request<{ product: Product }>(existing ? `/products/${existing.id}` : '/products', { method: existing ? 'PATCH' : 'POST', token, body: { category_id: String(form.get('category')), title: String(form.get('title')), description: String(form.get('description')), price_cent: cents(String(form.get('price'))), ...(existing ? { expected_version: existing.aggregate_version } : {}) } })
      setEditor(result.product)
      // 先保存商品再直传，逐张确认后使用最新商品版本发布；失败时已保存的图片仍可在编辑页查看。
      for (const file of files) {
        const upload = await request<{ upload_id: string; upload_url: string }>(`/products/${result.product.id}/images/uploads`, { method: 'POST', token, body: { content_type: file.type, size_bytes: file.size } })
        const response = await fetch(upload.upload_url, { method: 'PUT', body: file, headers: { 'Content-Type': file.type }, signal: AbortSignal.timeout(60000) })
        if (!response.ok) throw new Error('图片上传失败，请检查网络后重新选择图片。')
        result = await request<{ product: Product }>(`/products/${result.product.id}/images/complete`, { method: 'POST', token, body: { upload_id: upload.upload_id }, timeoutMs: 35000 })
        setEditor(result.product)
      }
      if (publishNow) { await request(`/products/${result.product.id}/publish`, { method: 'POST', token, body: { expected_version: result.product.aggregate_version } }) }
      setEditor(null); setDetail(tab === 'publish' ? null : result.product); if (tab === 'publish') { navigate('saved') } setRevision(value => value + 1); setNotice(publishNow ? '商品已发布。' : existing ? '商品信息已保存。' : '草稿已保存。')
    } catch (err) { setDialogError(message(err)) } finally { setBusy(false) }
  }
  // changeState 不自动重试写入，确认下架后才提交；成功后使用服务端新版本。
  async function changeState(product: Product, action: 'publish' | 'off-shelf') {
    if (action === 'off-shelf' && !window.confirm('确定下架这件商品？下架后将不在商品广场展示。')) return
    setBusy(true); setDialogError('')
    try { const result = await request<{ product: Product }>(`/products/${product.id}/${action}`, { method: 'POST', token, body: { expected_version: product.aggregate_version } }); setDetail(result.product); setRevision(value => value + 1); setNotice(action === 'publish' ? '商品已上架。' : '商品已下架。') } catch (err) { setDialogError(message(err)) } finally { setBusy(false) }
  }
  // refreshEditor 刷新编辑快照并保留所在界面，避免版本冲突重试时丢失编辑入口。
  async function refreshEditor(id: string) { setBusy(true); try { const result = await request<{ product: Product }>(`/products/${id}`, { token }); setEditor(result.product); setDialogError('已刷新商品，请核对后重新提交。') } catch (err) { setDialogError(message(err)) } finally { setBusy(false) } }
  // navigate 清理离开发布页的编辑状态，避免切换导航后意外弹出编辑框。
  function navigate(next: 'browse' | 'saved' | 'orders') { setEditor(null); setDialogError(''); setTab(next); setSearch(''); setCategory(''); setStatusFilter(''); setPage(1) }
  // openProfile 清理详情浮层后进入目标主页，切换用户时重置列表。
  function openProfile(id: string, section: ProfileSection = 'products') { setDetail(null); setEditor(null); setDialogError(''); setProfileId(id); setProfileSection(section); setTab('profile'); document.querySelectorAll('.user-dropdown[open]').forEach(node => node.removeAttribute('open')) }
  // socialChanged 刷新关系计数及关注商品流。
  function socialChanged() { setSocialRevision(value => value + 1); setRevision(value => value + 1) }
  // startPublishing 打开独立发布页，未登录时先进入登录流程。
  function startPublishing() { setDialogError(''); if (!user) { setAuthMode('login'); return } setEditor('new'); setTab('publish') }
  const visibleProducts = products.filter(product => (!statusFilter || product.status === statusFilter) && (!category || product.category_id === category) && product.title.toLowerCase().includes(search.trim().toLowerCase()))
  const editorForm = editor && (<form key={editor === 'new' ? 'new' : editor.aggregate_version} onSubmit={saveDraft}><label className="publish-field"><span>商品名称 <em>*</em></span><input name="title" required maxLength={128} defaultValue={editor === 'new' ? '' : editor.title} placeholder="例如：用了半年的机械键盘" /></label><div className="form-row"><label className="publish-field"><span>分类 <em>*</em></span><select name="category" required defaultValue={editor === 'new' ? '' : editor.category_id}><option value="" disabled>选择分类</option>{editor !== 'new' && !categories.some(item => item.id === editor.category_id) && <option value={editor.category_id}>原分类（可能已停用）</option>}{categories.map(item => <option key={item.id} value={item.id}>{item.name}</option>)}</select></label><label className="publish-field"><span>价格（元）<em>*</em></span><input name="price" required inputMode="decimal" defaultValue={editor === 'new' ? '' : yuan(editor.price_cent)} placeholder="0.00" /></label></div><label className="publish-field description-field"><span>商品描述</span><textarea name="description" rows={4} defaultValue={editor === 'new' ? '' : editor.description} placeholder="请输入商品成色、使用情况及其他说明。" /></label><section className="publish-images"><h2 className="panel-title">商品图片 <span>可选</span></h2><label className="image-upload-field"><span>选择商品图片</span><input name="photos" type="file" accept="image/jpeg,image/png" multiple disabled={busy} /><small>支持 JPEG、PNG，每张不超过 5MB，最多 9 张。保存商品时上传。</small></label>{editor !== 'new' && <ProductImages key={`${editor.id}:${editor.aggregate_version}`} id={editor.id} token={token} gallery />}</section>{dialogError && <p className="form-error" role="alert">{dialogError}{editor !== 'new' && <button type="button" disabled={busy} onClick={() => { void refreshEditor(editor.id) }}>刷新商品后重试</button>}</p>}<div className="publish-actions"><button type="submit" name="intent" value="draft" disabled={busy || !categories.length}>{busy ? '保存中…' : editor === 'new' ? '保存草稿' : '保存修改'}</button>{tab === 'publish' && <button type="submit" name="intent" value="publish" className="primary" disabled={busy || !categories.length}>{busy ? '处理中…' : '立即发布 →'}</button>}</div></form> )
  const owner = detail && user?.id === detail.seller_id
  const editable = detail && ['PRODUCT_STATUS_DRAFT', 'PRODUCT_STATUS_ON_SALE', 'PRODUCT_STATUS_OFF_SHELF'].includes(detail.status)
  return <>
    <header className="header order-enabled-header"><a className="brand" href="#" onClick={() => { navigate('browse') }}><svg className="bag-mark" width="25" height="28" viewBox="0 0 24 28" fill="none" aria-hidden="true"><path d="M5 8h14l2 17H3L5 8Z" stroke="currentColor" strokeWidth="1.8" strokeLinejoin="round"/><path d="M8 11V6a4 4 0 0 1 8 0v5" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round"/></svg><span className="brand-copy">ReMarket<small>二手交易平台</small></span></a><nav aria-label="主导航"><button className={tab === 'browse' ? 'nav-active' : ''} onClick={() => { navigate('browse') }}>首页</button><button className={tab === 'publish' ? 'nav-active' : ''} onClick={startPublishing}>发布商品</button><button className={tab === 'saved' ? 'nav-active' : ''} onClick={() => { if (!user) setAuthMode('login'); else { navigate('saved') } }}>我的商品</button><button className={tab === 'orders' ? 'nav-active' : ''} onClick={() => { if (!user) { setDialogError(''); setAuthMode('login') } else { setInitialOrderId(null); navigate('orders') } }}>我的订单</button></nav><div className="header-actions">{!authReady ? <span className="muted">恢复登录…</span> : user ? <><details className="user-dropdown" onToggle={event => { if (event.currentTarget.open) setSocialRevision(value => value + 1) }}><summary><span className="avatar">{user.nickname?.slice(0, 1) || '我'}</span><span className="user-name">{user.nickname}</span><svg width="12" height="12" viewBox="0 0 16 16" fill="none" aria-hidden="true"><path d="m4 6 4 4 4-4" stroke="currentColor" strokeWidth="1.4"/></svg></summary><div className="user-dropdown-panel"><div className="dropdown-account"><strong>{user.nickname}</strong><span>{user.username} · {user.role === 'ADMIN' ? '管理员' : '普通用户'}</span></div><AccountStats id={user.id} token={token} revision={socialRevision} open={section => openProfile(user.id, section)} /><button onClick={() => openProfile(user.id)}>个人主页</button><button onClick={event => { event.currentTarget.closest('details')?.removeAttribute('open'); navigate('saved') }}>我的商品</button><button onClick={logout}>退出登录</button></div></details></> : <button className="text-button" onClick={() => { setAuthMode('login'); setDialogError('') }}>登录 / 注册</button>}</div></header>

    <main className={tab === 'browse' ? 'market-main' : tab === 'saved' ? 'manage-main product-manage-main' : 'manage-main'}>
      {tab === 'browse' && <section className="search-hero"><div className="hero-copy"><h1>发现值得买的闲置好物</h1><p>数码 · 图书 · 生活 · 运动 · 更多</p><button className="primary" onClick={startPublishing}>立即发布商品 <span>→</span></button></div><div className="hero-objects" aria-hidden="true"><div className="hero-orbit"/><div className="hero-book book-one"/><div className="hero-book book-two"/><div className="hero-bag"/><div className="hero-box"><span>↑</span></div><div className="hero-camera"><i/></div><div className="hero-plant"><i/><i/><i/><span/></div></div></section>}
    {tab === 'browse' && <div className="market-category-bar"><div className="categories" aria-label="商品分类"><button className={!category ? 'selected' : ''} onClick={() => { setCategory(''); setPage(1) }}><CategoryIcon index={0}/>全部分类</button>{categories.map(item => <button key={item.id} className={category === item.id ? 'selected' : ''} onClick={() => { setCategory(item.id); setPage(1) }}><CategoryIcon index={categories.indexOf(item) + 1}/>{item.name}</button>)}</div></div>}
      {tab === 'saved' && <><section className="intro"><div><h1>商品管理</h1><p className="muted">管理你发布的商品，可查看、编辑或下架</p></div><button className="primary" onClick={startPublishing}>＋ 新建商品</button></section><div className="management-stats"><div><span className="stat-icon blue"><ManagementIcon state="ALL"/></span><div><span>全部商品</span><strong>{loading ? '—' : statistics ? statistics.total ?? '0' : '—'}</strong></div></div>{[['ON_SALE','在售商品','green'],['SOLD','已售出','orange'],['OFF_SHELF','已下架','gray']].map(([state,label,tone]) => <div key={state}><span className={`stat-icon ${tone}`}><ManagementIcon state={state}/></span><div><span>{label}</span><strong>{loading ? '—' : statistics ? statistics[state === 'ON_SALE' ? 'on_sale' : state === 'SOLD' ? 'sold' : 'off_shelf'] ?? '0' : '—'}</strong></div></div>)}</div></>}
      {tab === 'publish' && <section className="publish-page"><div className="intro"><div><h1>发布商品</h1><p className="muted">发布闲置物品，快速找到合适的买家</p></div></div><div className="publish-panel"><h2 className="panel-title">商品信息</h2>{editorForm}</div></section>}
      {notice && <div className="toast" role="status" aria-live="polite"><svg width="18" height="18" viewBox="0 0 24 24" fill="none" aria-hidden="true"><circle cx="12" cy="12" r="9" stroke="currentColor" strokeWidth="1.5"/><path d="m8 12 3 3 5-6" stroke="currentColor" strokeWidth="1.7" strokeLinecap="round" strokeLinejoin="round"/></svg><span>{notice}</span><button aria-label="关闭提示" onClick={() => setNotice('')}>×</button></div>}

      {tab === 'browse' && error && <p className="form-error" role="alert">{error}<button onClick={() => setError('')}>关闭</button></p>}
      {tab === 'browse' && <SearchMarket key={`${category}:${revision}:${user?.id ?? 'guest'}`} category={category} categories={categories} token={token} signedIn={!!user} login={() => { setDialogError(''); setAuthMode('login') }} open={id => { void openProduct(id) }} />}
      {(tab === 'saved') && <div className={tab === 'saved' ? 'management-panel' : 'market-list'}>{tab === 'saved' && <div className="manage-filters"><label className="manage-search"><svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" aria-hidden="true"><circle cx="10.5" cy="10.5" r="6.5"/><path d="m16 16 4.5 4.5"/></svg><input aria-label="搜索当前页商品" placeholder="搜索本页商品名称…" value={search} onChange={e => setSearch(e.target.value)}/></label><select aria-label="筛选当前页分类" value={category} onChange={e => setCategory(e.target.value)}><option value="">全部分类</option>{categories.map(item => <option key={item.id} value={item.id}>{item.name}</option>)}</select><select aria-label="筛选当前页状态" value={statusFilter} onChange={e => setStatusFilter(e.target.value)}><option value="">全部状态</option>{Object.entries(states).map(([value,label]) => <option key={value} value={value}>{label}</option>)}</select><button onClick={() => { setSearch('');setCategory('');setStatusFilter('') }}>重置</button>{tab === 'saved' && <button className="text-button refresh" disabled={loading || busy} onClick={() => setRevision(value => value + 1)}><svg width="14" height="14" viewBox="0 0 16 16" fill="none" aria-hidden="true"><path d="M12.8 6A5 5 0 1 0 13 9M12.8 2.8V6H9.6" stroke="currentColor" strokeWidth="1.4" strokeLinecap="round" strokeLinejoin="round"/></svg><span>刷新</span></button>}</div>}
      <div className="section-label"><div className="list-heading"><h2>我的商品</h2><span>{loading ? '加载中' : `${total} 件商品`}</span></div></div>
      {error ? <div className="empty error" role="alert"><span className="empty-symbol">!</span><h3>暂时没能加载</h3><p>{error}</p><button onClick={() => setRevision(value => value + 1)}>重新加载</button></div> : loading ? <div className="grid" aria-label="正在加载">{[0, 1, 2, 3, 4, 5].map(i => <div className="skeleton" key={i} />)}</div> : products.length ? <div className="table-panel"><table className="product-table"><thead><tr><th>商品信息</th><th>分类</th><th>价格</th><th>状态</th><th>发布时间</th><th className="action-cell">操作</th></tr></thead><tbody>{!visibleProducts.length && <tr><td colSpan={6} className="filter-empty">本页没有匹配的商品，请调整筛选条件。</td></tr>}{visibleProducts.map(product => <tr key={product.id}><td><div className="goods-info"><div className="goods-thumb"><ProductImages key={`${product.id}:${product.aggregate_version}`} id={product.id} token={token} /></div><div className="goods-copy"><button className="product-link" disabled={busy} onClick={() => void openProduct(product.id)}>{product.title}</button><span className="goods-description">{product.description || '暂无描述'}</span><span className="goods-id">编号：{product.id}</span></div></div></td><td><span className="category-text">{categories.find(item => item.id === product.category_id)?.name ?? product.category_id}</span></td><td><strong className="table-price">¥ {yuan(product.price_cent)}</strong></td><td><span className={`status-badge state-${product.status.replace('PRODUCT_STATUS_', '').toLowerCase()}`}>{states[product.status] ?? '未知状态'}</span></td><td className="date-cell">{product.first_published_at ? new Date(product.first_published_at).toLocaleDateString('zh-CN') : '未发布'}</td><td className="action-cell"><button className="table-action" disabled={busy} onClick={() => void openProduct(product.id)}>查看详情</button>{user?.id === product.seller_id && ['PRODUCT_STATUS_DRAFT', 'PRODUCT_STATUS_ON_SALE', 'PRODUCT_STATUS_OFF_SHELF'].includes(product.status) && <button className="table-action" disabled={busy} onClick={() => { setDialogError(''); setEditor(product) }}>编辑</button>}</td></tr>)}</tbody></table></div> : <div className="empty"><span className="empty-symbol" aria-hidden="true">—</span><h3>暂无商品</h3><p>新建商品，或输入商品编号查看详情。</p><button className="primary" onClick={() => { setDialogError(''); if (!user) setAuthMode('login'); else startPublishing() }}>新建商品</button></div>}
      {<div className="pagination">{tab === 'saved' && <span className="pagination-total">共 {total} 条</span>}<button disabled={page <= 1 || loading} onClick={() => setPage(value => value - 1)}>← 上一页</button><span>第 {page} 页</span><button disabled={BigInt(page * 12) >= BigInt(total) || loading} onClick={() => setPage(value => value + 1)}>下一页 →</button></div>}
      </div>}
      {tab === 'profile' && <ProfilePage key={`${profileId}:${profileSection}:${user?.id ?? 'guest'}`} id={profileId} token={token} revision={socialRevision} initialSection={profileSection} openProduct={id => { void openProduct(id) }} openProfile={openProfile} back={() => navigate('browse')} login={() => { setDialogError(''); setAuthMode('login') }} changed={socialChanged} />}
      {tab === 'orders' && user && <OrdersPage key={`${user.id}:${orderRole}`} user={user} token={token} role={orderRole} changeRole={setOrderRole} message={message} initialId={initialOrderId} clearInitial={() => setInitialOrderId(null)} browse={() => { navigate('browse') }} />}
    </main><footer><span>ReMarket 二手交易平台</span><span>商品管理 · 账号服务</span></footer>
    {authMode && <Modal title={authMode === 'login' ? '账号登录' : '注册账号'} busy={busy} close={() => { setAuthMode(null); setDialogError('') }}><p className="modal-subtitle">{authMode === 'login' ? '登录后可发布商品、保存草稿和管理商品状态。' : '填写账号信息，完成注册后即可登录。'}</p><form onSubmit={authenticate} key={authMode}><label>用户名<input name="username" required autoComplete="username" pattern="[A-Za-z][A-Za-z0-9_]{2,31}" placeholder="3–32 位字母、数字或下划线" /></label>{authMode === 'register' && <label>昵称<input name="nickname" required maxLength={64} placeholder="请输入昵称" /></label>}<label>密码<input name="password" type="password" required minLength={8} autoComplete={authMode === 'login' ? 'current-password' : 'new-password'} placeholder="至少 8 个字符，最多 72 字节" /></label>{dialogError && <p className="form-error" role="alert">{dialogError}</p>}<button className="primary full" disabled={busy}>{busy ? '处理中…' : authMode === 'login' ? '登录' : '创建账号'}</button></form><p className="switch-auth">{authMode === 'login' ? '还没有账号？' : '已经有账号？'}<button disabled={busy} onClick={() => { setAuthMode(authMode === 'login' ? 'register' : 'login'); setDialogError('') }}>{authMode === 'login' ? '立即注册' : '去登录'}</button></p></Modal>}
    {detail && !editor && !checkout && <Modal title="ReMarket · 商品详情" className="product-detail-dialog" busy={busy} close={() => setDetail(null)}><div className="product-detail-layout"><div className="product-detail-visual"><ProductImages key={`${detail.id}:${detail.aggregate_version}`} id={detail.id} token={token} interactive /></div><div className="product-detail-content"><div className="detail-heading"><span className={`status-badge state-${detail.status.replace('PRODUCT_STATUS_', '').toLowerCase()}`}>{states[detail.status]}</span><span className="muted">商品 #{detail.id}</span></div><h3 className="detail-title">{detail.title}</h3><p className="detail-price">¥ {yuan(detail.price_cent)}</p><p className="detail-category muted">{categories.find(item => item.id === detail.category_id)?.name ?? '二手商品'}{detail.first_published_at ? ` · ${new Date(detail.first_published_at).toLocaleDateString('zh-CN')} 发布` : ' · 尚未发布'}</p><SellerCard key={detail.seller_id} id={detail.seller_id} token={token} revision={socialRevision} open={openProfile} login={() => { setDetail(null); setDialogError(''); setAuthMode('login') }} changed={socialChanged} /><section className="product-description"><h4>商品描述</h4><p className="description">{detail.description || '卖家暂未填写商品描述'}</p></section>{dialogError && <p className="form-error" role="alert">{dialogError}<button disabled={busy} onClick={() => void openProduct(detail.id)}>刷新详情</button></p>}<div className="detail-actions">{detail.status === 'PRODUCT_STATUS_ON_SALE' && !owner && <button className="primary" disabled={busy || !authReady} onClick={() => { if (!user) { setDetail(null); setAuthMode('login'); setDialogError('') } else setCheckout(detail) }}>{user ? '立即购买' : '登录后购买'}</button>}{owner && editable && <button disabled={busy} onClick={() => { setEditor(detail); setDialogError('') }}>编辑商品</button>}{owner && ['PRODUCT_STATUS_DRAFT', 'PRODUCT_STATUS_OFF_SHELF'].includes(detail.status) && <button className="primary" disabled={busy} onClick={() => void changeState(detail, 'publish')}>{busy ? '处理中…' : detail.status === 'PRODUCT_STATUS_DRAFT' ? '确认发布' : '重新上架'}</button>}{(owner || user?.role === 'ADMIN') && detail.status === 'PRODUCT_STATUS_ON_SALE' && <button disabled={busy} onClick={() => void changeState(detail, 'off-shelf')}>{busy ? '处理中…' : '下架商品'}</button>}</div></div></div></Modal>}
    {checkout && user && <Checkout key={`${user.id}:${checkout.id}`} product={checkout} user={user} token={token} message={message} inspectOrders={() => { setCheckout(null); setDetail(null); setInitialOrderId(null); setOrderRole('buyer'); setTab('orders') }} close={() => setCheckout(null)} created={order => { setCheckout(null); setDetail(null); setInitialOrderId(order.id); setOrderRole('buyer'); setTab('orders'); setRevision(value => value + 1) }} />}
    {editor && tab !== 'publish' && <Modal title="编辑商品" busy={busy} close={() => { setEditor(null); setDialogError('') }}>{editorForm}</Modal>}
  </>
}
export default App
