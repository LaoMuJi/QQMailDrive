package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf16"
)

// IMAPClient 专用于 QQMailDrive 的轻量高效 IMAP 客户端（自带断线自愈重连机制与精确可取消进度流）
type IMAPClient struct {
	conn               *tls.Conn
	reader             *textproto.Reader
	writer             *bufio.Writer
	mu                 sync.Mutex
	interrupted        atomic.Bool
	dialCancel         context.CancelFunc
	dialMu             sync.Mutex
	tagCounter         uint64
	server             string
	email              string
	authCode           string
	folder             string
	selectedFolder     string // 方案 C：记录当前长连接在云端已处于 Selected 状态的文件夹，免除重复 SELECT 往返
	connected          bool
	lastActive         time.Time
	LastTTFB           time.Duration
	LastTransferDur    time.Duration
	LastThroughputMbps float64
	Logger             func(level LogLevel, module, msg string)
}

var ErrInterrupted = fmt.Errorf("底层连接已被调度器中断让位")

// Interrupt 强制从外部瞬间打断当前长连接的阻塞（无锁穿透，光速唤醒等待中的 Read，严禁二次重试）
func (c *IMAPClient) Interrupt() {
	if c == nil {
		return
	}
	c.interrupted.Store(true)
	c.dialMu.Lock()
	if c.dialCancel != nil {
		c.dialCancel()
		c.dialCancel = nil
	}
	c.dialMu.Unlock()

	conn := c.conn
	if conn != nil {
		_ = conn.SetDeadline(time.Now())
		_ = conn.Close()
	}
	c.mu.Lock()
	c.conn = nil
	c.connected = false
	c.selectedFolder = ""
	c.mu.Unlock()
}

// ResetInterrupt 重置中断状态，供长连接复用
func (c *IMAPClient) ResetInterrupt() {
	if c != nil {
		c.interrupted.Store(false)
	}
}

// IsInterrupted 检查当前长连接是否已被标记中断
func (c *IMAPClient) IsInterrupted() bool {
	if c == nil {
		return false
	}
	return c.interrupted.Load()
}

type MailHeaderSummary struct {
	SeqNum     uint32
	Subject    string
	Date       string
	XDriveMeta string
}

func NewIMAPClient(server, email, authCode string) *IMAPClient {
	return &IMAPClient{
		server:   server,
		email:    email,
		authCode: authCode,
	}
}

func (c *IMAPClient) log(level LogLevel, msg string) {
	if c.Logger != nil {
		c.Logger(level, "IMAP", msg)
	}
}

func (c *IMAPClient) nextTag() string {
	val := atomic.AddUint64(&c.tagCounter, 1)
	return fmt.Sprintf("QD%06d", val)
}

func (c *IMAPClient) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connectInternal()
}

func (c *IMAPClient) connectInternal() error {
	c.interrupted.Store(false)
	c.selectedFolder = ""
	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
	}
	if c.server == "" || c.email == "" || c.authCode == "" {
		return fmt.Errorf("邮箱配置未就绪")
	}

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 5*time.Second)
	c.dialMu.Lock()
	c.dialCancel = dialCancel
	c.dialMu.Unlock()
	defer func() {
		c.dialMu.Lock()
		c.dialCancel = nil
		c.dialMu.Unlock()
		dialCancel()
	}()

	c.log(LogLevelDebug, fmt.Sprintf("正在与 IMAP 服务器建立 TLS 长连接: %s...", c.server))

	tlsConfig := &tls.Config{
		ServerName: strings.Split(c.server, ":")[0],
	}

	tlsDialer := &tls.Dialer{
		NetDialer: &net.Dialer{Timeout: 5 * time.Second},
		Config:    tlsConfig,
	}

	connNet, err := tlsDialer.DialContext(dialCtx, "tcp", c.server)
	if err != nil {
		c.connected = false
		if c.interrupted.Load() || errors.Is(err, context.Canceled) {
			return ErrInterrupted
		}
		c.log(LogLevelError, fmt.Sprintf("连接 IMAP 服务器失败: %v", err))
		return fmt.Errorf("连接 IMAP 服务器失败: %w", err)
	}

	if c.interrupted.Load() {
		_ = connNet.Close()
		c.connected = false
		return ErrInterrupted
	}

	conn, ok := connNet.(*tls.Conn)
	if !ok {
		connNet.Close()
		return fmt.Errorf("无效的 TLS 连接类型")
	}

	c.conn = conn
	c.writer = bufio.NewWriterSize(conn, 512*1024)
	c.reader = textproto.NewReader(bufio.NewReaderSize(conn, 512*1024))

	if _, err := c.reader.ReadLine(); err != nil {
		c.conn.Close()
		c.connected = false
		c.log(LogLevelError, fmt.Sprintf("读取 IMAP 问候语失败: %v", err))
		return fmt.Errorf("读取 IMAP 问候语失败: %w", err)
	}

	tag := c.nextTag()
	cmd := fmt.Sprintf("%s LOGIN %s %s\r\n", tag, c.email, c.authCode)
	if _, err := c.writer.WriteString(cmd); err != nil {
		c.conn.Close()
		c.connected = false
		return err
	}
	if err := c.writer.Flush(); err != nil {
		c.conn.Close()
		c.connected = false
		return err
	}

	for {
		line, err := c.reader.ReadLine()
		if err != nil {
			c.conn.Close()
			c.connected = false
			c.log(LogLevelError, fmt.Sprintf("登录响应读取失败: %v", err))
			return fmt.Errorf("登录响应读取失败: %w", err)
		}
		if strings.HasPrefix(line, tag+" OK") {
			c.connected = true
			c.lastActive = time.Now()
			c.log(LogLevelInfo, fmt.Sprintf("✔ 成功建立并验证 IMAP 专属长连接 (账号: %s)", c.email))
			break
		} else if strings.HasPrefix(line, tag+" NO") || strings.HasPrefix(line, tag+" BAD") {
			c.conn.Close()
			c.connected = false
			c.log(LogLevelError, fmt.Sprintf("QQ 邮箱 IMAP 授权码登录失败: %s", line))
			return fmt.Errorf("QQ 邮箱 IMAP 授权码登录失败: %s", line)
		}
	}

	return nil
}

// ListMailboxes 通过 IMAP LIST 指令列出邮箱中所有文件夹名称
func (c *IMAPClient) ListMailboxes() ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil || !c.connected {
		if err := c.connectInternal(); err != nil {
			return nil, err
		}
	}

	tag := c.nextTag()
	cmd := fmt.Sprintf("%s LIST \"\" \"*\"\r\n", tag)
	if _, err := c.writer.WriteString(cmd); err != nil {
		return nil, err
	}
	_ = c.writer.Flush()

	var folders []string
	for {
		line, err := c.reader.ReadLine()
		if err != nil {
			return folders, err
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, tag+" OK") {
			break
		}
		if strings.HasPrefix(trimmed, tag+" NO") || strings.HasPrefix(trimmed, tag+" BAD") {
			return nil, fmt.Errorf("IMAP LIST 失败: %s", trimmed)
		}

		if strings.HasPrefix(trimmed, "* LIST") {
			fName := ""
			if strings.HasSuffix(trimmed, "\"") {
				lastQuote := len(trimmed) - 1
				prevQuote := strings.LastIndex(trimmed[:lastQuote], "\"")
				if prevQuote != -1 {
					fName = trimmed[prevQuote+1 : lastQuote]
				}
			}
			if fName == "" {
				parts := strings.Fields(trimmed)
				if len(parts) > 0 {
					fName = strings.Trim(parts[len(parts)-1], "\"")
				}
			}
			if fName != "" && fName != "/" {
				folders = append(folders, fName)
			}
		}
	}
	return folders, nil
}

