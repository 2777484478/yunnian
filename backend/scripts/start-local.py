#!/usr/bin/env python3
"""本地起后端，并让它脱离当前终端存活。

为什么要 double-fork
--------------------
每次 shell 调用都是独立的进程组，`./bin/yunnian-server &` 起的进程会随该次调用
结束被 SIGHUP 回收。表现是**最坑的一种**：刚探通 `/healthz` 返回 200，下一条命令就
Connection refused，而且中间那些请求一条都不落日志 —— 看起来像"后端自己崩了"。
macOS 没有 `setsid` 命令，所以只能 fork → 父进程退出 → `setsid()` → 再 fork →
重定向 0/1/2 → `execv`。

为什么要清代理环境变量
----------------------
Go 的 `http.ProxyFromEnvironment` 会读 `HTTP_PROXY/HTTPS_PROXY`。本机开着代理
（SnailLink，127.0.0.1:7890）时，集群内网地址会被塞进代理隧道，表现为 Jenkins /
K8s 调用**毫秒级 connection refused、且日志里没有 cause**，而且**只有外呼的那一路挂**，
本地接口一切正常 —— 极难定位。所以这里在 exec 之前把 5 个变量全部删掉。

用法
----
    python3 scripts/start-local.py          # 启动（已在跑则先停）
    python3 scripts/start-local.py --stop   # 停掉监听 8080 的 yunnian-server
    python3 scripts/start-local.py --status # 看进程与探针
"""

import os
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)  # backend/
BIN = os.path.join(ROOT, "bin", "yunnian-server")
LOG = os.path.join(ROOT, "bin", "yunnian-server.log")
PORT = 8080
PROG = "yunnian-server"

PROXY_VARS = ("HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "ALL_PROXY")


def listeners():
    """返回监听 8080 的进程 [(pid, command)]。本环境 `ps` 被禁，只能用 lsof。"""
    try:
        out = subprocess.run(
            ["lsof", "-nP", "-t", f"-iTCP:{PORT}", "-sTCP:LISTEN"],
            capture_output=True, text=True, check=False,
        ).stdout.split()
    except FileNotFoundError:
        return []
    found = []
    for pid in out:
        cmd = subprocess.run(
            ["lsof", "-p", pid], capture_output=True, text=True, check=False
        ).stdout
        # 只认命令行里确实是这个二进制的，避免误杀占了同一端口的别的程序。
        if PROG in cmd:
            found.append((int(pid), PROG))
    return found


def stop():
    found = listeners()
    if not found:
        print(f"没有监听 {PORT} 的 {PROG}")
        return 0
    for pid, _ in found:
        print(f"停止 {PROG} pid={pid}")
        os.kill(pid, 15)
    for _ in range(50):
        if not listeners():
            break
        import time
        time.sleep(0.1)
    left = listeners()
    if left:
        print(f"仍未退出，强制结束：{left}", file=sys.stderr)
        for pid, _ in left:
            os.kill(pid, 9)
    return 0


def status():
    found = listeners()
    print(f"监听 {PORT} 的 {PROG}: {found or '无'}")
    if not found:
        return 1
    # 127.0.0.1 在本沙箱会走代理，必须 --noproxy。
    probe = subprocess.run(
        ["curl", "-s", "--noproxy", "*", f"http://127.0.0.1:{PORT}/healthz"],
        capture_output=True, text=True, check=False,
    )
    print("healthz:", (probe.stdout or probe.stderr).strip()[:400])
    return 0


def start():
    if not os.path.exists(BIN):
        print(f"找不到 {BIN}，先 `go build -o bin/yunnian-server ./cmd/server`", file=sys.stderr)
        return 1
    stop()  # 换二进制必须先把旧进程停干净，否则端口占用会静默起不来

    pid = os.fork()
    if pid > 0:
        os.waitpid(pid, 0)  # 等第一层子进程退出，确保 setsid 已生效
        print(f"已启动，日志 {LOG}")
        return 0

    os.setsid()
    if os.fork() > 0:
        os._exit(0)

    os.chdir(ROOT)
    for k in PROXY_VARS:
        os.environ.pop(k, None)
    # 内网地址也要显式绕过（有些库只认 NO_PROXY）
    os.environ["NO_PROXY"] = "*"
    os.environ["no_proxy"] = "*"

    fd = os.open(LOG, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
    os.dup2(fd, 1)
    os.dup2(fd, 2)
    os.dup2(os.open(os.devnull, os.O_RDONLY), 0)
    os.execv(BIN, [BIN])
    os._exit(1)  # execv 失败才会到这里


if __name__ == "__main__":
    arg = sys.argv[1] if len(sys.argv) > 1 else ""
    if arg == "--stop":
        raise SystemExit(stop())
    if arg == "--status":
        raise SystemExit(status())
    raise SystemExit(start())
