package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/bits"
	"math/rand"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// WebmailSession 保存通过扫码获得的网页版 Session 与 Cookie
type WebmailSession struct {
	Uin        string            `json:"uin"`
	Sid        string            `json:"sid"`
	Cookies    map[string]string `json:"cookies"`
	ExpiresAt  int64             `json:"expires_at"`
	IsLoggedIn bool              `json:"is_logged_in"`
}

// WebmailClient 负责对接新版 QQ 邮箱网页版 (wx.mail.qq.com / ptlogin2 / 微信扫码)
type WebmailClient struct {
	client       *http.Client
	jar          *cookiejar.Jar
	session      WebmailSession
	invalidSid   string
	qmdFolderMap map[string]string
	isChecking   bool
	mu           sync.RWMutex
}

var GlobalWebmailClient *WebmailClient

func init() {
	jar, _ := cookiejar.New(nil)
	tr := &http.Transport{
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
		DisableKeepAlives:   false,
		MaxIdleConnsPerHost: 30,
	}
	GlobalWebmailClient = &WebmailClient{
		jar: jar,
		client: &http.Client{
			Transport: tr,
			Jar:       jar,
			Timeout:   0, // 设为 0：不设置客户端死限，保障 50MB 大文件分片流式下载与上传满速传输
		},
		qmdFolderMap: make(map[string]string),
		isChecking:   false,
	}
}

func (w *WebmailClient) SetChecking(checking bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.isChecking = checking
}

func (w *WebmailClient) IsChecking() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.isChecking
}

// hash33 腾讯 ptlogin2 标准 qrsig 签名算法
func hash33(str string) int64 {
	var hash int64 = 0
	for i := 0; i < len(str); i++ {
		hash += (hash << 5) + int64(str[i])
	}
	return 2147483647 & hash
}

// QRLoginContext 扫码会话上下文 (同时支持手机 QQ 与 微信)
type QRLoginContext struct {
	QRSig     string `json:"qrsig"`
	UUID      string `json:"uuid"`
	QRCodeB64 string `json:"qr_code_b64"`
	CreatedAt int64  `json:"created_at"`
	Type      string `json:"type"` // "qq" 或 "wx"
}

// GetQQLoginQRCode 拉取手机 QQ 官方邮箱专用扫码二维码 (appid=522005705, daid=4)
func (w *WebmailClient) GetQQLoginQRCode() (*QRLoginContext, error) {
	randVal := fmt.Sprintf("%.16f", rand.Float64())
	qrURL := fmt.Sprintf("https://ssl.ptlogin2.qq.com/ptqrshow?appid=522005705&e=2&l=M&s=3&d=72&v=4&t=%s&daid=4", randVal)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", qrURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://xui.ptlogin2.qq.com/")

	resp, err := w.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取 QQ 二维码失败: %w", err)
	}
	defer resp.Body.Close()

	var qrsig string
	for _, cookie := range resp.Cookies() {
		if cookie.Name == "qrsig" {
			qrsig = cookie.Value
			break
		}
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("读取 QQ 二维码失败: %w", err)
	}

	b64Image := "data:image/png;base64," + base64.StdEncoding.EncodeToString(bodyBytes)

	resCtx := &QRLoginContext{
		QRSig:     qrsig,
		QRCodeB64: b64Image,
		CreatedAt: time.Now().Unix(),
		Type:      "qq",
	}

	return resCtx, nil
}

// GetWebmailUserInfo 获取当前 sid 对应的 QQ 邮箱账号与 UIN (用于防乱扫串号校验)
func (w *WebmailClient) GetWebmailUserInfo(sid string) (email string, uin int64, err error) {
	cleanSid := strings.TrimSpace(sid)
	if cleanSid == "" || cleanSid == "getUrlParam" || len(cleanSid) < 15 {
		return "", 0, fmt.Errorf("无效 sid")
	}

	homeURL := "https://wx.mail.qq.com/home/home"
	formData := url.Values{}
	formData.Set("language", "zh")
	formData.Set("r", fmt.Sprintf("%d", time.Now().UnixNano()/1e6))
	formData.Set("sid", cleanSid)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", homeURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://wx.mail.qq.com/")
	req.Header.Set("Origin", "https://wx.mail.qq.com")
	req.Header.Set("Cookie", fmt.Sprintf("xm_sid=%s; sid=%s; qm_sid=%s", cleanSid, cleanSid, cleanSid))

	resp, err := w.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	var homeResp struct {
		Head struct {
			Ret int    `json:"ret"`
			Msg string `json:"msg"`
		} `json:"head"`
		Body struct {
			Email string      `json:"email"`
			Uin   interface{} `json:"uin"`
		} `json:"body"`
	}

	if err := json.Unmarshal(bodyBytes, &homeResp); err != nil {
		return "", 0, err
	}
	if homeResp.Head.Ret != 0 {
		return "", 0, fmt.Errorf("腾讯返回: ret=%d, msg=%s", homeResp.Head.Ret, homeResp.Head.Msg)
	}

	var parsedUIN int64
	switch v := homeResp.Body.Uin.(type) {
	case float64:
		parsedUIN = int64(v)
	case string:
		parsedUIN, _ = strconv.ParseInt(v, 10, 64)
	}

	return strings.TrimSpace(homeResp.Body.Email), parsedUIN, nil
}

