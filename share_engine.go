package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

var shareReceiverMu sync.Mutex

// ResolveSameNameConflict 同名文件冲突处理规范：
// 1. 若全网盘已存在完全相同哈希的文件 -> 返回 duplicateNode (可直接秒传)
// 2. 若目标路径已存在同名但哈希不同的文件 -> 将老文件重命名为 "原名_YYYYMMDD_HHMMSS.ext"，腾出原始路径给新文件
func (e *StorageEngine) ResolveSameNameConflict(vPath, newFileHash string) (duplicateNode *FileNode, finalVPath string) {
	e.indexLock.Lock()
	defer e.indexLock.Unlock()

	finalVPath = vPath

	// 1. 全局哈希去重比对 (全盘秒传检测)
	if newFileHash != "" {
		for _, f := range e.masterIndex.Files {
			if !f.IsDir && f.SHA256 != "" && strings.EqualFold(f.SHA256, newFileHash) {
				dup := f
				duplicateNode = &dup
				break
			}
		}
	}

	// 2. 目标路径同名检测
	existingNode, exists := e.masterIndex.Files[vPath]
	if !exists {
		return duplicateNode, finalVPath
	}

	// 若同名且哈希完全一致，直接走秒传逻辑
	if existingNode.SHA256 != "" && strings.EqualFold(existingNode.SHA256, newFileHash) {
		duplicateNode = &existingNode
		return duplicateNode, finalVPath
	}

	// 若同名但内容不同 -> 严格执行【改老保留新】：老文件追加时间戳
	dir := path.Dir(existingNode.VirtualPath)
	ext := filepath.Ext(existingNode.Name)
	base := strings.TrimSuffix(existingNode.Name, ext)
	timestampStr := time.Now().Format("20060102_150405")

	newOldName := fmt.Sprintf("%s_%s%s", base, timestampStr, ext)
	newOldVPath := path.Join(dir, newOldName)
	if dir == "/" {
		newOldVPath = "/" + newOldName
	}
	newOldVPath = filepath.ToSlash(newOldVPath)

	// 重命名老文件条目
	delete(e.masterIndex.Files, vPath)
	existingNode.Name = newOldName
	existingNode.VirtualPath = newOldVPath
	e.masterIndex.Files[newOldVPath] = existingNode

	e.LogInfo("ENGINE", fmt.Sprintf("⚠️ [同名版本归档] 检测到同名但内容不同文件，老文件自动重命名为: %s", newOldName))
	return duplicateNode, finalVPath
}

func (e *StorageEngine) ensureDirRecursiveLocked(vPath string) {
	parts := strings.Split(strings.Trim(vPath, "/"), "/")
	cur := ""
	for i := 0; i < len(parts)-1; i++ {
		cur += "/" + parts[i]
		if _, ok := e.masterIndex.Files[cur]; !ok {
			e.masterIndex.Files[cur] = FileNode{
				ID:          fmt.Sprintf("dir_%d_%d", time.Now().UnixNano(), i),
				Name:        parts[i],
				VirtualPath: cur,
				IsDir:       true,
				ModTime:     time.Now(),
			}
		}
	}
}

// EnsureDirRecursive 递归确保虚拟路径的所有父级目录在主索引中存在
func (e *StorageEngine) EnsureDirRecursive(vPath string) {
	e.indexLock.Lock()
	defer e.indexLock.Unlock()
	e.ensureDirRecursiveLocked(vPath)
}