// SelectFolderOnly 纯只读选定目标隔离空间目录，仅发送 SELECT，若不存在直接返回错误，绝不自动调用 CREATE（杜绝幽灵文件夹暗中新建）
func (c *IMAPClient) SelectFolderOnly(folder string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.selectFolderOnlyInternal(folder)
}

func (c *IMAPClient) selectFolderOnlyInternal(folder string) error {
	cleanFolder := decodeModifiedUTF7(folder)

	// 方案 C：连接复用免 SELECT 优化
	if c.conn != nil && c.connected && c.selectedFolder == cleanFolder {
		c.folder = cleanFolder
		return nil
	}

	globalSelectFolderMu.Lock()
	defer globalSelectFolderMu.Unlock()

	if c.conn != nil && c.connected && c.selectedFolder == cleanFolder {
		c.folder = cleanFolder
		return nil
	}

	c.folder = cleanFolder
	encodedFolder := encodeModifiedUTF7(cleanFolder)
	tag := c.nextTag()
	cmd := fmt.Sprintf("%s SELECT \"%s\"\r\n", tag, encodedFolder)
	if _, err := c.writer.WriteString(cmd); err != nil {
		c.selectedFolder = ""
		return err
	}
	_ = c.writer.Flush()

	for {
		line, err := c.reader.ReadLine()
		if err != nil {
			c.selectedFolder = ""
			return err
		}
		if strings.HasPrefix(line, tag+" OK") {
			c.selectedFolder = cleanFolder
			return nil
		} else if strings.HasPrefix(line, tag+" NO") {
			c.selectedFolder = ""
			return fmt.Errorf("隔离存储空间 [%s] 不存在: %s", cleanFolder, line)
		} else if strings.HasPrefix(line, tag+" BAD") {
			c.selectedFolder = ""
			return fmt.Errorf("选择隔离空间指令语法错误: %s", line)
		}
	}
}

func (c *IMAPClient) SelectFolder(folder string) error {
	return c.SelectFolderOnly(folder)
}

// encodeModifiedUTF7 按照 RFC 3501 将 UTF-8 字符串编码为 IMAP Modified UTF-7（如 "资料备份" -> "&jURlmVkHTv0-"）
// decodeModifiedUTF7 解码 RFC 3501 规定的 IMAP Modified UTF-7 邮箱名（如 &UXZO1mWHTvZZOQ- 解码为 "其他文件夹"）
// getIMAPBackoff 阶梯退避规则：100ms, 200ms, 500ms, 1000ms, 后续每次 5000ms
func getIMAPBackoff(attempt int) time.Duration {
	switch attempt {
	case 1:
		return 100 * time.Millisecond
	case 2:
		return 200 * time.Millisecond
	case 3:
		return 500 * time.Millisecond
	case 4:
		return 1000 * time.Millisecond
	default:
		return 5000 * time.Millisecond
	}
}

func decodeModifiedUTF7(s string) string {
	if !strings.Contains(s, "&") {
		return s
	}
	var buf strings.Builder
	for i := 0; i < len(s); {
		if s[i] == '&' {
			end := strings.IndexByte(s[i+1:], '-')
			if end == -1 {
				buf.WriteString(s[i:])
				break
			}
			end += i + 1
			if end == i+1 {
				buf.WriteByte('&')
				i = end + 1
				continue
			}
			part := s[i+1 : end]
			b64 := strings.ReplaceAll(part, ",", "/")
			pad := len(b64) % 4
			if pad != 0 {
				b64 += strings.Repeat("=", 4-pad)
			}
			raw, err := base64.StdEncoding.DecodeString(b64)
			if err != nil || len(raw)%2 != 0 {
				buf.WriteString(s[i : end+1])
			} else {
				u16 := make([]uint16, len(raw)/2)
				for j := 0; j < len(u16); j++ {
					u16[j] = uint16(raw[j*2])<<8 | uint16(raw[j*2+1])
				}
				buf.WriteString(string(utf16.Decode(u16)))
			}
			i = end + 1
		} else {
			buf.WriteByte(s[i])
			i++
		}
	}
	return buf.String()
}

func encodeModifiedUTF7(s string) string {
	var buf strings.Builder
	var nonAscii []rune

	flushNonAscii := func() {
		if len(nonAscii) == 0 {
			return
		}
		u16 := utf16.Encode(nonAscii)
		raw := make([]byte, len(u16)*2)
		for i, v := range u16 {
			raw[i*2] = byte(v >> 8)
			raw[i*2+1] = byte(v & 0xFF)
		}
		b64 := base64.StdEncoding.EncodeToString(raw)
		b64 = strings.ReplaceAll(b64, "/", ",")
		b64 = strings.TrimRight(b64, "=")
		buf.WriteByte('&')
		buf.WriteString(b64)
		buf.WriteByte('-')
		nonAscii = nil
	}

	for _, r := range s {
		if r >= 0x20 && r <= 0x7E {
			flushNonAscii()
			if r == '&' {
				buf.WriteString("&-")
			} else {
				buf.WriteRune(r)
			}
		} else {
			nonAscii = append(nonAscii, r)
		}
	}
	flushNonAscii()
	return buf.String()
}

func (c *IMAPClient) EnsureAndSelectFolder(folder string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ensureAndSelectFolderInternal(folder)
}

var globalSelectFolderMu sync.Mutex

