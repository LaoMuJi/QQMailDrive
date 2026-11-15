package main

import (
	"bytes"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed web/*
var embeddedWebFS embed.FS

type WebServer struct {
	port    int
	engine  *StorageEngine
	checker *ConsistencyChecker
}

var hasPromptedCrash = false

func NewWebServer(port int, engine *StorageEngine) *WebServer {
	return &WebServer{
		port:    port,
		engine:  engine,
		checker: NewConsistencyChecker(engine, engine.imapClient),
	}
}

func (s *WebServer) Start() error {
	mux := http.NewServeMux()

	// 静态文件服务 (自适应：优先磁盘 web/ 目录便于热调，无目录时无缝回退至内置嵌入资源，彻底禁用浏览器强缓存)
	var fileHandler http.Handler
	if stat, err := os.Stat("web"); err == nil && stat.IsDir() {
		fileHandler = http.FileServer(http.Dir("web"))
	} else {
		subFS, sErr := fs.Sub(embeddedWebFS, "web")
		if sErr == nil {
			fileHandler = http.FileServer(http.FS(subFS))
		} else {
			fileHandler = http.FileServer(http.Dir("web"))
		}
	}

	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		fileHandler.ServeHTTP(w, r)
	}))

	// API 路由
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/status/ack_clean_exit", s.handleAckCleanExit)
	mux.HandleFunc("/api/config", s.handleConfig)
	mux.HandleFunc("/api/files", s.handleFiles)
	mux.HandleFunc("/api/tasks", s.handleTasks)
	mux.HandleFunc("/api/tasks/pause_all", s.handleTasksPauseAll)
	mux.HandleFunc("/api/tasks/resume_all", s.handleTasksResumeAll)
	mux.HandleFunc("/api/tasks/toggle", s.handleTasksToggle)
	mux.HandleFunc("/api/tasks/clear_completed", s.handleTasksClearCompleted)
	mux.HandleFunc("/api/tasks/cancel", s.handleTasksCancel)
	mux.HandleFunc("/api/logs", s.handleLogs)
	mux.HandleFunc("/api/logs/clear", s.handleLogsClear)
	mux.HandleFunc("/api/upload", s.handleUpload)
	mux.HandleFunc("/api/preview", s.handlePreview)
	mux.HandleFunc("/api/thumb", s.handleThumbnail)
	mux.HandleFunc("/api/fs/download_local", s.handleDownloadLocal)
	mux.HandleFunc("/api/fs/pick_dir", s.handlePickDir)
	mux.HandleFunc("/api/download", s.handleDownloadLocal)
	mux.HandleFunc("/api/fs/mkdir", s.handleMkdir)
	mux.HandleFunc("/api/fs/rename", s.handleRename)
	mux.HandleFunc("/api/fs/move", s.handleMove)
	mux.HandleFunc("/api/fs/copy", s.handleCopy)
	mux.HandleFunc("/api/fs/delete", s.handleDelete)
	mux.HandleFunc("/api/fs/batch_delete", s.handleBatchDelete)
	mux.HandleFunc("/api/health/check", s.handleHealthCheck)
	mux.HandleFunc("/api/health/repair", s.handleHealthRepair)
	mux.HandleFunc("/api/maintenance/compact", s.handleCompact)
	mux.HandleFunc("/api/folders", s.handleFolders)
	mux.HandleFunc("/api/folders/create", s.handleFolderCreate)
	mux.HandleFunc("/api/folders/rename", s.handleFolderRename)
	mux.HandleFunc("/api/folders/delete", s.handleFolderDelete)
	mux.HandleFunc("/api/folders/switch", s.handleFolderSwitch)
	mux.HandleFunc("/api/folders/subdirs", s.handleFolderSubdirs)
	mux.HandleFunc("/api/share/create", s.handleShareCreate)
	mux.HandleFunc("/api/init/check", s.handleInitCheck)
	mux.HandleFunc("/api/webmail/status", s.handleWebmailStatus)
	mux.HandleFunc("/api/webmail/qr", s.handleWebmailQR)
	mux.HandleFunc("/api/webmail/qr_check", s.handleWebmailQRCheck)
	mux.HandleFunc("/api/webmail/set_sid", s.handleWebmailSetSid)
	mux.HandleFunc("/api/imap/reconnect", s.handleImapReconnect)
	mux.HandleFunc("/api/exit", s.handleExit)

	addr := fmt.Sprintf("127.0.0.1:%d", s.port)
	fmt.Printf("[QQMailDrive] 本地 Web 控制面板已就绪: http://%s\n", addr)
	return http.ListenAndServe(addr, mux)
}

func (s *WebServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.engine.indexLock.RLock()
	var totalSize int64
	var fileCount int
	for _, f := range s.engine.masterIndex.Files {
		if !f.IsDir {
			totalSize += f.Size
			fileCount++
		}
	}
	ver := s.engine.masterIndex.Version
	s.engine.indexLock.RUnlock()

	isConfigured := GlobalConfig.IsConfigured()
	isConnected := s.engine.imapClient != nil && s.engine.imapClient.connected && s.engine.imapClient.conn != nil
	isHttpConnected := GlobalWebmailClient != nil && GlobalWebmailClient.IsSessionValid()
	isHttpChecking := GlobalWebmailClient != nil && GlobalWebmailClient.IsChecking()

	resp := map[string]interface{}{
		"app_name":           AppName,
		"version":            AppVersion,
		"author":             AppAuthor,
		"build_date":         AppBuildDate,
		"is_configured":      isConfigured,
		"connected":          isConnected,
		"imap_connected":     isConnected,
		"http_connected":     isHttpConnected,
		"http_checking":      isHttpChecking,
		"email":              GlobalConfig.Email,
		"folder":             GlobalConfig.DisplayFolder(),
		"total_files":        fileCount,
		"total_size":         totalSize,
		"total_size_fmt":     formatBytes(totalSize),
		"index_ver":          ver,
		"cache_rev":          s.engine.GetFolderCacheRev(),
		"need_repair_prompt": false,
	}
	writeJSON(w, resp)
}

func (s *WebServer) handleAckCleanExit(w http.ResponseWriter, r *http.Request) {
	hasPromptedCrash = true
	SetCleanExitStatus(true)
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *WebServer) handleExit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, map[string]interface{}{"status": "ok", "message": "正在退出..."})
	go func() {
		time.Sleep(200 * time.Millisecond)
		if s.engine != nil {
			_ = s.engine.FlushMasterIndexSync()
			if s.engine.imapClient != nil {
				s.engine.imapClient.Close()
			}
		}
		SetCleanExitStatus(true)
		cleanTempDir()
		os.Exit(0)
	}()
}

func (s *WebServer) handleConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		configLock.RLock()
		cfg := GlobalConfig
		configLock.RUnlock()
		resp := map[string]interface{}{
			"email":                      cfg.Email,
			"user_nick":                  cfg.UserNick,
			"imap_server":                cfg.IMAPServer,
			"imap_auth_code":             cfg.IMAPAuthCode,
			"storage_folder":             ToDisplayFolder(cfg.StorageFolder),
			"canonical_storage_folder":   cfg.StorageFolder,
			"display_folder":             cfg.DisplayFolder(),
			"chunk_size_bytes":           cfg.ChunkSizeBytes,
			"chunk_size_mb":              cfg.ChunkSizeMB,
			"download_dir":               cfg.DownloadDir,
			"computer_id":                GetComputerID(),
			"max_concurrency":            cfg.MaxConcurrency,
			"chunk_download_concurrency": cfg.ChunkDownloadConcurrency,
			"http_download_concurrency":  cfg.HTTPDownloadConcurrency,
			"http_upload_concurrency":    cfg.HTTPUploadConcurrency,
			"request_delay_ms":           cfg.RequestDelayMS,
			"thumb_local_cache":          cfg.IsThumbLocalCacheEnabled(),
		}
		writeJSON(w, resp)
		return
	}

	if r.Method == http.MethodPost {
		var newCfg Config
		if err := json.NewDecoder(r.Body).Decode(&newCfg); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}

		configLock.RLock()
		oldCfg := GlobalConfig
		configLock.RUnlock()

		// 继承多电脑独立下载目录列表，避免单台电脑保存设置冲掉其他电脑记录
		newCfg.DownloadDirs = oldCfg.DownloadDirs

		// 严格保持已有存储空间文件夹不变：若前端未显式传递或传空，且未切换账号，则沿用已有文件夹；切换账号时绝不沿用旧空间！
		isEmailChanged := (oldCfg.Email != "" && newCfg.Email != "" && !strings.EqualFold(oldCfg.Email, newCfg.Email))
		if strings.TrimSpace(newCfg.StorageFolder) == "" {
			if !isEmailChanged && strings.TrimSpace(oldCfg.StorageFolder) != "" {
				newCfg.StorageFolder = oldCfg.StorageFolder
			} else {
				newCfg.StorageFolder = "QMD_MailDrive"
			}
		} else {
			newCfg.StorageFolder = ToCanonicalFolder(newCfg.StorageFolder)
		}
		if strings.TrimSpace(oldCfg.StorageFolder) != "" {
			oldCfg.StorageFolder = ToCanonicalFolder(oldCfg.StorageFolder)
		}

		if newCfg.ChunkSizeBytes <= 0 {
			if newCfg.ChunkSizeMB > 0 {
				newCfg.ChunkSizeBytes = int64(newCfg.ChunkSizeMB) * 1024 * 1024
			} else {
				newCfg.ChunkSizeBytes = 52428799
			}
		}
		newCfg.ChunkSizeMB = int(newCfg.ChunkSizeBytes / (1024 * 1024))
		if newCfg.MaxConcurrency <= 0 {
			newCfg.MaxConcurrency = 1
		} else if newCfg.MaxConcurrency > 3 {
			newCfg.MaxConcurrency = 3
		}
		if newCfg.ChunkDownloadConcurrency <= 0 {
			newCfg.ChunkDownloadConcurrency = 3
		} else if newCfg.ChunkDownloadConcurrency > 5 {
			newCfg.ChunkDownloadConcurrency = 5
		}
		if newCfg.HTTPDownloadConcurrency < 0 {
			newCfg.HTTPDownloadConcurrency = 3
		} else if newCfg.HTTPDownloadConcurrency > 5 {
			newCfg.HTTPDownloadConcurrency = 5
		}
		if newCfg.HTTPUploadConcurrency < 0 {
			newCfg.HTTPUploadConcurrency = 1
		} else if newCfg.HTTPUploadConcurrency > 3 {
			newCfg.HTTPUploadConcurrency = 3
		}
		if newCfg.RequestDelayMS < 0 {
			newCfg.RequestDelayMS = 250
		}
		if newCfg.IMAPServer == "" {
			newCfg.IMAPServer = "imap.qq.com:993"
		}
		newCfg.UserNick = SanitizeUserNick(newCfg.UserNick)

		credentialsChanged := isEmailChanged ||
			oldCfg.IMAPAuthCode != newCfg.IMAPAuthCode ||
			oldCfg.StorageFolder != newCfg.StorageFolder ||
			oldCfg.IMAPServer != newCfg.IMAPServer ||
			!s.engine.imapClient.connected

		if err := SaveConfig(newCfg); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if !newCfg.IsThumbLocalCacheEnabled() {
			s.engine.ClearMemThumbs()
		}

		// 若核心凭证未变（仅微调延时、并发数、分片大小等），立即秒级返回，无需重新握手与扫盘
		s.engine.LogInfo("SYSTEM", fmt.Sprintf("[系统设置] ✔ 配置已保存更新: IMAP下载并发 %d, HTTP下载并发 %d, HTTP上传并发 %d, 防风控延时 %dms, 空间目录 %s", newCfg.ChunkDownloadConcurrency, newCfg.HTTPDownloadConcurrency, newCfg.HTTPUploadConcurrency, newCfg.RequestDelayMS, newCfg.StorageFolder))
		if !credentialsChanged {
			// 同步存入云端「收到的文件」邮件中 (AES-256 加密，多端永久漫游)
			go func(cfgToSync Config) {
				if s.engine != nil {
					_ = s.engine.SaveCloudConfig(cfgToSync)
				}
			}(newCfg)

			writeJSON(w, map[string]interface{}{
				"status":        "ok",
				"connected":     true,
				"is_first_time": false,
				"folder":        ToDisplayFolder(newCfg.StorageFolder),
			})
			return
		}

		// 核心登录凭证变更时，彻底关闭旧账号所有连接池（物理掐断旧账号辅助下载长连接与主长连接）
		if isEmailChanged {
			GlobalWebmailClient.ClearSession()
			s.engine.LogInfo("WEBMAIL", "🔄 [账号已切换] 已安全重置内存会话，正在尝试恢复新账号云端 Webmail 授权...")
		}
		s.engine.CloseDownloadPool()
		s.engine.imapClient.Close()
		s.engine.imapClient.email = newCfg.Email
		s.engine.imapClient.authCode = newCfg.IMAPAuthCode
		s.engine.imapClient.server = newCfg.IMAPServer
		s.engine.UpdateCipherKey(newCfg.Email)

		connErr := s.engine.imapClient.Connect()
		if connErr != nil {
			writeJSON(w, map[string]interface{}{
				"status":  "error",
				"message": fmt.Sprintf("连接失败，请核对 QQ 邮箱与 16 位 IMAP 授权码: %v", connErr),
			})
			return
		}

		// 连入成功后：只有在真正切换了新账号邮箱 (isEmailChanged) 时，才从新账号云端拉取漫游配置；同账号时，以用户当前保存的配置为准并立即推送到云端！
		if isEmailChanged {
			if cloudCfg, err := s.engine.LoadCloudConfig(); err == nil && cloudCfg != nil {
				ApplyCloudConfig(cloudCfg)
				configLock.RLock()
				newCfg = GlobalConfig
				configLock.RUnlock()
				s.engine.LogInfo("SYSTEM", fmt.Sprintf("✔ [云端配置同步] 已成功从新账号拉取并漫游云端设置 (分片: %d 字节, 并发: %d, 防风控延时: %dms)",
					newCfg.ChunkSizeBytes, newCfg.ChunkDownloadConcurrency, newCfg.RequestDelayMS))
			} else {
				go func(c Config) {
					time.Sleep(500 * time.Millisecond)
					if s.engine != nil {
						_ = s.engine.SaveCloudConfig(c)
					}
				}(newCfg)
			}
		} else {
			// 同账号：将用户提交的新配置直接同步至云端邮件，绝不回退覆盖！
			go func(c Config) {
				time.Sleep(500 * time.Millisecond)
				if s.engine != nil {
					_ = s.engine.SaveCloudConfig(c)
				}
			}(newCfg)
		}

		canonicalFolder := ToCanonicalFolder(newCfg.StorageFolder)
		newCfg.StorageFolder = canonicalFolder
		if err := s.engine.imapClient.EnsureAndSelectFolder(canonicalFolder); err != nil {
			writeJSON(w, map[string]interface{}{
				"status":  "error",
				"message": fmt.Sprintf("在 QQ 邮箱创建隔离空间文件夹 [%s] 失败: %v", ToDisplayFolder(canonicalFolder), err),
			})
			return
		}

		isFirstTime, initErr := s.engine.InitOrLoadCloudIndex()
		if initErr != nil {
			writeJSON(w, map[string]interface{}{
				"status":  "error",
				"message": fmt.Sprintf("初始化网盘空间失败: %v", initErr),
			})
			return
		}

		// 自动从当前账号云端「收到的文件」中检索并静默恢复已保存的 sid（实现账号切换免扫码，跨设备永久漫游）
		go func() {
			time.Sleep(500 * time.Millisecond)
			if s.engine != nil && (!GlobalWebmailClient.IsSessionValid() || isEmailChanged) {
				if cloudSid, err := s.engine.LoadCloudSession(); err == nil && cloudSid != "" {
					GlobalWebmailClient.RestoreSession(cloudSid)
				}
			}
		}()

		writeJSON(w, map[string]interface{}{
			"status":        "ok",
			"connected":     true,
			"is_first_time": isFirstTime,
			"folder":        ToDisplayFolder(newCfg.StorageFolder),
		})
	}
}

// handleFiles 返回文件列表并执行稳定排序（文件夹在先，文件按修改时间倒序），防止前端乱跳
func (s *WebServer) handleFiles(w http.ResponseWriter, r *http.Request) {
	s.engine.indexLock.RLock()
	var list []FileNode
	for _, f := range s.engine.masterIndex.Files {
		list = append(list, f)
	}
	s.engine.indexLock.RUnlock()

	// 动态向上递归汇总计算每个文件夹及其所有深层祖先文件夹的真实总大小与文件总数
	dirSizes := make(map[string]int64)
	dirCounts := make(map[string]int)

	for _, f := range list {
		if !f.IsDir {
			parts := strings.Split(strings.Trim(f.VirtualPath, "/"), "/")
			cur := ""
			for i := 0; i < len(parts)-1; i++ {
				cur += "/" + parts[i]
				dirSizes[cur] += f.Size
				dirCounts[cur]++
			}
		}
	}
	for i := range list {
		if list[i].IsDir {
			list[i].Size = dirSizes[list[i].VirtualPath]
			list[i].DirectCount = dirCounts[list[i].VirtualPath]
		}
	}

	// 瘦身优化：/api/files 彻底移出几十兆的 Base64 字符串，改由 /api/thumb 视口懒加载
	for i := range list {
		if !list[i].IsDir && (list[i].IsMedia || isMediaExt(list[i].Name)) {
			list[i].IsMedia = true
		}
	}

	sort.Slice(list, func(i, j int) bool {
		if list[i].IsDir != list[j].IsDir {
			return list[i].IsDir // 文件夹排在前面
		}
		return list[i].ModTime.After(list[j].ModTime) // 默认新修改的文件排在上面
	})

	writeJSON(w, list)
}

// handleTasks 按照创建时间严格正序排序输出，彻底杜绝任务间随机横跳与日志抽搐
func (s *WebServer) handleTasks(w http.ResponseWriter, r *http.Request) {
	s.engine.tasksLock.RLock()
	defer s.engine.tasksLock.RUnlock()

	var list []*TransferTask
	for _, t := range s.engine.tasks {
		list = append(list, t)
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.Before(list[j].CreatedAt)
	})

	writeJSON(w, list)
}

func (s *WebServer) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}

	if !GlobalConfig.IsConfigured() {
		s.engine.LogError("UPLOAD", "❌ 上传请求拒绝: 尚未配置 QQ 邮箱账号或授权码")
		http.Error(w, "请先在设置中填写授权码连入 QQ 邮箱", 400)
		return
	}

	// 自动自愈保障：若主连接当前处于未就绪状态，立即尝试自动恢复重连并接入工作目录，彻底消除假死拦截
	if s.engine.imapClient == nil || !s.engine.imapClient.connected || s.engine.imapClient.conn == nil {
		s.engine.LogInfo("IMAP", "🔄 [连接池自愈] 检测到上传主链路当前未就绪，正在后台自动唤醒重连...")
		if cErr := s.engine.imapClient.Connect(); cErr != nil {
			s.engine.LogError("UPLOAD", fmt.Sprintf("❌ 上传启动失败，主连接唤醒失败: %v", cErr))
			http.Error(w, fmt.Sprintf("IMAP 链接恢复受阻，请检查授权码或网络: %v", cErr), 500)
			return
		}
		_ = s.engine.imapClient.EnsureAndSelectFolder(GlobalConfig.StorageFolder)
		s.engine.LogInfo("IMAP", fmt.Sprintf("✔ [连接池自愈] 上传主长连接已成功恢复接入: %s", GlobalConfig.StorageFolder))
	}

	localPath := r.FormValue("local_path")
	vPath := r.FormValue("virtual_path")
	thumbBase64 := r.FormValue("thumb_base64")

	// 方案 A：路径穿透直传（零磁盘临时副本，直接读取源文件）
	if localPath != "" {
		stat, err := os.Stat(localPath)
		if err != nil {
			http.Error(w, "本地原文件未找到: "+err.Error(), 400)
			return
		}
		if vPath == "" {
			vPath = "/" + stat.Name()
		}
		taskID := r.FormValue("task_id")
		if taskID == "" {
			taskID = fmt.Sprintf("task_%d", time.Now().UnixNano())
		}
		chunkSize := GlobalConfig.ChunkSizeBytes
		if chunkSize <= 0 {
			chunkSize = 52428799
		}
		rawChunkSize := RawChunkSizeFromTarget(chunkSize)
		totalChunks := int((stat.Size() + rawChunkSize - 1) / rawChunkSize)
		if totalChunks <= 0 {
			totalChunks = 1
		}
		task := &TransferTask{
			TaskID:      taskID,
			VirtualPath: vPath,
			FileName:    stat.Name(),
			TotalSize:   stat.Size(),
			Type:        "upload",
			Status:      "running",
			Stage:       StageScanning,
			TotalChunks: totalChunks,
			CreatedAt:   time.Now(),
		}
		s.engine.tasksLock.Lock()
		s.engine.tasks[taskID] = task
		s.engine.tasksLock.Unlock()

		go func() {
			_ = s.engine.UploadLargeFile(task, localPath, vPath, thumbBase64)
		}()
		writeJSON(w, map[string]string{"task_id": taskID, "status": "queued"})
		return
	}

	// 方案 B：网页表单纯内存管道直传 (零磁盘临时文件，彻底剔除 temp/ 目录，不产生任何临时前缀)
	file, header, err := r.FormFile("file")
	if vPath == "" && header != nil {
		vPath = "/" + header.Filename
	}

	if err == nil && header != nil {
		defer file.Close()
		taskID := r.FormValue("task_id")
		if taskID == "" {
			taskID = fmt.Sprintf("task_%d", time.Now().UnixNano())
		}
		chunkSize := GlobalConfig.ChunkSizeBytes
		if chunkSize <= 0 {
			chunkSize = 52428799
		}
		rawChunkSize := RawChunkSizeFromTarget(chunkSize)
		totalChunks := int((header.Size + rawChunkSize - 1) / rawChunkSize)
		if totalChunks <= 0 {
			totalChunks = 1
		}
		task := &TransferTask{
			TaskID:      taskID,
			VirtualPath: vPath,
			FileName:    header.Filename,
			TotalSize:   header.Size,
			Type:        "upload",
			Status:      "running",
			Stage:       StageScanning,
			TotalChunks: totalChunks,
			CreatedAt:   time.Now(),
		}

		s.engine.tasksLock.Lock()
		s.engine.tasks[taskID] = task
		s.engine.tasksLock.Unlock()

		// 直接纯内存管道切片直传，零磁盘临时文件写入，杜绝硬盘磨损与文件名污染
		s.engine.LogInfo("UPLOAD", fmt.Sprintf("🚀 [上传启动] 开始接收并切片直写数据流: %s (大小: %s, 路径: %s)", header.Filename, formatBytes(header.Size), vPath))
		if err := s.engine.UploadStream(task, file, header.Filename, header.Size, vPath, thumbBase64); err != nil {
			if task.Status == "cancelled" {
				writeJSON(w, map[string]string{"task_id": taskID, "status": "cancelled", "stage_detail": "上传已由用户主动取消"})
				return
			}
			s.engine.LogError("UPLOAD", fmt.Sprintf("❌ [上传失败] 文件上传失败 [%s]: %v", header.Filename, err))
			http.Error(w, "上传失败: "+err.Error(), 500)
			return
		}

		if task.Status == "cancelled" {
			writeJSON(w, map[string]string{"task_id": taskID, "status": "cancelled", "stage_detail": "上传已由用户主动取消"})
			return
		}

		s.engine.LogInfo("UPLOAD", fmt.Sprintf("✔ [上传入库] 文件已成功入库并同步主索引: %s", header.Filename))
		writeJSON(w, map[string]string{"task_id": taskID, "status": "completed", "stage_detail": task.StageDetail})
		return
	}

	s.engine.LogError("UPLOAD", fmt.Sprintf("❌ 上传表单解析失败: %v", err))
	http.Error(w, "未收到有效文件数据", 400)
}

// findFFmpeg 智能探测系统中可用的 ffmpeg 执行程序（同级目录优先，其次当前目录与环境变量）
func findFFmpeg() string {
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		p := filepath.Join(dir, "ffmpeg.exe")
		if _, err := os.Stat(p); err == nil {
			return p
		}
		pUnix := filepath.Join(dir, "ffmpeg")
		if _, err := os.Stat(pUnix); err == nil {
			return pUnix
		}
	}
	if _, err := os.Stat("ffmpeg.exe"); err == nil {
		return "ffmpeg.exe"
	}
	if _, err := os.Stat("ffmpeg"); err == nil {
		return "./ffmpeg"
	}
	if p, err := exec.LookPath("ffmpeg"); err == nil {
		return p
	}
	return ""
}

func (s *WebServer) handlePreview(w http.ResponseWriter, r *http.Request) {
	vPath := r.URL.Query().Get("path")
	s.engine.indexLock.RLock()
	node, exists := s.engine.masterIndex.Files[vPath]
	s.engine.indexLock.RUnlock()

	if !exists {
		http.Error(w, "文件或文件夹未找到", 404)
		return
	}

	// 文件夹打包 ZIP 下载支持
	if node.IsDir {
		zipName := node.Name + ".zip"
		tempZipPath := filepath.Join("temp", "pkg_"+node.ID+".zip")
		defer os.Remove(tempZipPath)

		if err := s.engine.DownloadFolderAsZip(node, tempZipPath); err != nil {
			http.Error(w, "文件夹打包下载失败: "+err.Error(), 500)
			return
		}
		if fi, err := os.Stat(tempZipPath); err == nil {
			w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", "attachment; filename=\"" + zipName + "\"")
		http.ServeFile(w, r, tempZipPath)
		return
	}

	// 确定落地目录：优先保存至用户配置的下载目录，若未设置则默认存放在 temp 目录下
	configLock.RLock()
	downloadDir := strings.TrimSpace(GlobalConfig.DownloadDir)
	configLock.RUnlock()

	// 优先检查用户下载目录或已有缓存中是否已有该文件的完整落盘版本
	targetFilePath := ""
	if downloadDir != "" {
		candidate := filepath.Join(downloadDir, node.Name)
		if _, doneErr := os.Stat(candidate + ".done"); doneErr == nil {
			targetFilePath = candidate
		}
	}
	if targetFilePath == "" {
		_ = os.MkdirAll("temp", 0755)
		candidate := filepath.Join("temp", fmt.Sprintf("%s_%s", node.ID, node.Name))
		if _, doneErr := os.Stat(candidate + ".done"); doneErr == nil {
			targetFilePath = candidate
		} else if _, doneLegacyErr := os.Stat(filepath.Join("temp", node.Name) + ".done"); doneLegacyErr == nil {
			targetFilePath = filepath.Join("temp", node.Name)
		} else {
			targetFilePath = candidate
		}
	}

	partFilePath := targetFilePath + ".part"

	targetDone := targetFilePath + ".done"
	partDone := partFilePath + ".done"

	_, doneTargetErr := os.Stat(targetDone)
	_, donePartErr := os.Stat(partDone)
	fiTarget, statTargetErr := os.Stat(targetFilePath)
	fiPart, statPartErr := os.Stat(partFilePath)

	transcodeParam := r.URL.Query().Get("transcode")

	// 动态双轨文件解析：优先已完成的正式文件，平滑兼容下载中的 .part 临时文件
	getValidMediaFilePath := func() string {
		if fi, err := os.Stat(targetFilePath); err == nil && fi.Size() > 0 {
			return targetFilePath
		}
		if fi, err := os.Stat(partFilePath); err == nil && fi.Size() > 0 {
			return partFilePath
		}
		return targetFilePath
	}

	// 极其关键：因为 .part 文件在启动时就执行了 Truncate(node.Size) 磁盘空间预分配，
	// 所以 fiPart.Size() 从第 0 秒起就等于 node.Size！绝不能用 Size == node.Size 判定完成，
	// 必须严格且唯一检验 .done 完成印记文件，否则会导致后续所有跳转误以为文件已 100% 下载完毕而跳过插队！
	isComplete := false
	if statTargetErr == nil && doneTargetErr == nil && (node.Size == 0 || fiTarget.Size() == node.Size) {
		isComplete = true
	} else if statPartErr == nil && donePartErr == nil && (node.Size == 0 || fiPart.Size() == node.Size) {
		isComplete = true
	}

	taskID := "task_prev_" + node.ID
	chunkSize := GlobalConfig.ChunkSizeBytes
	if chunkSize <= 0 {
		chunkSize = 52428799
	}
	rawChunkSize := RawChunkSizeFromTarget(chunkSize)
	totChunks := int((node.Size + rawChunkSize - 1) / rawChunkSize)
	if totChunks <= 0 {
		totChunks = 1
	}
	allCompleted := make([]int, totChunks)
	for i := 0; i < totChunks; i++ {
		allCompleted[i] = i + 1
	}

	if isComplete {
		// 文件本地已预载/下载完毕，立即向任务列表挂载或对齐已完成状态，界面展示“✔ 已下载完毕”与 100% 进度
		s.engine.tasksLock.Lock()
		task, taskExists := s.engine.tasks[taskID]
		if !taskExists {
			task = &TransferTask{
				TaskID:      taskID,
				VirtualPath: vPath,
				FileName:    node.Name,
				TotalSize:   node.Size,
				Type:        "preview",
				CreatedAt:   time.Now(),
			}
			s.engine.tasks[taskID] = task
		}
		task.Status = "completed"
		task.Stage = StageCompleted
		task.StageDetail = "✔ 本地已完全预载/已下载完毕，可直接离线秒播"
		task.Transferred = node.Size
		task.ProgressPct = 100.0
		task.SpeedMBs = 0
		task.TotalChunks = totChunks
		task.CompletedChunks = allCompleted
		task.ActiveChunks = nil
		task.WritingChunks = nil
		s.engine.tasksLock.Unlock()
	} else {
		// 动态定位已有传输任务 (无论是前台用户下载还是后台预览，统合为同一流水线，杜绝文件句柄冲突)
		s.engine.tasksLock.RLock()
		for _, t := range s.engine.tasks {
			if (t.Type == "preview" || t.Type == "download") && (t.VirtualPath == vPath || t.FileName == node.Name) && (t.Status == "running" || t.Status == "pending") {
				taskID = t.TaskID
				break
			}
		}
		s.engine.tasksLock.RUnlock()

		s.engine.tasksLock.Lock()
		task, taskExists := s.engine.tasks[taskID]
		needStart := !taskExists || task.Status == "cancelled" || task.Status == "failed"
		if needStart {
			task = &TransferTask{
				TaskID:      taskID,
				VirtualPath: vPath,
				FileName:    node.Name,
				TotalSize:   node.Size,
				Type:        "preview",
				Status:      "running",
				Stage:       StageDownloading,
				StageDetail: "正在拉取流式切片缓冲...",
				CreatedAt:   time.Now(),
			}
			s.engine.tasks[taskID] = task
		}
		s.engine.tasksLock.Unlock()

		if needStart {
			go func() {
				_ = s.engine.DownloadStream(task, node, targetFilePath)
			}()
		}

		// 检查用户是否拖动了播放控制条 (Range Seek 动态插队调度)
		// 注意：开启 FFmpeg 音频转码 (transcodeParam == "1") 时，客户端寻道完全由下方的 ?t= 参数接管；
		// 严禁在此处被浏览器的底层元数据探测 (如读取尾部 cues/moov 的 Range 请求) 误触发游标跳跃！
		rangeHdr := r.Header.Get("Range")
		chunkSize := GlobalConfig.ChunkSizeBytes
		if chunkSize <= 0 {
			chunkSize = 52428799
		}
		rawChunkSize := RawChunkSizeFromTarget(chunkSize)

		targetChunk := 0
		if transcodeParam != "1" && strings.HasPrefix(rangeHdr, "bytes=") {
			byteSpec := strings.TrimPrefix(rangeHdr, "bytes=")
			parts := strings.Split(byteSpec, "-")
			if len(parts) > 0 && parts[0] != "" {
				if startByte, sErr := strconv.ParseInt(parts[0], 10, 64); sErr == nil && startByte > 0 {
					targetChunk = int(startByte/rawChunkSize) + 1
				}
			}
		}

		if transcodeParam != "1" && targetChunk > 0 {
			s.engine.tasksLock.RLock()
			completedList := append([]int(nil), task.CompletedChunks...)
			totalC := task.TotalChunks
			cursor := task.PlayCursor
			s.engine.tasksLock.RUnlock()

			if totalC <= 0 && node.Size > 0 && rawChunkSize > 0 {
				totalC = int((node.Size + rawChunkSize - 1) / rawChunkSize)
			}
			lastChunkEstSize := node.Size - int64(totalC-1)*rawChunkSize

			// 改进 1：智能识别底层播放器尾部元数据探测 (Cues/moov)
			// 当目标分片为尾部分片 (Chunk N，或末片<15MB时的倒数第二片 Chunk N-1)，且系统处于起播/初期缓冲阶段时 (首分片或第二分片未完，或播放游标尚未后移)，
			// 属于浏览器底层容器解包探测，绝非用户主动拖拽进度条！
			// 此时严禁调用 PrioritizeTaskChunk，严禁篡改 PlayCursor，严禁触发急救熔断与错峰让位掐断正在高速拉取的第 2 分片！
			isTailMetaProbe := false
			if totalC >= 2 && (targetChunk == totalC || (totalC >= 3 && targetChunk == totalC-1 && lastChunkEstSize < TailMetadataMinChunkSize)) {
				hasC1Done := false
				hasC2Done := false
				for _, c := range completedList {
					if c == 1 {
						hasC1Done = true
					}
					if c == 2 {
						hasC2Done = true
					}
				}
				if !hasC1Done || !hasC2Done || cursor <= 2 {
					isTailMetaProbe = true
				}
			}

			if isTailMetaProbe {
				s.engine.LogDebug("STREAM", fmt.Sprintf("🎬 [元数据探测] 播放器底层读取尾部分片 #%d 关键元数据 (Cues/moov)，免打断主链路持续拉取", targetChunk))
			} else {
				// 真实用户拖拽寻道：立即锁定播放游标并掐断旧分片！
				s.engine.PrioritizeTaskChunk(taskID, targetChunk)
			}

			isTargetReady := false
			for _, c := range completedList {
				if c == targetChunk {
					isTargetReady = true
					break
				}
			}

			if !isTargetReady {
				// 平滑等待插队分片落盘，客户端断开连接时优雅退出
				waitTimeout := time.After(60 * time.Second)
				checkTicker := time.NewTicker(100 * time.Millisecond)
				defer checkTicker.Stop()

			waitSeekLoop:
				for {
					select {
					case <-r.Context().Done():
						return
					case <-waitTimeout:
						break waitSeekLoop
					case <-checkTicker.C:
						s.engine.tasksLock.RLock()
						cList := task.CompletedChunks
						tStatus := task.Status
						s.engine.tasksLock.RUnlock()

						if tStatus == "failed" || tStatus == "cancelled" {
							break waitSeekLoop
						}
						for _, c := range cList {
							if c == targetChunk {
								break waitSeekLoop
							}
						}
					}
				}
			}
		}

		// 边下边播就绪守卫：严格等待首个切片与末尾切片真正落盘
		isImage := false
		ext := strings.ToLower(filepath.Ext(node.Name))
		if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".webp" || ext == ".gif" || ext == ".bmp" {
			isImage = true
		}

		timeout := time.After(60 * time.Second)
		ticker := time.NewTicker(150 * time.Millisecond)
		defer ticker.Stop()

	waitLoop:
		for {
			select {
			case <-r.Context().Done():
				return
			case <-timeout:
				break waitLoop
			case <-ticker.C:
				s.engine.tasksLock.RLock()
				curStatus := task.Status
				completedChunks := append([]int(nil), task.CompletedChunks...)
				s.engine.tasksLock.RUnlock()

				if curStatus == "failed" || curStatus == "cancelled" {
					break waitLoop
				}

				if isImage {
					if curStatus == "completed" || len(completedChunks) > 0 {
						break waitLoop
					}
				} else {
					hasChunk1 := false
					totalC := task.TotalChunks
					lastChunkEstSize := node.Size - int64(totalC-1)*rawChunkSize
					hasChunkLast := (totalC <= 1)
					hasChunkPenultimate := (totalC < 3 || lastChunkEstSize >= TailMetadataMinChunkSize)

					for _, c := range completedChunks {
						if c == 1 {
							hasChunk1 = true
						}
						if totalC > 1 && c == totalC {
							hasChunkLast = true
						}
						if totalC >= 3 && lastChunkEstSize < TailMetadataMinChunkSize && c == totalC-1 {
							hasChunkPenultimate = true
						}
					}
					if (hasChunk1 && hasChunkLast && hasChunkPenultimate) || curStatus == "completed" {
						break waitLoop
					}
				}
			}
		}

		// 严格安全守护：若首分片或末分片仍未落盘，绝不下发 0 字节脏数据击溃浏览器！
		if !isImage && !isComplete {
			s.engine.tasksLock.RLock()
			cChunks := append([]int(nil), task.CompletedChunks...)
			tStatus := task.Status
			s.engine.tasksLock.RUnlock()

			hasC1 := false
			for _, c := range cChunks {
				if c == 1 {
					hasC1 = true
					break
				}
			}
			// 仅当首切片未就绪且任务异常中断时报错；流式缓冲中绝不下发 503 中断浏览器媒体管道
			if !hasC1 && tStatus != "completed" {
				if tStatus == "failed" || tStatus == "cancelled" {
					http.Error(w, "分片缓冲异常中断: "+task.Error, 500)
					return
				}
			}
		}
	}

	// 检查是否开启了 FFmpeg 实时音频转码兼容模式 (针对浏览器无法原生解码杜比 E-AC-3/AC-3/DTS 的 MKV 视频)
	ffmpegBin := findFFmpeg()
	if transcodeParam == "1" && ffmpegBin != "" {
		startTimeSec := "0"
		if tSec := r.URL.Query().Get("t"); tSec != "" {
			startTimeSec = tSec
		}

		// 关键联动：当转码请求指定了时间戳 t (如 t=8706)，必须精准换算对应分片并触发云端紧急插队落盘！
		if !isComplete {
			startSecF, _ := strconv.ParseFloat(startTimeSec, 64)
			durSecF, _ := strconv.ParseFloat(r.URL.Query().Get("dur"), 64)
			if durSecF <= 0 {
				durSecF = 7200 // 兜底 2 小时
			}

			chunkSize := GlobalConfig.ChunkSizeBytes
			if chunkSize <= 0 {
				chunkSize = 52428799
			}
			rawChunkSize := RawChunkSizeFromTarget(chunkSize)

			targetChunk := 1
			if startSecF > 0 && durSecF > 0 && node.Size > 0 {
				ratio := startSecF / durSecF
				if ratio > 1.0 {
					ratio = 1.0
				}
				targetByte := int64(ratio * float64(node.Size))
				targetChunk = int(targetByte/rawChunkSize) + 1
				if targetChunk < 1 {
					targetChunk = 1
				}
			}

			taskID := "task_prev_" + node.ID
			s.engine.tasksLock.RLock()
			for _, t := range s.engine.tasks {
				if (t.Type == "preview" || t.Type == "download") && (t.VirtualPath == vPath || t.FileName == node.Name) && (t.Status == "running" || t.Status == "pending" || t.Status == "completed") {
					taskID = t.TaskID
					break
				}
			}
			task, taskExists := s.engine.tasks[taskID]
			var cList []int
			totalChks := 0
			if taskExists && task != nil {
				cList = append([]int(nil), task.CompletedChunks...)
				totalChks = task.TotalChunks
				if task.TotalChunks > 0 && targetChunk > task.TotalChunks {
					targetChunk = task.TotalChunks
				}
			}
			s.engine.tasksLock.RUnlock()

			// 核心原则：无论目标分片当前是否已在本地落盘，只要用户发起了寻道/起播请求，
			// 都必须【无条件立即更新播放游标】并触发寻道打断，
			// 确保后台下载调度器把工作重心牢牢锁定在用户当前观影点 targetChunk 及后续分片上，
			// 绝不允许下载游标停留在旧时间点、源源不断偷跑下载几十个历史无用切片导致播放卡死！
			s.engine.PrioritizeTaskChunk(taskID, targetChunk)

			hasTargetInit := false
			hasNextInit := false
			for _, c := range cList {
				if c == targetChunk {
					hasTargetInit = true
				}
				if c == targetChunk+1 {
					hasNextInit = true
				}
			}
			// 严格防跳尾缓冲判定：目标分片与其紧随的下一分片必须同时落盘 (或已是全片末尾分片)，
			// 才能视为真正就绪；若只下完了 targetChunk 而 targetChunk+1 还是 0x00 空洞，
			// 必须进入等待队列等待 targetChunk+1 (通常仅需 5~8 秒)，绝不能让 FFmpeg 撞见空洞跳至片尾！
			targetReady := hasTargetInit && (hasNextInit || (totalChks > 0 && targetChunk >= totalChks))

			if !targetReady {
				s.engine.LogInfo("STREAM", fmt.Sprintf("🎬 [流式寻道] 播放器跳转至 %ss (对应分片 #%d)，防跳尾连续安全缓冲筹备中，调度器全力拉取起播切片...", startTimeSec, targetChunk))

				waitTimeout := time.After(90 * time.Second)
				checkTicker := time.NewTicker(100 * time.Millisecond)
				defer checkTicker.Stop()
				var targetFinishedAt time.Time

			waitTranscodeSeek:
				for {
					select {
					case <-r.Context().Done():
						return
					case <-waitTimeout:
						// 超时前做最终原子核验：如果目标分片已在磁盘上，放行启动起播
						s.engine.tasksLock.RLock()
						if t, ok := s.engine.tasks[taskID]; ok && t != nil {
							for _, c := range t.CompletedChunks {
								if c == targetChunk {
									targetReady = true
									break
								}
							}
						}
						s.engine.tasksLock.RUnlock()
						break waitTranscodeSeek
					case <-checkTicker.C:
						s.engine.tasksLock.RLock()
						var curCList []int
						tStatus := ""
						curTotal := 0
						if t, ok := s.engine.tasks[taskID]; ok && t != nil {
							curCList = append([]int(nil), t.CompletedChunks...)
							tStatus = t.Status
							curTotal = t.TotalChunks
						}
						s.engine.tasksLock.RUnlock()
						if tStatus == "failed" || tStatus == "cancelled" {
							break waitTranscodeSeek
						}
						hasTargetNow := false
						hasNextNow := false
						for _, c := range curCList {
							if c == targetChunk {
								hasTargetNow = true
							}
							if c == targetChunk+1 {
								hasNextNow = true
							}
						}
						// 核心防跳尾原则：
						// 1. 目标起播分片 targetChunk 必须落盘
						// 2. 紧随其后的下一分片 targetChunk + 1 也必须落盘 (或已是全片末尾分片)，
						//    确保 FFmpeg 拥有至少连续两个分片 (100MB+) 的充足连续缓冲，
						//    绝对严禁在读完单片后撞见 0x00 空洞直接导致 FFmpeg EBML 解复用器跳跃至尾部！
						// 3. 兜底保护：若目标分片就绪后超过 15 秒下一分片仍未就绪，允许放行
						if hasTargetNow {
							if targetFinishedAt.IsZero() {
								targetFinishedAt = time.Now()
							}
							if hasNextNow || (curTotal > 0 && targetChunk >= curTotal) || time.Since(targetFinishedAt) > 15*time.Second {
								targetReady = true
								break waitTranscodeSeek
							}
						}
					}
				}

				// 极其关键：若目标分片仍未就绪，绝对严禁让 FFmpeg 启动去读空洞！
				if !targetReady {
					s.engine.tasksLock.RLock()
					tStatus := ""
					if t, ok := s.engine.tasks[taskID]; ok && t != nil {
						tStatus = t.Status
					}
					s.engine.tasksLock.RUnlock()

					if tStatus == "cancelled" {
						s.engine.LogInfo("STREAM", fmt.Sprintf("✔ [寻道取消] 预览浮窗已关闭，退出分片 #%d 转码等待", targetChunk))
						return
					}
					s.engine.LogWarn("STREAM", fmt.Sprintf("⚠️ [寻道缓冲超时] 分片 #%d 尚未完成落盘，挂起转码以防 FFmpeg 读空跳转至尾部！", targetChunk))
					return
				}
			} else {
				s.engine.LogInfo("STREAM", fmt.Sprintf("🎬 [流式寻道] 播放器跳转至 %ss (对应分片 #%d)，双分片连续安全缓冲已就绪，立即起播并持续向后预拉取", startTimeSec, targetChunk))
			}
		}

		w.Header().Set("Content-Type", "video/mp4")
		w.Header().Set("Cache-Control", "no-cache")

		realInputPath := getValidMediaFilePath()
		args := []string{
			"-ss", startTimeSec,
			"-re",
			"-i", realInputPath,
			"-c:v", "copy",
			"-c:a", "aac",
			"-b:a", "192k",
			"-ac", "2",
			"-movflags", "frag_keyframe+empty_moov+default_base_moof",
			"-f", "mp4",
			"pipe:1",
		}

		cmd := exec.Command(ffmpegBin, args...)
		cmd.Stdout = w
		var errBuf bytes.Buffer
		cmd.Stderr = &errBuf

	s.engine.LogInfo("PREVIEW", fmt.Sprintf("🎧 [FFmpeg 实时音频转码] 启动音频兼容管道 (转码 AAC): %s (从 %ss 起播)", node.Name, startTimeSec))
		if runErr := cmd.Run(); runErr != nil {
			if r.Context().Err() != nil {
				s.engine.LogInfo("PREVIEW", "✔ [播放通道交接] 客户端寻道跳转或关闭浮窗，旧转码管道平滑断开")
				return
			}
			errStr := strings.TrimSpace(errBuf.String())
			if len(errStr) > 300 {
				errStr = errStr[len(errStr)-300:]
			}
			s.engine.LogWarn("PREVIEW", fmt.Sprintf("⚠️ [FFmpeg 转码管道退出] 状态: %v, 日志: %s", runErr, errStr))
		}
		return
	}

	actualServePath := getValidMediaFilePath()
	file, err := os.Open(actualServePath)
	if err != nil {
		altPath := targetFilePath
		if actualServePath == targetFilePath {
			altPath = partFilePath
		}
		if file, err = os.Open(altPath); err != nil {
			http.Error(w, "无法读取预览文件流: "+err.Error(), 500)
			return
		}
	}
	defer file.Close()

	fi, err := file.Stat()
	if err != nil {
		http.Error(w, "无法获取预览文件状态: "+err.Error(), 500)
		return
	}

	// 智能识别并注入真实标准 MIME 类型 (优先确保常见音视频格式规范，彻底绕过 Windows 注册表篡改)
	ext := strings.ToLower(filepath.Ext(node.Name))
	var contentType string
	switch ext {
	case ".mp4":
		contentType = "video/mp4"
	case ".webm":
		contentType = "video/webm"
	case ".mov":
		contentType = "video/quicktime"
	case ".mkv":
		contentType = "video/webm" // WebM 与 Matroska (MKV) 格式同构，激发现代浏览器 Matroska/EBML 原生解析器，保障 Cues 簇时间跳转
	case ".jpg", ".jpeg":
		contentType = "image/jpeg"
	case ".png":
		contentType = "image/png"
	case ".webp":
		contentType = "image/webp"
	case ".gif":
		contentType = "image/gif"
	case ".mp3":
		contentType = "audio/mpeg"
	default:
		contentType = mime.TypeByExtension(ext)
		if contentType == "" {
			contentType = "application/octet-stream"
		}
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Accept-Ranges", "bytes")

	// 使用 http.ServeContent 原生支持 HTTP 206 边下边播与视频进度条任意拖拽 (Seek)
	http.ServeContent(w, r, node.Name, fi.ModTime(), file)
}

func (s *WebServer) handleMkdir(w http.ResponseWriter, r *http.Request) {
	vPath := r.FormValue("path")
	if vPath == "" {
		http.Error(w, "Path cannot be empty", 400)
		return
	}
	targetFolder := r.FormValue("target_folder")
	configLock.RLock()
	currentFolder := GlobalConfig.StorageFolder
	configLock.RUnlock()

	vPath = strings.ReplaceAll(vPath, "\\", "/")
	vPath = "/" + strings.Trim(vPath, "/")
	parts := strings.Split(vPath, "/")
	name := parts[len(parts)-1]

	// 若指定了目标隔离空间且不等于当前空间，向目标隔离空间主索引写入
	if targetFolder != "" && targetFolder != ToDisplayFolder(currentFolder) && ToCanonicalFolder(targetFolder) != currentFolder {
		targetCanonical := ToCanonicalFolder(targetFolder)
		cli := NewIMAPClient(GlobalConfig.IMAPServer, GlobalConfig.Email, GlobalConfig.IMAPAuthCode)
		if err := cli.Connect(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer cli.Close()

		if err := cli.EnsureAndSelectFolder(targetCanonical); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		headers, _ := cli.FetchAllHeadersFast()
		var bestIdx uint32
		var bestVer int64 = 0
		for _, th := range headers {
			if strings.HasPrefix(th.Subject, "[QMD:INDEX]|") {
				p := strings.Split(th.Subject, "|")
				if len(p) >= 2 && strings.HasPrefix(p[1], "v") {
					var v int64
					fmt.Sscanf(p[1][1:], "%d", &v)
					if v > bestVer {
						bestVer = v
						bestIdx = th.SeqNum
					}
				}
			}
		}

		var targetIdx MasterIndex
		if bestIdx > 0 {
			if raw, rErr := cli.FetchRawMessageBody(bestIdx); rErr == nil {
				enc := extractAttachmentFromMIME(raw)
				if plain, dErr := s.engine.DecryptData(enc); dErr == nil {
					_ = json.Unmarshal(plain, &targetIdx)
				}
			}
		}
		if targetIdx.Files == nil {
			targetIdx.Files = make(map[string]FileNode)
		}
		targetIdx.Files[vPath] = FileNode{
			ID:          fmt.Sprintf("dir_%d", time.Now().UnixNano()),
			Name:        name,
			VirtualPath: vPath,
			IsDir:       true,
			StorageType: StorageTypeDir,
			ModTime:     time.Now(),
		}
		targetIdx.Version = bestVer + 1
		targetIdx.LastUpdated = time.Now()

		data, _ := json.Marshal(targetIdx)
		encData, _ := s.engine.EncryptData(data)
		subj := fmt.Sprintf("[QMD:INDEX]|v%06d|%d", targetIdx.Version, time.Now().Unix())
		meta := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"ver":%d}`, targetIdx.Version)))
		mime := s.engine.BuildMIMEMessage(subj, meta, "master_index.dat", encData)
		if err := cli.AppendDirectMessage(targetCanonical, subj, meta, mime); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}

	s.engine.indexLock.Lock()
	s.engine.masterIndex.Files[vPath] = FileNode{
		ID:          fmt.Sprintf("dir_%d", time.Now().UnixNano()),
		Name:        name,
		VirtualPath: vPath,
		IsDir:       true,
		StorageType: StorageTypeDir,
		ModTime:     time.Now(),
	}
	s.engine.masterIndex.Version++
	s.engine.indexLock.Unlock()

	s.engine.Log(fmt.Sprintf("[新建文件夹] 创建空文件夹: %s", vPath))
	if err := s.engine.SyncMasterIndexToCloud(); err != nil {
		s.engine.Log(fmt.Sprintf("❌ 新建文件夹后主索引同步云端失败: %v", err))
	}
	writeJSON(w, map[string]string{"status": "ok"})
}


func (s *WebServer) handleCopy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		Paths        []string `json:"paths"`
		TargetDir    string   `json:"target_dir"`
		TargetFolder string   `json:"target_folder,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	req.TargetDir = strings.ReplaceAll(req.TargetDir, "\\", "/")
	for i := range req.Paths {
		req.Paths[i] = strings.ReplaceAll(req.Paths[i], "\\", "/")
	}

	configLock.RLock()
	currentFolderCanonical := GlobalConfig.StorageFolder
	currentFolderDisplay := ToDisplayFolder(currentFolderCanonical)
	configLock.RUnlock()

	// 1. 跨隔离空间物理复制 (target_folder 存在且不等于当前空间)
	if strings.TrimSpace(req.TargetFolder) != "" &&
		req.TargetFolder != currentFolderDisplay &&
		ToCanonicalFolder(req.TargetFolder) != currentFolderCanonical {

		for _, p := range req.Paths {
			if err := s.engine.CopyFileToFolder(p, req.TargetFolder, req.TargetDir); err != nil {
				s.engine.LogError("SYSTEM", fmt.Sprintf("❌ [跨空间复制失败] %s -> [%s]: %v", p, req.TargetFolder, err))
				writeJSON(w, map[string]interface{}{"status": "error", "message": err.Error()})
				return
			}
		}
		s.engine.LogInfo("SYSTEM", fmt.Sprintf("📋 [跨空间复制成功] 成功将 %d 个项目克隆至隔离空间: [%s] (目标路径: %s)", len(req.Paths), req.TargetFolder, req.TargetDir))
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}

	// 2. 本空间内部快速复制 (CoW 零网络流量)
	if err := s.engine.CopyItems(req.Paths, req.TargetDir); err != nil {
		s.engine.LogError("SYSTEM", fmt.Sprintf("❌ [复制失败] 复制项目至 [%s] 失败: %v", req.TargetDir, err))
		http.Error(w, err.Error(), 500)
		return
	}
	s.engine.LogInfo("SYSTEM", fmt.Sprintf("📋 [复制完成] 成功复制 %d 个项目至: %s", len(req.Paths), req.TargetDir))
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *WebServer) handleMove(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}
	var req struct {
		Paths        []string `json:"paths"`
		TargetDir    string   `json:"target_dir"`
		TargetFolder string   `json:"target_folder,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	req.TargetDir = strings.ReplaceAll(req.TargetDir, "\\", "/")
	for i := range req.Paths {
		req.Paths[i] = strings.ReplaceAll(req.Paths[i], "\\", "/")
	}

	configLock.RLock()
	currentFolderCanonical := GlobalConfig.StorageFolder
	currentFolderDisplay := ToDisplayFolder(currentFolderCanonical)
	configLock.RUnlock()

	// 1. 若指定了跨隔离空间物理迁移 (target_folder 存在且不等于当前空间)
	if strings.TrimSpace(req.TargetFolder) != "" &&
		req.TargetFolder != currentFolderDisplay &&
		ToCanonicalFolder(req.TargetFolder) != currentFolderCanonical {

		for _, p := range req.Paths {
			if err := s.engine.MoveFileToFolder(p, req.TargetFolder, req.TargetDir); err != nil {
				s.engine.LogError("SYSTEM", fmt.Sprintf("❌ [跨空间移动失败] %s -> [%s]: %v", p, req.TargetFolder, err))
				writeJSON(w, map[string]interface{}{"status": "error", "message": err.Error()})
				return
			}
		}
		s.engine.LogInfo("SYSTEM", fmt.Sprintf("📦 [跨空间迁移成功] 成功将 %d 个项目迁移至隔离空间: [%s]", len(req.Paths), req.TargetFolder))
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}

	// 2. 本空间内部普通移动
	if err := s.engine.MoveItems(req.Paths, req.TargetDir); err != nil {
		s.engine.LogError("SYSTEM", fmt.Sprintf("❌ [移动失败] 移动项目至 [%s] 失败: %v", req.TargetDir, err))
		http.Error(w, err.Error(), 500)
		return
	}
	s.engine.LogInfo("SYSTEM", fmt.Sprintf("📦 [移动完成] 成功移动 %d 个项目至: %s", len(req.Paths), req.TargetDir))
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *WebServer) handleRename(w http.ResponseWriter, r *http.Request) {
	oldPath := strings.ReplaceAll(r.FormValue("old_path"), "\\", "/")
	newPath := strings.ReplaceAll(r.FormValue("new_path"), "\\", "/")

	if err := s.engine.RenameFileNode(oldPath, newPath); err != nil {
		s.engine.LogError("SYSTEM", fmt.Sprintf("❌ [重命名失败] %s -> %s: %v", oldPath, newPath, err))
		http.Error(w, err.Error(), 500)
		return
	}
	s.engine.LogInfo("SYSTEM", fmt.Sprintf("✏️ [重命名] 成功更名: %s -> %s", oldPath, newPath))
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *WebServer) handleDelete(w http.ResponseWriter, r *http.Request) {
	path := r.FormValue("path")

	if err := s.engine.DeleteFileFromStorage(path); err != nil {
		s.engine.LogError("SYSTEM", fmt.Sprintf("❌ [删除失败] 物理删除失败 [%s]: %v", path, err))
		http.Error(w, err.Error(), 500)
		return
	}
	s.engine.LogInfo("SYSTEM", fmt.Sprintf("🗑️ [删除完成] 成功物理删除云端文件及关联切片: %s", path))
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *WebServer) handleBatchDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", 405)
		return
	}

	var req struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	for i := range req.Paths {
		req.Paths[i] = strings.ReplaceAll(req.Paths[i], "\\", "/")
	}

	if err := s.engine.DeleteMultipleItems(req.Paths); err != nil {
		s.engine.LogError("SYSTEM", fmt.Sprintf("❌ [批量删除失败] %d 个项目删除受阻: %v", len(req.Paths), err))
		http.Error(w, err.Error(), 500)
		return
	}
	s.engine.LogInfo("SYSTEM", fmt.Sprintf("🗑️ [批量删除] 成功物理删除 %d 个项目并同步主索引", len(req.Paths)))
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *WebServer) handleCompact(w http.ResponseWriter, r *http.Request) {
	if !s.engine.imapClient.connected {
		http.Error(w, "邮箱未连入", 400)
		return
	}
	savedMails, newPacks, err := s.engine.RunDeepCompaction()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, map[string]interface{}{
		"status":      "ok",
		"saved_mails": savedMails,
		"new_packs":   newPacks,
	})
}

func (s *WebServer) handleHealthCheck(w http.ResponseWriter, r *http.Request) {
	if !s.engine.imapClient.connected {
		writeJSON(w, map[string]interface{}{
			"error": "邮箱尚未连入，请先在设置中填写授权码完成初始化",
		})
		return
	}
	report, err := s.checker.RunHealthCheck()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, report)
}

func (s *WebServer) handleHealthRepair(w http.ResponseWriter, r *http.Request) {
	s.engine.Log("[对账重构] 用户触发：开始执行云端数据对齐与主索引重构...")
	rep, err := s.checker.RunHealthCheck()
	if err != nil {
		s.engine.Log("❌ [对账重构] 云端对账扫描失败: " + err.Error())
		writeJSON(w, map[string]interface{}{"status": "error", "message": err.Error()})
		return
	}

	repairedCount := len(rep.OrphanFiles)
	ghostCount := len(rep.GhostEntries)
	orphanChunksCount := len(rep.OrphanChunkSeqs)

	if repairedCount > 0 {
		s.engine.Log(fmt.Sprintf("[对账重构] 正在补录入库 %d 个未入库完好文件...", repairedCount))
		if rErr := s.checker.RepairOrphans(rep.OrphanFiles); rErr != nil {
			s.engine.Log("⚠️ [对账重构] 补录过程提示: " + rErr.Error())
		}
	}

	if ghostCount > 0 {
		s.engine.Log(fmt.Sprintf("[对账重构] 正在剔除 %d 个断链死条目...", ghostCount))
		if gErr := s.checker.RepairPurgeGhosts(rep.GhostEntries); gErr != nil {
			s.engine.Log("⚠️ [对账重构] 剔除过程提示: " + gErr.Error())
		}
	}

	if orphanChunksCount > 0 {
		s.engine.Log(fmt.Sprintf("[对账重构] 正在物理彻底抹除 %d 封无主残留切片邮件...", orphanChunksCount))
		if pErr := s.checker.RepairPurgeOrphanChunks(rep.OrphanChunkSeqs); pErr != nil {
			s.engine.Log("⚠️ [对账重构] 清理切片提示: " + pErr.Error())
		}
	}

	s.engine.Log(fmt.Sprintf("✔ [对账重构] 重构完毕: 补录 %d 项，清理 %d 死条目，抹除 %d 封残留切片邮件！", repairedCount, ghostCount, orphanChunksCount))
	writeJSON(w, map[string]interface{}{
		"status":                "ok",
		"task_id":               "health_repair",
		"repaired_count":        repairedCount,
		"ghost_count":           ghostCount,
		"orphan_chunks_purged": orphanChunksCount,
		"scanned_mails":         rep.TotalMailsScanned,
	})
}

func writeJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(data)
}

func (s *WebServer) handleLogs(w http.ResponseWriter, r *http.Request) {
	level := LogLevel(strings.ToUpper(r.URL.Query().Get("level")))
	module := r.URL.Query().Get("module")
	limitStr := r.URL.Query().Get("limit")
	limit := 20000
	if limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}
	if r.URL.Query().Get("format") == "json" {
		logs := s.engine.GetStructuredLogs(level, module, limit)
		writeJSON(w, logs)
		return
	}
	logs := s.engine.GetGlobalLogs()
	writeJSON(w, logs)
}

func (s *WebServer) handleLogsClear(w http.ResponseWriter, r *http.Request) {
	s.engine.ClearLogs()
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *WebServer) handleTasksPauseAll(w http.ResponseWriter, r *http.Request) {
	s.engine.tasksLock.Lock()
	for _, t := range s.engine.tasks {
		if t.Status == "running" {
			t.Status = "paused"
		}
	}
	s.engine.tasksLock.Unlock()
	s.engine.Log("[传输控制] 用户触发：全部暂停进行中的传输任务")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *WebServer) handleTasksResumeAll(w http.ResponseWriter, r *http.Request) {
	s.engine.tasksLock.Lock()
	for _, t := range s.engine.tasks {
		if t.Status == "paused" {
			t.Status = "running"
		}
	}
	s.engine.tasksLock.Unlock()
	s.engine.Log("[传输控制] 用户触发：全部继续已暂停的上传任务")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *WebServer) handleTasksToggle(w http.ResponseWriter, r *http.Request) {
	taskID := r.URL.Query().Get("task_id")
	s.engine.tasksLock.Lock()
	defer s.engine.tasksLock.Unlock()

	if t, ok := s.engine.tasks[taskID]; ok {
		if t.Status == "running" {
			t.Status = "paused"
			s.engine.Log(fmt.Sprintf("[传输控制] 用户暂停了任务: %s", t.FileName))
		} else if t.Status == "paused" {
			t.Status = "running"
			s.engine.Log(fmt.Sprintf("[传输控制] 用户恢复了任务: %s", t.FileName))
		}
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *WebServer) handleTasksClearCompleted(w http.ResponseWriter, r *http.Request) {
	singleID := r.URL.Query().Get("task_id")
	if singleID != "" {
		s.engine.tasksLock.Lock()
		delete(s.engine.tasks, singleID)
		s.engine.tasksLock.Unlock()
		writeJSON(w, map[string]string{"status": "ok"})
		return
	}

	clearType := r.URL.Query().Get("type")
	s.engine.tasksLock.Lock()
	for id, t := range s.engine.tasks {
		if clearType == "completed" {
			if t.Status == "completed" {
				delete(s.engine.tasks, id)
			}
		} else if clearType == "failed" {
			if t.Status == "failed" || t.Status == "cancelled" {
				delete(s.engine.tasks, id)
			}
		} else {
			// 包含 type=all, 彻底清空所有已完成、失败、已取消
			if t.Status == "completed" || t.Status == "failed" || t.Status == "cancelled" {
				delete(s.engine.tasks, id)
			}
		}
	}
	s.engine.tasksLock.Unlock()
	logDesc := "已完成与失败"
	if clearType == "completed" {
		logDesc = "已完成"
	} else if clearType == "failed" {
		logDesc = "失败/异常"
	} else if clearType == "all" || clearType == "all_ended" {
		logDesc = "所有终态"
	}
	s.engine.LogInfo("SYSTEM", fmt.Sprintf("[传输控制] 用户触发：清空%s任务记录", logDesc))
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *WebServer) handleTasksCancel(w http.ResponseWriter, r *http.Request) {
	taskID := r.URL.Query().Get("task_id")
	if taskID == "" {
		http.Error(w, "task_id required", 400)
		return
	}
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "user_click"
	}

	s.engine.CancelTaskWithReason(taskID, reason)
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *WebServer) handlePickDir(w http.ResponseWriter, r *http.Request) {
	if runtime.GOOS == "windows" {
		// 采用 Base64 编码穿透 Windows PowerShell 控制台默认 GBK 代码页，彻底杜绝中文路径乱码
		cmd := exec.Command("powershell", "-NoProfile", "-Command",
			`[Console]::OutputEncoding = [System.Text.Encoding]::UTF8; Add-Type -AssemblyName System.Windows.Forms; $f = New-Object Windows.Forms.FolderBrowserDialog; $f.Description = '请选择 QQMailDrive 本地下载保存目录'; if($f.ShowDialog() -eq 'OK'){ [Convert]::ToBase64String([System.Text.Encoding]::UTF8.GetBytes($f.SelectedPath)) }`)
		out, err := cmd.Output()
		if err == nil {
			raw := strings.TrimSpace(string(out))
			if raw != "" {
				path := raw
				if decoded, bErr := base64.StdEncoding.DecodeString(raw); bErr == nil && len(decoded) > 0 {
					path = string(decoded)
				}
				writeJSON(w, map[string]interface{}{"status": "ok", "path": path})
				return
			}
			writeJSON(w, map[string]interface{}{"status": "cancelled"})
			return
		}
	} else if runtime.GOOS == "darwin" {
		cmd := exec.Command("osascript", "-e", `POSIX path of (choose folder with prompt "请选择下载保存目录")`)
		out, err := cmd.Output()
		if err == nil {
			path := strings.TrimSpace(string(out))
			if path != "" {
				writeJSON(w, map[string]interface{}{"status": "ok", "path": path})
				return
			}
			writeJSON(w, map[string]interface{}{"status": "cancelled"})
			return
		}
	}

	writeJSON(w, map[string]interface{}{"status": "unsupported"})
}

func (s *WebServer) handleDownloadLocal(w http.ResponseWriter, r *http.Request) {
	vPath := r.URL.Query().Get("path")
	configLock.RLock()
	downloadDir := GlobalConfig.DownloadDir
	configLock.RUnlock()

	if strings.TrimSpace(downloadDir) == "" {
		downloadDir = "downloads" // 用户未指定时，默认落地到可执行文件所在目录下的 downloads/ 文件夹，防止退出时被 cleanTempDir 清理
	}

	s.engine.indexLock.RLock()
	node, exists := s.engine.masterIndex.Files[vPath]
	s.engine.indexLock.RUnlock()

	if !exists {
		s.engine.LogError("DOWNLOAD", fmt.Sprintf("❌ 本地下载启动受阻，云端虚拟路径未找到: %s", vPath))
		http.Error(w, "文件未找到", 404)
		return
	}

	_ = os.MkdirAll(downloadDir, 0755)
	var destPath string
	if node.IsDir {
		destPath = filepath.Join(downloadDir, node.Name+".zip")
	} else {
		destPath = filepath.Join(downloadDir, node.Name)
	}

	// 秒级检测：若本地已存在该文件且有 .done 标记，直接标记完成，零等待秒开
	if fi, err := os.Stat(destPath); err == nil && (node.Size == 0 || fi.Size() == node.Size) {
		if _, doneErr := os.Stat(destPath + ".done"); doneErr == nil {
			taskID := fmt.Sprintf("task_dl_%d", time.Now().UnixNano())
			rawChunkSize := RawChunkSizeFromTarget(GlobalConfig.ChunkSizeBytes)
			totChunks := int((node.Size + rawChunkSize - 1) / rawChunkSize)
			if totChunks <= 0 {
				totChunks = 1
			}
			allC := make([]int, totChunks)
			for i := 0; i < totChunks; i++ {
				allC[i] = i + 1
			}
			task := &TransferTask{
				TaskID:          taskID,
				VirtualPath:     vPath,
				FileName:        node.Name,
				TotalSize:       node.Size,
				Type:            "download",
				Status:          "completed",
				Stage:           StageCompleted,
				StageDetail:     "✔ 本地已存在完整文件，秒级校验完成",
				Transferred:     node.Size,
				ProgressPct:     100.0,
				SpeedMBs:        0,
				TotalChunks:     totChunks,
				CompletedChunks: allC,
				CreatedAt:       time.Now(),
			}
			s.engine.tasksLock.Lock()
			s.engine.tasks[taskID] = task
			s.engine.tasksLock.Unlock()
			s.engine.LogInfo("DOWNLOAD", fmt.Sprintf("✔ [秒级即开] 本地已存在完整落盘文件，无需重复拉取: %s", destPath))
			writeJSON(w, map[string]interface{}{"status": "ok", "task_id": taskID, "saved_path": destPath, "file_name": node.Name})
			return
		}
	}

	s.engine.LogInfo("DOWNLOAD", fmt.Sprintf("[本地下载] 启动原生落地解密: %s -> %s", node.Name, destPath))

	prevTaskID := "task_prev_" + node.ID
	var inheritedChunks []int
	s.engine.tasksLock.RLock()
	if pt, ok := s.engine.tasks[prevTaskID]; ok && pt != nil {
		inheritedChunks = append([]int(nil), pt.CompletedChunks...)
	}
	s.engine.tasksLock.RUnlock()

	// 若此前有预览任务在占用该文件，立即平滑交接
	s.engine.CancelTaskWithReason(prevTaskID, "task_superseded")
	time.Sleep(100 * time.Millisecond)

	taskID := fmt.Sprintf("task_dl_%d", time.Now().UnixNano())
	task := &TransferTask{
		TaskID:          taskID,
		VirtualPath:     vPath,
		FileName:        node.Name,
		TotalSize:       node.Size,
		Type:            "download",
		Status:          "running",
		Stage:           StageDownloading,
		StageDetail:     "正在连接云端邮箱准备拉取切片...",
		CompletedChunks: inheritedChunks,
		CreatedAt:       time.Now(),
	}

	s.engine.tasksLock.Lock()
	s.engine.tasks[taskID] = task
	s.engine.tasksLock.Unlock()

	go func() {
		s.engine.Log(fmt.Sprintf("[本地直接下载] 启动后台原生流式解密任务: %s -> %s", node.Name, destPath))
		if node.IsDir {
			if err := s.engine.DownloadFolderAsZip(node, destPath); err == nil {
				task.Stage = StageCompleted
				task.Status = "completed"
				task.ProgressPct = 100.0
				s.engine.logTask(task, fmt.Sprintf("✔ 文件夹已成功打包解密保存至本地: %s", destPath))
			} else {
				task.Status = "failed"
				task.Error = err.Error()
				s.engine.logTask(task, fmt.Sprintf("❌ 文件夹打包本地保存失败: %v", err))
			}
		} else {
			if err := s.engine.DownloadStream(task, node, destPath); err != nil {
				task.Status = "failed"
				task.Error = err.Error()
				s.engine.logTask(task, fmt.Sprintf("❌ 文件下载解密失败: %v", err))
			}
		}
	}()

	writeJSON(w, map[string]interface{}{"status": "ok", "task_id": taskID, "saved_path": destPath, "file_name": node.Name})
}


// ==================== 隔离存储空间管理与编码处理 ====================

// decodeModifiedUTF7 is declared in imap_client.go

// isSystemMailbox 严格判定是否为 QQ 邮箱系统自带或衍生目录（排除系统黑名单）
func isSystemMailbox(name string) bool {
	decoded := decodeModifiedUTF7(name)
	rawLower := strings.ToLower(strings.TrimSpace(name))
	decLower := strings.ToLower(strings.TrimSpace(decoded))
	blacklist := map[string]bool{
		"inbox": true, "sent messages": true, "drafts": true,
		"deleted messages": true, "junk": true, "trash": true,
		"已发送": true, "已删除": true, "草稿箱": true, "垃圾邮件": true,
		"群邮件": true, "广告邮件": true, "其他文件夹": true, "订阅邮件": true,
		"漂流瓶": true, "qq邮件订阅": true, "其他": true,
	}
	if blacklist[rawLower] || blacklist[decLower] {
		return true
	}
	// 关键修复：仅排除“其他文件夹”系统目录本身，绝不排除其下由用户自建的隔离空间（如 其他文件夹/资料备份）！
	if name == "&UXZO1mWHTvZZOQ-" || decoded == "其他文件夹" {
		return true
	}
	return false
}

func (s *WebServer) handleFolders(w http.ResponseWriter, r *http.Request) {
	// 毫秒级极速列出 QMD_ 空间 (免除全盘几十个文件夹的轮询遍历探测)
	qmdList, err := s.engine.imapClient.ListQMDFolders()
	if err != nil {
		writeJSON(w, map[string]interface{}{
			"status":  "error",
			"message": err.Error(),
		})
		return
	}

	configLock.RLock()
	currentCanonical := GlobalConfig.StorageFolder
	configLock.RUnlock()

	type FolderInfo struct {
		Name        string `json:"name"`         // 界面显示纯净名 (绝不显示 QMD_)
		Canonical   string `json:"canonical"`    // 云端真实名称 (QMD_xxx)
		DisplayName string `json:"display_name"` // 纯净名
		IsCurrent   bool   `json:"is_current"`
		HasIndex    bool   `json:"has_index"`
		FileCount   int    `json:"file_count"`
		TotalSize   int64  `json:"total_size"`
		StatsLoaded bool   `json:"stats_loaded"`
	}

	var results []FolderInfo
	seen := make(map[string]bool)

	calcStats := func(files map[string]FileNode) (int, int64) {
		cnt := 0
		var sz int64
		for _, fn := range files {
			if !fn.IsDir {
				cnt++
				sz += fn.Size
			}
		}
		return cnt, sz
	}

	for _, raw := range qmdList {
		canonical := decodeModifiedUTF7(raw)
		display := ToDisplayFolder(canonical)
		if seen[display] || display == "" {
			continue
		}
		seen[display] = true
		isCur := (canonical == currentCanonical || display == ToDisplayFolder(currentCanonical))
		fCnt := 0
		var fSz int64
		statsOK := false
		if isCur {
			s.engine.indexLock.RLock()
			fCnt, fSz = calcStats(s.engine.masterIndex.Files)
			s.engine.indexLock.RUnlock()
			statsOK = true
		} else if cachedIdx, ok := s.engine.GetCachedFolderIndex(canonical); ok {
			fCnt, fSz = calcStats(cachedIdx.Files)
			statsOK = true
		}
		results = append(results, FolderInfo{
			Name:        display,
			Canonical:   canonical,
			DisplayName: display,
			IsCurrent:   isCur,
			HasIndex:    true,
			FileCount:   fCnt,
			TotalSize:   fSz,
			StatsLoaded: statsOK,
		})
	}

	// 其余隔离空间按名称自然升序排列，「收到的文件」永远置底
	sort.SliceStable(results, func(i, j int) bool {
		isRecI := (results[i].DisplayName == "收到的文件" || results[i].Name == "收到的文件")
		isRecJ := (results[j].DisplayName == "收到的文件" || results[j].Name == "收到的文件")
		if isRecI != isRecJ {
			return !isRecI
		}
		return strings.ToLower(results[i].DisplayName) < strings.ToLower(results[j].DisplayName)
	})

	// 兜底保障：若列表为空，默认加入 MailDrive
	if len(results) == 0 {
		results = append(results, FolderInfo{
			Name:        "MailDrive",
			Canonical:   "QMD_MailDrive",
			DisplayName: "MailDrive",
			IsCurrent:   true,
			HasIndex:    true,
		})
	}

	writeJSON(w, map[string]interface{}{
		"status":         "ok",
		"current_folder": GlobalConfig.DisplayFolder(),
		"folders":        results,
	})
}

func (s *WebServer) handleFolderRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldFolder string `json:"old_folder"`
		NewFolder string `json:"new_folder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "参数解析失败", 400)
		return
	}
	if ToDisplayFolder(req.OldFolder) == "收到的文件" || ToDisplayFolder(req.NewFolder) == "收到的文件" {
		writeJSON(w, map[string]interface{}{"status": "error", "message": "「收到的文件」为系统分享专属目录，禁止重命名！"})
		return
	}
	oldCanonical := ToCanonicalFolder(req.OldFolder)
	newCanonical := ToCanonicalFolder(req.NewFolder)

	if err := s.engine.imapClient.RenameFolder(oldCanonical, newCanonical); err != nil {
		writeJSON(w, map[string]interface{}{"status": "error", "message": err.Error()})
		return
	}

	configLock.Lock()
	if GlobalConfig.StorageFolder == oldCanonical {
		GlobalConfig.StorageFolder = newCanonical
		_ = SaveConfigInternal()
		go func() {
			if s.engine != nil {
				configLock.RLock()
				c := GlobalConfig
				configLock.RUnlock()
				_ = s.engine.SaveCloudConfig(c)
			}
		}()
	}
	configLock.Unlock()

	s.engine.RenameCachedFolderIndex(oldCanonical, newCanonical)
	s.engine.Log(fmt.Sprintf("[空间管理] ✔ 空间重命名成功: %s -> %s", ToDisplayFolder(oldCanonical), ToDisplayFolder(newCanonical)))
	writeJSON(w, map[string]interface{}{"status": "ok", "new_folder": ToDisplayFolder(newCanonical)})
}

func (s *WebServer) handleFolderDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Folder string `json:"folder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "参数解析失败", 400)
		return
	}

	targetRaw := strings.TrimSpace(req.Folder)
	targetDisplay := ToDisplayFolder(targetRaw)
	if targetDisplay == "" {
		targetDisplay = targetRaw
	}
	if targetDisplay == "收到的文件" {
		writeJSON(w, map[string]interface{}{"status": "error", "message": "「收到的文件」为系统分享专属目录，禁止删除！"})
		return
	}
	canonicalTarget := ToCanonicalFolder(targetDisplay)

	// 1. 获取当前所有 QMD_ 隔离空间列表 (排除专属分享目录)
	qmdList, err := s.engine.imapClient.ListQMDFolders()
	if err != nil {
		writeJSON(w, map[string]interface{}{"status": "error", "message": err.Error()})
		return
	}

	var validDisplayList []string
	seen := make(map[string]bool)
	for _, raw := range qmdList {
		disp := ToDisplayFolder(raw)
		if disp != "" && disp != "收到的文件" && !seen[disp] {
			seen[disp] = true
			validDisplayList = append(validDisplayList, disp)
		}
	}

	if len(validDisplayList) <= 1 {
		writeJSON(w, map[string]interface{}{
			"status":  "error",
			"message": "禁止删除：当前仅剩最后一个隔离空间，网盘必须至少保留一个空间！",
		})
		return
	}

	configLock.RLock()
	currentCanonical := GlobalConfig.StorageFolder
	currentActiveDisplay := ToDisplayFolder(currentCanonical)
	configLock.RUnlock()

	// 明确判定是否删除的是当前正在使用的活动空间
	isDeletingCurrentActive := (currentActiveDisplay == targetDisplay || currentCanonical == canonicalTarget)

	// 预先锁定删除后的首选备用空间 (从剩余空间中选第一个非目标空间)
	fallbackDisplay := ""
	for _, d := range validDisplayList {
		if d != targetDisplay && ToCanonicalFolder(d) != canonicalTarget {
			fallbackDisplay = d
			break
		}
	}
	if fallbackDisplay == "" {
		fallbackDisplay = "MailDrive"
	}
	fallbackCanonical := ToCanonicalFolder(fallbackDisplay)

	// 2. 执行云端清空与物理删除 (先定位云端真实路径，释放选中锁后 DELETE)
	actualMailbox := s.engine.imapClient.FindActualMailboxName(canonicalTarget)
	if err := s.engine.imapClient.DeleteFolder(actualMailbox); err != nil {
		writeJSON(w, map[string]interface{}{"status": "error", "message": err.Error()})
		return
	}

	s.engine.DeleteCachedFolderIndex(canonicalTarget)
	// 3. 严格执行方式 1 逻辑分流处理：
	if isDeletingCurrentActive {
		// 场景 A：删除了当前活动空间 -> 自动跳转至预先锁定的第一个备用非占用空间
		s.engine.CloseDownloadPool()

		configLock.Lock()
		GlobalConfig.StorageFolder = fallbackCanonical
		_ = SaveConfigInternal()
		go func() {
			if s.engine != nil {
				configLock.RLock()
				c := GlobalConfig
				configLock.RUnlock()
				_ = s.engine.SaveCloudConfig(c)
			}
		}()
		configLock.Unlock()

		_ = s.engine.imapClient.EnsureAndSelectFolder(fallbackCanonical)
		_, _ = s.engine.InitOrLoadCloudIndex()

		s.engine.Log(fmt.Sprintf("[空间管理] ✔ 已彻底删除原活动空间 [%s]，系统已安全自动跳转至首个可用空间: [%s]", targetDisplay, fallbackDisplay))
		writeJSON(w, map[string]interface{}{
			"status":         "ok",
			"switched":       true,
			"deleted_folder": targetDisplay,
			"current_folder": fallbackDisplay,
		})
		return
	}

	// 场景 B：删除的是闲置非活动空间 -> 保持当前活动空间完好不动！
	_ = s.engine.imapClient.EnsureAndSelectFolder(currentCanonical)
	s.engine.Log(fmt.Sprintf("[空间管理] ✔ 成功彻底物理抹除闲置隔离空间: [%s] (当前工作空间 [%s] 保持不变)", targetDisplay, currentActiveDisplay))
	writeJSON(w, map[string]interface{}{
		"status":         "ok",
		"switched":       false,
		"deleted_folder": targetDisplay,
		"current_folder": currentActiveDisplay,
	})
}

func (s *WebServer) handleShareCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Paths   []string `json:"paths"`
		ToEmail string   `json:"to_email"`
		Mode    string   `json:"mode"`
		Sid     string   `json:"sid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || len(req.Paths) == 0 || strings.TrimSpace(req.ToEmail) == "" {
		http.Error(w, "分享参数不完整", 400)
		return
	}

	if req.Sid != "" {
		GlobalWebmailClient.SetManualSession(req.Sid)
	}

	if !GlobalWebmailClient.IsSessionValid() {
		writeJSON(w, map[string]interface{}{
			"status":  "error",
			"message": "云端秒转未就绪：当前未登录 Webmail。请先在分享弹窗中点击【手动填 sid】输入一次当前网页版的 sid，系统将自动加密同步至「收到的文件」并启动极速直发！",
		})
		return
	}

	taskID := fmt.Sprintf("task_share_%d", time.Now().UnixNano())
	shareFileName := filepath.Base(req.Paths[0])
	if len(req.Paths) > 1 {
		shareFileName = fmt.Sprintf("%s 等 %d 个文件", filepath.Base(req.Paths[0]), len(req.Paths))
	}
	task := &TransferTask{
		TaskID:      taskID,
		VirtualPath: req.Paths[0],
		FileName:    shareFileName,
		Type:        "share",
		Status:      "running",
		Stage:       StageSharing,
		StageDetail: fmt.Sprintf("准备发送至 %s...", req.ToEmail),
		TargetEmail: req.ToEmail,
		CreatedAt:   time.Now(),
	}

	s.engine.tasksLock.Lock()
	s.engine.tasks[taskID] = task
	s.engine.tasksLock.Unlock()

	go func() {
		_ = s.engine.ShareFilesCloudFast(task, req.Paths, req.ToEmail, GlobalWebmailClient)
	}()

	writeJSON(w, map[string]interface{}{
		"status":  "ok",
		"mode":    "cloud_fast",
		"task_id": taskID,
	})
}

func (s *WebServer) handleWebmailStatus(w http.ResponseWriter, r *http.Request) {
	sess := GlobalWebmailClient.GetSession()
	if sess.Sid == "" && s.engine != nil && GlobalWebmailClient.GetInvalidSid() == "" {
		// 若内存会话未就绪且尚未确认失效，按需主动从云端「收到的文件」尝试静默恢复漫游会话
		if cloudSid, err := s.engine.LoadCloudSession(); err == nil && cloudSid != "" {
			GlobalWebmailClient.RestoreSession(cloudSid)
			sess = GlobalWebmailClient.GetSession()
		}
	}

	isValid := false
	if sess.Sid != "" && sess.Sid != "getUrlParam" && len(sess.Sid) >= 15 {
		// 权威探活：直接向腾讯官方目录 CGI 发起探活
		isValid = GlobalWebmailClient.CheckSidActive(sess.Sid)
		if isValid {
			// 双重防串号复核：核实当前 sid 绑定的邮箱是否确实归属于当前网盘账号
			scannedEmail, _, userErr := GlobalWebmailClient.GetWebmailUserInfo(sess.Sid)
			if userErr == nil && scannedEmail != "" {
				targetEmail := strings.ToLower(strings.TrimSpace(GlobalConfig.Email))
				actualEmail := strings.ToLower(strings.TrimSpace(scannedEmail))
				if targetEmail != "" && !strings.EqualFold(targetEmail, actualEmail) {
					GlobalWebmailClient.LogError("❌ [会话串号清除] 检测到当前绑定的 sid 归属于 %s，与当前网盘账号 %s 不一致！已自动清空！", scannedEmail, GlobalConfig.Email)
					GlobalWebmailClient.ClearSession()
					isValid = false
				}
			}
		} else {
			// 探活未通过，说明 sid 已失效，清空内存会话并自动清理云端过期凭证
			GlobalWebmailClient.ClearSession()
			if s.engine != nil {
				go s.engine.DeleteCloudSession()
			}
		}
		// 探活未通过时置 isValid=false 并清理云端 [QMD:SESSION]，提示重新扫码
	}
	writeJSON(w, map[string]interface{}{
		"status":     "ok",
		"is_valid":   isValid,
		"sid":        sess.Sid,
		"expires_at": sess.ExpiresAt,
	})
}

func (s *WebServer) handleWebmailQR(w http.ResponseWriter, r *http.Request) {
	ctx, err := GlobalWebmailClient.GetQQLoginQRCode()
	if err != nil {
		writeJSON(w, map[string]interface{}{"status": "error", "message": err.Error()})
		return
	}
	writeJSON(w, map[string]interface{}{
		"status":     "ok",
		"type":       "qq",
		"qr_b64":     ctx.QRCodeB64,
		"qrsig":      ctx.QRSig,
		"created_at": ctx.CreatedAt,
	})
}

func (s *WebServer) handleWebmailQRCheck(w http.ResponseWriter, r *http.Request) {
	var req struct {
		QRSig string `json:"qrsig"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.QRSig == "" {
		http.Error(w, "参数错误", 400)
		return
	}

	code, sid, msg, err := GlobalWebmailClient.CheckQRLoginStatus(req.QRSig)
	if err != nil && code != 66 && code != 67 && code != 65 {
		errMsg := msg
		if errMsg == "" {
			errMsg = err.Error()
		}
		writeJSON(w, map[string]interface{}{
			"status":  "error",
			"code":    code,
			"message": errMsg,
		})
		return
	}
	writeJSON(w, map[string]interface{}{
		"status":  "ok",
		"code":    code,
		"sid":     sid,
		"message": msg,
	})
}

func (s *WebServer) handleWebmailSetSid(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Sid string `json:"sid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Sid) == "" {
		http.Error(w, "sid 不可为空", 400)
		return
	}
	clean := strings.TrimSpace(req.Sid)
	if clean == "getUrlParam" || len(clean) < 15 {
		http.Error(w, "无效的 sid！sid 必须是从浏览器地址栏复制的 24 位真实令牌 (如 zV9xRYwuYnYu1mxqAB9OMAAA)，不可为 getUrlParam 或过短字符", 400)
		return
	}
	GlobalWebmailClient.SetManualSession(clean)
	writeJSON(w, map[string]interface{}{"status": "ok", "message": "sid 设置成功"})
}

func (s *WebServer) handleImapReconnect(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil {
		writeJSON(w, map[string]interface{}{"status": "error", "message": "存储引擎未就绪", "connected": false})
		return
	}
	if s.engine.imapClient == nil {
		s.engine.imapClient = NewIMAPClient(GlobalConfig.IMAPServer, GlobalConfig.Email, GlobalConfig.IMAPAuthCode)
		s.engine.imapClient.Logger = s.engine.LogWithLevel
	}
	s.engine.LogInfo("IMAP", "🔄 [手动重连] 正在尝试重新连接 IMAP 主长连接...")
	if err := s.engine.imapClient.Connect(); err != nil {
		s.engine.LogError("IMAP", fmt.Sprintf("❌ [手动重连失败] 无法连入 IMAP 服务器: %v", err))
		writeJSON(w, map[string]interface{}{"status": "error", "message": err.Error(), "connected": false})
		return
	}
	folder := GlobalConfig.StorageFolder
	if folder == "" {
		folder = "QMD_收到的文件"
	}
	_ = s.engine.imapClient.EnsureAndSelectFolder(folder)
	s.engine.LogInfo("IMAP", "✔ [手动重连成功] IMAP 长连接已恢复就绪并完成选区")
	writeJSON(w, map[string]interface{}{"status": "ok", "connected": true})
}

// handleInitCheck 用于首次运行引导的第一层检测：验证账号授权码并探测老用户网盘空间
func (s *WebServer) handleInitCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Email        string `json:"email"`
		IMAPAuthCode string `json:"imap_auth_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, map[string]interface{}{"status": "error", "message": "请求参数解析失败"})
		return
	}

	email := strings.TrimSpace(req.Email)
	if email != "" && !strings.Contains(email, "@") {
		email += "@qq.com"
	}
	authCode := strings.TrimSpace(req.IMAPAuthCode)

	if email == "" || authCode == "" {
		writeJSON(w, map[string]interface{}{"status": "error", "message": "邮箱账号与 16 位 IMAP 授权码不可为空"})
		return
	}

	// 建立独立临时探测客户端进行快速验证与云端探测
	imapServer := GlobalConfig.IMAPServer
	if imapServer == "" {
		imapServer = "imap.qq.com:993"
	}
	tempClient := NewIMAPClient(imapServer, email, authCode)
	if err := tempClient.Connect(); err != nil {
		writeJSON(w, map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("QQ 邮箱验证失败，请检查账号与 16 位授权码: %v", err),
		})
		return
	}
	defer tempClient.Close()

	qmdFolders, err := tempClient.ListQMDFolders()
	if err != nil {
		writeJSON(w, map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("获取云端网盘空间失败: %v", err),
		})
		return
	}

	type FolderInfo struct {
		Name        string `json:"name"`
		DisplayName string `json:"display_name"`
		HasIndex    bool   `json:"has_index"`
	}

	var existingFolders []FolderInfo
	seen := make(map[string]bool)
	for _, m := range qmdFolders {
		// 绝对严格：只有名字带有 QMD_ 前缀的才算网盘空间！
		display := ToDisplayFolder(m)
		// 允许包含「收到的文件」专属空间供老用户按需选择挂载进入
		if display == "" || seen[display] {
			continue
		}
		seen[display] = true
		existingFolders = append(existingFolders, FolderInfo{
			Name:        display,
			DisplayName: display,
			HasIndex:    true,
		})
	}

	// 确保「收到的文件」永远排在列表最底部
	sort.SliceStable(existingFolders, func(i, j int) bool {
		isRecI := (existingFolders[i].DisplayName == "收到的文件" || existingFolders[i].Name == "收到的文件")
		isRecJ := (existingFolders[j].DisplayName == "收到的文件" || existingFolders[j].Name == "收到的文件")
		if isRecI && !isRecJ {
			return false
		}
		if !isRecI && isRecJ {
			return true
		}
		return false
	})

	isOldUser := len(existingFolders) > 0

	writeJSON(w, map[string]interface{}{
		"status":           "ok",
		"is_old_user":      isOldUser,
		"existing_folders": existingFolders,
		"default_folder":   "MailDrive",
	})
}

func (s *WebServer) handleFolderCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Folder string `json:"folder"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Folder) == "" {
		http.Error(w, "文件夹名称不可为空", 400)
		return
	}
	displayFolder := strings.TrimSpace(req.Folder)
	if displayFolder == "收到的文件" {
		writeJSON(w, map[string]interface{}{"status": "error", "message": "「收到的文件」为系统分享专属目录，不可创建为常规网盘空间！"})
		return
	}
	canonicalFolder := ToCanonicalFolder(displayFolder)

	// 1. 在 QQ 邮箱云端确保并建立该新文件夹 (带 QMD_ 前缀)
	if err := s.engine.imapClient.EnsureAndSelectFolder(canonicalFolder); err != nil {
		writeJSON(w, map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("在 QQ 邮箱创建隔离空间失败: %v", err),
		})
		return
	}

	// 2. 切回原本当前正在使用的存储空间，保持当前空间绝对不切换！
	configLock.RLock()
	currentFolder := GlobalConfig.StorageFolder
	configLock.RUnlock()
	_ = s.engine.imapClient.EnsureAndSelectFolder(currentFolder)

	s.engine.SetCachedFolderIndex(canonicalFolder, MasterIndex{
		Version:      1,
		NextSeq:      "0000000001",
		LastUpdated:  time.Now(),
		Files:        make(map[string]FileNode),
		ThumbVolumes: make(map[string]ThumbVolumeInfo),
		ActiveThumb:  "01",
	})
	s.engine.Log(fmt.Sprintf("[空间管理] ✔ 成功在云端创建全新隔离存储空间: [%s] (未切换当前空间)", displayFolder))
	writeJSON(w, map[string]interface{}{
		"status": "ok",
		"folder": ToDisplayFolder(canonicalFolder),
	})
}

func (s *WebServer) handleFolderSwitch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Folder       string `json:"folder"`
		ForceRefresh bool   `json:"force_refresh"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Folder) == "" {
		http.Error(w, "目标文件夹不可为空", 400)
		return
	}
	displayFolder := strings.TrimSpace(req.Folder)
	canonicalFolder := ToCanonicalFolder(displayFolder)

	// 空间切换前：更新旧空间内存缓存；若此前有正在排队的未落盘防抖同步，在后台完成落盘，绝不阻断当前 0ms 瞬切
	configLock.RLock()
	oldFolder := GlobalConfig.StorageFolder
	configLock.RUnlock()
	if oldFolder != canonicalFolder {
		s.engine.indexLock.RLock()
		s.engine.SetCachedFolderIndex(oldFolder, s.engine.masterIndex)
		oldIdxCopy := cloneMasterIndex(s.engine.masterIndex)
		s.engine.indexLock.RUnlock()

		if s.engine.CancelDebounceSync() {
			go func(folder string, idx MasterIndex) {
				_ = s.engine.SyncSpecifiedIndexToCloud(folder, idx)
			}(oldFolder, oldIdxCopy)
		}
	}

	// 1. 若内存已预加载该空间索引且未要求强制刷新，直接 0ms 内存瞬切，IMAP 选区与落盘异步后台同步
	if !req.ForceRefresh {
		if cachedIdx, ok := s.engine.GetCachedFolderIndex(canonicalFolder); ok {
			configLock.Lock()
			GlobalConfig.StorageFolder = canonicalFolder
			configLock.Unlock()

			s.engine.indexLock.Lock()
			s.engine.masterIndex = cachedIdx
			s.engine.indexLock.Unlock()

			go func(targetCanonical string) {
				configLock.RLock()
				latest := GlobalConfig.StorageFolder
				configLock.RUnlock()
				if latest != targetCanonical {
					return
				}
				_ = s.engine.imapClient.SelectFolderOnly(targetCanonical)
				configLock.Lock()
				_ = SaveConfigInternal()
				configLock.Unlock()
			}(canonicalFolder)

			s.engine.Log(fmt.Sprintf("[空间切换] ⚡ 命中内存缓存瞬切至: [%s] (纳管项目: %d)", displayFolder, len(cachedIdx.Files)))
			writeJSON(w, map[string]interface{}{
				"status": "ok",
				"folder": ToDisplayFolder(canonicalFolder),
				"cached": true,
			})
			return
		}
	}

	// 2. 未命中缓存或强制刷新时，同步选定目录并拉取云端主索引
	if err := s.engine.imapClient.SelectFolderOnly(canonicalFolder); err != nil {
		writeJSON(w, map[string]interface{}{
			"status":  "error",
			"message": fmt.Sprintf("无法连接进入目标空间 [%s]: %v", displayFolder, err),
		})
		return
	}

	// 3. 尚未缓存或强制刷新时，重置辅助连接池并从云端拉取最新主索引
	s.engine.CloseDownloadPool()

	configLock.Lock()
	GlobalConfig.StorageFolder = canonicalFolder
	_ = SaveConfigInternal()
	go func() {
		if s.engine != nil {
			configLock.RLock()
			c := GlobalConfig
			configLock.RUnlock()
			_ = s.engine.SaveCloudConfig(c)
		}
	}()
	configLock.Unlock()

	_, _ = s.engine.InitOrLoadCloudIndex()

	s.engine.Log(fmt.Sprintf("[空间切换] ✔ 已从云端同步并切换至隔离空间: [%s]", displayFolder))
	writeJSON(w, map[string]interface{}{
		"status": "ok",
		"folder": ToDisplayFolder(canonicalFolder),
	})
}

// handleFolderSubdirs 快速只读抓取指定隔离空间的子目录树，用于跨空间移动弹窗展开深层目录
func (s *WebServer) handleFolderSubdirs(w http.ResponseWriter, r *http.Request) {
	folder := r.URL.Query().Get("folder")
	if strings.TrimSpace(folder) == "" {
		http.Error(w, "缺少文件夹参数", 400)
		return
	}

	type SubDirNode struct {
		Name        string `json:"name"`
		VirtualPath string `json:"virtual_path"`
	}

	// 优先命中内存缓存，移动/复制选择隔离文件夹时 0ms 秒开
	if cachedIdx, ok := s.engine.GetCachedFolderIndex(folder); ok {
		var dirNodes []SubDirNode
		dirNodes = append(dirNodes, SubDirNode{Name: "全部文件 (根目录)", VirtualPath: "/"})
		for _, f := range cachedIdx.Files {
			if f.IsDir && f.VirtualPath != "/" {
				dirNodes = append(dirNodes, SubDirNode{
					Name:        f.Name,
					VirtualPath: f.VirtualPath,
				})
			}
		}
		sort.Slice(dirNodes, func(i, j int) bool {
			return dirNodes[i].VirtualPath < dirNodes[j].VirtualPath
		})
		writeJSON(w, map[string]interface{}{
			"status":    "ok",
			"folder":    ToDisplayFolder(ToCanonicalFolder(folder)),
			"dir_nodes": dirNodes,
			"cached":    true,
		})
		return
	}

	// 临时客户端连接
	tempClient := NewIMAPClient(GlobalConfig.IMAPServer, GlobalConfig.Email, GlobalConfig.IMAPAuthCode)
	if err := tempClient.Connect(); err != nil {
		writeJSON(w, map[string]interface{}{"status": "error", "message": err.Error()})
		return
	}
	defer tempClient.Close()

	// 准确定位目标隔离空间的真实云端名称 (自动兼容 其他文件夹/QMD_xxx)
	actualBox := tempClient.FindActualMailboxName(folder)
	if err := tempClient.EnsureAndSelectFolder(actualBox); err != nil {
		writeJSON(w, map[string]interface{}{"status": "error", "message": err.Error()})
		return
	}

	headers, _ := tempClient.FetchAllHeadersFast()
	var bestIdx uint32
	var bestVer int64 = -1
	for _, th := range headers {
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

	var dirNodes []SubDirNode
	dirNodes = append(dirNodes, SubDirNode{Name: "全部文件 (根目录)", VirtualPath: "/"})

	// 严格只解析该目标空间自身的索引，绝不串入当前空间的数据！
	if bestIdx > 0 {
		if raw, err := tempClient.FetchRawMessageBody(bestIdx); err == nil {
			enc := extractAttachmentFromMIME(raw)
			if plain, dErr := s.engine.DecryptData(enc); dErr == nil {
				var targetIdx MasterIndex
				if json.Unmarshal(plain, &targetIdx) == nil && targetIdx.Files != nil {
					s.engine.SetCachedFolderIndex(actualBox, targetIdx)
					for _, f := range targetIdx.Files {
						if f.IsDir && f.VirtualPath != "/" {
							dirNodes = append(dirNodes, SubDirNode{
								Name:        f.Name,
								VirtualPath: f.VirtualPath,
							})
						}
					}
				}
			}
		}
	}

	sort.Slice(dirNodes, func(i, j int) bool {
		return dirNodes[i].VirtualPath < dirNodes[j].VirtualPath
	})

	writeJSON(w, map[string]interface{}{
		"status":   "ok",
		"folder":   ToDisplayFolder(actualBox),
		"dir_nodes": dirNodes,
	})
}

func (s *WebServer) handleThumbnail(w http.ResponseWriter, r *http.Request) {
	vPath := r.URL.Query().Get("path")
	if vPath == "" {
		http.Error(w, "缺少路径参数", 400)
		return
	}
	s.engine.indexLock.RLock()
	node, exists := s.engine.masterIndex.Files[vPath]
	s.engine.indexLock.RUnlock()
	if !exists || node.IsDir {
		http.Error(w, "未找到媒体文件", 404)
		return
	}

	configLock.RLock()
	useLocalThumbDB := GlobalConfig.IsThumbLocalCacheEnabled()
	configLock.RUnlock()
	cacheHeader := "no-store"
	if useLocalThumbDB {
		cacheHeader = "public, max-age=86400"
	}

	// 1. 负向缓存速断 (0ms 瞬时阻断，严防无图文件重复发起漫长云端请求)
	if s.engine.HasNoThumb(node.SHA256) {
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.Error(w, "未找到缩略图数据", 404)
		return
	}

	// 2. 正向缓存命中
	if cachedBytes, ok := s.engine.GetCachedThumb(node.SHA256); ok && len(cachedBytes) > 0 {
		w.Header().Set("Content-Type", "image/webp")
		w.Header().Set("Cache-Control", cacheHeader)
		_, _ = w.Write(cachedBytes)
		return
	}

	if s.engine.imapClient == nil || !s.engine.imapClient.connected {
		http.Error(w, "云端连接未就绪", 503)
		return
	}

	// 确定目标云端隔离目录（防止并发缩略图请求或后台收件扫描切走选区）
	configLock.RLock()
	curStorageFolder := GlobalConfig.StorageFolder
	configLock.RUnlock()
	targetBox := curStorageFolder
	if node.IsSharedIn || node.ShareID != "" {
		targetBox = s.engine.imapClient.FindActualMailboxName("收到的文件")
		if targetBox == "" {
			targetBox = "QMD_收到的文件"
		}
	}

	if targetBox != "" {
		_ = s.engine.imapClient.EnsureAndSelectFolder(targetBox)
	}

	s.engine.LogDebug("THUMB", fmt.Sprintf("🔍 [%s] 本地未命中缓存，正在从云端邮件拉取缩略图部件...", node.Name))

	headers, err := s.engine.imapClient.FetchAllHeadersFast()
	if err != nil {
		s.engine.LogWarn("THUMB", fmt.Sprintf("⚠️ [%s] 读取邮件列表失败: %v", node.Name, err))
		http.Error(w, "读取邮件列表失败", 500)
		return
	}
	seqs := s.engine.LocateChunkSeqsForNode(&node, headers)
	if len(seqs) == 0 {
		s.engine.LogWarn("THUMB", fmt.Sprintf("⚠️ [%s] 未在目录 [%s] 定位到分片邮件，已记录无图并降级展示默认图标", node.Name, targetBox))
		s.engine.MarkNoThumb(node.SHA256)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.Error(w, "未找到分片邮件", 404)
		return
	}

	// 依次尝试拉取 Part 1 / 1.2 / 1.1 正文数据 (每次拉取前确保选区仍在 targetBox，防止后台扫描 INBOX 抢占选区)
	var encThumb []byte
	for _, partID := range []string{"1", "1.2", "1.1"} {
		if targetBox != "" {
			_ = s.engine.imapClient.EnsureAndSelectFolder(targetBox)
		}
		partBytes, err := s.engine.imapClient.FetchMessagePart(seqs[0], partID)
		if err == nil && len(partBytes) > 0 {
			encThumb = s.engine.ExtractThumbnailData(partBytes)
			if len(encThumb) > 0 {
				break
			}
		}
	}
	if len(encThumb) == 0 {
		s.engine.LogInfo("THUMB", fmt.Sprintf("ℹ [%s] 云端邮件未附带缩略图，已记录无图并优雅降级展示默认图标", node.Name))
		s.engine.MarkNoThumb(node.SHA256)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.Error(w, "未找到缩略图数据", 404)
		return
	}

	keys := s.engine.ResolveDecryptionKeys(&node)
	var thumbPlain []byte
	for _, k := range keys {
		if p, dErr := s.engine.DecryptThumbnail(encThumb, k); dErr == nil && len(p) > 0 {
			thumbPlain = p
			break
		}
	}
	if len(thumbPlain) == 0 {
		s.engine.LogInfo("THUMB", fmt.Sprintf("ℹ [%s] 缩略图密文未能解密或格式不匹配，已记录无图并优雅降级展示默认图标", node.Name))
		s.engine.MarkNoThumb(node.SHA256)
		w.Header().Set("Cache-Control", "public, max-age=86400")
		http.Error(w, "未找到有效缩略图", 404)
		return
	}

	s.engine.SaveCachedThumb(node.SHA256, thumbPlain)
	s.engine.LogInfo("THUMB", fmt.Sprintf("✔ [%s] 云端缩略图拉取并解密成功 (%d 字节)", node.Name, len(thumbPlain)))
	w.Header().Set("Content-Type", "image/webp")
	w.Header().Set("Cache-Control", cacheHeader)
	_, _ = w.Write(thumbPlain)
}