// MoveFileToFolder 支持跨隔离空间的文件和文件夹物理迁移 (同步确定性双端事务：分片MOVE + 目标空间主索引增量录入 + 原空间主索引剔除)
func (e *StorageEngine) MoveFileToFolder(vPath, targetFolder, targetDir string) error {
	vPath = strings.TrimSpace(vPath)
	targetFolder = strings.TrimSpace(targetFolder)
	targetDir = strings.TrimSpace(targetDir)
	if targetDir == "" {
		targetDir = "/"
	}
	if targetDir != "/" && strings.HasSuffix(targetDir, "/") {
		targetDir = strings.TrimSuffix(targetDir, "/")
	}

	actualTarget := e.imapClient.FindActualMailboxName(targetFolder)
	canonicalTarget := ToCanonicalFolder(targetFolder)

	configLock.RLock()
	currentFolder := GlobalConfig.StorageFolder
	configLock.RUnlock()

	// 1. 若目标空间恰好是当前空间，则退化为本空间的普通内部移动
	if canonicalTarget == currentFolder || targetFolder == ToDisplayFolder(currentFolder) {
		return e.MoveItems([]string{vPath}, targetDir)
	}

	// 2. 跨隔离空间物理迁移：提取待迁移节点及所有子项
	e.indexLock.RLock()
	rootNode, exists := e.masterIndex.Files[vPath]
	if !exists {
		e.indexLock.RUnlock()
		return fmt.Errorf("原文件/目录不存在: %s", vPath)
	}

	nodesToMove := []FileNode{rootNode}
	if rootNode.IsDir {
		prefix := vPath + "/"
		for p, child := range e.masterIndex.Files {
			if strings.HasPrefix(p, prefix) {
				nodesToMove = append(nodesToMove, child)
			}
		}
	}
	e.indexLock.RUnlock()

	e.LogInfo("ENGINE", fmt.Sprintf("📦 [跨空间迁移] 开始将 %s (共 %d 项) 从空间 [%s] 迁移至 [%s] (目标路径: %s)...", 
		rootNode.Name, len(nodesToMove), ToDisplayFolder(currentFolder), ToDisplayFolder(targetFolder), targetDir))

	// 步骤 A: 定位所有关联文件的分片邮件并通过 IMAP MOVE 移至目标隔离空间
	var fileIDs []string
	for _, n := range nodesToMove {
		if !n.IsDir && n.ID != "" {
			fileIDs = append(fileIDs, n.ID)
		}
	}

	_ = e.imapClient.EnsureAndSelectFolder(currentFolder)
	if len(nodesToMove) > 0 {
		var seqs []uint32
		headers, hErr := e.imapClient.FetchAllHeadersFast()
		if hErr == nil {
			seqMap := make(map[uint32]bool)
			for _, n := range nodesToMove {
				if !n.IsDir {
					cSeqs := e.LocateChunkSeqsForNode(&n, headers)
					for _, s := range cSeqs {
						seqMap[s] = true
					}
				}
			}
			for s := range seqMap {
				seqs = append(seqs, s)
			}
		}

		if len(seqs) > 0 {
			if err := e.imapClient.MoveMessages(seqs, currentFolder, actualTarget); err != nil {
				return fmt.Errorf("云端分片邮件迁移至目标空间失败: %w", err)
			}
			e.LogInfo("ENGINE", fmt.Sprintf("✔ [分片迁移] 成功将 %d 封切片邮件转移至目标空间 [%s]", len(seqs), ToDisplayFolder(actualTarget)))
		}
	}

	// 步骤 B: 接入目标空间，拉取并更新目标空间的 MasterIndex 主索引
	targetClient := NewIMAPClient(GlobalConfig.IMAPServer, GlobalConfig.Email, GlobalConfig.IMAPAuthCode)
	if err := targetClient.Connect(); err != nil {
		return fmt.Errorf("接入目标隔离空间失败: %w", err)
	}
	defer targetClient.Close()

	if err := targetClient.EnsureAndSelectFolder(actualTarget); err != nil {
		return fmt.Errorf("选区目标隔离空间失败: %w", err)
	}

	tHeaders, _ := targetClient.FetchAllHeadersFast()
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

	var targetIdx MasterIndex
	if bestIdx > 0 {
		if raw, rErr := targetClient.FetchRawMessageBody(bestIdx); rErr == nil {
			enc := extractAttachmentFromMIME(raw)
			if plain, dErr := e.DecryptData(enc); dErr == nil {
				_ = json.Unmarshal(plain, &targetIdx)
			}
		}
	}
	if targetIdx.Files == nil {
		targetIdx.Files = make(map[string]FileNode)
	}

	// 计算迁移项目在新空间中的基础路径
	var newRootPath string
	if targetDir == "/" {
		newRootPath = "/" + rootNode.Name
	} else {
		newRootPath = targetDir + "/" + rootNode.Name
	}

	// 将所有项目录入目标空间索引
	for _, n := range nodesToMove {
		targetNode := n
		if n.VirtualPath == vPath {
			targetNode.VirtualPath = newRootPath
		} else {
			subRel := n.VirtualPath[len(vPath)+1:]
			targetNode.VirtualPath = newRootPath + "/" + subRel
		}
		targetIdx.Files[targetNode.VirtualPath] = targetNode

		// 补齐目标空间可能缺失的父目录节点
		parts := strings.Split(strings.Trim(targetNode.VirtualPath, "/"), "/")
		curDir := ""
		for i := 0; i < len(parts)-1; i++ {
			curDir += "/" + parts[i]
			if _, ok := targetIdx.Files[curDir]; !ok {
				targetIdx.Files[curDir] = FileNode{
					ID:          fmt.Sprintf("dir_%d_%d", time.Now().UnixNano(), i),
					Name:        parts[i],
					VirtualPath: curDir,
					IsDir:       true,
					ModTime:     time.Now(),
				}
			}
		}
	}

	targetIdx.Version = bestVer + 1
	targetIdx.LastUpdated = time.Now()

	// 提交目标空间新主索引
	data, _ := json.Marshal(targetIdx)
	encData, _ := e.EncryptData(data)
	subj := fmt.Sprintf("[QMD:INDEX]|v%06d|%d", targetIdx.Version, time.Now().Unix())
	meta := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"ver":%d}`, targetIdx.Version)))
	mime := e.BuildMIMEMessage(subj, meta, "master_index.dat", encData)
	if err := targetClient.AppendDirectMessage(actualTarget, subj, meta, mime); err != nil {
		return fmt.Errorf("写入目标空间主索引失败: %w", err)
	}
	e.LogInfo("ENGINE", fmt.Sprintf("✔ [目标索引更新] 成功为目标空间 [%s] 录入 %d 个节点 (主索引升至 v%d)", 
		ToDisplayFolder(actualTarget), len(nodesToMove), targetIdx.Version))

	// 步骤 C: 从当前原空间主索引中移除已迁移项目并立即同步云端
	e.indexLock.Lock()
	for _, n := range nodesToMove {
		delete(e.masterIndex.Files, n.VirtualPath)
	}
	e.masterIndex.Version++
	e.masterIndex.LastUpdated = time.Now()
	e.indexLock.Unlock()
	_ = e.SyncMasterIndexToCloud()
	e.LogInfo("ENGINE", fmt.Sprintf("✔ [原索引对齐] 已从原空间剔除 %d 个节点并更新原空间云端主索引", len(nodesToMove)))

	// 步骤 D: 确保主长连接重新锁定回当前工作空间
	_ = e.imapClient.EnsureAndSelectFolder(currentFolder)

	e.LogInfo("ENGINE", fmt.Sprintf("🎉 [跨空间迁移完成] 成功将 %s 移至 [%s]:%s 并完成双端索引对齐！", 
		rootNode.Name, ToDisplayFolder(targetFolder), newRootPath))
	return nil
}

// CopyFileToFolder 支持跨隔离空间的文件和文件夹物理复制 (同步确定性事务：云端分片原子 COPY + 目标空间主索引增量录入 + 原空间保持原样)
func (e *StorageEngine) CopyFileToFolder(vPath, targetFolder, targetDir string) error {
	vPath = strings.TrimSpace(vPath)
	targetFolder = strings.TrimSpace(targetFolder)
	targetDir = strings.TrimSpace(targetDir)
	if targetDir == "" {
		targetDir = "/"
	}
	if targetDir != "/" && strings.HasSuffix(targetDir, "/") {
		targetDir = strings.TrimSuffix(targetDir, "/")
	}

	actualTarget := e.imapClient.FindActualMailboxName(targetFolder)
	canonicalTarget := ToCanonicalFolder(targetFolder)

	configLock.RLock()
	currentFolder := GlobalConfig.StorageFolder
	configLock.RUnlock()

	// 1. 若目标空间恰好是当前空间，则退化为本空间的普通复制
	if canonicalTarget == currentFolder || targetFolder == ToDisplayFolder(currentFolder) {
		return e.CopyItems([]string{vPath}, targetDir)
	}

	// 2. 跨隔离空间物理复制：提取待复制节点及所有子项
	e.indexLock.RLock()
	rootNode, exists := e.masterIndex.Files[vPath]
	if !exists {
		e.indexLock.RUnlock()
		return fmt.Errorf("原文件/目录不存在: %s", vPath)
	}

	nodesToCopy := []FileNode{rootNode}
	if rootNode.IsDir {
		prefix := vPath + "/"
		for p, child := range e.masterIndex.Files {
			if strings.HasPrefix(p, prefix) {
				nodesToCopy = append(nodesToCopy, child)
			}
		}
	}
	e.indexLock.RUnlock()

	e.LogInfo("ENGINE", fmt.Sprintf("📋 [跨空间复制] 开始将 %s (共 %d 项) 从空间 [%s] 克隆至 [%s] (目标路径: %s)...", 
		rootNode.Name, len(nodesToCopy), ToDisplayFolder(currentFolder), ToDisplayFolder(targetFolder), targetDir))

	// 步骤 A: 定位所有关联文件的分片邮件并通过 IMAP COPY 克隆至目标隔离空间（原邮件不删除）
	var fileIDs []string
	for _, n := range nodesToCopy {
		if !n.IsDir && n.ID != "" {
			fileIDs = append(fileIDs, n.ID)
		}
	}

	_ = e.imapClient.EnsureAndSelectFolder(currentFolder)
	if len(nodesToCopy) > 0 {
		var seqs []uint32
		headers, hErr := e.imapClient.FetchAllHeadersFast()
		if hErr == nil {
			seqMap := make(map[uint32]bool)
			for _, n := range nodesToCopy {
				if !n.IsDir {
					cSeqs := e.LocateChunkSeqsForNode(&n, headers)
					for _, s := range cSeqs {
						seqMap[s] = true
					}
				}
			}
			for s := range seqMap {
				seqs = append(seqs, s)
			}
		}

		if len(seqs) > 0 {
			if err := e.imapClient.CopyMessages(seqs, currentFolder, actualTarget); err != nil {
				return fmt.Errorf("云端分片邮件克隆至目标空间失败: %w", err)
			}
			e.LogInfo("ENGINE", fmt.Sprintf("✔ [分片复制] 成功将 %d 封切片邮件克隆至目标空间 [%s]", len(seqs), ToDisplayFolder(actualTarget)))
		}
	}

	// 步骤 B: 接入目标空间，拉取并更新目标空间的 MasterIndex 主索引
	targetClient := NewIMAPClient(GlobalConfig.IMAPServer, GlobalConfig.Email, GlobalConfig.IMAPAuthCode)
	if err := targetClient.Connect(); err != nil {
		return fmt.Errorf("接入目标隔离空间失败: %w", err)
	}
	defer targetClient.Close()

	if err := targetClient.EnsureAndSelectFolder(actualTarget); err != nil {
		return fmt.Errorf("选区目标隔离空间失败: %w", err)
	}

	tHeaders, _ := targetClient.FetchAllHeadersFast()
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

	var targetIdx MasterIndex
	if bestIdx > 0 {
		if raw, rErr := targetClient.FetchRawMessageBody(bestIdx); rErr == nil {
			enc := extractAttachmentFromMIME(raw)
			if plain, dErr := e.DecryptData(enc); dErr == nil {
				_ = json.Unmarshal(plain, &targetIdx)
			}
		}
	}
	if targetIdx.Files == nil {
		targetIdx.Files = make(map[string]FileNode)
	}

	// 计算复制项目在新空间中的基础路径
	var newRootPath string
	destName := rootNode.Name
	if targetDir == "/" {
		newRootPath = "/" + destName
	} else {
		newRootPath = targetDir + "/" + destName
	}

	// 目标空间同名冲突处理 (自动附加 "- 副本")
	if _, clash := targetIdx.Files[newRootPath]; clash {
		var nameWithoutExt, ext string
		if rootNode.IsDir {
			nameWithoutExt = rootNode.Name
			ext = ""
		} else {
			ext = filepath.Ext(rootNode.Name)
			nameWithoutExt = strings.TrimSuffix(rootNode.Name, ext)
		}
		for i := 1; i <= 999; i++ {
			var cand string
			if i == 1 {
				cand = fmt.Sprintf("%s - 副本%s", nameWithoutExt, ext)
			} else {
				cand = fmt.Sprintf("%s - 副本 (%d)%s", nameWithoutExt, i, ext)
			}
			p := targetDir + "/" + cand
			if targetDir == "/" {
				p = "/" + cand
			}
			if _, exists := targetIdx.Files[p]; !exists {
				destName = cand
				newRootPath = p
				break
			}
		}
	}

	// 将所有项目录入目标空间索引
	for _, n := range nodesToCopy {
		targetNode := n
		if n.VirtualPath == vPath {
			targetNode.Name = destName
			targetNode.VirtualPath = newRootPath
		} else {
			subRel := n.VirtualPath[len(vPath)+1:]
			targetNode.VirtualPath = newRootPath + "/" + subRel
		}
		targetIdx.Files[targetNode.VirtualPath] = targetNode

		// 补齐目标空间可能缺失的父目录节点
		parts := strings.Split(strings.Trim(targetNode.VirtualPath, "/"), "/")
		curDir := ""
		for i := 0; i < len(parts)-1; i++ {
			curDir += "/" + parts[i]
			if _, ok := targetIdx.Files[curDir]; !ok {
				targetIdx.Files[curDir] = FileNode{
					ID:          fmt.Sprintf("dir_%d_%d", time.Now().UnixNano(), i),
					Name:        parts[i],
					VirtualPath: curDir,
					IsDir:       true,
					ModTime:     time.Now(),
				}
			}
		}
	}

	targetIdx.Version = bestVer + 1
	targetIdx.LastUpdated = time.Now()

	// 提交目标空间新主索引
	data, _ := json.Marshal(targetIdx)
	encData, _ := e.EncryptData(data)
	subj := fmt.Sprintf("[QMD:INDEX]|v%06d|%d", targetIdx.Version, time.Now().Unix())
	meta := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"ver":%d}`, targetIdx.Version)))
	mime := e.BuildMIMEMessage(subj, meta, "master_index.dat", encData)
	if err := targetClient.AppendDirectMessage(actualTarget, subj, meta, mime); err != nil {
		return fmt.Errorf("写入目标空间主索引失败: %w", err)
	}
	e.LogInfo("ENGINE", fmt.Sprintf("✔ [目标索引更新] 成功为目标空间 [%s] 录入 %d 个复制节点 (主索引升至 v%d)", 
		ToDisplayFolder(actualTarget), len(nodesToCopy), targetIdx.Version))

	// 步骤 C: 原空间主索引完全保持不变！确保主长连接重新锁定回当前工作空间
	_ = e.imapClient.EnsureAndSelectFolder(currentFolder)

	e.LogInfo("ENGINE", fmt.Sprintf("🎉 [跨空间复制完成] 成功将 %s 克隆至 [%s]:%s 并保持原空间完整！", 
		rootNode.Name, ToDisplayFolder(targetFolder), newRootPath))
	return nil
}

