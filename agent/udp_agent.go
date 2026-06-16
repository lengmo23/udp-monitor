// 节点采集代理 (Node 端 - Go 版)
//
// 等价移植自原 udp_agent.py：
//   - tail Xray access.log，正则提取 UDP 流量
//   - 基于 ipinfo mmdb 识别云厂商 / 地理位置
//   - 去重批量上报到中央服务器
// 依赖：github.com/oschwald/maxminddb-golang（纯 Go，读 mmdb）。其余仅标准库。
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/maxminddb-golang"
)

// ==================== 节点配置 ====================
var (
	// 凭据/地址通过环境变量注入，避免硬编码进仓库
	centralServerURL = envOr("UDP_CENTRAL_SERVER_URL", "https://your-server.example.com/api/upload")
	apiSecret        = os.Getenv("UDP_API_SECRET")

	logFilePath = envOr("UDP_LOG_FILE", "/var/log/xray/access.log")
	lastPosFile = envOr("UDP_LAST_POS_FILE", "last_pos.txt")
	geoipDBPath = envOr("UDP_GEOIP_DB", "/usr/share/GeoIP/ipinfo_lite.mmdb")

	nodeName = "获取中..."
	nodeIP   = "获取中..."

	// 忽略列表 (DNS/内网等基础过滤)
	skipIPs = map[string]bool{
		"1.1.1.1": true, "8.8.8.8": true, "8.8.4.4": true,
		"127.0.0.1": true, "1.0.0.1": true,
	}
	// 源 IP 黑名单
	ignoreSrcIPs = map[string]bool{
		"154.3.38.65": true,
	}
	// 云厂商白名单 (已缩减)
	cloudKeywords = []string{"Amazon", "AWS", "Google", "Microsoft", "Azure"}
)

// ==================== GeoIP ====================
var geoReader *maxminddb.Reader

func initGeoIP(path string) {
	r, err := maxminddb.Open(path)
	if err != nil {
		log.Printf("⚠️ GeoIP 未加载 (%s): %v", path, err)
		return
	}
	geoReader = r
}

func recStr(rec map[string]interface{}, key string) string {
	if v, ok := rec[key].(string); ok {
		return v
	}
	return ""
}

