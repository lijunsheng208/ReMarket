"""通过真实 Gateway 验证卖家主页与关注状态，结束后取消关系并下架自建商品。"""
import secrets
from images_acceptance import call

# main 仅创建独立测试账号与商品，不修改既有用户资料。
def main():
    accounts = []
    for label in ('卖家', '买家'):
        username = 'profile_' + secrets.token_hex(5)
        password = secrets.token_urlsafe(18)
        call('/auth/register', 'POST', {'username': username, 'password': password, 'nickname': '主页验收' + label})
        result = call('/auth/login', 'POST', {'username': username, 'password': password})
        accounts.append((result['user']['id'], result['access_token']))
    seller, buyer = accounts
    relation = '/users/' + seller[0] + '/follow'
    public_path = '/users/' + seller[0] + '/profile'
    product = None
    try:
        profile = call(public_path)
        assert profile['nickname'] == '主页验收卖家' and not profile.get('is_following')
        assert 'username' not in profile and 'role' not in profile
        for _ in range(2):
            call(relation, 'POST', token=buyer[1])
        status = call(relation, token=buyer[1])
        assert status['is_following'] and status['follower_count'] == '1'
        own = call('/users/' + buyer[0] + '/profile', token=buyer[1])
        assert own['is_self'] and own['following_count'] == '1'
        assert call(public_path)['follower_count'] == '1'
        print('PASS 公开资料、匿名状态、重复关注幂等及粉丝/关注统计', flush=True)
        category = call('/categories')['categories'][0]['id']
        product = call('/products', 'POST', {'category_id': category, 'title': '主页商品验收', 'price_cent': '100'}, seller[1])['product']
        assert not call('/users/' + seller[0] + '/products').get('products')
        product = call('/products/' + product['id'] + '/publish', 'POST', {'expected_version': product['aggregate_version']}, seller[1])['product']
        page = call('/users/' + seller[0] + '/products?page_size=1')
        assert [p['id'] for p in page['products']] == [product['id']]
        assert not call('/users/' + buyer[0] + '/products').get('products')
        print('PASS 卖家主页排除草稿与他人商品，发布后可见', flush=True)
        for _ in range(2):
            call(relation, 'DELETE', token=buyer[1])
        state = call(relation, token=buyer[1])
        assert not state.get('is_following') and state.get('follower_count', '0') == '0'
        print('PASS 取消关注幂等，关系及统计归零', flush=True)
    finally:
        call(relation, 'DELETE', token=buyer[1])
        if product:
            current = call('/products/' + product['id'], token=seller[1])['product']
            if current['status'] == 'PRODUCT_STATUS_ON_SALE':
                call('/products/' + product['id'] + '/off-shelf', 'POST', {'expected_version': current['aggregate_version']}, seller[1])
        print('自建关注已取消，自建商品已下架；保留验收账号和审计记录。', flush=True)

if __name__ == '__main__':
    main()