// ShareFilesStream  发送分享任务：通过 SMTP TLS 465 向对方邮箱依次推送分片并在最后发送自毁提货单清单
func (e *StorageEngine) ShareFilesStream(task *TransferTask, paths []string, toEmail string) error {
	toEmail = strings.TrimSpace(toEmail)
	if toEmail == "" {
		return fmt.Errorf("目标接收邮箱不能为空")
	}
	if !strings.Contains(toEmail, "@") {
		toEmail += "@qq.com"
	}

	task.Type = "share"
	task.TargetEmail = toEmail
	task.Status = "running"
	task.Stage = StageSharing
	task.StageDetail = fmt.Sprintf("正在准备向 %s 推送文件...", toEmail)

	// 收集待分享文件 (保持文件夹与子文件夹完整相对层级)
	var shareItems []ShareFileItem
	var shareDirs []string
	var totalBytes int64
	type itemWithRel struct {
		node    FileNode
		relPath string
	}

	e.indexLock.RLock()
	for _, p := range paths {
		if node, exists := e.masterIndex.Files[p]; exists {
			var targetNodes []itemWithRel
			if node.IsDir {
				shareDirs = append(shareDirs, node.Name)
				prefix := node.VirtualPath + "/"
				for subP, sub := range e.masterIndex.Files {
					if strings.HasPrefix(subP, prefix) {
						subRel := subP[len(prefix):]
						rel := node.Name + "/" + subRel
						if sub.IsDir {
							shareDirs = append(shareDirs, rel)
						} else {
							targetNodes = append(targetNodes, itemWithRel{node: sub, relPath: rel})
						}
					}
				}
			} else {
				targetNodes = append(targetNodes, itemWithRel{node: node, relPath: node.Name})
			}

			chunkSize := GlobalConfig.ChunkSizeBytes
			if chunkSize <= 0 {
				chunkSize = 52428799
			}
			for _, tn := range targetNodes {
				ck := tn.node.CipherKeyBase64
				if ck == "" {
					ck = base64.StdEncoding.EncodeToString(e.DeriveFileKey(tn.node.SHA256))
				}
				tn.node.EnsureChunks(chunkSize)
				var chunkItems []ShareChunkItem
				for _, c := range tn.node.Chunks {
					chunkItems = append(chunkItems, ShareChunkItem{
						Seq:       c.Seq,
						PartIndex: c.PartIndex,
						TotalPart: c.TotalPart,
						Size:      c.Size,
						SHA256:    c.SHA256,
						FileName:  tn.node.Name,
					})
				}
				thumbB64 := ""
				if isMediaExt(tn.node.Name) {
					if b, ok := e.GetCachedThumb(tn.node.SHA256); ok && len(b) > 0 {
						thumbB64 = base64.StdEncoding.EncodeToString(b)
					}
				}
				shareItems = append(shareItems, ShareFileItem{
					Name:            tn.node.Name,
					RelPath:         tn.relPath,
					FileID:          tn.node.ID,
					Size:            tn.node.Size,
					SHA256:          tn.node.SHA256,
					CipherKeyBase64: ck,
					Chunks:          chunkItems,
					ThumbBase64:     thumbB64,
				})
				totalBytes += tn.node.Size
			}
		}
	}
	e.indexLock.RUnlock()

	if len(shareItems) == 0 {
		task.Status = "failed"
		task.Error = "未找到可分享的文件"
		return fmt.Errorf("未找到可分享的文件")
	}

	task.TotalSize = totalBytes
	shareID := fmt.Sprintf("share_%d", time.Now().UnixNano())

	totalChunksCount := 0
	for _, fi := range shareItems {
		totalChunksCount += len(fi.Chunks)
	}
	task.TotalChunks = totalChunksCount

	e.LogInfo("SHARE", fmt.Sprintf("🚀 开始执行文件分享任务: 发往 %s (共 %d 个文件, %d 个分片, 总计 %s)", toEmail, len(shareItems), totalChunksCount, formatBytes(totalBytes)))

	// 初始化临时下载客户端拉取本地加密分片数据，然后通过 SMTP 发送给对方
	senderNick := GlobalConfig.UserNick
	if senderNick == "" {
		senderNick = strings.Split(GlobalConfig.Email, "@")[0]
	}

	currentChunkCounter := 0
	startTime := time.Now()

	// 1. 依次推送各个分片邮件
	for _, fileItem := range shareItems {
		// 定位该文件在云端的分片邮件
		e.indexLock.RLock()
		node := e.masterIndex.Files["/"+fileItem.Name]
		for _, f := range e.masterIndex.Files {
			if f.Name == fileItem.Name && f.Size == fileItem.Size {
				node = f
				break
			}
		}
		e.indexLock.RUnlock()

		// 检索分片邮件
		headers, _ := e.imapClient.FetchAllHeadersFast()
		mailMap := make(map[int]uint32)
		prefix := "[QMD:DATA]|" + node.ID + "|"
		for _, h := range headers {
			matched := false
			for _, chunk := range fileItem.Chunks {
				partTag := fmt.Sprintf("|%d-", chunk.PartIndex)
				if len(chunk.SHA256) >= 12 && strings.Contains(h.Subject, chunk.SHA256[:12]) && strings.Contains(h.Subject, partTag) {
					mailMap[chunk.PartIndex] = h.SeqNum
					matched = true
					break
				}
			}
			if !matched && (strings.HasPrefix(h.Subject, prefix) || strings.Contains(h.Subject, "|"+node.ID+"|")) {
				parts := strings.Split(h.Subject, "|")
				if len(parts) >= 3 {
					var pIdx int
					fmt.Sscanf(parts[2], "%d-", &pIdx)
					if pIdx > 0 {
						mailMap[pIdx] = h.SeqNum
					}
				}
			}
		}

		for cIdx, chunk := range fileItem.Chunks {
			if task.Status == "cancelled" {
				e.LogInfo("SHARE", fmt.Sprintf("✔ [%s] 分享已由用户主动取消", fileItem.Name))
				return nil
			}
			currentChunkCounter++
			task.CurrentChunk = currentChunkCounter
			task.ActiveChunks = []int{currentChunkCounter}
			task.RetryingChunks = nil
			task.StageDetail = fmt.Sprintf("正在推送 [%s] 分片 %d/%d 至 %s", fileItem.Name, currentChunkCounter, totalChunksCount, toEmail)

			// 从云端拉取原附件原始加密流
			mailSeq := mailMap[chunk.PartIndex]
			if mailSeq == 0 {
				task.Status = "failed"
				task.Error = fmt.Sprintf("未在云端找到分片 #%d 对应邮件", chunk.PartIndex)
				e.LogError("SHARE", fmt.Sprintf("❌ 未在云端找到分片 #%d 对应邮件", chunk.PartIndex))
				return fmt.Errorf("未在云端找到分片 #%d 对应邮件", chunk.PartIndex)
			}

			rawMsg, err := e.imapClient.FetchRawMessageBody(mailSeq)
			if err != nil {
				task.Status = "failed"
				task.Error = fmt.Sprintf("读取本地分片失败: %v", err)
				e.LogError("SHARE", fmt.Sprintf("❌ 分享分片读取失败: %v", err))
				return err
			}

			encAttachment := extractAttachmentFromMIME(rawMsg)
			if len(encAttachment) == 0 {
				task.Status = "failed"
				return fmt.Errorf("分片附件解析为空")
			}

			// 构造发送给对方的主题规范
			partSubj := fmt.Sprintf("[QMD:SHARE:DATA]|%s|%d-%d|%s", shareID, chunk.PartIndex, chunk.TotalPart, chunk.SHA256[:12])
			fileItem.Chunks[cIdx].Subject = partSubj

			metaJSON := fmt.Sprintf(`{"share_id":"%s","name":"%s","idx":%d,"tot":%d,"sender":"%s","nick":"%s"}`,
				shareID, fileItem.Name, chunk.PartIndex, chunk.TotalPart, GlobalConfig.Email, senderNick)
			metaB64 := base64.StdEncoding.EncodeToString([]byte(metaJSON))

			// 通过 SMTP 发送
			upStart := time.Now()
			var chunkSent int64
			progressCb := func(n int) bool {
				if task.Status == "cancelled" {
					return false
				}
				chunkSent += int64(n)
				task.Transferred += int64(float64(n) * 0.75)
				if task.TotalSize > 0 {
					task.ProgressPct = float64(task.Transferred) / float64(task.TotalSize) * 100.0
					if task.ProgressPct > 99.0 {
						task.ProgressPct = 99.0
					}
				}
				dur := time.Since(upStart).Seconds()
				if dur > 0 {
					task.SpeedMBs = (float64(chunkSent) / (1024 * 1024)) / dur
				}
				return true
			}

			var curThumbBody []byte
			if chunk.PartIndex == 1 && isMediaExt(fileItem.Name) {
				curThumbBody = e.GetOrFetchEncryptedThumbnail(&FileNode{
					Name:            fileItem.Name,
					SHA256:          fileItem.SHA256,
					CipherKeyBase64: fileItem.CipherKeyBase64,
				})
			}

			if err := e.smtpClient.SendStreamMessageWithProgress(toEmail, partSubj, metaB64, chunk.Seq+".7z", encAttachment, curThumbBody, progressCb); err != nil {
				task.Status = "failed"
				task.Error = err.Error()
				e.LogError("SHARE", fmt.Sprintf("❌ SMTP 发送分片邮件失败: %v", err))
				return err
			}

			task.CompletedChunks = append(task.CompletedChunks, currentChunkCounter)
			task.ActiveChunks = nil
			task.RetryingChunks = nil
			e.LogInfo("SHARE", fmt.Sprintf("✔ 成功推送分片 #%d 至 %s", currentChunkCounter, toEmail))

			// 防风控平滑延时
			delay := GlobalConfig.RequestDelayMS
			if delay > 0 && currentChunkCounter < totalChunksCount {
				time.Sleep(time.Duration(delay) * time.Millisecond)
			}
		}
	}

	if task.Status == "cancelled" {
		e.LogInfo("SHARE", "✔ 分享已被用户主动取消，终止投递提货清单")
		return nil
	}

	// 2. 最终发送极轻量的提货单清单邮件 (携带该文件专属信封密钥)
	// 提货单 JSON 瘦身：移除每一个文件的冗余 chunks 数组，直接实现几百字节极致轻量清单
	slimShareItems := make([]ShareFileItem, len(shareItems))
	for i, it := range shareItems {
		slimShareItems[i] = it
		slimShareItems[i].Chunks = nil
		slimShareItems[i].ThumbBase64 = ""
	}

	manifest := ShareManifest{
		ShareID:     shareID,
		SenderEmail: GlobalConfig.Email,
		SenderNick:  senderNick,
		CreatedAt:   time.Now().Unix(),
		Files:       slimShareItems,
		Dirs:        shareDirs,
	}

	task.RetryingChunks = nil
	task.ActiveChunks = nil
	task.StageDetail = "📬 正在投递提货清单..."
	manifestBytes, _ := json.Marshal(manifest)
	manifestSubj := fmt.Sprintf("[QMD:SHARE:MANIFEST]|%s|%d", shareID, len(shareItems))

	if err := e.smtpClient.SendManifestMessage(toEmail, manifestSubj, string(manifestBytes)); err != nil {
		task.Status = "failed"
		task.Error = "提货单邮件发送失败: " + err.Error()
		return err
	}
	go func(sid string) {
		time.Sleep(1500 * time.Millisecond)
		e.cleanupSentShareMails(sid)
	}(shareID)

	task.CompletedChunks = nil
	for i := 1; i <= totalChunksCount; i++ {
		task.CompletedChunks = append(task.CompletedChunks, i)
	}
	task.Stage = StageCompleted
	task.Status = "completed"
	task.StageDetail = fmt.Sprintf("✔ 分享成功送达 %s", toEmail)
	task.ProgressPct = 100.0
	task.ElapsedSec = time.Since(startTime).Seconds()
	e.LogInfo("SHARE", fmt.Sprintf("🎉 文件分享任务圆满完成！已向 %s 成功投递全部 %d 个分片及专属提货清单邮件！", toEmail, totalChunksCount))
	return nil
}

