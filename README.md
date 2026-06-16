# UDP Cloud Monitor

UDP 流量监控系统，包含节点采集代理和中央控制台。

## 项目结构

```
udp-monitor/
├── install.sh               # 统一交互式部署脚本 (选择 Server / Agent)
├── agent/
│   ├── udp_agent.go         # 节点采集代理 (Go 单二进制)
│   └── go.mod               # Go module (依赖 maxminddb-golang 读 GeoIP)
├── server/
│   ├── central_server.go    # 中央控制台 (Go 单二进制 + SSE)
│   ├── index.html           # Web 前端 (编译时内嵌进二进制)
│   ├── go.mod               # Go module (零外部依赖)
│   └── config.example.json  # 配置文件示例
└── README.md
```

## 组件说明

### Agent (节点端)
- Go 实现，编译为单个静态二进制（仅依赖 GeoIP 读取库 maxminddb-golang）
- 监控 Xray 日志中的 UDP 流量
- 基于 GeoIP 识别云厂商 IP
- 实时上报到中央服务器
- 支持 systemd 守护进程

### Server (中央控制台)
- Go 标准库实现，编译为单个静态二进制，运行时零依赖
- SSE (Server-Sent Events) 实时推送，前端用浏览器原生 `EventSource`，无 CDN 依赖
- 支持按日期/用户筛选
- Telegram 消息推送
- 暗色主题 Web 界面（编译时内嵌进二进制）

## 快速部署

Server 和 Agent 共用同一个交互式脚本，运行后按提示选择角色即可。

**一键执行**（脚本会自动克隆仓库并运行，需已装 `curl`）：

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/lengmo23/udp-monitor/main/install.sh)
```

或先克隆再运行：

```bash
git clone https://github.com/lengmo23/udp-monitor.git && cd udp-monitor && bash install.sh
```

运行后选择角色：

```
  1) Server (中央控制台)
  2) Agent  (节点采集端)
请选择要安装的角色 [1/2]:
```

### 选 1：Server（中央控制台）

- 自动生成 **端口** 和 **API 密钥**（也可回车用随机端口或手动指定端口）
- 检测/安装 Go → 编译单二进制 → 写配置 → 注册并启动 systemd 服务
- 安装结束会打印出端口和密钥，**记下来**，安装 Agent 时要用

> ipinfo token 不在这一步处理——它是 ipinfo.io 的外部凭据、且只有 Agent 下载 GeoIP 时才用到。

### 选 2：Agent（节点采集端）

依次询问三项，写入 systemd 后自动启动：

| 提示 | 说明 |
|------|------|
| 中央服务器地址 | `IP:端口`（如 `1.2.3.4:23456`）或完整 `https://域名`；脚本自动补 `/api/upload` |
| ipinfo token | 用于下载 GeoIP 数据库，可留空（留空则跳过下载，需手动放数据库） |
| API 密钥 | 必填，须与 Server 端打印出来的密钥一致 |

### 进阶：不想在服务器上装 Go

可在本机交叉编译，把单个二进制 scp 到目标机：

```bash
# Server
cd server && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o udp-central .
scp udp-central root@<server>:/opt/udp-monitor/

# Agent（同理，在 agent/ 目录）
cd agent && GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o udp-agent .
scp udp-agent root@<node>:/opt/udp-monitor/
```

## 配置说明

### Agent 配置 (`udp_agent.go`)
- `UDP_API_SECRET` (环境变量): 与服务器端 `API_SECRET` 保持一致（必填）
- `UDP_CENTRAL_SERVER_URL` (环境变量): 中央服务器地址，留空则用代码内默认值
- `UDP_LOG_FILE` (环境变量): Xray 日志路径，默认 `/var/log/xray/access.log`
- `UDP_GEOIP_DB` (环境变量): GeoIP 数据库路径，默认 `/usr/share/GeoIP/ipinfo_lite.mmdb`
- `CLOUD_KEYWORDS`: 云厂商白名单（代码内常量）

### Server 配置 (`/etc/udp-monitor/server.json` 或环境变量)
- `WEB_PORT`: Web 服务端口 (默认: 8866)
- `API_SECRET`: 与 Agent 端保持一致 (必填)
- `TG_BOT_TOKEN` / `TG_CHAT_ID`: Telegram Bot 配置 (可选)

环境变量优先级高于配置文件：
- `UDP_WEB_PORT`
- `UDP_API_SECRET`
- `UDP_WEB_HISTORY_LIMIT`
- `UDP_BATCH_MAX_CHARS`
- `UDP_TG_BOT_TOKEN`
- `UDP_TG_CHAT_ID`

## 依赖

### Server (Go)
- 构建期：Go 1.16+（仅标准库，无第三方依赖）
- 运行期：无（单个静态二进制）

### Agent (Go)
- 构建期：Go 1.19+，依赖 github.com/oschwald/maxminddb-golang（自动拉取）
- 运行期：无（单个静态二进制）；GeoIP 识别需 `ipinfo_lite.mmdb` 数据库文件

## License

Private - 仅供内部使用
