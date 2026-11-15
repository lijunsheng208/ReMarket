import { useEffect, useMemo, useSyncExternalStore } from 'react'
import { ChatSession, emptyChat } from './session'

// subscribeEmpty 为未登录状态提供稳定订阅入口，不创建任何网络连接。
function subscribeEmpty() { return () => {} }
// getEmpty 返回共享空快照，切换账号时不闪现上一账号聊天内容。
function getEmpty() { return emptyChat }
// useChat 让连接跨页面存活，账号/Token 变化时取消旧请求并释放旧连接。
export function useChat(userId: string, token: string, unauthorized: () => void) {
  const session = useMemo(() => userId && token ? new ChatSession(userId, token, { unauthorized }) : null, [userId, token, unauthorized])
  const snapshot = useSyncExternalStore(session?.subscribe ?? subscribeEmpty, session?.getSnapshot ?? getEmpty)
  useEffect(() => {
    if (!session) return
    session.start()
    // resume 只请求同步，是否已阅读由实际聊天窗口判断。
    const resume = () => { if (document.visibilityState === 'visible') session.resume() }
    window.addEventListener('online', resume); window.addEventListener('focus', resume); document.addEventListener('visibilitychange', resume)
    return () => { window.removeEventListener('online', resume); window.removeEventListener('focus', resume); document.removeEventListener('visibilitychange', resume); session.stop() }
  }, [session])
  return { session, snapshot }
}
