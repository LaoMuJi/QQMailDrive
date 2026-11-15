package main

import (
	"context"
	"errors"
	"archive/zip"
	"compress/flate"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/textproto"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	SeqLen     = 10
	MagicBytes          = "QQDRV01" // 7 字节大文件数据切片魔法头
	MagicThumbnailBytes = "QQTHM01" // 7 字节缩略图魔法头 (与大文件切片绝对物理隔离)
)

var syncIndexMu sync.Mutex
var uploadSem = make(chan struct{}, 1) // 串行防风控信号量：平滑顺序上传，杜绝 QQ 邮箱高频并发掐断 TCP (EOF)

// StorageEngine 核心存储与切片加密引擎
type StorageEngine struct {
	imapClient   *IMAPClient
	smtpClient   *SMTPClient
	masterIndex  MasterIndex
	indexLock    sync.RWMutex
	tasks        map[string]*TransferTask
	tasksLock    sync.RWMutex
	cipherKey    []byte // 32字节 AES-256 主密钥
	isProcessing bool
	globalLogs     []string
	structuredLogs []LogEntry
	logsLock       sync.RWMutex
	thumbCache   map[string]string // Key: fileID, Value: thumbBase64 (模式 A 独立缩略图缓存池)
	thumbLock    sync.RWMutex

	// 多线程下载常驻连接池 (避免每个分片重复 TLS 握手与腾讯登录风控)
	downloadPool []*IMAPClient
	poolMu       sync.Mutex

	// 任务取消句柄池
	cancelTokens map[string]context.CancelFunc
	cancelMu     sync.Mutex

	// 任务拖拽寻道极速打断回调池
	seekInterrupters map[string]func(int)
	seekMu           sync.Mutex

	// 核心优化 5：主索引防抖提交与合并定时器
	indexDebounceTimer *time.Timer
	debounceMu         sync.Mutex

	// 核心优化 6：全系统最近业务活动时间戳 (用于 30 分钟深度空闲自动缩容)
	lastActivity       time.Time

	// 多隔离空间内存索引缓存（支持毫秒级切换与移动/复制弹窗秒开）
	folderIndexCache map[string]MasterIndex
	folderCacheRev   int64
	folderCacheMu    sync.RWMutex
	prewarmMu        sync.Mutex
	prewarmActive    bool
}

var GlobalEngine *StorageEngine

func NewStorageEngine(client *IMAPClient) *StorageEngine {
	hasher := sha256.New()
	hasher.Write([]byte("QQMailDrive-MasterKey-2026-Salt"))
	hasher.Write([]byte(GlobalConfig.Email))
	key := hasher.Sum(nil)

	engine := &StorageEngine{
		imapClient: client,
		tasks:      make(map[string]*TransferTask),
		cipherKey:  key,
		thumbCache:     make(map[string]string),
		cancelTokens:   make(map[string]context.CancelFunc),
		seekInterrupters: make(map[string]func(int)),
		structuredLogs: make([]LogEntry, 0, 5000),
		lastActivity: time.Now(),
		folderIndexCache: make(map[string]MasterIndex),
		smtpClient:   NewSMTPClient(GlobalConfig.SMTPServer, GlobalConfig.Email, GlobalConfig.IMAPAuthCode),
		masterIndex: MasterIndex{
			Version:      0,
			NextSeq:      "0000000001",
			LastUpdated:  time.Now(),
			Files:        make(map[string]FileNode),
			ThumbVolumes: make(map[string]ThumbVolumeInfo),
			ActiveThumb:  "01",
		},
	}
	if client != nil {
		client.Logger = engine.LogWithLevel
	}
	engine.StartKeepAlive()
	engine.StartShareAutoReceiver()
	return engine
}

// RegisterCancelToken 注册任务取消句柄
func (e *StorageEngine) RegisterCancelToken(taskID string, cancel context.CancelFunc) {
	e.cancelMu.Lock()
	defer e.cancelMu.Unlock()
	e.cancelTokens[taskID] = cancel
}

// RemoveCancelToken 注销任务取消句柄
func (e *StorageEngine) RemoveCancelToken(taskID string) {
	e.cancelMu.Lock()
	defer e.cancelMu.Unlock()
	delete(e.cancelTokens, taskID)
}

// CancelTask 取消指定传输任务，并全闭环彻底清理本地与云端半成品数据
// RegisterSeekInterrupter 注册任务寻道极速打断器
func (e *StorageEngine) RegisterSeekInterrupter(taskID string, fn func(int)) {
	e.seekMu.Lock()
	defer e.seekMu.Unlock()
	e.seekInterrupters[taskID] = fn
}

// RemoveSeekInterrupter 注销任务寻道极速打断器
func (e *StorageEngine) RemoveSeekInterrupter(taskID string) {
	e.seekMu.Lock()
	defer e.seekMu.Unlock()
	delete(e.seekInterrupters, taskID)
}

// PrioritizeTaskChunk 用户在播放器拖拽进度条时，将指定切片置顶插队拉取，并光速掐断所有旧切片传输
func (e *StorageEngine) PrioritizeTaskChunk(taskID string, chunkIdx int) {
	e.tasksLock.RLock()
	task, ok := e.tasks[taskID]
	e.tasksLock.RUnlock()
	if ok && task != nil && (task.Status == "running" || task.Status == "pending") {
		e.tasksLock.Lock()
		alreadyDone := false
		for _, dc := range task.CompletedChunks {
			if dc == chunkIdx {
				alreadyDone = true
				break
			}
		}
		if !alreadyDone {
			task.PriorityChunk = chunkIdx
			e.LogInfo("STREAM", fmt.Sprintf("🎬 [流式插队] 播放器置顶优先拉取分片 #%d 以便快速起播", chunkIdx))
		} else {
			task.PriorityChunk = 0
		}
		task.PlayCursor = chunkIdx
		e.LogInfo("STREAM", fmt.Sprintf("⏩ [播放游标] 连续预缓冲锚点锁定在分片 #%d，全速向后流式推进", chunkIdx))
		e.tasksLock.Unlock()

		// 核心加速：瞬间唤醒插队打断器，直接从底层 TCP 切断所有正在拉取旧分片的连接，腾出通道立刻下载目标分片！
		e.seekMu.Lock()
		interrupter, hasInterrupter := e.seekInterrupters[taskID]
		e.seekMu.Unlock()
		if hasInterrupter && interrupter != nil {
			interrupter(chunkIdx)
		}
	}
}

// CancelTaskWithReason 严格区分用户主动操作、预览浮窗关闭回收与系统内部任务交接
func (e *StorageEngine) CancelTaskWithReason(taskID string, reason string) {
	e.cancelMu.Lock()
	if cancel, ok := e.cancelTokens[taskID]; ok {
		cancel()
		delete(e.cancelTokens, taskID)
	}
	e.cancelMu.Unlock()

	e.tasksLock.Lock()
	task, exists := e.tasks[taskID]
	if exists {
		if task.Status != "completed" {
			task.Status = "cancelled"
			task.StageDetail = "已停止并回收资源"
			task.SpeedMBs = 0
			task.ActiveChunks = nil
			task.WritingChunks = nil
		}
		if reason == "reupload" || reason == "user_retry" {
			delete(e.tasks, taskID)
		}
	}
	e.tasksLock.Unlock()

	if exists {
		if reason == "user_click" {
			e.LogInfo("USER", fmt.Sprintf("[用户操作] 用户在传输队列手动取消任务: %s", task.FileName))
		} else if reason == "close_preview" {
			e.LogInfo("PREVIEW", fmt.Sprintf("[预览回收] 预览浮窗已关闭，后台静默释放媒体流缓冲: %s", task.FileName))
			// 自动归档守护：若边下边播期间文件已 100% 落盘核验，趁着媒体句柄随浮窗关闭彻底释放，自动将 .part 重命名为正式文件
			go func(fn string, tID string) {
				time.Sleep(300 * time.Millisecond)
				var destPath string
				if strings.HasPrefix(tID, "task_prev_") {
					fid := strings.TrimPrefix(tID, "task_prev_")
					destPath = filepath.Join("temp", fmt.Sprintf("%s_%s", fid, fn))
					// 若带 ID 的文件不存在，尝试标准文件名
					if _, err := os.Stat(destPath + ".part.done"); os.IsNotExist(err) {
						altPath := filepath.Join("temp", fn)
						if _, altErr := os.Stat(altPath + ".part.done"); altErr == nil {
							destPath = altPath
						}
					}
				} else {
					downloadDir := strings.TrimSpace(GlobalConfig.DownloadDir)
					if downloadDir == "" {
						downloadDir = "downloads"
					}
					destPath = filepath.Join(downloadDir, fn)
				}
				partPath := destPath + ".part"
				if _, err := os.Stat(partPath + ".done"); err == nil {
					if _, err := os.Stat(destPath); os.IsNotExist(err) {
						if renErr := os.Rename(partPath, destPath); renErr == nil {
							_ = os.Remove(partPath + ".done")
							_ = os.Remove(partPath + ".meta")
							_ = os.WriteFile(destPath+".done", []byte("1"), 0644)
							e.LogInfo("PREVIEW", fmt.Sprintf("✔ [自动归档] 媒体播放句柄已彻底释放，文件已平滑完成正式命名: %s", fn))
						}
					}
				}
			}(task.FileName, task.TaskID)
		} else if reason == "task_superseded" {
			e.LogInfo("STREAM", fmt.Sprintf("[任务交接] 预览任务已平滑转入正式本地下载管道: %s", task.FileName))
		} else {
			e.LogInfo("SYSTEM", fmt.Sprintf("[系统回收] 任务已停止并回收资源: %s (%s)", task.FileName, reason))
		}
	}
}

func (e *StorageEngine) CancelTask(taskID string) {
	e.CancelTaskWithReason(taskID, "user_click")
}

// NextSeqIncr 10位 Base36 单调递增序列算法 (0000000001 ~ zzzzzzzzzz)
func (e *StorageEngine) NextSeqIncr() string {
	e.indexLock.Lock()
	defer e.indexLock.Unlock()

	cur := e.masterIndex.NextSeq
	if len(cur) != SeqLen {
		cur = "0000000001"
	}

	val, err := strconv.ParseUint(cur, 36, 64)
	if err != nil {
		val = 1
	}
	val++
	nextStr := strconv.FormatUint(val, 36)
	for len(nextStr) < SeqLen {
		nextStr = "0" + nextStr
	}
	e.masterIndex.NextSeq = nextStr
	return cur
}

// GenerateFileKey 生成32字节(256位)真随机AES信封密钥
func (e *StorageEngine) GenerateFileKey() ([]byte, string) {
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		hasher := sha256.New()
		hasher.Write([]byte(fmt.Sprintf("%d-%s", time.Now().UnixNano(), GlobalConfig.Email)))
		key = hasher.Sum(nil)
	}
	return key, base64.StdEncoding.EncodeToString(key)
}

// EncryptDataWithKey 使用指定AES-256密钥加密 (信封加密模式)

// DeriveFileKey 使用用户账号主密钥与整文件 SHA-256 哈希，确定性派生该文件的专属 32 字节 AES-256 密钥
func (e *StorageEngine) DeriveFileKey(fileHash string) []byte {
	if len(e.cipherKey) != 32 || fileHash == "" {
		return e.cipherKey
	}
	mac := hmac.New(sha256.New, e.cipherKey)
	mac.Write([]byte(fileHash))
	return mac.Sum(nil)
}

