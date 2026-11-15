"""使用协议夹具验证真实 React 页面；不连接业务服务或写业务数据。"""
import json
import os
import re
import time
from pathlib import Path
from urllib.parse import urlparse, parse_qs
from playwright.sync_api import sync_playwright, expect

ORIGIN = os.getenv("CHAT_WEB_ORIGIN", "http://127.0.0.1:15173")
ARTIFACTS = Path(os.getenv("CHAT_SCREENSHOT_DIR", "/private/tmp/remarket-chat-browser"))
ARTIFACTS.mkdir(parents=True, exist_ok=True)
PRODUCT_ID = "9007199254740993"
CONVERSATION_ID = "9007199254740994"


# Fixture 保存模拟服务器事实，用来断言界面重试不重复创建消息。
class Fixture:
    def __init__(self):
        self.messages = [
            self.message(1, "2", "你好，键盘还在，配件和原盒都齐全。"),
            self.message(2, "1", "想了解一下键帽有没有明显磨损？"),
            self.message(3, "2", "没有打油，平时都用防尘罩，可以当面试一下。"),
        ]
        self.read_seq = 0
        self.read_calls = []
        self.sockets = []
        self.attempts = []
        self.dropped = False

    # message 按生产协议构造字符串 ID，保留跨 JavaScript 安全整数边界。
    def message(self, seq, sender, content, key=None):
        return dict(id=str(9007199254741000 + seq), conversation_id=CONVERSATION_ID,
                    sender_id=sender, client_msg_id=key or "fixture-" + str(seq), seq=str(seq),
                    content=content, message_type="TEXT", created_at="2026-11-15T06:20:00Z")

    # conversation 每次请求重新计算权威未读数，不用前端乐观加减替代。
    def conversation(self):
        latest = self.messages[-1] if self.messages else {}
        return dict(id=CONVERSATION_ID, product_id=PRODUCT_ID, buyer_id="1", seller_id="2",
                    last_seq=str(len(self.messages)), last_read_seq=str(self.read_seq),
                    unread_count=str(sum(int(m["seq"]) > self.read_seq and m["sender_id"] != "1" for m in self.messages)),
                    last_message_id=latest.get("id", ""), last_message_content=latest.get("content", ""),
                    last_message_at="2026-11-15T06:20:00Z")

    # product 提供商品详情入口所需事实；浏览器测试不操作实际商品库。
    def product(self):
        return dict(id=PRODUCT_ID, seller_id="2", category_id="1", title="九成新机械键盘 · 原盒齐全",
                    description="使用半年，键帽状态很好。支持当面验货，配件和原盒都在。", price_cent="24900",
                    status="PRODUCT_STATUS_ON_SALE", aggregate_version="1")

    # http 模拟已实现的 Gateway 契约，方向和补拉固定上界与后端一致。
    def http(self, route):
        parsed = urlparse(route.request.url)
        path = parsed.path.removeprefix("/api/v1")
        query = parse_qs(parsed.query)
        if path == "/users/me":
            result = {"user": dict(id="1", nickname="小林", username="buyer", role="USER", status="ACTIVE")}
        elif path == "/categories":
            result = {"categories": [dict(id="1", name="数码电子", code="digital")]}
        elif re.fullmatch(r"/users/\d+/profile", path):
            result = dict(id=path.split("/")[2], nickname="阿泽" if path.split("/")[2] == "2" else "小林")
        elif path in ("/feed/home", "/feed/following", "/search/products"):
            result = {"products": [self.product()]}
        elif path == "/chat/tickets":
            result = {"ticket": "browser-one-time-ticket"}
        elif path.startswith("/products/") and path.endswith("/images"):
            result = {}
        elif path.startswith("/products/"):
            result = {"product": self.product()}
        elif path == "/conversations" and route.request.method == "POST":
            assert route.request.post_data_json["product_id"] == PRODUCT_ID
            result = self.conversation()
        elif path == "/conversations":
            result = {"conversations": [self.conversation()]}
        elif path.endswith("/messages"):
            size = int(query.get("page_size", ["20"])[0])
            if "after_seq" in query:
                after = int(query["after_seq"][0])
                until = int(query.get("sync_until_seq", ["0"])[0]) or len(self.messages)
                rows = [m for m in self.messages if after < int(m["seq"]) <= until]
                result = dict(messages=rows[:size], has_more=len(rows) > size,
                              next_seq=rows[:size][-1]["seq"] if rows[:size] else str(after), sync_until_seq=str(until))
            else:
                before = int(query.get("before_seq", [str(len(self.messages) + 1)])[0])
                rows = list(reversed([m for m in self.messages if int(m["seq"]) < before]))
                result = dict(messages=rows[:size], has_more=len(rows) > size,
                              next_seq=rows[:size][-1]["seq"] if rows[:size] else "0")
        elif path.endswith("/read"):
            requested = int(route.request.post_data_json["last_read_seq"])
            self.read_seq = max(self.read_seq, requested)
            self.read_calls.append(requested)
            result = dict(conversation_id=CONVERSATION_ID, user_id="1", last_read_seq=str(self.read_seq),
                          unread_count=self.conversation()["unread_count"], last_seq=str(len(self.messages)))
        else:
            raise AssertionError("未覆盖接口 " + route.request.method + " " + path)
        route.fulfill(status=200, content_type="application/json", body=json.dumps(result, ensure_ascii=False))

    # websocket 真实页面调用此协议夹具，模拟保存成功但首个 ACK 丢失。
    def websocket(self, ws):
        self.sockets.append(ws)

        # receive 以幂等键查找原消息，故障重试不能增加保存条数。
        def receive(raw):
            frame = json.loads(raw)
            if frame["type"] == "ping":
                ws.send(json.dumps(dict(type="pong", request_id=frame["request_id"])))
            elif frame["type"] == "chat.send":
                data = frame["data"]
                self.attempts.append(data["client_msg_id"])
                saved = next((m for m in self.messages if m["client_msg_id"] == data["client_msg_id"]), None)
                if saved is None:
                    saved = self.message(len(self.messages) + 1, "1", data["content"], data["client_msg_id"])
                    self.messages.append(saved)
                if data["content"] == "断线重试也只保存一次" and not self.dropped:
                    self.dropped = True
                    ws.close(code=1012, reason="fixture reconnect")
                    return
                ws.send(json.dumps(dict(type="chat.ack", request_id=frame["request_id"],
                                       data=dict(message_id=saved["id"], conversation_id=CONVERSATION_ID,
                                                 client_msg_id=saved["client_msg_id"], seq=saved["seq"]))))
                for _ in range(2):
                    ws.send(json.dumps(dict(type="chat.message", data={**saved, "message_id": saved["id"]})))
        ws.on_message(receive)

    # remote 可选择遗漏通知，验证周期/恢复补拉不只依赖 WebSocket。
    def remote(self, text, push=True):
        message = self.message(len(self.messages) + 1, "2", text)
        self.messages.append(message)
        if push:
            self.sockets[-1].send(json.dumps(dict(type="chat.message", data={**message, "message_id": message["id"]})))