// CheckSidActive 主动向腾讯官方 /list/folderlist 进行毫秒级保活探活 (真实 CGI 探活，100% 权威可靠)
func (w *WebmailClient) CheckSidActive(sid string) bool {
	cleanSid := strings.TrimSpace(sid)
	if cleanSid == "" || cleanSid == "getUrlParam" || len(cleanSid) < 15 {
		return false
	}

	folderURL := "https://wx.mail.qq.com/list/folderlist"
	formData := url.Values{}
	formData.Set("language", "zh")
	formData.Set("r", fmt.Sprintf("%d", time.Now().UnixNano()/1e6))
	formData.Set("sid", cleanSid)

	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", folderURL, strings.NewReader(formData.Encode()))
	if err != nil {
		w.LogError("❌ [SID探活] 构造请求失败: %v", err)
		return false
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://wx.mail.qq.com/")
	req.Header.Set("Origin", "https://wx.mail.qq.com")
	req.Header.Set("Cookie", fmt.Sprintf("xm_sid=%s; sid=%s; qm_sid=%s", cleanSid, cleanSid, cleanSid))

	resp, err := w.client.Do(req)
	if err != nil {
		w.LogError("❌ [SID探活] 请求网络失败: %v", err)
		return false
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		w.LogError("❌ [SID探活] 腾讯官方返回 HTTP %d", resp.StatusCode)
		return false
	}

	bodyBytes, _ := io.ReadAll(resp.Body)
	var res struct {
		Head struct {
			Ret int `json:"ret"`
		} `json:"head"`
	}
	if err := json.Unmarshal(bodyBytes, &res); err != nil {
		preview := string(bodyBytes)
		if len(preview) > 120 {
			preview = preview[:120]
		}
		w.LogError("❌ [SID探活] 解析响应失败 (HTTP %d): %s", resp.StatusCode, preview)
		return false
	}
	if res.Head.Ret != 0 {
		w.LogError("❌ [SID探活] 腾讯官方返回 Ret=%d (sid 可能已过期)", res.Head.Ret)
		return false
	}
	w.Log("✔ [SID探活通过] 当前 sid 有效就绪")
	return true
}

// CheckQRLoginStatus 检查手机 QQ 扫码认证状态
func (w *WebmailClient) CheckQRLoginStatus(qrsig string) (code int, sid string, tipMsg string, err error) {
	defer func() {
		if r := recover(); r != nil {
			code = -1
			tipMsg = fmt.Sprintf("QQ 扫码处理异常: %v", r)
			err = fmt.Errorf("panic: %v", r)
		}
	}()

	if qrsig == "" {
		return -1, "", "qrsig 为空", fmt.Errorf("qrsig 为空")
	}

	ptbkn := hash33(qrsig)
	u1 := "https%3A%2F%2Fmail.qq.com%2Fcgi-bin%2Flogin%3Fvt%3Dpassport%26vm%3Dwpt%26ft%3Dloginpage%26target%3D"
	pollURL := fmt.Sprintf("https://ssl.ptlogin2.qq.com/ptqrlogin?u1=%s&ptqrtoken=%d&ptredirect=0&h=1&t=1&g=1&from_ui=1&ptlang=2052&action=0-0-%d&js_ver=240321&js_type=1&login_sig=&pt_uistyle=40&aid=522005705&daid=4&has_onekey=1", u1, ptbkn, time.Now().UnixNano()/1e6)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", pollURL, nil)
	if err != nil {
		return -1, "", "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://xui.ptlogin2.qq.com/")
	req.Header.Set("Cookie", fmt.Sprintf("qrsig=%s", qrsig))

	resp, err := w.client.Do(req)
	if err != nil {
		return -1, "", "", err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	respStr := string(bodyBytes)

	re := regexp.MustCompile(`ptuiCB\('(\d+)',\s*'(\d+)',\s*'([^']*)',\s*'([^']*)',\s*'([^']*)'`)
	matches := re.FindStringSubmatch(respStr)
	if len(matches) < 6 {
		return -1, "", respStr, fmt.Errorf("无法解析腾讯登录响应: %s", respStr)
	}

	code, _ = strconv.Atoi(matches[1])
	jumpURL := matches[3]
	tipMsg = matches[5]

	if code == 0 {
		sid, err = w.exchangeForMailSession(jumpURL, resp.Cookies())
		if err != nil {
			return 0, "", fmt.Sprintf("扫码成功，但在换取邮箱会话时失败: %v", err), err
		}

		// 防乱扫串号强校验：核对扫码账号与当前网盘配置账号是否一致 (未通过核验严禁入库)
		scannedEmail, _, userErr := w.GetWebmailUserInfo(sid)
		if userErr != nil || scannedEmail == "" {
			errMsg := fmt.Sprintf("❌ [安全校验失败] 无法核实扫码账号归属 (%v)，已终止绑定，请重新扫码！", userErr)
			w.LogError(errMsg)
			w.ClearSession()
			return -1, "", errMsg, fmt.Errorf("%s", errMsg)
		}

		targetEmail := strings.ToLower(strings.TrimSpace(GlobalConfig.Email))
		actualEmail := strings.ToLower(strings.TrimSpace(scannedEmail))
		if targetEmail != "" && !strings.EqualFold(targetEmail, actualEmail) {
			errMsg := fmt.Sprintf("❌ [安全拦截] 扫码的 QQ 账号 (%s) 与当前网盘配置账号 (%s) 不一致！已自动终止授权，防止串号！请换用当前账号对应的手机 QQ 扫码。", scannedEmail, GlobalConfig.Email)
			w.LogError(errMsg)
			w.ClearSession()
			return -1, "", errMsg, fmt.Errorf("%s", errMsg)
		}

		w.SetManualSession(sid)
		return 0, sid, "手机 QQ 扫码成功，账号校验一致，已就绪云端秒发！", nil
	}

	return code, "", tipMsg, nil
}

// exchangeForMailSession 跟随授权跳转并换取 wx.mail.qq.com 的 sid 与 Cookie
func (w *WebmailClient) exchangeForMailSession(jumpURL string, loginCookies []*http.Cookie) (string, error) {
	if jumpURL == "" {
		return "", fmt.Errorf("跳转 URL 为空")
	}

	// 1. 请求跳转 URL 获取授权票据与 Cookie
	req, err := http.NewRequest("GET", jumpURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	for _, c := range loginCookies {
		req.AddCookie(c)
	}

	resp, err := w.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求登录跳转链失败: %w", err)
	}
	defer resp.Body.Close()

	// 优先从 jumpURL 重定向落地 URL 中解析 sid
	sid := ""
	jumpFinalURL := resp.Request.URL.String()
	if u, err := url.Parse(jumpFinalURL); err == nil {
		sid = u.Query().Get("sid")
	}

	// 从跳转响应 Cookies 中提取
	if sid == "" {
		for _, c := range resp.Cookies() {
			if c.Name == "sid" || c.Name == "qm_sid" || c.Name == "xm_sid" {
				sid = c.Value
				break
			}
		}
	}

	// 2. 请求进入 QQ 邮箱 Web 端换取 sid
	mailHomeURL := "https://wx.mail.qq.com/home/index"
	mReq, _ := http.NewRequest("GET", mailHomeURL, nil)
	mReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	mReq.Header.Set("Referer", "https://mail.qq.com/")

	mResp, err := w.client.Do(mReq)
	if err == nil && mResp != nil {
		defer mResp.Body.Close()
		finalURL := mResp.Request.URL.String()
		if sid == "" {
			if u, err := url.Parse(finalURL); err == nil {
				sid = u.Query().Get("sid")
			}
		}
		if sid == "" {
			for _, c := range mResp.Cookies() {
				if c.Name == "sid" || c.Name == "qm_sid" || c.Name == "xm_sid" {
					sid = c.Value
					break
				}
			}
		}
		if (sid == "" || sid == "getUrlParam" || len(sid) < 15) && mResp.Body != nil {
			body, _ := io.ReadAll(mResp.Body)
			sidRe := regexp.MustCompile(`[?&"\']sid=([a-zA-Z0-9_\-]{16,40})`)
			if sm := sidRe.FindStringSubmatch(string(body)); len(sm) > 1 {
				if sm[1] != "getUrlParam" {
					sid = sm[1]
				}
			}
		}
	}

	// 从 jar 中跨域提取
	if sid == "" && w.jar != nil {
		for _, domain := range []string{"https://wx.mail.qq.com", "https://mail.qq.com", "https://qq.com"} {
			u, _ := url.Parse(domain)
			for _, c := range w.jar.Cookies(u) {
				if (c.Name == "sid" || c.Name == "qm_sid" || c.Name == "xm_sid") && c.Value != "" && c.Value != "getUrlParam" {
					sid = c.Value
					break
				}
			}
			if sid != "" {
				break
			}
		}
	}

	if sid == "" || sid == "getUrlParam" || len(sid) < 15 {
		return "", fmt.Errorf("未能自动提取到真实有效 sid (捕获为: %s)，请直接在已登录网页版的浏览器地址栏复制 sid (形如 zV9xRYwuYnYu1mxqAB9OMAAA)", sid)
	}
	w.mu.Lock()
	w.invalidSid = ""
	w.session = WebmailSession{
		Sid:        sid,
		ExpiresAt:  time.Now().Add(30 * 24 * time.Hour).Unix(),
		IsLoggedIn: true,
	}
	w.mu.Unlock()

	return sid, nil
}

// ClearSession 仅重置当前内存中的 Webmail 登录态 (严格保留各账号在云端独立保存的 sid，本地零文件残留，下次切换回来免扫码)
func (w *WebmailClient) ClearSession() {
	w.mu.Lock()
	if w.session.Sid != "" {
		w.invalidSid = w.session.Sid
	}
	w.session = WebmailSession{}
	if w.jar != nil {
		if u, err := url.Parse("https://wx.mail.qq.com"); err == nil {
			w.jar.SetCookies(u, []*http.Cookie{})
		}
	}
	w.mu.Unlock()
}

func (w *WebmailClient) GetInvalidSid() string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.invalidSid
}

// RestoreSession 从云端恢复已有的会话令牌到内存，恢复前先向腾讯官方发起真实探活，有效才登入
func (w *WebmailClient) RestoreSession(sid string) bool {
	w.SetChecking(true)
	defer w.SetChecking(false)

	cleanSid := strings.TrimSpace(sid)
	if cleanSid == "" || cleanSid == "getUrlParam" || len(cleanSid) < 15 {
		return false
	}

	w.mu.RLock()
	isInv := (cleanSid == w.invalidSid)
	w.mu.RUnlock()
	if isInv {
		return false
	}

	// 权威探活：直接向腾讯官方目录 CGI 发起探活
	if !w.CheckSidActive(cleanSid) {
		w.mu.Lock()
		w.invalidSid = cleanSid
		w.session = WebmailSession{}
		if w.jar != nil {
			if u, err := url.Parse("https://wx.mail.qq.com"); err == nil {
				w.jar.SetCookies(u, []*http.Cookie{})
			}
		}
		w.mu.Unlock()

		if GlobalEngine != nil {
			GlobalEngine.LogError("WEBMAIL", fmt.Sprintf("⚠️ [云端会话已失效] 云端 Webmail sid (%s) 已在腾讯官方过期，自动清理云端过期凭证...", cleanSid))
			go GlobalEngine.DeleteCloudSession()
		}
		return false
	}

	w.mu.Lock()
	w.invalidSid = ""
	w.session = WebmailSession{
		Sid:        cleanSid,
		ExpiresAt:  time.Now().Add(30 * 24 * time.Hour).Unix(),
		IsLoggedIn: true,
	}
	if w.jar != nil {
		u, _ := url.Parse("https://wx.mail.qq.com")
		w.jar.SetCookies(u, []*http.Cookie{
			{Name: "xm_sid", Value: cleanSid, Path: "/", Domain: "wx.mail.qq.com"},
			{Name: "sid", Value: cleanSid, Path: "/", Domain: "wx.mail.qq.com"},
			{Name: "qm_sid", Value: cleanSid, Path: "/", Domain: "wx.mail.qq.com"},
		})
	}
	w.mu.Unlock()

	if GlobalEngine != nil {
		GlobalEngine.LogInfo("WEBMAIL", fmt.Sprintf("✔ [云端会话恢复成功] 成功激活 Webmail sid: %s (探活通过，免扫码极速通道已就绪)", cleanSid))
	}
	return true
}

// SetManualSession 支持用户手动录入或从前端传入 sid，纯内存与云端同步，本地零文件留存
func (w *WebmailClient) SetManualSession(sid string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	cleanSid := strings.TrimSpace(sid)
	if cleanSid == "getUrlParam" || len(cleanSid) < 15 {
		if GlobalEngine != nil {
			GlobalEngine.LogError("WEBMAIL", fmt.Sprintf("❌ [拦截无效sid] 拒绝设置无效 sid: [%s]！sid 必须是从浏览器地址栏复制的真实 24 位令牌 (如 zV9xRYwuYnYu1mxqAB9OMAAA)", cleanSid))
		}
		return
	}
	w.invalidSid = ""
	w.session = WebmailSession{
		Sid:        cleanSid,
		ExpiresAt:  time.Now().Add(30 * 24 * time.Hour).Unix(),
		IsLoggedIn: true,
	}
	if w.jar != nil {
		u, _ := url.Parse("https://wx.mail.qq.com")
		w.jar.SetCookies(u, []*http.Cookie{
			{Name: "xm_sid", Value: cleanSid, Path: "/", Domain: "wx.mail.qq.com"},
			{Name: "sid", Value: cleanSid, Path: "/", Domain: "wx.mail.qq.com"},
			{Name: "qm_sid", Value: cleanSid, Path: "/", Domain: "wx.mail.qq.com"},
		})
	}
	if GlobalEngine != nil {
		go GlobalEngine.SaveCloudSession(cleanSid)
		GlobalEngine.LogInfo("WEBMAIL", fmt.Sprintf("✔ [Webmail sid 绑定成功] 成功锁定云端 sid: %s (极速秒转已就绪，已同步至「收到的文件」云端永久漫游)", cleanSid))
	}
}

// IsSessionValid 判断当前是否有可用的 Web 登录态
func (w *WebmailClient) IsSessionValid() bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.session.IsLoggedIn && w.session.Sid != "" && w.session.Sid != "getUrlParam" && len(w.session.Sid) >= 15 && time.Now().Unix() < w.session.ExpiresAt
}

// GetSession 获取当前会话信息
// Log 输出标准结构化诊断日志至终端控制台与系统日志面板
func (w *WebmailClient) Log(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if GlobalEngine != nil {
		GlobalEngine.LogInfo("WEBMAIL", msg)
	} else {
		fmt.Printf("[INFO] [WEBMAIL] %s\n", msg)
	}
}

// LogWarn 输出警告级诊断日志
func (w *WebmailClient) LogWarn(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if GlobalEngine != nil {
		GlobalEngine.LogWarn("WEBMAIL", msg)
	} else {
		fmt.Printf("[WARN] [WEBMAIL] %s\n", msg)
	}
}

// LogError 输出错误级诊断日志
func (w *WebmailClient) LogError(format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	if GlobalEngine != nil {
		GlobalEngine.LogError("WEBMAIL", msg)
	} else {
		fmt.Printf("[ERROR] [WEBMAIL] %s\n", msg)
	}
}