// EncryptThumbnail 使用文件专属密钥加密缩略图 (打上 QQTHM01 物理隔离魔法头)
func (e *StorageEngine) EncryptThumbnail(plaintext, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("无效的AES密钥长度(%d), 需要32字节", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	var result []byte
	result = append(result, []byte(MagicThumbnailBytes)...)
	result = append(result, nonce...)
	result = append(result, ciphertext...)
	return result, nil
}

// DecryptThumbnail 使用文件专属密钥解密缩略图 (严格校验 QQTHM01 魔法头)
func (e *StorageEngine) DecryptThumbnail(data, key []byte) ([]byte, error) {
	magicLen := len(MagicThumbnailBytes)
	if len(data) < magicLen+12 {
		return nil, fmt.Errorf("缩略图密文数据长度不足")
	}
	if string(data[:magicLen]) != MagicThumbnailBytes {
		return nil, fmt.Errorf("缩略图魔法头不匹配")
	}
	nonce := data[magicLen : magicLen+12]
	ciphertext := data[magicLen+12:]

	if len(key) != 32 {
		return nil, fmt.Errorf("无效的AES密钥长度(%d), 需要32字节", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("缩略图解密校验失败: %w", err)
	}
	return plaintext, nil
}

func (e *StorageEngine) EncryptDataWithKey(plaintext, key []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("无效的AES密钥长度(%d), 需要32字节", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, nil)

	var result []byte
	result = append(result, []byte(MagicBytes)...)
	result = append(result, nonce...)
	result = append(result, ciphertext...)
	return result, nil
}

// DecryptDataWithKey 使用指定AES-256密钥解密
func (e *StorageEngine) DecryptDataWithKey(data, key []byte) ([]byte, error) {
	magicLen := len(MagicBytes)
	if len(data) < magicLen+12 {
		return nil, fmt.Errorf("密文数据长度不足")
	}
	if string(data[:magicLen]) != MagicBytes {
		return nil, fmt.Errorf("密文魔法头不匹配")
	}
	nonce := data[magicLen : magicLen+12]
	ciphertext := data[magicLen+12:]

	if len(key) != 32 {
		return nil, fmt.Errorf("无效的AES密钥长度(%d), 需要32字节", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("解密校验失败: %w", err)
	}
	return plaintext, nil
}

// EncryptData 默认使用系统主密钥加密 (用于主索引 master_index.dat 加密)
func (e *StorageEngine) EncryptData(plaintext []byte) ([]byte, error) {
	return e.EncryptDataWithKey(plaintext, e.cipherKey)
}

// DecryptData 默认使用系统主密钥解密
func (e *StorageEngine) DecryptData(data []byte) ([]byte, error) {
	return e.DecryptDataWithKey(data, e.cipherKey)
}

// ResolveDecryptionKeys 解析该节点可能的所有候选解密密钥 (支持信封密钥、历史发送方主密钥派生与当前主密钥三重自愈)
func (e *StorageEngine) ResolveDecryptionKeys(node *FileNode) [][]byte {
	var keys [][]byte
	seen := make(map[string]bool)

	addKey := func(k []byte) {
		if len(k) == 32 {
			kHex := hex.EncodeToString(k)
			if !seen[kHex] {
				seen[kHex] = true
				keys = append(keys, k)
			}
		}
	}

	// 1. 显式信封加密密钥 (如接收自好友分享的文件)
	if node.CipherKeyBase64 != "" {
		if k, err := base64.StdEncoding.DecodeString(node.CipherKeyBase64); err == nil {
			addKey(k)
		}
	}

	// 2. 本地文件派生密钥: HMAC-SHA256(MasterKey, FileSHA256) (0存储开销，1微秒实时推导)
	if node.SHA256 != "" && len(e.cipherKey) == 32 {
		addKey(e.DeriveFileKey(node.SHA256))
	}
	for _, c := range node.Chunks {
		if c.SHA256 != "" && len(e.cipherKey) == 32 {
			addKey(e.DeriveFileKey(c.SHA256))
		}
	}

	// 2. 若为分享导入的文件，自动从 SharedFrom 中提取发件人邮箱派生其主密钥 (兼容历史老版本分享)
	if node.SharedFrom != "" {
		senderEmail := node.SharedFrom
		if start := strings.Index(senderEmail, "("); start != -1 {
			if end := strings.Index(senderEmail, ")"); end > start {
				senderEmail = senderEmail[start+1 : end]
			}
		}
		senderEmail = strings.TrimSpace(senderEmail)
		if strings.Contains(senderEmail, "@") {
			hasher := sha256.New()
			hasher.Write([]byte("QQMailDrive-MasterKey-2026-Salt"))
			hasher.Write([]byte(senderEmail))
			addKey(hasher.Sum(nil))
		}
	}

	// 3. 当前登录账号的主密钥
	if len(e.cipherKey) == 32 {
		addKey(e.cipherKey)
	}

	return keys
}

// DecryptNodeData 依次尝试候选密钥解密数据，解密成功后自动回填补齐节点的 CipherKeyBase64
func (e *StorageEngine) DecryptNodeData(data []byte, node *FileNode) ([]byte, error) {
	keys := e.ResolveDecryptionKeys(node)
	var lastErr error
	for _, k := range keys {
		plain, err := e.DecryptDataWithKey(data, k)
		if err == nil {
			if node.CipherKeyBase64 == "" {
				node.CipherKeyBase64 = base64.StdEncoding.EncodeToString(k)
			}
			return plain, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("解密失败: 未找到有效密钥")
}


// BuildMIMEMessage 构造单封带伪装附件的标准 MIME 邮件数据
func (e *StorageEngine) BuildMIMEMessage(subject, metaHeader, fileName string, fileData []byte) []byte {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)

	buf.WriteString("From: " + GlobalConfig.Email + "\r\n")
	buf.WriteString("To: " + GlobalConfig.Email + "\r\n")
	buf.WriteString("Subject: " + subject + "\r\n")
	buf.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	buf.WriteString("X-Drive-Meta: " + metaHeader + "\r\n")
	buf.WriteString("MIME-Version: 1.0\r\n")
	buf.WriteString("Content-Type: multipart/mixed; boundary=\"" + writer.Boundary() + "\"\r\n\r\n")

	h := make(textproto.MIMEHeader)
	h.Set("Content-Type", "application/octet-stream; name=\"" + fileName + "\"")
	h.Set("Content-Disposition", "attachment; filename=\"" + fileName + "\"")
	h.Set("Content-Transfer-Encoding", "base64")

	part, err := writer.CreatePart(h)
	if err == nil {
		encoder := base64.NewEncoder(base64.StdEncoding, part)
		_, _ = encoder.Write(fileData)
		_ = encoder.Close()
	}
	_ = writer.Close()

	return buf.Bytes()
}


// isDeflateCompressible 规范做法 B：智能自适应检测文件是否值得在内存中压缩
func isDeflateCompressible(fileName string, sampleData []byte) bool {
	ext := strings.ToLower(filepath.Ext(fileName))

	// 1. 严禁压缩黑名单 (本身已有强熵压缩，再压会膨胀且极度浪费 CPU)
	uncompressibleExts := map[string]bool{
		// 视频
		".mp4": true, ".mkv": true, ".avi": true, ".mov": true, ".wmv": true,
		".flv": true, ".f4v": true, ".webm": true, ".m4v": true, ".ts": true,
		".mts": true, ".m2ts": true, ".rmvb": true, ".3gp": true,
		// 音频
		".mp3": true, ".aac": true, ".m4a": true, ".ogg": true, ".opus": true,
		".flac": true, ".alac": true, ".ape": true, ".wma": true,
		// 现代压缩图像
		".jpg": true, ".jpeg": true, ".png": true, ".webp": true, ".avif": true,
		".heic": true, ".heif": true, ".gif": true,
		// 常见归档包与安装包
		".zip": true, ".rar": true, ".7z": true, ".tar.gz": true, ".tgz": true,
		".tar.bz2": true, ".tbz2": true, ".tar.xz": true, ".txz": true, ".tar.zst": true,
		".zst": true, ".bz2": true, ".gz": true, ".xz": true, ".lzma": true,
		".cab": true, ".apk": true, ".ipa": true, ".jar": true, ".war": true,
		".ear": true, ".deb": true, ".rpm": true, ".appimage": true, ".dmg": true,
		// 现代 Office (底层即 Zip)
		".docx": true, ".xlsx": true, ".pptx": true, ".vsdx": true,
		".dotx": true, ".xltx": true, ".potx": true, ".odt": true, ".ods": true,
		".odp": true, ".epub": true,
	}

	if uncompressibleExts[ext] {
		return false
	}

	// 2. 必压白名单 (文本、代码、未压缩音画、数据库镜像，压缩收益高达 50%~90%)
	compressibleExts := map[string]bool{
		// 纯文本与标记
		".txt": true, ".log": true, ".out": true, ".err": true, ".nfo": true,
		".md": true, ".markdown": true, ".html": true, ".htm": true, ".xhtml": true,
		".rtf": true, ".tex": true, ".latex": true, ".rst": true,
		// 配置与数据
		".json": true, ".xml": true, ".yaml": true, ".yml": true, ".toml": true,
		".ini": true, ".conf": true, ".cfg": true, ".env": true, ".properties": true,
		".reg": true, ".plist": true, ".csv": true, ".tsv": true, ".sql": true,
		".ndjson": true, ".jsonl": true,
		// 源码与脚本
		".c": true, ".cpp": true, ".cc": true, ".cxx": true, ".h": true, ".hpp": true,
		".go": true, ".rs": true, ".java": true, ".kt": true, ".cs": true, ".swift": true,
		".py": true, ".rb": true, ".php": true, ".sh": true, ".bash": true, ".ps1": true,
		".bat": true, ".cmd": true, ".js": true, ".jsx": true, ".ts": true, ".tsx": true,
		".vue": true, ".svelte": true, ".css": true, ".scss": true, ".less": true,
		".svg": true, ".wasm": true, ".asm": true, ".s": true,
		// 未压缩媒体与镜像
		".bmp": true, ".dib": true, ".wav": true, ".aiff": true,
		".iso": true, ".img": true, ".raw": true, ".db": true, ".sqlite": true,
		".sqlite3": true, ".pdb": true, ".bin": true, ".obj": true,
	}

	if compressibleExts[ext] {
		return true
	}

	// 3. 未知或无扩展名：64KB 样本试探 (仅在内存中执行，0 磁盘开销)
	if len(sampleData) > 0 {
		sampleSize := len(sampleData)
		if sampleSize > 65536 {
			sampleSize = 65536
		}
		var buf bytes.Buffer
		fw, err := flate.NewWriter(&buf, flate.BestSpeed)
		if err == nil {
			_, _ = fw.Write(sampleData[:sampleSize])
			_ = fw.Close()
			// 若压缩后体积低于原样本的 88%，说明有良好压缩收益
			if float64(buf.Len()) < float64(sampleSize)*0.88 {
				return true
			}
		}
	}

	return false
}

// compressDeflateStream 纯内存管道 Deflate 快速压缩 (0 额外磁盘占用)
func compressDeflateStream(data []byte) []byte {
	var buf bytes.Buffer
	fw, err := flate.NewWriter(&buf, flate.DefaultCompression)
	if err != nil {
		return data
	}
	_, _ = fw.Write(data)
	_ = fw.Close()

	// 加上单字节魔法头标识已压缩 (0x01: Deflated, 0x00: Raw Store)
	res := make([]byte, 1+buf.Len())
	res[0] = 0x01
	copy(res[1:], buf.Bytes())
	return res
}

// decompressDeflateStream 纯内存管道 Inflate 解压 (0 额外磁盘占用)
func decompressDeflateStream(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return data, nil
	}
	// 检查压缩标识
	flag := data[0]
	rawPayload := data[1:]
	if flag == 0x00 {
		return rawPayload, nil // 原始 Store 模式
	}
	if flag == 0x01 {
		fr := flate.NewReader(bytes.NewReader(rawPayload))
		defer fr.Close()
		var out bytes.Buffer
		if _, err := io.Copy(&out, fr); err != nil {
			return nil, err
		}
		return out.Bytes(), nil
	}
	// 兼容历史未打标版本
	return data, nil
}

// UploadStream 纯内存管道切片流式直传 (0 磁盘临时副本，杜绝临时文件名污染，杜绝硬盘磨损)
func (e *StorageEngine) UploadStream(task *TransferTask, reader io.Reader, fileName string, totalSize int64, vPath, thumbBase64 string) error {
	ctx, cancel := context.WithCancel(context.Background())
	e.RegisterCancelToken(task.TaskID, cancel)
	defer e.RemoveCancelToken(task.TaskID)

	go func() {
		<-ctx.Done()
		if e.imapClient != nil {
			e.imapClient.Interrupt()
		}
	}()
	if e.imapClient != nil {
		e.imapClient.ResetInterrupt()
	}

	e.LogDebug("UPLOAD", fmt.Sprintf("[%s] 正在等待获取全局单通道上传门闸...", fileName))
	select {
	case uploadSem <- struct{}{}:
		e.LogDebug("UPLOAD", fmt.Sprintf("[%s] ✔ 成功获取全局上传门闸，开始处理数据流", fileName))
	case <-ctx.Done():
		e.LogInfo("UPLOAD", fmt.Sprintf("[%s] 排队等待中已被用户取消", fileName))
		return nil
	}
	defer func() {
		select {
		case <-uploadSem:
			e.LogDebug("UPLOAD", fmt.Sprintf("[%s] 全局上传门闸已成功释放", fileName))
		default:
		}
		delay := GlobalConfig.RequestDelayMS
		if delay > 0 {
			time.Sleep(time.Duration(delay) * time.Millisecond)
		}
	}()

	SetCleanExitStatus(false)
	task.Stage = StageScanning
	task.StageDetail = "正在处理文件数据流并计算特征..."
	e.logTask(task, fmt.Sprintf("开始处理上传数据流: %s (大小: %s)", fileName, formatBytes(totalSize)))

	task.TotalSize = totalSize
	chunkSize := GlobalConfig.ChunkSizeBytes
	if chunkSize <= 0 {
		chunkSize = int64(GlobalConfig.ChunkSizeMB) * 1024 * 1024
	}
	if chunkSize <= 0 {
		chunkSize = 52428799
	}

	fileID := fmt.Sprintf("file_%d", time.Now().UnixNano())
	task.FileID = fileID
	task.Type = "upload"

	// 任务中断或取消时，全闭环彻底清理已成功写入云端的半成品孤儿分片 (覆盖隔离空间及收件箱)
	cleanOrphanChunks := func() {
		if e.imapClient == nil || fileID == "" {
			return
		}

		go func(folder, fid, fn string, completedCount int) {
			time.Sleep(500 * time.Millisecond)

			// 关键修复 1：重置 IMAP 连接中断标志，并主动重连恢复长连接
			if e.imapClient != nil {
				e.imapClient.ResetInterrupt()
				if rErr := e.imapClient.Connect(); rErr != nil {
					e.LogError("UPLOAD", fmt.Sprintf("❌ [取消清理] 重建 IMAP 连接失败: %v", rErr))
				}
			}

			cleanFolder := func(targetFolder string) {
				if targetFolder == "" {
					return
				}
				e.LogInfo("UPLOAD", fmt.Sprintf("🧹 [取消清理] 正在排查并清理未完成分片邮件: %s (目录: [%s], 特征标识: %s)...", fn, targetFolder, fid))
				if err := e.imapClient.EnsureAndSelectFolder(targetFolder); err != nil {
					e.LogError("UPLOAD", fmt.Sprintf("❌ [取消清理] 选区目录 [%s] 失败: %v", targetFolder, err))
					return
				}

				var seqsToDelete []uint32

				// 策略 1: 纯特征查询 (去掉特殊字符与括号，避免腾讯 IMAP 服务器分词器过滤)
				seqs, sErr := e.imapClient.SearchMessageSeqs(fmt.Sprintf(`SUBJECT "%s"`, fid))
				if sErr == nil && len(seqs) > 0 {
					seqsToDelete = seqs
				}

				// 策略 2: 权威强力兜底 (直接全量扫描当前目录邮件 Header，100% 免疫云端搜索索引延迟)
				if len(seqsToDelete) == 0 {
					headers, hErr := e.imapClient.FetchAllHeadersFast()
					if hErr == nil {
						for _, h := range headers {
							if strings.Contains(h.Subject, fid) {
								seqsToDelete = append(seqsToDelete, h.SeqNum)
							}
						}
					} else {
						e.LogWarn("UPLOAD", fmt.Sprintf("⚠️ [取消清理] 兜底获取邮件列表失败: %v (%s)", hErr, targetFolder))
					}
				}

				if len(seqsToDelete) > 0 {
					if delErr := e.imapClient.DeleteAndExpunge(seqsToDelete); delErr == nil {
						e.LogInfo("UPLOAD", fmt.Sprintf("✔ [清理完毕] 已彻底清除云端目录 [%s] 残留的 %d 封未完成分片邮件 (%s)", targetFolder, len(seqsToDelete), fn))
					} else {
						e.LogError("UPLOAD", fmt.Sprintf("❌ [清理失败] 清理云端残留邮件失败: %v (%s)", delErr, fn))
					}
				} else {
					e.LogDebug("UPLOAD", fmt.Sprintf("[%s] 云端目录 [%s] 无残留分片邮件需要清理", fn, targetFolder))
				}
			}

			// 1. 清理主存储隔离空间
			cleanFolder(folder)

			// 2. 若主存储目录不是收件箱，同步排查收件箱 (防止 HTTP 协同写入后尚未归档的残留)
			if !isInboxFolder(folder) {
				cleanFolder("INBOX")
				_ = e.imapClient.EnsureAndSelectFolder(folder)
			}
		}(GlobalConfig.StorageFolder, fileID, fileName, len(task.CompletedChunks))
	}

	defer func() {
		if task.Status != "completed" {
			cleanOrphanChunks()
		}
	}()

	fileKey, _ := e.GenerateFileKey()
	fullHasher := sha256.New()
	var chunks []ChunkInfo

	// 针对中小型文件 (<= 64MB)，在内存中进行自适应压缩预判 (0 磁盘写入)
	// 若经 Deflate 压缩后总大小未超单个分片包上限 (默认 52428799 字节，约 50MB)，则直接作为单包直传 (仅发 1 封邮件，极大节省流量与邮件配额)
	if totalSize <= 64*1024*1024 {
		allBytes, rErr := io.ReadAll(reader)
		if rErr == nil && len(allBytes) > 0 {
			shouldCompress := isDeflateCompressible(fileName, allBytes)
			payloadToEncrypt := allBytes
			isCompressed := false
			if shouldCompress {
				compBytes := compressDeflateStream(allBytes)
				if len(compBytes) < len(allBytes) {
					payloadToEncrypt = compBytes
					isCompressed = true
				}
			}
			if !isCompressed {
				rawStore := make([]byte, 1+len(allBytes))
				rawStore[0] = 0x00
				copy(rawStore[1:], allBytes)
				payloadToEncrypt = rawStore
			}

			maxSinglePayload := chunkSize - 35
			if int64(len(payloadToEncrypt)) <= maxSinglePayload {
				task.TotalChunks = 1
				task.CurrentChunk = 1
				task.ActiveChunks = []int{1}

				if isCompressed {
					savedPct := (1.0 - float64(len(payloadToEncrypt)-1)/float64(len(allBytes))) * 100.0
					e.LogInfo("UPLOAD", fmt.Sprintf("✔ [自适应合并单包] %s 经压缩由 %s 骤降至 %s (节省 %.1f%% 流量与邮件配额)，直接作为单包直传", fileName, formatBytes(int64(len(allBytes))), formatBytes(int64(len(payloadToEncrypt))), savedPct))
				}

				partHash := sha256.Sum256(allBytes)
				partHashHex := hex.EncodeToString(partHash[:])
				// 全局秒传与同名冲突处理 (改老保留新)
				if dupNode, _ := e.ResolveSameNameConflict(vPath, partHashHex); dupNode != nil {
					task.CompletedChunks = []int{1}
					task.ActiveChunks = nil
					task.Transferred = totalSize
					task.ProgressPct = 100.0
					task.Stage = StageCompleted
					task.Status = "completed"
					task.StageDetail = "目录下重复文件"
					e.LogInfo("UPLOAD", fmt.Sprintf("⚡ [哈希秒传] %s 与已有文件 %s 哈希相同，瞬间完成秒传建档！", fileName, dupNode.Name))
					if dupNode.VirtualPath != vPath {
						// 跨路径复用分片
						cloned := *dupNode
						cloned.VirtualPath = vPath
						cloned.Name = fileName
						cloned.ModTime = time.Now()
						e.indexLock.Lock()
						e.masterIndex.Files[vPath] = cloned
						e.masterIndex.Version++
						e.indexLock.Unlock()
						_ = e.SyncMasterIndexToCloud()
					}
					return nil
				}
				modTime := time.Now()
				fileKey = e.DeriveFileKey(partHashHex)
				// fileKey derived via HMAC

				var encThumb []byte
				if thumbBase64 != "" {
					rawB64 := thumbBase64
					if idx := strings.Index(rawB64, ","); idx != -1 {
						rawB64 = rawB64[idx+1:]
					}
					if b, bErr := base64.StdEncoding.DecodeString(rawB64); bErr == nil && len(b) > 0 {
						if et, eErr := e.EncryptThumbnail(b, fileKey); eErr == nil {
							encThumb = et
						}
						e.SaveCachedThumb(partHashHex, b)
					}
				}

				task.CurrentSeq = fmt.Sprintf("QMD_%s_1_1_%d_%d.7z", partHashHex, totalSize, modTime.Unix())

				encData, err := e.EncryptDataWithKey(payloadToEncrypt, fileKey)
				if err != nil {
					task.Status = "failed"
					e.LogError("UPLOAD", "❌ AES 加密失败: "+err.Error())
					return err
				}

				metaJSON := fmt.Sprintf(`{"fid":"%s","name":"%s","vpath":"%s","size":%d,"mtime":%d,"idx":1,"tot":1,"hash":"%s","seq":"%s","psz":%d}`,
					fileID, fileName, vPath, totalSize, modTime.Unix(), partHashHex, task.CurrentSeq, len(allBytes))
				metaBase64 := base64.StdEncoding.EncodeToString([]byte(metaJSON))

				subject := fmt.Sprintf("[QMD:DATA]|%s|%s|1-1|%s", fileID, partHashHex, partHashHex[:12])
				task.Stage = StageUploading
				estMIMELen := int64(base64.StdEncoding.EncodedLen(len(encData))) + 450
				task.StageDetail = fmt.Sprintf("正在上传 %s", task.CurrentSeq)

				upStart := time.Now()
				var chunkWritten int64
				progressCb := func(sentBytes int) bool {
					if task.Status == "cancelled" || ctx.Err() != nil {
						return false
					}
					chunkWritten += int64(sentBytes)
					curRawAdded := int64(float64(chunkWritten) * float64(len(allBytes)) / float64(estMIMELen))
					if curRawAdded > totalSize {
						curRawAdded = totalSize
					}
					task.Transferred = curRawAdded
					if totalSize > 0 {
						task.ProgressPct = float64(curRawAdded) / float64(totalSize) * 100.0
						if task.ProgressPct > 99.0 {
							task.ProgressPct = 99.0
						}
					}
					curDur := time.Since(upStart).Seconds()
					if curDur > 0 {
						task.SpeedMBs = (float64(chunkWritten) / (1024 * 1024)) / curDur
					}
					return true
				}

				// 核心优化 2：流式写入 Socket，完全免去 67MB 中间内存分配
				if err := e.imapClient.AppendStreamMessageWithProgress(GlobalConfig.StorageFolder, subject, metaBase64, task.CurrentSeq, encData, encThumb, progressCb); err != nil {
					if task.Status == "cancelled" || ctx.Err() != nil {
						task.Status = "cancelled"
						e.LogInfo("UPLOAD", fmt.Sprintf("✔ [%s] 单包上传已由用户主动取消", fileName))
						return nil
					}
					task.Status = "failed"
					e.LogError("UPLOAD", "❌ 流式直写云端失败: "+err.Error())
					return err
				}

				task.CompletedChunks = []int{1}
				task.ActiveChunks = nil
				task.Transferred = totalSize
				task.ProgressPct = 100.0
				duration := time.Since(upStart).Seconds()
				if duration > 0 {
					task.SpeedMBs = (float64(estMIMELen) / (1024 * 1024)) / duration
				}

				chunks = append(chunks, ChunkInfo{
					Seq:       task.CurrentSeq,
					PartIndex: 1,
					TotalPart: 1,
					Size:      int64(len(allBytes)),
					SHA256:    partHashHex,
				})

				e.LogInfo("UPLOAD", fmt.Sprintf("✔ [IMAP 专属专线] 成功流式直传单包 %s (耗时: %.2fs, 速度: %.1f MB/s)", task.CurrentSeq, duration, task.SpeedMBs))

				task.Stage = StageCommitIdx
				task.StageDetail = "更新云端主索引邮件中..."
				fullHashHex := hex.EncodeToString(partHash[:])
				node := FileNode{
					ID:          fileID,
					Name:        fileName,
					VirtualPath: vPath,
					IsDir:       false,
					Size:        totalSize,
					ModTime:     modTime,
					SHA256:      fullHashHex,
					StorageType: StorageTypeChunked,
					Chunks:      nil, // 云端主索引彻底移除 Chunks 数组，极致瘦身
					CipherKeyBase64: base64.StdEncoding.EncodeToString(fileKey), // 保证任何场景下精准自愈解密
					IsMedia:     (encThumb != nil || isMediaExt(fileName)),
				}
				// 缩略图已内嵌至邮件正文，无需独立缩略图邮件
				e.indexLock.Lock()
				e.masterIndex.Files[vPath] = node
				parts := strings.Split(strings.Trim(vPath, "/"), "/")
				curDir := ""
				for i := 0; i < len(parts)-1; i++ {
					curDir += "/" + parts[i]
					if _, ok := e.masterIndex.Files[curDir]; !ok {
						e.masterIndex.Files[curDir] = FileNode{
							ID:          fmt.Sprintf("dir_%d_%d", time.Now().UnixNano(), i),
							Name:        parts[i],
							VirtualPath: curDir,
							IsDir:       true,
							ModTime:     time.Now(),
						}
					}
				}
				e.masterIndex.Version++
				e.masterIndex.LastUpdated = time.Now()
				e.indexLock.Unlock()

				// 核心优化 5：主索引防抖提交与合并
				e.SyncMasterIndexDebounced(1500 * time.Millisecond)
				task.Stage = StageCompleted
				task.Status = "completed"
				task.StageDetail = "✔ 上传完毕"
				e.LogInfo("UPLOAD", fmt.Sprintf("✔ 文件上传成功并完成云端建档: %s", fileName))
				return nil
			} else {
				reader = bytes.NewReader(allBytes)
			}
		}
	}

	rawChunkSize := RawChunkSizeFromTarget(chunkSize)
	totalChunks := int((totalSize + rawChunkSize - 1) / rawChunkSize)
	if totalChunks == 0 {
		totalChunks = 1
	}
	task.TotalChunks = totalChunks
	startTime := time.Now()
	modTime := time.Now()

	// 极速预扫整文件 64 位 SHA-256 全哈希与派生密钥 (零磁盘开销)
	var preFullHash string
	if seeker, ok := reader.(io.ReadSeeker); ok {
		h := sha256.New()
		if _, err := io.Copy(h, seeker); err == nil {
			preFullHash = hex.EncodeToString(h.Sum(nil))
			_, _ = seeker.Seek(0, io.SeekStart)
		}
	}
	if preFullHash != "" {
		fileKey = e.DeriveFileKey(preFullHash)
		// fileKey derived via HMAC
	}

	var encThumb []byte
	if thumbBase64 != "" {
		rawB64 := thumbBase64
		if idx := strings.Index(rawB64, ","); idx != -1 {
			rawB64 = rawB64[idx+1:]
		}
		if b, bErr := base64.StdEncoding.DecodeString(rawB64); bErr == nil && len(b) > 0 {
			if et, eErr := e.EncryptThumbnail(b, fileKey); eErr == nil {
				encThumb = et
			}
			if preFullHash != "" {
				e.SaveCachedThumb(preFullHash, b)
			}
		}
	}

	uploadCtx, uploadCancel := context.WithCancel(ctx)
	defer uploadCancel()

	// 核心架构：IMAP 与 HTTP 双通道真并发流水线 (True Parallel Dual-Channel Concurrent Upload Pipeline)
	// 1. 结构体封装已预处理好的分片（Deflate 压缩 + AES-256-GCM 独立密钥加密）
	type preparedUploadChunk struct {
		partIdx     int
		n           int
		partHashHex string
		seqName     string
		encData     []byte
		metaBase64  string
		subject     string
	}

	// 判断 HTTP 网页协同通道是否可用
	httpAvailable := (GlobalConfig.HTTPUploadConcurrency > 0) && (GlobalWebmailClient != nil && GlobalWebmailClient.IsSessionValid())
	numHttpWorkers := 0
	if httpAvailable && totalChunks > 1 {
		numHttpWorkers = GlobalConfig.HTTPUploadConcurrency
		if numHttpWorkers > 2 {
			numHttpWorkers = 2
		}
	}

	if numHttpWorkers > 0 {
		e.logTask(task, fmt.Sprintf("🚀 [双通道真并发就绪] 启动 IMAP 专线与 %d 路 HTTP 网页通道协同并行直传 (首切片直通 IMAP，数据切片双工并发)", numHttpWorkers))
	} else {
		e.logTask(task, "⚡ [IMAP 单专线传输] 启动 IMAP 专属直写管道流式上传...")
	}

	// 专用分发管道与并发控制
	imapFirstChan := make(chan *preparedUploadChunk, 1)              // 首切片直通 IMAP
	sharedDataChan := make(chan *preparedUploadChunk, 2)             // 纯数据分片共享池 (背压容量 2)
	fallbackImapChan := make(chan *preparedUploadChunk, totalChunks) // HTTP 故障无缝转入 IMAP 兜底池
	allDoneChan := make(chan struct{})                               // 全部分片完成唤醒信号

	var (
		taskMu            sync.Mutex
		chunkProgressMap  = make(map[int]int64)
		completedMap      = make(map[int]bool)
		chunksMu          sync.Mutex
		completedChunks   []ChunkInfo
		httpSubjsMu       sync.Mutex
		httpUploadedSubjs []string
		producerErr       error
		fatalErr          error
		fatalMu           sync.Mutex
		httpDisabled      atomic.Bool
	)

	setFatalErr := func(err error) {
		fatalMu.Lock()
		if fatalErr == nil {
			fatalErr = err
		}
		fatalMu.Unlock()
		uploadCancel()
	}

	getFatalErr := func() error {
		fatalMu.Lock()
		defer fatalMu.Unlock()
		return fatalErr
	}

	removeInt := func(slice []int, val int) []int {
		res := make([]int, 0, len(slice))
		for _, v := range slice {
			if v != val {
				res = append(res, v)
			}
		}
		return res
	}

	startChunk := func(pIdx int, seqName string) {
		taskMu.Lock()
		task.ActiveChunks = append(task.ActiveChunks, pIdx)
		sort.Ints(task.ActiveChunks)
		task.CurrentChunk = pIdx
		task.CurrentSeq = seqName + ".7z"
		task.Stage = StageUploading
		task.StageDetail = fmt.Sprintf("正在并发上传分片 %v", task.ActiveChunks)
		taskMu.Unlock()
	}

	checkPause := func(partIdx int) bool {
		if task.Status == "paused" {
			e.logTask(task, fmt.Sprintf("⏸ 任务已暂停 (停留在第 %d/%d 分片)", partIdx, totalChunks))
			for task.Status == "paused" {
				time.Sleep(300 * time.Millisecond)
			}
			if task.Status == "cancelled" || uploadCtx.Err() != nil {
				e.logTask(task, "❌ 任务已被用户取消")
				return false
			}
			e.logTask(task, fmt.Sprintf("▶️ 任务恢复执行，继续上传第 %d/%d 分片...", partIdx, totalChunks))
		}
		return task.Status != "cancelled" && uploadCtx.Err() == nil
	}

	makeProgressCb := func(partIdx, rawBytes, estSentLen int, workerStart time.Time) func(int) bool {
		var written int64
		return func(sentBytes int) bool {
			if task.Status == "cancelled" || uploadCtx.Err() != nil {
				return false
			}
			written += int64(sentBytes)
			curPartRaw := int64(float64(written) * float64(rawBytes) / float64(estSentLen))
			if curPartRaw > int64(rawBytes) {
				curPartRaw = int64(rawBytes)
			}

			taskMu.Lock()
			chunkProgressMap[partIdx] = curPartRaw
			var sumRaw int64
			for _, b := range chunkProgressMap {
				sumRaw += b
			}
			if sumRaw > totalSize {
				sumRaw = totalSize
			}
			task.Transferred = sumRaw
			if totalSize > 0 {
				task.ProgressPct = float64(sumRaw) / float64(totalSize) * 100.0
				if task.ProgressPct > 99.0 && len(completedMap) < totalChunks {
					task.ProgressPct = 99.0
				}
			}
			curElapsed := time.Since(startTime).Seconds()
			if curElapsed > 0 {
				task.SpeedMBs = (float64(sumRaw) / (1024 * 1024)) / curElapsed
				if task.SpeedMBs > 0 && totalSize > sumRaw {
					task.RemainSec = float64(totalSize-sumRaw) / (task.SpeedMBs * 1024 * 1024)
				}
			}
			task.ElapsedSec = curElapsed
			taskMu.Unlock()
			return true
		}
	}

	finishChunk := func(item *preparedUploadChunk, channelName string, duration float64, speedMBs float64) {
		taskMu.Lock()
		chunkProgressMap[item.partIdx] = int64(item.n)
		var sumRaw int64
		for _, b := range chunkProgressMap {
			sumRaw += b
		}
		if sumRaw > totalSize {
			sumRaw = totalSize
		}
		task.Transferred = sumRaw
		if totalSize > 0 {
			task.ProgressPct = float64(sumRaw) / float64(totalSize) * 100.0
		}
		task.ActiveChunks = removeInt(task.ActiveChunks, item.partIdx)
		if !completedMap[item.partIdx] {
			completedMap[item.partIdx] = true
			task.CompletedChunks = append(task.CompletedChunks, item.partIdx)
			sort.Ints(task.CompletedChunks)
		}
		task.ElapsedSec = time.Since(startTime).Seconds()
		if len(task.ActiveChunks) > 0 {
			task.StageDetail = fmt.Sprintf("正在并发上传分片 %v", task.ActiveChunks)
		} else if len(completedMap) == totalChunks {
			task.StageDetail = "所有分片已传输完毕"
		}
		isDone := (len(completedMap) == totalChunks)
		taskMu.Unlock()

		chunksMu.Lock()
		completedChunks = append(completedChunks, ChunkInfo{
			Seq:       item.seqName + ".7z",
			PartIndex: item.partIdx,
			TotalPart: totalChunks,
			Size:      int64(item.n),
			SHA256:    item.partHashHex,
		})
		chunksMu.Unlock()

		e.logTask(task, fmt.Sprintf("✔ [%s] 分片 #%d 写入成功 (耗时: %.2fs, 速度: %.1f MB/s)", channelName, item.partIdx, duration, speedMBs))

		if isDone {
			select {
			case <-allDoneChan:
			default:
				close(allDoneChan)
			}
		}
	}

	// 1. 生产者后台协程：流式读取原始数据 -> Deflate 压缩预判 -> AES-256 加密 -> 推入分发管道
	go func() {
		defer close(sharedDataChan)
		defer close(imapFirstChan)

		for pIdx := 1; pIdx <= totalChunks; pIdx++ {
			if uploadCtx.Err() != nil || task.Status == "cancelled" {
				return
			}

			buf := make([]byte, rawChunkSize)
			n, rErr := io.ReadFull(reader, buf)
			if n == 0 {
				return
			}
			pBytes := buf[:n]
			fullHasher.Write(pBytes)
			pHash := sha256.Sum256(pBytes)
			pHashHex := hex.EncodeToString(pHash[:])
			currHashKey := preFullHash
			if currHashKey == "" {
				currHashKey = pHashHex
			}
			sName := fmt.Sprintf("QMD_%s_%d_%d_%d_%d", currHashKey, pIdx, totalChunks, totalSize, modTime.Unix())

			shouldComp := isDeflateCompressible(fileName, pBytes)
			payload := pBytes
			if shouldComp {
				comp := compressDeflateStream(pBytes)
				if len(comp) < len(pBytes) {
					payload = comp
				} else {
					raw := make([]byte, 1+len(pBytes))
					raw[0] = 0x00
					copy(raw[1:], pBytes)
					payload = raw
				}
			} else {
				raw := make([]byte, 1+len(pBytes))
				raw[0] = 0x00
				copy(raw[1:], pBytes)
				payload = raw
			}

			enc, eErr := e.EncryptDataWithKey(payload, fileKey)
			if eErr != nil {
				producerErr = eErr
				setFatalErr(fmt.Errorf("AES 加密分片 #%d 失败: %w", pIdx, eErr))
				return
			}

			mJSON := fmt.Sprintf(`{"fid":"%s","name":"%s","vpath":"%s","size":%d,"mtime":%d,"idx":%d,"tot":%d,"hash":"%s","seq":"%s","psz":%d}`,
				fileID, fileName, vPath, totalSize, modTime.Unix(), pIdx, totalChunks, pHashHex, sName, n)
			mB64 := base64.StdEncoding.EncodeToString([]byte(mJSON))
			subj := fmt.Sprintf("[QMD:DATA]|%s|%s|%d-%d|%s", fileID, currHashKey, pIdx, totalChunks, currHashKey[:12])

			item := &preparedUploadChunk{
				partIdx:     pIdx,
				n:           n,
				partHashHex: pHashHex,
				seqName:     sName,
				encData:     enc,
				metaBase64:  mB64,
				subject:     subj,
			}

			if pIdx == 1 {
				select {
				case imapFirstChan <- item:
				case <-uploadCtx.Done():
					return
				}
			} else {
				select {
				case sharedDataChan <- item:
				case <-uploadCtx.Done():
					return
				}
			}

			if rErr == io.EOF || rErr == io.ErrUnexpectedEOF {
				return
			}
		}
	}()

	var wg sync.WaitGroup

	// 2. IMAP Worker 专线协程：优先上传首切片（携带元数据与缩略图正文），随后竞争纯数据分片或兜底执行 HTTP 失败分片
	wg.Add(1)
	go func() {
		defer wg.Done()

		// 2.1 首切片 Chunk 1 必须由 IMAP 专属专线直传
		select {
		case item1, ok := <-imapFirstChan:
			if !ok || item1 == nil {
				return
			}
			if !checkPause(item1.partIdx) {
				return
			}
			startChunk(item1.partIdx, item1.seqName)
			upStart := time.Now()
			curThumb := encThumb
			estMIMELen := int64(base64.StdEncoding.EncodedLen(len(item1.encData))) + 450 + int64(len(curThumb))
			pCb := makeProgressCb(item1.partIdx, item1.n, int(estMIMELen), upStart)

			e.logTask(task, fmt.Sprintf("⚡ [IMAP 专属专线] 开始流式上传首切片 #1/%d (%s.7z)...", totalChunks, item1.seqName))
			upErr := e.imapClient.AppendStreamMessageWithProgress(GlobalConfig.StorageFolder, item1.subject, item1.metaBase64, item1.seqName+".7z", item1.encData, curThumb, pCb)
			if upErr != nil {
				if task.Status != "cancelled" && uploadCtx.Err() == nil {
					setFatalErr(fmt.Errorf("IMAP 上传首切片失败: %w", upErr))
					task.Status = "failed"
					e.logTask(task, "❌ APPEND 流式写入云端失败: "+upErr.Error())
				}
				return
			}
			dur := time.Since(upStart).Seconds()
			var spd float64
			if dur > 0 {
				spd = (float64(estMIMELen) / (1024 * 1024)) / dur
			}
			finishChunk(item1, "IMAP 专属专线", dur, spd)

			delay := GlobalConfig.RequestDelayMS
			if delay > 0 && totalChunks > 1 {
				task.ThrottleWaitMS = delay
				time.Sleep(time.Duration(delay) * time.Millisecond)
				task.ThrottleWaitMS = 0
			}
		case <-allDoneChan:
			return
		case <-uploadCtx.Done():
			return
		}

		// 2.2 首切片完成后，IMAP Worker 继续参与后续纯数据分片的并发直传与兜底接管
		for {
			taskMu.Lock()
			doneCount := len(completedMap)
			taskMu.Unlock()
			if doneCount >= totalChunks || uploadCtx.Err() != nil || task.Status == "cancelled" {
				return
			}

			var item *preparedUploadChunk
			select {
			case fb := <-fallbackImapChan:
				item = fb
			default:
				select {
				case fb := <-fallbackImapChan:
					item = fb
				case it, ok := <-sharedDataChan:
					if ok {
						item = it
					} else {
						// sharedDataChan 已无新切片，等待可能因 HTTP 故障塞入 fallback 的切片，或全部完成信号
						select {
						case fb2 := <-fallbackImapChan:
							item = fb2
						case <-allDoneChan:
							return
						case <-uploadCtx.Done():
							return
						}
					}
				case <-allDoneChan:
					return
				case <-uploadCtx.Done():
					return
				}
			}

			if item == nil {
				return
			}

			if !checkPause(item.partIdx) {
				return
			}

			startChunk(item.partIdx, item.seqName)
			upStart := time.Now()
			estMIMELen := int64(base64.StdEncoding.EncodedLen(len(item.encData))) + 450
			pCb := makeProgressCb(item.partIdx, item.n, int(estMIMELen), upStart)

			e.logTask(task, fmt.Sprintf("⚡ [IMAP 专属专线] 正在流式直传纯数据分片 #%d/%d (%s.7z)...", item.partIdx, totalChunks, item.seqName))
			upErr := e.imapClient.AppendStreamMessageWithProgress(GlobalConfig.StorageFolder, item.subject, item.metaBase64, item.seqName+".7z", item.encData, nil, pCb)
			if upErr != nil {
				if task.Status != "cancelled" && uploadCtx.Err() == nil {
					setFatalErr(fmt.Errorf("IMAP 上传分片 #%d 失败: %w", item.partIdx, upErr))
					task.Status = "failed"
					e.logTask(task, fmt.Sprintf("❌ 分片 #%d IMAP 写入失败: %v", item.partIdx, upErr))
				}
				return
			}

			dur := time.Since(upStart).Seconds()
			var spd float64
			if dur > 0 {
				spd = (float64(estMIMELen) / (1024 * 1024)) / dur
			}
			finishChunk(item, "IMAP 专属专线", dur, spd)

			delay := GlobalConfig.RequestDelayMS
			if delay > 0 && doneCount+1 < totalChunks {
				task.ThrottleWaitMS = delay
				time.Sleep(time.Duration(delay) * time.Millisecond)
				task.ThrottleWaitMS = 0
			}
		}
	}()

	// 3. HTTP Worker 网页协同协程池：与 IMAP Worker 同时并发并行上传纯数据分片
	for wIdx := 1; wIdx <= numHttpWorkers; wIdx++ {
		wg.Add(1)
		go func(workerID int) {
			defer wg.Done()
			consecutiveHttpFails := 0

			for {
				if httpDisabled.Load() || uploadCtx.Err() != nil || task.Status == "cancelled" {
					return
				}

				taskMu.Lock()
				doneCount := len(completedMap)
				taskMu.Unlock()
				if doneCount >= totalChunks {
					return
				}

				var item *preparedUploadChunk
				select {
				case it, ok := <-sharedDataChan:
					if !ok {
						return
					}
					item = it
				case <-allDoneChan:
					return
				case <-uploadCtx.Done():
					return
				}

				if item == nil {
					return
				}

				if !checkPause(item.partIdx) {
					return
				}

				startChunk(item.partIdx, item.seqName)
				upStart := time.Now()
				estRawLen := len(item.encData)
				pCb := makeProgressCb(item.partIdx, item.n, estRawLen, upStart)

				e.logTask(task, fmt.Sprintf("⚡ [HTTP 网页协同-%d] 并发上传纯数据分片 #%d/%d (%s.7z)...", workerID, item.partIdx, totalChunks, item.seqName))
				upErr := GlobalWebmailClient.UploadChunkViaWebmail(GlobalConfig.Email, item.subject, item.seqName+".7z", item.metaBase64, item.encData, pCb)
				if upErr != nil {
					e.LogWarn("UPLOAD", fmt.Sprintf("⚠️ [HTTP 遇阻转入兜底] 分片 #%d HTTP 协同未成功 (%v)，立即无缝转入 IMAP 专属直写管道...", item.partIdx, upErr))
					taskMu.Lock()
					task.ActiveChunks = removeInt(task.ActiveChunks, item.partIdx)
					if len(task.ActiveChunks) > 0 {
						task.StageDetail = fmt.Sprintf("正在并发上传分片 %v", task.ActiveChunks)
					}
					taskMu.Unlock()

					consecutiveHttpFails++
					if consecutiveHttpFails >= 3 {
						e.LogWarn("UPLOAD", "⚠️ [HTTP通道熔断避让] HTTP 网页通道连续 3 次分片直传未成功（腾讯网页端对直接上传存在接口或大小限制），当前任务后续分片全量由 IMAP 专属专线高速直传承接，避免每个分片重复等待！")
						httpDisabled.Store(true)
					}

					// 若 Webmail 登录态已失效，标记 httpDisabled 避免后续无谓尝试
					if !GlobalWebmailClient.IsSessionValid() {
						e.LogWarn("UPLOAD", "⚠️ 检测到 Webmail 会话已失效，后续分片将全量由 IMAP 专属专线承接")
						httpDisabled.Store(true)
					}

					// 关键兜底：将失败分片塞入 fallbackImapChan，由 IMAP Worker 无缝接管！
					fallbackImapChan <- item
					time.Sleep(1 * time.Second)
					continue
				}

				// HTTP 上传成功，重置连续失败计数
				consecutiveHttpFails = 0
				httpSubjsMu.Lock()
				httpUploadedSubjs = append(httpUploadedSubjs, item.subject)
				httpSubjsMu.Unlock()

				dur := time.Since(upStart).Seconds()
				var spd float64
				if dur > 0 {
					spd = (float64(estRawLen) / (1024 * 1024)) / dur
				}
				finishChunk(item, fmt.Sprintf("HTTP 网页协同-%d", workerID), dur, spd)

				delay := GlobalConfig.RequestDelayMS
				if delay > 0 && doneCount+1 < totalChunks {
					task.ThrottleWaitMS = delay
					time.Sleep(time.Duration(delay) * time.Millisecond)
					task.ThrottleWaitMS = 0
				}
			}
		}(wIdx)
	}

	// 等待所有协同 Worker 完成
	wg.Wait()

	if err := getFatalErr(); err != nil {
		return err
	}
	if task.Status == "cancelled" || uploadCtx.Err() != nil {
		task.Status = "cancelled"
		e.LogInfo("UPLOAD", fmt.Sprintf("✔ [%s] 上传已由用户主动取消", fileName))
		return fmt.Errorf("上传已由用户主动取消")
	}

	// 校验所有切片是否均已完成
	taskMu.Lock()
	doneCount := len(completedMap)
	taskMu.Unlock()
	if doneCount < totalChunks {
		if producerErr != nil {
			task.Status = "failed"
			return producerErr
		}
		task.Status = "failed"
		return fmt.Errorf("上传未完全完成: 仅完成 %d/%d 分片", doneCount, totalChunks)
	}

	// 按照分片序号 PartIndex 严格排序分片信息
	chunksMu.Lock()
	sort.Slice(completedChunks, func(i, j int) bool {
		return completedChunks[i].PartIndex < completedChunks[j].PartIndex
	})
	chunks = completedChunks
	chunksMu.Unlock()

	// 统一批量归档：如果通过 HTTP 协同上传了分片，且目标存储目录不是收件箱，
	// 则在所有分片写入完毕后，由 IMAP 专线在收件箱中一次性批量将这些分片邮件平滑平移至目标隔离空间
	if len(httpUploadedSubjs) > 0 && !isInboxFolder(GlobalConfig.StorageFolder) {
		expectedCount := len(httpUploadedSubjs)
		e.LogInfo("UPLOAD", fmt.Sprintf("📦 [批量归档] 正在将 %d 个 HTTP 协同分片邮件从收件箱平移至隔离空间 [%s]...", expectedCount, GlobalConfig.StorageFolder))
		_ = e.imapClient.EnsureAndSelectFolder("INBOX")

		var seqsToMove []uint32
		// 轮询等待腾讯云端发信入库收件箱（最多等待 10 秒，每秒排查一次）
		for attempt := 1; attempt <= 10; attempt++ {
			time.Sleep(1 * time.Second)
			// 策略 1: 纯特征查询（去除特殊字符避免影响分词）
			seqs, sErr := e.imapClient.SearchMessageSeqs(fmt.Sprintf(`SUBJECT "%s"`, fileID))
			if sErr == nil && len(seqs) >= expectedCount {
				seqsToMove = seqs
				break
			}
			// 策略 2: 兜底扫描最新邮件头，彻底免疫异步搜索索引延迟
			if attempt >= 2 {
				headers, hErr := e.imapClient.FetchAllHeadersFast()
				if hErr == nil {
					var found []uint32
					for _, h := range headers {
						if strings.Contains(h.Subject, fileID) {
							found = append(found, h.SeqNum)
						}
					}
					if len(found) >= expectedCount {
						seqsToMove = found
						break
					}
					if attempt == 10 && len(found) > 0 {
						seqsToMove = found
						break
					}
				}
			}
		}

		if len(seqsToMove) > 0 {
			if mErr := e.imapClient.MoveMessages(seqsToMove, "INBOX", GlobalConfig.StorageFolder); mErr == nil {
				e.LogInfo("UPLOAD", fmt.Sprintf("✔ [批量归档] 成功将 %d 封分片邮件平滑归档至 [%s]", len(seqsToMove), GlobalConfig.StorageFolder))
			} else {
				e.LogWarn("UPLOAD", fmt.Sprintf("⚠️ [批量归档警告] 平移分片邮件失败: %v (分片仍安全保存在收件箱)", mErr))
			}
		} else {
			e.LogWarn("UPLOAD", fmt.Sprintf("⚠️ [批量归档提示] 收件箱暂未检索到协同分片邮件 (特征: %s)，可能云端投递存在延迟，分片仍安全保存在云端", fileID))
		}
		_ = e.imapClient.EnsureAndSelectFolder(GlobalConfig.StorageFolder)
	}

	// 提交主索引邮件前同样严格遵从用户自定义设定的延时，平滑缓冲保护 TCP 链路
	delay := GlobalConfig.RequestDelayMS
	if delay > 0 {
		time.Sleep(time.Duration(delay) * time.Millisecond)
	}

	task.Stage = StageCommitIdx
	task.StageDetail = "更新云端主索引邮件中..."
	e.logTask(task, "所有分片写入完毕，开始登记进入主索引...")

	fullHashHex := hex.EncodeToString(fullHasher.Sum(nil))

	node := FileNode{
		ID:          fileID,
		Name:        fileName,
		VirtualPath: vPath,
		IsDir:       false,
		Size:        totalSize,
		ModTime:     modTime,
		SHA256:      fullHashHex,
		StorageType: StorageTypeChunked,
		Chunks:      nil, // 彻底移除 Chunks，主索引极致瘦身
		CipherKeyBase64: base64.StdEncoding.EncodeToString(fileKey), // 保证任何场景下精准自愈解密
		IsMedia:     (encThumb != nil || isMediaExt(fileName)),
	}

	// 缩略图已加密内嵌至第 1 分片邮件正文 (MIME Part 1)，按需毫秒级按部件拉取，无需独立分卷邮件

	e.indexLock.Lock()
	e.masterIndex.Files[vPath] = node

	// 自动补齐虚拟父级目录，确保上传文件夹时根目录与多层子目录能正常展示！
	parts := strings.Split(strings.Trim(vPath, "/"), "/")
	curDir := ""
	for i := 0; i < len(parts)-1; i++ {
		curDir += "/" + parts[i]
		if _, ok := e.masterIndex.Files[curDir]; !ok {
			e.masterIndex.Files[curDir] = FileNode{
				ID:          fmt.Sprintf("dir_%d_%d", time.Now().UnixNano(), i),
				Name:        parts[i],
				VirtualPath: curDir,
				IsDir:       true,
				ModTime:     time.Now(),
			}
		}
	}

	e.masterIndex.Version++
	e.masterIndex.LastUpdated = time.Now()
	e.indexLock.Unlock()

	// 核心优化 5：主索引防抖合并提交 (1.5秒延迟防抖)
	e.SyncMasterIndexDebounced(1500 * time.Millisecond)

	task.Stage = StageCompleted
	task.Status = "completed"
	task.StageDetail = "✔ 上传完毕"
	task.ProgressPct = 100.0
	e.logTask(task, fmt.Sprintf("✔ 文件上传成功并完成云端建档: %s", fileName))

	// 恢复干净退出状态
	e.tasksLock.RLock()
	hasRunning := false
	for _, t := range e.tasks {
		if t.Status == "running" && t.TaskID != task.TaskID {
			hasRunning = true
			break
		}
	}
	e.tasksLock.RUnlock()

	if !hasRunning {
		SetCleanExitStatus(true)
	}

	return nil
}

// UploadLargeFile 从本地磁盘物理文件上传 (委托至 UploadStream 管道)
func (e *StorageEngine) UploadLargeFile(task *TransferTask, srcPath, vPath, thumbBase64 string) error {
	file, err := os.Open(srcPath)
	if err != nil {
		task.Status = "failed"
		e.logTask(task, "❌ 打开本地物理文件失败: "+err.Error())
		return err
	}
	defer file.Close()

	stat, err := file.Stat()
	if err != nil {
		task.Status = "failed"
		return err
	}

	actualName := task.FileName
	if actualName == "" {
		actualName = stat.Name()
	}

	return e.UploadStream(task, file, actualName, stat.Size(), vPath, thumbBase64)
}

// StartKeepAlive 核心优化 6：QQ 邮箱实测标准 IMAP 长连接工业级保活引擎
// 1. 240秒 (4分钟) 精确平衡心跳频率 (防腾讯 10~15 分钟会话超时，防高频被风控判定爬虫)
// 2. 空闲互斥锁：正在拉取/传输切片时 100% 绝对静默，仅连接在池中闲置超 2 分钟才发轻量 NOOP
// 3. 探活失败后台静默自愈 (Silent Reconnect)：休眠唤醒或断线后在后台自动握手选区恢复，前台零感知
// 4. 深度空闲 30 分钟自动缩容：仅保留主长连接，释放辅助并发连接，防长期占满腾讯并发配额
func (e *StorageEngine) StartKeepAlive() {
	ticker := time.NewTicker(240 * time.Second) // 规则 1：4 分钟黄金心跳频率
	go func() {
		for range ticker.C {
			// 规则 2：空闲互斥锁。检查是否有正在运行的传输或音视频播放任务
			e.tasksLock.RLock()
			hasActiveTasks := false
			for _, t := range e.tasks {
				if t != nil && (t.Status == "running" || t.Status == "pending") {
					hasActiveTasks = true
					break
				}
			}
			e.tasksLock.RUnlock()

			if hasActiveTasks {
				e.lastActivity = time.Now()
				continue // 正在传输业务数据，业务数据本身就是天然心跳，绝对禁止发送 NOOP 打扰！
			}

			// 规则 4：深度空闲 30 分钟自动缩容 (仅保留主长连接，释放副连接配额)
			if !e.lastActivity.IsZero() && time.Since(e.lastActivity) > 30*time.Minute {
				e.poolMu.Lock()
				if len(e.downloadPool) > 0 {
					e.LogDebug("IMAP", "网盘深度闲置超 30 分钟，自动缩容关闭辅助连接池，释放云端长连接并发配额")
					for _, c := range e.downloadPool {
						if c != nil {
							c.Close()
						}
					}
					e.downloadPool = nil
				}
				e.poolMu.Unlock()
			}

			// 保活主链路 (#1)：连接闲置超 2 分钟时，发送超轻量 NOOP
			if e.imapClient != nil && e.imapClient.connected {
				if time.Since(e.imapClient.lastActive) > 120*time.Second {
					if err := e.imapClient.Noop(); err != nil {
						// 规则 3：探活失败后台静默自愈 (Silent Reconnect)
						e.LogDebug("IMAP", fmt.Sprintf("主长连接探活失效，后台静默自愈重连: %v", err))
						_ = e.imapClient.Connect()
						if GlobalConfig.StorageFolder != "" {
							_ = e.imapClient.SelectFolderOnly(GlobalConfig.StorageFolder)
						}
					}
				}
			}

			// 保活辅助连接池：仅对闲置超 2 分钟的连接轻量探活，严格核验账号血统，失败只读恢复，绝不新建
			e.poolMu.Lock()
			for idx, c := range e.downloadPool {
				if c != nil {
					// 严格核查血统：若连接所属账号与当前 GlobalConfig 不一致，立即彻底断开关闭
					if !strings.EqualFold(c.email, GlobalConfig.Email) || c.authCode != GlobalConfig.IMAPAuthCode {
						c.Close()
						continue
					}
					if c.connected && time.Since(c.lastActive) > 120*time.Second {
						if err := c.Noop(); err != nil {
							e.LogDebug("IMAP", fmt.Sprintf("辅助长连接 #%d 探活失效，后台静默重连: %v", idx+2, err))
							_ = c.Connect()
							if GlobalConfig.StorageFolder != "" {
								_ = c.SelectFolderOnly(GlobalConfig.StorageFolder)
							}
						}
					}
				}
			}
			e.poolMu.Unlock()
		}
	}()
}

// SyncMasterIndexDebounced 核心优化 5：主索引防抖与合并提交 (默认 1.5 秒延时防抖，多任务连发合并为单次云端提交)
func (e *StorageEngine) SyncMasterIndexDebounced(delay time.Duration) {
	e.debounceMu.Lock()
	defer e.debounceMu.Unlock()

	if e.indexDebounceTimer != nil {
		e.indexDebounceTimer.Stop()
	}

	if delay <= 0 {
		delay = 1500 * time.Millisecond
	}

	e.indexDebounceTimer = time.AfterFunc(delay, func() {
		_ = e.SyncMasterIndexToCloud()
	})
}

// FlushMasterIndexSync 立即无延迟强刷主索引落盘 (程序退出、关键运维对账时调用)
func (e *StorageEngine) FlushMasterIndexSync() error {
	e.debounceMu.Lock()
	if e.indexDebounceTimer != nil {
		e.indexDebounceTimer.Stop()
		e.indexDebounceTimer = nil
	}
	e.debounceMu.Unlock()

	return e.SyncMasterIndexToCloud()
}

// CancelDebounceSync 取消当前防抖定时器，并返回是否存在未完成的排队同步
func (e *StorageEngine) CancelDebounceSync() bool {
	e.debounceMu.Lock()
	defer e.debounceMu.Unlock()
	if e.indexDebounceTimer != nil {
		e.indexDebounceTimer.Stop()
		e.indexDebounceTimer = nil
		return true
	}
	return false
}

// SyncSpecifiedIndexToCloud 独立同步指定空间的主索引到云端（支持异空间异步安全刷盘）
func (e *StorageEngine) SyncSpecifiedIndexToCloud(folder string, idx MasterIndex) error {
	syncIndexMu.Lock()
	defer syncIndexMu.Unlock()

	data, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	encData, err := e.EncryptData(data)
	if err != nil {
		return err
	}

	subject := fmt.Sprintf("[QMD:INDEX]|v%06d|%d", idx.Version, time.Now().Unix())
	meta := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"ver":%d}`, idx.Version)))
	mimeData := e.BuildMIMEMessage(subject, meta, "master_index.dat", encData)

	tempClient := NewIMAPClient(GlobalConfig.IMAPServer, GlobalConfig.Email, GlobalConfig.IMAPAuthCode)
	if err := tempClient.Connect(); err != nil {
		return err
	}
	defer tempClient.Close()

	if err := tempClient.AppendDirectMessage(folder, subject, meta, mimeData); err != nil {
		return err
	}

	e.SetCachedFolderIndex(folder, idx)
	return nil
}

// SyncMasterIndexToCloud 同步主索引到云端（互斥防堆叠，严格扫描并物理删除所有旧版本，确保邮箱内永远只留 1 封）
func (e *StorageEngine) SyncMasterIndexToCloud() error {
	syncIndexMu.Lock()
	defer syncIndexMu.Unlock()

	e.indexLock.RLock()
	data, err := json.Marshal(e.masterIndex)
	ver := e.masterIndex.Version
	totalFiles := len(e.masterIndex.Files)
	e.indexLock.RUnlock()
	if err != nil {
		e.Log(fmt.Sprintf("❌ 主索引序列化失败: %v", err))
		return err
	}

	encData, err := e.EncryptData(data)
	if err != nil {
		e.Log(fmt.Sprintf("❌ 主索引加密失败: %v", err))
		return err
	}

	subject := fmt.Sprintf("[QMD:INDEX]|v%06d|%d", ver, time.Now().Unix())
	meta := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"ver":%d}`, ver)))
	mimeData := e.BuildMIMEMessage(subject, meta, "master_index.dat", encData)

	// 1. 写入云端新主索引
	if err := e.imapClient.AppendDirectMessage(GlobalConfig.StorageFolder, subject, meta, mimeData); err != nil {
		e.Log(fmt.Sprintf("❌ 写入云端主索引邮件失败: %v", err))
		return fmt.Errorf("写入新索引邮件失败: %w", err)
	}
	e.Log(fmt.Sprintf("[主索引] 成功提交新版主索引邮件: %s (纳管项目: %d)", subject, totalFiles))
	e.indexLock.RLock()
	curCopy := cloneMasterIndex(e.masterIndex)
	e.indexLock.RUnlock()
	configLock.RLock()
	curFolderName := GlobalConfig.StorageFolder
	configLock.RUnlock()
	e.SetCachedFolderIndex(curFolderName, curCopy)

	// 2. 彻底清理所有旧版本历史索引邮件：通过报头扫描拉取所有 [QMD:INDEX]|，保留最新版本，清除其余全部
	time.Sleep(300 * time.Millisecond)
	headers, hErr := e.imapClient.FetchAllHeadersFast()
	if hErr == nil {
		type IndexMail struct {
			SeqNum  uint32
			Version int64
		}
		var indexMails []IndexMail

		for _, h := range headers {
			if strings.HasPrefix(h.Subject, "[QMD:INDEX]|") {
				parts := strings.Split(h.Subject, "|")
				var mailVer int64 = 0
				if len(parts) >= 2 && strings.HasPrefix(parts[1], "v") {
					if v, err := strconv.ParseInt(parts[1][1:], 10, 64); err == nil {
						mailVer = v
					}
				}
				indexMails = append(indexMails, IndexMail{SeqNum: h.SeqNum, Version: mailVer})
			}
		}

		if len(indexMails) > 1 {
			// 按版本号降序排序，第 0 项是最新版本
			sort.Slice(indexMails, func(i, j int) bool {
				return indexMails[i].Version > indexMails[j].Version
			})
			var toDelete []uint32
			for i := 1; i < len(indexMails); i++ {
				toDelete = append(toDelete, indexMails[i].SeqNum)
			}
			if len(toDelete) > 0 {
				e.Log(fmt.Sprintf("[主索引] 发现 %d 封旧版历史索引邮件，执行彻底物理清除 (EXPUNGE)...", len(toDelete)))
				_ = e.imapClient.DeleteAndExpunge(toDelete)
			}
		}
	}

	return nil
}

