package main

import "time"

// StorageType 存储类型
type StorageType string

const (
	StorageTypeChunked StorageType = "chunked" // 大文件独立切片
	StorageTypeBundled StorageType = "bundled" // 小文件聚合包
	StorageTypeDir     StorageType = "dir"     // 纯虚拟文件夹节点（零邮件成本）
)

// LogLevel 日志等级枚举
type LogLevel string

const (
	LogLevelDebug LogLevel = "DEBUG" // 详细性能探针、TTFB、网络吞吐、流控耗时
	LogLevelInfo  LogLevel = "INFO"  // 核心业务状态、任务出入队、完成通知、建档
	LogLevelWarn  LogLevel = "WARN"  // 自动重试、网络抖动、断线重连、格式降级
	LogLevelError LogLevel = "ERROR" // 解密失败、落盘失败、认证失败、网络中断
)

// 媒体流边下边播元数据调度常量 (15MB 黄金平衡阈值)
const (
	TailMetadataMinChunkSize = 15 * 1024 * 1024
	// ChunkProtocolOverhead 单分片加密与协议包装固定开销 (字节)：
	// MagicBytes ("QQDRV01", 7B) + Nonce (12B) + AES-GCM AuthTag (16B) + 压缩模式标记头 (1B) = 36 字节
	ChunkProtocolOverhead = 36
)

// RawChunkSizeFromTarget 根据用户期望的最终分片封包大小 (如 52428799) 倒推计算原始文件切片读取大小，
// 确保密文 .7z 封包大小严格不超过并精确对齐用户设定的字节数
func RawChunkSizeFromTarget(targetChunkSize int64) int64 {
	if targetChunkSize <= 0 {
		targetChunkSize = 52428799
	}
	raw := targetChunkSize - ChunkProtocolOverhead
	if raw <= 0 {
		return targetChunkSize
	}
	return raw
}

// LogEntry 结构化分级日志条目
type LogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	TimeStr   string    `json:"time_str"`          // 格式化时间 "15:04:05"
	Level     LogLevel  `json:"level"`             // DEBUG / INFO / WARN / ERROR
	Module    string    `json:"module"`            // "IMAP" / "SMTP" / "TRANSFER" / "PREVIEW" / "INDEX" / "SHARE" / "COMPACT" / "SYSTEM"
	Message   string    `json:"message"`           // 纯净消息体
	TaskID    string    `json:"task_id,omitempty"` // 关联的传输任务 ID (可选)
}

// ChunkInfo 单个分片记录
type ChunkInfo struct {
	Seq       string `json:"seq"`        // 分片序列号 (新版为自描述 QMD_<hash>_...，兼容旧版 10位序列号)
	PartIndex int    `json:"part_index"` // 当前分片索引 (从1开始)
	TotalPart int    `json:"total_part"` // 总分片数
	Size      int64  `json:"size"`       // 分片原始字节数
	SHA256    string `json:"sha256"`     // 该分片的 SHA-256 校验码
	MailUID   uint32 `json:"mail_uid"`   // 对应的 IMAP 邮件 UID
}

// FileNode 虚拟文件系统节点
type FileNode struct {
	ID          string      `json:"id"`           // 文件全局唯一 UUID
	Name        string      `json:"name"`         // 显示文件名
	VirtualPath string      `json:"virtual_path"` // 完整虚拟路径，如 "/我的项目/文档/note.txt"
	IsDir       bool        `json:"is_dir"`       // 是否为虚拟目录
	Size        int64       `json:"size"`         // 总字节大小
	ModTime     time.Time   `json:"mod_time"`     // 修改时间 / 接收时间
	SHA256      string      `json:"sha256"`       // 全文件最终 SHA-256 校验码
	StorageType StorageType `json:"storage_type"` // chunked / bundled / dir

	// 大文件切片专属
	Chunks []ChunkInfo `json:"chunks,omitempty"`

	// 小文件合包专属
	BundleSeq string `json:"bundle_seq,omitempty"` // 所属合包的10位序列号
	InnerPath string `json:"inner_path,omitempty"` // 在合包内的相对提取路径

	// 动态独立信封加密密钥 (Base64编码，32字节真随机AES-256密钥)
	CipherKeyBase64 string `json:"cipher_key_base64,omitempty"`

	// 分享转存标记
	IsSharedIn bool   `json:"is_shared_in,omitempty"` // 是否为接收自好友分享的文件
	SharedFrom string `json:"shared_from,omitempty"`  // 发送方账号或昵称
	ShareID    string `json:"share_id,omitempty"`     // 对应分享批次的唯一 ShareID

	// 媒体与缩略图专属
	IsMedia     bool   `json:"is_media"`                // 是否为图片或视频
	MediaType   string `json:"media_type,omitempty"`    // "image" 或 "video"
	ThumbVolID  string `json:"thumb_vol_id,omitempty"`  // 所属缩略图卷号，如 "01"
	ThumbItemID string `json:"thumb_item_id,omitempty"` // 缩略图在卷内的条目ID
	ThumbBase64 string `json:"thumb_base64,omitempty"`  // 极小预览直接内嵌缓存（或由前端懒加载）

	// 文件夹统计专属
	DirectCount int `json:"direct_count,omitempty"` // 文件夹内部直接包含的文件/文件夹数量
}

