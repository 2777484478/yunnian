#!/usr/bin/env python3
"""最小的假 K8s API Server，只为验证平台的集群探测链路。

它刻意做成"会拒绝"的：
  - /version 等接口全部要求 `Authorization: Bearer <TOKEN>`，否则回 401 —— 用来验证
    平台能把「拨通了但凭据被拒」与「根本拨不通」区分开（unauthorized vs unreachable）。
  - 支持 `?forbidden=1`（存在 forbidden.flag 时生效）：节点接口回 403 —— 用来验证
    「连通但无权列节点」会走 warnings，而不是把整次探测判成失败。

**这不是"假数据"，而是把一个真集群在该协议上的行为如实复现出来**：
它真的走 HTTPS、真的校验 Bearer、真的回 K8s 风格的 Status 对象。
因此在它上面得到的"接入成功/鉴权失败/不可达"结论，与对真集群的结论是同一套代码路径。

用法：
    python3 fake_k8s.py [port]      # 默认 16443，证书缺失时自动生成自签证书
"""
import json
import os
import ssl
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = os.path.dirname(os.path.abspath(__file__))
CERT = os.path.join(HERE, "cert.pem")
KEY = os.path.join(HERE, "key.pem")
FORBIDDEN_FILE = os.path.join(HERE, "forbidden.flag")

TOKEN = "yn-test-token"

NODES = [
    {"metadata": {"name": f"node-{i:02d}"},
     "status": {"conditions": [{"type": "Ready", "status": "True" if i != 2 else "False"}]}}
    for i in (1, 2, 3)
]
NAMESPACES = [{"metadata": {"name": n}} for n in
              ["default", "kube-system", "kube-public", "kube-node-lease",
               "yunnian-test", "yunnian-perf", "yunnian-staging",
               "yunnian-canary", "yunnian-prod", "monitoring",
               "logging", "ingress-nginx", "cert-manager", "argocd"]]


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):  # 静音
        pass

    def _send(self, code, payload):
        body = json.dumps(payload).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = self.path.split("?")[0]
        auth = self.headers.get("Authorization", "")
        if auth != f"Bearer {TOKEN}":
            self._send(401, {"kind": "Status", "status": "Failure",
                             "message": "Unauthorized", "code": 401})
            return
        forbidden = os.path.exists(FORBIDDEN_FILE)
        if path == "/version":
            self._send(200, {"major": "1", "minor": "29", "gitVersion": "v1.29.6",
                             "gitCommit": "deadbeef", "platform": "linux/amd64"})
        elif path == "/api/v1/nodes":
            if forbidden:
                self._send(403, {"kind": "Status", "status": "Failure",
                                 "message": 'nodes is forbidden: User cannot list resource "nodes"',
                                 "code": 403})
                return
            self._send(200, {"kind": "NodeList", "apiVersion": "v1", "items": NODES})
        elif path == "/api/v1/namespaces":
            self._send(200, {"kind": "NamespaceList", "apiVersion": "v1", "items": NAMESPACES})
        else:
            self._send(404, {"kind": "Status", "status": "Failure", "code": 404})


def ensure_cert():
    """证书不在就现生成一份自签的（kubeconfig 里用 insecure-skip-tls-verify，故无需 CA）。"""
    if os.path.exists(CERT) and os.path.exists(KEY):
        return
    import subprocess
    subprocess.run(
        ["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes",
         "-keyout", KEY, "-out", CERT, "-days", "3650",
         "-subj", "/CN=127.0.0.1",
         "-addext", "subjectAltName=IP:127.0.0.1,DNS:localhost"],
        check=True, capture_output=True,
    )
    print(f"已生成自签证书：{CERT}")


def main():
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 16443
    ensure_cert()
    ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ctx.load_cert_chain(CERT, KEY)
    srv = ThreadingHTTPServer(("127.0.0.1", port), Handler)
    srv.socket = ctx.wrap_socket(srv.socket, server_side=True)
    print(f"假 K8s API 已监听 https://127.0.0.1:{port}")
    srv.serve_forever()


if __name__ == "__main__":
    main()
