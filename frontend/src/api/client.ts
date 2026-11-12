export interface User { id: string; username: string; nickname: string; role: string; status: string; created_at?: string }
export interface Category { id: string; name: string; code: string }
export interface Product { id: string; seller_id: string; category_id: string; title: string; description?: string; price_cent: string; status: string; aggregate_version: string; first_published_at?: string; created_at?: string; images?: { object_key: string }[] }
export class ApiError extends Error { status: number; requestId?: string; constructor(message: string, status: number, requestId?: string) { super(message); this.status = status; this.requestId = requestId } }

// request 统一调用同源 Gateway；不记录密码及 Token，超时和取消传递给 fetch。
export async function request<T>(path: string, options: { method?: string; body?: unknown; token?: string; signal?: AbortSignal } = {}): Promise<T> {
  const headers: Record<string, string> = {}
  if (options.body !== undefined) headers['Content-Type'] = 'application/json'
  if (options.token) headers.Authorization = `Bearer ${options.token}`
  let response: Response
  try {
    response = await fetch(`/api/v1${path}`, { method: options.method ?? 'GET', headers, body: options.body === undefined ? undefined : JSON.stringify(options.body), signal: options.signal ? AbortSignal.any([options.signal, AbortSignal.timeout(12000)]) : AbortSignal.timeout(12000), cache: 'no-store' })
  } catch (error) {
    if (options.signal?.aborted) throw error
    throw new ApiError('暂时无法连接服务，请确认服务已启动后重试。', 0)
  }
  const data = await response.json().catch(() => null)
  if (!response.ok) throw new ApiError(data?.error?.message ?? '请求未成功，请稍后重试。', response.status, data?.error?.request_id)
  if (!data) throw new ApiError('服务返回了无效响应，请重试。', response.status)
  return data as T
}

// yuan 精确展示分金额，避免将 BIGINT 转为 JavaScript Number。
export function yuan(cent: string): string { const amount = BigInt(cent); return `${amount / 100n}.${(amount % 100n).toString().padStart(2, '0')}` }
// cents 校验人民币输入并精确转换为分，不使用浮点乘法。
export function cents(value: string): string {
  if (!/^\d+(\.\d{1,2})?$/.test(value)) throw new Error('价格须为正数，最多保留两位小数。')
  const [whole, fraction = ''] = value.split('.')
  const amount = BigInt(whole) * 100n + BigInt(fraction.padEnd(2, '0'))
  if (amount <= 0n || amount > 9223372036854775807n) throw new Error('价格超出允许范围。')
  return amount.toString()
}
