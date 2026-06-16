// 中央控制台 (Server 端 - Go 零依赖单二进制 + SSE 版)
//
// 等价替换原 Flask + flask-socketio 版本：
//   - 实时推送用浏览器原生 EventSource (SSE)，去掉了 Socket.IO 协议与 CDN 依赖
//   - 仅用 Go 标准库，go build 产物为单个静态二进制，运行时零依赖
package main

import (
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed index.html
var indexHTML string

// ==================== 配置 ====================
const configFile = "/etc/udp-monitor/server.json"

type Config struct {
	WebPort         int    `json:"WEB_PORT"`
	APISecret       string `json:"API_SECRET"`
	WebHistoryLimit int    `json:"WEB_HISTORY_LIMIT"`
	BatchMaxChars   int    `json:"BATCH_MAX_CHARS"`
	TGBotToken      string `json:"TG_BOT_TOKEN"`
	TGChatID        string `json:"TG_CHAT_ID"`
}

// 先填默认值 → 配置文件覆盖 → 环境变量覆盖
func loadConfig() Config {
	cfg := Config{
		WebPort:         8866,
		WebHistoryLimit: 2000,
		BatchMaxChars:   3800,
	}
	if data, err := os.ReadFile(configFile); err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			log.Printf("读取配置文件失败: %v", err)
		}
	}
	if v := os.Getenv("UDP_WEB_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.WebPort = n
		}
	}
	if v := os.Getenv("UDP_API_SECRET"); v != "" {
		cfg.APISecret = v
	}
	if v := os.Getenv("UDP_WEB_HISTORY_LIMIT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.WebHistoryLimit = n
		}
	}
	if v := os.Getenv("UDP_BATCH_MAX_CHARS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.BatchMaxChars = n
		}
	}
	if v := os.Getenv("UDP_TG_BOT_TOKEN"); v != "" {
		cfg.TGBotToken = v
	}
	if v := os.Getenv("UDP_TG_CHAT_ID"); v != "" {
		cfg.TGChatID = v
	}
	return cfg
}

// ==================== 全局状态 ====================
var (
	cfg     Config
	cstZone = time.FixedZone("CST", 8*3600)

	history   = []LogEntry{}
	historyMu sync.Mutex

	hub = &Hub{subs: make(map[chan string]struct{})}
)

type LogEntry struct {
	ID      string `json:"id"`
	Time    string `json:"time"`
	Content string `json:"content"`
}

// 上报数据结构
type Event struct {
	Src   string `json:"src"`
	Dst   string `json:"dst"`
	Route string `json:"route"`
	User  string `json:"user"`
	Loc   string `json:"loc"`
	Org   string `json:"org"`
	Count int    `json:"count"`
}

type UploadPayload struct {
	Secret   string  `json:"secret"`
	NodeName string  `json:"node_name"`
	NodeIP   string  `json:"node_ip"`
	Events   []Event `json:"events"`
}

// ==================== SSE 推送中心 ====================
type Hub struct {
	mu   sync.Mutex
	subs map[chan string]struct{}
}

func (h *Hub) add() chan string {
	ch := make(chan string, 16)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) remove(ch chan string) {
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

func (h *Hub) broadcast(msg string) {
	h.mu.Lock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default: // 订阅者缓冲已满则丢弃，避免阻塞整体广播
		}
	}
	h.mu.Unlock()
}

func sseFrame(event string, v interface{}) string {
	b, _ := json.Marshal(v)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", event, b)
}

// ==================== 消息构建与分发 ====================
func nowCST() string { return time.Now().In(cstZone).Format("2006-01-02 15:04:05") }

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func formatAndDispatch(p UploadPayload) {
	nodeName := html.EscapeString(orDefault(p.NodeName, "Unknown Node"))
	nodeIP := html.EscapeString(orDefault(p.NodeIP, "Unknown IP"))
	if len(p.Events) == 0 {
		return
	}

	// 按 user 分组，保留首次出现顺序
	order := []string{}
	groups := map[string][]Event{}
	for _, ev := range p.Events {
		if _, ok := groups[ev.User]; !ok {
			order = append(order, ev.User)
		}
		groups[ev.User] = append(groups[ev.User], ev)
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("<b>🚨 UDP Cloud Monitor</b>\n⏱️ <code>%s</code>\n📍 节点: %s\n", nowCST(), nodeName))

	for _, user := range order {
		sb.WriteString(fmt.Sprintf("\n👤 <b>%s</b>", html.EscapeString(user)))
		for i, ev := range groups[user] {
			routeStr := ""
			if ev.Route != "" {
				routeStr = fmt.Sprintf("<code>[%s]</code>", html.EscapeString(ev.Route))
			}
			countStr := ""
			if ev.Count > 1 {
				countStr = fmt.Sprintf("<b>(x%d)</b>", ev.Count)
			}
			line1 := fmt.Sprintf("%d. %s %s", i+1, routeStr, countStr)
			line2 := fmt.Sprintf("  🔸 %s ➔ <code>%s</code>", html.EscapeString(ev.Src), html.EscapeString(ev.Dst))
			line3 := fmt.Sprintf("  🏢 %s | %s", html.EscapeString(ev.Loc), html.EscapeString(ev.Org))
			sb.WriteString(fmt.Sprintf("\n%s\n%s\n%s\n", line1, line2, line3))
		}
	}

	dispatchMessage(sb.String(), nodeIP)
}