func (c *IMAPClient) ensureAndSelectFolderInternal(folder string) error {
	cleanFolder := decodeModifiedUTF7(folder)

	// 方案 C：连接复用免 SELECT 优化
	// 若当前长连接状态就绪且已处于目标文件夹 Selected 状态，彻底免除 SELECT 网络往返与排队锁开销，0ms 瞬间就绪！
	if c.conn != nil && c.connected && c.selectedFolder == cleanFolder {
		c.folder = cleanFolder
		return nil
	}

	globalSelectFolderMu.Lock()
	defer globalSelectFolderMu.Unlock()

	// 锁内二次检查，避免多协程竞争
	if c.conn != nil && c.connected && c.selectedFolder == cleanFolder {
		c.folder = cleanFolder
		return nil
	}

	// 1. 若当前长连接不存在或未标记 connected，必须先重新建连！
	if c.conn == nil || !c.connected {
		if err := c.connectInternal(); err != nil {
			return err
		}
	}

	c.folder = cleanFolder
	encodedFolder := encodeModifiedUTF7(cleanFolder)
	tag := c.nextTag()
	cmd := fmt.Sprintf("%s SELECT \"%s\"\r\n", tag, encodedFolder)
	if _, err := c.writer.WriteString(cmd); err != nil {
		c.selectedFolder = ""
		c.connected = false
		c.conn = nil
		// 自动重连重试一次
		if rErr := c.connectInternal(); rErr == nil {
			tag = c.nextTag()
			cmd = fmt.Sprintf("%s SELECT \"%s\"\r\n", tag, encodedFolder)
			if _, wErr := c.writer.WriteString(cmd); wErr != nil {
				return wErr
			}
		} else {
			return err
		}
	}
	_ = c.writer.Flush()

	folderExists := false
	readErr := false
	for {
		line, err := c.reader.ReadLine()
		if err != nil {
			c.selectedFolder = ""
			c.connected = false
			c.conn = nil
			readErr = true
			break
		}
		if strings.HasPrefix(line, tag+" OK") {
			folderExists = true
			c.selectedFolder = cleanFolder
			break
		} else if strings.HasPrefix(line, tag+" NO") {
			folderExists = false
			c.selectedFolder = ""
			break
		}
	}

	if readErr {
		// 发生网络连接断开时，自动重连并重试一次 SELECT 指令
		if rErr := c.connectInternal(); rErr == nil {
			tag = c.nextTag()
			cmd = fmt.Sprintf("%s SELECT \"%s\"\r\n", tag, encodedFolder)
			if _, wErr := c.writer.WriteString(cmd); wErr == nil {
				_ = c.writer.Flush()
				for {
					line, rErr2 := c.reader.ReadLine()
					if rErr2 != nil {
						break
					}
					if strings.HasPrefix(line, tag+" OK") {
						folderExists = true
						c.selectedFolder = cleanFolder
						break
					} else if strings.HasPrefix(line, tag+" NO") {
						folderExists = false
						c.selectedFolder = ""
						break
					}
				}
			}
		} else {
			return fmt.Errorf("读取 SELECT 响应失败且自动重连失败")
		}
	}

	if !folderExists {
		createTag := c.nextTag()
		createCmd := fmt.Sprintf("%s CREATE \"%s\"\r\n", createTag, encodedFolder)
		if _, err := c.writer.WriteString(createCmd); err != nil {
			c.selectedFolder = ""
			return err
		}
		_ = c.writer.Flush()
		createSuccess := false
		for {
			line, err := c.reader.ReadLine()
			if err != nil {
				c.selectedFolder = ""
				return err
			}
			if strings.HasPrefix(line, createTag+" OK") {
				createSuccess = true
				break
			} else if strings.HasPrefix(line, createTag+" NO") {
				lower := strings.ToLower(line)
				if strings.Contains(lower, "already exists") || strings.Contains(lower, "alreadyexists") || strings.Contains(lower, "已存在") {
					createSuccess = true
					break
				}
				c.selectedFolder = ""
				return fmt.Errorf("创建存储文件夹失败: %s", line)
			} else if strings.HasPrefix(line, createTag+" BAD") {
				c.selectedFolder = ""
				return fmt.Errorf("创建存储文件夹语法错误: %s", line)
			}
		}

		if createSuccess {
			selTag := c.nextTag()
			if _, err := c.writer.WriteString(fmt.Sprintf("%s SELECT \"%s\"\r\n", selTag, encodedFolder)); err != nil {
				c.selectedFolder = ""
				return err
			}
			_ = c.writer.Flush()
			for {
				line, err := c.reader.ReadLine()
				if err != nil {
					c.selectedFolder = ""
					return err
				}
				if strings.HasPrefix(line, selTag+" OK") {
					c.selectedFolder = cleanFolder
					break
				} else if strings.HasPrefix(line, selTag+" NO") || strings.HasPrefix(line, selTag+" BAD") {
					c.selectedFolder = ""
					return fmt.Errorf("创建后选区失败: %s", line)
				}
			}
		}
	}
	return nil
}

func (c *IMAPClient) AppendDirectMessage(folder, subject, metaBase64 string, mimeData []byte) error {
	return c.AppendDirectMessageWithProgress(folder, subject, metaBase64, mimeData, nil)
}

func (c *IMAPClient) AppendDirectMessageWithProgress(folder, subject, metaBase64 string, mimeData []byte, progressCb func(int) bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	targetFolder := decodeModifiedUTF7(folder)
	if targetFolder == "" {
		targetFolder = GlobalConfig.StorageFolder
	}

	for attempt := 1; attempt <= 16; attempt++ {
		if c.conn == nil || !c.connected {
			if err := c.connectInternal(); err != nil {
				return err
			}
		}
		if err := c.ensureAndSelectFolderInternal(targetFolder); err != nil {
			c.log(LogLevelWarn, fmt.Sprintf("选区目标目录 [%s] 异常: %v，尝试重连自愈...", targetFolder, err))
			_ = c.connectInternal()
		}

		tag := c.nextTag()
		cmd := fmt.Sprintf("%s APPEND \"%s\" (\\Seen) {%d}\r\n", tag, encodeModifiedUTF7(targetFolder), len(mimeData))
		if _, err := c.writer.WriteString(cmd); err != nil {
			_ = c.connectInternal()
			continue
		}
		if err := c.writer.Flush(); err != nil {
			_ = c.connectInternal()
			continue
		}

		line, err := c.reader.ReadLine()
		if err != nil || !strings.HasPrefix(line, "+") {
			_ = c.connectInternal()
			continue
		}

		const bufSize = 512 * 1024
		totalLen := len(mimeData)
		written := 0
		cancelled := false

		for written < totalLen {
			toWrite := bufSize
			if totalLen-written < toWrite {
				toWrite = totalLen - written
			}
			n, wErr := c.writer.Write(mimeData[written : written+toWrite])
			if wErr != nil {
				break
			}
			written += n
			if progressCb != nil {
				if !progressCb(n) {
					cancelled = true
					break
				}
			}
		}

		if cancelled {
			_ = c.conn.Close()
			c.conn = nil
			c.connected = false
			c.log(LogLevelInfo, "✔ 上传任务已被用户主动取消，已立即切断网络发送长连接")
			return fmt.Errorf("上传任务已被用户取消")
		}

		if written < totalLen {
			_ = c.connectInternal()
			continue
		}

		if _, err := c.writer.WriteString("\r\n"); err != nil {
			_ = c.connectInternal()
			continue
		}
		if err := c.writer.Flush(); err != nil {
			_ = c.connectInternal()
			continue
		}

		appendSuccess := false
		for {
			resp, rErr := c.reader.ReadLine()
			if rErr != nil {
				break
			}
			if strings.HasPrefix(resp, tag+" OK") {
				appendSuccess = true
				c.selectedFolder = "" // 强制下次读取时重新 SELECT 刷新最新邮件快照
				c.lastActive = time.Now()
				break
			} else if strings.HasPrefix(resp, tag+" NO") || strings.HasPrefix(resp, tag+" BAD") {
				if strings.Contains(resp, "OVERQUOTA") {
					c.log(LogLevelError, fmt.Sprintf("❌ [邮箱存储超限] QQ 邮箱存储容量已满 (NO [OVERQUOTA]): 腾讯服务器拒绝写入新邮件，请前往 mail.qq.com 清理已删除/垃圾邮件或点击左下角免费升级容量: %s", resp))
				} else {
					c.log(LogLevelError, fmt.Sprintf("IMAP APPEND 拒绝: %s", resp))
				}
				return fmt.Errorf("IMAP APPEND 拒绝: %s", resp)
			}
		}

		if appendSuccess {
			return nil
		}

		if c.interrupted.Load() {
			return fmt.Errorf("写入已由用户主动取消")
		}
		if attempt < 16 {
			backoff := getIMAPBackoff(attempt)
			c.log(LogLevelWarn, fmt.Sprintf("写入分片到云端超时或断开，等待 %v 后执行第 %d/16 次阶梯自愈重试...", backoff, attempt))
			time.Sleep(backoff)
		}
		_ = c.connectInternal()
		if c.folder != "" {
			_ = c.ensureAndSelectFolderInternal(c.folder)
		}
	}

	return fmt.Errorf("追加邮件到 QQ 邮箱失败，已重试 16 次")
}

