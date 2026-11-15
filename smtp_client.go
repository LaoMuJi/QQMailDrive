package main

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/smtp"
	"strings"
	"sync"
	"time"
)

// SMTPClient 专用于 QQMailDrive 的大邮件流式 SMTP 发送客户端 (支持多端口自愈、AUTH LOGIN 状态机、凭证清洗与多策略回退)
type SMTPClient struct {
	server   string
	email    string
	authCode string
	mu       sync.Mutex
	Logger   func(level LogLevel, module, msg string)
}

func NewSMTPClient(server, email, authCode string) *SMTPClient {
	if server == "" {
		server = "smtp.qq.com:465"
	}
	return &SMTPClient{
		server:   server,
		email:    email,
		authCode: authCode,
	}
}

func (c *SMTPClient) log(level LogLevel, msg string) {
	if c.Logger != nil {
		c.Logger(level, "SMTP", msg)
	} else if GlobalEngine != nil {
		GlobalEngine.LogWithLevel(level, "SMTP", msg)
	} else {
		fmt.Printf("[%s] [SMTP] %s\n", level, msg)
	}
}

// connectAndAuth 核心健壮连接与自愈鉴权 (兼容保留，默认使用第 1 轮 465 SSL 端口)
func (c *SMTPClient) connectAndAuth() (*smtp.Client, net.Conn, error) {
	return c.connectAndAuthWithAttempt(1)
}

// connectAndAuthWithAttempt 按照指定重试轮次交替轮换端口进行连接鉴权：
// 涵盖 分片流式直发 (SendStreamMessageWithProgress) 与 提货单邮件投递 (SendManifestMessage)
// 轮换端口机制：
//   - 第 1 轮：465 (SSL)
//   - 第 2 轮：587 (STARTTLS)
//   - 第 3 轮：465 (SSL)
//   - 第 4 轮：587 (STARTTLS)
//   - 第 5 轮：465 (SSL)
func (c *SMTPClient) connectAndAuthWithAttempt(attempt int) (*smtp.Client, net.Conn, error) {
	cleanEmail := strings.TrimSpace(c.email)
	if cleanEmail == "" {
		cleanEmail = strings.TrimSpace(GlobalConfig.Email)
	}
	rawCode := strings.TrimSpace(c.authCode)
	if rawCode == "" {
		rawCode = strings.TrimSpace(GlobalConfig.IMAPAuthCode)
	}
	cleanCode := strings.ReplaceAll(strings.ReplaceAll(rawCode, " ", ""), "	", "")
	lowerCode := strings.ToLower(cleanCode)
	pureQQ := strings.Split(cleanEmail, "@")[0]

	host, _, err := net.SplitHostPort(c.server)
	if err != nil {
		host = c.server
	}
	if host == "" {
		host = "smtp.qq.com"
	}

	tlsConfig := &tls.Config{
		ServerName: host,
	}

	dial465 := func() (*smtp.Client, net.Conn, error) {
		conn, err := tls.Dial("tcp", net.JoinHostPort(host, "465"), tlsConfig)
		if err != nil {
			return nil, nil, err
		}
		client, err := smtp.NewClient(conn, host)
		if err != nil {
			conn.Close()
			return nil, nil, err
		}
		return client, conn, nil
	}

	dial587 := func() (*smtp.Client, net.Conn, error) {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, "587"), 5*time.Second)
		if err != nil {
			return nil, nil, err
		}
		client, err := smtp.NewClient(conn, host)
		if err != nil {
			conn.Close()
			return nil, nil, err
		}
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(tlsConfig); err != nil {
				client.Close()
				return nil, nil, err
			}
		}
		return client, conn, nil
	}

	type authStrategy struct {
		name string
		user string
		pass string
		mech string
	}

	strategies := []authStrategy{
		{"邮箱全称+标准授权码 (AUTH LOGIN)", cleanEmail, lowerCode, "LOGIN"},
		{"纯QQ号+标准授权码 (AUTH LOGIN)", pureQQ, lowerCode, "LOGIN"},
		{"邮箱全称+原样授权码 (AUTH LOGIN)", cleanEmail, cleanCode, "LOGIN"},
		{"标准 PLAIN 认证", cleanEmail, lowerCode, "PLAIN"},
	}

	dialers := []struct {
		portName string
		fn       func() (*smtp.Client, net.Conn, error)
	}{
		{"465 (SSL)", dial465},
		{"587 (STARTTLS)", dial587},
	}

	// 轮换端口选择：根据 attempt 动态选择端口 (1: 465, 2: 587, 3: 465, 4: 587, 5: 465)
	chosenIdx := 0
	if attempt > 0 {
		chosenIdx = (attempt - 1) % len(dialers)
	}
	d := dialers[chosenIdx]

	c.log(LogLevelInfo, fmt.Sprintf("正在与 SMTP 服务器建立连接: %s:%s (账号: %s, 轮次: %d/5)...", host, d.portName, cleanEmail, attempt))
	var lastErr error
	for _, strat := range strategies {
		client, conn, dErr := d.fn()
		if dErr != nil {
			lastErr = dErr
			c.log(LogLevelDebug, fmt.Sprintf("连接 %s 端口失败: %v", d.portName, dErr))
			return nil, nil, dErr
		}

		var auth smtp.Auth
		if strat.mech == "LOGIN" {
			auth = LoginAuth(strat.user, strat.pass)
		} else {
			auth = smtp.PlainAuth("", strat.user, strat.pass, host)
		}

		aErr := client.Auth(auth)
		if aErr == nil {
			c.log(LogLevelInfo, fmt.Sprintf("✔ SMTP 认证成功！已通过 %s [%s 端口] 鉴权！", strat.name, d.portName))
			return client, conn, nil
		}

		lastErr = aErr
		c.log(LogLevelInfo, fmt.Sprintf("ℹ️ 尝试 %s [%s] 提示: %v", strat.name, d.portName, aErr))
		client.Quit()
		conn.Close()
	}

	c.log(LogLevelError, fmt.Sprintf("❌ SMTP [%s 端口] 鉴权失败: %v", d.portName, lastErr))
	return nil, nil, fmt.Errorf("SMTP [%s 端口] 登录失败，请核对 16 位授权码: %w", d.portName, lastErr)
}