// StartShareAutoReceiver 后台守护协程：静默轮询收件箱与垃圾箱，全自动处理收到的好友分享
func (e *StorageEngine) StartShareAutoReceiver() {
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		for range ticker.C {
			if !GlobalConfig.IsConfigured() || e.imapClient == nil || !e.imapClient.connected {
				continue
			}
			e.ScanAndAutoImportShares()
		}
	}()
}

// ScanAndAutoImportShares 核心执行：扫描提货单、核验分片完整性、IMAP MOVE 归档、主索引合并、自毁清单邮件
func (e *StorageEngine) ScanAndAutoImportShares() {
	// 若当前有正在运行的上传/下载/预览任务，后台分享扫描自动避让，绝不抢占主 IMAP 连接或切换目录！
	e.tasksLock.RLock()
	for _, t := range e.tasks {
		if t != nil && (t.Status == "running" || t.Status == "pending") {
			e.tasksLock.RUnlock()
			return
		}
	}
	e.tasksLock.RUnlock()

	shareReceiverMu.Lock()
	defer shareReceiverMu.Unlock()

	// 扫描结束后，无条件切回当前工作隔离目录，确保下载与预览随时处于正确选区！
	defer func() {
		configLock.RLock()
		curF := GlobalConfig.StorageFolder
		curEmail := GlobalConfig.Email
		configLock.RUnlock()
		if curF != "" && e.imapClient != nil && e.imapClient.connected && strings.EqualFold(e.imapClient.email, curEmail) {
			_ = e.imapClient.SelectFolderOnly(curF)
		}
	}()

	// 1. 扫描收件箱 INBOX
	headers, err := e.fetchFolderHeadersFast("INBOX")
	if err != nil {
		return
	}

	// 第一阶段：纯只读一次性提取所有提货单正文与 JSON（期间严禁任何 MOVE / EXPUNGE，保证 INBOX 序号 100% 稳定不失效）
	type parsedManifestItem struct {
		subject  string
		shareID  string
		manifest ShareManifest
	}
	var parsedManifests []parsedManifestItem

	for _, h := range headers {
		if !strings.HasPrefix(h.Subject, "[QMD:SHARE:MANIFEST]|") {
			continue
		}
		parts := strings.Split(h.Subject, "|")
		if len(parts) < 2 {
			continue
		}
		shareID := parts[1]

		// 读取提货单正文
		rawMsg, err := e.imapClient.FetchRawMessageBody(h.SeqNum)
		if err != nil {
			continue
		}

		// 解析 Manifest JSON (支持 RFC 5322 标准报头折行多行拼合 + 正文回退提取)
		var manifest ShareManifest
		var b64Buf strings.Builder
		inMetaHeader := false

		rawLines := strings.Split(string(rawMsg), "\n")
		for _, l := range rawLines {
			trimmed := strings.TrimRight(l, "\r")
			lowerTrimmed := strings.ToLower(trimmed)

			if strings.HasPrefix(lowerTrimmed, "x-drive-meta:") {
				inMetaHeader = true
				b64Buf.WriteString(strings.TrimSpace(trimmed[len("x-drive-meta:"):]))
				continue
			}
			if inMetaHeader {
				if strings.HasPrefix(trimmed, " ") || strings.HasPrefix(trimmed, "\t") {
					b64Buf.WriteString(strings.TrimSpace(trimmed))
					continue
				} else {
					inMetaHeader = false
				}
			}
		}

		if b64Buf.Len() > 0 {
			if decoded, dErr := base64.StdEncoding.DecodeString(b64Buf.String()); dErr == nil {
				_ = json.Unmarshal(decoded, &manifest)
			}
		}

		if manifest.ShareID == "" {
			bodyStr := string(rawMsg)
			if jsonStart := strings.Index(bodyStr, "{\"share_id\":"); jsonStart != -1 {
				subJSON := bodyStr[jsonStart:]
				if jsonEnd := strings.LastIndex(subJSON, "}"); jsonEnd != -1 {
					_ = json.Unmarshal([]byte(subJSON[:jsonEnd+1]), &manifest)
				}
			}
		}

		if manifest.ShareID == "" || len(manifest.Files) == 0 {
			continue
		}

		parsedManifests = append(parsedManifests, parsedManifestItem{
			subject:  h.Subject,
			shareID:  shareID,
			manifest: manifest,
		})
	}

	if len(parsedManifests) == 0 {
		return
	}

	// 严格锁定归档目标为专属隔离空间「收到的文件」(绝不在当前常规网盘空间内混入)
	targetStorageFolder := e.imapClient.FindActualMailboxName("收到的文件")
	if targetStorageFolder == "" {
		targetStorageFolder = "QMD_收到的文件"
	}

	extractPartTag := func(subj string) string {
		p := strings.Split(subj, "|")
		if len(p) >= 4 {
			return p[2] + "|" + p[3]
		}
		if len(p) >= 3 {
			return p[2]
		}
		return subj
	}

	chunkSize := GlobalConfig.ChunkSizeBytes
	if chunkSize <= 0 {
		chunkSize = 52428799
	}
	rawChunkSize := RawChunkSizeFromTarget(chunkSize)

	// 第二阶段：逐个核验分片并 MOVE 归档入库（仅当存在多个包且前包发生了 MOVE 时才顺手刷新 INBOX 序号）
	inboxHeaders := headers
	inboxDirty := false
	var manifestsToDelete []string
	var deadLetterManifests []struct {
		subject string
		shareID string
		age     time.Duration
		got     int
		total   int
		missStr string
	}

	for _, pm := range parsedManifests {
		if inboxDirty {
			if freshHeaders, fErr := e.fetchFolderHeadersFast("INBOX"); fErr == nil {
				inboxHeaders = freshHeaders
				inboxDirty = false
			}
		}

		shareID := pm.shareID
		manifest := pm.manifest
		sharePrefix := fmt.Sprintf("[QMD:SHARE:DATA]|%s|", shareID)

		var partSeqs []uint32
		for _, ph := range inboxHeaders {
			if strings.HasPrefix(ph.Subject, sharePrefix) {
				partSeqs = append(partSeqs, ph.SeqNum)
			}
		}

		totalRequiredParts := 0
		for _, fi := range manifest.Files {
			if len(fi.Chunks) > 0 {
				totalRequiredParts += len(fi.Chunks)
			} else {
				c := int((fi.Size + rawChunkSize - 1) / rawChunkSize)
				if c == 0 {
					c = 1
				}
				totalRequiredParts += c
			}
		}

		targetPartMap := make(map[string]bool)
		targetHeaders, tErr := e.fetchFolderHeadersFast(targetStorageFolder)
		if tErr == nil {
			for _, th := range targetHeaders {
				if strings.HasPrefix(th.Subject, sharePrefix) {
					targetPartMap[extractPartTag(th.Subject)] = true
				}
			}
		}

		allFoundTags := make(map[string]bool)
		for _, ph := range inboxHeaders {
			if strings.HasPrefix(ph.Subject, sharePrefix) {
				allFoundTags[extractPartTag(ph.Subject)] = true
			}
		}
		for tag := range targetPartMap {
			allFoundTags[tag] = true
		}
		combinedCount := len(allFoundTags)

		var totalRecvSize int64
		for _, fi := range manifest.Files {
			totalRecvSize += fi.Size
		}
		displayRecvName := manifest.Files[0].Name
		if len(manifest.Files) > 1 {
			displayRecvName = fmt.Sprintf("%s 等 %d 个文件", manifest.Files[0].Name, len(manifest.Files))
		}
		recvTaskID := "share_recv_" + shareID
		recvSenderNick := manifest.SenderNick
		if recvSenderNick == "" {
			recvSenderNick = "好友"
		}

		getOrCreateRecvTask := func() *TransferTask {
			e.tasksLock.Lock()
			defer e.tasksLock.Unlock()
			rt, exists := e.tasks[recvTaskID]
			if !exists || rt == nil {
				rt = &TransferTask{
					TaskID:      recvTaskID,
					Type:        "share_recv",
					FileName:    displayRecvName,
					VirtualPath: "/" + recvSenderNick + "/" + manifest.Files[0].Name,
					TotalSize:   totalRecvSize,
					TotalChunks: totalRequiredParts,
					Status:      "running",
					CreatedAt:   time.Now(),
				}
				e.tasks[recvTaskID] = rt
			}
			rt.TotalSize = totalRecvSize
			rt.TotalChunks = totalRequiredParts
			return rt
		}

		// 情况 A：全量集齐 (收件箱中的分片 + 目标隔离空间已有分片之和达到要求)
		if combinedCount >= totalRequiredParts {
			rt := getOrCreateRecvTask()
			doneList := make([]int, totalRequiredParts)
			for ci := 0; ci < totalRequiredParts; ci++ {
				doneList[ci] = ci + 1
			}
			e.tasksLock.Lock()
			rt.Status = "running"
			rt.CompletedChunks = doneList
			rt.ActiveChunks = nil
			rt.WritingChunks = []int{totalRequiredParts}
			rt.Transferred = totalRecvSize
			rt.ProgressPct = 99.0
			rt.StageDetail = "📬 分片已收齐，正在归档移入 [收到的文件]..."
			e.tasksLock.Unlock()

			if len(partSeqs) > 0 {
				if err := e.imapClient.MoveMessages(partSeqs, "INBOX", targetStorageFolder); err != nil {
					e.LogError("SHARE", fmt.Sprintf("转移分享分片至「收到的文件」失败: %v", err))
					e.tasksLock.Lock()
					rt.Status = "failed"
					rt.WritingChunks = nil
					rt.Error = err.Error()
					e.tasksLock.Unlock()
					continue
				}
				inboxDirty = true
				e.LogInfo("IMAP", fmt.Sprintf("✔ 成功将 %d 封分享邮件合流移至隔离空间 [%s]", len(partSeqs), ToDisplayFolder(targetStorageFolder)))
			}

			e.LogInfo("SHARE", fmt.Sprintf("📬 捕获到来自 %s (%s) 的完整分享包，正在挂载至隔离空间 [%s]...", manifest.SenderNick, manifest.SenderEmail, ToDisplayFolder(targetStorageFolder)))
			if err := e.ImportShareToReceivedFolder(manifest, nil); err != nil {
				e.LogError("SHARE", fmt.Sprintf("❌ 归档入库失败: %v", err))
				e.tasksLock.Lock()
				rt.Status = "failed"
				rt.WritingChunks = nil
				rt.Error = err.Error()
				e.tasksLock.Unlock()
				continue
			}

			e.tasksLock.Lock()
			rt.Status = "completed"
			rt.Stage = StageCompleted
			rt.WritingChunks = nil
			rt.ActiveChunks = nil
			rt.Transferred = totalRecvSize
			rt.ProgressPct = 100.0
			rt.StageDetail = fmt.Sprintf("来自 %s 的好友分享已存入 [收到的文件] 空间", recvSenderNick)
			e.tasksLock.Unlock()

			// 暂存待自毁清单，待全部提货单均安全归档后再统一删除
			manifestsToDelete = append(manifestsToDelete, pm.subject)
			continue
		}

		// 情况 B：尚未集齐 (combinedCount < totalRequiredParts)
		var shareAge time.Duration
		if manifest.CreatedAt > 0 {
			shareAge = time.Since(time.Unix(manifest.CreatedAt, 0))
		}

		if shareAge > 30*time.Minute {
			e.tasksLock.Lock()
			if rt, ok := e.tasks[recvTaskID]; ok && rt != nil {
				rt.Status = "failed"
				rt.ActiveChunks = nil
				rt.WritingChunks = nil
				rt.Error = "分享包超时残缺"
			}
			e.tasksLock.Unlock()
			var missingDesc []string
			for _, fi := range manifest.Files {
				tot := len(fi.Chunks)
				if tot == 0 {
					tot = int((fi.Size + rawChunkSize - 1) / rawChunkSize)
					if tot == 0 {
						tot = 1
					}
				}
				for p := 1; p <= tot; p++ {
					tag := fmt.Sprintf("%d-%d", p, tot)
					if !allFoundTags[tag] {
						missingDesc = append(missingDesc, fmt.Sprintf("#%d(%s)", p, fi.Name))
						if len(missingDesc) >= 3 {
							break
						}
					}
				}
				if len(missingDesc) >= 3 {
					break
				}
			}
			missStr := strings.Join(missingDesc, ", ")
			if missStr == "" {
				missStr = fmt.Sprintf("缺少 %d 个分片", totalRequiredParts-combinedCount)
			}
			deadLetterManifests = append(deadLetterManifests, struct {
				subject string
				shareID string
				age     time.Duration
				got     int
				total   int
				missStr string
			}{pm.subject, shareID, shareAge, combinedCount, totalRequiredParts, missStr})
			continue
		}

		rt := getOrCreateRecvTask()
		doneList := make([]int, combinedCount)
		for ci := 0; ci < combinedCount; ci++ {
			doneList[ci] = ci + 1
		}
		var activeList []int
		if combinedCount < totalRequiredParts {
			activeList = []int{combinedCount + 1}
		}
		e.tasksLock.Lock()
		rt.Status = "running"
		rt.CompletedChunks = doneList
		rt.ActiveChunks = activeList
		rt.WritingChunks = nil
		if totalRequiredParts > 0 {
			rt.Transferred = int64(float64(totalRecvSize) * float64(combinedCount) / float64(totalRequiredParts))
			rt.ProgressPct = float64(combinedCount) / float64(totalRequiredParts) * 99.0
		}
		rt.StageDetail = fmt.Sprintf("⏳ 正在接收分享 (%d/%d 分片)...", combinedCount, totalRequiredParts)
		e.tasksLock.Unlock()
		e.LogDebug("SHARE", fmt.Sprintf("⏳ 收到分享 [%s]，分片接收中 (%d/%d)...", shareID, combinedCount, totalRequiredParts))
	}

	// 第三阶段：全部安全入库后，统一物理自毁提货单清单邮件，收件箱恢复零垃圾残留
	for _, subj := range manifestsToDelete {
		e.deleteManifestInInbox(subj)
		e.LogInfo("SHARE", fmt.Sprintf("🧹 [提货单自毁] 清单邮件 %s 已从收件箱彻底物理删除！", subj))
	}
	for _, dl := range deadLetterManifests {
		e.LogWarn("SHARE", fmt.Sprintf("⚠️ [残缺分享包超时] 分享包 [%s] 创建于 %v 前已严重超时，已收到 %d/%d 分片，缺失分片: %s。为消除死循环，正在自毁清理收件箱死信清单...",
			dl.shareID, dl.age.Round(time.Minute), dl.got, dl.total, dl.missStr))
		e.deleteManifestInInbox(dl.subject)
		e.LogInfo("SHARE", fmt.Sprintf("🧹 [死信清单自毁] 已成功销毁失效提货单: %s，收件箱恢复正常", dl.subject))
	}
}

