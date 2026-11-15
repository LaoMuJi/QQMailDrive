package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"sync"
)

// AppInfo 存放软件版本与作者元数据
const (
	AppName      = "QQMailDrive"
	AppVersion   = "v1.0.0"
	AppAuthor    = "杀熊"
	AppBuildDate = "2026-11-15"
	DefaultPort  = 18080
	QMD_PREFIX   = "QMD_"
)

// Config 存储本地网盘配置 (精确到字节)
type Config struct {
	Email                    string            `json:"email"`
	IMAPAuthCode             string            `json:"imap_auth_code"`
	IMAPServer               string            `json:"imap_server"`
	SMTPServer               string            `json:"smtp_server,omitempty"`
	UserNick                 string            `json:"user_nick,omitempty"`                 // 发送分享时的昵称
	StorageFolder            string            `json:"storage_folder"`                      // 云端真实名称，带 QMD_ 前缀
	ChunkSizeBytes           int64             `json:"chunk_size_bytes"`                    // 单分片精确字节数（默认 52428799 约 50MB）
	ChunkSizeMB              int               `json:"chunk_size_mb,omitempty"`             // 兼容旧字段
	MaxConcurrency           int               `json:"max_concurrency"`                     // 最大并发传输任务数
	ChunkDownloadConcurrency int               `json:"chunk_download_concurrency"`          // 大文件分片并发下载数 (默认3)
	HTTPDownloadConcurrency  int               `json:"http_download_concurrency"`           // HTTP 网页端分片并发下载数 (默认5，设为0禁用)
	HTTPUploadConcurrency    int               `json:"http_upload_concurrency"`             // HTTP 网页端并发上传数 (默认1，设为0禁用)
	RequestDelayMS           int               `json:"request_delay_ms"`                    // 平滑延时，防风控（推荐250ms）
	AutoPackSmallFiles       bool              `json:"auto_pack_small_files"`               // 是否自动合包小文件
	SmallFileThresholdMB     int               `json:"small_file_threshold_mb"`             // 小文件判定阈值
	ThumbSizePX              int               `json:"thumb_size_px"`                       // 缩略图基准
	ThumbLocalCache          *bool             `json:"thumb_local_cache,omitempty"`         // 是否启用本地缩略图数据库缓存 (默认开启)
	CleanExit                bool              `json:"clean_exit"`                          // 是否正常退出
	DownloadDir              string            `json:"download_dir"`                        // 本地下载保存目录 (当前设备生效值)
	DownloadDirs             map[string]string `json:"download_dirs,omitempty"`             // 各电脑独立下载目录映射 [ComputerID -> Dir]
}

// CloudConfigData 定义存放在云端「收到的文件」专属邮件 [QMD:CONFIG] 中的漫游配置结构体
type CloudConfigData struct {
	Version                  int               `json:"version"`
	UpdatedTime              int64             `json:"updated_time"`
	UserNick                 string            `json:"user_nick,omitempty"`
	StorageFolder            string            `json:"storage_folder,omitempty"`
	ChunkSizeBytes           int64             `json:"chunk_size_bytes,omitempty"`
	MaxConcurrency           int               `json:"max_concurrency"`
	ChunkDownloadConcurrency int               `json:"chunk_download_concurrency"`
	HTTPDownloadConcurrency  int               `json:"http_download_concurrency"`
	HTTPUploadConcurrency    int               `json:"http_upload_concurrency"`
	RequestDelayMS           int               `json:"request_delay_ms"`
	AutoPackSmallFiles       bool              `json:"auto_pack_small_files"`
	SmallFileThresholdMB     int               `json:"small_file_threshold_mb,omitempty"`
	ThumbSizePX              int               `json:"thumb_size_px,omitempty"`
	ThumbLocalCache          *bool             `json:"thumb_local_cache,omitempty"`
	SMTPServer               string            `json:"smtp_server,omitempty"`
	DownloadDirs             map[string]string `json:"download_dirs,omitempty"`
}

