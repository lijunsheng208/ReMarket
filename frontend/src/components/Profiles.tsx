import { useEffect, useState } from 'react'
import { request, yuan } from '../api/client'
import type { Product, User } from '../api/client'
import { ProductImages } from './ProductImages'
export interface PublicProfile { id: string; nickname: string; follower_count?: string; following_count?: string; is_following?: boolean; is_self?: boolean; created_at?: string }
// usePublicProfile 随身份和关系版本更新真实资料，取消过时请求避免展示其他用户的结果。
function usePublicProfile(id: string, token: string, revision: number) {
  const [profile, setProfile] = useState<PublicProfile | null>(null)
  const [error, setError] = useState('')
  useEffect(() => { const controller = new AbortController(); request<PublicProfile>(`/users/${id}/profile`, { token, signal: controller.signal }).then(data => { if (!controller.signal.aborted) { setProfile(data); setError('') } }).catch(err => { if (!controller.signal.aborted) setError(err instanceof Error ? err.message : '资料加载失败') }); return () => controller.abort() }, [id, token, revision])
  return { profile, error }
}
// AccountStats 将账号粉丝和关注计数显示为主页入口，不以列表长度代替总数。
export function AccountStats({ id, token, revision, open }: { id: string; token: string; revision: number; open: (section: 'followers' | 'following') => void }) {
  const { profile, error } = usePublicProfile(id, token, revision)
  return <div className="account-social">{error ? <span role="status">统计暂不可用</span> : <><button onClick={() => open('followers')}><strong>{profile?.follower_count ?? (profile ? '0' : '—')}</strong> 粉丝</button><button onClick={() => open('following')}><strong>{profile?.following_count ?? (profile ? '0' : '—')}</strong> 关注</button></>}</div>
}
// SellerCard 展示真实卖家昵称与默认头像，关注写操作结束后由上层刷新统计和 Feed。
export function SellerCard({ id, token, revision, open, login, changed }: { id: string; token: string; revision: number; open: (id: string) => void; login: () => void; changed: () => void }) {
  const { profile, error } = usePublicProfile(id, token, revision)
  const [busy, setBusy] = useState(false)
  const [actionError, setActionError] = useState('')
  // toggle 以当前账号执行幂等关注，不做乐观计数，网络失败时保留原状态。
  async function toggle() { if (!token) { login(); return } if (!profile || busy) return; setBusy(true); setActionError(''); try { await request(`/users/${id}/follow`, { method: profile.is_following ? 'DELETE' : 'POST', token }); changed() } catch (err) { setActionError(err instanceof Error ? err.message : '操作失败，请重试') } finally { setBusy(false) } }
  return <div className="seller-block"><div className="seller-row"><button className="seller-home" onClick={() => open(id)} disabled={!profile}><span className="seller-avatar" aria-label="卖家默认头像">{profile?.nickname?.slice(0, 1) ?? '人'}</span><span><strong>{profile?.nickname ?? '加载卖家资料…'}</strong><small>查看{profile?.is_self ? '我的' : '卖家'}主页 · {profile?.follower_count ?? '0'} 粉丝</small></span></button>{profile && !profile.is_self && <button className={profile.is_following ? 'follow-button' : 'follow-button primary'} disabled={busy} onClick={() => void toggle()}>{busy ? '处理中…' : profile.is_following ? '已关注 · 取消' : '+ 关注'}</button>}</div>{(error || actionError) && <p className="form-error" role="alert">{error || actionError}</p>}</div>
}
export type ProfileSection = 'products' | 'followers' | 'following'
// ProfilePage 展示卖家公开商品和真实关系分页，自己的非公开商品仍交给商品管理页面。
export function ProfilePage({ id, token, revision, initialSection, openProduct, openProfile, back, login, changed }: { id: string; token: string; revision: number; initialSection: ProfileSection; openProduct: (id: string) => void; openProfile: (id: string) => void; back: () => void; login: () => void; changed: () => void }) {
  const { profile, error: profileError } = usePublicProfile(id, token, revision)
  const [section, setSection] = useState<ProfileSection>(initialSection)
  const [cursor, setCursor] = useState('')
  const [next, setNext] = useState('')
  const [page, setPage] = useState(1)
  const [products, setProducts] = useState<Product[]>([])
  const [people, setPeople] = useState<{ id: string; user: User }[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  useEffect(() => {
    const controller = new AbortController()
    if (section !== 'products' && !token) { queueMicrotask(() => { if (!controller.signal.aborted) { setLoading(false); setPeople([]); setNext('') } }); return () => controller.abort() }
    queueMicrotask(() => { if (!controller.signal.aborted) { setLoading(true); setError('') } })
    const params = new URLSearchParams({ page_size: '12' }); if (cursor) params.set(section === 'products' ? 'cursor' : 'after_id', cursor)
    request<{ products?: Product[]; items?: { id: string; user: User }[]; next_cursor?: string; next_after_id?: string }>(`/users/${id}/${section}?${params}`, { token, signal: controller.signal }).then(data => { if (!controller.signal.aborted) { setProducts(data.products ?? []); setPeople(data.items ?? []); setNext(data.next_cursor ?? data.next_after_id ?? '') } }).catch(err => { if (!controller.signal.aborted) { setError(err instanceof Error ? err.message : '加载失败'); setNext('') } }).finally(() => { if (!controller.signal.aborted) setLoading(false) })
    return () => controller.abort()
  }, [id, section, cursor, token, revision])
  // selectSection 切换主页内容时清空不同接口的游标，匿名用户先登录才能查看关系列表。
  function selectSection(value: ProfileSection) { if (value !== 'products' && !token) { login(); return } setSection(value); setCursor(''); setNext(''); setPage(1) }
  return <section className="profile-page"><button className="text-button" onClick={back}>← 返回首页</button>{profileError ? <p className="form-error" role="alert">{profileError}</p> : <><header className="profile-header"><div className="profile-identity"><span className="seller-avatar large">{profile?.nickname?.slice(0, 1) ?? '人'}</span><div><h1>{profile?.nickname ?? '加载用户资料…'}</h1><p className="muted">{profile?.is_self ? '我的个人主页' : '卖家个人主页'}{profile?.created_at ? ` · ${new Date(profile.created_at).toLocaleDateString('zh-CN')} 加入` : ''}</p></div></div><div className="profile-counts"><button onClick={() => selectSection('followers')}><strong>{profile?.follower_count ?? '0'}</strong>粉丝</button><button onClick={() => selectSection('following')}><strong>{profile?.following_count ?? '0'}</strong>关注</button></div></header>{profile && !profile.is_self && <SellerCard id={id} token={token} revision={revision} open={openProfile} login={login} changed={changed} />}<nav className="profile-tabs" aria-label="个人主页内容">{(['products', 'followers', 'following'] as const).map(value => <button key={value} className={section === value ? 'selected' : ''} aria-pressed={section === value} onClick={() => selectSection(value)}>{value === 'products' ? '在售商品' : value === 'followers' ? '粉丝' : '关注'}</button>)}</nav>{section !== 'products' && !token ? <div className="empty"><p>登录后查看粉丝和关注列表。</p><button onClick={login}>去登录</button></div> : error ? <p className="form-error" role="alert">{error}<button onClick={changed}>重试</button></p> : loading ? <div className="empty" role="status">加载中…</div> : section === 'products' ? products.length ? <div className="market-grid">{products.map(p => <button className="market-card" key={p.id} onClick={() => openProduct(p.id)}><div className="market-cover"><ProductImages id={p.id} token={token} /></div><div className="market-card-info"><h3>{p.title}</h3><strong>¥ {yuan(p.price_cent)}</strong></div></button>)}</div> : <div className="empty"><h3>暂无在售商品</h3></div> : people.length ? <div className="people-grid">{people.map(person => <button className="person-card" key={person.id} onClick={() => openProfile(person.user.id)}><span className="seller-avatar">{person.user.nickname?.slice(0, 1) ?? '人'}</span><strong>{person.user.nickname}</strong><span>查看主页 →</span></button>)}</div> : <div className="empty"><p>{section === 'followers' ? '暂无粉丝' : '暂无关注'}</p></div>}<div className="pagination"><button disabled={page === 1 || loading} onClick={() => { setCursor(''); setPage(1) }}>返回第一页</button><span>第 {page} 页</span><button disabled={!next || next === '0' || loading || !!error} onClick={() => { setCursor(next); setPage(value => value + 1) }}>下一页 →</button></div></>}</section>
}