func (e *StorageEngine) fetchFolderHeadersFast(folder string) ([]MailHeaderSummary, error) {
	e.imapClient.mu.Lock()
	defer e.imapClient.mu.Unlock()

	if err := e.imapClient.ensureAndSelectFolderInternal(folder); err != nil {
		return nil, err
	}

	tag := e.imapClient.nextTag()
	cmd := fmt.Sprintf("%s FETCH 1:* (BODY.PEEK[HEADER.FIELDS (SUBJECT DATE X-DRIVE-META)])\r\n", tag)
	if _, err := e.imapClient.writer.WriteString(cmd); err != nil {
		return nil, err
	}
	_ = e.imapClient.writer.Flush()

	var summaries []MailHeaderSummary
	var cur *MailHeaderSummary

	for {
		line, err := e.imapClient.reader.ReadLine()
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimSpace(line)

		if strings.HasPrefix(trimmed, "* ") && strings.Contains(trimmed, "FETCH") {
			fields := strings.Fields(trimmed)
			if len(fields) >= 2 {
				var seq uint32
				fmt.Sscanf(fields[1], "%d", &seq)
				if seq > 0 {
					cur = &MailHeaderSummary{SeqNum: seq}
				}
			}
		} else if cur != nil {
			lower := strings.ToLower(line)
			if strings.HasPrefix(lower, "subject:") {
				cur.Subject = strings.TrimSpace(line[8:])
			} else if strings.HasPrefix(lower, "x-drive-meta:") {
				cur.XDriveMeta = strings.TrimSpace(line[13:])
			} else if line == ")" || strings.HasSuffix(line, ")") {
				summaries = append(summaries, *cur)
				cur = nil
			}
		}
		if strings.HasPrefix(trimmed, tag+" OK") || strings.HasPrefix(trimmed, tag+" NO") || strings.HasPrefix(trimmed, tag+" BAD") {
			break
		}
	}
	return summaries, nil
}


