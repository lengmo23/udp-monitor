#!/bin/bash

# ====================================================
# UDP Cloud Monitor - 统一交互式部署脚本
# 直接下载预编译二进制（无需 Go / 无需源码）。
# 用法: bash install.sh  → 选择 安装/卸载 Server 或 Agent
#   - Server: 自动生成端口 + API 密钥，并打印出来
#   - Agent : 询问服务器地址、ipinfo token、API 密钥
# ====================================================

CONFIG_DIR="/etc/udp-monitor"
RELEASE_BASE="https://github.com/lengmo23/udp-monitor/releases/latest/download"

# 以 root 运行时无需 sudo（部分精简系统未装 sudo）
SUDO=""
if [ "$(id -u)" -ne 0 ]; then SUDO="sudo"; fi

# 生成随机 API 密钥（优先 openssl，退化用 /dev/urandom）
gen_secret() {
    if command -v openssl >/dev/null 2>&1; then
        openssl rand -hex 24
    else
        head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n'
    fi
}

# uname -m → release 资产架构后缀
arch_suffix() {
    case "$(uname -m)" in
        x86_64|amd64)  echo "amd64" ;;
        aarch64|arm64) echo "arm64" ;;
        *)             echo "" ;;
    esac
}

# 下载 URL 到临时文件，成功则打印临时文件路径，失败返回非 0
http_download() {
    local url="$1" tmp
    tmp="$(mktemp)"
    if command -v curl >/dev/null 2>&1; then
        curl -fsSL "$url" -o "$tmp" || { rm -f "$tmp"; return 1; }
    elif command -v wget >/dev/null 2>&1; then
        wget -qO "$tmp" "$url" || { rm -f "$tmp"; return 1; }
    else
        echo "[❌ 错误] 需要 curl 或 wget 之一。" >&2
        rm -f "$tmp"; return 1
    fi
    [ -s "$tmp" ] || { rm -f "$tmp"; return 1; }
    echo "$tmp"
}

# 下载预编译二进制到目标路径：$1=资产基名(udp-central/udp-agent)  $2=目标路径
fetch_binary() {
    local base="$1" dest="$2" arch url tmp
    arch="$(arch_suffix)"
    if [ -z "$arch" ]; then
        echo "[❌ 错误] 不支持的架构: $(uname -m)。仅提供 amd64/arm64 预编译二进制。"
        echo "    请在有 Go 的机器上交叉编译后 scp（见 README）。"
        exit 1
    fi
    url="$RELEASE_BASE/${base}-linux-${arch}"
    echo "[*] 下载预编译二进制: ${base}-linux-${arch}"
    if ! tmp="$(http_download "$url")"; then
        echo "[❌ 错误] 下载失败: $url"
        echo "    请检查网络，或确认该架构的 release 资产存在。"
        exit 1
    fi
    $SUDO mkdir -p "$(dirname "$dest")"
    $SUDO mv "$tmp" "$dest"
    $SUDO chmod +x "$dest"
}

# ==================== Server ====================
install_server() {
    local BIN_NAME="udp-central"
    local INSTALL_DIR="/opt/udp-monitor"
    local SERVICE_NAME="udp-central"
    local CONFIG_FILE="$CONFIG_DIR/server.json"

    # 端口（随机，可回车接受或手动指定）与密钥
    local GEN_PORT=$(( (RANDOM * 32768 + RANDOM) % 40000 + 20000 ))
    read -rp "Web 端口 [回车=随机生成 $GEN_PORT]: " IN_PORT
    local PORT="${IN_PORT:-$GEN_PORT}"
    local SECRET
    SECRET="$(gen_secret)"

    echo ""
    echo "[1/4] ⬇️  下载 Server 二进制 (无需 Go)..."
    $SUDO mkdir -p "$INSTALL_DIR" "$CONFIG_DIR"
    fetch_binary "udp-central" "$INSTALL_DIR/$BIN_NAME"
    echo "[+] ✅ 二进制就位: $INSTALL_DIR/$BIN_NAME"

    echo "[2/4] 📝 写入配置文件..."
    $SUDO tee "$CONFIG_FILE" > /dev/null <<EOF
{
    "WEB_PORT": $PORT,
    "API_SECRET": "$SECRET",
    "WEB_HISTORY_LIMIT": 2000,
    "BATCH_MAX_CHARS": 3800,
    "TG_BOT_TOKEN": "",
    "TG_CHAT_ID": ""
}
EOF
    $SUDO chmod 600 "$CONFIG_FILE"

    echo "[3/4] ⚙️ 注册 systemd 服务..."
    $SUDO tee /etc/systemd/system/$SERVICE_NAME.service > /dev/null <<EOF
[Unit]
Description=Central UDP Monitor Web Service (Go)
After=network.target

[Service]
Type=simple
User=root
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/$BIN_NAME
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

    echo "[4/4] 🚀 启动服务..."
    $SUDO systemctl daemon-reload
    $SUDO systemctl enable "$SERVICE_NAME" >/dev/null 2>&1
    $SUDO systemctl restart "$SERVICE_NAME"

    echo ""
    echo "================= ✅ Server 部署完成 ================="
    echo "  端口 (WEB_PORT)   : $PORT"
    echo "  密钥 (API_SECRET) : $SECRET"
    echo "  配置文件          : $CONFIG_FILE"
    echo "  访问地址          : http://<本机IP>:$PORT"
    echo "-----------------------------------------------------"
    echo "  安装 Agent 时请填入："
    echo "    服务器地址  : <本机IP>:$PORT"
    echo "    API 密钥    : $SECRET"
    echo "    ipinfo token: 到 ipinfo.io 获取(用于 GeoIP，可留空)"
    echo "====================================================="
}