// AppendStreamMessageWithProgress 优化方案 2：零中间内存缓冲区，直接将加密分片数据 Base64 编码流式写入 TLS 套接字
func (c *IMAPClient) AppendStreamMessageWithProgress(folder, subject, metaBase64, seqFileName string, fileData, thumbBody []byte, progressCb func(int) bool) error {
	if c.interrupted.Load() {
		return ErrInterrupted
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	// 1. 构造标准 MIME 前置标头与结尾边界
	boundary := fmt.Sprintf("----_QQMailDrive_%d", time.Now().UnixNano())
	var hdrBuf bytes.Buffer
	hdrBuf.WriteString("From: " + c.email + "\r\n")
	hdrBuf.WriteString("To: " + c.email + "\r\n")
	hdrBuf.WriteString("Subject: " + subject + "\r\n")
	hdrBuf.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	hdrBuf.WriteString("X-Drive-Meta: " + metaBase64 + "\r\n")
	hdrBuf.WriteString("MIME-Version: 1.0\r\n")
	hdrBuf.WriteString("Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n\r\n")

	// Part 1: 若包含缩略图，将其作为邮件正文写入 (轻量加密数据 ~10KB)
	if len(thumbBody) > 0 {
		hdrBuf.WriteString("--" + boundary + "\r\n")
		hdrBuf.WriteString("Content-Type: text/plain; charset=\"utf-8\"\r\n")
		hdrBuf.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
		hdrBuf.WriteString(base64.StdEncoding.EncodeToString(thumbBody) + "\r\n")
	}

	// Part 2: 正常数据切片作为普通附件 (50MB)
	hdrBuf.WriteString("--" + boundary + "\r\n")
	hdrBuf.WriteString("Content-Type: application/octet-stream; name=\"" + seqFileName + "\"\r\n")
	hdrBuf.WriteString("Content-Disposition: attachment; filename=\"" + seqFileName + "\"\r\n")
	hdrBuf.WriteString("Content-Transfer-Encoding: base64\r\n\r\n")
	headerBytes := hdrBuf.Bytes()

	footerBytes := []byte("\r\n--" + boundary + "--\r\n")

	// 2. 毫厘不差精准预计算整个邮件 Literal 字节总数 (IMAP 铁律)
	b64Len := base64.StdEncoding.EncodedLen(len(fileData))
	totalMIMELen := int64(len(headerBytes)) + int64(b64Len) + int64(len(footerBytes))

	for attempt := 1; attempt <= 16; attempt++ {
		if c.interrupted.Load() {
			return ErrInterrupted
		}
		if c.conn == nil || !c.connected {
			if err := c.connectInternal(); err != nil {
				return err
			}
			if c.folder != "" {
				_ = c.ensureAndSelectFolderInternal(c.folder)
			}
		}

		targetFolder := decodeModifiedUTF7(folder)
		if targetFolder == "" {
			targetFolder = GlobalConfig.StorageFolder
		}
		if err := c.ensureAndSelectFolderInternal(targetFolder); err != nil {
			c.log(LogLevelWarn, fmt.Sprintf("选区目标目录 [%s] 异常: %v，尝试重连自愈...", targetFolder, err))
			_ = c.connectInternal()
		}

		tag := c.nextTag()
		cmd := fmt.Sprintf("%s APPEND \"%s\" (\\Seen) {%d}\r\n", tag, encodeModifiedUTF7(targetFolder), totalMIMELen)
		if _, err := c.writer.WriteString(cmd); err != nil {
			_ = c.connectInternal()
			continue
		}
		if err := c.writer.Flush(); err != nil {
			_ = c.connectInternal()
			continue
		}

		line, err := c.reader.ReadLine()
		if err != nil || !strings.HasPrefix(line, "+") {
			_ = c.connectInternal()
			continue
		}

		// 3. 步骤 A：发送前置报头
		if _, err := c.writer.Write(headerBytes); err != nil {
			_ = c.connectInternal()
			continue
		}

		// 4. 步骤 B：使用 base64.NewEncoder 流式直写，512KB 分批写入并上报进度
		b64Enc := base64.NewEncoder(base64.StdEncoding, c.writer)
		const streamChunkSize = 512 * 1024
		dataLen := len(fileData)
		written := 0
		cancelled := false
		streamErr := false

		for written < dataLen {
			toWrite := streamChunkSize
			if dataLen-written < toWrite {
				toWrite = dataLen - written
			}
			n, wErr := b64Enc.Write(fileData[written : written+toWrite])
			if wErr != nil {
				streamErr = true
				break
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
			_ = c.conn.Close()
			c.conn = nil
			c.connected = false
			c.log(LogLevelInfo, "✔ 上传任务已被用户主动取消，已立即切断网络发送长连接")
			return fmt.Errorf("上传任务已被用户取消")
		}

		if streamErr || written < dataLen {
			_ = c.connectInternal()
			continue
		}

		// 5. 必须在写尾部边界前 Close，把末尾的 '=' 填充符完整刷入 c.writer！
		if err := b64Enc.Close(); err != nil {
			_ = c.connectInternal()
			continue
		}

		// 6. 步骤 C：发送后置边界与 IMAP 换行
		if _, err := c.writer.Write(footerBytes); err != nil {
			_ = c.connectInternal()
			continue
		}

		if _, err := c.writer.WriteString("\r\n"); err != nil {
			_ = c.connectInternal()
			continue
		}
		if err := c.writer.Flush(); err != nil {
			_ = c.connectInternal()
			continue
		}

		// 7. 等待 OK 响应
		appendSuccess := false
		for {
			resp, rErr := c.reader.ReadLine()
			if rErr != nil {
				break
			}
			if strings.HasPrefix(resp, tag+" OK") {
				appendSuccess = true
				c.selectedFolder = "" // 强制下次读取时重新 SELECT 刷新最新邮件快照
				c.lastActive = time.Now()
				break
			} else if strings.HasPrefix(resp, tag+" NO") || strings.HasPrefix(resp, tag+" BAD") {
				if strings.Contains(resp, "OVERQUOTA") {
					c.log(LogLevelError, fmt.Sprintf("❌ [邮箱存储超限] QQ 邮箱存储容量已满 (NO [OVERQUOTA]): 腾讯服务器拒绝写入新邮件，请前往 mail.qq.com 清理已删除/垃圾邮件或点击左下角免费升级容量: %s", resp))
				} else {
					c.log(LogLevelError, fmt.Sprintf("IMAP APPEND 拒绝: %s", resp))
				}
				return fmt.Errorf("IMAP APPEND 拒绝: %s", resp)
			}
		}

		if appendSuccess {
			return nil
		}

		if c.interrupted.Load() {
			return fmt.Errorf("写入已由用户主动取消")
		}
		if attempt < 16 {
			backoff := getIMAPBackoff(attempt)
			c.log(LogLevelWarn, fmt.Sprintf("流式写入分片到云端超时或断开，等待 %v 后执行第 %d/16 次阶梯自愈重试...", backoff, attempt))
			time.Sleep(backoff)
		}
		_ = c.connectInternal()
		if c.folder != "" {
			_ = c.ensureAndSelectFolderInternal(c.folder)
		}
	}

	return fmt.Errorf("流式追加邮件到 QQ 邮箱失败，已重试 16 次")
}

// FetchAllHeadersFast 高性能拉取所有邮件的序号、主题、日期与自定义元数据报头
func (c *IMAPClient) FetchAllHeadersFast() ([]MailHeaderSummary, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil || !c.connected {
		if err := c.connectInternal(); err != nil {
			return nil, err
		}
		if c.folder != "" {
			_ = c.ensureAndSelectFolderInternal(c.folder)
		}
	}

	if c.folder == "" && GlobalConfig.StorageFolder != "" {
		c.folder = GlobalConfig.StorageFolder
		_ = c.ensureAndSelectFolderInternal(GlobalConfig.StorageFolder)
	}

	tag := c.nextTag()
	cmd := fmt.Sprintf("%s FETCH 1:* (BODY.PEEK[HEADER.FIELDS (SUBJECT DATE X-DRIVE-META)])\r\n", tag)
	if _, err := c.writer.WriteString(cmd); err != nil {
		return nil, err
	}
	if err := c.writer.Flush(); err != nil {
		return nil, err
	}

	var summaries []MailHeaderSummary
	var currentSummary *MailHeaderSummary
	var lastField string

	for {
		line, err := c.reader.ReadLine()
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "* ") && strings.Contains(trimmed, "FETCH") {
			if currentSummary != nil {
				summaries = append(summaries, *currentSummary)
				currentSummary = nil
			}
			fields := strings.Fields(trimmed)
			if len(fields) >= 2 {
				if seq, err := strconv.ParseUint(fields[1], 10, 32); err == nil {
					currentSummary = &MailHeaderSummary{SeqNum: uint32(seq)}
				}
			}
			lastField = ""
		} else if currentSummary != nil {
			if trimmed == ")" {
				summaries = append(summaries, *currentSummary)
				currentSummary = nil
				lastField = ""
			} else if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				// RFC 2822 header folding continuation
				continuation := strings.TrimSpace(line)
				if lastField == "x-drive-meta" {
					currentSummary.XDriveMeta += continuation
				} else if lastField == "subject" {
					currentSummary.Subject += " " + continuation
				}
			} else {
				lower := strings.ToLower(line)
				if strings.HasPrefix(lower, "subject:") {
					currentSummary.Subject = strings.TrimSpace(line[8:])
					lastField = "subject"
				} else if strings.HasPrefix(lower, "date:") {
					currentSummary.Date = strings.TrimSpace(line[5:])
					lastField = "date"
				} else if strings.HasPrefix(lower, "x-drive-meta:") {
					currentSummary.XDriveMeta = strings.TrimSpace(line[13:])
					lastField = "x-drive-meta"
				} else {
					lastField = ""
				}
			}
		}

		if strings.HasPrefix(line, tag+" OK") {
			if currentSummary != nil {
				summaries = append(summaries, *currentSummary)
				currentSummary = nil
			}
			break
		} else if strings.HasPrefix(line, tag+" NO") || strings.HasPrefix(line, tag+" BAD") {
			if currentSummary != nil {
				summaries = append(summaries, *currentSummary)
				currentSummary = nil
			}
			break
		}
	}

	return summaries, nil
}