// SendStreamMessageWithProgress 通过 SMTP 流式直发带附件大邮件 (零临时磁盘写入，512KB 分批上报进度)
func (c *SMTPClient) SendStreamMessageWithProgress(toEmail, subject, metaBase64, seqFileName string, fileData, thumbBody []byte, progressCb func(int) bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		client, conn, err := c.connectAndAuthWithAttempt(attempt)
		if err != nil {
			lastErr = err
			c.log(LogLevelWarn, fmt.Sprintf("SMTP 连接鉴权异常 (%v)，等待 100ms 后执行第 %d/5 次轮换端口重试...", err, attempt))
			time.Sleep(100 * time.Millisecond)
			continue
		}

		sendErr := func() error {
			defer conn.Close()
			defer client.Quit()

			cleanFrom := strings.Trim(strings.TrimSpace(c.email), "<>")
			if cleanFrom == "" {
				cleanFrom = strings.Trim(strings.TrimSpace(GlobalConfig.Email), "<>")
			}
			cleanTo := strings.Trim(strings.TrimSpace(toEmail), "<>")

			if err := c.sendMailFrom(client, cleanFrom); err != nil {
				return fmt.Errorf("MAIL FROM 失败: %w", err)
			}
			if err := c.sendRcptTo(client, cleanTo); err != nil {
				return fmt.Errorf("RCPT TO 失败 (目标地址可能受限): %w", err)
			}

			w, err := client.Data()
			if err != nil {
				return fmt.Errorf("DATA 握手失败: %w", err)
			}
			defer w.Close()

			boundary := fmt.Sprintf("----_QMD_Share_%d", time.Now().UnixNano())
			var hdrBuf bytes.Buffer
			hdrBuf.WriteString("From: " + c.email + "\r\n")
			hdrBuf.WriteString("To: " + toEmail + "\r\n")
			hdrBuf.WriteString("Subject: " + subject + "\r\n")
			hdrBuf.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
			if metaBase64 != "" {
				hdrBuf.WriteString(foldHeaderLine("X-Drive-Meta", metaBase64))
			}
			hdrBuf.WriteString("MIME-Version: 1.0\r\n")
			hdrBuf.WriteString("Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n\r\n")

			// Part 1: 邮件正文 (若包含缩略图，写入 Part 1)
			if len(thumbBody) > 0 {
				hdrBuf.WriteString("--" + boundary + "\r\n")
				hdrBuf.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
				hdrBuf.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
				hdrBuf.WriteString(base64.StdEncoding.EncodeToString(thumbBody) + "\r\n")
			}

			// Part 2: 附件切片
			hdrBuf.WriteString("--" + boundary + "\r\n")
			hdrBuf.WriteString("Content-Type: application/octet-stream; name=\"" + seqFileName + "\"\r\n")
			hdrBuf.WriteString("Content-Disposition: attachment; filename=\"" + seqFileName + "\"\r\n")
			hdrBuf.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")

			if _, err := w.Write(hdrBuf.Bytes()); err != nil {
				return fmt.Errorf("写入报头失败: %w", err)
			}

			lw := newLineWrappingWriter(w, 76)
			b64Enc := base64.NewEncoder(base64.StdEncoding, lw)
			const bufSize = 512 * 1024
			totalLen := len(fileData)
			written := 0
			cancelled := false

			for written < totalLen {
				toWrite := bufSize
				if totalLen-written < toWrite {
					toWrite = totalLen - written
				}
				n, wErr := b64Enc.Write(fileData[written : written+toWrite])
				if wErr != nil {
					return fmt.Errorf("流式发送附件失败: %w", wErr)
				}
				written += n
				if progressCb != nil {
					b64Sent := int(float64(n) * 4.0 / 3.0)
					if !progressCb(b64Sent) {
						cancelled = true
						break
					}
				}
			}

			if cancelled {
				c.log(LogLevelInfo, "✔ 发送任务已被用户主动取消")
				return fmt.Errorf("发信任务已取消")
			}

			if err := b64Enc.Close(); err != nil {
				return err
			}

			if lw.lineLen > 0 {
				if _, err := w.Write([]byte("\r\n")); err != nil {
					return err
				}
			}

			footer := "--" + boundary + "--\r\n"
			if _, err := w.Write([]byte(footer)); err != nil {
				return err
			}

			c.log(LogLevelInfo, fmt.Sprintf("✔ 邮件成功推送至 %s (主题: %s)", toEmail, subject))
			return nil
		}()

		if sendErr == nil {
			return nil
		}
		if sendErr.Error() == "发信任务已取消" {
			return sendErr
		}

		lastErr = sendErr
		if attempt < 5 {
			c.log(LogLevelWarn, fmt.Sprintf("SMTP 发送遇到异常 (%v)，等待 100ms 后执行第 %d/5 次轮换端口重试...", sendErr, attempt))
			time.Sleep(100 * time.Millisecond)
		}
	}

	return fmt.Errorf("SMTP 发送分片邮件失败，已轮换端口重试 5 次: %w", lastErr)
}