// ThumbVolumeInfo 缩略图分卷信息
type ThumbVolumeInfo struct {
	VolID      string `json:"vol_id"`      // "01", "02"...
	Seq        string `json:"seq"`         // 对应的10位邮件附件序列号
	TotalItems int    `json:"total_items"` // 包含的缩略图数量
	TotalBytes int64  `json:"total_bytes"` // 累积字节大小
	IsSealed   bool   `json:"is_sealed"`   // 是否已满额封包
}

// MasterIndex 全局主索引结构

// EnsureChunks 动态推导并补齐分片列表 (若云端主索引瘦身省略了 Chunks 数组，则根据文件大小与哈希零开销推导)
func (n *FileNode) EnsureChunks(chunkSize int64) []ChunkInfo {
	if len(n.Chunks) > 0 {
		return n.Chunks
	}
	if n.IsDir || n.Size <= 0 {
		return nil
	}
	rawChunkSize := RawChunkSizeFromTarget(chunkSize)
	totalChunks := int((n.Size + rawChunkSize - 1) / rawChunkSize)
	if totalChunks == 0 {
		totalChunks = 1
	}
	chunks := make([]ChunkInfo, 0, totalChunks)
	for i := 1; i <= totalChunks; i++ {
		curSize := rawChunkSize
		if i == totalChunks {
			curSize = n.Size - int64(i-1)*rawChunkSize
		}
		chunks = append(chunks, ChunkInfo{
			PartIndex: i,
			TotalPart: totalChunks,
			Size:      curSize,
			SHA256:    n.SHA256,
		})
	}
	n.Chunks = chunks
	return chunks
}

type MasterIndex struct {
	Version      int64                      `json:"version"`       // 递增版本号 (v1, v2...)
	LastUpdated  time.Time                  `json:"last_updated"`  // 最后提交时间
	NextSeq      string                     `json:"next_seq"`      // 旧版递增序列号 (仅用于兼容读取早期历史网盘数据)
	RootPath     string                     `json:"root_path"`     // 根目录标识
	Files        map[string]FileNode        `json:"files"`         // Key: VirtualPath，映射文件节点
	ThumbVolumes map[string]ThumbVolumeInfo `json:"thumb_volumes"` // 缩略图分卷表
	ActiveThumb  string                     `json:"active_thumb"`  // 当前正在写入的缩略图卷ID
}

// TaskStage 传输任务执行阶段枚举
type TaskStage string

const (
	StageScanning    TaskStage = "1/5 扫描与预处理"
	StageCompress    TaskStage = "2/5 本地打包与切片"
	StageEncrypt     TaskStage = "3/5 强加密计算"
	StageUploading   TaskStage = "4/5 邮件追加写入"
	StageSharing     TaskStage = "SMTP 推送至对方"
	StageCommitIdx   TaskStage = "5/5 提交主索引"
	StageDownloading TaskStage = "下载与解密中"
	StageVerify      TaskStage = "落盘与哈希核验"
	StageCompleted   TaskStage = "已完成"
	StagePaused      TaskStage = "已暂停"
	StageFailed      TaskStage = "传输失败"
)