// SearchMessageSeqs 执行 IMAP SEARCH 指令并返回匹配邮件的序号列表
func (c *IMAPClient) SearchMessageSeqs(criteria string) ([]uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil || !c.connected {
		if err := c.connectInternal(); err != nil {
			return nil, err
		}
		if c.folder != "" {
			_ = c.ensureAndSelectFolderInternal(c.folder)
		}
	}

	if c.folder == "" && GlobalConfig.StorageFolder != "" {
		c.folder = GlobalConfig.StorageFolder
		_ = c.ensureAndSelectFolderInternal(GlobalConfig.StorageFolder)
	}

	tag := c.nextTag()
	cmd := fmt.Sprintf("%s SEARCH %s\r\n", tag, criteria)
	if _, err := c.writer.WriteString(cmd); err != nil {
		return nil, err
	}
	if err := c.writer.Flush(); err != nil {
		return nil, err
	}

	var seqs []uint32
	for {
		line, err := c.reader.ReadLine()
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "* SEARCH") {
			parts := strings.Fields(trimmed)
			for i := 2; i < len(parts); i++ {
				if seq, sErr := strconv.ParseUint(parts[i], 10, 32); sErr == nil {
					seqs = append(seqs, uint32(seq))
				}
			}
		}
		if strings.HasPrefix(trimmed, tag+" OK") {
			break
		}
		if strings.HasPrefix(trimmed, tag+" NO") || strings.HasPrefix(trimmed, tag+" BAD") {
			return seqs, fmt.Errorf("IMAP SEARCH 失败: %s", trimmed)
		}
	}

	return seqs, nil
}

// FetchRawMessageBody 下载单封邮件完整数据流 (带网络重连自愈与流式长包读取)
func (c *IMAPClient) FetchRawMessageBody(seq uint32) ([]byte, error) {
	return c.FetchRawMessageBodyWithProgress(seq, nil)
}