// SendManifestMessage 发送极轻量的提货清单引导邮件 (无大附件，几KB瞬发)
func (c *SMTPClient) SendManifestMessage(toEmail, subject, manifestJSON string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var lastErr error
	for attempt := 1; attempt <= 5; attempt++ {
		client, conn, err := c.connectAndAuthWithAttempt(attempt)
		if err != nil {
			lastErr = err
			c.log(LogLevelWarn, fmt.Sprintf("SMTP 提货单连接鉴权异常 (%v)，等待 100ms 后执行第 %d/5 次轮换端口重试...", err, attempt))
			time.Sleep(100 * time.Millisecond)
			continue
		}

		sendErr := func() error {
			defer conn.Close()
			defer client.Quit()

			cleanFrom := strings.Trim(strings.TrimSpace(c.email), "<>")
			if cleanFrom == "" {
				cleanFrom = strings.Trim(strings.TrimSpace(GlobalConfig.Email), "<>")
			}
			cleanTo := strings.Trim(strings.TrimSpace(toEmail), "<>")

			if err := c.sendMailFrom(client, cleanFrom); err != nil {
				return fmt.Errorf("MAIL FROM 失败: %w", err)
			}
			if err := c.sendRcptTo(client, cleanTo); err != nil {
				return fmt.Errorf("RCPT TO 失败 (目标地址可能受限): %w", err)
			}

			w, err := client.Data()
			if err != nil {
				return fmt.Errorf("DATA 握手失败: %w", err)
			}
			defer w.Close()

			manifestB64 := base64.StdEncoding.EncodeToString([]byte(manifestJSON))
			var msgBuf bytes.Buffer
			msgBuf.WriteString("From: " + cleanFrom + "\r\n")
			msgBuf.WriteString("To: " + cleanTo + "\r\n")
			msgBuf.WriteString("Subject: " + subject + "\r\n")
			msgBuf.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
			msgBuf.WriteString("Message-ID: <" + fmt.Sprintf("qmd.%d@qq.com", time.Now().UnixNano()) + ">\r\n")
			msgBuf.WriteString(foldHeaderLine("X-Drive-Meta", manifestB64))
			msgBuf.WriteString("Content-Type: text/plain; charset=UTF-8\r\n\r\n")
			msgBuf.WriteString("这是一封来自 QQMailDrive 的文件分享提货清单邮件。请通过客户端自动接收，无需手动处理。\r\n\r\n")

			var prettyJSON bytes.Buffer
			if err := json.Indent(&prettyJSON, []byte(manifestJSON), "", "  "); err == nil {
				msgBuf.Write(prettyJSON.Bytes())
			} else {
				msgBuf.WriteString(manifestJSON)
			}
			msgBuf.WriteString("\r\n")

			if _, err := io.Copy(w, &msgBuf); err != nil {
				return err
			}

			c.log(LogLevelInfo, fmt.Sprintf("✔ 分享清单凭证邮件已通过 SMTP 协议成功送达 %s", cleanTo))
			return nil
		}()

		if sendErr == nil {
			return nil
		}
		lastErr = sendErr
		if attempt < 5 {
			c.log(LogLevelWarn, fmt.Sprintf("SMTP 提货单发送遇到异常 (%v)，等待 100ms 后执行第 %d/5 次轮换端口重试...", sendErr, attempt))
			time.Sleep(100 * time.Millisecond)
		}
	}

	return fmt.Errorf("提货单邮件发送失败，已轮换端口重试 5 次: %w", lastErr)
}

