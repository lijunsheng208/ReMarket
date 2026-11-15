"""验证真实订单引发的预占、释放、售出事件能同步搜索，仅操作自建验收商品。"""
import secrets
from search_acceptance import call, search, wait_for

# account 创建独立验收身份，不读取或输出用户凭据。
def account():
    username = 'test_' + secrets.token_hex(6)
    password = secrets.token_urlsafe(18)
    call('/auth/register', 'POST', {'username': username, 'password': password, 'nickname': '搜索状态验收'})
    return call('/auth/login', 'POST', {'username': username, 'password': password})['access_token']

# main 通过真实订单接口推进状态，完成只代表双方确认，不涉及在线支付。
def main():
    seller, buyer = account(), account()
    marker = 'state' + secrets.token_hex(5)
    category = call('/categories')['categories'][0]['id']
    p = call('/products', 'POST', {'category_id': category, 'title': marker, 'price_cent': '100', 'description': '搜索状态测试'}, seller)['product']
    product_id = p['id']
    order_id = None
    try:
        call('/products/' + product_id + '/publish', 'POST', {'expected_version': p['aggregate_version']}, seller)
        wait_for(lambda: bool(search(marker).get('products')), 'order fixture published and visible')
        order_id = call('/orders', 'POST', {'product_id': product_id}, buyer)['order']['id']
        wait_for(lambda: call('/orders/' + order_id, token=buyer)['order']['status'] == 'ORDER_STATUS_RESERVED', 'order reserved')
        wait_for(lambda: not search(marker).get('products'), 'reserved -> absent in search')
        call('/orders/' + order_id + '/cancel', 'POST', None, buyer)
        wait_for(lambda: call('/orders/' + order_id, token=buyer)['order']['status'] == 'ORDER_STATUS_CANCELLED', 'order cancelled')
        wait_for(lambda: bool(search(marker).get('products')), 'released -> visible in search')
        order_id = call('/orders', 'POST', {'product_id': product_id}, buyer)['order']['id']
        wait_for(lambda: call('/orders/' + order_id, token=buyer)['order']['status'] == 'ORDER_STATUS_RESERVED', 'second order reserved')
        call('/orders/' + order_id + '/confirm', 'POST', None, buyer)
        call('/orders/' + order_id + '/confirm', 'POST', None, seller)
        wait_for(lambda: call('/orders/' + order_id, token=buyer)['order']['status'] == 'ORDER_STATUS_COMPLETED', 'both parties confirmed')
        wait_for(lambda: call('/products/' + product_id, token=seller)['product']['status'] == 'PRODUCT_STATUS_SOLD' and not search(marker).get('products'), 'sold -> absent in search')
        print('FIXTURE', marker, 'product', product_id, 'retained SOLD', flush=True)
    finally:
        if order_id:
            order = call('/orders/' + order_id, token=buyer)['order']
            if order['status'] in ['ORDER_STATUS_PENDING', 'ORDER_STATUS_RESERVED']:
                call('/orders/' + order_id + '/cancel', 'POST', None, buyer)
        p = call('/products/' + product_id, token=seller)['product']
        if p['status'] == 'PRODUCT_STATUS_ON_SALE':
            call('/products/' + product_id + '/off-shelf', 'POST', {'expected_version': p['aggregate_version']}, seller)

if __name__ == '__main__':
    main()