func (c *IMAPClient) FetchRawMessageBodyWithProgress(seq uint32, progressCb func(int) bool) ([]byte, error) {
	if c.interrupted.Load() {
		return nil, ErrInterrupted
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	for attempt := 1; attempt <= 16; attempt++ {
		if c.interrupted.Load() {
			return nil, ErrInterrupted
		}
		if c.conn == nil || !c.connected {
			if err := c.connectInternal(); err != nil {
				return nil, err
			}
			if c.folder != "" {
				selStart := time.Now()
				_ = c.ensureAndSelectFolderInternal(c.folder)
				c.log(LogLevelDebug, fmt.Sprintf("🔍 [连接探针] 云端邮箱选区完成 (耗时: %.2fs, 目录: %s)", time.Since(selStart).Seconds(), c.folder))
			}
		}

		if c.folder == "" && GlobalConfig.StorageFolder != "" {
			c.folder = GlobalConfig.StorageFolder
			_ = c.ensureAndSelectFolderInternal(c.folder)
		}

		cmdSentTime := time.Now()
		tag := c.nextTag()
		cmd := fmt.Sprintf("%s FETCH %d BODY.PEEK[]\r\n", tag, seq)
		if _, err := c.writer.WriteString(cmd); err != nil {
			if rErr := c.connectInternal(); rErr == nil && c.folder != "" {
				_ = c.ensureAndSelectFolderInternal(c.folder)
			}
			continue
		}
		if err := c.writer.Flush(); err != nil {
			if rErr := c.connectInternal(); rErr == nil && c.folder != "" {
				_ = c.ensureAndSelectFolderInternal(c.folder)
			}
			continue
		}

		var data []byte
		fetchErr := false

		for {
			line, err := c.reader.ReadLine()
			if err != nil {
				fetchErr = true
				break
			}
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, tag+" OK") {
				c.lastActive = time.Now()
				break
			}
			if strings.HasPrefix(trimmed, tag+" NO") || strings.HasPrefix(trimmed, tag+" BAD") {
				if strings.Contains(trimmed, "No mailbox selected") && attempt < 3 {
					c.selectedFolder = ""
					if c.folder == "" {
						c.folder = GlobalConfig.StorageFolder
					}
					_ = c.ensureAndSelectFolderInternal(c.folder)
					fetchErr = true
					break
				}
				c.log(LogLevelError, fmt.Sprintf("IMAP FETCH 拒绝: %s", trimmed))
				return nil, fmt.Errorf("IMAP FETCH 拒绝: %s", trimmed)
			}

			lIdx := strings.LastIndex(trimmed, "{")
			rIdx := strings.LastIndex(trimmed, "}")
			if lIdx != -1 && rIdx > lIdx {
				sizeStr := strings.TrimSpace(trimmed[lIdx+1 : rIdx])
				if sz, err := strconv.Atoi(sizeStr); err == nil && sz > 0 {
					ttfb := time.Since(cmdSentTime)
					transferStart := time.Now()

					data = make([]byte, sz)
					const bufSize = 512 * 1024
					readTotal := 0
					cancelled := false

					for readTotal < sz {
						if c.interrupted.Load() {
							cancelled = true
							break
						}
						toRead := bufSize
						if sz-readTotal < toRead {
							toRead = sz - readTotal
						}
						n, rErr := io.ReadFull(c.reader.R, data[readTotal : readTotal+toRead])
						if n > 0 {
							readTotal += n
							if progressCb != nil {
								// 检查回调函数返回结果，若为 false 代表外部已取消该任务
								if !progressCb(n) {
									cancelled = true
									break
								}
							}
						}
						if rErr != nil {
							fetchErr = true
							break
						}
					}

					if cancelled || c.interrupted.Load() {
						// 立即主动关闭底层 TLS 连接，彻底熔断网络流量
						if c.conn != nil {
							_ = c.conn.Close()
							c.conn = nil
						}
						c.connected = false
						c.log(LogLevelInfo, fmt.Sprintf("✔ 分片邮件 (Seq #%d) 连接已由调度器平滑中断让位", seq))
						return nil, ErrInterrupted
					}

					transferDur := time.Since(transferStart)
					mbps := 0.0
					if transferDur.Seconds() > 0 {
						mbps = (float64(sz*8) / (1024 * 1024)) / transferDur.Seconds()
					}
					c.LastTTFB = ttfb
					c.LastTransferDur = transferDur
					c.LastThroughputMbps = mbps
				}
			}
		}

		if !fetchErr {
			if len(data) > 0 {
				return data, nil
			}
			return nil, fmt.Errorf("邮件 #%d 内容为空或已不存在", seq)
		}

		if c.interrupted.Load() {
			return nil, ErrInterrupted
		}
		if attempt < 16 {
			backoff := getIMAPBackoff(attempt)
			c.log(LogLevelWarn, fmt.Sprintf("拉取邮件 #%d 遇到网络波动，等待 %v 后执行第 %d/16 次阶梯自愈重试...", seq, backoff, attempt))
			time.Sleep(backoff)
		}
		_ = c.connectInternal()
		if c.folder != "" {
			_ = c.ensureAndSelectFolderInternal(c.folder)
		}
	}

	return nil, fmt.Errorf("拉取邮件序号 #%d 内容为空或连接中断，已重试 16 次", seq)
}

// DeleteAndExpunge 标记删除并彻底物理清空
func (c *IMAPClient) DeleteAndExpunge(seqs []uint32) error {
	if len(seqs) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.folder != "" {
		_ = c.ensureAndSelectFolderInternal(c.folder)
	}

	seen := make(map[uint32]bool)
	var seqStrs []string
	for _, s := range seqs {
		if !seen[s] && s > 0 {
			seen[s] = true
			seqStrs = append(seqStrs, strconv.Itoa(int(s)))
		}
	}
	if len(seqStrs) == 0 {
		return nil
	}
	seqRange := strings.Join(seqStrs, ",")

	tag := c.nextTag()
	cmd := fmt.Sprintf("%s STORE %s +FLAGS (\\Deleted)\r\n", tag, seqRange)
	if _, err := c.writer.WriteString(cmd); err != nil {
		return err
	}
	_ = c.writer.Flush()
	for {
		line, err := c.reader.ReadLine()
		if err != nil {
			return err
		}
		if strings.HasPrefix(line, tag+" OK") || strings.HasPrefix(line, tag+" NO") || strings.HasPrefix(line, tag+" BAD") {
			break
		}
	}

	expTag := c.nextTag()
	if _, err := c.writer.WriteString(fmt.Sprintf("%s EXPUNGE\r\n", expTag)); err != nil {
		return err
	}
	_ = c.writer.Flush()
	for {
		line, err := c.reader.ReadLine()
		if err != nil {
			return err
		}
		if strings.HasPrefix(line, expTag+" OK") || strings.HasPrefix(line, expTag+" NO") || strings.HasPrefix(line, expTag+" BAD") {
			break
		}
	}

	c.log(LogLevelInfo, fmt.Sprintf("✔ 成功从云端彻底物理清除 (EXPUNGE) %d 封冗余邮件", len(seqStrs)))
	return nil
}

// Noop 保活心跳
func (c *IMAPClient) Noop() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	tag := c.nextTag()
	if _, err := c.writer.WriteString(fmt.Sprintf("%s NOOP\r\n", tag)); err != nil {
		return err
	}
	_ = c.writer.Flush()
	for {
		line, err := c.reader.ReadLine()
		if err != nil {
			return err
		}
		if strings.HasPrefix(line, tag+" OK") {
			return nil
		}
	}
}

func (c *IMAPClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		tag := c.nextTag()
		_, _ = c.writer.WriteString(fmt.Sprintf("%s LOGOUT\r\n", tag))
		_ = c.writer.Flush()
		_ = c.conn.Close()
		c.conn = nil
	}
	c.connected = false
	c.selectedFolder = ""
}


// ListQMDFolders 纯粹且严格地只列出带有 QMD_ 前缀的隔离网盘目录
func (c *IMAPClient) ListQMDFolders() ([]string, error) {
	allFolders, err := c.ListMailboxes()
	if err != nil {
		return nil, err
	}
	var qmdFolders []string
	for _, f := range allFolders {
		if IsQMDFolder(f) {
			qmdFolders = append(qmdFolders, f)
		}
	}
	return qmdFolders, nil
}

// RenameFolder 重命名云端隔离文件夹 (先探查云端真实路径，释放占用锁后执行 RENAME)
// FindActualMailboxName 自动在云端全盘文件夹中模糊匹配找到真实的完整路径 (例如自动匹配 其他文件夹/QMD_A01)
func (c *IMAPClient) FindActualMailboxName(folder string) string {
	all, _ := c.ListMailboxes()
	targetClean := decodeModifiedUTF7(folder)
	targetClean = strings.TrimPrefix(targetClean, "其他文件夹/")
	targetClean = strings.TrimPrefix(targetClean, "/")
	targetCanonical := ToCanonicalFolder(targetClean)

	for _, m := range all {
		cm := decodeModifiedUTF7(m)
		trimmed := strings.TrimPrefix(cm, "其他文件夹/")
		trimmed = strings.TrimPrefix(trimmed, "/")
		if trimmed == targetCanonical || cm == targetCanonical || trimmed == targetClean {
			return cm
		}
	}
	return targetCanonical
}

