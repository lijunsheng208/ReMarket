"""连接隔离 Go 集成测试服务器，验证真实浏览器→Vite→HTTP/WS→gRPC→MySQL/Redis。"""
import json
import os
from pathlib import Path
from playwright.sync_api import sync_playwright, expect

ORIGIN = os.environ["CHAT_WEB_ORIGIN"]
TOKEN = os.environ["CHAT_BROWSER_TOKEN"]


# run 使用临时账号凭据，不拦截或模拟浏览器的 API/WebSocket 请求。
def run():
    with sync_playwright() as p:
        browser = p.chromium.launch(channel="chrome", headless=True)
        context = browser.new_context(viewport={"width": 1280, "height": 960}, locale="zh-CN")
        page = context.new_page()
        errors = []
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.add_init_script("sessionStorage.setItem('remarket.token'," + json.dumps(TOKEN) + ")")
        # DropAck 只在浏览器丢弃一次真实 ACK，后端消息已提交，生产 Session 必须恢复原结果。
        page.add_init_script("""
          window.__lostChatAck = false;
          const NativeSocket = window.WebSocket;
          window.WebSocket = class DropAck extends NativeSocket {
            constructor(...args) {
              super(...args);
              this.addEventListener('message', event => {
                const frame = JSON.parse(event.data);
                if (!window.__lostChatAck && frame.type === 'chat.ack' && frame.request_id === this.dropId) {
                  window.__lostChatAck = true;
                  event.stopImmediatePropagation();
                  this.close();
                }
              });
            }
            send(data) {
              const frame = JSON.parse(data);
              if (frame.type === 'chat.send' && frame.data.content === 'ACK丢失的真实消息') this.dropId = frame.request_id;
              super.send(data);
            }
          };
        """)
        page.goto(ORIGIN)
        page.wait_for_load_state("networkidle")
        page.get_by_text("隔离验收机械键盘", exact=True).click()
        page.get_by_role("button", name="联系卖家", exact=True).click()
        expect(page.get_by_label("消息内容")).to_be_visible()
        page.wait_for_function("document.querySelector('.chat-connection')?.textContent.includes('已连接')")
        page.get_by_label("消息内容").fill("真实浏览器跨节点消息")
        page.get_by_label("消息内容").press("Enter")
        expect(page.locator(".chat-bubble").filter(has_text="收到，明天下午三点见。")).to_have_count(1)
        page.get_by_label("消息内容").fill("ACK丢失的真实消息")
        page.get_by_label("消息内容").press("Enter")
        page.wait_for_function("window.__lostChatAck")
        page.wait_for_function("document.querySelector('.chat-connection')?.textContent.includes('已连接') && !document.querySelector('.chat-message.pending')")
        expect(page.locator(".chat-bubble").filter(has_text="ACK丢失的真实消息")).to_have_count(1)
        result = page.evaluate("""async token => {
          const response = await fetch('/api/v1/__chat_browser_fixture/message', {method:'POST', headers:{Authorization:'Bearer '+token}});
          return response.status;
        }""", TOKEN)
        assert result == 200
        page.evaluate("window.dispatchEvent(new Event('online'))")
        expect(page.locator(".chat-bubble").filter(has_text="通知遗漏后通过补拉恢复")).to_have_count(1)
        expect(page.locator(".chat-message.pending")).to_have_count(0)
        assert errors == [], errors
        directory = Path("/private/tmp/remarket-chat-browser")
        directory.mkdir(parents=True, exist_ok=True)
        page.screenshot(path=str(directory / "chat-live.png"), full_page=True)
        context.close()
        browser.close()
    print("PASS: 真实 Vite/双 Node/gRPC/MySQL/Redis 浏览器收发、ACK 丢失恢复及通知遗漏补拉")


if __name__ == "__main__":
    run()
