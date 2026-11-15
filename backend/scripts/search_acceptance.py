"""通过真实 Gateway 验证搜索完整链路；只创建独立验收数据，结束时下架自建商品。"""
import json
import os
import secrets
import time
import urllib.parse
import urllib.request

BASE = os.environ.get('SEARCH_ACCEPTANCE_URL', 'http://127.0.0.1:18081') + '/api/v1'

# call 通过 Gateway 操作业务，不直接写数据库或 ES，凭据只保留在内存。
def call(path, method='GET', data=None, token=''):
    headers = {'Content-Type': 'application/json'}
    if path == '/orders' and method == 'POST':
        headers['Idempotency-Key'] = secrets.token_hex(16)
    if token:
        headers['Authorization'] = 'Bearer ' + token
    req = urllib.request.Request(BASE + path, data=json.dumps(data).encode() if data is not None else None, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=15) as response:
        return json.load(response)

# search 每次默认打开新快照，观察最终状态同步而非复用旧 PIT。
def search(marker, **params):
    return call('/search/products?' + urllib.parse.urlencode({'query': marker, 'page_size': 12, **params}))

# wait_for 在有界时间内等待最终一致视图，不把固定延迟当成功依据。
def wait_for(check, label):
    end = time.monotonic() + 45
    while time.monotonic() < end:
        if check():
            print('PASS', label, flush=True)
            return
        time.sleep(.5)
    raise AssertionError(label + ' timeout')

# main 验证发布、分页、筛选、修改、下架与恢复；finally 仅下架本次创建的商品。
def main():
    marker = 'srch' + secrets.token_hex(5)
    password = secrets.token_urlsafe(18)
    username = 'test_' + secrets.token_hex(6)
    call('/auth/register', 'POST', {'username': username, 'password': password, 'nickname': '搜索验收'})
    token = call('/auth/login', 'POST', {'username': username, 'password': password})['access_token']
    category = call('/categories')['categories'][0]['id']
    created = []
    try:
        for i in range(13):
            p = call('/products', 'POST', {'category_id': category, 'title': f'{marker} 手机 {i}', 'description': '搜索完整链路验收', 'price_cent': str(10000 + i)}, token)['product']
            created.append(p['id'])
            call('/products/' + p['id'] + '/publish', 'POST', {'expected_version': p['aggregate_version']}, token)
        wait_for(lambda: len(search(marker).get('products', [])) == 12 and bool(search(marker).get('next_cursor')), 'publish -> Kafka -> ES -> HTTP')
        first = search(marker, sort='price_asc')
        second = search(marker, sort='price_asc', cursor=first['next_cursor'])
        ids = [p['id'] for p in first['products'] + second.get('products', [])]
        assert len(ids) == 13 and len(set(ids)) == 13 and not second.get('next_cursor')
        assert [int(p['price_cent']) for p in first['products']] == sorted(int(p['price_cent']) for p in first['products'])
        print('PASS cursor stable pagination, no duplicates, price sorting', flush=True)
        filtered = search(marker, category_id=category, min_price_cent=10003, max_price_cent=10005)
        assert len(filtered.get('products', [])) == 3
        print('PASS category and inclusive price filters', flush=True)
        p = call('/products/' + created[0], token=token)['product']
        p = call('/products/' + p['id'], 'PATCH', {'expected_version': p['aggregate_version'], 'title': marker + ' updated', 'price_cent': '12345'}, token)['product']
        wait_for(lambda: any(x['id'] == p['id'] and x['price_cent'] == '12345' for x in search(marker + ' updated').get('products', [])), 'update -> search snapshot')
        p = call('/products/' + p['id'] + '/off-shelf', 'POST', {'expected_version': p['aggregate_version']}, token)['product']
        wait_for(lambda: not search(marker + ' updated').get('products'), 'off shelf -> absent in new search')
        p = call('/products/' + p['id'] + '/publish', 'POST', {'expected_version': p['aggregate_version']}, token)['product']
        wait_for(lambda: bool(search(marker + ' updated').get('products')), 'republish -> visible again')
        print('FIXTURE', marker, 'products', len(created), flush=True)
    finally:
        for product_id in created:
            p = call('/products/' + product_id, token=token)['product']
            if p['status'] == 'PRODUCT_STATUS_ON_SALE':
                call('/products/' + product_id + '/off-shelf', 'POST', {'expected_version': p['aggregate_version']}, token)
        wait_for(lambda: not search(marker).get('products'), 'cleanup: own fixtures off shelf and absent')

if __name__ == '__main__':
    main()