func (w *WebmailClient) GetSession() WebmailSession {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.session
}

// FindDirIDByFolderName 根据隔离空间文件夹名称全量递归反查腾讯 Web 端的 dirid (严格对齐抓包 POST 协议)
func (w *WebmailClient) FindDirIDByFolderName(folderName string) (string, error) {
	w.mu.RLock()
	sid := w.session.Sid
	w.mu.RUnlock()

	if sid == "" || sid == "getUrlParam" || len(sid) < 15 {
		w.LogError("❌ [sid无效拦截] 当前 sid 为无效值: [%s] (登录态失效)", sid)
		return "", fmt.Errorf("Webmail sid 无效 (当前为: %s)。请在浏览器打开 QQ 邮箱网页版，从顶部地址栏复制 24 位真实 sid (形如 zV9xRYwuYnYu1mxqAB9OMAAA)并在弹窗中手动填入", sid)
	}

	cleanTarget := strings.ToLower(ToDisplayFolder(folderName))
	canonicalTarget := strings.ToLower(ToCanonicalFolder(folderName))

	folderURL := "https://wx.mail.qq.com/list/folderlist"
	formData := url.Values{}
	formData.Set("language", "zh")
	formData.Set("r", fmt.Sprintf("%d", time.Now().UnixNano()/1e6))
	formData.Set("sid", sid)

	w.Log("🔍 [目录检索] 正在向云端 POST 文件夹列表: %s (匹配目标: 纯净名[%s] / 规范名[%s])...", folderURL, cleanTarget, canonicalTarget)

	req, err := http.NewRequest("POST", folderURL, strings.NewReader(formData.Encode()))
	if err != nil {
		w.LogError("❌ [目录检索] 构造 HTTP 失败: %v", err)
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://wx.mail.qq.com/")
	req.Header.Set("Origin", "https://wx.mail.qq.com")
	req.Header.Set("Cookie", fmt.Sprintf("xm_sid=%s; sid=%s; qm_sid=%s", sid, sid, sid))

	resp, err := w.client.Do(req)
	if err != nil {
		w.LogError("❌ [目录检索] 发送 HTTP 失败: %v", err)
		return "", err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	w.Log("📥 [目录响应] HTTP %d, 大小: %d 字节, 当前使用的 sid: [%s]", resp.StatusCode, len(bodyBytes), sid)
	w.Log("📜 [云端返回原文] %s", string(bodyBytes))

	if resp.StatusCode != 200 {
		w.LogError("❌ [目录响应异常] HTTP 状态码 %d, 响应片段: %s", resp.StatusCode, string(bodyBytes[:minInt(200, len(bodyBytes))]))
		return "", fmt.Errorf("HTTP 状态码异常: %d", resp.StatusCode)
	}

	type FolderEntry struct {
		ID   string
		Name string
	}
	var discoveredFolders []FolderEntry

	var rawData interface{}
	if err := json.Unmarshal(bodyBytes, &rawData); err == nil {
		var collectDirs func(v interface{})
		collectDirs = func(v interface{}) {
		switch node := v.(type) {
		case map[string]interface{}:
			nameVal, _ := node["name"].(string)
			var dirID string
			for _, k := range []string{"dirid", "dir", "id", "folderid"} {
				if d, ok := node[k]; ok {
					dirID = fmt.Sprintf("%v", d)
					break
				}
			}
			if nameVal != "" && dirID != "" {
				discoveredFolders = append(discoveredFolders, FolderEntry{
					ID:   dirID,
					Name: decodeModifiedUTF7(nameVal),
				})
			}
			for _, val := range node {
				collectDirs(val)
			}
		case []interface{}:
			for _, item := range node {
				collectDirs(item)
			}
		}
		}
		collectDirs(rawData)
	} else {
		w.LogError("❌ [目录JSON解析失败] 无法反序列化: %v, 原文片段: %s", err, string(bodyBytes[:minInt(300, len(bodyBytes))]))
	}

	var folderNames []string
	var matchedDirID string
	newQmdMap := make(map[string]string)

	for _, f := range discoveredFolders {
		folderNames = append(folderNames, fmt.Sprintf("%s(dirid=%s)", f.Name, f.ID))
		rawLower := strings.ToLower(f.Name)
		dispLower := strings.ToLower(ToDisplayFolder(f.Name))
		if strings.HasPrefix(strings.ToUpper(f.Name), "QMD_") {
			newQmdMap[f.Name] = f.ID
		}
		if matchedDirID == "" {
			if dispLower == cleanTarget || rawLower == cleanTarget || rawLower == canonicalTarget ||
				strings.Contains(rawLower, canonicalTarget) || strings.Contains(dispLower, cleanTarget) ||
				strings.EqualFold(dispLower, cleanTarget) {
				matchedDirID = f.ID
			}
		}
	}

	w.mu.Lock()
	w.qmdFolderMap = newQmdMap
	w.mu.Unlock()

	w.Log("📂 [云端目录清单] 共发现 %d 个文件夹: [%s]", len(discoveredFolders), strings.Join(folderNames, ", "))

	if matchedDirID != "" {
		w.Log("✔ [目录锁定成功] 目标空间 [%s] 成功对应云端 dirid: %s", folderName, matchedDirID)
		return matchedDirID, nil
	}

	w.LogError("❌ [目录未命中] 云端未找到与 [%s] 匹配的文件夹 (目标纯净名: %s, 规范名: %s)", folderName, cleanTarget, canonicalTarget)
	return "", fmt.Errorf("未能在线检索到目录 [%s] 的 dirid (请核对云端目录清单)", folderName)
}

// GetAllQMDFolderDirIDs 获取云端发现的所有 QMD_ 隔离空间目录映射 (用于跨空间智能分片寻址)
func (w *WebmailClient) GetAllQMDFolderDirIDs() map[string]string {
	w.mu.RLock()
	defer w.mu.RUnlock()
	res := make(map[string]string)
	for k, v := range w.qmdFolderMap {
		res[k] = v
	}
	return res
}

// WebmailMailItem 邮件列表项
type WebmailMailItem struct {
	MailID       string `json:"mailid"`
	Subject      string `json:"subject"`
	AttachName   string `json:"attach_name"`
	AttachFileID string `json:"attach_fileid"`
}

// GetMailAttachmentInfo 抓包实测专用：通过 /read/readmail 精准获取邮件真实附件 fileid 与名称
func (w *WebmailClient) GetMailAttachmentInfo(mailID string) (attachName, fileID string, err error) {
	w.mu.RLock()
	sid := w.session.Sid
	w.mu.RUnlock()

	readURL := "https://wx.mail.qq.com/read/readmail"
	formData := url.Values{}
	formData.Set("mailid", mailID)
	formData.Set("func", "1")
	formData.Set("language", "zh")
	formData.Set("r", fmt.Sprintf("%d", time.Now().UnixNano()/1e6))
	formData.Set("sid", sid)

	req, err := http.NewRequest("POST", readURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://wx.mail.qq.com/")
	req.Header.Set("Origin", "https://wx.mail.qq.com")
	req.Header.Set("Cookie", fmt.Sprintf("xm_sid=%s; sid=%s; qm_sid=%s", sid, sid, sid))

	resp, err := w.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	bodyBytes, _ := io.ReadAll(resp.Body)
	var readResp struct {
		Head struct {
			Ret int    `json:"ret"`
			Msg string `json:"msg"`
		} `json:"head"`
		Body struct {
			Item struct {
				NormalAttach []struct {
					FileID string `json:"fileid"`
					Name   string `json:"name"`
				} `json:"normal_attach"`
			} `json:"item"`
		} `json:"body"`
	}

	if err := json.Unmarshal(bodyBytes, &readResp); err != nil {
		return "", "", err
	}
	if readResp.Head.Ret != 0 {
		return "", "", fmt.Errorf("读取邮件详情失败: ret=%d, msg=%s", readResp.Head.Ret, readResp.Head.Msg)
	}

	if len(readResp.Body.Item.NormalAttach) > 0 {
		return readResp.Body.Item.NormalAttach[0].Name, readResp.Body.Item.NormalAttach[0].FileID, nil
	}
	return "", "", fmt.Errorf("邮件中未发现 normal_attach 普通附件")
}

// DownloadAttachmentBytesWithProgress 通过 Webmail HTTP 原生流式通道高速拉取切片附件 (对齐 HAR 抓包协议)
func (w *WebmailClient) DownloadAttachmentBytesWithProgress(mailID, fileID, attachName string, progressCb func(int) bool) ([]byte, error) {
	w.mu.RLock()
	sid := w.session.Sid
	w.mu.RUnlock()

	if sid == "" {
		return nil, fmt.Errorf("Webmail 会话已失效，请重新扫码")
	}

	// 若未显式传入 fileID，自动调用 readmail 补齐
	if fileID == "" {
		realName, realFID, err := w.GetMailAttachmentInfo(mailID)
		if err != nil {
			return nil, fmt.Errorf("获取邮件附件 fileid 失败: %w", err)
		}
		fileID = realFID
		if attachName == "" {
			attachName = realName
		}
	}

	downURL := fmt.Sprintf("https://wx.mail.qq.com/attach/download?mailid=%s&fileid=%s&name=%s&sid=%s",
		url.QueryEscape(mailID),
		url.QueryEscape(fileID),
		url.QueryEscape(attachName),
		url.QueryEscape(sid),
	)

	req, err := http.NewRequest("GET", downURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://wx.mail.qq.com/")
	req.Header.Set("Cookie", fmt.Sprintf("xm_sid=%s; sid=%s; qm_sid=%s", sid, sid, sid))

	resp, err := w.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP 附件请求失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP 下载返回异常状态码: %d", resp.StatusCode)
	}

	// 快速探测是否返回了 JSON 错误（如 {"head":{"ret":-20002,"msg":"登录态失效"...}}）
	peekBuf := make([]byte, 512)
	nPeek, pErr := io.ReadFull(resp.Body, peekBuf)
	if pErr != nil && pErr != io.EOF && pErr != io.ErrUnexpectedEOF {
		return nil, fmt.Errorf("读取 HTTP 响应流失败: %w", pErr)
	}
	validPeek := peekBuf[:nPeek]
	if bytes.HasPrefix(validPeek, []byte(`{"head":{"ret":`)) {
		var errResp struct {
			Head struct {
				Ret int    `json:"ret"`
				Msg string `json:"msg"`
			} `json:"head"`
		}
		if json.Unmarshal(validPeek, &errResp) == nil && errResp.Head.Ret != 0 {
			if errResp.Head.Ret == -20002 {
				w.ClearSession()
			}
			return nil, fmt.Errorf("腾讯返回错误 (ret=%d): %s", errResp.Head.Ret, errResp.Head.Msg)
		}
	}

	// 流式读取完整二进制数据
	var buf bytes.Buffer
	if resp.ContentLength > 0 {
		buf.Grow(int(resp.ContentLength))
	}
	buf.Write(validPeek)
	if progressCb != nil && nPeek > 0 {
		if !progressCb(nPeek) {
			return nil, fmt.Errorf("传输已被用户中断")
		}
	}

	chunkBuf := make([]byte, 128*1024)
	for {
		n, rErr := resp.Body.Read(chunkBuf)
		if n > 0 {
			buf.Write(chunkBuf[:n])
			if progressCb != nil {
				if !progressCb(n) {
					return nil, fmt.Errorf("传输已被用户中断")
				}
			}
		}
		if rErr != nil {
			if rErr == io.EOF {
				break
			}
			return nil, fmt.Errorf("HTTP 下载流中断: %w", rErr)
		}
	}

	return buf.Bytes(), nil
}

// LocateWebmailChunkMap 检索目标隔离空间中匹配该文件的所有切片邮件，构建 [partIdx -> WebmailMailItem] 映射
func (w *WebmailClient) LocateWebmailChunkMap(folderName, hash12, fileID string) (map[int]WebmailMailItem, error) {
	dirID, err := w.FindDirIDByFolderName(folderName)
	if err != nil {
		return nil, err
	}

	mailList, err := w.FetchFolderMailList(dirID)
	if err != nil {
		return nil, err
	}

	res := make(map[int]WebmailMailItem)
	for _, m := range mailList {
		if !strings.HasPrefix(m.Subject, "[QMD:DATA]|") && !strings.HasPrefix(m.Subject, "[QMD:SHARE:DATA]|") {
			continue
		}

		isTarget := false
		if hash12 != "" && strings.Contains(m.Subject, hash12) {
			isTarget = true
		} else if fileID != "" && strings.Contains(m.Subject, "|"+fileID+"|") {
			isTarget = true
		}

		if !isTarget {
			continue
		}

		// 从主题提取分片序号 (格式如 "2-10" 或 "2/10")
		parts := strings.Split(m.Subject, "|")
		for _, p := range parts {
			if strings.Contains(p, "-") {
				var pIdx, tot int
				if n, _ := fmt.Sscanf(p, "%d-%d", &pIdx, &tot); n >= 1 && pIdx >= 1 {
					res[pIdx] = m
					break
				}
			} else if strings.Contains(p, "/") {
				var pIdx, tot int
				if n, _ := fmt.Sscanf(p, "%d/%d", &pIdx, &tot); n >= 1 && pIdx >= 1 {
					res[pIdx] = m
					break
				}
			}
		}
	}

	return res, nil
}

// FetchFolderMailList 根据隔离空间 dirid 拉取分片邮件列表 (对齐抓包 POST 协议)
func (w *WebmailClient) FetchFolderMailList(dirID string) ([]WebmailMailItem, error) {
	w.mu.RLock()
	sid := w.session.Sid
	w.mu.RUnlock()

	if sid == "" {
		return nil, fmt.Errorf("Web 会话已失效，请重新扫码")
	}

	listURL := "https://wx.mail.qq.com/list/maillist"
	w.Log("🔍 [切片检索] 正在从 dirid=%s 拉取云端分片邮件列表...", dirID)

	var results []WebmailMailItem
	seenMailID := make(map[string]bool)
	const pageSize = 100

	for pageNow := 0; pageNow < 1000; pageNow++ {
		formData := url.Values{}
		formData.Set("dir", dirID)
		formData.Set("dirid", dirID)
		formData.Set("page_now", fmt.Sprintf("%d", pageNow))
		formData.Set("page_size", fmt.Sprintf("%d", pageSize))
		formData.Set("enable_topmail", "true")
		formData.Set("func", "1")
		formData.Set("sort_type", "1")
		formData.Set("sort_direction", "1")
		formData.Set("language", "zh")
		formData.Set("r", fmt.Sprintf("%d", time.Now().UnixNano()/1e6))
		formData.Set("sid", sid)

		req, err := http.NewRequest("POST", listURL, strings.NewReader(formData.Encode()))
		if err != nil {
			w.LogError("❌ [切片检索] 构造 HTTP 失败: %v", err)
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		req.Header.Set("Referer", "https://wx.mail.qq.com/")
		req.Header.Set("Origin", "https://wx.mail.qq.com")
		req.Header.Set("Cookie", fmt.Sprintf("xm_sid=%s; sid=%s; qm_sid=%s", sid, sid, sid))

		resp, err := w.client.Do(req)
		if err != nil {
			w.LogError("❌ [切片检索网络失败] %v", err)
			return nil, fmt.Errorf("读取云端邮件列表失败: %w", err)
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}

		var listResp struct {
			Head struct {
				Ret int    `json:"ret"`
				Msg string `json:"msg"`
			} `json:"head"`
			Body struct {
				List []struct {
					EmailID string `json:"emailid"`
					Subject string `json:"subject"`
					TID     string `json:"tid"`
				} `json:"list"`
			} `json:"body"`
		}

		if err := json.Unmarshal(bodyBytes, &listResp); err != nil {
			w.LogError("❌ [切片JSON解析失败] 无法反序列化: %v", err)
			return nil, fmt.Errorf("解析邮件列表 JSON 异常: %w", err)
		}

		if listResp.Head.Ret != 0 {
			return nil, fmt.Errorf("腾讯返回错误: ret=%d, msg=%s", listResp.Head.Ret, listResp.Head.Msg)
		}

		if len(listResp.Body.List) == 0 {
			break
		}

		for _, m := range listResp.Body.List {
			if m.EmailID == "" || m.Subject == "" {
				continue
			}
			// 自适应会话聚合模式：若 emailid 为 C{dirid}T{tid} 壳子 ID，自动展开获取真实子邮件 ZL0012_... 列表
			if strings.HasPrefix(m.EmailID, "C") {
				tid := m.TID
				if tid == "" {
					if tIdx := strings.IndexByte(m.EmailID, 'T'); tIdx > 1 && tIdx+1 < len(m.EmailID) {
						tid = m.EmailID[tIdx+1:]
					}
				}
				if tid != "" {
					if subItems, subErr := w.fetchColloSublist(dirID, tid, sid); subErr == nil && len(subItems) > 0 {
						for _, sub := range subItems {
							if !seenMailID[sub.MailID] {
								seenMailID[sub.MailID] = true
								results = append(results, sub)
							}
						}
						continue
					}
				}
			}
			if !seenMailID[m.EmailID] {
				seenMailID[m.EmailID] = true
				results = append(results, WebmailMailItem{
					MailID:  m.EmailID,
					Subject: m.Subject,
				})
			}
		}

		if len(listResp.Body.List) < pageSize {
			break
		}
	}

	w.Log("📋 [切片清单完成] dirid=%s 内共解析出有效邮件 %d 封", dirID, len(results))
	return results, nil
}

// fetchColloSublist 展开会话聚合邮件 (C...T...)，拉取 sublist 中每封子邮件真实的 emailid
func (w *WebmailClient) fetchColloSublist(dirID, tid, sid string) ([]WebmailMailItem, error) {
	colloURL := "https://wx.mail.qq.com/list/collo_maillist"
	formData := url.Values{}
	formData.Set("dirid", dirID)
	formData.Set("tid", tid)
	formData.Set("language", "zh")
	formData.Set("r", fmt.Sprintf("%d", time.Now().UnixNano()/1e6))
	formData.Set("sid", sid)

	req, err := http.NewRequest("POST", colloURL, strings.NewReader(formData.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Referer", "https://wx.mail.qq.com/")
	req.Header.Set("Origin", "https://wx.mail.qq.com")
	req.Header.Set("Cookie", fmt.Sprintf("xm_sid=%s; sid=%s; qm_sid=%s", sid, sid, sid))

	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	var colloResp struct {
		Head struct {
			Ret int    `json:"ret"`
			Msg string `json:"msg"`
		} `json:"head"`
		Body struct {
			Sublist []struct {
				EmailID string `json:"emailid"`
				Subject string `json:"subject"`
			} `json:"sublist"`
		} `json:"body"`
	}

	if err := json.Unmarshal(bodyBytes, &colloResp); err != nil {
		return nil, err
	}
	if colloResp.Head.Ret != 0 {
		return nil, fmt.Errorf("collo_maillist ret=%d, msg=%s", colloResp.Head.Ret, colloResp.Head.Msg)
	}

	var subItems []WebmailMailItem
	for _, s := range colloResp.Body.Sublist {
		if s.EmailID != "" && s.Subject != "" {
			subItems = append(subItems, WebmailMailItem{
				MailID:  s.EmailID,
				Subject: s.Subject,
			})
		}
	}
	return subItems, nil
}

// ForwardChunkCloudFast 核心云端免流秒转：严格按照腾讯 Webmail 的两步握手协议 (func=2 预建 -> func=1 投递)
func (w *WebmailClient) ForwardChunkCloudFast(toEmail, subject, fwMailID, attachName, attachFileID, content string, onRetry func(int, time.Duration)) error {
	w.mu.RLock()
	sid := w.session.Sid
	w.mu.RUnlock()

	if sid == "" {
		return fmt.Errorf("未登录 Web 邮箱，请扫码授权")
	}

	// 如果传入时尚未填充 attachFileID，自动通过 /read/readmail 精准补齐
	if attachFileID == "" {
		realName, realFID, err := w.GetMailAttachmentInfo(fwMailID)
		if err != nil {
			return fmt.Errorf("获取邮件附件 fileid 失败: %w", err)
		}
		attachFileID = realFID
		if attachName == "" {
			attachName = realName
		}
	}

	sendURL := "https://wx.mail.qq.com/send/sendmail"
	attachJSON := fmt.Sprintf(`{"name":"%s","fileid":"%s"}`, attachName, attachFileID)

	w.Log("🚀 [云端直发第一步] 正在预建转发任务 (func=2): fw_mailid=%s, fileid=%s, 接收方=%s", fwMailID, attachFileID, toEmail)

	// 第一步：func=2 预建草稿，获取临时 mailid (全面接入智能阶梯避让重试，攻克 -20003 频控)
	var draftMailID string
	maxRetries := 10
	for attempt := 1; attempt <= maxRetries; attempt++ {
		form1 := url.Values{}
		form1.Set("func", "2")
		form1.Set("time_zone", "28800")
		form1.Set("time_zone_dst", "0")
		form1.Set("manual_save", "false")
		form1.Set("fw_mailid", fwMailID)
		form1.Set("subject", subject)
		form1.Set("to", fmt.Sprintf("<%s>", toEmail))
		form1.Set("from", fmt.Sprintf("<%s>", GlobalConfig.Email))
		form1.Set("attachment", attachJSON)
		if content != "" {
			form1.Set("content", fmt.Sprintf("<pre>%s</pre>", content))
		}
		form1.Set("is_text", "false")
		form1.Set("is_urgent", "false")
		form1.Set("is_resume", "false")
		form1.Set("save_to_send", "false")
		form1.Set("need_receipt", "false")
		form1.Set("separated_copy", "false")
		form1.Set("language", "zh")
		form1.Set("r", fmt.Sprintf("%d", time.Now().UnixNano()/1e6))
		form1.Set("sid", sid)

		req1, err := http.NewRequest("POST", sendURL, strings.NewReader(form1.Encode()))
		if err != nil {
			return err
		}
		req1.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req1.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		req1.Header.Set("Referer", "https://wx.mail.qq.com/")
		req1.Header.Set("Origin", "https://wx.mail.qq.com")
		req1.Header.Set("Cookie", fmt.Sprintf("xm_sid=%s; sid=%s; qm_sid=%s", sid, sid, sid))

		resp1, err := w.client.Do(req1)
		if err != nil {
			return fmt.Errorf("第一步请求失败: %w", err)
		}

		bodyBytes1, _ := io.ReadAll(resp1.Body)
		resp1.Body.Close()

		var stage1Resp struct {
			Head struct {
				Ret int    `json:"ret"`
				Msg string `json:"msg"`
			} `json:"head"`
			Body struct {
				MailID string `json:"mailid"`
			} `json:"body"`
		}
		if err := json.Unmarshal(bodyBytes1, &stage1Resp); err != nil {
			return fmt.Errorf("第一步响应解析异常: %w", err)
		}

		if stage1Resp.Head.Ret == 0 && stage1Resp.Body.MailID != "" {
			draftMailID = stage1Resp.Body.MailID
			break
		}

		// 若遇到 -20003 (发信频率过快/频控)，按用户指定阶梯执行避让重试：
		// 第1次 500ms，第2次 1s，第3次 2s，第4次 3s，后续每次 5s
		if stage1Resp.Head.Ret == -20003 && attempt < maxRetries {
			var backoff time.Duration
			switch attempt {
			case 1:
				backoff = 500 * time.Millisecond
			case 2:
				backoff = 1 * time.Second
			case 3:
				backoff = 2 * time.Second
			case 4:
				backoff = 3 * time.Second
			default:
				backoff = 5 * time.Second
			}
			if onRetry != nil {
				onRetry(attempt, backoff)
			}
			w.LogWarn("⏳ [预建频控避让] 腾讯预建草稿触发发信频率控制 (-20003)，等待 %v 后自动重试 (第 %d/%d 次)...", backoff, attempt, maxRetries-1)
			time.Sleep(backoff)
			continue
		}

		w.LogError("❌ [第一步被拒] 腾讯返回: ret=%d, msg=%s", stage1Resp.Head.Ret, stage1Resp.Head.Msg)
		return fmt.Errorf("转发预建失败 (ret=%d): %s", stage1Resp.Head.Ret, stage1Resp.Head.Msg)
	}

	if draftMailID == "" {
		return fmt.Errorf("第一步预建草稿重试超出最大次数 (-20003)")
	}
	w.Log("✔ [第一步成功] 腾讯已受理草稿，锁定 mailid: %s，等待云端就绪并执行真实投递 (func=1)...", draftMailID)

	// 平滑缓冲微延时，确保云端分布式草稿写入完全就绪
	time.Sleep(80 * time.Millisecond)

	// 第二步：func=1 携带生成的 mailid 真实发送 (支持智能阶梯避让重试，完美攻克 -20003 频控)
	for attempt := 1; attempt <= maxRetries; attempt++ {
		form2 := url.Values{}
		form2.Set("func", "1")
		form2.Set("mailid", draftMailID)
		form2.Set("time_zone", "28800")
		form2.Set("time_zone_dst", "0")
		form2.Set("fw_mailid", fwMailID)
		form2.Set("subject", subject)
		form2.Set("to", fmt.Sprintf("<%s>", toEmail))
		form2.Set("from", fmt.Sprintf("<%s>", GlobalConfig.Email))
		form2.Set("attachment", attachJSON)
		if content != "" {
			form2.Set("content", fmt.Sprintf("<pre>%s</pre>", content))
		}
		form2.Set("is_text", "false")
		form2.Set("is_urgent", "false")
		form2.Set("is_resume", "false")
		form2.Set("save_to_send", "false")
		form2.Set("need_receipt", "false")
		form2.Set("separated_copy", "false")
		form2.Set("language", "zh")
		form2.Set("r", fmt.Sprintf("%d", time.Now().UnixNano()/1e6))
		form2.Set("sid", sid)

		req2, err := http.NewRequest("POST", sendURL, strings.NewReader(form2.Encode()))
		if err != nil {
			return err
		}
		req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req2.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		req2.Header.Set("Referer", "https://wx.mail.qq.com/")
		req2.Header.Set("Origin", "https://wx.mail.qq.com")
		req2.Header.Set("Cookie", fmt.Sprintf("xm_sid=%s; sid=%s; qm_sid=%s", sid, sid, sid))

		resp2, err := w.client.Do(req2)
		if err != nil {
			return fmt.Errorf("第二步请求失败: %w", err)
		}

		bodyBytes2, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()

		var stage2Resp struct {
			Head struct {
				Ret int    `json:"ret"`
				Msg string `json:"msg"`
			} `json:"head"`
			Body struct {
				MessageID string `json:"messageid"`
			} `json:"body"`
		}
		if err := json.Unmarshal(bodyBytes2, &stage2Resp); err != nil {
			return fmt.Errorf("第二步响应解析异常: %w", err)
		}
		if stage2Resp.Head.Ret == 0 {
			w.Log("🎉 [云端直发真正成功] 腾讯服务器已投递完成！messageid: %s (0 本地流量)", stage2Resp.Body.MessageID)
			return nil
		}

		// 若遇到 -20003 (发信频率过快/频控)，按用户指定阶梯执行避让重试：
		// 第1次 500ms，第2次 1s，第3次 2s，第4次 3s，后续每次 5s
		if stage2Resp.Head.Ret == -20003 && attempt < maxRetries {
			var backoff time.Duration
			switch attempt {
			case 1:
				backoff = 500 * time.Millisecond
			case 2:
				backoff = 1 * time.Second
			case 3:
				backoff = 2 * time.Second
			case 4:
				backoff = 3 * time.Second
			default:
				backoff = 5 * time.Second
			}
			if onRetry != nil {
				onRetry(attempt, backoff)
			}
			w.LogWarn("⏳ [频控避让] 触发腾讯云端发信频率控制 (-20003)，等待 %v 后自动重试 (第 %d/%d 次)...", backoff, attempt, maxRetries-1)
			time.Sleep(backoff)
			continue
		}

		w.LogError("❌ [第二步被拒] 腾讯返回: ret=%d, msg=%s", stage2Resp.Head.Ret, stage2Resp.Head.Msg)
		return fmt.Errorf("腾讯云端拒绝发送 (ret=%d): %s", stage2Resp.Head.Ret, stage2Resp.Head.Msg)
	}
	return fmt.Errorf("重试超出最大次数")
}

// SendPlainMailCloudFast 通过腾讯 Webmail 接口直接发送纯文本/提货单邮件 (0 本地流量，免 SMTP，杜绝 535)
func (w *WebmailClient) SendPlainMailCloudFast(toEmail, subject, content string) error {
	w.mu.RLock()
	sid := w.session.Sid
	w.mu.RUnlock()

	if sid == "" {
		return fmt.Errorf("未登录 Webmail")
	}

	sendURL := "https://wx.mail.qq.com/send/sendmail"
	maxRetries := 10

	// 第一步：func=2 预建草稿 (全面接入智能阶梯避让重试，攻克 -20003 频控)
	var draftMailID string
	for attempt := 1; attempt <= maxRetries; attempt++ {
		form1 := url.Values{}
		form1.Set("func", "2")
		form1.Set("time_zone", "28800")
		form1.Set("time_zone_dst", "0")
		form1.Set("manual_save", "false")
		form1.Set("subject", subject)
		form1.Set("to", fmt.Sprintf("<%s>", toEmail))
		form1.Set("from", fmt.Sprintf("<%s>", GlobalConfig.Email))
		form1.Set("content", fmt.Sprintf("<pre>%s</pre>", content))
		form1.Set("is_text", "false")
		form1.Set("is_urgent", "false")
		form1.Set("is_resume", "false")
		form1.Set("save_to_send", "false")
		form1.Set("need_receipt", "false")
		form1.Set("separated_copy", "false")
		form1.Set("language", "zh")
		form1.Set("r", fmt.Sprintf("%d", time.Now().UnixNano()/1e6))
		form1.Set("sid", sid)

		req1, err := http.NewRequest("POST", sendURL, strings.NewReader(form1.Encode()))
		if err != nil {
			return err
		}
		req1.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req1.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		req1.Header.Set("Referer", "https://wx.mail.qq.com/")
		req1.Header.Set("Origin", "https://wx.mail.qq.com")
		req1.Header.Set("Cookie", fmt.Sprintf("xm_sid=%s; sid=%s; qm_sid=%s", sid, sid, sid))

		resp1, err := w.client.Do(req1)
		if err != nil {
			return fmt.Errorf("提货单预建失败: %w", err)
		}

		bodyBytes1, _ := io.ReadAll(resp1.Body)
		resp1.Body.Close()

		var stage1Resp struct {
			Head struct {
				Ret int    `json:"ret"`
				Msg string `json:"msg"`
			} `json:"head"`
			Body struct {
				MailID string `json:"mailid"`
			} `json:"body"`
		}
		if err := json.Unmarshal(bodyBytes1, &stage1Resp); err != nil {
			return err
		}
		if stage1Resp.Head.Ret == 0 && stage1Resp.Body.MailID != "" {
			draftMailID = stage1Resp.Body.MailID
			break
		}

		if stage1Resp.Head.Ret == -20003 && attempt < maxRetries {
			var backoff time.Duration
			switch attempt {
			case 1:
				backoff = 500 * time.Millisecond
			case 2:
				backoff = 1 * time.Second
			case 3:
				backoff = 2 * time.Second
			case 4:
				backoff = 3 * time.Second
			default:
				backoff = 5 * time.Second
			}
			w.LogWarn("⏳ [清单频控避让] 提货清单预建草稿触发发信频率控制 (-20003)，等待 %v 后自动重试 (第 %d/%d 次)...", backoff, attempt, maxRetries-1)
			time.Sleep(backoff)
			continue
		}

		return fmt.Errorf("提货单预建被拒: ret=%d, msg=%s", stage1Resp.Head.Ret, stage1Resp.Head.Msg)
	}

	if draftMailID == "" {
		return fmt.Errorf("提货单预建超出最大重试次数")
	}

	time.Sleep(80 * time.Millisecond)

	// 第二步：func=1 真实投递
	for attempt := 1; attempt <= maxRetries; attempt++ {
		form2 := url.Values{}
		form2.Set("func", "1")
		form2.Set("mailid", draftMailID)
		form2.Set("time_zone", "28800")
		form2.Set("time_zone_dst", "0")
		form2.Set("subject", subject)
		form2.Set("to", fmt.Sprintf("<%s>", toEmail))
		form2.Set("from", fmt.Sprintf("<%s>", GlobalConfig.Email))
		form2.Set("content", fmt.Sprintf("<pre>%s</pre>", content))
		form2.Set("is_text", "false")
		form2.Set("is_urgent", "false")
		form2.Set("is_resume", "false")
		form2.Set("save_to_send", "false")
		form2.Set("need_receipt", "false")
		form2.Set("separated_copy", "false")
		form2.Set("language", "zh")
		form2.Set("r", fmt.Sprintf("%d", time.Now().UnixNano()/1e6))
		form2.Set("sid", sid)

		req2, err := http.NewRequest("POST", sendURL, strings.NewReader(form2.Encode()))
		if err != nil {
			return err
		}
		req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req2.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		req2.Header.Set("Referer", "https://wx.mail.qq.com/")
		req2.Header.Set("Origin", "https://wx.mail.qq.com")
		req2.Header.Set("Cookie", fmt.Sprintf("xm_sid=%s; sid=%s; qm_sid=%s", sid, sid, sid))

		resp2, err := w.client.Do(req2)
		if err != nil {
			return fmt.Errorf("提货单投递网络失败: %w", err)
		}

		bodyBytes2, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()

		var stage2Resp struct {
			Head struct {
				Ret int    `json:"ret"`
				Msg string `json:"msg"`
			} `json:"head"`
			Body struct {
				MessageID string `json:"messageid"`
			} `json:"body"`
		}
		if err := json.Unmarshal(bodyBytes2, &stage2Resp); err != nil {
			return err
		}
		if stage2Resp.Head.Ret == 0 {
			w.Log("🎉 [提货清单送达] 提货单已成功投递！messageid: %s (0 本地流量)", stage2Resp.Body.MessageID)
			return nil
		}

		if stage2Resp.Head.Ret == -20003 && attempt < maxRetries {
			var backoff time.Duration
			switch attempt {
			case 1:
				backoff = 500 * time.Millisecond
			case 2:
				backoff = 1 * time.Second
			case 3:
				backoff = 2 * time.Second
			case 4:
				backoff = 3 * time.Second
			default:
				backoff = 5 * time.Second
			}
			w.LogWarn("⏳ [清单频控避让] 提货清单投递触发发信频率控制 (-20003)，等待 %v 后自动重试 (第 %d/%d 次)...", backoff, attempt, maxRetries-1)
			time.Sleep(backoff)
			continue
		}

		return fmt.Errorf("提货单发送被拒: ret=%d, msg=%s", stage2Resp.Head.Ret, stage2Resp.Head.Msg)
	}
	return fmt.Errorf("提货单投递超出最大重试次数")
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type progressReader struct {
	reader     io.Reader
	total      int64
	read       int64
	progressCb func(int) bool
}

func (pr *progressReader) Read(p []byte) (n int, err error) {
	n, err = pr.reader.Read(p)
	if n > 0 {
		pr.read += int64(n)
		if pr.progressCb != nil {
			if !pr.progressCb(n) {
				return n, fmt.Errorf("传输已被用户中断")
			}
		}
	}
	return n, err
}

// decodeBase64Safe 支持标准 Base64 及 URL Safe Base64 自动补齐填充
func decodeBase64Safe(s string) ([]byte, error) {
	s = strings.ReplaceAll(s, "-", "+")
	s = strings.ReplaceAll(s, "_", "/")
	for len(s)%4 != 0 {
		s += "="
	}
	return base64.StdEncoding.DecodeString(s)
}

// rushaSHA1 纯 Go 完美复刻腾讯网页版前端 Rusha.js 累积哈希算法
// 针对 5MB 满块芯片直接提取中间未填充的 5 个 32 位寄存器 (按小端序 Hex 输出，对齐 getState().sha1)
type rushaSHA1 struct {
	h      [5]uint32
	offset int
}

func newRushaSHA1() *rushaSHA1 {
	return &rushaSHA1{
		h: [5]uint32{
			0x67452301,
			0xEFCDAB89,
			0x98BADCFE,
			0x10325476,
			0xC3D2E1F0,
		},
	}
}

func (r *rushaSHA1) Update(p []byte) {
	for len(p) >= 64 {
		r.block(p[:64])
		p = p[64:]
		r.offset += 64
	}
}

func (r *rushaSHA1) block(p []byte) {
	var w [80]uint32
	for i := 0; i < 16; i++ {
		w[i] = binary.BigEndian.Uint32(p[i*4 : (i+1)*4])
	}
	for i := 16; i < 80; i++ {
		w[i] = bits.RotateLeft32(w[i-3]^w[i-8]^w[i-14]^w[i-16], 1)
	}

	a, b, c, d, e := r.h[0], r.h[1], r.h[2], r.h[3], r.h[4]

	for i := 0; i < 20; i++ {
		t := bits.RotateLeft32(a, 5) + (b&c | ^b&d) + e + 0x5A827999 + w[i]
		e, d, c, b, a = d, c, bits.RotateLeft32(b, 30), a, t
	}
	for i := 20; i < 40; i++ {
		t := bits.RotateLeft32(a, 5) + (b ^ c ^ d) + e + 0x6ED9EBA1 + w[i]
		e, d, c, b, a = d, c, bits.RotateLeft32(b, 30), a, t
	}
	for i := 40; i < 60; i++ {
		t := bits.RotateLeft32(a, 5) + (b&c | b&d | c&d) + e + 0x8F1BBCDC + w[i]
		e, d, c, b, a = d, c, bits.RotateLeft32(b, 30), a, t
	}
	for i := 60; i < 80; i++ {
		t := bits.RotateLeft32(a, 5) + (b ^ c ^ d) + e + 0xCA62C1D6 + w[i]
		e, d, c, b, a = d, c, bits.RotateLeft32(b, 30), a, t
	}

	r.h[0] += a
	r.h[1] += b
	r.h[2] += c
	r.h[3] += d
	r.h[4] += e
}

// GetStateSha1 返回对应前端 rusha.getState().sha1 的 40 字符小端序 Hex 串
func (r *rushaSHA1) GetStateSha1() string {
	buf := make([]byte, 20)
	for i := 0; i < 5; i++ {
		binary.LittleEndian.PutUint32(buf[i*4:(i+1)*4], r.h[i])
	}
	return hex.EncodeToString(buf)
}

// UploadAttachmentBytes 逆向重构版：完美对齐新版腾讯 QQ 邮箱 (wx.mail.qq.com) 官方 4 步握手协议
// 1. /attach/get_enc_key -> 2. AES-128-CTR 硬件级加密与双向哈希 -> 3. /attach/create -> 4. FTN 直传集群 (支持秒传免传)
func (w *WebmailClient) UploadAttachmentBytes(fileName string, data []byte, progressCb func(int) bool) (string, error) {
	w.mu.RLock()
	sid := w.session.Sid
	w.mu.RUnlock()

	if sid == "" {
		return "", fmt.Errorf("未登录 Webmail")
	}

	dataLen := len(data)
	rawMd5Bytes := md5.Sum(data)
	rawMd5 := hex.EncodeToString(rawMd5Bytes[:])
	rawSha1Bytes := sha1.Sum(data)
	rawSha1 := hex.EncodeToString(rawSha1Bytes[:])

	// 第一步：POST /attach/get_enc_key 获取腾讯云端动态分配的 AES-128-CTR 密钥与 IV
	rVal := fmt.Sprintf("%d", time.Now().UnixNano()/1e6)
	keyForm := url.Values{}
	keyForm.Set("bizid", "50")
	keyForm.Set("size", strconv.Itoa(dataLen))
	keyForm.Set("md5", rawMd5)
	keyForm.Set("filename", fileName)
	keyForm.Set("language", "zh")
	keyForm.Set("r", rVal)
	keyForm.Set("sid", sid)

	keyURL := "https://wx.mail.qq.com/attach/get_enc_key"
	keyReq, err := http.NewRequest("POST", keyURL, strings.NewReader(keyForm.Encode()))
	if err != nil {
		return "", fmt.Errorf("构建 get_enc_key 请求失败: %w", err)
	}
	keyReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	keyReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	keyReq.Header.Set("Referer", "https://wx.mail.qq.com/")
	keyReq.Header.Set("Origin", "https://wx.mail.qq.com")
	keyReq.Header.Set("Cookie", fmt.Sprintf("xm_sid=%s; sid=%s; qm_sid=%s", sid, sid, sid))

	keyResp, err := w.client.Do(keyReq)
	if err != nil {
		return "", fmt.Errorf("请求 get_enc_key 失败: %w", err)
	}
	keyRespBody, _ := io.ReadAll(keyResp.Body)
	keyResp.Body.Close()

	if keyResp.StatusCode != 200 {
		return "", fmt.Errorf("get_enc_key 腾讯返回 HTTP %d: %s", keyResp.StatusCode, string(keyRespBody))
	}

	var keyResult struct {
		Head struct {
			Ret int    `json:"ret"`
			Msg string `json:"msg"`
		} `json:"head"`
		Body struct {
			KeyB64 string `json:"key_b64"`
			IvB64  string `json:"iv_b64"`
		} `json:"body"`
	}
	if err := json.Unmarshal(keyRespBody, &keyResult); err != nil || keyResult.Head.Ret != 0 || keyResult.Body.KeyB64 == "" {
		return "", fmt.Errorf("解析 get_enc_key 失败 (ret=%d): %s", keyResult.Head.Ret, string(keyRespBody))
	}

	keyBytes, err := decodeBase64Safe(keyResult.Body.KeyB64)
	if err != nil {
		return "", fmt.Errorf("解码 key_b64 失败: %w", err)
	}
	ivBytes, err := decodeBase64Safe(keyResult.Body.IvB64)
	if err != nil {
		return "", fmt.Errorf("解码 iv_b64 失败: %w", err)
	}

	// 第二步：执行 AES-128-CTR 加密 (字节数严格 100% 保持一致，无任何膨胀)
	block, err := aes.NewCipher(keyBytes)
	if err != nil {
		return "", fmt.Errorf("初始化 AES 失败: %w", err)
	}
	ctrStream := cipher.NewCTR(block, ivBytes)
	encData := make([]byte, dataLen)
	ctrStream.XORKeyStream(encData, data)

	encSha1Bytes := sha1.Sum(encData)
	encSha1 := hex.EncodeToString(encSha1Bytes[:])

	// 第三步：POST /attach/create 创建云端附件记录 (支持极速秒传)
	createForm := url.Values{}
	createForm.Set("sha", rawSha1)
	createForm.Set("md5", rawMd5)
	createForm.Set("name", fileName)
	createForm.Set("size", strconv.Itoa(dataLen))
	createForm.Set("bizid", "50")
	createForm.Set("support_cstore", "true")
	createForm.Set("enc_iv", keyResult.Body.IvB64)
	createForm.Set("enc_key", keyResult.Body.KeyB64)
	createForm.Set("enc_sha", encSha1)
	createForm.Set("enc_size", strconv.Itoa(dataLen))
	createForm.Set("create_from", "compose")
	createForm.Set("language", "zh")
	createForm.Set("r", fmt.Sprintf("%d", time.Now().UnixNano()/1e6))
	createForm.Set("sid", sid)

	createURL := "https://wx.mail.qq.com/attach/create"
	createReq, err := http.NewRequest("POST", createURL, strings.NewReader(createForm.Encode()))
	if err != nil {
		return "", fmt.Errorf("构建 attach/create 请求失败: %w", err)
	}
	createReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	createReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	createReq.Header.Set("Referer", "https://wx.mail.qq.com/")
	createReq.Header.Set("Origin", "https://wx.mail.qq.com")
	createReq.Header.Set("Cookie", fmt.Sprintf("xm_sid=%s; sid=%s; qm_sid=%s", sid, sid, sid))

	createResp, err := w.client.Do(createReq)
	if err != nil {
		return "", fmt.Errorf("请求 attach/create 失败: %w", err)
	}
	createRespBody, _ := io.ReadAll(createResp.Body)
	createResp.Body.Close()

	if createResp.StatusCode != 200 {
		return "", fmt.Errorf("attach/create 腾讯返回 HTTP %d: %s", createResp.StatusCode, string(createRespBody))
	}

	var createResult struct {
		Head struct {
			Ret int    `json:"ret"`
			Msg string `json:"msg"`
		} `json:"head"`
		Body struct {
			FileID    string `json:"fileid"`
			IsExist   bool   `json:"is_exist"`
			DNS       string `json:"dns"`
			UploadKey string `json:"upload_key"`
		} `json:"body"`
	}
	if err := json.Unmarshal(createRespBody, &createResult); err != nil || createResult.Head.Ret != 0 || createResult.Body.FileID == "" {
		return "", fmt.Errorf("解析 attach/create 失败 (ret=%d): %s", createResult.Head.Ret, string(createRespBody))
	}

	fileID := createResult.Body.FileID

	// 极速秒传命中：如果腾讯云端已有相同文件，直接返回 fileid，0 流量秒过！
	if createResult.Body.IsExist {
		w.Log("⚡ [HTTP云端极速秒传] 命中云端现有指纹，免传直通成功: fileid=%s (%s)", fileID, fileName)
		if progressCb != nil {
			progressCb(dataLen)
		}
		return fileID, nil
	}

	// 第四步：将加密数据直传腾讯 FTN 分布式存储集群 (gzc-dfsup.mail.ftn.qq.com)
	dns := createResult.Body.DNS
	if dns == "" {
		dns = "gzc-dfsup.mail.ftn.qq.com"
	}
	uKey := createResult.Body.UploadKey
	appID := 1371 // attach 专用应用 ID

	chipSize := 5242880 // 5MB 芯片切片标准
	chipsCount := (dataLen + chipSize - 1) / chipSize

	if chipsCount <= 1 {
		// 单片极速直传 (<= 5MB)
		putURL := fmt.Sprintf("https://%s/%d/%s?ukey=%s", dns, appID, fileID, uKey)
		upReader := &progressReader{
			reader:     bytes.NewReader(encData),
			total:      int64(dataLen),
			progressCb: progressCb,
		}
		putReq, err := http.NewRequest("PUT", putURL, upReader)
		if err != nil {
			return "", fmt.Errorf("构建 FTN 单片 PUT 失败: %w", err)
		}
		putReq.ContentLength = int64(dataLen)
		putReq.Header.Set("Content-Type", "application/octet-stream")
		putReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		putReq.Header.Set("Referer", "https://wx.mail.qq.com/")
		putReq.Header.Set("Origin", "https://wx.mail.qq.com")

		putResp, err := w.client.Do(putReq)
		if err != nil {
			return "", fmt.Errorf("FTN 单片直传失败: %w", err)
		}
		putRespBody, _ := io.ReadAll(putResp.Body)
		putResp.Body.Close()

		if putResp.StatusCode != 200 {
			errCode := putResp.Header.Get("X-Error-Code")
			errMsg := putResp.Header.Get("X-Error-Message")
			return "", fmt.Errorf("FTN 单片直传返回 HTTP %d (code=%s, msg=%s): %s", putResp.StatusCode, errCode, errMsg, string(putRespBody))
		}
	} else {
		// 多芯片分片上传 (针对 50MB 等大分片): ?init -> 多路 PUT -> ?complete
		type chipPart struct {
			EndOffset    int    `json:"endoffset"`
			PartNumber   int    `json:"partnumber"`
			CumulateSha1 string `json:"cumulatesha1"`
		}
		rusha := newRushaSHA1()
		parts := make([]chipPart, 0, chipsCount)
		for p := 1; p <= chipsCount; p++ {
			startOffset := (p - 1) * chipSize
			endOffset := p * chipSize
			if endOffset > dataLen {
				endOffset = dataLen
			}
			chunkSlice := encData[startOffset:endOffset]
			var cumSha string
			if p == chipsCount {
				// 最后一个芯片：携带标准完整 SHA-1
				cumSha = encSha1
			} else {
				// 中间 5MB 满块芯片：累积更新 SHA-1 并提取 Rusha 内部寄存器小端序 Hex (getState().sha1)
				rusha.Update(chunkSlice)
				cumSha = rusha.GetStateSha1()
			}
			parts = append(parts, chipPart{
				EndOffset:    endOffset,
				PartNumber:   p,
				CumulateSha1: cumSha,
			})
		}

		initPayload, _ := json.Marshal(map[string]interface{}{
			"partcount": len(parts),
			"parts":     parts,
		})

		initURL := fmt.Sprintf("https://%s/%d/%s?init&ukey=%s", dns, appID, fileID, uKey)
		initReq, err := http.NewRequest("POST", initURL, bytes.NewReader(initPayload))
		if err != nil {
			return "", fmt.Errorf("构建 FTN ?init 失败: %w", err)
		}
		initReq.ContentLength = int64(len(initPayload))
		initReq.Header.Set("Content-Type", "application/octet-stream")
		initReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		initReq.Header.Set("Referer", "https://wx.mail.qq.com/")
		initReq.Header.Set("Origin", "https://wx.mail.qq.com")

		initResp, err := w.client.Do(initReq)
		if err != nil {
			return "", fmt.Errorf("FTN ?init 请求失败: %w", err)
		}
		initRespBody, _ := io.ReadAll(initResp.Body)
		initResp.Body.Close()
		if initResp.StatusCode != 200 {
			errCode := initResp.Header.Get("X-Error-Code")
			errMsg := initResp.Header.Get("X-Error-Message")
			return "", fmt.Errorf("FTN ?init 返回 HTTP %d (code=%s, msg=%s): %s", initResp.StatusCode, errCode, errMsg, string(initRespBody))
		}

		// 循环上传每一个 5MB 芯片
		type completePart struct {
			PartNumber int    `json:"partnumber"`
			ETag       string `json:"ETag"`
		}
		completeParts := make([]completePart, 0, chipsCount)

		var totalUploaded int64
		for p := 1; p <= chipsCount; p++ {
			startOffset := (p - 1) * chipSize
			endOffset := p * chipSize
			if endOffset > dataLen {
				endOffset = dataLen
			}
			chunkSlice := encData[startOffset:endOffset]
			partLen := int64(len(chunkSlice))

			chipURL := fmt.Sprintf("https://%s/%d/%s?ukey=%s&partnumber=%d", dns, appID, fileID, uKey, p)
			upReader := &progressReader{
				reader: bytes.NewReader(chunkSlice),
				total:  partLen,
				progressCb: func(n int) bool {
					totalUploaded += int64(n)
					if progressCb != nil {
						return progressCb(n)
					}
					return true
				},
			}

			chipReq, err := http.NewRequest("PUT", chipURL, upReader)
			if err != nil {
				return "", fmt.Errorf("构建 FTN Part %d PUT 失败: %w", p, err)
			}
			chipReq.ContentLength = partLen
			chipReq.Header.Set("Content-Type", "application/octet-stream")
			chipReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
			chipReq.Header.Set("Referer", "https://wx.mail.qq.com/")
			chipReq.Header.Set("Origin", "https://wx.mail.qq.com")

			chipResp, err := w.client.Do(chipReq)
			if err != nil {
				return "", fmt.Errorf("FTN Part %d 直传失败: %w", p, err)
			}
			chipRespBody, _ := io.ReadAll(chipResp.Body)
			chipResp.Body.Close()
			if chipResp.StatusCode != 200 {
				errCode := chipResp.Header.Get("X-Error-Code")
				errMsg := chipResp.Header.Get("X-Error-Message")
				return "", fmt.Errorf("FTN Part %d 返回 HTTP %d (code=%s, msg=%s): %s", p, chipResp.StatusCode, errCode, errMsg, string(chipRespBody))
			}

			eTag := chipResp.Header.Get("ETag")
			if eTag == "" {
				eTag = chipResp.Header.Get("Etag")
			}
			completeParts = append(completeParts, completePart{
				PartNumber: p,
				ETag:       eTag,
			})
		}

		// 发送 ?complete 结束标记
		compPayload, _ := json.Marshal(map[string]interface{}{
			"partcount": len(completeParts),
			"parts":     completeParts,
		})

		compURL := fmt.Sprintf("https://%s/%d/%s?complete&ukey=%s", dns, appID, fileID, uKey)
		compReq, err := http.NewRequest("POST", compURL, bytes.NewReader(compPayload))
		if err != nil {
			return "", fmt.Errorf("构建 FTN ?complete 失败: %w", err)
		}
		compReq.ContentLength = int64(len(compPayload))
		compReq.Header.Set("Content-Type", "application/octet-stream")
		compReq.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		compReq.Header.Set("Referer", "https://wx.mail.qq.com/")
		compReq.Header.Set("Origin", "https://wx.mail.qq.com")

		compResp, err := w.client.Do(compReq)
		if err != nil {
			return "", fmt.Errorf("FTN ?complete 请求失败: %w", err)
		}
		compRespBody, _ := io.ReadAll(compResp.Body)
		compResp.Body.Close()
		if compResp.StatusCode != 200 {
			errCode := compResp.Header.Get("X-Error-Code")
			errMsg := compResp.Header.Get("X-Error-Message")
			return "", fmt.Errorf("FTN ?complete 返回 HTTP %d (code=%s, msg=%s): %s", compResp.StatusCode, errCode, errMsg, string(compRespBody))
		}
	}

	w.Log("✔ [HTTP直传成功] 成功通过 FTN 直传集群入库附件: fileid=%s, 文件名=%s", fileID, fileName)
	return fileID, nil
}

// SendChunkMail 通过腾讯 Webmail 接口将切片附件自发自收投递入库
func (w *WebmailClient) SendChunkMail(toEmail, subject, fileName, attachFileID, metaBase64 string) error {
	w.mu.RLock()
	sid := w.session.Sid
	w.mu.RUnlock()

	if sid == "" {
		return fmt.Errorf("未登录 Webmail")
	}

	sendURL := "https://wx.mail.qq.com/send/sendmail"
	// 腾讯 Webmail 接口规范：单个普通附件参数为单个 JSON 结构字符串 {"name":"...","fileid":"..."}（不可带外层数组中括号）
	attachJSON := fmt.Sprintf(`{"name":"%s","fileid":"%s"}`, fileName, attachFileID)

	// 邮件正文：将元数据作为 HTML 注释静默存放，对用户彻底隐形，避免正文出现长串 Base64 乱码
	content := "<div><br></div>"
	if metaBase64 != "" {
		content = fmt.Sprintf("<div><!-- QMD-META: %s --><br></div>", metaBase64)
	}

	fromAddr := fmt.Sprintf("<%s>", GlobalConfig.Email)
	if GlobalConfig.UserNick != "" {
		fromAddr = fmt.Sprintf(`"%s"<%s>`, GlobalConfig.UserNick, GlobalConfig.Email)
	}
	toAddr := fmt.Sprintf("<%s>", toEmail)
	if GlobalConfig.UserNick != "" {
		toAddr = fmt.Sprintf(`"%s"<%s>`, GlobalConfig.UserNick, toEmail)
	}

	maxRetries := 5
	var draftMailID string

	// 第一步：func=2 预建草稿
	for attempt := 1; attempt <= maxRetries; attempt++ {
		form1 := url.Values{}
		form1.Set("func", "2")
		form1.Set("time_zone", "28800")
		form1.Set("time_zone_dst", "0")
		form1.Set("manual_save", "false")
		form1.Set("re_mailid", "")
		form1.Set("fw_mailid", "")
		form1.Set("calendar", "")
		form1.Set("subject", subject)
		form1.Set("to", toAddr)
		form1.Set("from", fromAddr)
		form1.Set("content", content)
		form1.Set("is_text", "false")
		form1.Set("is_urgent", "false")
		form1.Set("is_resume", "false")
		form1.Set("save_to_send", "false")
		form1.Set("need_receipt", "false")
		form1.Set("separated_copy", "false")
		form1.Set("attachment", attachJSON)
		form1.Set("language", "zh")
		form1.Set("r", fmt.Sprintf("%d", time.Now().UnixNano()/1e6))
		form1.Set("sid", sid)

		req1, err := http.NewRequest("POST", sendURL, strings.NewReader(form1.Encode()))
		if err != nil {
			return err
		}
		req1.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req1.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		req1.Header.Set("Referer", "https://wx.mail.qq.com/")
		req1.Header.Set("Origin", "https://wx.mail.qq.com")
		req1.Header.Set("Cookie", fmt.Sprintf("xm_sid=%s; sid=%s; qm_sid=%s", sid, sid, sid))

		resp1, err := w.client.Do(req1)
		if err != nil {
			return err
		}
		bodyBytes1, _ := io.ReadAll(resp1.Body)
		resp1.Body.Close()

		var stage1Resp struct {
			Head struct {
				Ret int    `json:"ret"`
				Msg string `json:"msg"`
			} `json:"head"`
			Body struct {
				MailID string `json:"mailid"`
			} `json:"body"`
		}
		if json.Unmarshal(bodyBytes1, &stage1Resp) == nil && stage1Resp.Head.Ret == 0 && stage1Resp.Body.MailID != "" {
			draftMailID = stage1Resp.Body.MailID
			break
		}

		if stage1Resp.Head.Ret == -20003 && attempt < maxRetries {
			backoff := time.Duration(attempt) * 500 * time.Millisecond
			w.LogWarn("⏳ [HTTP发信频控避让] 预建草稿触发频率控制 (-20003)，避让等待 %v 后重试...", backoff)
			time.Sleep(backoff)
			continue
		}

		return fmt.Errorf("预建分片草稿失败: ret=%d, msg=%s", stage1Resp.Head.Ret, stage1Resp.Head.Msg)
	}

	if draftMailID == "" {
		return fmt.Errorf("预建分片草稿超时")
	}

	time.Sleep(60 * time.Millisecond)

	// 第二步：func=1 真实投递
	for attempt := 1; attempt <= maxRetries; attempt++ {
		form2 := url.Values{}
		form2.Set("func", "1")
		form2.Set("mailid", draftMailID)
		form2.Set("time_zone", "28800")
		form2.Set("time_zone_dst", "0")
		form2.Set("re_mailid", "")
		form2.Set("fw_mailid", "")
		form2.Set("calendar", "")
		form2.Set("subject", subject)
		form2.Set("to", toAddr)
		form2.Set("from", fromAddr)
		form2.Set("content", content)
		form2.Set("is_text", "false")
		form2.Set("is_urgent", "false")
		form2.Set("is_resume", "false")
		form2.Set("save_to_send", "false")
		form2.Set("need_receipt", "false")
		form2.Set("separated_copy", "false")
		form2.Set("attachment", attachJSON)
		form2.Set("language", "zh")
		form2.Set("r", fmt.Sprintf("%d", time.Now().UnixNano()/1e6))
		form2.Set("sid", sid)

		req2, err := http.NewRequest("POST", sendURL, strings.NewReader(form2.Encode()))
		if err != nil {
			return err
		}
		req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req2.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
		req2.Header.Set("Referer", "https://wx.mail.qq.com/")
		req2.Header.Set("Origin", "https://wx.mail.qq.com")
		req2.Header.Set("Cookie", fmt.Sprintf("xm_sid=%s; sid=%s; qm_sid=%s", sid, sid, sid))

		resp2, err := w.client.Do(req2)
		if err != nil {
			return err
		}
		bodyBytes2, _ := io.ReadAll(resp2.Body)
		resp2.Body.Close()

		var stage2Resp struct {
			Head struct {
				Ret int    `json:"ret"`
				Msg string `json:"msg"`
			} `json:"head"`
		}
		if json.Unmarshal(bodyBytes2, &stage2Resp) == nil && stage2Resp.Head.Ret == 0 {
			return nil
		}

		if stage2Resp.Head.Ret == -20003 && attempt < maxRetries {
			backoff := time.Duration(attempt) * 500 * time.Millisecond
			w.LogWarn("⏳ [HTTP发信频控避让] 真实投递触发频率控制 (-20003)，避让等待 %v 后重试...", backoff)
			time.Sleep(backoff)
			continue
		}

		return fmt.Errorf("分片投递失败: ret=%d, msg=%s", stage2Resp.Head.Ret, stage2Resp.Head.Msg)
	}

	return fmt.Errorf("分片投递超出最大重试次数")
}

// UploadChunkViaWebmail 完整执行“二进制附件上传 + 自发自收投递入库”闭环
func (w *WebmailClient) UploadChunkViaWebmail(toEmail, subject, fileName, metaBase64 string, data []byte, progressCb func(int) bool) error {
	fid, err := w.UploadAttachmentBytes(fileName, data, progressCb)
	if err != nil {
		return fmt.Errorf("HTTP 附件直传失败: %w", err)
	}
	if err := w.SendChunkMail(toEmail, subject, fileName, fid, metaBase64); err != nil {
		return fmt.Errorf("HTTP 自发入库失败: %w", err)
	}
	return nil
}
