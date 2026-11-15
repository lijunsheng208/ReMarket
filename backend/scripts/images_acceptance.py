"""通过已配置 OSS 的真实 Gateway 验证私有图片链路，保留验收记录并下架自建商品。"""
import json
import secrets
import struct
import urllib.error
import urllib.parse
import urllib.request
import zlib
import os
BASE = os.environ.get('IMAGES_ACCEPTANCE_URL', 'http://127.0.0.1:8080') + '/api/v1'

# call 使用真实 Gateway，凭据及预签名地址不写入日志。
def call(path, method='GET', data=None, token=''):
    headers = {'Content-Type': 'application/json'}
    if token:
        headers['Authorization'] = 'Bearer ' + token
    req = urllib.request.Request(BASE + path, data=json.dumps(data).encode() if data is not None else None, headers=headers, method=method)
    with urllib.request.urlopen(req, timeout=35) as response:
        return json.load(response)


# png 构造带正确校验和的小型 PNG，不依赖外部图片或 Pillow。
def png():
    def chunk(kind, data):
        return struct.pack('!I', len(data)) + kind + data + struct.pack('!I', zlib.crc32(kind + data) & 0xffffffff)
    return b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('!IIBBBBB', 2, 2, 8, 2, 0, 0, 0)) + chunk(b'IDAT', zlib.compress(b'\0\xff\0\0\xff\0\0' * 2)) + chunk(b'IEND', b'')

# put 使用短期签名 URL 直传，不输出签名参数。
def put(url, data):
    req = urllib.request.Request(url, data=data, method='PUT', headers={'Content-Type': 'image/png'})
    with urllib.request.urlopen(req, timeout=30) as response:
        assert response.status in (200, 201)

# main 验证直传、审核、幂等、私有访问及临时对象覆盖隔离。
def main():
    password = secrets.token_urlsafe(18)
    username = 'oss_' + secrets.token_hex(6)
    call('/auth/register', 'POST', {'username': username, 'password': password, 'nickname': 'OSS图片验收'})
    token = call('/auth/login', 'POST', {'username': username, 'password': password})['access_token']
    category = call('/categories')['categories'][0]['id']
    p = call('/products', 'POST', {'category_id': category, 'title': 'OSS图片链路验收', 'price_cent': '100'}, token)['product']
    try:
        raw = png()
        upload = call('/products/' + p['id'] + '/images/uploads', 'POST', {'content_type': 'image/png', 'size_bytes': len(raw)}, token)
        print('PASS 商品创建及上传凭证申请', flush=True)
        put(upload['upload_url'], raw)
        result = call('/products/' + p['id'] + '/images/complete', 'POST', {'upload_id': upload['upload_id']}, token)['product']
        assert len(result['images']) == 1 and int(result['aggregate_version']) > int(p['aggregate_version'])
        again = call('/products/' + p['id'] + '/images/complete', 'POST', {'upload_id': upload['upload_id']}, token)['product']
        assert again['images'] == result['images'] and again['aggregate_version'] == result['aggregate_version']
        print('PASS OSS预签名直传、服务端校验与幂等完成', flush=True)
        image_url = call('/products/' + p['id'] + '/images', token=token)['images'][0]['url']
        with urllib.request.urlopen(image_url, timeout=15) as response:
            assert response.headers.get_content_type() == 'image/png'
            confirmed = response.read()
        unsigned = urllib.parse.urlunsplit(urllib.parse.urlsplit(image_url)._replace(query=''))
        try:
            urllib.request.urlopen(unsigned, timeout=15)
        except urllib.error.HTTPError as error:
            assert error.code == 403
        else:
            raise AssertionError('正式图片允许匿名未签名访问')
        print('PASS 私有对象拒绝无签名访问，签名图片可读', flush=True)
        put(upload['upload_url'], b'invalid file overwritten after complete')
        with urllib.request.urlopen(image_url, timeout=15) as response:
            assert response.read() == confirmed
        print('PASS 重用临时上传凭据不能覆盖正式图片', flush=True)
        try:
            call('/products/' + p['id'] + '/images')
        except urllib.error.HTTPError as error:
            assert error.code == 404
        else:
            raise AssertionError('匿名访问草稿图片未拒绝')
        p = call('/products/' + p['id'] + '/publish', 'POST', {'expected_version': result['aggregate_version']}, token)['product']
        assert call('/products/' + p['id'] + '/images')['images']
        print('PASS 草稿私有及发布后图片可展示', flush=True)
        print('验收商品 ID:', p['id'], flush=True)
    finally:
        current = call('/products/' + p['id'], token=token)['product']
        if current['status'] == 'PRODUCT_STATUS_ON_SALE':
            call('/products/' + p['id'] + '/off-shelf', 'POST', {'expected_version': current['aggregate_version']}, token)
        print('自建商品已下架或保留为未公开草稿，图片与审计记录保留。', flush=True)

if __name__ == '__main__':
    try:
        main()
    except urllib.error.HTTPError as error:
        # HTTPError 默认字符串可能带签名 URL，禁止将其打印。
        import xml.etree.ElementTree as ET
        try:
            root = ET.fromstring(error.read())
            code = root.findtext('Code', '')
        except Exception:
            code = ''
        raise SystemExit('图片验收 HTTP 失败，状态码 ' + str(error.code) + '，OSS错误码 ' + code) from None