// MoveItems 零流量批量移动文件和文件夹（含内部所有嵌套层级的子文件，解死锁并记录详尽日志）
func (e *StorageEngine) MoveItems(paths []string, targetDir string) error {
	e.indexLock.Lock()

	targetDir = strings.TrimSpace(targetDir)
	if targetDir == "" {
		targetDir = "/"
	}
	if targetDir != "/" && strings.HasSuffix(targetDir, "/") {
		targetDir = strings.TrimSuffix(targetDir, "/")
	}

	e.Log(fmt.Sprintf("[移动] 准备移动 %d 个项目至目标目录 [%s]...", len(paths), targetDir))
	movedCount := 0

	for _, srcPath := range paths {
		srcPath = strings.TrimSpace(srcPath)
		if srcPath == "" || srcPath == "/" {
			continue
		}

		node, exists := e.masterIndex.Files[srcPath]
		if !exists {
			e.Log(fmt.Sprintf("⚠️ 移动跳过: 源项目不存在 [%s]", srcPath))
			continue
		}

		// 防死锁循环移动：不能移动到自身或自身的深层子目录下
		if node.IsDir {
			if targetDir == srcPath || strings.HasPrefix(targetDir, srcPath+"/") {
				e.Log(fmt.Sprintf("⚠️ 移动跳过: 禁止将文件夹移入其自身或其子目录 [%s]", srcPath))
				continue
			}
		}

		var newBase string
		if targetDir == "/" {
			newBase = "/" + node.Name
		} else {
			newBase = targetDir + "/" + node.Name
		}

		delete(e.masterIndex.Files, srcPath)
		node.VirtualPath = newBase
		e.masterIndex.Files[newBase] = node
		movedCount++
		e.Log(fmt.Sprintf("[移动] %s -> %s", srcPath, newBase))

		// 若为文件夹，递归重定位其内部所有的深层子文件与子文件夹
		if node.IsDir {
			prefix := srcPath + "/"
			for p, child := range e.masterIndex.Files {
				if strings.HasPrefix(p, prefix) {
					subRel := p[len(prefix):]
					newSubPath := newBase + "/" + subRel
					delete(e.masterIndex.Files, p)
					child.VirtualPath = newSubPath
					e.masterIndex.Files[newSubPath] = child
					e.Log(fmt.Sprintf("[移动子项] %s -> %s", p, newSubPath))
				}
			}
		}
	}

	e.masterIndex.Version++
	e.masterIndex.LastUpdated = time.Now()

	// 关键修复：必须在调用 SyncMasterIndexToCloud 之前释放 indexLock，彻底消灭死锁！
	e.indexLock.Unlock()

	if movedCount > 0 {
		if err := e.SyncMasterIndexToCloud(); err != nil {
			e.Log(fmt.Sprintf("❌ 移动后主索引同步云端失败: %v", err))
			return err
		}
		e.Log(fmt.Sprintf("✔ 成功移动 %d 项至 [%s]，主索引已同步", movedCount, targetDir))
	} else {
		e.Log("⚠️ 未执行任何项目的实际移动")
	}

	return nil
}

// resolveCopyNameConflict 针对复制冲突计算 "- 副本" 名称
func (e *StorageEngine) resolveCopyNameConflict(targetDir, originalName string, isDir bool) string {
	var nameWithoutExt, ext string
	if isDir {
		nameWithoutExt = originalName
		ext = ""
	} else {
		ext = filepath.Ext(originalName)
		nameWithoutExt = strings.TrimSuffix(originalName, ext)
	}

	candidate := fmt.Sprintf("%s - 副本%s", nameWithoutExt, ext)
	p := targetDir + "/" + candidate
	if targetDir == "/" {
		p = "/" + candidate
	}
	if _, exists := e.masterIndex.Files[p]; !exists {
		return candidate
	}

	for i := 2; i <= 999; i++ {
		candidate = fmt.Sprintf("%s - 副本 (%d)%s", nameWithoutExt, i, ext)
		p = targetDir + "/" + candidate
		if targetDir == "/" {
			p = "/" + candidate
		}
		if _, exists := e.masterIndex.Files[p]; !exists {
			return candidate
		}
	}
	return fmt.Sprintf("%s - 副本_%d%s", nameWithoutExt, time.Now().Unix(), ext)
}