# ==================== Agent ====================
install_agent() {
    local BIN_NAME="udp-agent"
    local INSTALL_DIR="/opt/udp-monitor"
    local SERVICE_NAME="udp-agent"
    local LOG_FILE="/var/log/udp_agent.log"
    local GEOIP_DIR="/usr/share/GeoIP"
    local GEOIP_DB="ipinfo_lite.mmdb"

    # 交互输入
    read -rp "中央服务器地址 (如 1.2.3.4:8866 或 https://your.domain): " SERVER_ADDR
    if [ -z "$SERVER_ADDR" ]; then echo "[❌ 错误] 服务器地址不能为空"; exit 1; fi
    read -rp "ipinfo token (用于 GeoIP 下载，可留空): " IPINFO_TOKEN
    read -rp "API 密钥 (须与 Server 端一致): " API_SECRET
    if [ -z "$API_SECRET" ]; then echo "[❌ 错误] API 密钥不能为空"; exit 1; fi

    # 组装上报 URL：未带协议则默认 http；自动补 /api/upload
    local BASE="$SERVER_ADDR"
    case "$SERVER_ADDR" in
        http://*|https://*) ;;
        *) BASE="http://$SERVER_ADDR" ;;
    esac
    BASE="${BASE%/}"
    local CENTRAL_SERVER_URL="$BASE/api/upload"

    echo ""
    echo "[1/4] ⬇️  下载 Agent 二进制 (无需 Go)..."
    fetch_binary "udp-agent" "$INSTALL_DIR/$BIN_NAME"
    echo "[+] ✅ 二进制就位: $INSTALL_DIR/$BIN_NAME"

    echo "[2/4] 🌍 配置 GeoIP 数据库..."
    $SUDO mkdir -p "$GEOIP_DIR"
    if [ ! -f "$GEOIP_DIR/$GEOIP_DB" ] && [ -z "$IPINFO_TOKEN" ]; then
        echo "[!] ⚠️  未提供 ipinfo token，跳过 GeoIP 下载。"
        echo "    可手动放置数据库到 $GEOIP_DIR/$GEOIP_DB 后重启服务。"
    elif [ ! -f "$GEOIP_DIR/$GEOIP_DB" ]; then
        echo "[*] 正在从 ipinfo.io 下载数据库..."
        local GTMP
        if GTMP="$(http_download "https://ipinfo.io/data/ipinfo_lite.mmdb?_src=frontend&token=${IPINFO_TOKEN}")"; then
            $SUDO mv "$GTMP" "$GEOIP_DIR/$GEOIP_DB"
            echo "[+] ✅ GeoIP 下载成功"
        else
            echo "[❌ 错误] GeoIP 下载失败，请检查 token 或网络。"
        fi
    else
        echo "[+] ✅ GeoIP 数据库已就位"
    fi

    echo "[3/4] ⚙️ 初始化日志并注册 systemd 服务..."
    $SUDO touch "$LOG_FILE"
    $SUDO chmod 666 "$LOG_FILE"
    $SUDO tee /etc/systemd/system/$SERVICE_NAME.service > /dev/null <<EOF
[Unit]
Description=UDP Agent Service (Node Data Collector, Go)
Wants=network-online.target
After=network-online.target xray.service