// GetComputerID 生成当前电脑的唯一稳定识别码 (主机名_网卡哈希)，用于区分多设备下载目录
func GetComputerID() string {
	hostname, _ := os.Hostname()
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		hostname = "PC"
	}

	interfaces, err := net.Interfaces()
	var macs []string
	if err == nil {
		for _, iface := range interfaces {
			if iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			mac := iface.HardwareAddr.String()
			if mac != "" {
				macs = append(macs, mac)
			}
		}
	}
	sort.Strings(macs)
	raw := hostname + "|" + strings.Join(macs, ",")
	h := sha256.Sum256([]byte(raw))
	shortHash := hex.EncodeToString(h[:4])
	return fmt.Sprintf("%s_%s", hostname, shortHash)
}

// ApplyCloudConfig 将从云端拉取到的配置安全合并至当前内存 GlobalConfig 与本地磁盘
func ApplyCloudConfig(cloudData *CloudConfigData) {
	if cloudData == nil {
		return
	}
	configLock.Lock()
	defer configLock.Unlock()

	// 1. 同步漫游核心配置项
	if cloudData.UserNick != "" {
		GlobalConfig.UserNick = SanitizeUserNick(cloudData.UserNick)
	}
	if cloudData.ChunkSizeBytes > 0 {
		GlobalConfig.ChunkSizeBytes = cloudData.ChunkSizeBytes
		GlobalConfig.ChunkSizeMB = int(cloudData.ChunkSizeBytes / (1024 * 1024))
	}
	if cloudData.MaxConcurrency > 0 {
		if cloudData.MaxConcurrency > 3 {
			GlobalConfig.MaxConcurrency = 3
		} else {
			GlobalConfig.MaxConcurrency = cloudData.MaxConcurrency
		}
	}
	if cloudData.ChunkDownloadConcurrency > 0 {
		if cloudData.ChunkDownloadConcurrency > 5 {
			GlobalConfig.ChunkDownloadConcurrency = 5
		} else {
			GlobalConfig.ChunkDownloadConcurrency = cloudData.ChunkDownloadConcurrency
		}
	}
	if cloudData.HTTPDownloadConcurrency >= 0 {
		if cloudData.HTTPDownloadConcurrency > 5 {
			GlobalConfig.HTTPDownloadConcurrency = 5
		} else {
			GlobalConfig.HTTPDownloadConcurrency = cloudData.HTTPDownloadConcurrency
		}
	}
	if cloudData.HTTPUploadConcurrency >= 0 {
		if cloudData.HTTPUploadConcurrency > 3 {
			GlobalConfig.HTTPUploadConcurrency = 3
		} else {
			GlobalConfig.HTTPUploadConcurrency = cloudData.HTTPUploadConcurrency
		}
	}
	if cloudData.RequestDelayMS >= 0 {
		GlobalConfig.RequestDelayMS = cloudData.RequestDelayMS
	}
	if cloudData.SmallFileThresholdMB > 0 {
		GlobalConfig.SmallFileThresholdMB = cloudData.SmallFileThresholdMB
	}
	if cloudData.ThumbSizePX > 0 {
		GlobalConfig.ThumbSizePX = cloudData.ThumbSizePX
	}
	if cloudData.ThumbLocalCache != nil {
		GlobalConfig.ThumbLocalCache = cloudData.ThumbLocalCache
	}
	if cloudData.SMTPServer != "" {
		GlobalConfig.SMTPServer = cloudData.SMTPServer
	}
	if cloudData.StorageFolder != "" {
		canonical := ToCanonicalFolder(cloudData.StorageFolder)
		GlobalConfig.StorageFolder = canonical
	}

	// 2. 合并多电脑下载目录映射
	if GlobalConfig.DownloadDirs == nil {
		GlobalConfig.DownloadDirs = make(map[string]string)
	}
	for cid, dir := range cloudData.DownloadDirs {
		if dir != "" {
			GlobalConfig.DownloadDirs[cid] = dir
		}
	}

	// 3. 针对当前电脑匹配其专属下载目录
	currentCID := GetComputerID()
	if myDir, ok := GlobalConfig.DownloadDirs[currentCID]; ok && myDir != "" {
		GlobalConfig.DownloadDir = myDir
	}

	// 4. 持久化到本地 config.json
	_ = SaveConfigInternal()
}

func (c *Config) IsThumbLocalCacheEnabled() bool {
	if c.ThumbLocalCache == nil {
		return true
	}
	return *c.ThumbLocalCache
}