// CopyItems 本空间内通过 IMAP 协议原子克隆分片邮件并安全录入主索引 (保证下载、缩略图与对账 100% 完好)
func (e *StorageEngine) CopyItems(paths []string, targetDir string) error {
	targetDir = strings.TrimSpace(targetDir)
	if targetDir == "" {
		targetDir = "/"
	}
	if targetDir != "/" && strings.HasSuffix(targetDir, "/") {
		targetDir = strings.TrimSuffix(targetDir, "/")
	}

	configLock.RLock()
	currentFolder := GlobalConfig.StorageFolder
	configLock.RUnlock()
	actualFolder := e.imapClient.FindActualMailboxName(currentFolder)

	// 1. 收集待复制的所有节点
	e.indexLock.RLock()
	type CopyTaskItem struct {
		SourcePath string
		DestPath   string
		DestName   string
		Node       FileNode
	}
	var itemsToCopy []CopyTaskItem
	var allFileIDs []string

	for _, srcPath := range paths {
		srcPath = strings.TrimSpace(srcPath)
		if srcPath == "" || srcPath == "/" {
			continue
		}
		rootNode, exists := e.masterIndex.Files[srcPath]
		if !exists {
			continue
		}
		// 防自身嵌套
		if rootNode.IsDir {
			if targetDir == srcPath || strings.HasPrefix(targetDir, srcPath+"/") {
				continue
			}
		}

		destName := rootNode.Name
		var newBase string
		if targetDir == "/" {
			newBase = "/" + destName
		} else {
			newBase = targetDir + "/" + destName
		}
		if _, clash := e.masterIndex.Files[newBase]; clash {
			destName = e.resolveCopyNameConflict(targetDir, rootNode.Name, rootNode.IsDir)
			if targetDir == "/" {
				newBase = "/" + destName
			} else {
				newBase = targetDir + "/" + destName
			}
		}

		itemsToCopy = append(itemsToCopy, CopyTaskItem{
			SourcePath: srcPath,
			DestPath:   newBase,
			DestName:   destName,
			Node:       rootNode,
		})
		if !rootNode.IsDir && rootNode.ID != "" {
			allFileIDs = append(allFileIDs, rootNode.ID)
		}

		if rootNode.IsDir {
			prefix := srcPath + "/"
			for p, child := range e.masterIndex.Files {
				if strings.HasPrefix(p, prefix) {
					subRel := p[len(prefix):]
					newSubPath := newBase + "/" + subRel
					itemsToCopy = append(itemsToCopy, CopyTaskItem{
						SourcePath: p,
						DestPath:   newSubPath,
						DestName:   child.Name,
						Node:       child,
					})
					if !child.IsDir && child.ID != "" {
						allFileIDs = append(allFileIDs, child.ID)
					}
				}
			}
		}
	}
	e.indexLock.RUnlock()

	if len(itemsToCopy) == 0 {
		return nil
	}

	e.Log(fmt.Sprintf("[复制] 准备通过 IMAP 协议在空间 [%s] 内复制 %d 个项目至 [%s]...", ToDisplayFolder(currentFolder), len(itemsToCopy), targetDir))

	// 2. 依据用户明确指令：同隔离文件夹内部也调用 IMAP COPY 协议在云端克隆切片邮件
	_ = e.imapClient.EnsureAndSelectFolder(currentFolder)
	if len(allFileIDs) > 0 {
		var seqs []uint32
		headers, hErr := e.imapClient.FetchAllHeadersFast()
		if hErr == nil {
			for _, h := range headers {
				for _, fid := range allFileIDs {
					if strings.Contains(h.Subject, "|"+fid+"|") {
						seqs = append(seqs, h.SeqNum)
						break
					}
				}
			}
		}
		if len(seqs) > 0 {
			if err := e.imapClient.CopyMessages(seqs, currentFolder, actualFolder); err != nil {
				e.Log(fmt.Sprintf("⚠️ 云端分片邮件 IMAP COPY 提示: %v (继续保障元数据完整性)", err))
			} else {
				e.Log(fmt.Sprintf("✔ [分片克隆] 成功在当前空间通过 IMAP COPY 克隆 %d 封切片邮件", len(seqs)))
			}
		}
	}

	// 3. 录入主索引，严格保留原始 ID、密钥与切片结构，保证下载、预览、缩略图与对账 100% 完好！
	e.indexLock.Lock()
	for _, item := range itemsToCopy {
		copied := item.Node
		copied.Name = item.DestName
		copied.VirtualPath = item.DestPath
		copied.ModTime = time.Now()
		// 关键：保留 ID、CipherKeyBase64、Chunks、ThumbItemID、ThumbVolID、ThumbBase64
		// 严禁改写为 "copy_..."，保证与云端切片邮件及缩略图卷完全一致！
		e.masterIndex.Files[item.DestPath] = copied

		// 补齐中间父目录节点
		parts := strings.Split(strings.Trim(item.DestPath, "/"), "/")
		curDir := ""
		for i := 0; i < len(parts)-1; i++ {
			curDir += "/" + parts[i]
			if _, ok := e.masterIndex.Files[curDir]; !ok {
				e.masterIndex.Files[curDir] = FileNode{
					ID:          fmt.Sprintf("dir_%d_%d", time.Now().UnixNano(), i),
					Name:        parts[i],
					VirtualPath: curDir,
					IsDir:       true,
					ModTime:     time.Now(),
				}
			}
		}
	}

	e.masterIndex.Version++
	e.masterIndex.LastUpdated = time.Now()
	e.indexLock.Unlock()

	_ = e.SyncMasterIndexToCloud()
	e.Log(fmt.Sprintf("✔ 成功在空间 [%s] 内复制 %d 项至 [%s]，主索引已同步", ToDisplayFolder(currentFolder), len(itemsToCopy), targetDir))
	return nil
}

// RenameFileNode 重命名文件或文件夹（零流量毫秒级修改主索引）
func (e *StorageEngine) RenameFileNode(oldPath, newPath string) error {
	e.indexLock.Lock()
	node, exists := e.masterIndex.Files[oldPath]
	if !exists {
		e.indexLock.Unlock()
		e.Log(fmt.Sprintf("❌ 重命名失败: 原文件不存在 [%s]", oldPath))
		return fmt.Errorf("文件或目录未找到")
	}

	delete(e.masterIndex.Files, oldPath)
	node.VirtualPath = newPath
	node.Name = filepath.Base(newPath)
	e.masterIndex.Files[newPath] = node

	// 若为文件夹，同步递归更新其下所有子文件的 VirtualPath
	if node.IsDir {
		for p, child := range e.masterIndex.Files {
			if strings.HasPrefix(p, oldPath+"/") {
				newChildPath := newPath + p[len(oldPath):]
				delete(e.masterIndex.Files, p)
				child.VirtualPath = newChildPath
				e.masterIndex.Files[newChildPath] = child
			}
		}
	}

	e.masterIndex.Version++
	e.masterIndex.LastUpdated = time.Now()
	e.indexLock.Unlock()

	e.Log(fmt.Sprintf("[重命名] %s -> %s", oldPath, newPath))
	return e.SyncMasterIndexToCloud()
}

// DeleteFileFromStorage 删除单个文件或文件夹
func (e *StorageEngine) DeleteFileFromStorage(vPath string) error {
	return e.DeleteMultipleItems([]string{vPath})
}

// DeleteMultipleItems 批量深度递归级联删除文件与文件夹（包含多层嵌套文件夹里面的所有东西）
func (e *StorageEngine) DeleteMultipleItems(paths []string) error {
	e.indexLock.RLock()
	expandedTargets := make(map[string]FileNode)
	for _, targetPath := range paths {
		targetPath = strings.TrimSpace(targetPath)
		if targetPath == "" {
			continue
		}
		if node, exists := e.masterIndex.Files[targetPath]; exists {
			expandedTargets[targetPath] = node
			if node.IsDir {
				prefix := targetPath + "/"
				for p, sub := range e.masterIndex.Files {
					if strings.HasPrefix(p, prefix) {
						expandedTargets[p] = sub
					}
				}
			}
		}
	}
	e.indexLock.RUnlock()

	if len(expandedTargets) == 0 {
		return nil
	}

	e.Log(fmt.Sprintf("[删除] 准备彻底删除 %d 个项目: %v", len(expandedTargets), paths))

	// 1. 收集待删除文件的所有唯一 FID, ShareID, Chunk SHA256 与 Seq (具备引用保护与多维关联匹配)
	targetFids := make(map[string]bool)
	targetShareIDs := make(map[string]bool)
	targetChunkHashes := make(map[string]bool)
	targetSeqs := make(map[string]bool)

	e.indexLock.RLock()
	for _, node := range expandedTargets {
		if !node.IsDir {
			stillReferenced := false
			for p, other := range e.masterIndex.Files {
				if _, inDel := expandedTargets[p]; inDel {
					continue
				}
				if !other.IsDir && (other.ID == node.ID || (other.SHA256 != "" && other.SHA256 == node.SHA256)) {
					stillReferenced = true
					break
				}
			}
			if !stillReferenced {
				if node.ID != "" {
					targetFids[node.ID] = true
				}
				if node.ShareID != "" {
					if len(node.SHA256) >= 12 {
						targetShareIDs[node.ShareID+"|"+node.SHA256[:12]] = true
					} else {
						targetShareIDs[node.ShareID] = true
					}
				}
				if node.SHA256 != "" {
					if len(node.SHA256) >= 12 {
						targetChunkHashes[node.SHA256[:12]] = true
					}
					targetChunkHashes[node.SHA256] = true
				}
				for _, c := range node.Chunks {
					if c.SHA256 != "" {
						if len(c.SHA256) >= 12 {
							targetChunkHashes[c.SHA256[:12]] = true
						}
						targetChunkHashes[c.SHA256] = true
					}
					if c.Seq != "" {
						targetSeqs[c.Seq] = true
					}
				}
				if node.StorageType == StorageTypeBundled && node.BundleSeq != "" {
					bundleStillUsed := false
					for p, other := range e.masterIndex.Files {
						if _, inDel := expandedTargets[p]; inDel {
							continue
						}
						if other.StorageType == StorageTypeBundled && other.BundleSeq == node.BundleSeq {
							bundleStillUsed = true
							break
						}
					}
					if !bundleStillUsed {
						targetSeqs[node.BundleSeq] = true
					}
				}
			}
			e.thumbLock.Lock()
			delete(e.thumbCache, node.ID)
			e.thumbLock.Unlock()
		}
	}
	e.indexLock.RUnlock()

	// 2. 单次全盘报头极速拉取 + 内存多维精准匹配 (支持常规切片与分享切片彻底删除)
	var toDeleteSeqs []uint32
	if len(targetFids) > 0 || len(targetShareIDs) > 0 || len(targetChunkHashes) > 0 || len(targetSeqs) > 0 {
		e.Log(fmt.Sprintf("[删除] 正在快速匹配待清理文件的云端关联切片邮件..."))
		headers, hErr := e.imapClient.FetchAllHeadersFast()
		if hErr == nil {
			for _, h := range headers {
				matched := false
				for fid := range targetFids {
					if strings.Contains(h.Subject, fid) {
						matched = true
						break
					}
				}
				if !matched && h.XDriveMeta != "" {
					for fid := range targetFids {
						if strings.Contains(h.XDriveMeta, fid) {
							matched = true
							break
						}
					}
				}
				if !matched && (strings.HasPrefix(h.Subject, "[QMD:SHARE:DATA]|") || strings.HasPrefix(h.Subject, "[QMD:DATA]|") || strings.HasPrefix(h.Subject, "[QMD:BUNDLE]|")) {
					for sidKey := range targetShareIDs {
						if parts := strings.SplitN(sidKey, "|", 2); len(parts) == 2 {
							if strings.Contains(h.Subject, parts[0]) && strings.Contains(h.Subject, parts[1]) {
								matched = true
								break
							}
						} else if strings.Contains(h.Subject, sidKey) {
							matched = true
							break
						}
					}
					if !matched {
						for hsh := range targetChunkHashes {
							if strings.Contains(h.Subject, hsh) {
								matched = true
								break
							}
						}
					}
					if !matched {
						for seq := range targetSeqs {
							if strings.Contains(h.Subject, seq) {
								matched = true
								break
							}
						}
					}
				}
				if matched {
					toDeleteSeqs = append(toDeleteSeqs, h.SeqNum)
				}
			}
		}
	}

	// 物理清除大文件切片邮件 (STORE +FLAGS \Deleted 并 EXPUNGE)
	if len(toDeleteSeqs) > 0 {
		e.Log(fmt.Sprintf("[删除] 正在向 QQ 邮箱发送物理清除指令，彻底抹除 %d 封切片邮件...", len(toDeleteSeqs)))
		if err := e.imapClient.DeleteAndExpunge(toDeleteSeqs); err != nil {
			e.Log(fmt.Sprintf("❌ 云端切片邮件清除失败: %v", err))
		} else {
			e.Log(fmt.Sprintf("✔ 关联云端切片邮件 (%d 封) 已全部彻底物理抹除 (EXPUNGE 成功)", len(toDeleteSeqs)))
		}
	} else {
		e.Log("[删除] 未发现需要清理的切片邮件 (可能为纯空文件夹或已被删除)")
	}

	// 从主索引中彻底销毁所有被删除的文件和子文件夹节点
	e.indexLock.Lock()
	for p := range expandedTargets {
		delete(e.masterIndex.Files, p)
	}
	e.masterIndex.Version++
	e.masterIndex.LastUpdated = time.Now()
	e.indexLock.Unlock()

	// 同步新主索引至云端
	if err := e.SyncMasterIndexToCloud(); err != nil {
		e.Log(fmt.Sprintf("❌ 删除后主索引同步云端失败: %v", err))
		return err
	}

	e.Log(fmt.Sprintf("✔ 成功删除 %d 个项目，主索引更新次数:  %d 并同步云端！", len(expandedTargets), e.masterIndex.Version))
	return nil
}

// RunDeepCompaction 深度装箱组合整理（BFD 算法重组零散文件与瘦身缩略图，根据用户参数 chunk_size_bytes 严格装箱）
func (e *StorageEngine) RunDeepCompaction() (int, int, error) {
	e.indexLock.RLock()
	maxBinBytes := GlobalConfig.ChunkSizeBytes
	if maxBinBytes <= 0 {
		maxBinBytes = 52428799
	}

	// 1. 扫描全盘所有体积小于单分片上限 75% 的零散独立小文件
	var smallFiles []FileNode
	for _, f := range e.masterIndex.Files {
		if !f.IsDir && f.StorageType == StorageTypeChunked && f.Size < (maxBinBytes*3/4) {
			smallFiles = append(smallFiles, f)
		}
	}
	e.indexLock.RUnlock()

	if len(smallFiles) < 2 {
		e.Log("✔ 深度整理：全盘文件均已处于最优紧凑状态或零散文件不足，无需重组")
		return 0, 0, nil
	}

	e.Log(fmt.Sprintf("[深度整理] 发现 %d 个零散小文件，按目录亲和性与 BFD 最佳适应装箱规划中 (单包上限: %s)...", len(smallFiles), formatBytes(maxBinBytes)))

	// 按目录亲和性分组
	dirGroups := make(map[string][]FileNode)
	for _, sf := range smallFiles {
		dir := path.Dir(sf.VirtualPath)
		dirGroups[dir] = append(dirGroups[dir], sf)
	}

	totalSavedMails := 0
	totalNewPacks := 0

	for dir, group := range dirGroups {
		if len(group) < 2 {
			continue
		}

		// 按文件大小降序排序 (Best-Fit Decreasing)
		sort.Slice(group, func(i, j int) bool {
			return group[i].Size > group[j].Size
		})

		type Bin struct {
			Files      []FileNode
			TotalBytes int64
		}
		var bins []Bin

		for _, item := range group {
			placed := false
			for bi := range bins {
				if bins[bi].TotalBytes+item.Size <= maxBinBytes {
					bins[bi].Files = append(bins[bi].Files, item)
					bins[bi].TotalBytes += item.Size
					placed = true
					break
				}
			}
			if !placed {
				bins = append(bins, Bin{
					Files:      []FileNode{item},
					TotalBytes: item.Size,
				})
			}
		}

		// 对每一个多于 1 个文件的箱子执行真实的物理重组
		for _, b := range bins {
			if len(b.Files) < 2 {
				continue
			}

			newSeq := fmt.Sprintf("QMD_BUNDLE_%d_%d", time.Now().UnixNano(), len(b.Files))
			var zipBuf bytes.Buffer
			zw := zip.NewWriter(&zipBuf)

			var oldSeqsToDelete []uint32
			var successPacked []FileNode

			for _, fn := range b.Files {
				tempF := filepath.Join("temp", "pack_"+fn.ID)
				if err := e.DownloadFileStream(fn, tempF, nil); err == nil {
					if fData, rErr := os.ReadFile(tempF); rErr == nil {
						_ = os.Remove(tempF)
						if w, cErr := zw.Create(fn.Name); cErr == nil {
							_, _ = w.Write(fData)
							successPacked = append(successPacked, fn)

							// 收集旧切片邮件准备物理删除 (SEARCH + 报头全盘扫描双重兜底)
							seqs, _ := e.imapClient.SearchMessageSeqs(fmt.Sprintf(`SUBJECT "%s"`, fn.ID))
							if len(seqs) == 0 {
								if allH, hErr := e.imapClient.FetchAllHeadersFast(); hErr == nil {
									for _, h := range allH {
										if strings.Contains(h.Subject, fn.ID) {
											seqs = append(seqs, h.SeqNum)
										}
									}
								}
							}
							oldSeqsToDelete = append(oldSeqsToDelete, seqs...)
						}
					}
				}
			}
			_ = zw.Close()

			if len(successPacked) < 2 {
				continue
			}

			// 加密并上传新大包
			if encZip, err := e.EncryptData(zipBuf.Bytes()); err == nil {
				metaJSON := fmt.Sprintf(`{"type":"bundle","seq":"%s","count":%d}`, newSeq, len(successPacked))
				metaBase64 := base64.StdEncoding.EncodeToString([]byte(metaJSON))
				newSubject := fmt.Sprintf("[QMD:BUNDLE]|%s|%d|%s", newSeq, len(successPacked), newSeq)
				mimeData := e.BuildMIMEMessage(newSubject, metaBase64, newSeq+".7z", encZip)

				if err := e.imapClient.AppendDirectMessage(GlobalConfig.StorageFolder, newSubject, metaBase64, mimeData); err == nil {
					// 物理清除旧切片邮件
					if len(oldSeqsToDelete) > 0 {
						_ = e.imapClient.DeleteAndExpunge(oldSeqsToDelete)
					}

					// 更新主索引中的指针为合包格式
					e.indexLock.Lock()
					for _, packedNode := range successPacked {
						if cur, ok := e.masterIndex.Files[packedNode.VirtualPath]; ok {
							cur.StorageType = StorageTypeBundled
							cur.BundleSeq = newSeq
							cur.InnerPath = packedNode.Name
							cur.Chunks = nil
							e.masterIndex.Files[packedNode.VirtualPath] = cur
						}
					}
					e.indexLock.Unlock()

					savedInThisBin := len(successPacked) - 1
					totalSavedMails += savedInThisBin
					totalNewPacks++
					e.Log(fmt.Sprintf("✔ 成功将目录 [%s] 的 %d 个零散文件合并为紧凑大包 %s (节省 %d 封邮件配额)", dir, len(successPacked), newSeq+".7z", savedInThisBin))
				}
			}
		}
	}

	if totalSavedMails > 0 {
		e.masterIndex.Version++
		e.masterIndex.LastUpdated = time.Now()
		_ = e.SyncMasterIndexToCloud()
		e.Log(fmt.Sprintf("🚀 深度整理全部完成！共创建 %d 个新紧凑大包，累计为 QQ 邮箱释放 %d 封邮件配额！", totalNewPacks, totalSavedMails))
	} else {
		e.Log("✔ 深度整理完成：全盘文件均已达到紧凑状态")
	}

	return totalSavedMails, totalNewPacks, nil
}

// DownloadFileStream 文件流式下载（按 node.ID 定位切片，双重兜底定位，解决 0 字节 Bug）
// DownloadStream 后端原生流式下载任务（磁盘秒级预分配 + 多线程并发拉取 + 偏移量无序写入 WriteAt）

// GetDownloadClients 获取或维护多线程高速下载常驻连接池 (包含主连接 e.imapClient 与常驻副连接)
func (e *StorageEngine) GetDownloadClients(targetConcurrency int) []*IMAPClient {
	e.poolMu.Lock()
	defer e.poolMu.Unlock()

	if targetConcurrency < 2 {
		targetConcurrency = 2
	}
	if targetConcurrency > 8 {
		targetConcurrency = 8
	}

	// 1. 确保主连接首先纳入（主连接已登录并选中文件夹，零握手开销）
	clients := []*IMAPClient{e.imapClient}
	e.Log("[连接池] 链路 #1 (主常驻长连接) 状态: 已就绪")

	// 2. 检查现有辅助长连接池：严格核验账号血统，断开并剔除任何非当前账号的异族连接
	var validPool []*IMAPClient
	for _, c := range e.downloadPool {
		if c != nil {
			if !strings.EqualFold(c.email, GlobalConfig.Email) || c.authCode != GlobalConfig.IMAPAuthCode {
				c.Close()
			} else {
				validPool = append(validPool, c)
			}
		}
	}
	e.downloadPool = validPool

	// 复用当前账号已就绪长连接，纯只读选定目标目录（绝不发送 CREATE）
	for idx, c := range e.downloadPool {
		linkID := idx + 2
		if c != nil && len(clients) < targetConcurrency {
			if !c.connected || c.conn == nil {
				e.Log(fmt.Sprintf("[连接池] 正在唤醒重连辅助链路 #%d...", linkID))
				if cErr := c.Connect(); cErr == nil {
					_ = c.SelectFolderOnly(GlobalConfig.StorageFolder)
					clients = append(clients, c)
					e.Log(fmt.Sprintf("[连接池] 辅助长连接 #%d 恢复并成功接入目录: %s", linkID, GlobalConfig.StorageFolder))
				} else {
					e.Log(fmt.Sprintf("[连接池] 辅助长连接 #%d 恢复受阻: %v", linkID, cErr))
				}
			} else {
				_ = c.SelectFolderOnly(GlobalConfig.StorageFolder)
				clients = append(clients, c)
				e.Log(fmt.Sprintf("[连接池] 辅助长连接 #%d 状态: 已就绪 (接入目录: %s)", linkID, GlobalConfig.StorageFolder))
			}
		}
	}

	// 3. 若连接数未达标，为当前账号平滑增补全新长连接
	for len(clients) < targetConcurrency {
		linkID := len(clients) + 1
		time.Sleep(300 * time.Millisecond)
		e.Log(fmt.Sprintf("[连接池] 正在握手建立全新辅助长连接 #%d (目标并发: %d)...", linkID, targetConcurrency))
		cli := NewIMAPClient(GlobalConfig.IMAPServer, GlobalConfig.Email, GlobalConfig.IMAPAuthCode)
		cli.Logger = e.LogWithLevel
		if cErr := cli.Connect(); cErr == nil {
			_ = cli.SelectFolderOnly(GlobalConfig.StorageFolder)
			e.downloadPool = append(e.downloadPool, cli)
			clients = append(clients, cli)
			e.Log(fmt.Sprintf("[连接池] ✔ 成功建立并验证第 %d 条高速下载专属 IMAP 链路", len(clients)))
		} else {
			e.Log(fmt.Sprintf("[连接池] ⚠️ 辅助下载链路 #%d 握手受限 (%v)，将采用已就绪的 %d 条链路并发", linkID, cErr, len(clients)))
			break
		}
	}

	e.Log(fmt.Sprintf("[连接池] 🚀 最终就绪链路数: %d 条 (主链路 + %d 条副链路)", len(clients), len(clients)-1))
	return clients
}

// CloseDownloadPool 关闭并清理辅助连接池
func (e *StorageEngine) CloseDownloadPool() {
	e.poolMu.Lock()
	defer e.poolMu.Unlock()
	for _, c := range e.downloadPool {
		if c != nil {
			c.Close()
		}
	}
	e.downloadPool = nil
}

// LocateChunkSeqsForNode 严谨定位并精准按分片序号排重归一化切片邮件序列 (支持常规切片与分享切片多维精确定位，不漏片不错位)
func (e *StorageEngine) LocateChunkSeqsForNode(node *FileNode, headers []MailHeaderSummary) []uint32 {
	targetID := node.ID
	hash12 := ""
	if len(node.SHA256) >= 12 {
		hash12 = node.SHA256[:12]
	}

	partSeqMap := make(map[int]uint32)
	chunkSize := GlobalConfig.ChunkSizeBytes
	if chunkSize <= 0 {
		chunkSize = 52428799
	}
	node.EnsureChunks(chunkSize)
	totalParts := len(node.Chunks)
	if totalParts == 0 {
		totalParts = 1
	}

	for _, h := range headers {
		if strings.HasPrefix(h.Subject, "[QMD:DATA]|") || strings.HasPrefix(h.Subject, "[QMD:SHARE:DATA]|") {
			isTargetFile := false
			if hash12 != "" && strings.Contains(h.Subject, hash12) {
				isTargetFile = true
			} else if targetID != "" && strings.Contains(h.Subject, "|"+targetID+"|") {
				isTargetFile = true
			} else if node.ShareID != "" && strings.Contains(h.Subject, "|"+node.ShareID+"|") && (hash12 == "" || strings.Contains(h.Subject, hash12)) {
				isTargetFile = true
			} else if targetID != "" && h.XDriveMeta != "" {
				if metaBytes, err := base64.StdEncoding.DecodeString(h.XDriveMeta); err == nil {
					if strings.Contains(string(metaBytes), fmt.Sprintf(`"fid":"%s"`, targetID)) {
						isTargetFile = true
					}
				}
			}

			if isTargetFile {
				matchedPartIdx := 0
				parts := strings.Split(h.Subject, "|")
				for _, p := range parts {
					if strings.Contains(p, "-") {
						var pIdx, tot int
						if n, _ := fmt.Sscanf(p, "%d-%d", &pIdx, &tot); n >= 1 && pIdx >= 1 {
							matchedPartIdx = pIdx
							if _, exists := partSeqMap[pIdx]; !exists {
								partSeqMap[pIdx] = h.SeqNum
							}
						}
					}
				}
				if matchedPartIdx == 0 && h.XDriveMeta != "" {
					if metaBytes, err := base64.StdEncoding.DecodeString(h.XDriveMeta); err == nil {
						var m map[string]interface{}
						if json.Unmarshal(metaBytes, &m) == nil {
							if idx, ok := m["idx"].(float64); ok && int(idx) >= 1 {
								if _, exists := partSeqMap[int(idx)]; !exists {
									partSeqMap[int(idx)] = h.SeqNum
								}
							}
						}
					}
				}
			}
		}
	}

	seqs := make([]uint32, 0, len(partSeqMap))
	for i := 1; i <= totalParts; i++ {
		if s, ok := partSeqMap[i]; ok {
			seqs = append(seqs, s)
		}
	}
	return seqs
}