// sendMailFrom 健壮发送 MAIL FROM，彻底杜绝 QQ 邮箱 SMTP 502 "Invalid paramenters" 报错
func (c *SMTPClient) sendMailFrom(client *smtp.Client, fromEmail string) error {
	cleanFrom := strings.Trim(strings.TrimSpace(fromEmail), "<>")
	if cleanFrom == "" {
		cleanFrom = strings.Trim(strings.TrimSpace(c.email), "<>")
	}
	if cleanFrom == "" {
		cleanFrom = strings.Trim(strings.TrimSpace(GlobalConfig.Email), "<>")
	}

	// 优先通过 client.Text 发送纯净原生指令 MAIL FROM:<email>
	// 原因：Go 官方 net/smtp 的 client.Mail() 会自动附带 BODY=8BITMIME 或 SMTPUTF8 参数，
	// 而腾讯 QQ 邮箱 SMTP 服务器 (smtp.qq.com) 对此扩展参数不兼容，直接报错 502 "Invalid paramenters"。
	if client.Text != nil {
		id, err := client.Text.Cmd("MAIL FROM:<%s>", cleanFrom)
		if err == nil {
			client.Text.StartResponse(id)
			_, _, rErr := client.Text.ReadResponse(250)
			client.Text.EndResponse(id)
			if rErr == nil {
				return nil
			}
		}
	}

	// 备用降级
	return client.Mail(cleanFrom)
}

