import { useEffect, useState } from 'react'
import { request } from '../api/client'
interface ImageAccess { id: string; url: string }
// ProductImages 按商品可见性申请短期图片地址；失败显示占位，不泄露对象键或凭据。
export function ProductImages({ id, token = '', gallery = false, interactive = false }: { id: string; token?: string; gallery?: boolean; interactive?: boolean }) {
  const [images, setImages] = useState<ImageAccess[]>([])
  const [selected, setSelected] = useState('')
  const [failed, setFailed] = useState<string[]>([])
  useEffect(() => {
    const controller = new AbortController()
    request<{ images?: ImageAccess[] }>(`/products/${id}/images`, { token, signal: controller.signal })
      .then(data => { if (!controller.signal.aborted) { setImages(data.images ?? []); setFailed([]) } })
      .catch(() => { if (!controller.signal.aborted) setImages([]) })
    return () => controller.abort()
  }, [id, token])
  const visible = images.filter(image => !failed.includes(image.id))
  if (interactive && visible.length) {
    const active = visible.find(image => image.id === selected) ?? visible[0]
    return <div className="detail-gallery"><a className="detail-main-image" href={active.url} target="_blank" rel="noopener noreferrer" aria-label="查看商品大图"><img src={active.url} alt="商品实拍大图" referrerPolicy="no-referrer" onError={() => setFailed(value => [...value, active.id])} /></a><div className="detail-thumbnails" aria-label="选择商品图片">{visible.map((image, index) => <button key={image.id} className={image.id === active.id ? 'selected' : ''} aria-pressed={image.id === active.id} aria-label={`查看第 ${index + 1} 张图片`} onClick={() => setSelected(image.id)}><img src={image.url} alt="" referrerPolicy="no-referrer" /></button>)}</div><small className="muted">点击主图查看大图</small></div>
  }
  if (!visible.length) return <span className="image-fallback">暂无商品图片</span>
  return <span className={gallery ? 'product-gallery' : 'product-photo'}>{(gallery ? visible : visible.slice(0, 1)).map(image => <img key={image.id} src={image.url} alt="商品实拍" loading="lazy" referrerPolicy="no-referrer" onError={() => setFailed(value => [...value, image.id])} />)}</span>
}