// ShareFilesCloudFast 核心实现：基于 wx.mail.qq.com 的纯云端零流量秒级转发 (带全透明日志追踪与严格校验)
func (e *StorageEngine) ShareFilesCloudFast(task *TransferTask, paths []string, toEmail string, webClient *WebmailClient) error {
	toEmail = strings.TrimSpace(toEmail)
	if toEmail == "" {
		return fmt.Errorf("目标接收邮箱不能为空")
	}
	if !strings.Contains(toEmail, "@") {
		toEmail += "@qq.com"
	}

	task.Type = "share"
	task.TargetEmail = toEmail
	task.Status = "running"
	task.Stage = StageSharing
	task.StageDetail = fmt.Sprintf("正在通过腾讯云端机房极速转交至 %s (本地0流量)...", toEmail)

	// 1. 递归收集待分享文件的全部子文件、子目录与完整相对层级元数据
	var shareItems []ShareFileItem
	var shareDirs []string
	var totalBytes int64
	type itemWithRelCloud struct {
		node    FileNode
		relPath string
	}

	e.indexLock.RLock()
	for _, p := range paths {
		if node, exists := e.masterIndex.Files[p]; exists {
			var targetNodes []itemWithRelCloud
			if node.IsDir {
				shareDirs = append(shareDirs, node.Name)
				prefix := node.VirtualPath + "/"
				for subP, sub := range e.masterIndex.Files {
					if strings.HasPrefix(subP, prefix) {
						subRel := subP[len(prefix):]
						rel := node.Name + "/" + subRel
						if sub.IsDir {
							shareDirs = append(shareDirs, rel)
						} else {
							targetNodes = append(targetNodes, itemWithRelCloud{node: sub, relPath: rel})
						}
					}
				}
			} else {
				targetNodes = append(targetNodes, itemWithRelCloud{node: node, relPath: node.Name})
			}

			chunkSize := GlobalConfig.ChunkSizeBytes
			if chunkSize <= 0 {
				chunkSize = 52428799
			}
			for _, tn := range targetNodes {
				ck := tn.node.CipherKeyBase64
				if ck == "" {
					ck = base64.StdEncoding.EncodeToString(e.DeriveFileKey(tn.node.SHA256))
				}
				tn.node.EnsureChunks(chunkSize)
				var chunkItems []ShareChunkItem
				for _, c := range tn.node.Chunks {
					chunkItems = append(chunkItems, ShareChunkItem{
						Seq:       c.Seq,
						PartIndex: c.PartIndex,
						TotalPart: c.TotalPart,
						Size:      c.Size,
						SHA256:    c.SHA256,
						FileName:  tn.node.Name,
					})
				}
				thumbB64 := ""
				if isMediaExt(tn.node.Name) {
					if b, ok := e.GetCachedThumb(tn.node.SHA256); ok && len(b) > 0 {
						thumbB64 = base64.StdEncoding.EncodeToString(b)
					}
				}
				shareItems = append(shareItems, ShareFileItem{
					Name:            tn.node.Name,
					RelPath:         tn.relPath,
					FileID:          tn.node.ID,
					Size:            tn.node.Size,
					SHA256:          tn.node.SHA256,
					CipherKeyBase64: ck,
					Chunks:          chunkItems,
					ThumbBase64:     thumbB64,
				})
				totalBytes += tn.node.Size
			}
		}
	}
	e.indexLock.RUnlock()

	if len(shareItems) == 0 {
		task.Status = "failed"
		task.Error = "未找到可分享的文件"
		e.LogError("SHARE", "❌ 分享失败：未在主索引中找到待分享文件的有效节点！")
		return fmt.Errorf("未找到可分享的文件")
	}

	task.TotalSize = totalBytes
	shareID := fmt.Sprintf("share_%d", time.Now().UnixNano())

	totalChunksCount := 0
	for _, fi := range shareItems {
		totalChunksCount += len(fi.Chunks)
	}
	task.TotalChunks = totalChunksCount

	e.LogInfo("SHARE", fmt.Sprintf("⚡ [云端秒转发启动] 目标: %s (共 %d 个文件, %d 个分片, 总计 %s，本地免下载免上传！)",
		toEmail, len(shareItems), totalChunksCount, formatBytes(totalBytes)))

	// 2. 检索当前隔离空间的 dirid (精准反查，若未找到直接严谨报错，坚决不盲猜)
	configLock.RLock()
	curFolder := GlobalConfig.StorageFolder
	configLock.RUnlock()

	dirID, dErr := webClient.FindDirIDByFolderName(curFolder)
	if dErr != nil {
		e.LogError("SHARE", fmt.Sprintf("❌ [云端秒转终止] 未能锁定空间 [%s] 的云端 dirid: %v", curFolder, dErr))
		task.Status = "failed"
		task.Error = fmt.Sprintf("未能锁定空间 dirid: %v", dErr)
		return dErr
	}

	// 3. 拉取该目录内的邮件列表
	mailList, err := webClient.FetchFolderMailList(dirID)
	if err != nil {
		e.LogError("SHARE", fmt.Sprintf("❌ [云端秒转终止] 拉取空间 (dirid=%s) 邮件列表失败: %v", dirID, err))
		task.Status = "failed"
		task.Error = err.Error()
		return err
	}
	allLoadedFolders := map[string]bool{dirID: true}

	if len(mailList) == 0 {
		e.LogInfo("SHARE", fmt.Sprintf("⚠️ 云端目录 (dirid=%s) 内为空，将开启全空间跨目录分片寻址！", dirID))
	}

	// 4. 依次调用 Web 接口云端极速转发每一个分片
	senderNick := GlobalConfig.UserNick
	if senderNick == "" {
		senderNick = strings.Split(GlobalConfig.Email, "@")[0]
	}

	currentChunkCounter := 0
	startTime := time.Now()

	for _, fileItem := range shareItems {
		targetID := fileItem.FileID
		if targetID == "" {
			e.indexLock.RLock()
			for _, f := range e.masterIndex.Files {
				if f.Name == fileItem.Name && f.Size == fileItem.Size {
					targetID = f.ID
					break
				}
			}
			e.indexLock.RUnlock()
		}
		if strings.HasPrefix(targetID, "copy_") {
			if idx := strings.LastIndex(targetID, "_"); idx != -1 {
				targetID = targetID[idx+1:]
			}
		}

		for cIdx, chunk := range fileItem.Chunks {
			if task.Status == "cancelled" {
				e.LogInfo("SHARE", fmt.Sprintf("✔ [%s] 分享已由用户主动取消", fileItem.Name))
				return nil
			}
			currentChunkCounter++
			task.CurrentChunk = currentChunkCounter
			task.ActiveChunks = []int{currentChunkCounter}
			task.RetryingChunks = nil
			task.StageDetail = fmt.Sprintf("⚡ 正在转交 [%s] 分片 %d/%d 至 %s...", fileItem.Name, currentChunkCounter, totalChunksCount, toEmail)

			partSubj := fmt.Sprintf("[QMD:SHARE:DATA]|%s|%d-%d|%s", shareID, chunk.PartIndex, chunk.TotalPart, chunk.SHA256[:12])
			fileItem.Chunks[cIdx].Subject = partSubj

			// 严格比对提取对应的 mailid 和 fileid
			partTag := fmt.Sprintf("|%d-", chunk.PartIndex)
			var targetMail WebmailMailItem
			matched := false

			for _, item := range mailList {
				// 优先规则：按文件 SHA-256 前缀 + 分片序号精准匹配 (避免同文件分片共用 SHA256 时全部命中列表首条)
				if len(chunk.SHA256) >= 12 && strings.Contains(item.Subject, chunk.SHA256[:12]) && strings.Contains(item.Subject, partTag) {
					targetMail = item
					matched = true
					break
				}
				// 兜底规则：主题中包含该文件ID (或 Seq)，并且包含该分片序号
				if (strings.Contains(item.Subject, targetID) || (chunk.Seq != "" && strings.Contains(item.Subject, chunk.Seq))) &&
					strings.Contains(item.Subject, partTag) {
					targetMail = item
					matched = true
					break
				}
			}

			// 跨空间智能分片寻址：若当前目录未匹配到分片，自动检索其他所有 QMD 隔离空间
			if !matched {
				for fName, otherDirID := range webClient.GetAllQMDFolderDirIDs() {
					if allLoadedFolders[otherDirID] {
						continue
					}
					allLoadedFolders[otherDirID] = true
					e.LogInfo("SHARE", fmt.Sprintf("🔍 [跨空间分片寻址] 当前空间未命中分片 #%d，正在检索空间 [%s] (dirid=%s)...", chunk.PartIndex, fName, otherDirID))
					if otherMails, oErr := webClient.FetchFolderMailList(otherDirID); oErr == nil && len(otherMails) > 0 {
						mailList = append(mailList, otherMails...)
						for _, item := range otherMails {
							if len(chunk.SHA256) >= 12 && strings.Contains(item.Subject, chunk.SHA256[:12]) && strings.Contains(item.Subject, partTag) {
								targetMail = item
								matched = true
								break
							}
							if (strings.Contains(item.Subject, targetID) || (chunk.Seq != "" && strings.Contains(item.Subject, chunk.Seq))) &&
								strings.Contains(item.Subject, partTag) {
								targetMail = item
								matched = true
								break
							}
						}
						if matched {
							e.LogInfo("SHARE", fmt.Sprintf("✔ [跨空间命中] 成功在隔离空间 [%s] 锁定文件分片 #%d 对应邮件！", fName, chunk.PartIndex))
							break
						}
					}
				}
			}

			if !matched {
				e.LogError("SHARE", fmt.Sprintf("❌ [分片未匹配] 分片 #%d 未在云端 (dirid=%s 及其他空间) 匹配到对应邮件！(寻找特征: ID=%s, partTag=%s, seq=%s)",
					chunk.PartIndex, dirID, targetID, partTag, chunk.Seq))
				task.Status = "failed"
				task.Error = fmt.Sprintf("分片 #%d 云端未匹配到附件 fileid", chunk.PartIndex)
				return fmt.Errorf("分片 #%d 云端未匹配到附件 fileid", chunk.PartIndex)
			}

			if targetMail.AttachFileID == "" {
				realName, realFID, getErr := webClient.GetMailAttachmentInfo(targetMail.MailID)
				if getErr == nil && realFID != "" {
					targetMail.AttachName = realName
					targetMail.AttachFileID = realFID
				} else {
					e.LogError("SHARE", fmt.Sprintf("❌ [附件fileid缺失] 邮件 %s (mailid: %s) 获取附件详情失败: %v", targetMail.Subject, targetMail.MailID, getErr))
					task.Status = "failed"
					task.Error = fmt.Sprintf("分片 #%d 邮件附件 fileid 缺失: %v", chunk.PartIndex, getErr)
					return fmt.Errorf("分片 #%d 邮件附件 fileid 缺失: %w", chunk.PartIndex, getErr)
				}
			}

			e.LogInfo("SHARE", fmt.Sprintf("🔗 [分片就绪] 分片 #%d 匹配成功 -> mailid: %s, fileid: %s, 附件名: %s",
				chunk.PartIndex, targetMail.MailID, targetMail.AttachFileID, targetMail.AttachName))

			// 构造邮件正文：如果是媒体文件的第 1 分片，携带加密缩略图密文；否则写入分片描述
			// 确保每个邮件均同时具备正文 (Part 1) 与附件 (Part 2) 完整结构
			chunkBodyContent := ""
			if chunk.PartIndex == 1 && isMediaExt(fileItem.Name) {
				encThumb := e.GetOrFetchEncryptedThumbnail(&FileNode{
					ID:              fileItem.FileID,
					Name:            fileItem.Name,
					Size:            fileItem.Size,
					SHA256:          fileItem.SHA256,
					CipherKeyBase64: fileItem.CipherKeyBase64,
				})
				if len(encThumb) > 0 {
					chunkBodyContent = base64.StdEncoding.EncodeToString(encThumb)
					e.LogInfo("THUMB", fmt.Sprintf("🖼️ [分享附带缩略图] [%s] 已随第 1 分片附带加密缩略图 (%d 字节)", fileItem.Name, len(encThumb)))
				} else {
					e.LogWarn("THUMB", fmt.Sprintf("⚠️ [分享缩略图缺失] [%s] 未能从本地或云端获取到缩略图，第 1 分片将不含缩略图", fileItem.Name))
				}
			}

			onRetry := func(attempt int, backoff time.Duration) {
				task.RetryingChunks = []int{currentChunkCounter}
				task.ActiveChunks = nil
				task.StageDetail = fmt.Sprintf("⏳ 触发频控(-20003)，避让等待 %v 后执行第 %d 次重试...", backoff, attempt)
			}

			// 调用腾讯云端秒转发
			fwdErr := webClient.ForwardChunkCloudFast(toEmail, partSubj, targetMail.MailID, targetMail.AttachName, targetMail.AttachFileID, chunkBodyContent, onRetry)
			if fwdErr != nil {
				e.LogError("SHARE", fmt.Sprintf("❌ [云端转发失败] 分片 #%d 转发请求被腾讯拒绝: %v", chunk.PartIndex, fwdErr))
				task.RetryingChunks = nil
				task.ActiveChunks = nil
				task.Status = "failed"
				task.Error = fwdErr.Error()
				task.StageDetail = "❌ 转发失败: " + fwdErr.Error()
				return fwdErr
			}

			e.LogInfo("SHARE", fmt.Sprintf("✔ [分片直达] 第 %d/%d 个分片已由腾讯机房秒级推向对方邮箱！", currentChunkCounter, totalChunksCount))

			task.CompletedChunks = append(task.CompletedChunks, currentChunkCounter)
			task.ActiveChunks = nil
			task.RetryingChunks = nil
			task.StageDetail = fmt.Sprintf("⚡ 正在转交 [%s] 分片 %d/%d 至 %s...", fileItem.Name, currentChunkCounter, totalChunksCount, toEmail)
			task.Transferred += chunk.Size
			if task.TotalSize > 0 {
				task.ProgressPct = float64(task.Transferred) / float64(task.TotalSize) * 100.0
			}
			// 极速秒发平滑微间隔：保持秒发体验，通过清单 JSON 强绑定文件名与哈希精确定位
			cloudDelay := 200 * time.Millisecond
			time.Sleep(cloudDelay)
		}
	}

	if task.Status == "cancelled" {
		e.LogInfo("SHARE", "✔ 分享已被用户主动取消，终止投递提货清单")
		return nil
	}

	// 5. 投递提货单清单 (微延时确保出站就绪)
	task.RetryingChunks = nil
	task.ActiveChunks = nil
	task.StageDetail = "📬 正在投递提货清单..."
	time.Sleep(200 * time.Millisecond)
	e.LogInfo("SHARE", "📬 [投递提货清单] 正在向对方发送提货单 (携带 AES-256 独立信封解密密钥)...")

	// 提货单 JSON 瘦身：移除每一个文件的冗余 chunks 数组，直接实现几百字节极致轻量清单
	slimShareItems := make([]ShareFileItem, len(shareItems))
	for i, it := range shareItems {
		slimShareItems[i] = it
		slimShareItems[i].Chunks = nil
		slimShareItems[i].ThumbBase64 = ""
	}

	manifest := ShareManifest{
		ShareID:     shareID,
		SenderEmail: GlobalConfig.Email,
		SenderNick:  senderNick,
		CreatedAt:   time.Now().Unix(),
		Files:       slimShareItems,
		Dirs:        shareDirs,
	}

	manifestBytes, _ := json.Marshal(manifest)
	manifestSubj := fmt.Sprintf("[QMD:SHARE:MANIFEST]|%s|%d", shareID, len(shareItems))

	// 按照用户最高铁律指令：提货单必须 100% 严格锁定走底层的 SMTP 协议通道投递！
	// 【特别警告】没有用户明确允许，绝不能用 Webmail 云端直发绕过或兜底！必须正面确保 SMTP 协议通道健康可用并真实检验！
	if err := e.smtpClient.SendManifestMessage(toEmail, manifestSubj, string(manifestBytes)); err != nil {
		e.LogError("SHARE", fmt.Sprintf("❌ [提货单发送失败] %v", err))
		task.Status = "failed"
		task.Error = "提货单发送失败: " + err.Error()
		return err
	}
	e.LogInfo("SHARE", "✔ [提货单送达] 提货清单凭证已通过 SMTP 协议成功送达对方邮箱！")
	go func(sid string) {
		time.Sleep(1500 * time.Millisecond)
		e.cleanupSentShareMails(sid)
	}(shareID)

	task.CompletedChunks = nil
	for i := 1; i <= totalChunksCount; i++ {
		task.CompletedChunks = append(task.CompletedChunks, i)
	}
	task.RetryingChunks = nil
	task.ActiveChunks = nil
	task.Stage = StageCompleted
	task.Status = "completed"
	task.StageDetail = fmt.Sprintf("✔ 极速云端分享成功送达 %s (全程 0 本地流量)", toEmail)
	task.ProgressPct = 100.0
	task.ElapsedSec = time.Since(startTime).Seconds()
	e.LogInfo("SHARE", fmt.Sprintf("🎉 [云端秒转发完成] 成功通过腾讯机房直发全部 %d 个分片至 %s，用时仅 %.1f 秒，本地 0 流量消耗！",
		totalChunksCount, toEmail, task.ElapsedSec))
	return nil
}

