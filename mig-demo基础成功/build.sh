#!/usr/bin/env bash
# 编译 + 上传 + 重启（在本地 Windows/Git Bash 里执行）
#
# 用法：
#   ./build.sh                 # 部署到 10.19.207.10:8000
#   ./build.sh 10.19.207.34    # 指定服务器 IP
#   PORT=8077 ./build.sh       # 指定端口
#   NO_DEPLOY=1 ./build.sh     # 只在本机编译，不上传
set -euo pipefail

HOST="${1:-10.19.207.10}"
SSH_PORT="${SSH_PORT:-55555}"
USER_NAME="${USER_NAME:-root}"
PORT="${PORT:-8000}"
APP="mig-demo"
REMOTE_DIR="${REMOTE_DIR:-/root/${APP}}"
REMOTE_BIN="${REMOTE_DIR}/${APP}"
REMOTE_LOG="${REMOTE_DIR}/run.log"

echo "==> 编译 (linux/amd64, 无 CGO)"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "${APP}" .

if [ "${NO_DEPLOY:-0}" = "1" ]; then
  echo "==> NO_DEPLOY=1，跳过上传"
  exit 0
fi

echo "==> 上传到 ${USER_NAME}@${HOST}:${SSH_PORT}"
ssh -p "${SSH_PORT}" "${USER_NAME}@${HOST}" "mkdir -p ${REMOTE_DIR}"
# 传成 .new：直接覆盖正在运行的二进制会 ETXTBSY
scp -P "${SSH_PORT}" "${APP}" "${USER_NAME}@${HOST}:${REMOTE_BIN}.new"

echo "==> 重启服务"
ssh -p "${SSH_PORT}" "${USER_NAME}@${HOST}" bash -s <<EOF
set -e
cd ${REMOTE_DIR}
# -x 精确匹配进程名；pkill -f mig-demo 会连 ssh 自己的命令行一起杀掉
pkill -9 -x ${APP} 2>/dev/null || true
sleep 1
mv -f ${REMOTE_BIN}.new ${REMOTE_BIN}
chmod +x ${REMOTE_BIN}
nohup ${REMOTE_BIN} -listen ":${PORT}" > ${REMOTE_LOG} 2>&1 &
sleep 2
if pgrep -x ${APP} > /dev/null; then
  echo "已启动: http://\$(hostname -I | awk '{print \$1}'):${PORT}"
else
  echo "启动失败，日志："
  tail -n 20 ${REMOTE_LOG} || true
  exit 1
fi
ss -ltn 2>/dev/null | grep ":${PORT} " || echo "警告：${PORT} 端口未监听"
tail -n 5 ${REMOTE_LOG} || true
EOF