// 标量（含数字）转字符串；map/array 返回空
func recAny(rec map[string]interface{}, key string) string {
	v, ok := rec[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case map[string]interface{}, []interface{}:
		return ""
	case bool:
		if t {
			return "true"
		}
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}

// 兼容 GeoLite2 的嵌套结构 key->names->en
func nestedNameEn(rec map[string]interface{}, key string) string {
	if m, ok := rec[key].(map[string]interface{}); ok {
		if names, ok := m["names"].(map[string]interface{}); ok {
			if en, ok := names["en"].(string); ok {
				return en
			}
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func parseGenericLocation(rec map[string]interface{}) (string, string) {
	country := firstNonEmpty(recStr(rec, "country"), recStr(rec, "country_name"))
	city := firstNonEmpty(recStr(rec, "city"), recStr(rec, "city_name"))
	if country == "" {
		country = nestedNameEn(rec, "country")
	}
	if city == "" {
		city = nestedNameEn(rec, "city")
	}
	return country, city
}

func parseGenericOrg(rec map[string]interface{}) (string, string) {
	var parts []string
	for _, key := range []string{"asn", "as_name", "as_domain", "org", "isp", "company"} {
		if v := recAny(rec, key); v != "" {
			parts = append(parts, v)
		}
	}
	if aso := recStr(rec, "autonomous_system_organization"); aso != "" {
		parts = append(parts, aso)
	}
	rawOrg := strings.Join(parts, " | ")
	displayOrg := firstNonEmpty(recStr(rec, "as_name"), recStr(rec, "company"), recStr(rec, "org"), recStr(rec, "isp"))
	asn := firstNonEmpty(recAny(rec, "asn"), recAny(rec, "autonomous_system_number"))
	if asn != "" {
		asnStr := asn
		if !strings.HasPrefix(strings.ToUpper(asnStr), "AS") {
			asnStr = "AS" + asnStr
		}
		if displayOrg != "" && !strings.Contains(displayOrg, asnStr) {
			displayOrg = displayOrg + " (" + asnStr + ")"
		} else if displayOrg == "" {
			displayOrg = asnStr
		}
	}
	if displayOrg == "" && len(parts) > 0 {
		displayOrg = parts[len(parts)-1]
	}
	return rawOrg, displayOrg
}

func getIPDetails(ip string) (locStr, rawOrg, displayOrg string) {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return "非法IP", "", ""
	}
	var country, city string
	if geoReader != nil {
		var rec map[string]interface{}
		if err := geoReader.Lookup(parsed, &rec); err == nil && rec != nil {
			country, city = parseGenericLocation(rec)
			rawOrg, displayOrg = parseGenericOrg(rec)
		}
	}
	locStr = strings.TrimSpace(country + " " + city)
	if locStr == "" {
		locStr = "Unknown"
	}
	return locStr, rawOrg, displayOrg
}

func initNodeInfo() {
	log.Println("正在自动获取节点外网 IP...")
	ip, err := httpGetText("https://api.ipify.org", 5*time.Second)
	if err != nil {
		log.Printf("⚠️ 获取节点信息失败: %v", err)
		nodeIP = "Unknown IP"
		nodeName = "Unknown Node"
		return
	}
	nodeIP = strings.TrimSpace(ip)
	loc, _, displayOrg := getIPDetails(nodeIP)
	if displayOrg != "" {
		nodeName = fmt.Sprintf("%s - %s, %s", nodeIP, loc, displayOrg)
	} else {
		nodeName = fmt.Sprintf("%s - %s", nodeIP, loc)
	}
	log.Printf("✅ 节点初始化成功: 识别身份 [%s]", nodeName)
}

func isCloudOrg(raw string) bool {
	if raw == "" {
		return false
	}
	low := strings.ToLower(raw)
	for _, kw := range cloudKeywords {
		if strings.Contains(low, strings.ToLower(kw)) {
			return true
		}
	}
	return false
}

// ==================== 采集队列（去重批量）====================
type Event struct {
	Src   string `json:"src"`
	Dst   string `json:"dst"`
	Route string `json:"route"`
	User  string `json:"user"`
	Loc   string `json:"loc"`
	Org   string `json:"org"`
	Count int    `json:"count"`
}

var (
	batchMu     sync.Mutex
	batch       []*Event
	batchIndex  = map[string]*Event{}
	lastEnqueue = time.Now()
)

func enqueueEvent(ev *Event) {
	key := ev.Dst + "|" + ev.User + "|" + ev.Route
	batchMu.Lock()
	if exist, ok := batchIndex[key]; ok {
		exist.Count++
		batchMu.Unlock()
		return
	}
	ev.Count = 1
	batch = append(batch, ev)
	batchIndex[key] = ev
	lastEnqueue = time.Now()
	full := len(batch) >= 50
	batchMu.Unlock()
	if full {
		go flushBatch()
	}
}

func flushBatch() {
	batchMu.Lock()
	if len(batch) == 0 {
		batchMu.Unlock()
		return
	}
	events := batch
	batch = nil
	batchIndex = map[string]*Event{}
	batchMu.Unlock()

	payload := map[string]interface{}{
		"secret":    apiSecret,
		"node_name": nodeName,
		"node_ip":   nodeIP,
		"events":    events,
	}
	body, _ := json.Marshal(payload)
	log.Printf("正在向中央控制台发送 %d 条监控记录...", len(events))
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Post(centralServerURL, "application/json", strings.NewReader(string(body)))
	if err != nil {
		log.Printf("网络异常: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		log.Printf("上报被拒绝: HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
}

func flusherLoop() {
	for {
		time.Sleep(1 * time.Second)
		batchMu.Lock()
		due := len(batch) > 0 && time.Since(lastEnqueue) >= 2*time.Second
		batchMu.Unlock()
		if due {
			go flushBatch()
		}
	}
}

// ==================== 日志解析 ====================
// 兼容含 from 字样的日志正则
var logPattern = regexp.MustCompile(
	`(?i)(?:from\s+)?(?:(?:tcp|udp):)?(?P<src>[\d.]+):\d+\s+accepted\s+(?P<proto>tcp|udp):(?P<dst>[\d.]+):\d+(?:\s+\[(?P<route>.*?)\])?(?:\s+email:\s*(?P<user>\S+))?`,
)

var (
	reAmazon = regexp.MustCompile(`(?i)Amazon\.com|Amazon Data Services`)
	reGoogle = regexp.MustCompile(`(?i)Google LLC`)
	reMS     = regexp.MustCompile(`(?i)Microsoft Corporation`)
)

func shortenOrg(s string) string {
	s = reAmazon.ReplaceAllString(s, "AWS")
	s = reGoogle.ReplaceAllString(s, "Google")
	s = reMS.ReplaceAllString(s, "Microsoft")
	if r := []rune(s); len(r) > 30 {
		s = string(r[:28]) + "..."
	}
	return s
}

func namedGroups(re *regexp.Regexp, match []string) map[string]string {
	res := map[string]string{}
	for i, name := range re.SubexpNames() {
		if name != "" && i < len(match) {
			res[name] = match[i]
		}
	}
	return res
}

func processLine(line string) {
	if !strings.Contains(line, "accepted") {
		return
	}
	match := logPattern.FindStringSubmatch(line)
	if match == nil {
		return
	}
	d := namedGroups(logPattern, match)
	if strings.ToLower(d["proto"]) != "udp" {
		return
	}
	src, dst := d["src"], d["dst"]
	if ignoreSrcIPs[src] {
		return
	}
	if skipIPs[dst] || skipIPs[src] {
		return
	}
	user := d["user"]
	if user == "" {
		return
	}
	if ul := strings.ToLower(user); ul == "unknown" || ul == "none" {
		return
	}

	loc, rawOrg, displayOrg := getIPDetails(dst)

	// ================== 核心判定逻辑 ==================
	isTarget := false
	if isCloudOrg(rawOrg) {
		// 1. 缩减后的云厂商白名单
		isTarget = true
	} else if strings.Contains(strings.ToLower(loc), "germany") && strings.Contains(strings.ToLower(rawOrg), "tencent") {
		// 2. 特例：德国 + 腾讯
		isTarget = true
	}
	if !isTarget {
		return
	}

	enqueueEvent(&Event{
		Src:   src,
		Dst:   dst,
		Route: d["route"],
		User:  strings.SplitN(user, "@", 2)[0],
		Loc:   loc,
		Org:   shortenOrg(displayOrg),
	})
}

// ==================== tail 主循环 ====================
func readLastPos() int64 {
	data, err := os.ReadFile(lastPosFile)
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func monitor() {
	pos := readLastPos()
	if fi, err := os.Stat(logFilePath); err == nil {
		curr := fi.Size()
		if pos == 0 && curr > 1024*1024 {
			pos = curr // 首次启动且日志很大：从尾部开始
		}
		if pos > curr {
			pos = 0
		}
	}

	initNodeInfo()
	log.Printf("Node Agent 已启动，准备上报至: %s", centralServerURL)
	go flusherLoop()

	for {
		fi, err := os.Stat(logFilePath)
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}
		curr := fi.Size()
		if pos > curr { // 日志被轮转/截断
			pos = 0
		}
		if pos == curr {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		f, err := os.Open(logFilePath)
		if err != nil {
			time.Sleep(1 * time.Second)
			continue
		}
		if _, err := f.Seek(pos, io.SeekStart); err != nil {
			f.Close()
			time.Sleep(1 * time.Second)
			continue
		}
		reader := bufio.NewReader(f)
		var consumed int64
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				break // 末尾不完整的行留到下一轮再读
			}
			consumed += int64(len(line))
			processLine(strings.TrimRight(line, "\r\n"))
		}
		f.Close()
		pos += consumed
		_ = os.WriteFile(lastPosFile, []byte(strconv.FormatInt(pos, 10)), 0644)
	}
}

// ==================== 工具 ====================
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func httpGetText(url string, timeout time.Duration) (string, error) {
	client := &http.Client{Timeout: timeout}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1024))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func main() {
	initGeoIP(geoipDBPath)
	if apiSecret == "" {
		log.Println("⚠️ 未设置 UDP_API_SECRET 环境变量，上报将被服务端拒绝 (401)")
	}
	monitor()
}