// deleteManifestInInbox 在收件箱 INBOX 中精准按主题搜索并彻底物理删除提货单邮件 (切回 INBOX 彻底清除，杜绝误删数据)
func (e *StorageEngine) deleteManifestInInbox(manifestSubject string) {
	e.imapClient.mu.Lock()
	defer e.imapClient.mu.Unlock()

	if err := e.imapClient.ensureAndSelectFolderInternal("INBOX"); err != nil {
		return
	}

	tag := e.imapClient.nextTag()
	cmd := fmt.Sprintf("%s SEARCH SUBJECT %q UNDELETED\r\n", tag, manifestSubject)
	if _, err := e.imapClient.writer.WriteString(cmd); err != nil {
		return
	}
	_ = e.imapClient.writer.Flush()

	var matchedSeqs []uint32
	for {
		line, err := e.imapClient.reader.ReadLine()
		if err != nil {
			break
		}
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "* SEARCH") {
			parts := strings.Fields(trimmed)
			for i := 2; i < len(parts); i++ {
				var seq uint32
				if n, _ := fmt.Sscanf(parts[i], "%d", &seq); n > 0 && seq > 0 {
					matchedSeqs = append(matchedSeqs, seq)
				}
			}
		}
		if strings.HasPrefix(trimmed, tag+" ") {
			break
		}
	}

	if len(matchedSeqs) > 0 {
		var seqStrs []string
		for _, s := range matchedSeqs {
			seqStrs = append(seqStrs, strconv.Itoa(int(s)))
		}
		seqRange := strings.Join(seqStrs, ",")

		delTag := e.imapClient.nextTag()
		delCmd := fmt.Sprintf("%s STORE %s +FLAGS (\\Deleted)\r\n", delTag, seqRange)
		_, _ = e.imapClient.writer.WriteString(delCmd)
		_ = e.imapClient.writer.Flush()
		for {
			line, err := e.imapClient.reader.ReadLine()
			if err != nil || strings.HasPrefix(strings.TrimSpace(line), delTag+" ") {
				break
			}
		}

		expTag := e.imapClient.nextTag()
		_, _ = e.imapClient.writer.WriteString(fmt.Sprintf("%s EXPUNGE\r\n", expTag))
		_ = e.imapClient.writer.Flush()
		for {
			line, err := e.imapClient.reader.ReadLine()
			if err != nil || strings.HasPrefix(strings.TrimSpace(line), expTag+" ") {
				break
			}
		}
	}
}

