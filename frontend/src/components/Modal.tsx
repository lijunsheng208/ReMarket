import { useEffect, useRef } from 'react'
import type { ReactNode } from 'react'

// Modal 使用原生对话框管理焦点与键盘，忙碌时阻止关闭以避免丢失操作结果。
export function Modal({ title, children, close, busy = false }: { title: string; children: ReactNode; close: () => void; busy?: boolean }) {
  const ref = useRef<HTMLDialogElement>(null)
  useEffect(() => { const dialog = ref.current; dialog?.showModal(); return () => dialog?.close() }, [])
  return <dialog ref={ref} aria-labelledby="dialog-title" onCancel={event => { event.preventDefault(); if (!busy) close() }}><div className="modal-head"><h2 id="dialog-title">{title}</h2><button className="icon-button" aria-label="关闭对话框" disabled={busy} onClick={close}>×</button></div>{children}</dialog>
}
