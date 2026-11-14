import { registerHooks } from 'node:module'
import { test } from 'node:test'
import assert from 'node:assert/strict'

// resolve 仅为 Node 的类型擦除测试补全前端 bundler 模块扩展名，不修改生产加载方式。
registerHooks({ resolve(specifier, context, next) { return next(specifier === './client' && context.parentURL?.endsWith('/api/orders.ts') ? './client.ts' : specifier, context) } })
const { createOrder, decideOrder, listOrders, checkoutKey, forgetCheckout, processing } = await import('../src/api/orders.ts')
const { yuan, ApiError } = await import('../src/api/client.ts')

// 此用例验证同意图在未知结果后仍复用键，并与其他账号/商品隔离。
test('结算键跨重试稳定且按买家和商品隔离', () => {
  const values = new Map()
  globalThis.sessionStorage = { getItem: key => values.get(key) ?? null, setItem: (key, value) => values.set(key, value), removeItem: key => values.delete(key) }
  const first = checkoutKey('1', '9007199254740993')
  assert.equal(checkoutKey('1', '9007199254740993'), first)
  assert.notEqual(checkoutKey('2', '9007199254740993'), first)
  assert.notEqual(checkoutKey('1', '2'), first)
  forgetCheckout('1', '9007199254740993')
  assert.notEqual(checkoutKey('1', '9007199254740993'), first)
  globalThis.sessionStorage = { getItem() { throw new Error('disabled') } }
  assert.throws(() => checkoutKey('1', '2'), /会话存储/)
})

// 此用例验证真实前端 API 适配保留大整数、唯一幂等头及用户 Token，接受 202 而不伪造成交。
test('创建请求与 202 契约保持字符串精度和幂等键', async () => {
  let observed
  globalThis.fetch = async (path, options) => { observed = { path, options }; return new Response(JSON.stringify({ order: { id: '9007199254740993', status: 'ORDER_STATUS_PENDING' } }), { status: 202 }) }
  const result = await createOrder('9007199254740993', 'stable-key', 'user-token', new AbortController().signal)
  assert.equal(observed.path, '/api/v1/orders')
  assert.equal(observed.options.headers['Idempotency-Key'], 'stable-key')
  assert.equal(observed.options.headers.Authorization, 'Bearer user-token')
  assert.deepEqual(JSON.parse(observed.options.body), { product_id: '9007199254740993' })
  assert.equal(result.order.id, '9007199254740993')
  assert.equal(processing(result.order), true)
  assert.equal(yuan('9007199254740993'), '90071992547409.93')
})

// 此用例验证终结接口发送空体，并严格使用后端支持的买卖角色和分页参数。
test('确认取消空体及买卖列表参数', async () => {
  const calls = []
  globalThis.fetch = async (path, options) => { calls.push({ path, options }); return new Response('{}', { status: 200 }) }
  await decideOrder('123', 'cancel', 'token', new AbortController().signal)
  await decideOrder('123', 'confirm', 'token', new AbortController().signal)
  await listOrders('seller', 2, 'token', new AbortController().signal)
  assert.equal(calls[0].path, '/api/v1/orders/123/cancel')
  assert.equal(calls[1].path, '/api/v1/orders/123/confirm')
  assert.equal(calls[0].options.body, undefined)
  assert.equal(calls[1].options.headers['Content-Type'], undefined)
  assert.equal(calls[2].path, '/api/v1/orders?role=seller&page=2&page_size=12')
})

// 此用例验证超时和认证错误继续保留状态及请求编号，由页面决定安全重试，不自动重新下单。
test('错误保持状态与请求编号，网络失败不自动重试', async () => {
  globalThis.fetch = async () => new Response(JSON.stringify({ error: { message: '响应超时', request_id: 'req-1' } }), { status: 504 })
  await assert.rejects(createOrder('1', 'same-key', 'token', new AbortController().signal), error => error instanceof ApiError && error.status === 504 && error.requestId === 'req-1')
  let calls = 0
  globalThis.fetch = async () => { calls++; throw new Error('network') }
  await assert.rejects(createOrder('1', 'same-key', 'token', new AbortController().signal), error => error.status === 0)
  assert.equal(calls, 1)
})