// TransferTask 可观测传输任务状态
type TransferTask struct {
	TaskID          string    `json:"task_id"`
	FileID          string    `json:"file_id,omitempty"`          // 上传文件唯一标识 (file_<unix_nano>)，用于中断精准闭环清理残留
	VirtualPath     string    `json:"virtual_path"`
	FileName        string    `json:"file_name"`
	TotalSize       int64     `json:"total_size"`
	Transferred     int64     `json:"transferred"`
	ProgressPct     float64   `json:"progress_pct"`
	Stage           TaskStage `json:"stage"`
	StageDetail     string    `json:"stage_detail"`
	CurrentChunk    int       `json:"current_chunk"`
	TotalChunks     int       `json:"total_chunks"`
	CurrentSeq      string    `json:"current_seq"`
	PriorityChunk   int       `json:"priority_chunk,omitempty"`   // 用户拖拽即时插队目标分片
	PlayCursor      int       `json:"play_cursor,omitempty"`      // 播放器时间轴锚点
	CompletedChunks []int     `json:"completed_chunks,omitempty"` // 已完成分片
	ActiveChunks    []int     `json:"active_chunks,omitempty"`    // 进行中分片
	RetryingChunks  []int     `json:"retrying_chunks,omitempty"`  // 重试避让中分片
	WritingChunks   []int     `json:"writing_chunks,omitempty"`   // 解密落盘中分片
	SpeedMBs        float64   `json:"speed_mbs"`
	ElapsedSec      float64   `json:"elapsed_sec"`
	RemainSec       float64   `json:"remain_sec"`
	ThrottleWaitMS  int       `json:"throttle_wait_ms"` // 防风控延时倒计时
	Status          string    `json:"status"`           // "running", "paused", "failed", "completed", "cancelled"
	Type            string    `json:"type,omitempty"`   // "upload", "download", "preview", "share"
	TargetEmail     string    `json:"target_email,omitempty"`
	Error           string    `json:"error,omitempty"`
	Logs            []string  `json:"logs"`
	CreatedAt       time.Time `json:"created_at"`
}

// ShareChunkItem 分享分片清单项
type ShareChunkItem struct {
Seq       string `json:"seq"`
	PartIndex int    `json:"part_index"`
	TotalPart int    `json:"total_part"`
	Size      int64  `json:"size"`
	SHA256    string `json:"sha256"`
	Subject   string `json:"subject"`
	FileName  string `json:"file_name,omitempty"` // 显式绑定所属文件名与哈希
}

// ShareFileItem 分享包中的文件清单项
type ShareFileItem struct {

	Name            string           `json:"name"`
	RelPath         string           `json:"rel_path,omitempty"` // 保持文件夹与子文件夹相对层级结构 (如: "myFolder/sub/file2.jpg")
	FileID          string           `json:"file_id,omitempty"`
	Size            int64            `json:"size"`
	SHA256          string           `json:"sha256"`
	CipherKeyBase64 string `json:"cipher_key_base64,omitempty"` // 专属信封密钥
	Chunks          []ShareChunkItem `json:"chunks"`
	ThumbBase64     string           `json:"thumb_base64,omitempty"`
}

// ShareManifest 一次性分享提货单清单
type ShareManifest struct {
	ShareID     string          `json:"share_id"`
	SenderEmail string          `json:"sender_email"`
	SenderNick  string          `json:"sender_nick"`
	CreatedAt   int64           `json:"created_at"`
	Files       []ShareFileItem `json:"files"`
	Dirs        []string        `json:"dirs,omitempty"` // 保持空文件夹或目录层级
}

// HealthCheckReport 一致性对账报告
type HealthCheckReport struct {
	TotalMailsScanned int        `json:"total_mails_scanned"`
	MasterIndexVer    int64      `json:"master_index_ver"`
	OrphanFiles       []FileNode `json:"orphan_files"`
	OrphanChunksCount int        `json:"orphan_chunks_count"`
	OrphanChunkSeqs   []uint32   `json:"orphan_chunk_seqs,omitempty"` // 云端残留无主切片邮件序列号
	IncompleteFiles   []string   `json:"incomplete_files"`
	GhostEntries      []string   `json:"ghost_entries"`
	Message           string     `json:"message"`
}
