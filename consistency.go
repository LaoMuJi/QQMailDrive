package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ConsistencyChecker 云端一致性对账器（以云端邮件为唯一权威）
type ConsistencyChecker struct {
	engine *StorageEngine
	client *IMAPClient
}

type ParsedChunkMail struct {
	Header   MailHeaderSummary
	Fid      string
	Name     string
	VPath    string
	Size     int64
	PartSize int64
	MTime    int64
	PartIdx  int
	TotPart  int
	Hash     string
	Seq      string
}

func NewConsistencyChecker(engine *StorageEngine, client *IMAPClient) *ConsistencyChecker {
	return &ConsistencyChecker{
		engine: engine,
		client: client,
	}
}

// RunHealthCheck 执行全盘快速一致性扫描（只读 Header，几秒内完成，杜绝误判）
func (c *ConsistencyChecker) RunHealthCheck() (*HealthCheckReport, error) {
	configLock.RLock()
	curBox := GlobalConfig.StorageFolder
	configLock.RUnlock()
	if curBox != "" {
		_ = c.client.EnsureAndSelectFolder(curBox)
	}
	headers, err := c.client.FetchAllHeadersFast()
	if err != nil {
		return nil, fmt.Errorf("拉取邮箱元数据失败: %w", err)
	}

	report := &HealthCheckReport{
		TotalMailsScanned: len(headers),
		MasterIndexVer:    c.engine.masterIndex.Version,
	}

	// 1. 结构化解析云端所有分片邮件与索引纳管关联
	cloudFidChunks := make(map[string][]ParsedChunkMail)  // Key: fid
	cloudHashChunks := make(map[string][]ParsedChunkMail) // Key: hash12
	cloudBundles := make(map[string]bool)               // Key: bundleSeq
	var orphanChunkSeqs []uint32

	activeRefIDs := make(map[string]bool)
	activeRefHashes := make(map[string]bool)
	activeRefSeqs := make(map[string]bool)

	c.engine.indexLock.RLock()
	for _, node := range c.engine.masterIndex.Files {
		if !node.IsDir {
			if node.ID != "" {
				activeRefIDs[node.ID] = true
				if strings.HasPrefix(node.ID, "copy_") {
					if idx := strings.LastIndex(node.ID, "_"); idx != -1 {
						activeRefIDs[node.ID[idx+1:]] = true
					}
				}
			}
			if node.ShareID != "" {
				activeRefIDs[node.ShareID] = true
			}
			if node.SHA256 != "" {
				activeRefHashes[node.SHA256] = true
				if len(node.SHA256) >= 12 {
					activeRefHashes[node.SHA256[:12]] = true
				}
			}
			for _, ch := range node.Chunks {
				if ch.SHA256 != "" {
					activeRefHashes[ch.SHA256] = true
					if len(ch.SHA256) >= 12 {
						activeRefHashes[ch.SHA256[:12]] = true
					}
				}
				if ch.Seq != "" {
					activeRefSeqs[ch.Seq] = true
				}
			}
		}
	}
	c.engine.indexLock.RUnlock()

	for _, h := range headers {
		if strings.HasPrefix(h.Subject, "[QMD:DATA]|") || strings.HasPrefix(h.Subject, "[QMD:SHARE:DATA]|") {
			parts := strings.Split(h.Subject, "|")
			fid := ""
			if len(parts) >= 2 {
				fid = parts[1]
			}

			// 从 X-Drive-Meta 或主题还原切片属性
			parsed := ParsedChunkMail{Header: h, Fid: fid}
			if len(parts) >= 4 {
				var pIdx, tot int
				if n, _ := fmt.Sscanf(parts[2], "%d-%d", &pIdx, &tot); n == 2 {
					parsed.PartIdx = pIdx
					parsed.TotPart = tot
					parsed.Hash = parts[3]
				} else if n, _ := fmt.Sscanf(parts[3], "%d-%d", &pIdx, &tot); n == 2 {
					parsed.PartIdx = pIdx
					parsed.TotPart = tot
					if len(parts) >= 5 {
						parsed.Hash = parts[4]
					} else {
						parsed.Hash = parts[2]
					}
				}
			}
			if h.XDriveMeta != "" {
				if metaBytes, err := base64.StdEncoding.DecodeString(h.XDriveMeta); err == nil {
					var m map[string]interface{}
					if json.Unmarshal(metaBytes, &m) == nil {
						if f, ok := m["fid"].(string); ok && f != "" {
							parsed.Fid = f
							fid = f
						} else if s, ok := m["share_id"].(string); ok && s != "" {
							parsed.Fid = s
							fid = s
						}
						if n, ok := m["name"].(string); ok && n != "" {
							parsed.Name = n
						}
						if idx, ok := m["idx"].(float64); ok {
							parsed.PartIdx = int(idx)
						}
						if tot, ok := m["tot"].(float64); ok {
							parsed.TotPart = int(tot)
						}
						if s, ok := m["seq"].(string); ok {
							parsed.Seq = s
						}
						if ha, ok := m["hash"].(string); ok {
							parsed.Hash = ha
						}
						if vp, ok := m["vpath"].(string); ok {
							parsed.VPath = vp
						}
						if sz, ok := m["size"].(float64); ok {
							parsed.Size = int64(sz)
						}
						if psz, ok := m["psz"].(float64); ok {
							parsed.PartSize = int64(psz)
						}
						if mt, ok := m["mtime"].(float64); ok {
							parsed.MTime = int64(mt)
						}
					}
				}
			}
			if fid != "" {
				cloudFidChunks[fid] = append(cloudFidChunks[fid], parsed)
			}
			if len(parsed.Hash) >= 12 {
				cloudHashChunks[parsed.Hash[:12]] = append(cloudHashChunks[parsed.Hash[:12]], parsed)
			}

			// 检查是否受当前主索引纳管：未被任何文件引用的切片判定为无主残留邮件
			isReferenced := false
			if activeRefIDs[fid] {
				isReferenced = true
			} else if parsed.Hash != "" && activeRefHashes[parsed.Hash] {
				isReferenced = true
			} else if parsed.Seq != "" && activeRefSeqs[parsed.Seq] {
				isReferenced = true
			} else {
				for hsh := range activeRefHashes {
					if strings.Contains(h.Subject, hsh) {
						isReferenced = true
						break
					}
				}
			}

			if !isReferenced {
				orphanChunkSeqs = append(orphanChunkSeqs, h.SeqNum)
			}
		} else if strings.HasPrefix(h.Subject, "[QMD:BUNDLE]|") {
			parts := strings.Split(h.Subject, "|")
			if len(parts) >= 2 {
				cloudBundles[parts[1]] = true
			}
		}
	}

	// 2. 检查主索引记录在云端是否真实存在（严格防误判，只有分片全无才判定幽灵条目）
	c.engine.indexLock.Lock()
	for vPath, node := range c.engine.masterIndex.Files {
		if node.IsDir {
			continue
		}
		if node.StorageType == StorageTypeChunked {
			chunks, exists := cloudFidChunks[node.ID]
			if (!exists || len(chunks) == 0) && node.ShareID != "" {
				chunks, exists = cloudFidChunks[node.ShareID]
			}
			if (!exists || len(chunks) == 0) && node.SHA256 != "" {
				chunks, exists = cloudFidChunks[node.SHA256]
			}
			if (!exists || len(chunks) == 0) && len(node.SHA256) >= 12 {
				chunks, exists = cloudHashChunks[node.SHA256[:12]]
			}
			// 自动自愈兼容：若为带 copy_ 前缀的历史副本节点，提取真实基础 ID 对齐并修复主索引
			if (!exists || len(chunks) == 0) && strings.HasPrefix(node.ID, "copy_") {
				if idx := strings.LastIndex(node.ID, "_"); idx != -1 {
					baseID := node.ID[idx+1:]
					if bChunks, bExists := cloudFidChunks[baseID]; bExists && len(bChunks) > 0 {
						chunks = bChunks
						exists = true
						repaired := node
						repaired.ID = baseID
						repaired.ThumbItemID = baseID
						c.engine.masterIndex.Files[vPath] = repaired
					}
				}
			}
			// 自动自愈兼容：若发现根目录下存在由历史对账误补录生成的 /share_<digits> 节点，且空间中已有该分享的正式归档文件，则自动剔除多余的根占位条目
			if strings.HasPrefix(node.VirtualPath, "/share_") && !strings.Contains(strings.TrimPrefix(node.VirtualPath, "/share_"), "/") && !strings.Contains(node.Name, ".") {
				hasFormalNode := false
				for otherVPath, otherNode := range c.engine.masterIndex.Files {
					if otherVPath != vPath && !otherNode.IsDir && (otherNode.IsSharedIn || otherNode.ShareID != "") {
						if (node.SHA256 != "" && otherNode.SHA256 == node.SHA256) || otherNode.ShareID == node.ID || otherNode.ID == node.ID {
							hasFormalNode = true
							break
						}
					}
				}
				if hasFormalNode {
					c.engine.Log(fmt.Sprintf("🧹 [对账自愈] 自动清理历史误补录的冗余分享占位条目: %s", vPath))
					delete(c.engine.masterIndex.Files, vPath)
					continue
				}
			}
			if !exists || len(chunks) == 0 {
				// 云端没有任何一封该文件的切片邮件，才列为幽灵条目
				report.GhostEntries = append(report.GhostEntries, vPath)
			}
		} else if node.StorageType == StorageTypeBundled {
			if node.BundleSeq != "" && !cloudBundles[node.BundleSeq] {
				report.GhostEntries = append(report.GhostEntries, vPath)
			}
		}
	}
	c.engine.indexLock.Unlock()

	// 3. 检查云端是否有未在主索引登记的完好文件或半残切片
	for fid, partList := range cloudFidChunks {
		var existingNode *FileNode
		c.engine.indexLock.RLock()
		for _, node := range c.engine.masterIndex.Files {
			if node.IsDir {
				continue
			}
			matched := false
			if node.ID == fid {
				matched = true
			} else if node.ShareID != "" && node.ShareID == fid {
				matched = true
			} else if node.SHA256 != "" && node.SHA256 == fid {
				matched = true
			} else if strings.HasPrefix(node.ID, "copy_") {
				if idx := strings.LastIndex(node.ID, "_"); idx != -1 && node.ID[idx+1:] == fid {
					matched = true
				}
			} else if strings.HasPrefix(fid, "copy_") {
				if idx := strings.LastIndex(fid, "_"); idx != -1 && fid[idx+1:] == node.ID {
					matched = true
				}
			}

			// 进一步比对分片哈希（防止分享切片或不同 ID 的同源分片被误报为孤儿）
			if !matched && len(partList) > 0 {
				for _, p := range partList {
					if p.Hash != "" {
						if node.SHA256 != "" && (node.SHA256 == p.Hash || strings.HasPrefix(node.SHA256, p.Hash) || strings.HasPrefix(p.Hash, node.SHA256)) {
							matched = true
							break
						}
						for _, ch := range node.Chunks {
							if ch.SHA256 != "" && (ch.SHA256 == p.Hash || strings.HasPrefix(ch.SHA256, p.Hash) || strings.HasPrefix(p.Hash, ch.SHA256)) {
								matched = true
								break
							}
						}
					}
					if matched {
						break
					}
				}
			}

			if matched {
				n := node
				existingNode = &n
				break
			}
		}
		c.engine.indexLock.RUnlock()

		// 未录入索引，或者之前录入但破损（如大小为0），且云端存在切片
		if (existingNode == nil || existingNode.Size == 0) && len(partList) > 0 {
			sort.Slice(partList, func(i, j int) bool {
				return partList[i].PartIdx < partList[j].PartIdx
			})

			totParts := partList[0].TotPart
			if totParts <= 0 {
				totParts = len(partList)
			}
			fileName := partList[0].Name
			if fileName == "" {
				fileName = fid
			}

			if len(partList) == totParts {
				var chunks []ChunkInfo
				var calcSize int64
				for _, p := range partList {
					pSz := p.PartSize
					if pSz <= 0 {
						pSz = 52428799
					}
					calcSize += pSz
					chunks = append(chunks, ChunkInfo{
						Seq:       p.Seq,
						PartIndex: p.PartIdx,
						TotalPart: p.TotPart,
						Size:      pSz,
						SHA256:    p.Hash,
					})
				}

				totSize := partList[0].Size
				if totSize <= 0 {
					totSize = calcSize
				}

				vPath := partList[0].VPath
				if vPath == "" {
					vPath = "/" + fileName
				}

				modTime := time.Now()
				if partList[0].MTime > 0 {
					modTime = time.Unix(partList[0].MTime, 0)
				} else if partList[0].Header.Date != "" {
					if t, err := time.Parse(time.RFC1123Z, partList[0].Header.Date); err == nil {
						modTime = t
					} else if t, err := time.Parse("Mon, 2 Jan 2006 15:04:05 -0700", partList[0].Header.Date); err == nil {
						modTime = t
					}
				}

				orphanNode := FileNode{
					ID:          fid,
					Name:        fileName,
					VirtualPath: vPath,
					Size:        totSize,
					SHA256:      partList[0].Hash,
					StorageType: StorageTypeChunked,
					Chunks:      chunks,
					ModTime:     modTime,
				}
				report.OrphanFiles = append(report.OrphanFiles, orphanNode)
			} else {
				report.IncompleteFiles = append(report.IncompleteFiles,
					fmt.Sprintf("文件: %s (需 %d 片，实际云端有 %d 片)", fileName, totParts, len(partList)))
			}
		}
	}

	report.OrphanChunkSeqs = orphanChunkSeqs
	report.OrphanChunksCount = len(orphanChunkSeqs)
	report.Message = fmt.Sprintf("对账扫描完毕，共审查 %d 封邮件", len(headers))
	return report, nil
}

