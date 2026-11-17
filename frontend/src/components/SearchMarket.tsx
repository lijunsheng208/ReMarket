import { useEffect, useRef, useState } from 'react'
import { ProductImages } from './ProductImages'
import type { FormEvent } from 'react'
import { ApiError, request, yuan } from '../api/client'
import type { Category } from '../api/client'

interface SearchProduct { id: string; category_id: string; title: string; price_cent: string }
interface Filters { query: string; sort: string; min: string; max: string }
const initial: Filters = { query: '', sort: 'newest', min: '', max: '' }
// priceBound 将元精确转换为非负 BIGINT 分；空值表示没有边界。
function priceBound(value: string): string | undefined {
  if (!value) return undefined
  if (!/^\d+(\.\d{1,2})?$/.test(value)) throw new Error('价格须为非负数，最多两位小数。')
  const [whole, fraction = ''] = value.split('.')
  const result = BigInt(whole) * 100n + BigInt(fraction.padEnd(2, '0'))
  if (result > 9223372036854775807n) throw new Error('价格超出允许范围。')
  return result.toString()
}
// SearchMarket 根据浏览场景选择 Feed 或搜索，切换时放弃旧游标并取消过时请求。
export function SearchMarket({ category, categories, token, signedIn, login, open }: { category: string; categories: Category[]; token: string; signedIn: boolean; login: () => void; open: (id: string) => void }) {
  const [mode, setMode] = useState<'home' | 'search' | 'following'>(category ? 'search' : 'home')
  const [draft, setDraft] = useState(initial)
  const [selectedCategory, setSelectedCategory] = useState(category)
  const [appliedCategory, setAppliedCategory] = useState(category)
  const [filters, setFilters] = useState(initial)
  const [cursor, setCursor] = useState('')
  const [next, setNext] = useState('')
  const [history, setHistory] = useState<string[]>([])
  const [products, setProducts] = useState<SearchProduct[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [expired, setExpired] = useState(false)
  const [notice, setNotice] = useState('')
  const recoveredCursor = useRef(false)
  const [revision, setRevision] = useState(0)
  useEffect(() => {
    if (recoveredCursor.current) { recoveredCursor.current = false; return }
    const controller = new AbortController()
    queueMicrotask(() => { if (!controller.signal.aborted) { setLoading(true); setError(''); setExpired(false); setNotice('') } })
    const params = new URLSearchParams({ page_size: '12' })
    if (mode === 'search') {
      params.set('query', filters.query); params.set('sort', filters.sort)
      if (appliedCategory) params.set('category_id', appliedCategory)
      if (filters.min) params.set('min_price_cent', filters.min)
      if (filters.max) params.set('max_price_cent', filters.max)
    }
    if (cursor) params.set('cursor', cursor)
    const path = mode === 'search' ? '/search/products' : `/feed/${mode}`
    // loadPage 仅对带旧游标的过期请求恢复一次；新快照从第一页开始，禁止复用旧排序位置。
    async function loadPage() {
      try {
        return await request<{ products?: SearchProduct[]; next_cursor?: string }>(`${path}?${params}`, { signal: controller.signal, token: mode === 'following' ? token : undefined })
      } catch (err) {
        if (controller.signal.aborted || mode !== 'search' || !cursor || !(err instanceof ApiError) || err.status !== 409) throw err
        params.delete('cursor')
        // 先清除失效分页状态；更新 cursor 交给首屏请求成功后执行，避免触发重复请求。
        const data = await request<{ products?: SearchProduct[]; next_cursor?: string }>(`${path}?${params}`, { signal: controller.signal })
        if (!controller.signal.aborted) {
          recoveredCursor.current = true
          setCursor('')
          setHistory([])
          setNotice('搜索结果已过期，已为你刷新并返回第一页。')
        }
        return data
      }
    }
    loadPage()
      .then(data => { if (!controller.signal.aborted) { setProducts(data.products ?? []); setNext(data.next_cursor ?? '') } })
      .catch(err => { if (!controller.signal.aborted) { setProducts([]); setNext(''); setExpired(err instanceof ApiError && mode === 'search' && err.status === 409); setError(err instanceof Error ? err.message : '商品加载失败，请重试。') } })
      .finally(() => { if (!controller.signal.aborted) setLoading(false) })
    return () => controller.abort()
  }, [appliedCategory, filters, cursor, revision, mode, token])
  // restart 放弃当前来源的游标；搜索 PIT 由服务端 TTL 回收。
  function restart() { setCursor(''); setHistory([]); setNext(''); setRevision(value => value + 1) }
  // submit 校验用户输入，保持金额精度，并将条件和分页一次性重置。
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    try {
      const min = priceBound(draft.min), max = priceBound(draft.max)
      if (min !== undefined && max !== undefined && BigInt(min) > BigInt(max)) throw new Error('最低价格不能高于最高价格。')
      if (draft.sort === 'relevance' && !draft.query.trim()) throw new Error('相关性排序需要关键词。')
      setMode(draft.query.trim() || selectedCategory || min !== undefined || max !== undefined || draft.sort !== 'newest' ? 'search' : 'home')
      setAppliedCategory(selectedCategory)
      setFilters({ ...draft, query: draft.query.trim(), min: min ?? '', max: max ?? '' }); restart()
    } catch (err) { setError(err instanceof Error ? err.message : '筛选条件无效。') }
  }
  // switchFeed 清理搜索条件及分页；关注视图隐藏全站筛选，未登录时仅打开登录入口。
  function switchFeed(nextMode: 'home' | 'following') {
    if (nextMode === 'following' && !signedIn) { login(); return }
    setMode(nextMode); setDraft(initial); setFilters(initial); setSelectedCategory(''); setAppliedCategory(''); restart()
  }
  return <section className="market-list" aria-label="商品浏览" data-feed-mode={mode}>
    <div className="feed-navigation" aria-label="商品浏览方式">
      <button className={mode === 'home' ? 'selected' : ''} aria-pressed={mode === 'home'} onClick={() => switchFeed('home')}>最新商品</button>
      <button className={mode === 'following' ? 'selected' : ''} aria-pressed={mode === 'following'} onClick={() => switchFeed('following')}>关注动态</button>
    </div>
    {mode !== 'following' && <form className="search-panel" onSubmit={submit}>
      <div className="search-panel-top">
        <div className="search-panel-intro"><span className="search-panel-symbol" aria-hidden="true"><svg width="46" height="46" viewBox="0 0 48 48" fill="none"><rect x="6" y="4" width="31" height="36" rx="7" fill="currentColor" opacity=".09" transform="rotate(15 22 22)"/><circle cx="21" cy="21" r="12" stroke="currentColor" strokeWidth="4"/><path d="m30 30 11 11" stroke="currentColor" strokeWidth="5" strokeLinecap="round"/></svg></span><div><h2>商品搜索</h2><p>快速查找你想要的二手好物</p></div></div>
        <div className="search-panel-query"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" aria-hidden="true"><circle cx="10.5" cy="10.5" r="6.5"/><path d="m16 16 4.5 4.5"/></svg><input aria-label="搜索商品" placeholder="搜索商品名称、品牌或关键词，如「机械键盘」" maxLength={100} value={draft.query} onChange={e => setDraft({ ...draft, query: e.target.value })}/><button className="primary" type="submit" disabled={loading}>搜索</button></div>
      </div>
      <div className="search-panel-filters">
        <div className="search-panel-price"><span>价格区间：</span><input aria-label="最低价格" placeholder="最低价" inputMode="decimal" value={draft.min} onChange={e => setDraft({ ...draft, min: e.target.value })}/><span className="price-separator">–</span><input aria-label="最高价格" placeholder="最高价" inputMode="decimal" value={draft.max} onChange={e => setDraft({ ...draft, max: e.target.value })}/><span>元</span></div>
        <label className="search-panel-category"><span>分类：</span><select aria-label="搜索分类" value={selectedCategory} onChange={e => setSelectedCategory(e.target.value)}><option value="">全部分类</option>{categories.map(item => <option key={item.id} value={item.id}>{item.name}</option>)}</select></label>
        <label className="search-panel-sort"><span>排序：</span><select aria-label="搜索排序" value={draft.sort} onChange={e => setDraft({ ...draft, sort: e.target.value })}><option value="newest">最新发布</option><option value="relevance">相关性</option><option value="price_asc">价格从低到高</option><option value="price_desc">价格从高到低</option></select></label>
        <button className="search-panel-reset" type="button" disabled={loading} onClick={() => { switchFeed('home') }}><svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M20 10a8 8 0 1 0-1 7M20 4v6h-6"/></svg>重置</button>
      </div>
    </form>}
    <div className="section-label"><div className="list-heading"><h2>{mode === 'following' ? '关注动态' : mode === 'home' ? '最新商品' : '搜索结果'}</h2></div><span className="muted">{mode === 'search' ? '商品状态可能稍有延迟，详情以最新状态为准' : mode === 'following' ? '你关注的卖家正在出售的商品' : '发现新发布的闲置好物'}</span></div>
    {notice && <p className="muted" role="status">{notice}</p>}
    {error ? <div className="empty error" role="alert"><p>{error}</p><button onClick={expired ? restart : () => setRevision(value => value + 1)}>{expired ? '重新搜索' : '重试'}</button></div> : loading ? <div className="market-grid" aria-label={mode === 'search' ? '正在搜索' : '正在加载商品'}>{[0, 1, 2, 3].map(i => <div className="skeleton" key={i} />)}</div> : products.length ? <div className="market-grid">{products.map(product => <button className="market-card" key={product.id} onClick={() => open(product.id)}><div className="market-cover"><ProductImages id={product.id} token={token} /><span className="cover-category">{categories.find(item => item.id === product.category_id)?.name ?? '二手商品'}</span></div><div className="market-card-info"><h3>{product.title}</h3><strong>¥ {yuan(product.price_cent)}</strong></div></button>)}</div> : <div className="empty"><h3>{mode === 'following' ? '暂无关注动态' : mode === 'home' ? '暂无在售商品' : '没有匹配的商品'}</h3><p>{mode === 'following' ? '关注卖家后，可以在这里查看他们的在售商品。' : mode === 'home' ? '稍后刷新，看看新发布的闲置好物。' : '试试其他关键词、分类或价格范围。'}</p></div>}
    <div className="pagination"><button disabled={!history.length || loading || !!error} onClick={restart}>返回第一页</button><span>第 {history.length + 1} 页</span><button disabled={!next || loading || !!error} onClick={() => { setHistory([...history, cursor]); setCursor(next) }}>下一页 →</button></div>
  </section>
}