// sendRcptTo 健壮发送 RCPT TO
func (c *SMTPClient) sendRcptTo(client *smtp.Client, toEmail string) error {
	cleanTo := strings.Trim(strings.TrimSpace(toEmail), "<>")
	if client.Text != nil {
		id, err := client.Text.Cmd("RCPT TO:<%s>", cleanTo)
		if err == nil {
			client.Text.StartResponse(id)
			code, _, rErr := client.Text.ReadResponse(250)
			client.Text.EndResponse(id)
			if rErr == nil || code == 250 || code == 251 {
				return nil
			}
		}
	}
	return client.Rcpt(cleanTo)
}

// loginAuth 严格遵循 RFC 规范与 QQ 邮箱官方 AUTH LOGIN 交互状态机
type loginAuth struct {
	username, password string
	step               int
}

func LoginAuth(username, password string) smtp.Auth {
	return &loginAuth{username: username, password: password, step: 0}
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	a.step = 0
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if more {
		a.step++
		if a.step == 1 {
			return []byte(a.username), nil
		}
		if a.step == 2 {
			return []byte(a.password), nil
		}
	}
	return nil, nil
}
// foldHeaderLine 按照 RFC 5322 / RFC 5321 规范，将超长报头在 76 字符处安全折行 (CRLF + 空格)，确保每行 <= 78 字节
func foldHeaderLine(headerName, headerVal string) string {
	var buf strings.Builder
	buf.WriteString(headerName)
	buf.WriteString(": ")

	const maxLen = 76
	rem := headerVal
	first := true
	for len(rem) > 0 {
		if !first {
			buf.WriteString("\r\n ")
		}
		chunkLen := maxLen
		if len(rem) < chunkLen {
			chunkLen = len(rem)
		}
		buf.WriteString(rem[:chunkLen])
		rem = rem[chunkLen:]
		first = false
	}
	buf.WriteString("\r\n")
	return buf.String()
}

// lineWrappingWriter 按照 RFC 2045 (MIME Base64 单行最多 76 字符) 与 RFC 5321 (SMTP 单行严禁超过 998 字节) 规范，
// 在写入流时每达到 maxLen 字符自动插入 \r\n，杜绝 SMTP 邮件单行超长被腾讯等邮件中继服务器截断损坏
type lineWrappingWriter struct {
	w       io.Writer
	lineLen int
	maxLen  int
}

func newLineWrappingWriter(w io.Writer, maxLen int) *lineWrappingWriter {
	if maxLen <= 0 {
		maxLen = 76
	}
	return &lineWrappingWriter{w: w, maxLen: maxLen}
}

func (lw *lineWrappingWriter) Write(p []byte) (n int, err error) {
	total := len(p)
	for len(p) > 0 {
		avail := lw.maxLen - lw.lineLen
		if avail <= 0 {
			if _, err := lw.w.Write([]byte("\r\n")); err != nil {
				return n, err
			}
			lw.lineLen = 0
			avail = lw.maxLen
		}
		toWrite := len(p)
		if toWrite > avail {
			toWrite = avail
		}
		written, err := lw.w.Write(p[:toWrite])
		lw.lineLen += written
		n += written
		if err != nil {
			return n, err
		}
		p = p[toWrite:]
	}
	return total, nil
}