var (
	GlobalConfig Config
	configLock   sync.RWMutex
	configPath   = "config.json"
)

// IsQMDFolder 严格判定文件夹名称是否属于 QMD 规范 (只有带 QMD_ 前缀的才算，其他一概不认)
func IsQMDFolder(rawName string) bool {
	clean := decodeModifiedUTF7(rawName)
	clean = strings.TrimPrefix(clean, "其他文件夹/")
	clean = strings.TrimPrefix(clean, "/")
	return strings.HasPrefix(clean, QMD_PREFIX)
}

// ToCanonicalFolder 将用户界面输入的无前缀文件夹名转换为云端带 QMD_ 前缀的标准名称
func ToCanonicalFolder(display string) string {
	display = strings.TrimSpace(display)
	if display == "" {
		display = "MailDrive"
	}
	clean := decodeModifiedUTF7(display)
	clean = strings.TrimPrefix(clean, "其他文件夹/")
	clean = strings.TrimPrefix(clean, "/")
	clean = strings.TrimPrefix(clean, QMD_PREFIX)
	return QMD_PREFIX + clean
}

// ToDisplayFolder 严格提取显示名：只有真带 QMD_ 前缀的才剥离显示；不带 QMD_ 的一律返回空(彻底过滤)
func ToDisplayFolder(canonical string) string {
	clean := decodeModifiedUTF7(canonical)
	clean = strings.TrimPrefix(clean, "其他文件夹/")
	clean = strings.TrimPrefix(clean, "/")
	if strings.HasPrefix(clean, QMD_PREFIX) {
		return strings.TrimPrefix(clean, QMD_PREFIX)
	}
	return clean
}

func (c *Config) DisplayFolder() string {
	return ToDisplayFolder(c.StorageFolder)
}

// SanitizeUserNick 清洗分享人昵称，去除首尾空格并替换路径非法字符，防止接收端目录拼接异常
func SanitizeUserNick(nick string) string {
	nick = strings.TrimSpace(nick)
	if nick == "" {
		return ""
	}
	replacer := strings.NewReplacer("/", "_", "\\", "_", ":", "_", "*", "_", "?", "_", "\"", "_", "<", "_", ">", "_", "|", "_")
	return strings.TrimSpace(replacer.Replace(nick))
}

// IsConfigured 校验是否已完成真实账号与授权码配置
func (c *Config) IsConfigured() bool {
	if c.Email == "" || c.Email == "your_qq_number@qq.com" {
		return false
	}
	if c.IMAPAuthCode == "" || c.IMAPAuthCode == "your_imap_authorization_code" {
		return false
	}
	return true
}

func DefaultConfig() Config {
	return Config{
		Email:                    "",
		IMAPAuthCode:             "",
		IMAPServer:               "imap.qq.com:993",
		SMTPServer:               "smtp.qq.com:465",
		UserNick:                 "",
		StorageFolder:            "QMD_MailDrive",
		ChunkSizeBytes:           52428799, // 52428799 字节（约 50.00 MB）
		MaxConcurrency:           1,
		ChunkDownloadConcurrency: 3,
		HTTPDownloadConcurrency:  3,
		HTTPUploadConcurrency:    1,
		RequestDelayMS:           250,
		AutoPackSmallFiles:       true,
		SmallFileThresholdMB:     5,
		ThumbSizePX:              384,
		ThumbLocalCache:          func() *bool { b := true; return &b }(),
		CleanExit:                true,
		DownloadDir:              "",
		DownloadDirs:             make(map[string]string),
	}
}

