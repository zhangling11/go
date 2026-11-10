#!/usr/bin/env bash
# 编译 + 上传 + 重启（在本地 Windows/Git Bash 里执行）
#
# 用法：
#   ./build.sh                 # 用默认 A 机地址
#   ./build.sh 10.19.207.10    # 指定服务器 IP
#   PORT=8000 ./build.sh       # 指定端口
set -euo pipefail

HOST="${1:-10.19.207.10}"
SSH_PORT="${SSH_PORT:-55555}"
USER_NAME="${USER_NAME:-root}"
PORT="${PORT:-8000}"
APP="mig-demo"
REMOTE_DIR="${REMOTE_DIR:-/root/${APP}}"

echo "==> 编译 (linux/amd64, 无 CGO)"
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o "${APP}" .

echo "==> 上传到 ${USER_NAME}@${HOST}:${SSH_PORT}"
ssh -p "${SSH_PORT}" "${USER_NAME}@${HOST}" "mkdir -p ${REMOTE_DIR}"
scp -P "${SSH_PORT}" "${APP}" "${USER_NAME}@${HOST}:${REMOTE_DIR}/"

echo "==> 重启服务"
ssh -p "${SSH_PORT}" "${USER_NAME}@${HOST}" bash -s <<EOF
set -e
cd ${REMOTE_DIR}
pkill -f './${APP}' 2>/dev/null || true
chmod +x ./${APP}
nohup ./${APP} -listen ":${PORT}" > ${REMOTE_DIR}/run.log 2>&1 &
sleep 1
echo "已启动: http://\$(hostname -I | awk '{print \$1}'):${PORT}"
tail -n 5 ${REMOTE_DIR}/run.log || true
EOF