[Service]
Type=simple
User=root
WorkingDirectory=$INSTALL_DIR
ExecStart=$INSTALL_DIR/$BIN_NAME
StandardOutput=append:$LOG_FILE
StandardError=append:$LOG_FILE
Restart=always
RestartSec=5
Environment="UDP_API_SECRET=$API_SECRET"
Environment="UDP_CENTRAL_SERVER_URL=$CENTRAL_SERVER_URL"

[Install]
WantedBy=multi-user.target
EOF

    echo "[4/4] 🚀 启动服务..."
    $SUDO systemctl daemon-reload
    $SUDO systemctl enable "$SERVICE_NAME" >/dev/null 2>&1
    $SUDO systemctl restart "$SERVICE_NAME"

    echo ""
    echo "================= ✅ Agent 部署完成 ================="
    echo "  上报地址 : $CENTRAL_SERVER_URL"
    echo "  服务名称 : $SERVICE_NAME"
    echo "  查看日志 : tail -f $LOG_FILE"
    echo "  查看状态 : sudo systemctl status $SERVICE_NAME"
    echo "====================================================="
}

# ==================== 卸载 ====================
uninstall_server() {
    local SERVICE_NAME="udp-central"
    local BIN="/opt/udp-monitor/udp-central"
    local CONFIG_FILE="$CONFIG_DIR/server.json"

    echo "[*] 停止并禁用服务 $SERVICE_NAME ..."
    $SUDO systemctl stop "$SERVICE_NAME" 2>/dev/null
    $SUDO systemctl disable "$SERVICE_NAME" 2>/dev/null
    $SUDO rm -f "/etc/systemd/system/$SERVICE_NAME.service"
    $SUDO systemctl daemon-reload
    $SUDO rm -f "$BIN"
    echo "[+] 已移除服务与二进制。"

    read -rp "是否同时删除配置文件 $CONFIG_FILE (含端口/密钥)? [y/N]: " DEL_CFG
    case "$DEL_CFG" in
        y|Y) $SUDO rm -f "$CONFIG_FILE"; $SUDO rmdir "$CONFIG_DIR" 2>/dev/null; echo "[+] 配置文件已删除。" ;;
        *)   echo "[*] 保留配置文件: $CONFIG_FILE" ;;
    esac
    echo "✅ Server 卸载完成。"
}

uninstall_agent() {
    local LOG_FILE="/var/log/udp_agent.log"
    local GEOIP_DB="/usr/share/GeoIP/ipinfo_lite.mmdb"

    echo "[*] 停止并禁用 agent 服务 (含旧版 Python 服务名) ..."
    for svc in udp-agent udp_agent; do
        $SUDO systemctl stop "$svc" 2>/dev/null
        $SUDO systemctl disable "$svc" 2>/dev/null
        $SUDO rm -f "/etc/systemd/system/$svc.service"
    done
    $SUDO systemctl daemon-reload

    $SUDO rm -f /opt/udp-monitor/udp-agent /opt/udp-monitor/last_pos.txt
    $SUDO rm -f /root/udp_agent.py /root/last_pos.txt   # 旧版 Python agent 残留
    $SUDO rm -f "$LOG_FILE"
    echo "[+] 已移除服务、二进制、日志与位置记录。"

    read -rp "是否删除 GeoIP 数据库 $GEOIP_DB? [y/N]: " DEL_DB
    case "$DEL_DB" in
        y|Y) $SUDO rm -f "$GEOIP_DB"; echo "[+] GeoIP 数据库已删除。" ;;
        *)   echo "[*] 保留 GeoIP 数据库: $GEOIP_DB" ;;
    esac
    echo "✅ Agent 卸载完成。"
}

# ==================== 入口 ====================
ROLE="$1"  # 支持从参数传入，跳过菜单
if [ -z "$ROLE" ]; then
    echo "====================================================="
    echo "          UDP Cloud Monitor 部署脚本"
    echo "====================================================="
    echo "  1) 安装 Server (中央控制台)"
    echo "  2) 安装 Agent  (节点采集端)"
    echo "  3) 卸载 Server"
    echo "  4) 卸载 Agent"
    echo "-----------------------------------------------------"
    read -rp "请选择 [1/2/3/4]: " ROLE
fi
case "$ROLE" in
    1) install_server ;;
    2) install_agent ;;
    3) uninstall_server ;;
    4) uninstall_agent ;;
    *) echo "[❌ 错误] 无效选择: '$ROLE'（请输入 1-4）"; exit 1 ;;
esac
