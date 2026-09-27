#!/usr/bin/env python3
# 採取用の偽の上流: git http-backend (CGI) を包み、receive-pack / upload-pack の POST の本文を、そのまま保存する。
import http.server, os, subprocess, sys, json, socketserver

ROOT = sys.argv[1]          # bare repo の親
OUT = sys.argv[2]           # 採取の出力先
PORT = int(sys.argv[3])
counter = [0]

def dechunk(rfile):
    out = b''
    while True:
        line = rfile.readline().strip()
        n = int(line.split(b';')[0], 16)
        if n == 0:
            while rfile.readline().strip():
                pass
            return out
        out += rfile.read(n)
        rfile.readline()

class H(http.server.BaseHTTPRequestHandler):
    protocol_version = 'HTTP/1.1'
    def log_message(self, *a): pass
    def handle_any(self, method):
        path, _, query = self.path.partition('?')
        if self.headers.get('Transfer-Encoding', '').lower() == 'chunked':
            body = dechunk(self.rfile)
        else:
            n = int(self.headers.get('Content-Length', 0))
            body = self.rfile.read(n) if n else b''
        if method == 'POST':
            counter[0] += 1
            tag = open(os.path.join(OUT, '..', 'TAG')).read().strip()
            svc = path.rsplit('/', 1)[-1]
            base = f'{OUT}/{tag}.{counter[0]:02d}.{svc}'
            open(base + '.body', 'wb').write(body)
            meta = {k: v for k, v in self.headers.items() if k.lower() in ('content-type', 'content-encoding', 'transfer-encoding', 'content-length', 'accept', 'user-agent', 'git-protocol', 'accept-encoding')}
            meta['path'] = self.path
            open(base + '.meta.json', 'w').write(json.dumps(meta, indent=1))
        env = {
            'PATH': os.environ['PATH'], 'GIT_PROJECT_ROOT': ROOT, 'GIT_HTTP_EXPORT_ALL': '1',
            'REQUEST_METHOD': method, 'PATH_INFO': path, 'QUERY_STRING': query,
            'CONTENT_TYPE': self.headers.get('Content-Type', ''), 'CONTENT_LENGTH': str(len(body)),
            'REMOTE_USER': 'cap', 'REMOTE_ADDR': '127.0.0.1', 'GIT_HTTP_MAX_REQUEST_BUFFER': '1g',
            'GIT_CONFIG_NOSYSTEM': '1', 'HOME': ROOT,
        }
        if self.headers.get('Git-Protocol'): env['GIT_PROTOCOL'] = self.headers['Git-Protocol']
        if self.headers.get('Content-Encoding'): env['HTTP_CONTENT_ENCODING'] = self.headers['Content-Encoding']
        p = subprocess.run(['git', 'http-backend'], input=body, env=env, capture_output=True)
        head, _, rest = p.stdout.partition(b'\r\n\r\n')
        status, hdrs = 200, []
        for line in head.split(b'\r\n'):
            k, _, v = line.decode().partition(':')
            if k.lower() == 'status': status = int(v.strip().split()[0])
            elif k: hdrs.append((k, v.strip()))
        self.send_response(status)
        for k, v in hdrs: self.send_header(k, v)
        self.send_header('Content-Length', str(len(rest)))
        self.end_headers()
        self.wfile.write(rest)
    def do_GET(self): self.handle_any('GET')
    def do_POST(self): self.handle_any('POST')

class S(socketserver.ThreadingMixIn, http.server.HTTPServer):
    daemon_threads = True
S(('127.0.0.1', PORT), H).serve_forever()