func (e *StorageEngine) DownloadStream(task *TransferTask, node FileNode, destPath string) error {
	if node.StorageType == StorageTypeBundled {
		bundledStart := time.Now()
		e.tasksLock.Lock()
		task.Status = "running"
		task.Stage = StageDownloading
		task.StageDetail = "正在从合包中解压提取文件..."
		task.TotalChunks = 1
		task.TotalSize = node.Size
		e.tasksLock.Unlock()

		err := e.DownloadBundledFile(node, destPath)
		if err != nil {
			e.tasksLock.Lock()
			task.Status = "failed"
			task.Error = err.Error()
			e.tasksLock.Unlock()
			e.logTask(task, "❌ 合包提取失败: "+err.Error())
			return err
		}
		e.tasksLock.Lock()
		task.Transferred = node.Size
		task.ProgressPct = 100.0
		task.CompletedChunks = []int{1}
		task.Status = "completed"
		task.Stage = StageCompleted
		task.StageDetail = "✔ 合包解压就绪，文件已完整组装"
		task.SpeedMBs = 0
		task.ElapsedSec = time.Since(bundledStart).Seconds()
		e.tasksLock.Unlock()
		e.logTask(task, fmt.Sprintf("✔ 合包文件提取完成: %s", destPath))
		return nil
	}

	if e.imapClient != nil {
		e.imapClient.ResetInterrupt()
	}
	ctx, cancel := context.WithCancel(context.Background())
	e.RegisterCancelToken(task.TaskID, cancel)
	defer e.RemoveCancelToken(task.TaskID)

	var (
		activeClientsMu sync.Mutex
		activeClients   []*IMAPClient
	)

	type activeWorkerInfo struct {
		linkID   int
		chunkIdx int
		client   *IMAPClient
	}

	var (
		inFlightWorkersMu sync.Mutex
		inFlightWorkers   = make(map[int]activeWorkerInfo)
	)

	// 极速取消拦截器：一旦任务被取消，瞬间无锁打断所有底层网络 Socket (包括链路 #1)，毫秒级快速退出，严禁后台偷跑重试
	go func() {
		<-ctx.Done()
		inFlightWorkersMu.Lock()
		for _, info := range inFlightWorkers {
			if info.client != nil {
				info.client.Interrupt()
			}
		}
		inFlightWorkersMu.Unlock()

		activeClientsMu.Lock()
		for _, c := range activeClients {
			if c != nil {
				c.Interrupt()
			}
		}
		activeClientsMu.Unlock()
	}()

	// 注册主动熔断回调：当用户在播放器拖拽进度条到 targetChunk 时，急救链路瞬间打断，辅助链路错峰让位！
	// 彻底杜绝 3 条长连接在同一毫秒内同时断线重连并在腾讯云端 SELECT 目录锁死排队 28 秒的严重瓶颈！
	e.RegisterSeekInterrupter(task.TaskID, func(targetChunk int) {
		inFlightWorkersMu.Lock()
		defer inFlightWorkersMu.Unlock()

		firstFound := false
		for lID, info := range inFlightWorkers {
			if info.client != nil && info.chunkIdx > 1 && info.chunkIdx < targetChunk {
				if !firstFound {
					firstFound = true
					e.LogInfo("STREAM", fmt.Sprintf("⚡ [急救绿色通道] 视频寻道至分片 #%d，【急救链路 #%d】立即掐断旧分片 #%d 独占首发建连特权", targetChunk, lID, info.chunkIdx))
					info.client.Interrupt()
				} else {
					targetLink := lID
					targetCli := info.client
					oldC := info.chunkIdx
					go func(cli *IMAPClient, link int, cIdx int) {
						time.Sleep(350 * time.Millisecond)
						e.LogInfo("STREAM", fmt.Sprintf("⚡ [错峰换道] 辅助链路 #%d 错峰让位，退出旧分片 #%d 传输", link, cIdx))
						cli.Interrupt()
					}(targetCli, targetLink, oldC)
				}
			}
		}
	})
	defer e.RemoveSeekInterrupter(task.TaskID)

	task.Stage = StageDownloading
	task.StageDetail = "正在定位云端切片邮件..."
	e.logTask(task, fmt.Sprintf("开始下载文件: %s (大小: %s)", node.Name, formatBytes(node.Size)))

	// 1. 无论是大文件还是小文件，均在 .part 临时文件上秒级打桩预分配完整体积，避免破坏正式文件状态
	partPath := destPath + ".part"
	destFile, err := os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		task.Status = "failed"
		task.Error = err.Error()
		e.logTask(task, "❌ 创建本地落地文件失败: "+err.Error())
		return err
	}
	defer destFile.Close()

	if node.Size > 0 {
		_ = destFile.Truncate(node.Size)
		e.logTask(task, fmt.Sprintf("✔ 已在磁盘预先开辟完整空间: %s (大小: %s)", node.Name, formatBytes(node.Size)))
	}

	// 2. 检索云端切片邮件：严格按分片序号排重归一化比对（彻底消除重复克隆邮件导致的切片膨胀与死锁）
	var seqs []uint32
	headers, hErr := e.imapClient.FetchAllHeadersFast()
	if hErr == nil {
		seqs = e.LocateChunkSeqsForNode(&node, headers)
	}
	if len(seqs) == 0 {
		_ = destFile.Close()
		_ = os.Remove(destPath)
		task.Status = "failed"
		task.Error = "云端未找到分片邮件"
		e.logTask(task, fmt.Sprintf("❌ 下载失败：未能在云端找到文件 %s 的分片邮件", node.Name))
		return fmt.Errorf("未能从云端定位分片邮件")
	}

	task.TotalChunks = len(seqs)
	task.TotalSize = node.Size

	chunkSize := GlobalConfig.ChunkSizeBytes
	if chunkSize <= 0 {
		chunkSize = 52428799
	}
	rawChunkSize := RawChunkSizeFromTarget(chunkSize)

	// 3. 单分片文件（小文件或单片）：直接单线程快速完成
	if len(seqs) == 1 {
		activeClientsMu.Lock()
		activeClients = []*IMAPClient{e.imapClient}
		activeClientsMu.Unlock()
		if e.imapClient != nil {
			e.imapClient.ResetInterrupt()
		}
		task.TotalChunks = 1
		task.CurrentChunk = 1
		task.StageDetail = "正在拉取并解密数据流..."

		for task.Status == "paused" {
			time.Sleep(200 * time.Millisecond)
		}
		if task.Status == "cancelled" {
			_ = destFile.Close()
			_ = os.Remove(destPath)
			return nil
		}

		singleStart := time.Now()
		var rawMsg []byte
		var err error
		isHTTP := false

		configLock.RLock()
		cfgHTTPConc := GlobalConfig.HTTPDownloadConcurrency
		curFolder := GlobalConfig.StorageFolder
		configLock.RUnlock()

		if GlobalWebmailClient != nil && GlobalWebmailClient.IsSessionValid() && cfgHTTPConc > 0 {
			targetBox := ""
			if e.imapClient != nil {
				targetBox = e.imapClient.folder
			}
			if targetBox == "" {
				targetBox = curFolder
			}
			hash12 := ""
			if len(node.SHA256) >= 12 {
				hash12 = node.SHA256[:12]
			}
			if wmMap, wmErr := GlobalWebmailClient.LocateWebmailChunkMap(targetBox, hash12, node.ID); wmErr == nil && len(wmMap) > 0 {
				if wmItem, ok := wmMap[1]; ok {
					isHTTP = true
					e.LogInfo("DOWNLOAD", fmt.Sprintf("⚡ [HTTP免Base64直下] 单分片文件 %s 正在通过 Webmail 极速通道拉取...", node.Name))
					httpDlProgress := func(n int) bool {
						if task.Status == "cancelled" || ctx.Err() != nil {
							return false
						}
						task.Transferred += int64(n)
						if task.Transferred > task.TotalSize {
							task.Transferred = task.TotalSize
						}
						if task.TotalSize > 0 {
							task.ProgressPct = float64(task.Transferred) / float64(task.TotalSize) * 100.0
						}
						elapsed := time.Since(singleStart).Seconds()
						if elapsed > 0 {
							task.SpeedMBs = (float64(task.Transferred) / (1024 * 1024)) / elapsed
							task.ElapsedSec = elapsed
						}
						return true
					}
					rawMsg, err = GlobalWebmailClient.DownloadAttachmentBytesWithProgress(wmItem.MailID, wmItem.AttachFileID, wmItem.AttachName, httpDlProgress)
					if err != nil {
						e.LogWarn("DOWNLOAD", fmt.Sprintf("⚠️ [单片HTTP通道] 拉取失败 (%v)，自动降级回退至 IMAP 链路", err))
						isHTTP = false
					}
				}
			}
		}

		if !isHTTP {
			singleDlProgress := func(n int) bool {
				if task.Status == "cancelled" || ctx.Err() != nil {
					return false
				}
				for task.Status == "paused" {
					time.Sleep(200 * time.Millisecond)
					if task.Status == "cancelled" || ctx.Err() != nil {
						return false
					}
				}
				deltaRaw := int64(float64(n) * 0.75)
				task.Transferred += deltaRaw
				if task.Transferred > task.TotalSize {
					task.Transferred = task.TotalSize
				}
				if task.TotalSize > 0 {
					task.ProgressPct = float64(task.Transferred) / float64(task.TotalSize) * 100.0
					if task.ProgressPct > 99.0 {
						task.ProgressPct = 99.0
					}
				}
				elapsed := time.Since(singleStart).Seconds()
				if elapsed > 0 {
					task.SpeedMBs = (float64(task.Transferred) / (1024 * 1024)) / elapsed
					task.ElapsedSec = elapsed
				}
				return true
			}
			rawMsg, err = e.imapClient.FetchRawMessageBodyWithProgress(seqs[0], singleDlProgress)
		}

		if task.Status == "cancelled" || ctx.Err() != nil {
			_ = destFile.Close()
			_ = os.Remove(destPath)
			task.Status = "cancelled"
			task.SpeedMBs = 0
			e.LogInfo("DOWNLOAD", fmt.Sprintf("✔ 任务已完全中断并清理碎片: %s", node.Name))
			return nil
		}
		if err != nil {
			_ = destFile.Close()
			_ = os.Remove(destPath)
			task.Status = "failed"
			task.Error = err.Error()
			return err
		}
		var encAttachment []byte
		if bytes.HasPrefix(rawMsg, []byte(MagicBytes)) {
			encAttachment = rawMsg
		} else {
			encAttachment = extractAttachmentFromMIME(rawMsg)
		}
		plain, err := e.DecryptNodeData(encAttachment, &node)
		if err != nil {
			_ = destFile.Close()
			_ = os.Remove(destPath)
			task.Status = "failed"
			task.Error = err.Error()
			return err
		}
		decompressed, decErr := decompressDeflateStream(plain)
		if decErr != nil {
			decompressed = plain
		}
		if _, wErr := destFile.WriteAt(decompressed, 0); wErr != nil {
			_ = destFile.Close()
			_ = os.Remove(partPath)
			task.Status = "failed"
			task.Error = wErr.Error()
			return wErr
		}
		_ = destFile.Close()
		_ = os.Remove(destPath)
		_ = os.Rename(partPath, destPath)
		_ = os.Remove(partPath + ".meta")
		_ = os.WriteFile(destPath+".done", []byte("1"), 0644)
		task.CompletedChunks = []int{1}
		task.ActiveChunks = nil
		task.WritingChunks = nil
		task.Transferred = int64(len(decompressed))
		task.ProgressPct = 100.0
		task.Stage = StageCompleted
		task.Status = "completed"
		task.ElapsedSec = time.Since(singleStart).Seconds()
		e.logTask(task, fmt.Sprintf("✔ 小文件下载解密完成，已完整写入磁盘: %s", destPath))
		return nil
	}

	// 4. 多分片大文件：多线程并发拉取 + 解密落盘流水线解耦 (CPU解密与网络拉取完全并行) + 严格执行防风控延时
	concurrency := GlobalConfig.ChunkDownloadConcurrency
	if concurrency < 1 {
		concurrency = 3
	}
	if concurrency > len(seqs) {
		concurrency = len(seqs)
	}

	// 方案 B：下载流程彻底剥离 request_delay_ms（只读操作零风控风险，全速流水线秒级派发，彻底消除分片间的人为停顿）
	delay := 0

	// 获取已就绪的专属高速常驻连接池（彻底杜绝分片重复握手延迟与断流）
	dlClients := e.GetDownloadClients(concurrency)
	activeClientsMu.Lock()
	activeClients = dlClients
	activeClientsMu.Unlock()
	for _, c := range dlClients {
		if c != nil {
			c.ResetInterrupt()
		}
	}

	// 动态检测与预加载 HTTP Webmail 极速通道 (对齐 HAR 抓包协议，免 Base64 直下)
	var webmailChunkMap map[int]WebmailMailItem
	httpConcurrency := 0
	configLock.RLock()
	cfgHTTPConcurrency := GlobalConfig.HTTPDownloadConcurrency
	configLock.RUnlock()

	if GlobalWebmailClient != nil && GlobalWebmailClient.IsSessionValid() && cfgHTTPConcurrency > 0 {
		hash12 := ""
		if len(node.SHA256) >= 12 {
			hash12 = node.SHA256[:12]
		}
		targetBox := ""
		if e.imapClient != nil {
			targetBox = e.imapClient.folder
		}
		if targetBox == "" {
			configLock.RLock()
			targetBox = GlobalConfig.StorageFolder
			configLock.RUnlock()
		}
		if wmMap, wmErr := GlobalWebmailClient.LocateWebmailChunkMap(targetBox, hash12, node.ID); wmErr == nil && len(wmMap) > 0 {
			webmailChunkMap = wmMap
			httpConcurrency = cfgHTTPConcurrency
			if httpConcurrency > len(wmMap) {
				httpConcurrency = len(wmMap)
			}
			e.LogInfo("DOWNLOAD", fmt.Sprintf("⚡ [HTTP 双通道] 成功对接 Webmail 网页端，已就绪 %d 个 HTTP 极速分片，激活 %d 条 HTTP 免Base64并发链路", len(wmMap), httpConcurrency))
		}
	}

	type ChannelType string
	const (
		ChannelIMAP ChannelType = "IMAP"
		ChannelHTTP ChannelType = "HTTP"
	)

	type clientItem struct {
		id          int
		channelType ChannelType
		cli         *IMAPClient
	}

	totalConcurrency := len(dlClients) + httpConcurrency
	if totalConcurrency <= 0 {
		dlClients = []*IMAPClient{e.imapClient}
		totalConcurrency = 1
	}

	clientPool := make(chan *clientItem, totalConcurrency)
	for idx, c := range dlClients {
		clientPool <- &clientItem{id: idx + 1, channelType: ChannelIMAP, cli: c}
	}
	for h := 1; h <= httpConcurrency; h++ {
		clientPool <- &clientItem{id: len(dlClients) + h, channelType: ChannelHTTP, cli: nil}
	}
	realConcurrency := totalConcurrency

	var (
		netWg            sync.WaitGroup
		cpuWg            sync.WaitGroup
		transferredBytes int64
		completedChunks  int32
		downloadErr      error
		errOnce          sync.Once
		yieldedMu        sync.Mutex
		inFlightMu       sync.Mutex
		lastFetchMu      sync.Mutex
		lastFetchTime    time.Time
	)
	yieldedChunks := make(map[int]bool)
	inFlight := make([]bool, len(seqs))

	// 解耦流水线传输结构体：网络 Worker 只要拉取完原始邮件，立刻交由 CPU Worker 解密，网络连接立即继续拉取下一片！
	type chunkPayload struct {
		partIdx    int
		mailSeq    uint32
		rawMsg     []byte
		linkID     int
		fetchStart time.Time
	}

	payloadChan := make(chan *chunkPayload, realConcurrency*2)
	sem := make(chan struct{}, realConcurrency)
	startTime := time.Now()

	// 启动 CPU 并行解密与落盘 Worker 池 (充分利用多核 CPU，AES-256 + Deflate 解密不占用网络下载时间)
	cpuWorkers := runtime.NumCPU()
	if cpuWorkers > 4 {
		cpuWorkers = 4
	}
	if cpuWorkers < 2 {
		cpuWorkers = 2
	}

	for w := 0; w < cpuWorkers; w++ {
		cpuWg.Add(1)
		go func() {
			defer cpuWg.Done()
			for payload := range payloadChan {
				if downloadErr != nil || task.Status == "cancelled" {
					continue
				}

				realIdx := payload.partIdx
				expectedHash := ""
				var encAttachment []byte
				if bytes.HasPrefix(payload.rawMsg, []byte(MagicBytes)) {
					// HTTP 极速通道直接拉取的原生二进制密文 (零 MIME 报头与零 Base64 开销)
					encAttachment = payload.rawMsg
				} else {
					// 仅针对 IMAP 邮件报文解析报头区域与 Base64 附件
					hdrEnd := bytes.Index(payload.rawMsg, []byte("\r\n\r\n"))
					if hdrEnd == -1 {
						hdrEnd = bytes.Index(payload.rawMsg, []byte("\n\n"))
					}
					if hdrEnd == -1 || hdrEnd > 8192 {
						hdrEnd = 8192
						if hdrEnd > len(payload.rawMsg) {
							hdrEnd = len(payload.rawMsg)
						}
					}
					msgLines := strings.Split(string(payload.rawMsg[:hdrEnd]), "\n")
					for _, l := range msgLines {
						trimmedL := strings.TrimRight(l, "\r")
						if strings.HasPrefix(strings.ToLower(trimmedL), "x-drive-meta:") {
							metaB64 := strings.TrimSpace(trimmedL[13:])
							if b, bErr := base64.StdEncoding.DecodeString(metaB64); bErr == nil {
								var m map[string]interface{}
								if json.Unmarshal(b, &m) == nil {
									if v, ok := m["idx"].(float64); ok {
										realIdx = int(v)
									}
									if h, ok := m["hash"].(string); ok {
										expectedHash = h
									}
								}
							}
							break
						}
					}
					encAttachment = extractAttachmentFromMIME(payload.rawMsg)
				}
				plain, err := e.DecryptNodeData(encAttachment, &node)
				if err != nil {
					errOnce.Do(func() { downloadErr = fmt.Errorf("分片 #%d 解密失败: %w", realIdx, err) })
					continue
				}

				decompressed, decErr := decompressDeflateStream(plain)
				if decErr != nil {
					decompressed = plain
				}

				// 严格分片哈希比对校验 (若不匹配则判定网络包传输脏损，丢弃并自动重新拉取)
				if expectedHash != "" {
					actualHash := fmt.Sprintf("%x", sha256.Sum256(decompressed))
					if !strings.EqualFold(actualHash, expectedHash) {
						e.LogWarn("DOWNLOAD", fmt.Sprintf("⚠️ [分片哈希不符] 分片 #%d 实际哈希 (%s) 与云端标准 (%s) 不一致，已舍弃脏数据并自动重新拉取！", realIdx, actualHash[:12], expectedHash[:12]))
						yieldedMu.Lock()
						yieldedChunks[realIdx-1] = true
						yieldedMu.Unlock()
						inFlightMu.Lock()
						inFlight[realIdx-1] = false
						inFlightMu.Unlock()
						continue
					}
				}

				offset := int64(realIdx-1) * rawChunkSize
				if _, wErr := destFile.WriteAt(decompressed, offset); wErr != nil {
					errOnce.Do(func() { downloadErr = fmt.Errorf("分片 #%d 写入磁盘失败: %w", realIdx, wErr) })
					inFlightMu.Lock()
					inFlight[realIdx-1] = false
					inFlightMu.Unlock()
					continue
				}

				e.tasksLock.Lock()
				alreadyIn := false
				for _, ec := range task.CompletedChunks {
					if ec == realIdx {
						alreadyIn = true
						break
					}
				}
				if !alreadyIn {
					task.CompletedChunks = append(task.CompletedChunks, realIdx)
				}
				if task.PriorityChunk == realIdx {
					task.PriorityChunk = 0
					curCursor := task.PlayCursor
					if curCursor <= 0 { curCursor = 1 }
					e.LogInfo("DOWNLOAD", fmt.Sprintf("✔ [插队就绪] 分片 #%d 已成功落盘，插队指针归零，持续由游标 #%d 顺流向后全速缓冲", realIdx, curCursor))
				}
				var newActive []int
				for _, a := range task.ActiveChunks {
					if a != realIdx {
						newActive = append(newActive, a)
					}
				}
				task.ActiveChunks = newActive

				var newWriting []int
				for _, w := range task.WritingChunks {
					if w != realIdx {
						newWriting = append(newWriting, w)
					}
				}
				task.WritingChunks = newWriting
				curCompletedList := append([]int(nil), task.CompletedChunks...)
				doneCount := len(task.CompletedChunks)
				e.tasksLock.Unlock()

				// 成功落盘，安全释放 inFlight 锁，并原子递增落盘计数
				inFlightMu.Lock()
				inFlight[realIdx-1] = false
				inFlightMu.Unlock()
				atomic.AddInt32(&completedChunks, 1)

				// 实时将已完成分片索引原子写入 .meta 断点记录，供随时断点续接
				if mBytes, mErr := json.Marshal(curCompletedList); mErr == nil {
					_ = os.WriteFile(partPath+".meta", mBytes, 0644)
				}

				dur := time.Since(payload.fetchStart).Seconds()
				task.ElapsedSec = time.Since(startTime).Seconds()
				task.StageDetail = fmt.Sprintf("下载中 速度: %.1f MB/s", task.SpeedMBs)
				e.logTask(task, fmt.Sprintf("✔ [落盘] 分片 #%d 解密并成功写入磁盘 (耗时: %.2fs, 大小: %s)", realIdx, dur, formatBytes(int64(len(decompressed)))))
				if len(seqs) > 0 {
					calcPct := float64(doneCount) / float64(len(seqs)) * 100.0
					if calcPct > 99.0 && doneCount < len(seqs) {
						calcPct = 99.0
					}
					task.ProgressPct = calcPct
				}
			}
		}()
	}

	e.logTask(task, fmt.Sprintf("🚀 启动大文件流水线下载引擎: %d 分片, 网络并发链路: %d, CPU解密并发: %d, 分发延时: %dms", len(seqs), realConcurrency, cpuWorkers, delay))

	// 针对音视频等媒体流，构建智能首尾关键元数据调度队列 (Head + Tail + Penultimate Tail)
	orderedIndices := make([]int, len(seqs))
	for idx := range seqs {
		orderedIndices[idx] = idx
	}
	isMediaFile := node.IsMedia || strings.HasSuffix(strings.ToLower(node.Name), ".mp4") || strings.HasSuffix(strings.ToLower(node.Name), ".mkv") || strings.HasSuffix(strings.ToLower(node.Name), ".mov")
	var essentialMetaIndices []int

	if isMediaFile && len(seqs) >= 2 {
		lastIdx := len(seqs) - 1
		essentialMetaIndices = append(essentialMetaIndices, 0)       // Chunk 1 (Head)
		essentialMetaIndices = append(essentialMetaIndices, lastIdx) // Chunk N (Tail)

		// 智能尾数据尺寸判定：若末分片小于 15MB 黄金平衡阈值，倒数第二分片也协同纳入尾部关键元数据！
		lastChunkEstSize := node.Size - int64(lastIdx)*rawChunkSize
		if len(seqs) >= 3 && lastChunkEstSize < TailMetadataMinChunkSize {
			penultimateIdx := lastIdx - 1
			essentialMetaIndices = append(essentialMetaIndices, penultimateIdx)
			e.LogInfo("DOWNLOAD", fmt.Sprintf("🔍 [智能媒体流] 末分片 #%d 体积较小 (%s < 15MB)，将倒数第二分片 #%d 协同纳入尾部元数据优先调度队列 (先行拉取分片 #1, #%d, #%d)", len(seqs), formatBytes(lastChunkEstSize), len(seqs)-1, len(seqs), len(seqs)-1))
		} else {
			e.LogInfo("DOWNLOAD", fmt.Sprintf("🔍 [智能媒体流] 启用首尾分片优先就绪调度 (分片 #1 与分片 #%d 先行拉取)", len(seqs)))
		}
	}

	// 自动检查并加载已有断点落盘记录 (.meta)
	metaPath := partPath + ".meta"
	if mData, mErr := os.ReadFile(metaPath); mErr == nil {
		var savedChunks []int
		if json.Unmarshal(mData, &savedChunks) == nil {
			e.tasksLock.Lock()
			for _, sc := range savedChunks {
				if sc >= 1 && sc <= len(seqs) {
					alreadyDone := false
					for _, ec := range task.CompletedChunks {
						if ec == sc {
							alreadyDone = true
							break
						}
					}
					if !alreadyDone {
						task.CompletedChunks = append(task.CompletedChunks, sc)
					}
				}
			}
			e.tasksLock.Unlock()
		}
	}

	dispatched := make([]bool, len(seqs))
	alreadyDoneCount := 0
	e.tasksLock.RLock()
	for _, c := range task.CompletedChunks {
		if c >= 1 && c <= len(seqs) && !dispatched[c-1] {
			dispatched[c-1] = true
			alreadyDoneCount++
		}
	}
	e.tasksLock.RUnlock()
	if alreadyDoneCount > 0 {
		completedChunks = int32(alreadyDoneCount)
		initialBytes := int64(alreadyDoneCount) * rawChunkSize
		if initialBytes > task.TotalSize {
			initialBytes = task.TotalSize
		}
		transferredBytes = initialBytes
		task.Transferred = initialBytes
		if task.TotalSize > 0 {
			task.ProgressPct = float64(initialBytes) / float64(task.TotalSize) * 100.0
		}
		e.LogInfo("DOWNLOAD", fmt.Sprintf("⚡ [断点续传] %s 成功复用已有 %d 个已落盘分片，直接跳过已完成数据！", node.Name, alreadyDoneCount))
	}

