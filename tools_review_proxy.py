# Copyright (c) 黄智韬. All rights reserved.
# 本地 UI 审查专用：HTTP 8901 -> HTTPS 8900 的开发反代（仅临时使用，不随产品分发）。
import http.server
import ssl
import http.client


class Proxy(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'

    def _forward(self, body=None):
        ctx = ssl._create_unverified_context()
        conn = http.client.HTTPSConnection('localhost', 8900, context=ctx, timeout=30)
        headers = {k: v for k, v in self.headers.items() if k.lower() != 'host'}
        conn.request(self.command, self.path, body=body, headers=headers)
        resp = conn.getresponse()
        data = resp.read()
        self.send_response(resp.status)
        for k, v in resp.getheaders():
            if k.lower() in ('transfer-encoding', 'content-length', 'connection'):
                continue
            self.send_header(k, v)
        self.send_header('Content-Length', str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self._forward()

    def do_POST(self):
        n = int(self.headers.get('Content-Length') or 0)
        self._forward(self.rfile.read(n) if n else None)

    def do_PUT(self):
        n = int(self.headers.get('Content-Length') or 0)
        self._forward(self.rfile.read(n) if n else None)

    def do_DELETE(self):
        self._forward()

    def log_message(self, *a):
        pass


if __name__ == '__main__':
    http.server.ThreadingHTTPServer(('127.0.0.1', 8901), Proxy).serve_forever()