func LoadConfig() error {
	configLock.Lock()
	defer configLock.Unlock()

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			GlobalConfig = DefaultConfig()
			return SaveConfigInternal()
		}
		return err
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		GlobalConfig = DefaultConfig()
		return SaveConfigInternal()
	}

	if cfg.ChunkSizeBytes <= 0 {
		if cfg.ChunkSizeMB > 0 {
			cfg.ChunkSizeBytes = int64(cfg.ChunkSizeMB) * 1024 * 1024
		} else {
			cfg.ChunkSizeBytes = 52428799
		}
	}
	cfg.ChunkSizeMB = 0

	if cfg.RequestDelayMS < 0 {
		cfg.RequestDelayMS = 250
	}
	if cfg.MaxConcurrency <= 0 {
		cfg.MaxConcurrency = 1
	} else if cfg.MaxConcurrency > 3 {
		cfg.MaxConcurrency = 3
	}
	if cfg.ChunkDownloadConcurrency <= 0 {
		cfg.ChunkDownloadConcurrency = 3
	} else if cfg.ChunkDownloadConcurrency > 5 {
		cfg.ChunkDownloadConcurrency = 5
	}
	if cfg.HTTPDownloadConcurrency < 0 {
		cfg.HTTPDownloadConcurrency = 3
	} else if cfg.HTTPDownloadConcurrency > 5 {
		cfg.HTTPDownloadConcurrency = 5
	}
	if cfg.HTTPUploadConcurrency < 0 {
		cfg.HTTPUploadConcurrency = 1
	} else if cfg.HTTPUploadConcurrency > 3 {
		cfg.HTTPUploadConcurrency = 3
	}
	if cfg.SMTPServer == "" {
		cfg.SMTPServer = "smtp.qq.com:465"
	}
	cfg.UserNick = SanitizeUserNick(cfg.UserNick)

	// 自动确保云端目录带有 QMD_ 前缀
	cfg.StorageFolder = decodeModifiedUTF7(cfg.StorageFolder)
	if cfg.StorageFolder == "" || cfg.StorageFolder == "MailDrive" {
		cfg.StorageFolder = "QMD_MailDrive"
	} else if !strings.HasPrefix(cfg.StorageFolder, QMD_PREFIX) {
		cfg.StorageFolder = QMD_PREFIX + cfg.StorageFolder
	}

	// 初始化与匹配当前设备下载目录映射
	if cfg.DownloadDirs == nil {
		cfg.DownloadDirs = make(map[string]string)
	}
	cid := GetComputerID()
	if savedDir, ok := cfg.DownloadDirs[cid]; ok && savedDir != "" {
		cfg.DownloadDir = savedDir
	} else if cfg.DownloadDir != "" {
		cfg.DownloadDirs[cid] = cfg.DownloadDir
	}

	GlobalConfig = cfg
	return nil
}

func SaveConfig(cfg Config) error {
	configLock.Lock()
	defer configLock.Unlock()

	// 智能合并并更新当前设备下载目录映射
	if cfg.DownloadDirs == nil {
		if GlobalConfig.DownloadDirs != nil {
			cfg.DownloadDirs = make(map[string]string)
			for k, v := range GlobalConfig.DownloadDirs {
				cfg.DownloadDirs[k] = v
			}
		} else {
			cfg.DownloadDirs = make(map[string]string)
		}
	}
	cid := GetComputerID()
	if cfg.DownloadDir != "" {
		cfg.DownloadDirs[cid] = cfg.DownloadDir
	}

	if cfg.ChunkSizeBytes <= 0 {
		if cfg.ChunkSizeMB > 0 {
			cfg.ChunkSizeBytes = int64(cfg.ChunkSizeMB) * 1024 * 1024
		} else {
			cfg.ChunkSizeBytes = 52428799
		}
	}
	cfg.ChunkSizeMB = 0

	cfg.StorageFolder = decodeModifiedUTF7(cfg.StorageFolder)
	if cfg.StorageFolder == "" || cfg.StorageFolder == "MailDrive" {
		cfg.StorageFolder = "QMD_MailDrive"
	} else if !strings.HasPrefix(cfg.StorageFolder, QMD_PREFIX) {
		cfg.StorageFolder = QMD_PREFIX + cfg.StorageFolder
	}

	if cfg.SMTPServer == "" {
		cfg.SMTPServer = "smtp.qq.com:465"
	}
	cfg.UserNick = SanitizeUserNick(cfg.UserNick)

	GlobalConfig = cfg
	return SaveConfigInternal()
}

func SaveConfigInternal() error {
	data, err := json.MarshalIndent(GlobalConfig, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(configPath, data, 0644)
}

func SetCleanExitStatus(clean bool) {
	configLock.Lock()
	defer configLock.Unlock()
	GlobalConfig.CleanExit = clean
	_ = SaveConfigInternal()
}