// inFlight & inFlightMu declared in main var block

	allChunksComplete := func() bool {
		e.tasksLock.RLock()
		defer e.tasksLock.RUnlock()
		if len(task.CompletedChunks) < len(seqs) {
			return false
		}
		doneMap := make(map[int]bool)
		for _, c := range task.CompletedChunks {
			doneMap[c] = true
		}
		for i := 1; i <= len(seqs); i++ {
			if !doneMap[i] {
				return false
			}
		}
		return true
	}

	for !allChunksComplete() {
		if task.Status == "cancelled" || ctx.Err() != nil || downloadErr != nil {
			break
		}
		// 暂停与续传控制
		if task.Status == "paused" {
			e.logTask(task, "⏸ 下载任务已暂停...")
			for task.Status == "paused" {
				time.Sleep(300 * time.Millisecond)
			}
			if task.Status == "cancelled" || ctx.Err() != nil {
				_ = destFile.Close()
				e.logTask(task, "❌ 任务已被用户取消")
				return nil
			}
			e.logTask(task, "▶️ 任务恢复执行，继续并发调度...")
		}

		selectedIdx := -1

		// 检查是否有用户在播放器控制台拖拽插队的分片 (PriorityChunk) 或向前播放游标 (PlayCursor)
		e.tasksLock.RLock()
		pChunk := task.PriorityChunk
		cursor := task.PlayCursor
		if cursor <= 0 {
			cursor = 1
		}
		completedSnapshot := append([]int(nil), task.CompletedChunks...)
		activeSnapshot := append([]int(nil), task.ActiveChunks...)
		e.tasksLock.RUnlock()

		inFlightMu.Lock()
		isChunkAvailable := func(cIdx int) bool {
			targetPart := cIdx + 1
			for _, doneC := range completedSnapshot {
				if doneC == targetPart {
					return false
				}
			}
			if inFlight[cIdx] {
				return false
			}
			for _, actC := range activeSnapshot {
				if actC == targetPart {
					return false
				}
			}
			return true
		}

		// 优先级 1：紧急即时插队分片 (当前起播卡点分片，最高置顶无条件抢占)
		if pChunk > 0 && pChunk <= len(seqs) && isChunkAvailable(pChunk-1) {
			selectedIdx = pChunk - 1
			e.LogInfo("DOWNLOAD", fmt.Sprintf("⚡ [极速插队] 立即派发起播关键分片 #%d", pChunk))
		}

		// 起播关键切片最高置顶抢占 (Priority 1) 派发后，副链路全速顺流并行跟进下一分片 (targetChunk + 1)
		// 确保起播分片落盘时，紧随其后的预缓冲分片也已处于高位就绪状态，严防单片空洞导致播放器读穿跳尾！

		// 优先级 2：首尾关键元数据最高优先拉取 (Chunk 1、Chunk N 以及小尾片关联的 Chunk N-1)
		// 严防无尾切片导致播放器无法解析 Cues/moov 从而无法起播！
		if selectedIdx == -1 && len(essentialMetaIndices) > 0 && (pChunk <= 1 || pChunk == 0) {
			for _, mIdx := range essentialMetaIndices {
				if isChunkAvailable(mIdx) {
					selectedIdx = mIdx
					e.LogInfo("DOWNLOAD", fmt.Sprintf("🎬 [关键元数据就绪] 优先拉取媒体元数据分片 #%d (保障播放器毫秒级解析头尾 Cues/moov)", mIdx+1))
					break
				}
			}
		}

		// 优先级 3：从播放游标 (cursor) 向后连续拉取至文件末尾 (cursor, cursor+1, cursor+2 ... len(seqs))
		// 确保用户拖拽后，后续观影数据流始终保持源源不断的正向预缓冲，绝不中途倒退回历史分片！
		if selectedIdx == -1 && cursor > 0 && cursor <= len(seqs) {
			for c := cursor - 1; c < len(seqs); c++ {
				if isChunkAvailable(c) {
					selectedIdx = c
					break
				}
			}
		}

		// 优先级 4：仅当从游标到文件结尾全部已落盘或已在拉取中时，才回过头去补全游标前方被跳过的历史分片 (1 到 cursor-1)
		if selectedIdx == -1 && cursor > 1 {
			// 先核查当前游标之后是否真的全部完成或在拉取中
			hasPendingAfterCursor := false
			for c := cursor - 1; c < len(seqs); c++ {
				if isChunkAvailable(c) {
					hasPendingAfterCursor = true
					break
				}
			}

			// 只有在游标后方绝无任何待处理分片时，才放行历史分片补全！严防第三次跳转时倒退拉取导致视频画面错乱
			if !hasPendingAfterCursor {
				yieldedMu.Lock()
				for cIdx := range yieldedChunks {
					if cIdx < cursor-1 && isChunkAvailable(cIdx) {
						selectedIdx = cIdx
					}
					delete(yieldedChunks, cIdx)
					if selectedIdx != -1 {
						break
					}
				}
				yieldedMu.Unlock()

				if selectedIdx == -1 {
					for c := 0; c < cursor-1; c++ {
						if isChunkAvailable(c) {
							selectedIdx = c
							break
						}
					}
				}
			}
		}

		// 优先级 5：常规原序兜底 (仅限未指定游标或所有游标段均排满时)
		if selectedIdx == -1 && cursor <= 1 {
			for _, ordIdx := range orderedIndices {
				if isChunkAvailable(ordIdx) {
					selectedIdx = ordIdx
					break
				}
			}
		}

		if selectedIdx != -1 {
			inFlight[selectedIdx] = true
		}
		inFlightMu.Unlock()

		if selectedIdx == -1 {
			time.Sleep(60 * time.Millisecond)
			continue
		}

		dispatched[selectedIdx] = true

		sem <- struct{}{}
		netWg.Add(1)

		// 核心保证：主调度器按绝对优先级【同步从连接池出队】并绑定链路！
		// 严防子协程异步唤醒竞争导致关键切片与链路顺序被颠倒
		cItem := <-clientPool

		partNumber := selectedIdx + 1
		seq := seqs[selectedIdx]
		go func(pIdx int, mailSeq uint32, conn *clientItem) {
			isQueuedToCpu := false
			defer func() {
				if !isQueuedToCpu {
					inFlightMu.Lock()
					inFlight[pIdx-1] = false
					inFlightMu.Unlock()
				}
				<-sem
				netWg.Done()
			}()

// 主调度器已在派发层完成绝对优先级锁定，并发辅助链路全速拉取无需自杀让位

			for task.Status == "paused" {
				time.Sleep(200 * time.Millisecond)
			}

			if downloadErr != nil || task.Status == "cancelled" || ctx.Err() != nil {
				clientPool <- conn
				return
			}

			workerClient := conn.cli
			linkID := conn.id
			if workerClient != nil {
				if workerClient.Logger == nil {
					workerClient.Logger = e.LogWithLevel
				}
				workerClient.ResetInterrupt()
			}
			defer func() {
				clientPool <- conn
			}()

			var isInterrupted int32
			interruptFunc := func() {
				atomic.StoreInt32(&isInterrupted, 1)
				if workerClient != nil {
					workerClient.Interrupt()
				}
			}

			// 寻道紧急拦截：核验在排队等待连接期间，用户是否执行了寻道跳转
			e.tasksLock.RLock()
			curCursor := task.PlayCursor
			curP := task.PriorityChunk
			e.tasksLock.RUnlock()

			refTarget := curP
			if refTarget <= 0 {
				refTarget = curCursor
			}

			// 如果当前分片编号小于寻道目标分片，且寻道目标及后续分片中仍有尚未拉取的切片在等待链路：
			// 说明当前分片属于被跳过的历史分片且正在抢占通道，必须立即让位归还连接！
			// 但如果寻道目标及后续分片全部已落盘或已全部在并发拉取中，此时说明当前链路处于空闲状态，
			// 允许且应当拉取历史分片以补全全片，绝不误杀导致死循环刷屏！
			if refTarget > 1 && pIdx < refTarget {
				hasWaitingAfter := false
				inFlightMu.Lock()
				for c := refTarget - 1; c < len(seqs); c++ {
					if isChunkAvailable(c) {
						hasWaitingAfter = true
						break
					}
				}
				inFlightMu.Unlock()

				if hasWaitingAfter {
					e.LogInfo("STREAM", fmt.Sprintf("⚡ [即时熔断] 分片 #%d 位于寻道目标 #%d 之前，立即让位并释放【链路 #%d】", pIdx, refTarget, linkID))
					yieldedMu.Lock()
					yieldedChunks[pIdx-1] = true
					yieldedMu.Unlock()
					return
				}
			}

			inFlightWorkersMu.Lock()
			inFlightWorkers[linkID] = activeWorkerInfo{
				linkID:   linkID,
				chunkIdx: pIdx,
				client:   workerClient,
			}
			inFlightWorkersMu.Unlock()
			defer func() {
				inFlightWorkersMu.Lock()
				delete(inFlightWorkers, linkID)
				inFlightWorkersMu.Unlock()
			}()

			e.tasksLock.Lock()
			task.ActiveChunks = append(task.ActiveChunks, pIdx)
			e.tasksLock.Unlock()
			defer func() {
				e.tasksLock.Lock()
				var rem []int
				for _, a := range task.ActiveChunks {
					if a != pIdx {
						rem = append(rem, a)
					}
				}
				task.ActiveChunks = rem
				e.tasksLock.Unlock()
			}()

			// 流水线抗同频错峰调度 (Staggered Pipeline Jitter)：
			// 彻底解决 5 条链路在同一毫秒扎堆冲刷云端存储引发的 7 秒 I/O 锁死排队与断流波谷；
			// 起播关键分片（首分片或拖拽目标）享有 0ms 零延迟独占瞬发特权；
			// 其余辅助链路在发包前保持至少 350ms 的自然相位差，让各链路的“网络接收”与“等待 TTFB”阶段完美交错开来，
			// 将任务管理器中起伏剧烈的波浪形断流曲线彻底平滑为平稳连续的高速水流！
			e.tasksLock.RLock()
			curPriority := task.PriorityChunk
			e.tasksLock.RUnlock()

			if linkID > 1 && (curPriority <= 0 || pIdx != curPriority) {
				lastFetchMu.Lock()
				now := time.Now()
				var waitDur time.Duration
				if now.Before(lastFetchTime) {
					waitDur = lastFetchTime.Sub(now)
				} else {
					timeSinceLast := now.Sub(lastFetchTime)
					if timeSinceLast < 350*time.Millisecond {
						waitDur = 350*time.Millisecond - timeSinceLast
					}
				}
				lastFetchTime = now.Add(waitDur).Add(350 * time.Millisecond)
				lastFetchMu.Unlock()

				if waitDur > 0 {
					time.Sleep(waitDur)
				}
			} else {
				lastFetchMu.Lock()
				lastFetchTime = time.Now().Add(350 * time.Millisecond)
				lastFetchMu.Unlock()
			}

			fetchStart := time.Now()
			chunkDlProgress := func(n int) bool {
				if atomic.LoadInt32(&isInterrupted) == 1 || task.Status == "cancelled" || ctx.Err() != nil {
					return false
				}
				if workerClient != nil && workerClient.IsInterrupted() {
					return false
				}
				// 检查用户是否拖拽至更靠后的分片：若是且目标分片尚未完成，立即主动熔断旧分片，全速为拖拽目标分片腾出网络连接！
				e.tasksLock.RLock()
				curP := task.PriorityChunk
				isTargetDone := false
				for _, dc := range task.CompletedChunks {
					if dc == curP {
						isTargetDone = true
						break
					}
				}
				e.tasksLock.RUnlock()
				if curP > 0 && !isTargetDone && pIdx < curP {
					e.LogInfo("DOWNLOAD", fmt.Sprintf("⚡ [跳段熔断] 用户向后拖拽至分片 #%d，正在拉取的分片 #%d 立即主动让道退出", curP, pIdx))
					interruptFunc()
					return false
				}
				cursor := task.PlayCursor
				if cursor > 1 && pIdx < cursor {
					// 检查游标后方是否仍有未完成分片
					hasForwardPending := false
					for c := cursor; c <= len(seqs); c++ {
						cDone := false
						for _, dc := range task.CompletedChunks {
							if dc == c { cDone = true; break }
						}
						if !cDone { hasForwardPending = true; break }
					}
					if hasForwardPending {
						e.LogInfo("DOWNLOAD", fmt.Sprintf("⚡ [历史避让] 分片 #%d 位于播放游标 #%d 之前，主动让出链路优先保障后续观影缓冲", pIdx, cursor))
						interruptFunc()
						return false
					}
				}
				for task.Status == "paused" {
					time.Sleep(200 * time.Millisecond)
					if atomic.LoadInt32(&isInterrupted) == 1 || task.Status == "cancelled" || ctx.Err() != nil {
						return false
					}
				}
				deltaRaw := int64(n)
				if conn.channelType == ChannelIMAP {
					deltaRaw = int64(float64(n) * 0.75) // IMAP Base64 膨胀还原
				}
				cur := atomic.AddInt64(&transferredBytes, deltaRaw)
				if cur > task.TotalSize {
					cur = task.TotalSize
				}
				task.Transferred = cur
				if task.TotalSize > 0 {
					task.ProgressPct = float64(cur) / float64(task.TotalSize) * 100.0
					if task.ProgressPct > 99.0 && atomic.LoadInt32(&completedChunks) < int32(len(seqs)) {
						task.ProgressPct = 99.0
					}
				}
				elapsed := time.Since(startTime).Seconds()
				if elapsed > 0 {
					task.SpeedMBs = (float64(cur) / (1024 * 1024)) / elapsed
					task.ElapsedSec = elapsed
				}
				if task.SpeedMBs > 0 && task.TotalSize > cur {
					task.RemainSec = float64(task.TotalSize-cur) / (task.SpeedMBs * 1024 * 1024)
				}
				return true
			}

			var rawMsg []byte
			var err error

			if conn.channelType == ChannelHTTP {
				wmItem, hasWM := webmailChunkMap[pIdx]
				if !hasWM {
					// 该切片在 Webmail 未检索到，自动回退由 IMAP 链路拉取
					yieldedMu.Lock()
					yieldedChunks[pIdx-1] = true
					yieldedMu.Unlock()
					return
				}
				e.logTask(task, fmt.Sprintf("[HTTP免Base64直下] 分片 #%d 正在通过【HTTP通道 #%d】高速拉取...", pIdx, linkID))
				rawMsg, err = GlobalWebmailClient.DownloadAttachmentBytesWithProgress(wmItem.MailID, wmItem.AttachFileID, wmItem.AttachName, chunkDlProgress)
				if err != nil {
					e.LogWarn("DOWNLOAD", fmt.Sprintf("⚠️ [HTTP通道 #%d] 分片 #%d HTTP拉取受限 (%v)，自动放回调度池由 IMAP 链路重试", linkID, pIdx, err))
					yieldedMu.Lock()
					yieldedChunks[pIdx-1] = true
					yieldedMu.Unlock()
					return
				}
			} else {
				e.logTask(task, fmt.Sprintf("[并发拉取] 分片 #%d 正在通过【IMAP链路 #%d】高速拉取...", pIdx, linkID))
				rawMsg, err = workerClient.FetchRawMessageBodyWithProgress(mailSeq, chunkDlProgress)
			}

			dur := time.Since(fetchStart).Seconds()
			if err != nil {
				if task.Status == "cancelled" || ctx.Err() != nil {
					e.LogInfo("DOWNLOAD", fmt.Sprintf("✔ 链路 #%d 正在拉取的分片 #%d 已随用户取消安全释放", linkID, pIdx))
					return
				}
				e.tasksLock.RLock()
				curP := task.PriorityChunk
				isTargetDone := false
				for _, dc := range task.CompletedChunks {
					if dc == curP {
						isTargetDone = true
						break
					}
				}
				e.tasksLock.RUnlock()
				curCursor := task.PlayCursor
				// 若属于调度换道让位，绝非系统错误！将该分片无损收录至待调度队列供后续自动补全
				if atomic.LoadInt32(&isInterrupted) == 1 || errors.Is(err, ErrInterrupted) || (curP > 0 && !isTargetDone && pIdx < curP) || (curCursor > 1 && pIdx < curCursor) || strings.Contains(err.Error(), "让位") || strings.Contains(err.Error(), "中断") || strings.Contains(err.Error(), "取消") {
					targetA := curP
					if targetA <= 0 { targetA = curCursor }
					e.LogInfo("STREAM", fmt.Sprintf("✔ [跳段让位] 分片 #%d 已主动让位换道，释放【链路 #%d】供插队分片 #%d 极速拉取", pIdx, linkID, targetA))
					yieldedMu.Lock()
					yieldedChunks[pIdx-1] = true
					yieldedMu.Unlock()
					return
				}
				e.LogError("DOWNLOAD", fmt.Sprintf("❌ [下载异常] 分片 #%d 在【链路 #%d】传输异常: %v", pIdx, linkID, err))
				errOnce.Do(func() { downloadErr = fmt.Errorf("分片 #%d 传输异常: %w", pIdx, err) })
				return
			}

			if workerClient != nil {
				ttfb := workerClient.LastTTFB.Seconds()
				transferSec := workerClient.LastTransferDur.Seconds()
				mbps := workerClient.LastThroughputMbps
				e.logTask(task, fmt.Sprintf("🔍 [性能探针] 分片 #%d【IMAP链路 #%d】: 首字节等待(TTFB/腾讯寻道): %.2fs | 纯网络接收: %.2fs (瞬时带宽: %.1f Mbps)", pIdx, linkID, ttfb, transferSec, mbps))
			} else {
				mbps := 0.0
				if dur > 0 {
					mbps = (float64(len(rawMsg)) * 8 / (1024 * 1024)) / dur
				}
				e.logTask(task, fmt.Sprintf("⚡ [HTTP探针] 分片 #%d【HTTP通道 #%d】: 原生二进制免Base64直下耗时: %.2fs (平均带宽: %.1f Mbps)", pIdx, linkID, dur, mbps))
			}
			e.logTask(task, fmt.Sprintf("✔ [拉取完成] 分片 #%d 已由【链路 #%d】拉取完毕 (总耗时: %.2fs, 大小: %s)", pIdx, linkID, dur, formatBytes(int64(len(rawMsg)))))

			// 立即将拉取到的原始数据投入流水线队列，连接立即释放回池用于下一个分片
			e.tasksLock.Lock()
			task.WritingChunks = append(task.WritingChunks, pIdx)
			e.tasksLock.Unlock()

			payloadChan <- &chunkPayload{
				partIdx:    pIdx,
				mailSeq:    mailSeq,
				rawMsg:     rawMsg,
				linkID:     linkID,
				fetchStart: fetchStart,
			}
			isQueuedToCpu = true
		}(partNumber, seq, cItem)

		// 主调度器在派发下一个分片前，平滑休眠设定的防风控延迟
		if delay > 0 && int(atomic.LoadInt32(&completedChunks)) < len(seqs) {
			time.Sleep(time.Duration(delay) * time.Millisecond)
		}
	}

	netWg.Wait()
	close(payloadChan)
	cpuWg.Wait()

	defer func() {
		if e.imapClient != nil {
			e.imapClient.ResetInterrupt()
		}
	}()

	if task.Status == "cancelled" || ctx.Err() != nil {
		_ = destFile.Close()
		if task.Type == "download" {
			_ = os.Remove(partPath)
			e.LogInfo("DOWNLOAD", fmt.Sprintf("✔ 任务已完全中断并已清理本地碎片: %s", node.Name))
		} else {
			e.LogInfo("DOWNLOAD", fmt.Sprintf("✔ 任务已完全中断，已安全保留落盘分片: %s", node.Name))
		}
		task.Status = "cancelled"
		task.SpeedMBs = 0
		task.ActiveChunks = nil
		task.WritingChunks = nil
		task.StageDetail = "已由用户手动取消并回收资源"
		return nil
	}

	if downloadErr != nil {
		_ = destFile.Close()
		_ = os.Remove(partPath)
		task.Status = "failed"
		task.Error = downloadErr.Error()
		e.logTask(task, "❌ 多线程下载失败: "+downloadErr.Error())
		return downloadErr
	}

	// 5. 进入落盘与哈希核验阶段，绝不提前虚假宣告完成！
	e.tasksLock.Lock()
	task.Stage = StageVerify
	task.StageDetail = "正在执行分片位图核验与磁盘物理刷盘..."
	e.tasksLock.Unlock()
	e.LogInfo("DOWNLOAD", fmt.Sprintf("💿 [落盘组装] %s 全部 %d 个切片已拉取完毕，正在执行物理刷盘与最终完整性核验...", node.Name, len(seqs)))

	_ = destFile.Sync()
	_ = destFile.Close()

	// 严格核验：检查每一个分片 1..len(seqs) 是否均在 CompletedChunks 中，严禁任何空洞！
	e.tasksLock.RLock()
	doneSet := make(map[int]bool)
	for _, c := range task.CompletedChunks {
		doneSet[c] = true
	}
	e.tasksLock.RUnlock()

	missingChunks := 0
	for i := 1; i <= len(seqs); i++ {
		if !doneSet[i] {
			missingChunks++
		}
	}
	if missingChunks > 0 {
		e.LogWarn("DOWNLOAD", fmt.Sprintf("❌ [核验失败] 发现 %d 个分片缺失或损坏，保留断点切片，中止组装: %s", missingChunks, node.Name))
		task.Status = "failed"
		task.Error = fmt.Sprintf("落盘完整性核验失败: 缺失 %d 个分片", missingChunks)
		return fmt.Errorf("落盘完整性核验失败")
	}

	// 先行在 .part 上打上 .done 标记，确保即使重命名遇到 Windows 文件锁，也是绝对的完整文件状态
	_ = os.WriteFile(partPath+".done", []byte("1"), 0644)

	// 平滑重命名重试机制 (针对 Windows 预览播放器句柄占用的平滑重试)
	var rErr error
	for attempt := 0; attempt < 10; attempt++ {
		_ = os.Remove(destPath)
		rErr = os.Rename(partPath, destPath)
		if rErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	if rErr != nil {
		e.LogInfo("DOWNLOAD", fmt.Sprintf("✔ [边下边播就绪] 媒体播放管道正在实时占用媒体流，已通过双轨安全机制锁定完整文件，待浮窗关闭后自动平滑归档: %s", node.Name))
		_ = os.WriteFile(destPath+".done", []byte("1"), 0644)
	} else {
		_ = os.Remove(partPath + ".done")
		_ = os.Remove(partPath + ".meta")
		_ = os.WriteFile(destPath+".done", []byte("1"), 0644)
	}

	e.tasksLock.Lock()
	task.Stage = StageCompleted
	task.StageDetail = "✔ 落盘校验完毕，文件已就绪"
	task.Status = "completed"
	task.Transferred = task.TotalSize
	task.ProgressPct = 100.0
	task.SpeedMBs = 0
	task.ElapsedSec = time.Since(startTime).Seconds()
	e.tasksLock.Unlock()
	e.logTask(task, fmt.Sprintf("✔ 大文件多线程并发下载解密完成，已完整组装至本地: %s", destPath))
	return nil
}

// DownloadBundledFile 从云端小文件合包中定位并解压提取目标文件
func (e *StorageEngine) DownloadBundledFile(node FileNode, destPath string) error {
	if node.BundleSeq == "" {
		return fmt.Errorf("合包标识为空，无法定位合包邮件")
	}

	headers, err := e.imapClient.FetchAllHeadersFast()
	if err != nil {
		return fmt.Errorf("获取云端邮件报头失败: %w", err)
	}

	var bundleSeqNum uint32
	bundleTag := fmt.Sprintf("[QMD:BUNDLE]|%s|", node.BundleSeq)
	for _, h := range headers {
		if strings.HasPrefix(h.Subject, "[QMD:BUNDLE]|") && (strings.Contains(h.Subject, bundleTag) || strings.Contains(h.Subject, "|"+node.BundleSeq+"|")) {
			bundleSeqNum = h.SeqNum
			break
		}
	}

	if bundleSeqNum == 0 {
		return fmt.Errorf("未能从云端定位合包邮件 (BundleSeq: %s)", node.BundleSeq)
	}

	rawMsg, err := e.imapClient.FetchRawMessageBody(bundleSeqNum)
	if err != nil {
		return fmt.Errorf("拉取合包邮件失败 (序号 %d): %w", bundleSeqNum, err)
	}

	encAttachment := extractAttachmentFromMIME(rawMsg)
	if len(encAttachment) == 0 {
		return fmt.Errorf("合包邮件中未找到有效附件数据")
	}

	plainZip, err := e.DecryptNodeData(encAttachment, &node)
	if err != nil {
		return fmt.Errorf("合包数据解密失败: %w", err)
	}

	zr, err := zip.NewReader(bytes.NewReader(plainZip), int64(len(plainZip)))
	if err != nil {
		return fmt.Errorf("合包 ZIP 解析失败: %w", err)
	}

	targetName := node.InnerPath
	if targetName == "" {
		targetName = node.Name
	}

	var targetZipFile *zip.File
	for _, f := range zr.File {
		if f.Name == targetName || f.Name == node.Name {
			targetZipFile = f
			break
		}
	}

	if targetZipFile == nil {
		return fmt.Errorf("合包中未找到目标文件: %s", targetName)
	}

	rc, err := targetZipFile.Open()
	if err != nil {
		return fmt.Errorf("打开合包内文件失败: %w", err)
	}
	defer rc.Close()

	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}

	outF, err := os.OpenFile(destPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0666)
	if err != nil {
		return fmt.Errorf("创建本地输出文件失败: %w", err)
	}
	defer outF.Close()

	if _, err := io.Copy(outF, rc); err != nil {
		return fmt.Errorf("写入文件失败: %w", err)
	}

	_ = os.WriteFile(destPath+".done", []byte("1"), 0644)
	return nil
}

func (e *StorageEngine) DownloadFileStream(node FileNode, destPath string, progressCb func(float64)) error {
	if node.StorageType == StorageTypeBundled {
		err := e.DownloadBundledFile(node, destPath)
		if err == nil && progressCb != nil {
			progressCb(100.0)
		}
		return err
	}

	destFile, err := os.Create(destPath)
	if err != nil {
		e.Log(fmt.Sprintf("❌ 创建本地落地文件失败: %v", err))
		return err
	}
	defer destFile.Close()

	e.Log(fmt.Sprintf("[下载] 开始定位并解密文件: %s (ID: %s, 大小: %s)", node.Name, node.ID, formatBytes(node.Size)))

	// 全盘报头精确逐字比对定位（严格排重与序号对齐）
	var seqs []uint32
	headers, hErr := e.imapClient.FetchAllHeadersFast()
	if hErr == nil {
		seqs = e.LocateChunkSeqsForNode(&node, headers)
	}
	if len(seqs) == 0 {
		_ = destFile.Close()
		_ = os.Remove(destPath)
		e.Log(fmt.Sprintf("❌ 下载失败：未能在云端找到文件 %s 的分片邮件 (ID: %s)", node.Name, node.ID))
		return fmt.Errorf("未能从云端定位文件 %s 的切片邮件 (ID: %s)", node.Name, node.ID)
	}

	type PartData struct {
		Idx  int
		Data []byte
	}
	var parts []PartData

	for _, seq := range seqs {
		rawMsg, err := e.imapClient.FetchRawMessageBody(seq)
		if err != nil {
			_ = destFile.Close()
			_ = os.Remove(destPath)
			e.Log(fmt.Sprintf("❌ 拉取分片邮件失败 (序号 %d): %v", seq, err))
			return err
		}

		idx := 1
		lines := strings.Split(string(rawMsg), "\n")
		for _, l := range lines {
			trimmedL := strings.TrimRight(l, "\r")
			if strings.HasPrefix(strings.ToLower(trimmedL), "x-drive-meta:") {
				metaB64 := strings.TrimSpace(trimmedL[13:])
				if b, err := base64.StdEncoding.DecodeString(metaB64); err == nil {
					var m map[string]interface{}
					if json.Unmarshal(b, &m) == nil {
						if i, ok := m["idx"].(float64); ok {
							idx = int(i)
						}
					}
				}
				break
			}
		}

		encAttachment := extractAttachmentFromMIME(rawMsg)
		plain, err := e.DecryptNodeData(encAttachment, &node)
		if err != nil {
			_ = destFile.Close()
			_ = os.Remove(destPath)
			e.Log(fmt.Sprintf("❌ 分片解密校验失败: %v", err))
			return fmt.Errorf("分片解密失败: %w", err)
		}

		// 内存自适应解压还原
		decompressed, decErr := decompressDeflateStream(plain)
		if decErr != nil {
			decompressed = plain
		}
		parts = append(parts, PartData{Idx: idx, Data: decompressed})
	}

	// 按切片索引升序组装
	sort.Slice(parts, func(i, j int) bool {
		return parts[i].Idx < parts[j].Idx
	})

	for i, p := range parts {
		if _, err := destFile.Write(p.Data); err != nil {
			_ = destFile.Close()
			_ = os.Remove(destPath)
			return err
		}
		if progressCb != nil {
			progressCb(float64(i+1) / float64(len(parts)) * 100.0)
		}
	}

	e.Log(fmt.Sprintf("✔ 文件流式解密完成并落地: %s", node.Name))
	return nil
}

