package main

import (
	"bytes"
	"encoding/gob"
	"os"
	"path/filepath"
	"sync"
)

const thumbDBFile = "cache/thumbs.db"

var (
	thumbDBMu     sync.RWMutex
	thumbDBLoaded bool
	thumbDBMap    = make(map[string][]byte)
	thumbMemMap   = make(map[string][]byte)
	noThumbMap    = make(map[string]bool)
)

// HasNoThumb 判断此文件是否已被判定为无缩略图或解密损坏 (负缓存，0ms直接阻断后续重复拉取)
func (e *StorageEngine) HasNoThumb(sha256Key string) bool {
	if sha256Key == "" {
		return false
	}
	thumbDBMu.RLock()
	defer thumbDBMu.RUnlock()
	return noThumbMap[sha256Key]
}

// MarkNoThumb 记录此文件无有效缩略图 (负缓存命中)，防止向云端重复发起漫长无效的 IMAP 请求
func (e *StorageEngine) MarkNoThumb(sha256Key string) {
	if sha256Key == "" {
		return
	}
	thumbDBMu.Lock()
	noThumbMap[sha256Key] = true
	thumbDBMu.Unlock()
}

func loadThumbDBLocked() {
	if thumbDBLoaded {
		return
	}
	thumbDBLoaded = true
	data, err := os.ReadFile(filepath.FromSlash(thumbDBFile))
	if err != nil || len(data) == 0 {
		return
	}
	var m map[string][]byte
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&m); err == nil && m != nil {
		thumbDBMap = m
	}
}

func persistThumbDBLocked() {
	dbPath := filepath.FromSlash(thumbDBFile)
	_ = os.MkdirAll(filepath.Dir(dbPath), 0755)
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(thumbDBMap); err != nil {
		return
	}
	tmpPath := dbPath + ".tmp"
	if err := os.WriteFile(tmpPath, buf.Bytes(), 0644); err != nil {
		return
	}
	_ = os.Rename(tmpPath, dbPath)
}

// ClearMemThumbs 清空进程内临时缩略图缓存（关闭本地缓存开关时立即生效）
func (e *StorageEngine) ClearMemThumbs() {
	thumbDBMu.Lock()
	thumbMemMap = make(map[string][]byte)
	thumbDBMu.Unlock()
}

// GetCachedThumb 统一读取缩略图：开启时优先读本地数据库 cache/thumbs.db，关闭时不读本地库（仅命中本次会话内存或返回未命中以从云端拉取）
func (e *StorageEngine) GetCachedThumb(sha256Key string) ([]byte, bool) {
	if sha256Key == "" {
		return nil, false
	}
	configLock.RLock()
	useLocalDB := GlobalConfig.IsThumbLocalCacheEnabled()
	configLock.RUnlock()

	if useLocalDB {
		thumbDBMu.Lock()
		loadThumbDBLocked()
		b, ok := thumbDBMap[sha256Key]
		thumbDBMu.Unlock()
		if ok && len(b) > 0 {
			cp := make([]byte, len(b))
			copy(cp, b)
			return cp, true
		}
		return nil, false
	}

	thumbDBMu.RLock()
	b, ok := thumbMemMap[sha256Key]
	thumbDBMu.RUnlock()
	if ok && len(b) > 0 {
		cp := make([]byte, len(b))
		copy(cp, b)
		return cp, true
	}
	return nil, false
}

// SaveCachedThumb 统一写入缩略图：开启时持久化到本地数据库 cache/thumbs.db，关闭时仅存当前会话内存不落盘
func (e *StorageEngine) SaveCachedThumb(sha256Key string, data []byte) {
	if sha256Key == "" || len(data) == 0 {
		return
	}
	cp := make([]byte, len(data))
	copy(cp, data)

	configLock.RLock()
	useLocalDB := GlobalConfig.IsThumbLocalCacheEnabled()
	configLock.RUnlock()

	thumbDBMu.Lock()
	delete(noThumbMap, sha256Key)
	if useLocalDB {
		loadThumbDBLocked()
		thumbDBMap[sha256Key] = cp
		persistThumbDBLocked()
	} else {
		thumbMemMap[sha256Key] = cp
	}
	thumbDBMu.Unlock()
}
