import { registerHooks } from 'node:module'
import { test } from 'node:test'
import assert from 'node:assert/strict'

// resolve 为原生 TS 测试补全相对模块后缀，不改变前端 bundler 加载规则。
registerHooks({ resolve(specifier, context, next) {
  if (context.parentURL?.includes('/src/') && /^\.\.?\//.test(specifier) && !/\.\w+$/.test(specifier)) return next(specifier + '.ts', context)
  return next(specifier, context)
} })
const { ChatSession } = await import('../src/chat/session.ts')
const { Ledger, compareSeq } = await import('../src/chat/ledger.ts')
const { ApiError } = await import('../src/api/client.ts')
const { socketURL } = await import('../src/api/chat.ts')

const conversation = { id: '10', product_id: '9007199254740993', buyer_id: '1', seller_id: '2', last_seq: '0', last_read_seq: '0', unread_count: '0' }
// message 构造服务端持久化消息，用不同序号测试排序、去重与补拉缺口。
function message(seq, sender = '2', key = 'remote-' + seq) { return { id: (9007199254740993n + BigInt(seq)).toString(), conversation_id: '10', sender_id: sender, client_msg_id: key, seq: String(seq), content: '消息 ' + seq, created_at: '2026-11-15T00:00:00Z' } }
// memoryStorage 隔离各测试的待发箱并允许检查落盘内容。
function memoryStorage() { const data = new Map(); return { data, getItem: k => data.get(k) ?? null, setItem: (k, v) => data.set(k, v) } }
class Socket {
  readyState = 0; sent = []; onopen = null; onmessage = null; onclose = null; onerror = null
  // send 记录真正出网的协议数据，验证稳定幂等键和在途限制。
  send(data) { if (this.readyState !== 1) throw new Error('closed'); this.sent.push(JSON.parse(data)) }
  // open 按浏览器握手顺序开启套接字。
  open() { this.readyState = 1; this.onopen?.({}) }
  // server 注入网络帧，生产代码仍使用真实协议解析。
  server(value) { this.onmessage?.({ data: JSON.stringify(value) }) }
  // close 模拟断线，触发生产重连路径。
  close() { if (this.readyState === 3) return; this.readyState = 3; this.onclose?.({}) }
  // ready 只有应用心跳确认后才能发送待发消息。
  ready() { this.open(); const ping = this.sent.find(f => f.type === 'ping'); this.server({ type: 'pong', request_id: ping.request_id }) }
}
// settle 等待网络承诺完成，避免固定 sleep 依赖时序。
async function settle(check, attempts = 100) { for (let i = 0; i < attempts; i++) { if (check()) return; await new Promise(resolve => setTimeout(resolve, 2)) } assert.fail('等待状态超时') }
// fixture 构建实际 Session，网络与时钟可控但不复刻其状态机。
function fixture(extra = {}) {
  const sockets = []; const calls = []; let clock = 1000; let serial = 0; let unauthorized = 0
  const storage = extra.storage ?? memoryStorage()
  const api = async (path, options = {}) => {
    calls.push({ path, options })
    if (extra.api) { const result = await extra.api(path, options); if (result !== undefined) return result }
    if (path === '/chat/tickets') return { ticket: 'one-time-ticket' }
    if (path.startsWith('/conversations?')) return { conversations: [conversation] }
    if (path.includes('/messages')) {
      const query = new URLSearchParams(path.split('?')[1])
      return query.has('after_seq') ? { messages: [], next_seq: query.get('after_seq'), sync_until_seq: query.get('sync_until_seq') === '0' ? query.get('after_seq') : query.get('sync_until_seq') } : { messages: [] }
    }
    if (path.endsWith('/read')) return { conversation_id: '10', user_id: '1', last_read_seq: options.body.last_read_seq, unread_count: '0' }
    throw new Error('unexpected ' + path)
  }
  const session = new ChatSession(extra.userId ?? '1', 'private-token', { api, socket: () => { const s = new Socket(); sockets.push(s); return s }, origin: 'https://market.example', storage, now: () => clock, uuid: () => 'key-' + (++serial), unauthorized: () => unauthorized++, tickMs: 1000000, retryMs: 1 })
  return { session, sockets, calls, storage, now: n => { clock = n }, unauthorized: () => unauthorized }
}

// 此用例验证乱序不能越过缺口、重复消息不增加行数、大整数序号精确排序。
test('账本保持连续序号并精确去重', () => {
  const ledger = new Ledger(); ledger.seed([message(2), message(1)], true)
  ledger.put(message(4)); assert.equal(ledger.cursor, '2')
  ledger.put(message(3)); ledger.put(message(3)); assert.equal(ledger.cursor, '4'); assert.equal(ledger.sorted().length, 4)
  assert.throws(() => ledger.put({ ...message(3), id: '22' }), /顺序异常/)
  assert.equal(compareSeq('9007199254740993', '9007199254740992'), 1)
  const tail = new Ledger(); tail.seed([{ ...message(1), seq: '9007199254740993' }], true)
  assert.equal(tail.cursor, '9007199254740993')
  assert.equal(socketURL('https://market.example', 'once').startsWith('wss://market.example/api/v1/chat/ws?ticket=once'), true)
})

// 此用例覆盖离线入箱、ACK 丢失、刷新恢复和同一键重发，不生成第二个发送意图。
test('离线消息先保存，重连与刷新重试保留原键', async t => {
  const f = fixture(); t.after(() => f.session.stop()); f.session.start()
  await settle(() => f.sockets.length === 1 && f.session.getSnapshot().selected === '10')
  f.session.send('你好'); const pending = f.session.getSnapshot().pending[0]
  assert.equal(f.sockets[0].sent.length, 0)
  assert.match([...f.storage.data.values()][0], /你好/)
  assert.doesNotMatch([...f.storage.data.values()][0], /private-token/)
  f.sockets[0].ready()
  const first = f.sockets[0].sent.find(frame => frame.type === 'chat.send')
  assert.equal(first.data.client_msg_id, pending.client_msg_id)
  f.sockets[0].close(); await settle(() => f.sockets.length === 2); f.sockets[1].ready()
  const retried = f.sockets[1].sent.find(frame => frame.type === 'chat.send')
  assert.deepEqual(first.data, retried.data)
  f.session.stop()
  const reloaded = fixture({ storage: f.storage }); t.after(() => reloaded.session.stop()); reloaded.session.start()
  await settle(() => reloaded.sockets.length === 1); reloaded.sockets[0].ready()
  assert.equal(reloaded.sockets[0].sent.find(frame => frame.type === 'chat.send').data.client_msg_id, pending.client_msg_id)
  const foreign = fixture({ userId: '99', storage: f.storage }); t.after(() => foreign.session.stop()); foreign.session.start()
  await settle(() => foreign.sockets.length === 1); foreign.sockets[0].ready()
  assert.equal(foreign.sockets[0].sent.some(frame => frame.type === 'chat.send'), false)
})

// 此用例覆盖 ACK/重复推送/补拉合并，已保存消息只展示一次且移出待发箱。
test('ACK 与重复推送只显示一条消息', async t => {
  const f = fixture(); t.after(() => f.session.stop()); f.session.start()
  await settle(() => f.sockets.length && f.session.getSnapshot().selected); f.sockets[0].ready()
  f.session.send('hello')
  const sent = f.sockets[0].sent.find(frame => frame.type === 'chat.send'); const m = { ...message(1, '1', sent.data.client_msg_id), content: 'hello' }
  f.sockets[0].server({ type: 'chat.ack', request_id: sent.request_id, data: { message_id: m.id, conversation_id: '10', client_msg_id: m.client_msg_id, seq: '1' } })
  f.sockets[0].server({ type: 'chat.message', data: { ...m, message_id: m.id } })
  f.sockets[0].server({ type: 'chat.message', data: { ...m, message_id: m.id } })
  assert.equal(f.session.getSnapshot().pending.length, 0); assert.equal(f.session.getSnapshot().messages.length, 1)
  assert.equal(f.session.getSnapshot().messages[0].id, '9007199254740994')
})

// 此用例验证补拉固定上界，即使较大序号已通过推送到达也不能跳过缺失消息。
test('补拉填补推送缺口并复用本轮上界', async t => {
  let release
  const blocked = new Promise(resolve => { release = resolve })
  let syncCalls = 0
  const f = fixture({ api: async path => {
    if (!path.includes('/messages')) return
    const params = new URLSearchParams(path.split('?')[1])
    if (!params.has('after_seq')) return { messages: [message(1)] }
    syncCalls++
    if (syncCalls === 1) { await blocked; return { messages: [message(2)], next_seq: '2', sync_until_seq: '3', has_more: true } }
    if (syncCalls === 2) { assert.equal(params.get('after_seq'), '2'); assert.equal(params.get('sync_until_seq'), '3'); return { messages: [message(3)], next_seq: '3', sync_until_seq: '3' } }
    return { messages: [], next_seq: params.get('after_seq'), sync_until_seq: params.get('after_seq') }
  } })
  t.after(() => f.session.stop()); f.session.start()
  await settle(() => f.sockets.length && f.session.getSnapshot().messages.length === 1); f.sockets[0].ready()
  await settle(() => syncCalls === 1)
  f.sockets[0].server({ type: 'chat.message', data: { ...message(4), message_id: message(4).id } })
  release(); await settle(() => !f.session.getSnapshot().syncing)
  assert.deepEqual(f.session.getSnapshot().messages.map(m => m.seq), ['1', '2', '3', '4'])
})

// 此用例验证可见阅读由显式调用控制，补拉和后台消息不会自动清零未读。
test('只有显式可见阅读推进已读，迟到通知不回退', async t => {
  const f = fixture({ api: path => path.includes('/messages') && !path.includes('after_seq') ? { messages: [message(1)] } : undefined })
  t.after(() => f.session.stop()); f.session.start()
  await settle(() => f.sockets.length && f.session.getSnapshot().messages.length === 1); f.sockets[0].ready()
  assert.equal(f.calls.some(c => c.path.endsWith('/read')), false)
  await f.session.markVisible('10')
  assert.equal(f.calls.find(c => c.path.endsWith('/read')).options.body.last_read_seq, '1')
  f.sockets[0].server({ type: 'chat.read', data: { conversation_id: '10', user_id: '2', last_read_seq: '5' } })
  f.sockets[0].server({ type: 'chat.read', data: { conversation_id: '10', user_id: '2', last_read_seq: '3' } })
  assert.equal(f.session.getSnapshot().peerRead, '5')
})

// 此用例验证认证失效停止重连，以及卸载后的迟到请求不能泄漏旧账号会话。
test('认证失效与停止生命周期释放请求', async t => {
  const auth = fixture({ api: path => { if (path === '/chat/tickets') throw new ApiError('expired', 401) } })
  t.after(() => auth.session.stop()); auth.session.start(); await settle(() => auth.unauthorized() === 1)
  assert.equal(auth.session.getSnapshot().status, 'stopped')
  let release
  const pending = new Promise(resolve => { release = resolve })
  const late = fixture({ api: path => path.startsWith('/conversations?') ? pending : undefined })
  t.after(() => late.session.stop()); late.session.start(); late.session.stop()
  release({ conversations: [conversation] }); await new Promise(resolve => setTimeout(resolve, 5))
  assert.equal(late.session.getSnapshot().conversations.length, 0)
})

// 此用例验证拒绝存储时不发送，以及明确业务失败保留原键供手动处理。
test('存储失败不出网，业务失败不自动新建消息', async t => {
  const f = fixture({ storage: { getItem: () => null, setItem: () => { throw new Error('disabled') } } })
  t.after(() => f.session.stop()); f.session.start(); await settle(() => f.sockets.length && f.session.getSnapshot().selected); f.sockets[0].ready()
  assert.throws(() => f.session.send('hello'), /会话存储/); assert.equal(f.sockets[0].sent.some(f => f.type === 'chat.send'), false)
  const good = fixture(); t.after(() => good.session.stop()); good.session.start(); await settle(() => good.sockets.length && good.session.getSnapshot().selected); good.sockets[0].ready()
  good.session.send('hello'); const sent = good.sockets[0].sent.find(f => f.type === 'chat.send')
  good.sockets[0].server({ type: 'chat.error', request_id: sent.request_id, data: { code: 'PermissionDenied', message: '无权访问会话' } })
  assert.equal(good.session.getSnapshot().pending[0].status, 'failed')
  good.now(12000); good.session.resume()
  assert.equal(good.sockets[0].sent.filter(f => f.type === 'chat.send').length, 1)
})

// 此用例验证慢列表响应不能把刚提交的已读状态和未读数覆盖为旧快照。
test('迟到列表不会覆盖更新后的已读状态', async t => {
  let listCalls = 0; let release
  const oldList = new Promise(resolve => { release = resolve })
  const f = fixture({ api: path => {
    if (path.startsWith('/conversations?')) {
      listCalls++
      if (listCalls === 2) return oldList
      return { conversations: [{ ...conversation, last_seq: '1', last_read_seq: listCalls > 2 ? '1' : '0', unread_count: listCalls > 2 ? '0' : '1' }] }
    }
    if (path.includes('/messages') && !path.includes('after_seq')) return { messages: [message(1)] }
  } })
  t.after(() => f.session.stop()); f.session.start()
  await settle(() => f.sockets.length && f.session.getSnapshot().messages.length); f.sockets[0].ready()
  await settle(() => listCalls === 2)
  await f.session.markVisible('10')
  release({ conversations: [{ ...conversation, last_seq: '1', last_read_seq: '0', unread_count: '1' }] })
  await settle(() => listCalls >= 3 && !f.session.getSnapshot().loading)
  const row = f.session.getSnapshot().conversations[0]
  assert.equal(row.last_read_seq, '1'); assert.equal(row.unread_count, '0')
})

// 此用例验证长连接仍在时 ACK 超时可安全重发，并限制同时在途的消息数量。
test('ACK 超时重用键，最多五条在途消息', async t => {
  const f = fixture(); t.after(() => f.session.stop()); f.session.start()
  await settle(() => f.sockets.length && f.session.getSnapshot().selected); f.sockets[0].ready()
  for (let i = 0; i < 9; i++) f.session.send('message ' + i)
  const first = f.sockets[0].sent.filter(f => f.type === 'chat.send')
  assert.equal(first.length, 5)
  f.now(10000); f.session.resume()
  const second = f.sockets[0].sent.filter(f => f.type === 'chat.send').slice(5)
  assert.equal(second.length, 5)
  assert.deepEqual(first.map(f => f.data), second.map(f => f.data))
  assert.notEqual(first[0].request_id, second[0].request_id)
})

// 此用例验证损坏的持久化记录不被新发送覆盖，避免丢失未知结果的原消息键。
test('损坏待发箱不会被静默覆盖', async t => {
  const storage = memoryStorage(); storage.setItem('remarket.chat.outbox.v1.1', '{broken')
  const f = fixture({ storage }); t.after(() => f.session.stop()); f.session.start()
  await settle(() => f.sockets.length && f.session.getSnapshot().selected); f.sockets[0].ready()
  assert.throws(() => f.session.send('new message'))
  assert.equal(storage.getItem('remarket.chat.outbox.v1.1'), '{broken')
  assert.equal(f.sockets[0].sent.some(f => f.type === 'chat.send'), false)
})

// 此用例验证已加载到会话末页后，周期刷新不会重新打开已耗尽的旧分页游标。
test('会话末页刷新不会重复显示旧的下一页入口', async t => {
  let calls = 0
  const second = { ...conversation, id: '11', product_id: '12' }
  const f = fixture({ api: path => {
    if (!path.startsWith('/conversations?')) return
    calls++
    if (path.includes('cursor=next')) return { conversations: [second] }
    return { conversations: [conversation], next_cursor: 'next' }
  } })
  t.after(() => f.session.stop()); f.session.start()
  await settle(() => f.session.getSnapshot().nextCursor === 'next' && !f.session.getSnapshot().loading)
  await f.session.refreshList(true)
  assert.equal(f.session.getSnapshot().nextCursor, '')
  await f.session.refreshList()
  assert.equal(f.session.getSnapshot().nextCursor, '')
  assert.equal(f.session.getSnapshot().conversations.length, 2)
  assert.ok(calls >= 3)
})