// DownloadFolderAsZip 文件夹打包 ZIP 下载支持
func (e *StorageEngine) DownloadFolderAsZip(folderNode FileNode, destZipPath string) error {
	zipFile, err := os.Create(destZipPath)
	if err != nil {
		return err
	}
	defer zipFile.Close()

	zipWriter := zip.NewWriter(zipFile)
	defer zipWriter.Close()

	e.indexLock.RLock()
	prefix := folderNode.VirtualPath + "/"
	var subFiles []FileNode
	for p, f := range e.masterIndex.Files {
		if strings.HasPrefix(p, prefix) && !f.IsDir {
			subFiles = append(subFiles, f)
		}
	}
	e.indexLock.RUnlock()

	for _, sf := range subFiles {
		relPath := sf.VirtualPath[len(prefix):]
		tempSingle := filepath.Join("temp", "sub_"+sf.ID)
		if err := e.DownloadFileStream(sf, tempSingle, nil); err == nil {
			if srcF, oErr := os.Open(tempSingle); oErr == nil {
				if w, err := zipWriter.Create(relPath); err == nil {
					_, _ = io.Copy(w, srcF)
				}
				_ = srcF.Close()
			}
			_ = os.Remove(tempSingle)
		}
	}

	return nil
}

// LoadThumbnailsFromCloud (兼容旧版格式)：从云端加载历史遗留的独立缩略图分卷邮件 [QMD:THUMB]
func (e *StorageEngine) LoadThumbnailsFromCloud() {
	headers, err := e.imapClient.FetchAllHeadersFast()
	if err != nil {
		return
	}

	e.thumbLock.Lock()
	defer e.thumbLock.Unlock()

	count := 0
	for _, h := range headers {
		if strings.HasPrefix(h.Subject, "[QMD:THUMB]|") {
			rawMsg, err := e.imapClient.FetchRawMessageBody(h.SeqNum)
			if err != nil {
				continue
			}
			encData := extractAttachmentFromMIME(rawMsg)
			if plainJSON, err := e.DecryptData(encData); err == nil {
				var volMap map[string]string
				if json.Unmarshal(plainJSON, &volMap) == nil {
					for fid, tb := range volMap {
						e.thumbCache[fid] = tb
						count++
					}
				}
			}
		}
	}
	if count > 0 {
		e.Log(fmt.Sprintf("✔ 云端缩略图分卷加载完成，已载入 %d 张缩略图缓存", count))
	}
}