// RepairOrphans 补录完好孤儿文件（直接恢复至根目录，保持完整 Chunks 数据结构）
func (c *ConsistencyChecker) RepairOrphans(orphans []FileNode) error {
	c.engine.indexLock.Lock()
	for _, o := range orphans {
		// 自动补齐中间所有父级文件夹，保持目录树结构
		parts := strings.Split(strings.Trim(o.VirtualPath, "/"), "/")
		if len(parts) > 1 {
			curDir := ""
			for i := 0; i < len(parts)-1; i++ {
				curDir += "/" + parts[i]
				if _, exists := c.engine.masterIndex.Files[curDir]; !exists {
					c.engine.masterIndex.Files[curDir] = FileNode{
						ID:          fmt.Sprintf("dir_%d_%d", time.Now().UnixNano(), i),
						Name:        parts[i],
						VirtualPath: curDir,
						IsDir:       true,
						ModTime:     o.ModTime,
					}
				}
			}
		}

		if existing, exists := c.engine.masterIndex.Files[o.VirtualPath]; !exists || existing.Size == 0 {
			c.engine.masterIndex.Files[o.VirtualPath] = o
		}
	}
	c.engine.masterIndex.Version++
	c.engine.masterIndex.LastUpdated = time.Now()
	c.engine.indexLock.Unlock()

	return c.engine.SyncMasterIndexToCloud()
}

// RepairPurgeGhosts 仅清理真正断链的幽灵记录
func (c *ConsistencyChecker) RepairPurgeGhosts(ghostPaths []string) error {
	c.engine.indexLock.Lock()
	for _, p := range ghostPaths {
		delete(c.engine.masterIndex.Files, p)
	}
	c.engine.masterIndex.Version++
	c.engine.masterIndex.LastUpdated = time.Now()
	c.engine.indexLock.Unlock()

	return c.engine.SyncMasterIndexToCloud()
}

// RepairPurgeOrphanChunks 彻底物理清除云端已无主残留的无用切片邮件 (EXPUNGE)
func (c *ConsistencyChecker) RepairPurgeOrphanChunks(seqs []uint32) error {
	if len(seqs) == 0 {
		return nil
	}
	c.engine.Log(fmt.Sprintf("🗑️ [对账清理] 正在向邮箱发送物理清除指令，彻底抹除 %d 封无主残留切片邮件...", len(seqs)))
	return c.client.DeleteAndExpunge(seqs)
}
