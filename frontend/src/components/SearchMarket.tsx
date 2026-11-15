import { useEffect, useState } from 'react'
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
// SearchMarket 查询真实搜索接口，条件变化重新开始快照，取消过时请求避免覆盖新结果。
export function SearchMarket({ category, categories, open }: { category: string; categories: Category[]; open: (id: string) => void }) {
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
  const [revision, setRevision] = useState(0)
  useEffect(() => {
    const controller = new AbortController()
    queueMicrotask(() => { if (!controller.signal.aborted) { setLoading(true); setError(''); setExpired(false) } })
    const params = new URLSearchParams({ query: filters.query, sort: filters.sort, page_size: '12' })
    if (appliedCategory) params.set('category_id', appliedCategory)
    if (filters.min) params.set('min_price_cent', filters.min)
    if (filters.max) params.set('max_price_cent', filters.max)
    if (cursor) params.set('cursor', cursor)
    request<{ products?: SearchProduct[]; next_cursor?: string }>(`/search/products?${params}`, { signal: controller.signal })
      .then(data => { if (!controller.signal.aborted) { setProducts(data.products ?? []); setNext(data.next_cursor ?? '') } })
      .catch(err => { if (!controller.signal.aborted) { setProducts([]); setNext(''); setExpired(err instanceof ApiError && err.status === 409); setError(err instanceof Error ? err.message : '搜索失败，请重试。') } })
      .finally(() => { if (!controller.signal.aborted) setLoading(false) })
    return () => controller.abort()
  }, [appliedCategory, filters, cursor, revision])
  // restart 放弃旧游标并创建新搜索，旧 PIT 由服务端 TTL 回收。
  function restart() { setCursor(''); setHistory([]); setNext(''); setRevision(value => value + 1) }
  // submit 校验用户输入，保持金额精度，并将条件和分页一次性重置。
  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    try {
      const min = priceBound(draft.min), max = priceBound(draft.max)
      if (min !== undefined && max !== undefined && BigInt(min) > BigInt(max)) throw new Error('最低价格不能高于最高价格。')
      if (draft.sort === 'relevance' && !draft.query.trim()) throw new Error('相关性排序需要关键词。')
      setAppliedCategory(selectedCategory)
      setFilters({ ...draft, query: draft.query.trim(), min: min ?? '', max: max ?? '' }); restart()
    } catch (err) { setError(err instanceof Error ? err.message : '筛选条件无效。') }
  }
  return <section className="market-list" aria-label="商品搜索">
    <form className="search-panel" onSubmit={submit}>
      <div className="search-panel-top">
        <div className="search-panel-intro"><span className="search-panel-symbol" aria-hidden="true"><svg width="46" height="46" viewBox="0 0 48 48" fill="none"><rect x="6" y="4" width="31" height="36" rx="7" fill="currentColor" opacity=".09" transform="rotate(15 22 22)"/><circle cx="21" cy="21" r="12" stroke="currentColor" strokeWidth="4"/><path d="m30 30 11 11" stroke="currentColor" strokeWidth="5" strokeLinecap="round"/></svg></span><div><h2>商品搜索</h2><p>快速查找你想要的二手好物</p></div></div>
        <div className="search-panel-query"><svg width="20" height="20" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" aria-hidden="true"><circle cx="10.5" cy="10.5" r="6.5"/><path d="m16 16 4.5 4.5"/></svg><input aria-label="搜索商品" placeholder="搜索商品名称、品牌或关键词，如「机械键盘」" maxLength={100} value={draft.query} onChange={e => setDraft({ ...draft, query: e.target.value })}/><button className="primary" type="submit" disabled={loading}>搜索</button></div>
      </div>
      <div className="search-panel-filters">
        <div className="search-panel-price"><span>价格区间：</span><input aria-label="最低价格" placeholder="最低价" inputMode="decimal" value={draft.min} onChange={e => setDraft({ ...draft, min: e.target.value })}/><span className="price-separator">–</span><input aria-label="最高价格" placeholder="最高价" inputMode="decimal" value={draft.max} onChange={e => setDraft({ ...draft, max: e.target.value })}/><span>元</span></div>
        <label className="search-panel-category"><span>分类：</span><select aria-label="搜索分类" value={selectedCategory} onChange={e => setSelectedCategory(e.target.value)}><option value="">全部分类</option>{categories.map(item => <option key={item.id} value={item.id}>{item.name}</option>)}</select></label>
        <label className="search-panel-sort"><span>排序：</span><select aria-label="搜索排序" value={draft.sort} onChange={e => setDraft({ ...draft, sort: e.target.value })}><option value="newest">最新发布</option><option value="relevance">相关性</option><option value="price_asc">价格从低到高</option><option value="price_desc">价格从高到低</option></select></label>
        <button className="search-panel-reset" type="button" disabled={loading} onClick={() => { setDraft(initial); setFilters(initial); setSelectedCategory(''); setAppliedCategory(''); restart() }}><svg width="17" height="17" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true"><path d="M20 10a8 8 0 1 0-1 7M20 4v6h-6"/></svg>重置</button>
      </div>
    </form>
    <div className="section-label"><div className="list-heading"><h2>商品列表</h2></div><span className="muted">商品状态可能稍有延迟，详情以最新状态为准</span></div>
    {error ? <div className="empty error" role="alert"><p>{error}</p><button onClick={expired ? restart : () => setRevision(value => value + 1)}>{expired ? '重新搜索' : '重试'}</button></div> : loading ? <div className="market-grid" aria-label="正在搜索">{[0, 1, 2, 3].map(i => <div className="skeleton" key={i} />)}</div> : products.length ? <div className="market-grid">{products.map(product => <button className="market-card" key={product.id} onClick={() => open(product.id)}><div className="market-cover"><span>暂无商品图片</span><span className="cover-category">{categories.find(item => item.id === product.category_id)?.name ?? '二手商品'}</span></div><div className="market-card-info"><h3>{product.title}</h3><strong>¥ {yuan(product.price_cent)}</strong></div></button>)}</div> : <div className="empty"><h3>没有匹配的商品</h3><p>试试其他关键词、分类或价格范围。</p></div>}
    <div className="pagination"><button disabled={!history.length || loading || !!error} onClick={restart}>返回第一页</button><span>第 {history.length + 1} 页</span><button disabled={!next || loading || !!error} onClick={() => { setHistory([...history, cursor]); setCursor(next) }}>下一页 →</button></div>
  </section>
}