func (c *IMAPClient) RenameFolder(oldName, newName string) error {
	allFolders, _ := c.ListMailboxes()
	cleanOldTarget := decodeModifiedUTF7(oldName)
	cleanOldTarget = strings.TrimPrefix(cleanOldTarget, "其他文件夹/")
	cleanOldTarget = strings.TrimPrefix(cleanOldTarget, "/")

	actualOld := cleanOldTarget
	prefixDir := ""
	for _, f := range allFolders {
		cf := decodeModifiedUTF7(f)
		trimmed := strings.TrimPrefix(cf, "其他文件夹/")
		trimmed = strings.TrimPrefix(trimmed, "/")
		if trimmed == cleanOldTarget {
			actualOld = cf
			if strings.HasPrefix(cf, "其他文件夹/") {
				prefixDir = "其他文件夹/"
			}
			break
		}
	}

	cleanNewTarget := decodeModifiedUTF7(newName)
	cleanNewTarget = strings.TrimPrefix(cleanNewTarget, "其他文件夹/")
	cleanNewTarget = strings.TrimPrefix(cleanNewTarget, "/")
	actualNew := prefixDir + cleanNewTarget

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil || !c.connected {
		if err := c.connectInternal(); err != nil {
			return err
		}
	}

	// 先切到 INBOX 释放可能存在的文件夹选中锁
	_ = c.ensureAndSelectFolderInternal("INBOX")

	encOld := encodeModifiedUTF7(actualOld)
	encNew := encodeModifiedUTF7(actualNew)
	tag := c.nextTag()
	cmd := fmt.Sprintf("%s RENAME \"%s\" \"%s\"\r\n", tag, encOld, encNew)
	if _, err := c.writer.WriteString(cmd); err != nil {
		return err
	}
	_ = c.writer.Flush()

	for {
		line, err := c.reader.ReadLine()
		if err != nil {
			return err
		}
		if strings.HasPrefix(line, tag+" OK") {
			c.selectedFolder = ""
			c.log(LogLevelInfo, fmt.Sprintf("✔ 成功将云端目录重命名: %s -> %s", actualOld, actualNew))
			return nil
		}
		if strings.HasPrefix(line, tag+" NO") || strings.HasPrefix(line, tag+" BAD") {
			return fmt.Errorf("重命名目录失败: %s", line)
		}
	}
}

