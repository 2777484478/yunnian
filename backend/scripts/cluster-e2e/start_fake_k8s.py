#!/usr/bin/env python3
"""double-fork 启动假 K8s，让它脱离当前终端存活。

为什么不能直接 `python3 fake_k8s.py &`：
每个 shell 调用是独立的，`&` 起的进程会随该调用结束被 SIGHUP 回收 ——
表现为"刚探通 401、下一个命令就 Connection refused"。macOS 没有 `setsid`，
所以只能 double-fork（fork → 父退 → setsid → 再 fork → 重定向 0/1/2 → execv）。

用法：python3 start_fake_k8s.py [port]
停止：lsof -nP -iTCP:16443 -sTCP:LISTEN  拿到 PID 后 kill
"""
import os
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
PY = sys.executable or "python3"
LOG = os.path.join(HERE, "fake-k8s.log")

port = sys.argv[1] if len(sys.argv) > 1 else "16443"

pid = os.fork()
if pid > 0:
    print("parent exiting, child pid =", pid)
    os._exit(0)

os.setsid()

pid = os.fork()
if pid > 0:
    os._exit(0)

os.chdir(HERE)
fd = os.open(LOG, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o644)
os.dup2(fd, 1)
os.dup2(fd, 2)
os.dup2(os.open(os.devnull, os.O_RDONLY), 0)
os.execv(PY, [PY, os.path.join(HERE, "fake_k8s.py"), port])