// ImportShareToReceivedFolder 将收到的分享分片邮件与索引存入专属隔离空间「收到的文件」，绝不污染当前常规网盘空间
func (e *StorageEngine) ImportShareToReceivedFolder(manifest ShareManifest, partSeqs []uint32) error {
	targetStorageFolder := e.imapClient.FindActualMailboxName("收到的文件")
	if targetStorageFolder == "" {
		targetStorageFolder = "QMD_收到的文件"
	}

	configLock.RLock()
	currentFolder := GlobalConfig.StorageFolder
	configLock.RUnlock()

	// 1. 将分片邮件从 INBOX 移动到「收到的文件」专属隔离空间 (仅当 partSeqs 非空时执行)
	if len(partSeqs) > 0 {
		if err := e.imapClient.MoveMessages(partSeqs, "INBOX", targetStorageFolder); err != nil {
			return fmt.Errorf("转移分享邮件至「收到的文件」失败: %w", err)
		}
		e.LogInfo("IMAP", fmt.Sprintf("✔ 成功将 %d 封分享邮件通过 MOVE 移至隔离空间 [%s]", len(partSeqs), ToDisplayFolder(targetStorageFolder)))
	}

	// 2. 构造发件人子目录：发件昵称_发件QQ号 (如 /534130271_534130271)
	// 必须在 昵称_qq号 文件夹下面归档，请勿更改！
	senderNick := SanitizeUserNick(manifest.SenderNick)
	if senderNick == "" {
		senderNick = "好友"
	}
	senderQQ := strings.Split(manifest.SenderEmail, "@")[0]
	folderDirName := fmt.Sprintf("%s_%s", senderNick, senderQQ)
	folderDirName = strings.ReplaceAll(folderDirName, "\\", "_")
	folderDirName = strings.ReplaceAll(folderDirName, "/", "_")
	targetDir := "/" + folderDirName

	isCurrentSpaceReceived := (ToDisplayFolder(currentFolder) == "收到的文件" || currentFolder == targetStorageFolder)

	if isCurrentSpaceReceived {
		// 严密防死锁：在加写锁 e.indexLock.Lock 前后调用无死锁的 ensureDirRecursiveLocked，杜绝不可重入读写锁死锁卡死
		e.indexLock.Lock()
		e.ensureDirRecursiveLocked(targetDir + "/placeholder.txt")
		for _, d := range manifest.Dirs {
			e.ensureDirRecursiveLocked(targetDir + "/" + d + "/placeholder.txt")
		}
		for _, sf := range manifest.Files {
			relPath := sf.RelPath
			if relPath == "" {
				relPath = sf.Name
			}
			targetVPath := targetDir + "/" + relPath
			e.ensureDirRecursiveLocked(targetVPath)

			if oldNode, exists := e.masterIndex.Files[targetVPath]; exists {
				ext := filepath.Ext(oldNode.Name)
				base := strings.TrimSuffix(oldNode.Name, ext)
				dirOfFile := path.Dir(targetVPath)
				newOldName := fmt.Sprintf("%s_%s%s", base, time.Now().Format("20060102_150405"), ext)
				newOldVPath := dirOfFile + "/" + newOldName
				delete(e.masterIndex.Files, targetVPath)
				oldNode.Name = newOldName
				oldNode.VirtualPath = newOldVPath
				e.masterIndex.Files[newOldVPath] = oldNode
			}

			var chunks []ChunkInfo
			for _, sc := range sf.Chunks {
				chunks = append(chunks, ChunkInfo{
					Seq:       sc.Seq,
					PartIndex: sc.PartIndex,
					TotalPart: sc.TotalPart,
					Size:      sc.Size,
					SHA256:    sc.SHA256,
				})
			}

			if sf.ThumbBase64 != "" {
				var b []byte
				rawB64 := sf.ThumbBase64
				if idx := strings.Index(rawB64, ","); idx != -1 {
					rawB64 = rawB64[idx+1:]
				}
				if dec, err := base64.StdEncoding.DecodeString(rawB64); err == nil {
					b = dec
				}
				if len(b) > 0 {
					e.SaveCachedThumb(sf.SHA256, b)
				}
			}
			isMedia := (sf.ThumbBase64 != "" || isMediaExt(sf.Name))
			newNode := FileNode{
				ID:              fmt.Sprintf("share_%d", time.Now().UnixNano()),
				Name:            sf.Name,
				VirtualPath:     targetVPath,
				IsDir:           false,
				Size:            sf.Size,
				ModTime:         time.Now(),
				SHA256:          sf.SHA256,
				StorageType:     StorageTypeChunked,
				Chunks:          chunks,
				CipherKeyBase64: sf.CipherKeyBase64,
				IsSharedIn:      true,
				SharedFrom:      fmt.Sprintf("%s (%s)", senderNick, manifest.SenderEmail),
				ShareID:         manifest.ShareID,
				IsMedia:         isMedia,
			}
			e.masterIndex.Files[targetVPath] = newNode
			e.LogInfo("SHARE", fmt.Sprintf("✔ [入库成功] 来自 %s 的文件已成功挂载入隔离空间 [收到的文件]: %s", senderNick, targetVPath))


		}
		e.masterIndex.Version++
		e.masterIndex.LastUpdated = time.Now()
		e.indexLock.Unlock()
		_ = e.SyncMasterIndexToCloud()
	} else {
		// 当前在其他隔离空间 (如 529481127)：通过独立客户端向「收到的文件」空间主索引录入，绝不污染当前空间
		targetClient := NewIMAPClient(GlobalConfig.IMAPServer, GlobalConfig.Email, GlobalConfig.IMAPAuthCode)
		if err := targetClient.Connect(); err != nil {
			return fmt.Errorf("接入「收到的文件」空间失败: %w", err)
		}
		defer targetClient.Close()

		if err := targetClient.EnsureAndSelectFolder(targetStorageFolder); err != nil {
			return fmt.Errorf("选区「收到的文件」空间失败: %w", err)
		}

		tHeaders, _ := targetClient.FetchAllHeadersFast()
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

		var targetIdx MasterIndex
		if bestIdx > 0 {
			if raw, rErr := targetClient.FetchRawMessageBody(bestIdx); rErr == nil {
				enc := extractAttachmentFromMIME(raw)
				if plain, dErr := e.DecryptData(enc); dErr == nil {
					_ = json.Unmarshal(plain, &targetIdx)
				}
			}
		}
		if targetIdx.Files == nil {
			targetIdx.Files = make(map[string]FileNode)
		}

		// 递归确保目标空间中所有父级与子级目录节点存在
		ensureTargetDirRecursive := func(vPath string) {
			parts := strings.Split(strings.Trim(vPath, "/"), "/")
			curDir := ""
			for i := 0; i < len(parts)-1; i++ {
				curDir += "/" + parts[i]
				if _, ok := targetIdx.Files[curDir]; !ok {
					targetIdx.Files[curDir] = FileNode{
						ID:          fmt.Sprintf("dir_%d_%d", time.Now().UnixNano(), i),
						Name:        parts[i],
						VirtualPath: curDir,
						IsDir:       true,
						ModTime:     time.Now(),
					}
				}
			}
		}

		ensureTargetDirRecursive(targetDir + "/placeholder.txt")
		for _, d := range manifest.Dirs {
			ensureTargetDirRecursive(targetDir + "/" + d + "/placeholder.txt")
		}

		for _, sf := range manifest.Files {
			relPath := sf.RelPath
			if relPath == "" {
				relPath = sf.Name
			}
			targetVPath := targetDir + "/" + relPath
			ensureTargetDirRecursive(targetVPath)

			if oldNode, exists := targetIdx.Files[targetVPath]; exists {
				ext := filepath.Ext(oldNode.Name)
				base := strings.TrimSuffix(oldNode.Name, ext)
				dirOfFile := path.Dir(targetVPath)
				newOldName := fmt.Sprintf("%s_%s%s", base, time.Now().Format("20060102_150405"), ext)
				newOldVPath := dirOfFile + "/" + newOldName
				delete(targetIdx.Files, targetVPath)
				oldNode.Name = newOldName
				oldNode.VirtualPath = newOldVPath
				targetIdx.Files[newOldVPath] = oldNode
			}

			var chunks []ChunkInfo
			for _, sc := range sf.Chunks {
				chunks = append(chunks, ChunkInfo{
					Seq:       sc.Seq,
					PartIndex: sc.PartIndex,
					TotalPart: sc.TotalPart,
					Size:      sc.Size,
					SHA256:    sc.SHA256,
				})
			}

			if sf.ThumbBase64 != "" {
				var b []byte
				rawB64 := sf.ThumbBase64
				if idx := strings.Index(rawB64, ","); idx != -1 {
					rawB64 = rawB64[idx+1:]
				}
				if dec, err := base64.StdEncoding.DecodeString(rawB64); err == nil {
					b = dec
				}
				if len(b) > 0 {
					e.SaveCachedThumb(sf.SHA256, b)
				}
			}
			isMedia := (sf.ThumbBase64 != "" || isMediaExt(sf.Name))
			newNode := FileNode{
				ID:              fmt.Sprintf("share_%d", time.Now().UnixNano()),
				Name:            sf.Name,
				VirtualPath:     targetVPath,
				IsDir:           false,
				Size:            sf.Size,
				ModTime:         time.Now(),
				SHA256:          sf.SHA256,
				StorageType:     StorageTypeChunked,
				Chunks:          chunks,
				CipherKeyBase64: sf.CipherKeyBase64,
				IsSharedIn:      true,
				SharedFrom:      fmt.Sprintf("%s (%s)", senderNick, manifest.SenderEmail),
				ShareID:         manifest.ShareID,
				IsMedia:         isMedia,
			}
			targetIdx.Files[targetVPath] = newNode
			e.LogInfo("SHARE", fmt.Sprintf("✔ [入库成功] 来自 %s 的文件已成功挂载入隔离空间 [收到的文件]: %s", senderNick, targetVPath))
		}

		targetIdx.Version = bestVer + 1
		targetIdx.LastUpdated = time.Now()

		data, _ := json.Marshal(targetIdx)
		encData, _ := e.EncryptData(data)
		subj := fmt.Sprintf("[QMD:INDEX]|v%06d|%d", targetIdx.Version, time.Now().Unix())
		meta := base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"ver":%d}`, targetIdx.Version)))
		mime := e.BuildMIMEMessage(subj, meta, "master_index.dat", encData)
		if err := targetClient.AppendDirectMessage(targetStorageFolder, subj, meta, mime); err != nil {
			return fmt.Errorf("写入「收到的文件」空间主索引失败: %w", err)
		}
		e.SetCachedFolderIndex(targetStorageFolder, targetIdx)
		e.LogInfo("ENGINE", fmt.Sprintf("✔ [隔离空间归档] 成功将分享文件录入「收到的文件」隔离空间 (主索引更新至 v%d)", targetIdx.Version))

		// 切回当前工作空间
		_ = e.imapClient.EnsureAndSelectFolder(currentFolder)
	}

	return nil
}

// cleanupSentShareMails 分享发送完成后通过独立临时连接清理「已发送」中对应本次 shareID 的发件副本，绝不干扰主连接选区
func (e *StorageEngine) cleanupSentShareMails(shareID string) {
	if shareID == "" || !GlobalConfig.IsConfigured() {
		return
	}
	tempClient := NewIMAPClient(GlobalConfig.IMAPServer, GlobalConfig.Email, GlobalConfig.IMAPAuthCode)
	if err := tempClient.Connect(); err != nil {
		return
	}
	defer tempClient.Close()

	sentCandidates := []string{"Sent Messages", "已发送"}
	selectedSent := false
	for _, box := range sentCandidates {
		if err := tempClient.SelectFolderOnly(box); err == nil {
			selectedSent = true
			break
		}
	}
	if !selectedSent {
		return
	}

	seqs, err := tempClient.SearchMessageSeqs(fmt.Sprintf(`SUBJECT "%s"`, shareID))
	if err != nil || len(seqs) == 0 {
		return
	}
	if err := tempClient.DeleteAndExpunge(seqs); err == nil {
		e.LogInfo("SHARE", fmt.Sprintf("🧹 [发件清理] 已从「已发送」物理清除 %d 封本次分享发件副本 (%s)", len(seqs), shareID))
	}
}
