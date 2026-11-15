import { useEffect, useState } from 'react'
import { request } from '../api/client'
interface ImageAccess { id: string; url: string }
// ProductImages 按商品可见性申请短期图片地址；失败显示占位，不泄露对象键或凭据。
export function ProductImages({ id, token = '', gallery = false }: { id: string; token?: string; gallery?: boolean }) {
  const [images, setImages] = useState<ImageAccess[]>([])
  const [failed, setFailed] = useState<string[]>([])
  useEffect(() => {
    const controller = new AbortController()
    request<{ images?: ImageAccess[] }>(`/products/${id}/images`, { token, signal: controller.signal })
      .then(data => { if (!controller.signal.aborted) { setImages(data.images ?? []); setFailed([]) } })
      .catch(() => { if (!controller.signal.aborted) setImages([]) })
    return () => controller.abort()
  }, [id, token])
  const visible = images.filter(image => !failed.includes(image.id))
  if (!visible.length) return <span className="image-fallback">暂无商品图片</span>
  return <span className={gallery ? 'product-gallery' : 'product-photo'}>{(gallery ? visible : visible.slice(0, 1)).map(image => <img key={image.id} src={image.url} alt="商品实拍" loading="lazy" referrerPolicy="no-referrer" onError={() => setFailed(value => [...value, image.id])} />)}</span>
}