// SaveThumbnailToVolume (兼容旧版格式)：将缩略图存入独立缩略图分卷并同步至云端专属邮件
func (e *StorageEngine) SaveThumbnailToVolume(fileID, thumbBase64 string) error {
	if thumbBase64 == "" {
		return nil
	}

	e.thumbLock.Lock()
	e.thumbCache[fileID] = thumbBase64
	currentVol := e.masterIndex.ActiveThumb
	if currentVol == "" {
		currentVol = "01"
		e.masterIndex.ActiveThumb = currentVol
	}

	volMap := make(map[string]string)
	for fid, tb := range e.thumbCache {
		volMap[fid] = tb
	}
	e.thumbLock.Unlock()

	data, err := json.Marshal(volMap)
	if err != nil {
		return err
	}

	maxBytes := GlobalConfig.ChunkSizeBytes
	if maxBytes <= 0 {
		maxBytes = 52428799
	}

	if int64(len(data)) >= maxBytes {
		curVolNum, _ := strconv.Atoi(currentVol)
		newVol := fmt.Sprintf("%02d", curVolNum+1)
		e.indexLock.Lock()
		e.masterIndex.ActiveThumb = newVol
		e.indexLock.Unlock()
		e.Log(fmt.Sprintf("[缩略图] 分卷 vol_%s 达到容量上限 (%s)，自动开辟新分卷 vol_%s", currentVol, formatBytes(int64(len(data))), newVol))
		currentVol = newVol
		volMap = map[string]string{fileID: thumbBase64}
		data, _ = json.Marshal(volMap)
	}

	encData, err := e.EncryptData(data)
	if err != nil {
		return err
	}

	subject := fmt.Sprintf("[QMD:THUMB]|vol_%s|%d", currentVol, time.Now().Unix())
	meta := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"vol":"%s","count":%d}`, currentVol, len(volMap))))
	mimeData := e.BuildMIMEMessage(subject, meta, fmt.Sprintf("thumb_vol_%s.dat", currentVol), encData)

	if err := e.imapClient.AppendDirectMessage(GlobalConfig.StorageFolder, subject, meta, mimeData); err != nil {
		e.Log(fmt.Sprintf("❌ 写入云端缩略图分卷邮件失败: %v", err))
		return err
	}

	time.Sleep(200 * time.Millisecond)
	headers, _ := e.imapClient.FetchAllHeadersFast()
	var oldVolSeqs []uint32
	prefix := fmt.Sprintf("[QMD:THUMB]|vol_%s|", currentVol)
	for _, h := range headers {
		if strings.HasPrefix(h.Subject, prefix) {
			oldVolSeqs = append(oldVolSeqs, h.SeqNum)
		}
	}
	if len(oldVolSeqs) > 1 {
		toDelete := oldVolSeqs[:len(oldVolSeqs)-1]
		_ = e.imapClient.DeleteAndExpunge(toDelete)
	}

	e.Log(fmt.Sprintf("✔ 缩略图已同步至云端专属分卷邮件 [QMD:THUMB]|vol_%s", currentVol))
	return nil
}

// InitOrLoadCloudIndex 纯云端拉取主索引：精准按最高版本拉取，绝不盲目归零，保留全部空文件夹与缩略图
func (e *StorageEngine) InitOrLoadCloudIndex() (bool, error) {
	if !e.imapClient.connected {
		return false, fmt.Errorf("IMAP 未连接")
	}
	configLock.RLock()
	curBox := GlobalConfig.StorageFolder
	configLock.RUnlock()
	if curBox != "" {
		_ = e.imapClient.EnsureAndSelectFolder(curBox)
	}

	headers, hErr := e.imapClient.FetchAllHeadersFast()
	if hErr == nil {
		type IndexMail struct {
			SeqNum  uint32
			Version int64
		}
		var indexMails []IndexMail

		for _, h := range headers {
			if strings.HasPrefix(h.Subject, "[QMD:INDEX]|") {
				parts := strings.Split(h.Subject, "|")
				var mailVer int64 = 0
				if len(parts) >= 2 && strings.HasPrefix(parts[1], "v") {
					if v, err := strconv.ParseInt(parts[1][1:], 10, 64); err == nil {
						mailVer = v
					}
				}
				indexMails = append(indexMails, IndexMail{SeqNum: h.SeqNum, Version: mailVer})
			}
		}

		if len(indexMails) > 0 {
			sort.Slice(indexMails, func(i, j int) bool {
				return indexMails[i].Version > indexMails[j].Version
			})
			bestSeq := indexMails[0].SeqNum
			rawMsg, fErr := e.imapClient.FetchRawMessageBody(bestSeq)
			if fErr == nil {
				encData := extractAttachmentFromMIME(rawMsg)
				if plainJSON, err := e.DecryptData(encData); err == nil {
					var cloudIdx MasterIndex
					if json.Unmarshal(plainJSON, &cloudIdx) == nil {
						e.indexLock.Lock()
						e.masterIndex = cloudIdx
						e.indexLock.Unlock()
						configLock.RLock()
						curFolderForCache := GlobalConfig.StorageFolder
						configLock.RUnlock()
						e.SetCachedFolderIndex(curFolderForCache, cloudIdx)
						e.Log(fmt.Sprintf("✔ 成功从云端同步主索引！主索引更新次数:  %d, 纳管项目: %d", e.masterIndex.Version, len(e.masterIndex.Files)))
						go func() {
							if GlobalWebmailClient != nil && !GlobalWebmailClient.IsSessionValid() {
								GlobalWebmailClient.SetChecking(true)
								defer GlobalWebmailClient.SetChecking(false)
								if cloudSid, err := e.LoadCloudSession(); err == nil && cloudSid != "" {
									GlobalWebmailClient.RestoreSession(cloudSid)
								}
							}
							e.PrewarmAllFolderIndexes()
						}()

						// 清理历史残留多余的旧索引，确保唯一
						if len(indexMails) > 1 {
							var toDel []uint32
							for i := 1; i < len(indexMails); i++ {
								toDel = append(toDel, indexMails[i].SeqNum)
							}
							_ = e.imapClient.DeleteAndExpunge(toDel)
						}

						// 兼容旧版：从历史专属缩略图分卷拉取所有缩略图
						e.LoadThumbnailsFromCloud()
						go func() {
							time.Sleep(2 * time.Second)
							e.AutoMigrateSharedFilesToReceivedFolder()
						}()
						return false, nil
					}
				}
			}
		}
	}

	// 检查邮箱是否有任何切片数据邮件（含分享分片 [QMD:SHARE:DATA]|），如果有任何数据邮件，绝不当作新空间清空！
	if curBox != "" {
		_ = e.imapClient.EnsureAndSelectFolder(curBox)
	}
	dataSeqs, _ := e.imapClient.SearchMessageSeqs(`SUBJECT "[QMD:DATA]|"`)
	shareDataSeqs, _ := e.imapClient.SearchMessageSeqs(`SUBJECT "[QMD:SHARE:DATA]|"`)
	if len(dataSeqs) > 0 || len(shareDataSeqs) > 0 {
		e.Log("[安全保护] 检测到云端存有数据/分享分片但索引拉取暂时延迟，保持等待，绝不覆盖清空！")
		return false, fmt.Errorf("云端主索引加载中，请稍候")
	}

	// 只有云端完全是空邮箱、且没有任何数据时，才初始化首个版本
	e.Log("[首次初始化] 全新空间，建立初始云端索引...")
	e.indexLock.Lock()
	e.masterIndex = MasterIndex{
		Version:      1,
		NextSeq:      "0000000001",
		LastUpdated:  time.Now(),
		Files:        make(map[string]FileNode),
		ThumbVolumes: make(map[string]ThumbVolumeInfo),
		ActiveThumb:  "01",
	}
	initCopy := cloneMasterIndex(e.masterIndex)
	e.indexLock.Unlock()
	configLock.RLock()
	curEmptyFolder := GlobalConfig.StorageFolder
	configLock.RUnlock()
	e.SetCachedFolderIndex(curEmptyFolder, initCopy)

	// 全新空间同样尝试快速拉取当前账号已有的云端会话，优先恢复后再预热
	go func() {
		if GlobalWebmailClient != nil && !GlobalWebmailClient.IsSessionValid() {
			GlobalWebmailClient.SetChecking(true)
			defer GlobalWebmailClient.SetChecking(false)
			if cloudSid, err := e.LoadCloudSession(); err == nil && cloudSid != "" {
				GlobalWebmailClient.RestoreSession(cloudSid)
			}
		}
		e.PrewarmAllFolderIndexes()
	}()

	return true, e.SyncMasterIndexToCloud()
}

func cloneMasterIndex(src MasterIndex) MasterIndex {
	dst := src
	dst.Files = make(map[string]FileNode, len(src.Files))
	for k, v := range src.Files {
		dst.Files[k] = v
	}
	dst.ThumbVolumes = make(map[string]ThumbVolumeInfo, len(src.ThumbVolumes))
	for k, v := range src.ThumbVolumes {
		dst.ThumbVolumes[k] = v
	}
	return dst
}

func (e *StorageEngine) SetCachedFolderIndex(folder string, idx MasterIndex) {
	key := ToCanonicalFolder(strings.TrimSpace(folder))
	if key == "" {
		return
	}
	e.folderCacheMu.Lock()
	if e.folderIndexCache == nil {
		e.folderIndexCache = make(map[string]MasterIndex)
	}
	e.folderIndexCache[key] = cloneMasterIndex(idx)
	e.folderCacheRev++
	e.folderCacheMu.Unlock()
}

func (e *StorageEngine) GetCachedFolderIndex(folder string) (MasterIndex, bool) {
	key := ToCanonicalFolder(strings.TrimSpace(folder))
	if key == "" {
		return MasterIndex{}, false
	}
	e.folderCacheMu.RLock()
	idx, ok := e.folderIndexCache[key]
	e.folderCacheMu.RUnlock()
	if !ok {
		return MasterIndex{}, false
	}
	return cloneMasterIndex(idx), true
}

func (e *StorageEngine) DeleteCachedFolderIndex(folder string) {
	key := ToCanonicalFolder(strings.TrimSpace(folder))
	if key == "" {
		return
	}
	e.folderCacheMu.Lock()
	delete(e.folderIndexCache, key)
	e.folderCacheRev++
	e.folderCacheMu.Unlock()
}

func (e *StorageEngine) GetFolderCacheRev() int64 {
	e.folderCacheMu.RLock()
	rev := e.folderCacheRev
	e.folderCacheMu.RUnlock()
	return rev
}

func (e *StorageEngine) RenameCachedFolderIndex(oldFolder, newFolder string) {
	oldKey := ToCanonicalFolder(strings.TrimSpace(oldFolder))
	newKey := ToCanonicalFolder(strings.TrimSpace(newFolder))
	if oldKey == "" || newKey == "" {
		return
	}
	e.folderCacheMu.Lock()
	if idx, ok := e.folderIndexCache[oldKey]; ok {
		delete(e.folderIndexCache, oldKey)
		e.folderIndexCache[newKey] = idx
		e.folderCacheRev++
	}
	e.folderCacheMu.Unlock()
}

// PrewarmAllFolderIndexes 登录后后台静默预热其余所有隔离空间的云端索引，切换与移动/复制秒开
func (e *StorageEngine) PrewarmAllFolderIndexes() {
	if !GlobalConfig.IsConfigured() || e.imapClient == nil {
		return
	}
	e.prewarmMu.Lock()
	if e.prewarmActive {
		e.prewarmMu.Unlock()
		return
	}
	e.prewarmActive = true
	e.prewarmMu.Unlock()
	defer func() {
		e.prewarmMu.Lock()
		e.prewarmActive = false
		e.prewarmMu.Unlock()
	}()

	tempClient := NewIMAPClient(GlobalConfig.IMAPServer, GlobalConfig.Email, GlobalConfig.IMAPAuthCode)
	if err := tempClient.Connect(); err != nil {
		return
	}
	defer tempClient.Close()

	qmdList, err := tempClient.ListQMDFolders()
	if err != nil || len(qmdList) == 0 {
		return
	}

	var pending []string
	for _, raw := range qmdList {
		canonical := decodeModifiedUTF7(raw)
		key := ToCanonicalFolder(canonical)
		if key == "" {
			continue
		}
		if _, ok := e.GetCachedFolderIndex(key); !ok {
			pending = append(pending, canonical)
		}
	}
	if len(pending) == 0 {
		return
	}

	loadedCount := 0
	for _, canonical := range pending {
		if !GlobalConfig.IsConfigured() {
			break
		}
		actualBox := tempClient.FindActualMailboxName(canonical)
		if err := tempClient.EnsureAndSelectFolder(actualBox); err != nil {
			continue
		}
		headers, hErr := tempClient.FetchAllHeadersFast()
		if hErr != nil {
			continue
		}
		var bestSeq uint32
		var bestVer int64 = -1
		for _, h := range headers {
			if strings.HasPrefix(h.Subject, "[QMD:INDEX]|") {
				parts := strings.Split(h.Subject, "|")
				var mailVer int64 = 0
				if len(parts) >= 2 && strings.HasPrefix(parts[1], "v") {
					_, _ = fmt.Sscanf(parts[1][1:], "%d", &mailVer)
				}
				if mailVer > bestVer {
					bestVer = mailVer
					bestSeq = h.SeqNum
				}
			}
		}
		if bestSeq > 0 {
			if rawMsg, fErr := tempClient.FetchRawMessageBody(bestSeq); fErr == nil {
				encData := extractAttachmentFromMIME(rawMsg)
				if plainJSON, dErr := e.DecryptData(encData); dErr == nil {
					var cloudIdx MasterIndex
					if json.Unmarshal(plainJSON, &cloudIdx) == nil {
						if cloudIdx.Files == nil {
							cloudIdx.Files = make(map[string]FileNode)
						}
						e.SetCachedFolderIndex(canonical, cloudIdx)
						loadedCount++
						continue
					}
				}
			}
		}
		// 空空间也写入空索引缓存，避免切换和移动弹窗重复扫描
		e.SetCachedFolderIndex(canonical, MasterIndex{
			Version:      1,
			NextSeq:      "0000000001",
			LastUpdated:  time.Now(),
			Files:        make(map[string]FileNode),
			ThumbVolumes: make(map[string]ThumbVolumeInfo),
			ActiveThumb:  "01",
		})
		loadedCount++
	}
	if loadedCount > 0 {
		e.LogInfo("INDEX", fmt.Sprintf("✔ [索引预热] 已完成 %d 个其余隔离空间的云端索引预加载，切换与移动秒开就绪", loadedCount))
	}
}

func (e *StorageEngine) UpdateCipherKey(email string) {
	hasher := sha256.New()
	hasher.Write([]byte("QQMailDrive-MasterKey-2026-Salt"))
	hasher.Write([]byte(email))
	e.cipherKey = hasher.Sum(nil)
}

func (e *StorageEngine) LogWithLevel(level LogLevel, module, msg string) {
	timeStr := time.Now().Format("15:04:05")
	entry := fmt.Sprintf("[%s] [%s] [%s] %s", timeStr, level, module, msg)
	e.logsLock.Lock()
	e.globalLogs = append(e.globalLogs, entry)
	if len(e.globalLogs) > 20000 {
		e.globalLogs = e.globalLogs[len(e.globalLogs)-20000:]
	}
	e.structuredLogs = append(e.structuredLogs, LogEntry{
		Timestamp: time.Now(),
		TimeStr:   timeStr,
		Level:     level,
		Module:    module,
		Message:   msg,
	})
	if len(e.structuredLogs) > 20000 {
		e.structuredLogs = e.structuredLogs[len(e.structuredLogs)-20000:]
	}
	e.logsLock.Unlock()
}

func (e *StorageEngine) LogDebug(module, msg string) { e.LogWithLevel(LogLevelDebug, module, msg) }
func (e *StorageEngine) LogInfo(module, msg string)  { e.LogWithLevel(LogLevelInfo, module, msg) }
func (e *StorageEngine) LogWarn(module, msg string)  { e.LogWithLevel(LogLevelWarn, module, msg) }
func (e *StorageEngine) LogError(module, msg string) { e.LogWithLevel(LogLevelError, module, msg) }

func (e *StorageEngine) ClearLogs() {
	e.logsLock.Lock()
	e.globalLogs = nil
	e.structuredLogs = nil
	e.logsLock.Unlock()
}

func (e *StorageEngine) Log(msg string) {
	level := LogLevelInfo
	module := "SYSTEM"
	if strings.Contains(msg, "❌") || strings.Contains(msg, "失败") || strings.Contains(msg, "错误") {
		level = LogLevelError
	} else if strings.Contains(msg, "⚠️") || strings.Contains(msg, "超时") || strings.Contains(msg, "重试") {
		level = LogLevelWarn
	} else if strings.Contains(msg, "🔍") || strings.Contains(msg, "探针") || strings.Contains(msg, "TTFB") || strings.Contains(msg, "握手") {
		level = LogLevelDebug
	}

	if strings.Contains(msg, "IMAP") || strings.Contains(msg, "链路") || strings.Contains(msg, "连接池") {
		module = "IMAP"
	} else if strings.Contains(msg, "下载") || strings.Contains(msg, "拉取") || strings.Contains(msg, "落盘") {
		module = "DOWNLOAD"
	} else if strings.Contains(msg, "上传") || strings.Contains(msg, "切片") || strings.Contains(msg, "Deflate") {
		module = "UPLOAD"
	} else if strings.Contains(msg, "预览") || strings.Contains(msg, "推流") {
		module = "PREVIEW"
	} else if strings.Contains(msg, "索引") || strings.Contains(msg, "主索引") {
		module = "INDEX"
	} else if strings.Contains(msg, "整理") || strings.Contains(msg, "合包") {
		module = "COMPACT"
	}

	e.LogWithLevel(level, module, msg)
}

func (e *StorageEngine) GetGlobalLogs() []string {
	e.logsLock.RLock()
	defer e.logsLock.RUnlock()
	res := make([]string, len(e.globalLogs))
	copy(res, e.globalLogs)
	return res
}

func (e *StorageEngine) GetStructuredLogs(level LogLevel, module string, limit int) []LogEntry {
	e.logsLock.RLock()
	defer e.logsLock.RUnlock()
	var res []LogEntry
	for _, l := range e.structuredLogs {
		if level != "" && level != "ALL" && l.Level != level {
			continue
		}
		if module != "" && module != "ALL" && !strings.EqualFold(l.Module, module) {
			continue
		}
		res = append(res, l)
	}
	if limit > 0 && len(res) > limit {
		res = res[len(res)-limit:]
	}
	return res
}

func (e *StorageEngine) logTask(task *TransferTask, msg string) {
	level := LogLevelInfo
	module := "TRANSFER"
	if strings.Contains(msg, "❌") || strings.Contains(msg, "失败") {
		level = LogLevelError
	} else if strings.Contains(msg, "⚠️") || strings.Contains(msg, "重试") {
		level = LogLevelWarn
	} else if strings.Contains(msg, "🔍") || strings.Contains(msg, "探针") {
		level = LogLevelDebug
	}
	if task != nil {
		if task.Type == "download" || task.Type == "preview" {
			module = "DOWNLOAD"
		} else if task.Type == "upload" {
			module = "UPLOAD"
		}
	}
	timeStr := time.Now().Format("15:04:05")
	entry := fmt.Sprintf("[%s] [%s] [%s] %s", timeStr, level, module, msg)
	if task != nil {
		task.Logs = append(task.Logs, entry)
		if len(task.Logs) > 100 {
			task.Logs = task.Logs[len(task.Logs)-100:]
		}
	}
	taskName := ""
	if task != nil {
		taskName = task.FileName
	}
	if taskName != "" {
		e.LogWithLevel(level, module, fmt.Sprintf("[%s] %s", taskName, msg))
	} else {
		e.LogWithLevel(level, module, msg)
	}
}

func extractAttachmentFromMIME(msg []byte) []byte {
	prefix := []byte("content-transfer-encoding:")
	headerLower := bytes.ToLower(msg)
	magicBytes := []byte(MagicBytes)

	var candidates [][]byte
	searchPos := 0

	for {
		idx := bytes.Index(headerLower[searchPos:], prefix)
		if idx == -1 {
			break
		}
		actualIdx := searchPos + idx
		pos := actualIdx + len(prefix)
		for pos < len(headerLower) && (headerLower[pos] == ' ' || headerLower[pos] == '\t') {
			pos++
		}
		if bytes.HasPrefix(headerLower[pos:], []byte("base64")) {
			sub := msg[pos:]
			sepIdx := bytes.Index(sub, []byte("\r\n\r\n"))
			sepLen := 4
			if sepIdx == -1 {
				sepIdx = bytes.Index(sub, []byte("\n\n"))
				sepLen = 2
			}
			if sepIdx != -1 {
				bodyStart := pos + sepIdx + sepLen
				if bodyStart < len(msg) {
					bodySub := msg[bodyStart:]
					rawB64 := bodySub
					if boundaryIdx := bytes.Index(bodySub, []byte("\n--")); boundaryIdx != -1 {
						rawB64 = bodySub[:boundaryIdx]
					}

					cleanB64 := make([]byte, len(rawB64))
					cleanLen := 0
					for _, b := range rawB64 {
						if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '+' || b == '/' || b == '=' {
							cleanB64[cleanLen] = b
							cleanLen++
						}
					}
					cleanB64 = cleanB64[:cleanLen]
					if cleanLen > 0 {
						decoded := make([]byte, base64.StdEncoding.DecodedLen(cleanLen))
						if n, err := base64.StdEncoding.Decode(decoded, cleanB64); err == nil && n > 0 {
							dec := decoded[:n]
							if !bytes.HasPrefix(dec, []byte(MagicThumbnailBytes)) {
								if bytes.HasPrefix(dec, magicBytes) {
									return dec
								}
								candidates = append(candidates, dec)
							}
						} else if dec, decErr := base64.StdEncoding.DecodeString(string(cleanB64)); decErr == nil && len(dec) > 0 {
							if !bytes.HasPrefix(dec, []byte(MagicThumbnailBytes)) {
								if bytes.HasPrefix(dec, magicBytes) {
									return dec
								}
								candidates = append(candidates, dec)
							}
						}
					}
				}
			}
		}
		searchPos = pos + 6
		if searchPos >= len(headerLower) {
			break
		}
	}

	var largest []byte
	for _, c := range candidates {
		if len(c) > len(largest) {
			largest = c
		}
	}
	return largest
}

func formatBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/float64(div), "KMGTPE"[exp])
}


const sessionTargetFolder = "QMD_收到的文件"

// SaveCloudSession 将当前有效 Webmail 会话令牌以 AES-256 加密存入「收到的文件」系统专属邮件 [QMD:SESSION] (永不被删，本地零留存)
func (e *StorageEngine) SaveCloudSession(sid string) error {
	cleanSid := strings.TrimSpace(sid)
	if cleanSid == "" || cleanSid == "getUrlParam" || len(cleanSid) < 15 {
		return fmt.Errorf("无效 sid")
	}
	if !e.imapClient.connected {
		return fmt.Errorf("IMAP 未连接")
	}

	encData, err := e.EncryptData([]byte(cleanSid))
	if err != nil {
		return err
	}

	subject := fmt.Sprintf("[QMD:SESSION]|%d", time.Now().Unix())
	meta := base64.StdEncoding.EncodeToString([]byte(`{"type":"webmail_session"}`))
	mimeData := e.BuildMIMEMessage(subject, meta, "session.dat", encData)

	currentFolder := GlobalConfig.StorageFolder

	// 确保系统专属「收到的文件」目录就绪并写入
	if err := e.imapClient.EnsureAndSelectFolder(sessionTargetFolder); err != nil {
		e.Log(fmt.Sprintf("❌ 进入「收到的文件」目录失败: %v", err))
		return err
	}

	// 1. 先清理该系统目录下残留的旧 session 邮件，提前腾出容量
	headers, hErr := e.imapClient.FetchAllHeadersFast()
	if hErr == nil {
		var oldSeqs []uint32
		for _, h := range headers {
			if strings.HasPrefix(h.Subject, "[QMD:SESSION]|") {
				oldSeqs = append(oldSeqs, h.SeqNum)
			}
		}
		if len(oldSeqs) > 0 {
			_ = e.imapClient.DeleteAndExpunge(oldSeqs)
		}
	}

	// 2. 写入最新云端会话
	if err := e.imapClient.AppendDirectMessage(sessionTargetFolder, subject, meta, mimeData); err != nil {
		if currentFolder != "" && currentFolder != sessionTargetFolder {
			_ = e.imapClient.EnsureAndSelectFolder(currentFolder)
		}
		if strings.Contains(err.Error(), "OVERQUOTA") {
			e.LogWarn("WEBMAIL", "⚠️ [云端会话漫游] QQ 邮箱容量已达上限 (OVERQUOTA)，会话令牌云端备份暂缓写入。")
		}
		return err
	}
	e.Log(fmt.Sprintf("✔ [云端专属漫游] Webmail sid 已加密存入「收到的文件」邮件: %s (本地零留存，不怕隔离空间被删，跨设备永久漫游)", subject))

	// 严格恢复切回用户当前的业务网盘隔离空间
	if currentFolder != "" && currentFolder != sessionTargetFolder {
		_ = e.imapClient.EnsureAndSelectFolder(currentFolder)
	}

	return nil
}

// LoadCloudSession 从系统专属「收到的文件」独立邮件 [QMD:SESSION] 中检索并解密恢复有效 sid
func (e *StorageEngine) LoadCloudSession() (string, error) {
	if !e.imapClient.connected {
		return "", fmt.Errorf("IMAP 未连接")
	}

	currentFolder := GlobalConfig.StorageFolder

	// 选定「收到的文件」目录
	if err := e.imapClient.EnsureAndSelectFolder(sessionTargetFolder); err != nil {
		return "", err
	}
	defer func() {
		if currentFolder != "" && currentFolder != sessionTargetFolder {
			_ = e.imapClient.EnsureAndSelectFolder(currentFolder)
		}
	}()

	headers, err := e.imapClient.FetchAllHeadersFast()
	if err != nil {
		return "", err
	}

	var targetSeq uint32
	var maxTime int64 = 0
	for _, h := range headers {
		if strings.HasPrefix(h.Subject, "[QMD:SESSION]|") {
			parts := strings.Split(h.Subject, "|")
			var t int64 = 0
			if len(parts) >= 2 {
				t, _ = strconv.ParseInt(parts[1], 10, 64)
			}
			if t >= maxTime {
				maxTime = t
				targetSeq = h.SeqNum
			}
		}
	}

	if targetSeq == 0 {
		return "", fmt.Errorf("未在「收到的文件」中找到有效会话邮件")
	}

	rawMsg, err := e.imapClient.FetchRawMessageBody(targetSeq)
	if err != nil {
		return "", err
	}

	encData := extractAttachmentFromMIME(rawMsg)
	plainBytes, err := e.DecryptData(encData)
	if err != nil {
		return "", err
	}

	sid := strings.TrimSpace(string(plainBytes))
	if sid == "" || sid == "getUrlParam" || len(sid) < 15 {
		return "", fmt.Errorf("云端解析出的 sid 无效")
	}

	e.Log(fmt.Sprintf("✔ [云端会话恢复] 成功从「收到的文件」恢复 Webmail sid: %s (本地零留痕，跨设备无感漫游)", sid))
	return sid, nil
}

// DeleteCloudSession 清除「收到的文件」中的会话邮件 (在账号切换或失效时触发)
func (e *StorageEngine) DeleteCloudSession() {
	if !e.imapClient.connected {
		return
	}

	currentFolder := GlobalConfig.StorageFolder
	if err := e.imapClient.EnsureAndSelectFolder(sessionTargetFolder); err != nil {
		return
	}
	defer func() {
		if currentFolder != "" && currentFolder != sessionTargetFolder {
			_ = e.imapClient.EnsureAndSelectFolder(currentFolder)
		}
	}()

	headers, err := e.imapClient.FetchAllHeadersFast()
	if err != nil {
		return
	}
	var toDel []uint32
	for _, h := range headers {
		if strings.HasPrefix(h.Subject, "[QMD:SESSION]|") {
			toDel = append(toDel, h.SeqNum)
		}
	}
	if len(toDel) > 0 {
		_ = e.imapClient.DeleteAndExpunge(toDel)
		e.Log("🗑️ [云端会话清理] 已从「收到的文件」彻底清除旧的 [QMD:SESSION] 会话邮件")
	}
}

// SaveCloudConfig 将所有网盘核心配置参数以 AES-256 加密存入「收到的文件」系统专属邮件 [QMD:CONFIG] (多端永久漫游，按电脑ID独立记录下载路径)
func (e *StorageEngine) SaveCloudConfig(cfg Config) error {
	if !e.imapClient.connected {
		return fmt.Errorf("IMAP 未连接")
	}

	cid := GetComputerID()
	downloadDirs := make(map[string]string)
	configLock.RLock()
	if GlobalConfig.DownloadDirs != nil {
		for k, v := range GlobalConfig.DownloadDirs {
			downloadDirs[k] = v
		}
	}
	configLock.RUnlock()

	if cfg.DownloadDirs != nil {
		for k, v := range cfg.DownloadDirs {
			if v != "" {
				downloadDirs[k] = v
			}
		}
	}
	if cfg.DownloadDir != "" {
		downloadDirs[cid] = cfg.DownloadDir
	}

	cloudData := CloudConfigData{
		Version:                  1,
		UpdatedTime:              time.Now().Unix(),
		UserNick:                 cfg.UserNick,
		StorageFolder:            cfg.StorageFolder,
		ChunkSizeBytes:           cfg.ChunkSizeBytes,
		MaxConcurrency:           cfg.MaxConcurrency,
		ChunkDownloadConcurrency: cfg.ChunkDownloadConcurrency,
		HTTPDownloadConcurrency:  cfg.HTTPDownloadConcurrency,
		HTTPUploadConcurrency:    cfg.HTTPUploadConcurrency,
		RequestDelayMS:           cfg.RequestDelayMS,
		AutoPackSmallFiles:       cfg.AutoPackSmallFiles,
		SmallFileThresholdMB:     cfg.SmallFileThresholdMB,
		ThumbSizePX:              cfg.ThumbSizePX,
		ThumbLocalCache:          cfg.ThumbLocalCache,
		SMTPServer:               cfg.SMTPServer,
		DownloadDirs:             downloadDirs,
	}

	rawJSON, err := json.Marshal(cloudData)
	if err != nil {
		return fmt.Errorf("序列化云端配置失败: %w", err)
	}

	encData, err := e.EncryptData(rawJSON)
	if err != nil {
		return fmt.Errorf("加密云端配置失败: %w", err)
	}

	subject := fmt.Sprintf("[QMD:CONFIG]|%d", time.Now().Unix())
	meta := base64.StdEncoding.EncodeToString([]byte(`{"type":"cloud_config"}`))
	mimeData := e.BuildMIMEMessage(subject, meta, "config.dat", encData)

	currentFolder := GlobalConfig.StorageFolder
	if cfg.StorageFolder != "" {
		currentFolder = cfg.StorageFolder
	}

	// 确保进入「收到的文件」专属目录
	if err := e.imapClient.EnsureAndSelectFolder(sessionTargetFolder); err != nil {
		e.Log(fmt.Sprintf("❌ 进入「收到的文件」目录失败: %v", err))
		return err
	}

	// 1. 先清理该目录下残留的旧 [QMD:CONFIG]| 邮件，提前腾出容量与邮件配额
	headers, hErr := e.imapClient.FetchAllHeadersFast()
	if hErr == nil {
		var oldSeqs []uint32
		for _, h := range headers {
			if strings.HasPrefix(h.Subject, "[QMD:CONFIG]|") {
				oldSeqs = append(oldSeqs, h.SeqNum)
			}
		}
		if len(oldSeqs) > 0 {
			_ = e.imapClient.DeleteAndExpunge(oldSeqs)
		}
	}

	// 2. 写入最新云端配置邮件
	if err := e.imapClient.AppendDirectMessage(sessionTargetFolder, subject, meta, mimeData); err != nil {
		if currentFolder != "" && currentFolder != sessionTargetFolder {
			_ = e.imapClient.EnsureAndSelectFolder(currentFolder)
		}
		if strings.Contains(err.Error(), "OVERQUOTA") {
			e.LogWarn("CONFIG", "⚠️ [云端同步提示] 本地配置已保存成功！但检测到当前 QQ 邮箱存储空间已满 (NO [OVERQUOTA])，云端漫游备份邮件写入受阻。请登录 mail.qq.com 清理垃圾箱/已删除邮件或点击免费扩容升级容量。")
		} else {
			e.LogWarn("CONFIG", fmt.Sprintf("⚠️ [云端同步提示] 本地配置已保存成功，云端漫游邮件同步提示: %v", err))
		}
		return err
	}
	e.Log(fmt.Sprintf("✔ [云端配置同步] 软件设置已加密存入「收到的文件」邮件: %s (本地零丢失，多设备永久漫游，已同步本机下载路径: %s)", subject, cfg.DownloadDir))

	// 严格恢复切回用户当前的业务网盘空间
	if currentFolder != "" && currentFolder != sessionTargetFolder {
		_ = e.imapClient.EnsureAndSelectFolder(currentFolder)
	}

	return nil
}

// LoadCloudConfig 从「收到的文件」系统专属邮件 [QMD:CONFIG] 中检索并解密恢复最新云端配置
func (e *StorageEngine) LoadCloudConfig() (*CloudConfigData, error) {
	if !e.imapClient.connected {
		return nil, fmt.Errorf("IMAP 未连接")
	}

	currentFolder := GlobalConfig.StorageFolder

	if err := e.imapClient.EnsureAndSelectFolder(sessionTargetFolder); err != nil {
		return nil, err
	}
	defer func() {
		if currentFolder != "" && currentFolder != sessionTargetFolder {
			_ = e.imapClient.EnsureAndSelectFolder(currentFolder)
		}
	}()

	headers, err := e.imapClient.FetchAllHeadersFast()
	if err != nil {
		return nil, err
	}

	var targetSeq uint32
	var maxTime int64 = 0
	for _, h := range headers {
		if strings.HasPrefix(h.Subject, "[QMD:CONFIG]|") {
			parts := strings.Split(h.Subject, "|")
			var t int64 = 0
			if len(parts) >= 2 {
				t, _ = strconv.ParseInt(parts[1], 10, 64)
			}
			if t >= maxTime {
				maxTime = t
				targetSeq = h.SeqNum
			}
		}
	}

	if targetSeq == 0 {
		return nil, fmt.Errorf("未在「收到的文件」中找到云端配置备份邮件")
	}

	rawMsg, err := e.imapClient.FetchRawMessageBody(targetSeq)
	if err != nil {
		return nil, err
	}

	encData := extractAttachmentFromMIME(rawMsg)
	plainBytes, err := e.DecryptData(encData)
	if err != nil {
		return nil, fmt.Errorf("解密云端配置失败: %w", err)
	}

	var cloudData CloudConfigData
	if err := json.Unmarshal(plainBytes, &cloudData); err != nil {
		return nil, fmt.Errorf("解析云端配置 JSON 失败: %w", err)
	}

	e.Log(fmt.Sprintf("✔ [云端配置同步] 成功拉取云端设置 (时间戳: %d, 分片: %d 字节, 并发: %d, 防风控延时: %dms, 纳管设备数: %d)",
		cloudData.UpdatedTime, cloudData.ChunkSizeBytes, cloudData.ChunkDownloadConcurrency, cloudData.RequestDelayMS, len(cloudData.DownloadDirs)))
	return &cloudData, nil
}

// AutoMigrateSharedFilesToReceivedFolder 自动纠偏自愈：若历史在常规空间(如 529481127)中误存了分享文件，自动将分片邮件与索引迁移回专属隔离空间「收到的文件」
func (e *StorageEngine) AutoMigrateSharedFilesToReceivedFolder() {
	targetFolder := e.imapClient.FindActualMailboxName("收到的文件")
	if targetFolder == "" {
		targetFolder = "QMD_收到的文件"
	}

	qmdList, err := e.imapClient.ListQMDFolders()
	if err != nil {
		return
	}

	for _, raw := range qmdList {
		canonical := decodeModifiedUTF7(raw)
		if canonical == targetFolder || ToDisplayFolder(canonical) == "收到的文件" {
			continue
		}

		tempClient := NewIMAPClient(GlobalConfig.IMAPServer, GlobalConfig.Email, GlobalConfig.IMAPAuthCode)
		if err := tempClient.Connect(); err != nil {
			continue
		}

		if err := tempClient.EnsureAndSelectFolder(canonical); err != nil {
			tempClient.Close()
			continue
		}

		tHeaders, _ := tempClient.FetchAllHeadersFast()
		var bestIdx uint32
		var bestVer int64 = 0
		for _, th := range tHeaders {
			if strings.HasPrefix(th.Subject, "[QMD:INDEX]|") {
				parts := strings.Split(th.Subject, "|")
				if len(parts) >= 2 && strings.HasPrefix(parts[1], "v") {
					var v int64
					fmt.Sscanf(parts[1][1:], "%d", &v)
					if v > bestVer {
						bestVer = v
						bestIdx = th.SeqNum
					}
				}
			}
		}

		if bestIdx > 0 {
			if rawMsg, rErr := tempClient.FetchRawMessageBody(bestIdx); rErr == nil {
				enc := extractAttachmentFromMIME(rawMsg)
				if plain, dErr := e.DecryptData(enc); dErr == nil {
					var otherIdx MasterIndex
					if json.Unmarshal(plain, &otherIdx) == nil {
						hasShared := false
						for p := range otherIdx.Files {
							if strings.HasPrefix(p, "/收到的文件") {
								hasShared = true
								break
							}
						}
						tempClient.Close()
						if hasShared {
							e.LogInfo("ENGINE", fmt.Sprintf("🔄 [自动纠偏归档位置] 发现空间 [%s] 中误存了好友分享文件，正在自动迁移归档至专属隔离空间「收到的文件」...", ToDisplayFolder(canonical)))
							configLock.Lock()
							oldCur := GlobalConfig.StorageFolder
							GlobalConfig.StorageFolder = canonical
							configLock.Unlock()

							_ = e.imapClient.EnsureAndSelectFolder(canonical)
							_, _ = e.InitOrLoadCloudIndex()
							_ = e.MoveFileToFolder("/收到的文件", targetFolder, "/")

							configLock.Lock()
							GlobalConfig.StorageFolder = oldCur
							configLock.Unlock()
							_ = e.imapClient.EnsureAndSelectFolder(oldCur)
							_, _ = e.InitOrLoadCloudIndex()
						}
					} else {
						tempClient.Close()
					}
				} else {
					tempClient.Close()
				}
			} else {
				tempClient.Close()
			}
		} else {
			tempClient.Close()
		}
	}
}

// extractThumbnailFromMIME 从邮件 MIME 数据流中精准提取以 QQTHM01 为特征头的缩略图密文
func extractThumbnailFromMIME(msg []byte) []byte {
	prefix := []byte("content-transfer-encoding:")
	headerLower := bytes.ToLower(msg)
	thumbMagic := []byte(MagicThumbnailBytes)

	searchPos := 0
	for {
		idx := bytes.Index(headerLower[searchPos:], prefix)
		if idx == -1 {
			break
		}
		actualIdx := searchPos + idx
		pos := actualIdx + len(prefix)
		for pos < len(headerLower) && (headerLower[pos] == ' ' || headerLower[pos] == '\t') {
			pos++
		}
		if bytes.HasPrefix(headerLower[pos:], []byte("base64")) {
			sub := msg[pos:]
			sepIdx := bytes.Index(sub, []byte("\r\n\r\n"))
			sepLen := 4
			if sepIdx == -1 {
				sepIdx = bytes.Index(sub, []byte("\n\n"))
				sepLen = 2
			}
			if sepIdx != -1 {
				bodyStart := pos + sepIdx + sepLen
				if bodyStart < len(msg) {
					bodySub := msg[bodyStart:]
					rawB64 := bodySub
					if boundaryIdx := bytes.Index(bodySub, []byte("\n--")); boundaryIdx != -1 {
						rawB64 = bodySub[:boundaryIdx]
					}
					cleanB64 := make([]byte, len(rawB64))
					cleanLen := 0
					for _, b := range rawB64 {
						if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '+' || b == '/' || b == '=' {
							cleanB64[cleanLen] = b
							cleanLen++
						}
					}
					cleanB64 = cleanB64[:cleanLen]
					if cleanLen > 0 {
						if dec, err := base64.StdEncoding.DecodeString(string(cleanB64)); err == nil {
							if bytes.HasPrefix(dec, thumbMagic) {
								return dec
							}
						}
					}
				}
			}
		}
		searchPos = pos + 6
		if searchPos >= len(headerLower) {
			break
		}
	}
	return nil
}

func isMediaExt(name string) bool {
	lower := strings.ToLower(name)
	exts := []string{".jpg", ".jpeg", ".png", ".webp", ".gif", ".bmp", ".mp4", ".mkv", ".mov", ".avi", ".flv", ".webm", ".ts"}
	for _, ext := range exts {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

func isInboxFolder(folder string) bool {
	f := strings.TrimSpace(strings.ToUpper(folder))
	return f == "INBOX" || f == "收件箱" || f == ""
}


// ExtractThumbnailData 从邮件正文或 MIME 数据流中精准解析出以 QQTHM01 为特征头的缩略图密文
func (e *StorageEngine) ExtractThumbnailData(partBytes []byte) []byte {
	if len(partBytes) == 0 {
		return nil
	}
	thumbMagic := []byte(MagicThumbnailBytes) // "QQTHM01"

	// 1. 若本身已经是二进制密文 (已包含 QQTHM01 特征头)
	if bytes.HasPrefix(partBytes, thumbMagic) {
		return partBytes
	}

	// 2. 先尝试从完整 MIME 结构中提取
	if enc := extractThumbnailFromMIME(partBytes); len(enc) > 0 && bytes.HasPrefix(enc, thumbMagic) {
		return enc
	}

	// 3. 处理 quoted-printable 软换行与 =3D，并精准定位 QQTHM01 的 Base64 特征头 (UVFUSE0wM)
	normBytes := bytes.ReplaceAll(partBytes, []byte("=\r\n"), nil)
	normBytes = bytes.ReplaceAll(normBytes, []byte("=\n"), nil)
	normBytes = bytes.ReplaceAll(normBytes, []byte("=3D"), []byte("="))

	stripHTMLTags := func(raw []byte) []byte {
		var out bytes.Buffer
		inTag := false
		for _, b := range raw {
			if b == '<' {
				inTag = true
				continue
			}
			if b == '>' {
				inTag = false
				continue
			}
			if !inTag {
				out.WriteByte(b)
			}
		}
		return out.Bytes()
	}

	extractByMagic := func(raw []byte) []byte {
		cleaned := stripHTMLTags(raw)
		magicIdx := bytes.Index(cleaned, []byte("UVFUSE0wM"))
		if magicIdx == -1 {
			return nil
		}
		var b64 []byte
		sawPad := false
		for i := magicIdx; i < len(cleaned); i++ {
			b := cleaned[i]
			if b == '=' {
				sawPad = true
				b64 = append(b64, b)
			} else if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '+' || b == '/' {
				if sawPad {
					break
				}
				b64 = append(b64, b)
			} else if b == '\r' || b == '\n' || b == ' ' || b == '\t' {
				continue
			} else {
				break
			}
		}
		if len(b64) > 8 {
			b64Str := string(b64)
			if dec, err := base64.StdEncoding.DecodeString(b64Str); err == nil && bytes.HasPrefix(dec, thumbMagic) {
				return dec
			}
			rawStr := strings.TrimRight(b64Str, "=")
			if rem := len(rawStr) % 4; rem == 1 && len(rawStr) > 1 {
				rawStr = rawStr[:len(rawStr)-1]
			}
			if dec, err := base64.RawStdEncoding.DecodeString(rawStr); err == nil && bytes.HasPrefix(dec, thumbMagic) {
				return dec
			}
		}
		return nil
	}

	if hit := extractByMagic(normBytes); len(hit) > 0 {
		return hit
	}

	// 解码常见 quoted-printable 转义字符
	normBytes = bytes.ReplaceAll(normBytes, []byte("=3C"), []byte("<"))
	normBytes = bytes.ReplaceAll(normBytes, []byte("=3c"), []byte("<"))
	normBytes = bytes.ReplaceAll(normBytes, []byte("=3E"), []byte(">"))
	normBytes = bytes.ReplaceAll(normBytes, []byte("=3e"), []byte(">"))
	normBytes = bytes.ReplaceAll(normBytes, []byte("=2B"), []byte("+"))
	normBytes = bytes.ReplaceAll(normBytes, []byte("=2b"), []byte("+"))
	normBytes = bytes.ReplaceAll(normBytes, []byte("=2F"), []byte("/"))
	normBytes = bytes.ReplaceAll(normBytes, []byte("=2f"), []byte("/"))
	if hit := extractByMagic(normBytes); len(hit) > 0 {
		return hit
	}

	// 若整段正文被 Webmail 以多行 Base64 编码包裹 (<pre>UVFUSE0wM...</pre>)，拼接连续 Base64 行后解码再提取
	var b64Block bytes.Buffer
	flushB64Block := func() []byte {
		if b64Block.Len() > 32 {
			s := b64Block.String()
			var outerDec []byte
			if d, err := base64.StdEncoding.DecodeString(s); err == nil {
				outerDec = d
			} else {
				raw := strings.TrimRight(s, "=")
				if rem := len(raw) % 4; rem == 1 && len(raw) > 1 {
					raw = raw[:len(raw)-1]
				}
				if d2, err2 := base64.RawStdEncoding.DecodeString(raw); err2 == nil {
					outerDec = d2
				}
			}
			if len(outerDec) > 0 {
				if hit := extractByMagic(outerDec); len(hit) > 0 {
					return hit
				}
			}
		}
		b64Block.Reset()
		return nil
	}
	isB64Line := func(ln []byte) bool {
		if len(ln) == 0 || bytes.HasPrefix(ln, []byte("--")) || bytes.Contains(ln, []byte(":")) || bytes.Contains(ln, []byte("<")) || bytes.Contains(ln, []byte(" ")) {
			return false
		}
		for _, c := range ln {
			if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '+' || c == '/' || c == '=') {
				return false
			}
		}
		return true
	}
	for _, line := range bytes.Split(normBytes, []byte("\n")) {
		trimmed := bytes.TrimSpace(line)
		if isB64Line(trimmed) {
			b64Block.Write(trimmed)
		} else if hit := flushB64Block(); len(hit) > 0 {
			return hit
		}
	}
	if hit := flushB64Block(); len(hit) > 0 {
		return hit
	}

	// 4. 兜底剥离可能存在的 HTML 标签 (如 Webmail 自动包裹的 <pre>...</pre> 或 <div>...</div>)
	var cleanBuf bytes.Buffer
	inTag := false
	for _, b := range normBytes {
		if b == '<' {
			inTag = true
			continue
		}
		if b == '>' {
			inTag = false
			continue
		}
		if !inTag {
			cleanBuf.WriteByte(b)
		}
	}

	// 4. 严格提取 Base64 字符流 (过滤一切换行与不可见字符)
	rawBytes := cleanBuf.Bytes()
	cleanB64 := make([]byte, len(rawBytes))
	cleanLen := 0
	for _, b := range rawBytes {
		if (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z') || (b >= '0' && b <= '9') || b == '+' || b == '/' || b == '=' {
			cleanB64[cleanLen] = b
			cleanLen++
		}
	}
	cleanB64 = cleanB64[:cleanLen]
	if cleanLen > 0 {
		if dec, err := base64.StdEncoding.DecodeString(string(cleanB64)); err == nil {
			if bytes.HasPrefix(dec, thumbMagic) {
				return dec
			}
		}
	}

	return nil
}

// GetOrFetchEncryptedThumbnail 获取或自愈拉取指定文件的加密缩略图密文 (带 QQTHM01 魔法头)
func (e *StorageEngine) GetOrFetchEncryptedThumbnail(node *FileNode) []byte {
	if node == nil || !isMediaExt(node.Name) {
		return nil
	}

	fileKey := e.DeriveFileKey(node.SHA256)
	if node.CipherKeyBase64 != "" {
		if k, err := base64.StdEncoding.DecodeString(node.CipherKeyBase64); err == nil && len(k) == 32 {
			fileKey = k
		}
	}

	// 1. 优先从本地缩略图缓存读取并加密
	if b, ok := e.GetCachedThumb(node.SHA256); ok && len(b) > 0 {
		if enc, encErr := e.EncryptThumbnail(b, fileKey); encErr == nil {
			return enc
		}
	}

	// 2. 本地不存在时，锁定目标隔离空间后从云端分片 #1 邮件正文 (Part 1 / 1.2 / 1.1) 拉取
	if e.imapClient != nil && e.imapClient.connected {
		configLock.RLock()
		targetBox := GlobalConfig.StorageFolder
		configLock.RUnlock()
		if node.IsSharedIn || node.ShareID != "" {
			if recvBox := e.imapClient.FindActualMailboxName("收到的文件"); recvBox != "" {
				targetBox = recvBox
			}
		}
		if targetBox != "" {
			_ = e.imapClient.EnsureAndSelectFolder(targetBox)
		}
		headers, hErr := e.imapClient.FetchAllHeadersFast()
		if hErr != nil {
			e.LogWarn("THUMB", fmt.Sprintf("⚠️ [%s] 拉取目录 [%s] 邮件头失败: %v", node.Name, targetBox, hErr))
		} else {
			seqs := e.LocateChunkSeqsForNode(node, headers)
			if len(seqs) == 0 {
				e.LogWarn("THUMB", fmt.Sprintf("⚠️ [%s] 在目录 [%s] 未定位到分片 #1 (扫描邮件数: %d, sha256=%s)", node.Name, targetBox, len(headers), node.SHA256))
			} else {
				for _, partID := range []string{"1", "1.2", "1.1"} {
					if targetBox != "" {
						_ = e.imapClient.EnsureAndSelectFolder(targetBox)
					}
					partBytes, pErr := e.imapClient.FetchMessagePart(seqs[0], partID)
					if pErr == nil && len(partBytes) > 0 {
						enc := e.ExtractThumbnailData(partBytes)
						if len(enc) > 0 {
							// 自动回填缩略图缓存
							if decPlain, dErr := e.DecryptThumbnail(enc, fileKey); dErr == nil && len(decPlain) > 0 {
								e.SaveCachedThumb(node.SHA256, decPlain)
							} else {
								e.MarkNoThumb(node.SHA256)
							}
							return enc
						}
						sample := partBytes
					if len(sample) > 120 {
						sample = sample[:120]
					}
					e.LogWarn("THUMB", fmt.Sprintf("⚠️ [%s] seq=%d part=%s 返回 %d 字节但未解析出 QQTHM01 (前120字节=%q)", node.Name, seqs[0], partID, len(partBytes), string(sample)))
					} else {
						e.LogWarn("THUMB", fmt.Sprintf("⚠️ [%s] seq=%d part=%s 拉取失败/为空: err=%v, len=%d", node.Name, seqs[0], partID, pErr, len(partBytes)))
					}
				}
			}
		}
	}

	return nil
}