# pump_until 轮询协议夹具状态时继续处理浏览器事件，避免阻塞 WebSocket 回调。
def pump_until(page, predicate, reason):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        if predicate():
            return
        page.wait_for_timeout(20)
    raise AssertionError(reason)


# run 在桌面和手机 viewport 中验证发送、重试、后台未读和文本安全。
def run():
    fixture = Fixture()
    with sync_playwright() as p:
        browser = p.chromium.launch(channel="chrome", headless=True)
        context = browser.new_context(viewport={"width": 1365, "height": 950}, locale="zh-CN")
        page = context.new_page()
        errors = []
        page.on("pageerror", lambda error: errors.append(str(error)))
        page.add_init_script("sessionStorage.setItem('remarket.token','browser-fixture-token')")
        page.route("**/api/v1/**", fixture.http)
        page.route_web_socket("**/api/v1/chat/ws*", fixture.websocket)
        page.goto(ORIGIN)
        page.wait_for_load_state("networkidle")
        expect(page.get_by_text("九成新机械键盘 · 原盒齐全", exact=True)).to_be_visible()
        assert not fixture.read_calls, "后台连接不应自动清空未读"
        page.get_by_text("九成新机械键盘 · 原盒齐全", exact=True).click()
        page.get_by_role("button", name="联系卖家", exact=True).click()
        expect(page.get_by_role("heading", name="消息", exact=True)).to_be_visible()
        expect(page.get_by_label("消息内容")).to_be_visible()
        page.wait_for_function("document.querySelector('.chat-connection')?.textContent.includes('已连接')")
        page.wait_for_function("document.querySelectorAll('.chat-message').length===3")
        page.get_by_label("消息内容").fill("你好，可以约明天下午当面验货吗？")
        page.get_by_label("消息内容").press("Enter")
        expect(page.locator(".chat-bubble").filter(has_text="你好，可以约明天下午当面验货吗？")).to_have_count(1)
        expect(page.get_by_text("已发送", exact=True).first).to_be_visible()
        previous_sockets = len(fixture.sockets)
        page.get_by_label("消息内容").fill("断线重试也只保存一次")
        page.get_by_label("消息内容").press("Enter")
        pump_until(page, lambda: len(fixture.sockets) > previous_sockets, "未自动重新握手")
        page.wait_for_function("document.querySelector('.chat-connection')?.textContent.includes('已连接') && !document.querySelector('.chat-message.pending')")
        assert sum(m["content"] == "断线重试也只保存一次" for m in fixture.messages) == 1
        assert len({m["client_msg_id"] for m in fixture.messages if m["content"] == "断线重试也只保存一次"}) == 1
        page.get_by_label("消息内容").fill("<img src=x onerror=alert(1)>")
        page.get_by_label("消息内容").press("Enter")
        expect(page.locator(".chat-bubble").filter(has_text="<img src=x onerror=alert(1)>")).to_have_count(1)
        assert page.locator(".chat-message-feed img").count() == 0
        pump_until(page, lambda: fixture.read_seq == len(fixture.messages), "可见窗口未推进已读")
        nav = page.get_by_role("navigation", name="主导航")
        nav.get_by_role("button", name="首页", exact=True).click()
        previous_read = fixture.read_seq
        fixture.remote("可以，明天下午三点地铁站见。")
        page.wait_for_function("document.querySelector('.chat-nav-unread') !== null")
        assert fixture.read_seq == previous_read, "后台收到消息不得已读"
        nav.get_by_role("button", name=re.compile("^消息")).click()
        expect(page.locator(".chat-bubble").filter(has_text="地铁站见")).to_have_count(1)
        fixture.remote("我会带上原盒和配件。", push=False)
        page.evaluate("window.dispatchEvent(new Event('online'))")
        expect(page.locator(".chat-bubble").filter(has_text="我会带上原盒和配件。")).to_have_count(1)
        page.screenshot(path=str(ARTIFACTS / "chat-desktop.png"), full_page=True)
        page.set_viewport_size({"width": 390, "height": 844})
        expect(page.get_by_label("会话列表")).to_be_visible()
        page.locator(".chat-conversation").first.click()
        expect(page.get_by_label("消息内容")).to_be_visible()
        assert page.evaluate("document.documentElement.scrollWidth <= window.innerWidth"), "手机页面横向溢出"
        page.screenshot(path=str(ARTIFACTS / "chat-mobile.png"), full_page=True)
        page.get_by_role("button", name="← 会话", exact=True).click()
        expect(page.get_by_label("会话列表")).to_be_visible()
        page.set_viewport_size({"width": 1365, "height": 950})
        # 超过一页的会话重载后先加载最近历史，向前翻页保持原消息锚点。
        for i in range(27):
            fixture.remote("历史咨询补充 " + str(i + 1), push=False)
        page.reload()
        page.wait_for_load_state("networkidle")
        page.get_by_role("navigation", name="主导航").get_by_role("button", name=re.compile("^消息")).click()
        expect(page.locator(".chat-message")).to_have_count(20)
        feed = page.locator(".chat-message-feed")
        feed.evaluate("node => node.scrollTop = 0")
        first_id = page.locator(".chat-message").first.get_attribute("data-message-id")
        anchor = page.locator(f'[data-message-id="{first_id}"]')
        original_top = anchor.bounding_box()["y"]
        page.get_by_role("button", name="查看更早消息", exact=True).click()
        expect(page.locator(".chat-message")).to_have_count(len(fixture.messages))
        pump_until(page, lambda: abs(anchor.bounding_box()["y"] - original_top) < 5, "向前翻页改变阅读位置")
        page.locator(".user-dropdown summary").click()
        page.get_by_role("button", name="退出登录", exact=True).click()
        expect(page.get_by_label("消息内容")).to_have_count(0)
        expect(page.locator(".chat-nav-unread")).to_have_count(0)
        assert errors == [], errors
        context.close()
        browser.close()
    print("PASS: 浏览器夹具验证商品联系卖家、单条展示、稳定键重连、遗漏补拉、后台未读、文本安全及手机布局")
    print("截图目录:", ARTIFACTS)


if __name__ == "__main__":
    run()