// DeleteFolder 彻底删除云端隔离文件夹 (自动清空内部邮件 + 释放占用锁 + 物理删除目录)
func (c *IMAPClient) DeleteFolder(folderName string) error {
	allFolders, _ := c.ListMailboxes()
	cleanTarget := decodeModifiedUTF7(folderName)
	cleanTarget = strings.TrimPrefix(cleanTarget, "其他文件夹/")
	cleanTarget = strings.TrimPrefix(cleanTarget, "/")

	actualFolder := cleanTarget
	for _, f := range allFolders {
		cf := decodeModifiedUTF7(f)
		trimmed := strings.TrimPrefix(cf, "其他文件夹/")
		trimmed = strings.TrimPrefix(trimmed, "/")
		if trimmed == cleanTarget {
			actualFolder = cf
			break
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil || !c.connected {
		if err := c.connectInternal(); err != nil {
			return err
		}
	}

	encFolder := encodeModifiedUTF7(actualFolder)

	// 第 1 步：选中目标文件夹并清空内部所有邮件 (防止 QQ 邮箱因文件夹非空拒绝删除)
	selTag := c.nextTag()
	if _, err := c.writer.WriteString(fmt.Sprintf("%s SELECT \"%s\"\r\n", selTag, encFolder)); err == nil {
		_ = c.writer.Flush()
		selOK := false
		for {
			line, rErr := c.reader.ReadLine()
			if rErr != nil {
				break
			}
			if strings.HasPrefix(line, selTag+" OK") {
				selOK = true
				break
			}
			if strings.HasPrefix(line, selTag+" NO") || strings.HasPrefix(line, selTag+" BAD") {
				break
			}
		}
		if selOK {
			storeTag := c.nextTag()
			_, _ = c.writer.WriteString(fmt.Sprintf("%s STORE 1:* +FLAGS (\\Deleted)\r\n", storeTag))
			_ = c.writer.Flush()
			for {
				line, rErr := c.reader.ReadLine()
				if rErr != nil || strings.HasPrefix(line, storeTag+" OK") || strings.HasPrefix(line, storeTag+" NO") || strings.HasPrefix(line, storeTag+" BAD") {
					break
				}
			}
			expTag := c.nextTag()
			_, _ = c.writer.WriteString(fmt.Sprintf("%s EXPUNGE\r\n", expTag))
			_ = c.writer.Flush()
			for {
				line, rErr := c.reader.ReadLine()
				if rErr != nil || strings.HasPrefix(line, expTag+" OK") || strings.HasPrefix(line, expTag+" NO") || strings.HasPrefix(line, expTag+" BAD") {
					break
				}
			}
		}
	}

	// 第 2 步：关键！切换到 INBOX 彻底释放对目标文件夹的占用锁 (IMAP 严禁删除当前处于 Selected 状态的文件夹)
	unSelTag := c.nextTag()
	_, _ = c.writer.WriteString(fmt.Sprintf("%s SELECT \"INBOX\"\r\n", unSelTag))
	_ = c.writer.Flush()
	for {
		line, rErr := c.reader.ReadLine()
		if rErr != nil || strings.HasPrefix(line, unSelTag+" OK") || strings.HasPrefix(line, unSelTag+" NO") || strings.HasPrefix(line, unSelTag+" BAD") {
			break
		}
	}
	c.selectedFolder = "INBOX"

	// 第 3 步：执行物理 DELETE
	tag := c.nextTag()
	cmd := fmt.Sprintf("%s DELETE \"%s\"\r\n", tag, encFolder)
	if _, err := c.writer.WriteString(cmd); err != nil {
		return err
	}
	_ = c.writer.Flush()

	for {
		line, err := c.reader.ReadLine()
		if err != nil {
			return err
		}
		if strings.HasPrefix(line, tag+" OK") {
			c.selectedFolder = ""
			c.log(LogLevelInfo, fmt.Sprintf("✔ 成功清空并删除云端隔离空间文件夹: %s", actualFolder))
			return nil
		}
		if strings.HasPrefix(line, tag+" NO") || strings.HasPrefix(line, tag+" BAD") {
			return fmt.Errorf("删除隔离空间文件夹失败: %s", line)
		}
	}
}

// CopyMessages 将选区内的多封邮件批量原子复制至目标文件夹 (利用云端原生 IMAP COPY，原邮件完整保留)
func (c *IMAPClient) CopyMessages(seqs []uint32, fromFolder, toFolder string) error {
	if len(seqs) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.ensureAndSelectFolderInternal(fromFolder); err != nil {
		return fmt.Errorf("选区原文件夹失败: %w", err)
	}

	seen := make(map[uint32]bool)
	var seqStrs []string
	for _, s := range seqs {
		if !seen[s] && s > 0 {
			seen[s] = true
			seqStrs = append(seqStrs, strconv.Itoa(int(s)))
		}
	}
	if len(seqStrs) == 0 {
		return nil
	}
	seqRange := strings.Join(seqStrs, ",")
	encTo := encodeModifiedUTF7(decodeModifiedUTF7(toFolder))

	copyTag := c.nextTag()
	copyCmd := fmt.Sprintf("%s COPY %s \"%s\"\r\n", copyTag, seqRange, encTo)
	if _, err := c.writer.WriteString(copyCmd); err != nil {
		return err
	}
	_ = c.writer.Flush()
	for {
		line, err := c.reader.ReadLine()
		if err != nil {
			return err
		}
		if strings.HasPrefix(line, copyTag+" OK") {
			c.log(LogLevelInfo, fmt.Sprintf("✔ 成功将 %d 封邮件通过 COPY 克隆至 [%s]", len(seqStrs), toFolder))
			return nil
		}
		if strings.HasPrefix(line, copyTag+" NO") || strings.HasPrefix(line, copyTag+" BAD") {
			return fmt.Errorf("COPY 邮件至目标文件夹失败: %s", line)
		}
	}
}

// MoveMessages 将选区内的多封邮件批量移动至目标文件夹 (支持 MOVE 指令，若服务器不支持则平滑降级为 COPY + STORE \Deleted + EXPUNGE)
func (c *IMAPClient) MoveMessages(seqs []uint32, fromFolder, toFolder string) error {
	if len(seqs) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.ensureAndSelectFolderInternal(fromFolder); err != nil {
		return fmt.Errorf("选区原文件夹失败: %w", err)
	}

	seen := make(map[uint32]bool)
	var seqStrs []string
	for _, s := range seqs {
		if !seen[s] && s > 0 {
			seen[s] = true
			seqStrs = append(seqStrs, strconv.Itoa(int(s)))
		}
	}
	if len(seqStrs) == 0 {
		return nil
	}
	seqRange := strings.Join(seqStrs, ",")
	encTo := encodeModifiedUTF7(decodeModifiedUTF7(toFolder))

	// 尝试执行原生的 IMAP MOVE 指令 (RFC 6851，QQ 邮箱支持)
	moveTag := c.nextTag()
	moveCmd := fmt.Sprintf("%s MOVE %s \"%s\"\r\n", moveTag, seqRange, encTo)
	if _, err := c.writer.WriteString(moveCmd); err == nil {
		_ = c.writer.Flush()
		moveSuccess := false
		for {
			line, rErr := c.reader.ReadLine()
			if rErr != nil {
				break
			}
			if strings.HasPrefix(line, moveTag+" OK") {
				moveSuccess = true
				break
			}
			if strings.HasPrefix(line, moveTag+" NO") || strings.HasPrefix(line, moveTag+" BAD") {
				break
			}
		}
		if moveSuccess {
			c.log(LogLevelInfo, fmt.Sprintf("✔ 成功将 %d 封邮件通过 MOVE 移至 [%s]", len(seqStrs), toFolder))
			return nil
		}
	}

	// 降级模式：COPY + 标记删除 + EXPUNGE
	copyTag := c.nextTag()
	copyCmd := fmt.Sprintf("%s COPY %s \"%s\"\r\n", copyTag, seqRange, encTo)
	if _, err := c.writer.WriteString(copyCmd); err != nil {
		return err
	}
	_ = c.writer.Flush()
	for {
		line, err := c.reader.ReadLine()
		if err != nil {
			return err
		}
		if strings.HasPrefix(line, copyTag+" OK") {
			break
		}
		if strings.HasPrefix(line, copyTag+" NO") || strings.HasPrefix(line, copyTag+" BAD") {
			return fmt.Errorf("COPY 邮件至目标文件夹失败: %s", line)
		}
	}

	// 标记原邮件删除并物理清除
	delTag := c.nextTag()
	if _, err := c.writer.WriteString(fmt.Sprintf("%s STORE %s +FLAGS (\\Deleted)\r\n", delTag, seqRange)); err == nil {
		_ = c.writer.Flush()
		for {
			line, err := c.reader.ReadLine()
			if err != nil || strings.HasPrefix(line, delTag+" OK") || strings.HasPrefix(line, delTag+" NO") || strings.HasPrefix(line, delTag+" BAD") {
				break
			}
		}
	}

	expTag := c.nextTag()
	if _, err := c.writer.WriteString(fmt.Sprintf("%s EXPUNGE\r\n", expTag)); err == nil {
		_ = c.writer.Flush()
		for {
			line, err := c.reader.ReadLine()
			if err != nil || strings.HasPrefix(line, expTag+" OK") || strings.HasPrefix(line, expTag+" NO") || strings.HasPrefix(line, expTag+" BAD") {
				break
			}
		}
	}

	c.log(LogLevelInfo, fmt.Sprintf("✔ 成功将 %d 封邮件转移至 [%s] 并物理清理原件", len(seqStrs), toFolder))
	return nil
}

// FetchMessagePart 精准按部件拉取邮件数据 (如 part="1" 仅拉取 10KB 缩略图正文，完全不读 50MB 附件)
func (c *IMAPClient) FetchMessagePart(seq uint32, part string) ([]byte, error) {
	if c.interrupted.Load() {
		return nil, ErrInterrupted
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	for attempt := 1; attempt <= 16; attempt++ {
		if c.interrupted.Load() {
			return nil, ErrInterrupted
		}
		if c.conn == nil || !c.connected {
			if err := c.connectInternal(); err != nil {
				return nil, err
			}
			if c.folder != "" {
				_ = c.ensureAndSelectFolderInternal(c.folder)
			}
		}

		if c.folder == "" && GlobalConfig.StorageFolder != "" {
			c.folder = GlobalConfig.StorageFolder
			_ = c.ensureAndSelectFolderInternal(c.folder)
		}

		tag := c.nextTag()
		cmd := fmt.Sprintf("%s FETCH %d BODY.PEEK[%s]\r\n", tag, seq, part)
		if _, err := c.writer.WriteString(cmd); err != nil {
			_ = c.connectInternal()
			continue
		}
		if err := c.writer.Flush(); err != nil {
			_ = c.connectInternal()
			continue
		}

		var data []byte
		fetchErr := false

		for {
			line, err := c.reader.ReadLine()
			if err != nil {
				fetchErr = true
				break
			}
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, tag+" OK") {
				c.lastActive = time.Now()
				break
			}
			if strings.HasPrefix(trimmed, tag+" NO") || strings.HasPrefix(trimmed, tag+" BAD") {
				return nil, fmt.Errorf("IMAP FETCH 部件拒绝: %s", trimmed)
			}

			lIdx := strings.LastIndex(trimmed, "{")
			rIdx := strings.LastIndex(trimmed, "}")
			if lIdx != -1 && rIdx > lIdx {
				sizeStr := strings.TrimSpace(trimmed[lIdx+1 : rIdx])
				if sz, err := strconv.Atoi(sizeStr); err == nil && sz > 0 {
					data = make([]byte, sz)
					_, rErr := io.ReadFull(c.reader.R, data)
					if rErr != nil {
						fetchErr = true
						break
					}
				}
			}
		}

		if !fetchErr {
			if len(data) > 0 {
				return data, nil
			}
			return nil, fmt.Errorf("邮件部件 (seq=%d, part=%s) 内容为空或不存在", seq, part)
		}
		if attempt < 16 {
			backoff := getIMAPBackoff(attempt)
			c.log(LogLevelWarn, fmt.Sprintf("拉取邮件部件 (seq=%d, part=%s) 遇到网络波动，等待 %v 后执行第 %d/16 次阶梯自愈重试...", seq, part, backoff, attempt))
			time.Sleep(backoff)
		}
		_ = c.connectInternal()
		if c.folder != "" {
			_ = c.ensureAndSelectFolderInternal(c.folder)
		}
	}
	return nil, fmt.Errorf("未能拉取邮件部件数据 (seq=%d, part=%s)，已重试 16 次", seq, part)
}