func dispatchMessage(content, nodeIP string) {
	webText := content
	if nodeIP != "" && nodeIP != "Unknown IP" {
		webText = strings.ReplaceAll(webText, nodeIP, "***.***.***.***")
	}

	entry := LogEntry{ID: randomID(), Time: nowCST(), Content: webText}

	historyMu.Lock()
	history = append([]LogEntry{entry}, history...)
	if len(history) > cfg.WebHistoryLimit {
		history = history[:cfg.WebHistoryLimit]
	}
	historyMu.Unlock()

	hub.broadcast(sseFrame("new_log", entry))

	// Telegram 推送原文（不脱敏）
	if cfg.TGBotToken != "" {
		sendTelegram(content)
	}
}

// Telegram sendMessage 单条上限 4096 字符；超长则按行切分成多条依次发送
var telegramAPIBase = "https://api.telegram.org"

func sendTelegram(text string) {
	limit := cfg.BatchMaxChars
	if limit < 500 || limit > 4096 {
		limit = 3800 // 留足余量，避免贴着 4096 上限
	}
	for _, chunk := range splitForTelegram(text, limit) {
		sendTelegramChunk(chunk)
	}
}

func sendTelegramChunk(text string) {
	api := fmt.Sprintf("%s/bot%s/sendMessage", telegramAPIBase, cfg.TGBotToken)
	body, _ := json.Marshal(map[string]interface{}{
		"chat_id":                  cfg.TGChatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	})
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Post(api, "application/json", strings.NewReader(string(body)))
	if err != nil {
		log.Printf("TG 发送失败: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		log.Printf("TG 返回非 200: %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
}

// 按 \n 行边界累积切分。消息里每行的 HTML 标签都在本行闭合，按行切不会拆断标签；
// limit 以字节计且 <=4096，由于 UTF-8 字节数 >= UTF-16 码元数，可保证不超过
// Telegram 按 UTF-16 计的 4096 上限。
func splitForTelegram(text string, limit int) []string {
	var chunks []string
	var b strings.Builder
	flush := func() {
		if b.Len() > 0 {
			chunks = append(chunks, b.String())
			b.Reset()
		}
	}
	for _, line := range strings.Split(text, "\n") {
		sep := 0
		if b.Len() > 0 {
			sep = 1 // 行间的 \n
		}
		if b.Len()+sep+len(line) > limit {
			flush()
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(line)
	}
	flush()
	return chunks
}

func randomID() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// ==================== HTTP 处理 ====================
func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, indexHTML)
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var p UploadPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil || p.Secret != cfg.APISecret {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"status":"error","msg":"Unauthorized"}`))
		return
	}
	go formatAndDispatch(p)
	writeJSON(w, map[string]string{"status": "success"})
}

func dataHandler(w http.ResponseWriter, r *http.Request) {
	historyMu.Lock()
	snapshot := make([]LogEntry, len(history))
	copy(snapshot, history)
	historyMu.Unlock()
	writeJSON(w, snapshot)
}

func clearHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	historyMu.Lock()
	history = []LogEntry{}
	historyMu.Unlock()
	hub.broadcast(sseFrame("history_cleared", map[string]string{}))
	writeJSON(w, map[string]string{"status": "success"})
}

func streamHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // 关掉反代缓冲，SSE 才能实时

	ch := hub.add()
	defer hub.remove(ch)

	fmt.Fprint(w, "retry: 3000\n\n") // 断线 3s 自动重连
	flusher.Flush()

	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done(): // 浏览器断开
			return
		case msg := <-ch:
			fmt.Fprint(w, msg)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprint(w, ": ping\n\n") // 注释行心跳，保活
			flusher.Flush()
		}
	}
}

func main() {
	cfg = loadConfig()
	if cfg.APISecret == "" {
		log.Fatal("[FATAL] API_SECRET 未配置，拒绝启动。请在 /etc/udp-monitor/server.json 中设置 API_SECRET，或通过 UDP_API_SECRET 环境变量注入。")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/api/upload", uploadHandler)
	mux.HandleFunc("/api/data", dataHandler)
	mux.HandleFunc("/api/clear", clearHandler)
	mux.HandleFunc("/api/stream", streamHandler)

	addr := fmt.Sprintf("0.0.0.0:%d", cfg.WebPort)
	log.Printf("[*] 中央控制台已启动，正在监听 %s", addr)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		// 不设 WriteTimeout：SSE 是长连接，写超时会切断推送
	}
	log.Fatal(srv.ListenAndServe())
}
