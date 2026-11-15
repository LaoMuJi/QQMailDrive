function formatMediaDuration(sec) {
  if (!sec || isNaN(sec) || sec < 0) sec = 0;
  sec = Math.floor(sec);
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  const pad = n => (n < 10 ? "0" : "") + n;
  if (h > 0) {
    return `${pad(h)}:${pad(m)}:${pad(s)}`;
  }
  return `${pad(m)}:${pad(s)}`;
}

// 浮动窗口平滑拖拽绑定函数 (按住标题栏可在屏幕任意位置移动停靠)
function makeElementDraggable(dragHandle, targetEl) {
  if (!dragHandle || !targetEl) return;
  dragHandle.onmousedown = (e) => {
    if (e.target.tagName === "BUTTON" || e.target.classList.contains("close-x")) return;
    let isDragging = true;
    let startX = e.clientX;
    let startY = e.clientY;
    const rect = targetEl.getBoundingClientRect();
    let origX = rect.left;
    let origY = rect.top;

    targetEl.style.transform = "none";
    targetEl.style.left = `${origX}px`;
    targetEl.style.top = `${origY}px`;

    const onMouseMove = (me) => {
      if (!isDragging) return;
      const dx = me.clientX - startX;
      const dy = me.clientY - startY;
      const newX = Math.max(10, Math.min(window.innerWidth - targetEl.offsetWidth - 10, origX + dx));
      const newY = Math.max(10, Math.min(window.innerHeight - targetEl.offsetHeight - 10, origY + dy));
      targetEl.style.left = `${newX}px`;
      targetEl.style.top = `${newY}px`;
    };

    const onMouseUp = () => {
      isDragging = false;
      document.removeEventListener("mousemove", onMouseMove);
      document.removeEventListener("mouseup", onMouseUp);
    };

    document.addEventListener("mousemove", onMouseMove);
    document.addEventListener("mouseup", onMouseUp);
  };
}

function getGridAvailableWidth() {
  const sidebarEl = document.querySelector(".sidebar");
  if (sidebarEl && sidebarEl.offsetWidth > 100) {
    return Math.max(140, sidebarEl.offsetWidth - 68);
  }
  const container = document.getElementById("sideTransferList");
  if (container && container.clientWidth > 40) {
    return Math.max(140, container.clientWidth - 34);
  }
  return 317;
}

// 颗粒列宽动态均分计算算法（严格遵循设计规则：以 10x10 为基准预算行数，颗粒数固定，各行均分宽度，行列严格垂直对齐，侧边栏拖动动态响应）
function calculateGridColumns(totalChunks, containerWidth) {
  const total = Math.max(1, totalChunks || 1);
  const w = containerWidth || 280;
  const gap = 3;
  const minWidth = 10;
  const maxCols = Math.max(1, Math.floor((w + gap) / (minWidth + gap)));

  // 1. 如果小文件一行就排满了，那就整行均分宽度
  if (total <= maxCols) {
    return total;
  }

  // 2. 如果大文件显示多行，根据宽度先预算按最小 10x10 排列，看看需要多少行
  const totalRows = Math.ceil(total / maxCols);

  // 3. 知道了多少行，颗粒数是固定的，每个颗粒均分一下宽度，行列严格垂直对齐
  let cols = Math.ceil(total / totalRows);
  if (cols > maxCols) cols = maxCols;

  return Math.max(1, cols);
}

// 获取第二行分片已落盘/已上传/已推送进度提示文本
function getChunkProgressText(item) {
  if (item.is_duplicate || (item.stage_detail && item.stage_detail.includes("重复文件"))) {
    return "目录下重复文件";
  }
  const total = Math.max(1, item.total_chunks || 1);
  let done = item.completed_chunks ? item.completed_chunks.length : 0;
  if (item.status === "completed") {
    done = total;
  }
  if (item.type === "upload") {
    if (item.status !== "completed" && (!item.stage || item.stage === "1/5 扫描与预处理" || item.stage === "2/5 本地打包与切片" || (item.stage_detail && (item.stage_detail.includes("扫描") || item.stage_detail.includes("打包") || item.stage_detail.includes("特征"))))) {
      return `${done}/${total} 分片已打包`;
    }
    return `${done}/${total} 分片已上传`;
  }
  if (item.type === "share") {
    return `${done}/${total} 分片已推送`;
  }
  if (item.type === "preview") {
    if (item.status === "completed") {
      return `${total}/${total} 分片已下载完毕`;
    }
    return `${done}/${total} 分片已缓冲`;
  }
  if (item.type === "share_recv") {
    return `${done}/${total} 分片已归档`;
  }
  return `${done}/${total} 分片已落盘`;
}

// 任务对应时间标签获取函数
function getTaskTimeLabel(item) {
  if (item.type === "upload") return "上传用时";
  if (item.type === "share") return "分享用时";
  if (item.type === "preview") return item.status === "completed" ? "准备用时" : "缓冲用时";
  if (item.type === "share_recv") return "接收用时";
  return "下载用时";
}

// 区块化分片进度条渲染函数 (根据分片完成与活跃状态动态变色)
// 严格按固定的实际分片数渲染，每个颗粒方块严格对应 1 个物理切片
function renderChunkBlocks(item) {
  const rawChunkSize = Math.max(1, (configuredChunkSizeBytes || 52428799) - 36);
  const total = Math.max(1, item.total_chunks || Math.ceil((item.total || item.size || 0) / rawChunkSize) || 1);
  const completedSet = new Set(item.completed_chunks || []);
  const writingSet = new Set(item.writing_chunks || []);
  const activeSet = new Set(item.active_chunks || []);

  let html = "";
  for (let i = 1; i <= total; i++) {
    let stateClass = "waiting";
    let title = `切片 #${i} (排队等待)`;

    // 核心修复：当前序切片已完成，或者当前指针已推进到 i 之后，确保前序颗粒绝对不灭！
    const isSequentialDone = (item.current_chunk && i < item.current_chunk && (item.type === "upload" || item.type === "share"));
    const retryingSet = new Set(item.retrying_chunks || []);
    const isRetryingStage = Boolean(item.stage_detail && (item.stage_detail.includes("重试") || item.stage_detail.includes("避让") || item.stage_detail.includes("频控")));

    if (item.status === "completed" || completedSet.has(i) || isSequentialDone) {
      stateClass = "completed";
      title = `切片 #${i} (✔ 已完成)`;
    } else if (item.status === "failed" && (i === (item.current_chunk || 1) || activeSet.has(i))) {
      stateClass = "failed";
      title = `切片 #${i} (❌ 传输失败，可点击重试)`;
    } else if (retryingSet.has(i) || (isRetryingStage && (i === (item.current_chunk || 1) || activeSet.has(i)))) {
      stateClass = "retrying";
      title = `切片 #${i} (⏳ 触发重试/频控避让中)`;
    } else if (writingSet.has(i)) {
      stateClass = "writing";
      title = `切片 #${i} (💿 正在解密落盘)`;
    } else if (activeSet.has(i) || (item.status === "running" && i === (item.current_chunk || 1))) {
      stateClass = "active";
      title = `切片 #${i} (⚡ 正在传输)`;
    }

    html += `<div class="chunk-block ${stateClass}" title="${title}"></div>`;
  }
  return html;
}

// 传输用时精准计时与冻结控制
function getTaskElapsedSec(item) {
  if (item.status === "running") {
    item.elapsed_sec = null;
    item.completed_at = null;
    item.paused_elapsed_sec = null;
    if (!item.start_time) {
      if (item.created_at) {
        const t = typeof item.created_at === "number" ? (item.created_at > 1e11 ? item.created_at : item.created_at * 1000) : new Date(item.created_at).getTime();
        if (!isNaN(t) && t > 0) item.start_time = t;
      }
      if (!item.start_time) item.start_time = Date.now();
    }
    return Math.max(0, (Date.now() - item.start_time - (item.total_paused_ms || 0)) / 1000);
  }

  if (item.status === "paused") {
    if (item.paused_elapsed_sec !== undefined && item.paused_elapsed_sec !== null) {
      return item.paused_elapsed_sec;
    }
    const currentPausedAt = item.paused_at || Date.now();
    const pausedSec = Math.max(0, (currentPausedAt - (item.start_time || currentPausedAt) - (item.total_paused_ms || 0)) / 1000);
    item.paused_elapsed_sec = pausedSec;
    return pausedSec;
  }

  if (item.status === "completed" || item.status === "failed" || item.status === "cancelled") {
    if (item.elapsed_sec !== undefined && item.elapsed_sec !== null && item.elapsed_sec > 0) {
      return item.elapsed_sec;
    }
    const end = item.completed_at || Date.now();
    const st = item.start_time || end;
    item.elapsed_sec = Math.max(0, (end - st - (item.total_paused_ms || 0)) / 1000);
    return item.elapsed_sec;
  }

  if (item.elapsed_sec !== undefined && item.elapsed_sec !== null) {
    return item.elapsed_sec;
  }
  if (!item.start_time) return 0;
  return Math.max(0, (Date.now() - item.start_time - (item.total_paused_ms || 0)) / 1000);
}

// 渲染第 4 排操作按钮
function formatDuration(sec) {
  if (!sec || isNaN(sec) || sec < 0) sec = 0;
  sec = Math.floor(sec);
  const h = Math.floor(sec / 3600);
  const m = Math.floor((sec % 3600) / 60);
  const s = sec % 60;
  if (h > 0) {
    return `${h < 10 ? "0" : ""}${h}:${m < 10 ? "0" : ""}${m}:${s < 10 ? "0" : ""}${s}`;
  }
  return `${m < 10 ? "0" : ""}${m}:${s < 10 ? "0" : ""}${s}`;
}

function renderItemActionButtons(item) {
  if (item.is_system) {
    return `<span class="btn-item-locked" title="系统关键任务，执行中不可取消">🔒 维护中</span>`;
  }
  if (item.status === "failed") {
    return `
      <button class="btn-item-retry" data-id="${item.id}" title="重新加入队列重试此任务"><span>🔄</span><span>重试</span></button>
      <button class="btn-item-cancel" data-id="${item.id}" title="清除此失败记录"><span>✖</span><span>清除</span></button>
    `;
  }
  if (item.stage === "落盘与哈希核验" || (item.stage_detail && item.stage_detail.includes("核验"))) {
    return `<span style="font-size:11px; color:#38bdf8; font-weight:600;">💿 正在落盘组装</span>`;
  }
  if (item.status === "completed") {
    return `
      <button class="btn-item-done" data-id="${item.id}" title="已完成，点击从列表移除此项">
        <span>✔</span><span>完成</span>
      </button>
    `;
  }
  if (item.status === "cancelled") {
    return `
      <button class="btn-item-done btn-item-close" data-id="${item.id}" title="已取消，点击关闭并从列表移除">
        <span>✖</span><span>关闭</span>
      </button>
    `;
  }
  const canPause = (item.type === "upload" || item.type === "download");
  const toggleBtnHtml = canPause ? `
    <button class="btn-item-toggle" data-id="${item.id}" data-backend="${item.backend_id || ''}" title="${item.status === "paused" ? "继续传输" : "暂停传输"}">
      <span class="btn-toggle-icon">${item.status === "paused" ? "▶" : "⏸"}</span><span class="btn-toggle-text">${item.status === "paused" ? "继续" : "暂停"}</span>
    </button>
  ` : "";
  return `
    ${toggleBtnHtml}
    <button class="btn-item-cancel" data-id="${item.id}" title="取消此传输任务并释放资源">
      <span>✖</span><span>取消</span>
    </button>
  `;
}

// 全局 HTML 转义安全工具函数 (防 XSS 与模板异常)
function escapeHtml(str) {
  if (str === null || str === undefined) return "";
  return String(str)
    .replace(/&/g, "&amp;")
    .replace(/</g, "&lt;")
    .replace(/>/g, "&gt;")
    .replace(/"/g, "&quot;")
    .replace(/'/g, "&#039;");
}

// 递归提取拖拽目录与子文件（支持深层嵌套文件夹树解析）
async function extractFilesFromDataTransfer(dataTransfer) {
  const items = dataTransfer.items;
  if (!items || items.length === 0) {
    return Array.from(dataTransfer.files || []);
  }

  const fileList = [];
  async function traverseEntry(entry, path = "") {
    if (entry.isFile) {
      const file = await new Promise((resolve) => {
        entry.file(resolve, () => resolve(null));
      });
      if (file) {
        const relPath = path ? path + "/" + file.name : file.name;
        Object.defineProperty(file, "webkitRelativePath", {
          value: relPath,
          writable: true,
          configurable: true
        });
        fileList.push(file);
      }
    } else if (entry.isDirectory) {
      const dirReader = entry.createReader();
      const readAllEntries = async () => {
        let allEntries = [];
        let batch;
        do {
          batch = await new Promise((resolve) => dirReader.readEntries(resolve, () => resolve([])));
          if (batch && batch.length > 0) {
            allEntries = allEntries.concat(batch);
          }
        } while (batch && batch.length > 0);
        return allEntries;
      };
      try {
        const subEntries = await readAllEntries();
        for (const sub of subEntries) {
          await traverseEntry(sub, path ? path + "/" + entry.name : entry.name);
        }
      } catch (e) {
        console.error("读取目录条目异常:", e);
      }
    }
  }

  // 关键修复：必须在任何 await 异步挂起前，同步提取所有 webkitGetAsEntry 和 files 快照！
  // 否则浏览器在第一个文件 await 完成的瞬间会自动清空 dataTransfer.items，导致只上传第 1 个文件！
  const rawFilesSnapshot = Array.from(dataTransfer.files || []);
  const entriesSnapshot = [];
  for (let i = 0; i < items.length; i++) {
    const item = items[i];
    if (item.webkitGetAsEntry) {
      const entry = item.webkitGetAsEntry();
      if (entry) {
        entriesSnapshot.push(entry);
      }
    } else {
      const f = item.getAsFile();
      if (f) fileList.push(f);
    }
  }

  for (const entry of entriesSnapshot) {
    await traverseEntry(entry);
  }

  return fileList.length > 0 ? fileList : rawFilesSnapshot;
}


// 格式化实时传输字节：小的用 KB，大的用 MB/GB
function formatTransferSize(bytes) {
  if (!bytes || bytes <= 0) return '0 KB';
  if (bytes < 1024 * 1024) {
    return (bytes / 1024).toFixed(1) + ' KB';
  }
  return (bytes / (1024 * 1024)).toFixed(1) + ' MB';
}

// QQMailDrive 前端核心业务逻辑
// 作者: 杀熊 | 版本: v1.2.0

let currentPath = "/";
let filesData = [];
let pollingTimer = null;
let activeTasks = [];
let isConfiguredState = false;
let currentActiveStorageFolder = "";
let isImapConnected = false;
let isHttpConnected = false;
let isRefreshingImap = false;
let isRefreshingHttp = false;
let sessionSkipHttpPrompt = false; // 用户点击“暂不登录”后，在本次程序运行期间后续操作不再弹窗打扰
let standaloneQRPollTimer = null;

// 纯净隔离空间名称格式化 (坚决过滤任何 QMD_ / qmd_ / 其他文件夹/ 前缀，避免前缀泄露至UI)
function cleanFolderDisplayName(folder) {
  if (!folder) return "";
  let s = String(folder).trim();
  s = s.replace(/^(其他文件夹\/)?(QMD_|qmd_)/i, "");
  s = s.replace(/^QMD_/i, "");
  s = s.replace(/^qmd_/i, "");
  return s;
}
let expandedFolders = new Set(["/"]); // 记录当前展开的文件夹树节点
let isListView = true; // 默认采用详细信息列表视图
let crashPromptDismissed = false;

// 检索过滤关键字
let searchKeyword = "";

// 排序状态 (默认文件夹在最前，按修改时间倒序)
let sortField = "time"; // 'name' | 'size' | 'time'
let sortOrder = "desc"; // 'asc' | 'desc'

// 批量勾选状态
let selectedPaths = new Set();

// 移动目标暂存
let moveTargetPaths = [];
let selectedMoveDest = "/";

// 底部多任务查看状态
let selectedTaskId = null;

// 全局严格统一消息队列 (全场景排队流控调度器)
let clientTaskQueue = [];
let isDispatcherRunning = false;
let configuredDelayMS = 250;
let maxUploadConcurrency = 1; // 上传严格保持单任务串行传输，大文件内部一个邮件接一个邮件发送
let maxDownloadConcurrency = 3; // 大文件同时下载设置的上限

// 任务完成刷新去重集合
let handledCompletedTaskSet = new Set();

document.addEventListener("DOMContentLoaded", () => {
  initApp();
  bindEvents();
});

// ==================== 顶部轻量非阻塞 Toast 提示 (严格单图标) ====================
function showToast(msg, isError = false) {
  const container = document.getElementById("toastWrapper");
  if (!container) return;
  const cleanMsg = (msg || "").replace(/^[✔✅❌⚠️\s]+/, "").trim();
  const item = document.createElement("div");
  item.className = `toast-item ${isError ? 'toast-error' : ''}`;
  item.innerHTML = `<span>${isError ? '⚠️' : '✔'}</span> <span>${cleanMsg}</span>`;
  container.appendChild(item);
  setTimeout(() => {
    if (item.parentNode) item.parentNode.removeChild(item);
  }, 3000);
}

// ==================== 网页端自建交互弹窗 (彻底替换浏览器原生 prompt / confirm) ====================
function showInputPrompt({ title = "✏️ 请输入", desc = "", label = "输入内容", defaultValue = "", placeholder = "请输入..." }) {
  return new Promise((resolve) => {
    const modal = document.getElementById("inputPromptModal");
    const titleEl = document.getElementById("inputPromptTitle");
    const descEl = document.getElementById("inputPromptDesc");
    const labelEl = document.getElementById("inputPromptLabel");
    const inputEl = document.getElementById("inputPromptVal");
    const btnOk = document.getElementById("btnConfirmPrompt");
    const btnCancel = document.getElementById("btnCancelPrompt");
    const btnClose = document.getElementById("btnCloseInputPromptModal");

    if (!modal || !inputEl || !btnOk || !btnCancel) {
      const fallback = prompt(desc || title, defaultValue);
      resolve(fallback);
      return;
    }

    titleEl.innerText = title;
    descEl.innerText = desc;
    labelEl.innerText = label;
    inputEl.value = defaultValue;
    inputEl.placeholder = placeholder;

    modal.classList.remove("hidden");
    inputEl.focus();
    inputEl.select();

    const cleanup = () => {
      modal.classList.add("hidden");
      btnOk.onclick = null;
      btnCancel.onclick = null;
      if (btnClose) btnClose.onclick = null;
      inputEl.onkeydown = null;
    };
    if (btnClose) btnClose.onclick = () => { cleanup(); resolve(null); };

    btnOk.onclick = () => {
      const val = inputEl.value;
      cleanup();
      resolve(val);
    };

    btnCancel.onclick = () => {
      cleanup();
      resolve(null);
    };

    inputEl.onkeydown = (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        btnOk.click();
      } else if (e.key === "Escape") {
        e.preventDefault();
        btnCancel.click();
      }
    };
  });
}

function showConfirmDialog({ title = "⚠️ 操作确认", message = "", isDanger = true, confirmText = "确定执行", cancelText = "取消" }) {
  return new Promise((resolve) => {
    const modal = document.getElementById("confirmDialogModal");
    const titleEl = document.getElementById("confirmDialogTitle");
    const msgEl = document.getElementById("confirmDialogMsg");
    const btnYes = document.getElementById("btnConfirmDialogYes");
    const btnNo = document.getElementById("btnConfirmDialogNo");
    const btnClose = document.getElementById("btnCloseConfirmDialogModal");

    if (!modal || !btnYes || !btnNo) {
      const fallback = confirm(message || title);
      resolve(fallback);
      return;
    }

    titleEl.innerText = title;
    msgEl.innerText = message;
    btnYes.innerText = confirmText;
    btnYes.className = isDanger ? "btn btn-danger" : "btn btn-primary";

    if (!cancelText) {
      btnNo.style.display = "none";
    } else {
      btnNo.style.display = "";
      btnNo.innerText = cancelText;
    }

    modal.classList.remove("hidden");
    btnYes.focus();

    const cleanup = () => {
      modal.classList.add("hidden");
      btnNo.style.display = "";
      btnYes.onclick = null;
      btnNo.onclick = null;
      if (btnClose) btnClose.onclick = null;
      document.removeEventListener("keydown", keyHandler);
    };
    if (btnClose) btnClose.onclick = () => { cleanup(); resolve(false); };

    const keyHandler = (e) => {
      if (e.key === "Escape") {
        cleanup();
        resolve(false);
      }
    };
    document.addEventListener("keydown", keyHandler);

    btnYes.onclick = () => {
      cleanup();
      resolve(true);
    };

    btnNo.onclick = () => {
      cleanup();
      resolve(false);
    };
  });
}

function showAlertDialog({ title = "⚠️ 提示", message = "", isDanger = false, confirmText = "我知道了" }) {
  return showConfirmDialog({ title, message, isDanger, confirmText, cancelText: null });
}

// 全局网页端下载与操作日志 (统一滚屏日志流，直接记录高精度不可变真实发生时间)
let webClientLogs = [];
function logWebTransfer(msg) {
  const now = new Date();
  const timeStr = now.toTimeString().split(' ')[0];

  let level = "INFO";
  let mod = "CLIENT";
  if (msg.includes("ERROR") || msg.includes("❌") || msg.includes("失败")) level = "ERROR";
  else if (msg.includes("WARN") || msg.includes("⚠️") || msg.includes("超时")) level = "WARN";
  else if (msg.includes("DEBUG") || msg.includes("🔍") || msg.includes("探针")) level = "DEBUG";

  if (msg.includes("预览")) mod = "PREVIEW";
  else if (msg.includes("下载") || msg.includes("上传") || msg.includes("队列")) mod = "TRANSFER";
  else if (msg.includes("空间") || msg.includes("设置") || msg.includes("删除") || msg.includes("移动") || msg.includes("重命名") || msg.includes("新建") || msg.includes("文件夹")) mod = "SYSTEM";
  else if (msg.includes("IMAP") || msg.includes("连接") || msg.includes("链路")) mod = "IMAP";
  else if (msg.includes("索引") || msg.includes("对账") || msg.includes("重构")) mod = "INDEX";

  const cleanMsg = msg.replace(/\[(DEBUG|INFO|WARN|ERROR)\]/g, "").replace(/\[(IMAP|TRANSFER|PREVIEW|INDEX|SYSTEM|CLIENT)\]/g, "").trim();

  webClientLogs.push({
    timestamp: now.toISOString(),
    time_str: timeStr,
    level: level,
    module: mod,
    message: cleanMsg
  });
  if (webClientLogs.length > 2000) {
    webClientLogs = webClientLogs.slice(webClientLogs.length - 2000);
  }
  updateUnifiedLogs(false);
}

function updateChunkSizeLabel(bytes) {
  const mb = (bytes / (1024 * 1024)).toFixed(2);
  const el = document.getElementById("chunkSizeConverted");
  if (el) el.innerText = `(约 ${mb} MB)`;
}

async function initApp() {
  await loadStatus();
  if (isConfiguredState) {
    showCloudLoading(true);
    await loadFiles();
    showCloudLoading(false);
  }
  startTaskPolling();
}

function showCloudLoading(show, title = "", desc = "") {
  const overlay = document.getElementById("cloudLoadingOverlay");
  if (overlay) {
    const h4 = overlay.querySelector("h4");
    const p = overlay.querySelector("p");
    if (h4) h4.innerText = title || "正在检索 QQ 邮箱云端主索引...";
    if (p) p.innerText = desc || "正在从云端加密存储拉取最新文件树，请稍候";
    if (show) overlay.classList.remove("hidden");
    else overlay.classList.add("hidden");
  }
}

let backendFailCount = 0;
let isBackendOffline = false;

function setBackendOfflineUI(offline) {
  if (isBackendOffline === offline) return;
  isBackendOffline = offline;

  const btnImapStatus = document.getElementById("btnImapStatus");
  const imapStatusText = document.getElementById("imapStatusText");
  const btnHttpStatus = document.getElementById("btnHttpStatus");
  const httpStatusText = document.getElementById("httpStatusText");
  const navAccountLabel = document.getElementById("navAccountLabel");
  const connDot = document.getElementById("connDot");
  const connText = document.getElementById("connText");
  const offlineBanner = document.getElementById("offlineStatusBar");

  if (offline) {
    if (btnImapStatus) {
      btnImapStatus.className = "status-pill-btn disconnected";
      if (imapStatusText) imapStatusText.innerText = "IMAP通道 已断开";
      btnImapStatus.title = "QQMailDrive 服务端已退出 / 本地连接已断开";
    }
    if (btnHttpStatus) {
      btnHttpStatus.className = "status-pill-btn disconnected";
      if (httpStatusText) httpStatusText.innerText = "HTTP通道 已断开";
      btnHttpStatus.title = "QQMailDrive 服务端已退出 / 本地连接已断开";
    }
    if (connDot) connDot.classList.remove("connected");
    if (connText) connText.innerText = "服务端已离线 (程序已退出)";
    if (navAccountLabel) {
      if (!navAccountLabel.dataset.origText && navAccountLabel.innerText && !navAccountLabel.innerText.includes("离线")) {
        navAccountLabel.dataset.origText = navAccountLabel.innerText;
      }
      navAccountLabel.innerText = "服务端已退出 (已离线)";
    }
    if (offlineBanner) {
      offlineBanner.classList.remove("hidden");
    }
  } else {
    if (offlineBanner) {
      offlineBanner.classList.add("hidden");
    }
    if (navAccountLabel && navAccountLabel.dataset.origText) {
      navAccountLabel.innerText = navAccountLabel.dataset.origText;
    }
  }
}

async function loadStatus() {
  try {
    const res = await fetch("/api/status");
    if (!res.ok) throw new Error("HTTP " + res.status);
    const data = await res.json();

    backendFailCount = 0;
    if (isBackendOffline) {
      setBackendOfflineUI(false);
      showToast("✔ 已恢复与 QQMailDrive 本地服务端的连接");
    }

    const dot = document.getElementById("connDot");
    const text = document.getElementById("connText");
    const navAccountLabel = document.getElementById("navAccountLabel");
    const btnImapStatus = document.getElementById("btnImapStatus");
    const imapStatusText = document.getElementById("imapStatusText");
    const btnHttpStatus = document.getElementById("btnHttpStatus");
    const httpStatusText = document.getElementById("httpStatusText");

    isConfiguredState = !!data.is_configured;

    if (!data.is_configured) {
      if (dot) dot.classList.remove("connected");
      if (text) text.innerText = "首次使用 (请初始化配置)";
      if (navAccountLabel) navAccountLabel.innerText = "首次使用 (请配置)";
      if (btnImapStatus) {
        btnImapStatus.className = "status-pill-btn disconnected";
        if (imapStatusText) imapStatusText.innerText = "IMAP通道 未连接";
      }
      if (btnHttpStatus) {
        btnHttpStatus.className = "status-pill-btn disconnected";
        if (httpStatusText) httpStatusText.innerText = "HTTP通道 未连接";
      }
      document.getElementById("firstRunModal").classList.remove("hidden");
      // crash modal removed
      return;
    }

    if (navAccountLabel) {
      navAccountLabel.innerText = data.email || "未绑定账号";
    }

    // IMAP 通道连接状态判定
    isImapConnected = !!(data.imap_connected !== undefined ? data.imap_connected : data.connected);
    if (dot) {
      if (isImapConnected) dot.classList.add("connected");
      else dot.classList.remove("connected");
    }
    if (text) {
      text.innerText = isImapConnected ? `${data.email} (已连接)` : (data.email ? `${data.email} (连接中断)` : "连接中断");
    }

    if (btnImapStatus && !isRefreshingImap) {
      if (isImapConnected) {
        btnImapStatus.className = "status-pill-btn connected";
        if (imapStatusText) imapStatusText.innerText = "IMAP通道 已连接";
        btnImapStatus.title = "IMAP 邮件存储长连接通道已就绪，工作正常 (点击可刷新状态)";
      } else {
        btnImapStatus.className = "status-pill-btn disconnected";
        if (imapStatusText) imapStatusText.innerText = "IMAP通道 已断开";
        btnImapStatus.title = "IMAP 邮件通道已断开 (点击可刷新连接)";
      }
    }

    // HTTP 网页极速通道状态判定
    isHttpConnected = !!data.http_connected;
    const isHttpChecking = !!data.http_checking;
    if (btnHttpStatus && !isRefreshingHttp) {
      if (isHttpChecking) {
        btnHttpStatus.className = "status-pill-btn connecting";
        if (httpStatusText) httpStatusText.innerText = "HTTP通道 正在连接...";
        btnHttpStatus.title = "正在连接并检测云端会话 (点击可直接扫码登录)...";
        // 自动延时再次拉取，探活或恢复完成后自动转为已连接或未连接，绝不卡死
        setTimeout(() => {
          if (!isRefreshingHttp) loadStatus();
        }, 1500);
      } else if (isHttpConnected) {
        btnHttpStatus.className = "status-pill-btn connected";
        if (httpStatusText) httpStatusText.innerText = "HTTP通道 已连接";
        btnHttpStatus.title = "HTTP 网页免Base64直下通道已激活 (免除33%膨胀，极速5并发，点击可刷新状态)";
      } else {
        btnHttpStatus.className = "status-pill-btn disconnected";
        if (httpStatusText) httpStatusText.innerText = "HTTP通道 未连接";
        btnHttpStatus.title = "HTTP 网页极速通道未连接 (点击可扫码登录)";
      }
    }

    if (isImapConnected) {
      document.getElementById("firstRunModal").classList.add("hidden");
    }

    const dfEl = document.getElementById("dispFolder"); if (dfEl) dfEl.innerText = data.folder || "MailDrive";
    currentActiveStorageFolder = cleanFolderDisplayName(data.folder || currentActiveStorageFolder || "");
    updateBreadcrumb();
    if (data.connected && isolatedSpaces.length === 0) {
      loadIsolatedSpaces();
    }
    // 从后端实时同步全局流控参数
    fetch("/api/config").then(r => r.json()).then(cfg => {
      if (typeof cfg.request_delay_ms === "number" && cfg.request_delay_ms >= 0) configuredDelayMS = cfg.request_delay_ms; else configuredDelayMS = 250;
      if (typeof cfg.chunk_size_bytes === "number" && cfg.chunk_size_bytes > 0) configuredChunkSizeBytes = cfg.chunk_size_bytes; else configuredChunkSizeBytes = 52428799;
      if (cfg.max_concurrency > 0) maxUploadConcurrency = cfg.max_concurrency;
      if (cfg.chunk_download_concurrency > 0) maxDownloadConcurrency = cfg.chunk_download_concurrency; else maxDownloadConcurrency = 3;
    }).catch(()=>{});
    const sizeEl = document.getElementById("dispTotalSize");
    if (sizeEl) sizeEl.innerText = formatBytes(data.total_size || 0);
    const filesEl = document.getElementById("dispTotalFiles");
    if (filesEl) filesEl.innerText = `${data.total_files || 0} 个`;

    // 异常退出已改为系统强行自动对账修复，无需手动弹窗
  } catch (err) {
    console.error("加载状态异常:", err);
    backendFailCount++;
    if (backendFailCount >= 2) {
      setBackendOfflineUI(true);
    }
  }
}

async function loadFiles() {
  try {
    const res = await fetch("/api/files");
    filesData = await res.json() || [];

    // 计算去重后的所有实际文件的总大小（严格排除文件夹，杜绝重复计算）
    const actualFiles = filesData.filter(f => !f.is_dir);
    const totalBytes = actualFiles.reduce((acc, f) => acc + (f.size || 0), 0);
    const sizeEl = document.getElementById("dispTotalSize");
    if (sizeEl) sizeEl.innerText = formatBytes(totalBytes);
    const filesEl = document.getElementById("dispTotalFiles");
    if (filesEl) filesEl.innerText = `${actualFiles.length} 个`;

    const activeName = cleanFolderDisplayName(currentActiveStorageFolder);
    if (isolatedSpaces && isolatedSpaces.length > 0 && activeName) {
      const targetSpace = isolatedSpaces.find(s => cleanFolderDisplayName(s.display_name || s.name) === activeName);
      if (targetSpace) {
        targetSpace.file_count = actualFiles.length;
        targetSpace.total_size = totalBytes;
        targetSpace.stats_loaded = true;
      }
    }

    renderFolderTree();
    updateBreadcrumb();
    renderFiles();
  } catch (err) {
    console.error("加载文件列表异常:", err);
  }
}

// ==================== 多级动态文件夹树核心逻辑 ====================

function buildFolderTreeData() {
  const folderSet = new Set(["/"]);

  filesData.forEach(f => {
    if (f.is_dir) {
      folderSet.add(f.virtual_path);
    }
    const parts = f.virtual_path.split("/").filter(Boolean);
    let cur = "";
    for (let i = 0; i < parts.length - 1; i++) {
      cur += "/" + parts[i];
      folderSet.add(cur);
    }
  });

  const currentSpaceDisplayName = cleanFolderDisplayName(currentActiveStorageFolder) || "全部文件";
  const root = { path: "/", name: currentSpaceDisplayName, children: [], level: 0 };
  const map = { "/": root };

  const sortedPaths = Array.from(folderSet).filter(p => p !== "/").sort();
  sortedPaths.forEach(p => {
    const parts = p.split("/").filter(Boolean);
    const name = parts[parts.length - 1];
    const parentPath = parts.length === 1 ? "/" : ("/" + parts.slice(0, -1).join("/"));
    const node = { path: p, name, children: [], level: parts.length };
    map[p] = node;

    if (map[parentPath]) {
      map[parentPath].children.push(node);
    } else {
      root.children.push(node);
    }
  });

  return root;
}

let isolatedSpaces = [];
let expandedSpaces = new Set();
let activeSpaceMenuEl = null;

function closeSpaceActionMenu() {
  if (activeSpaceMenuEl) {
    activeSpaceMenuEl.remove();
    activeSpaceMenuEl = null;
  }
}

async function loadIsolatedSpaces() {
  try {
    const res = await fetch("/api/folders");
    const data = await res.json();
    if (data.status === "ok" && Array.isArray(data.folders)) {
      data.folders.sort((a, b) => {
        const aName = cleanFolderDisplayName(a.display_name || a.name);
        const bName = cleanFolderDisplayName(b.display_name || b.name);
        const aRec = (aName === "收到的文件");
        const bRec = (bName === "收到的文件");
        if (aRec && !bRec) return 1;
        if (!aRec && bRec) return -1;
        return aName.localeCompare(bName, "zh-CN", { numeric: true, sensitivity: "base" });
      });
      isolatedSpaces = data.folders;
      renderFolderTree();
      if (isolatedSpaces.some(s => !s.stats_loaded)) {
        setTimeout(() => {
          loadIsolatedSpaces();
        }, 3000);
      }
    }
  } catch (err) {
    console.error("加载隔离空间列表异常:", err);
  }
}

let spaceSwitchSeq = 0;

async function switchIsolatedSpace(targetFolder, forceRefresh = false) {
  const mySeq = ++spaceSwitchSeq;
  try {
    const sRes = await fetch("/api/folders/switch", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ folder: targetFolder, force_refresh: forceRefresh })
    });
    const sData = await sRes.json();
    if (mySeq !== spaceSwitchSeq) return;
    if (sData.status === "ok") {
      currentActiveStorageFolder = cleanFolderDisplayName(sData.folder || targetFolder);
      try {
        const emailHint = document.getElementById("connText")?.innerText?.split(" ")[0] || "";
        if (emailHint.includes("@") && currentActiveStorageFolder) {
          localStorage.setItem("qmd_last_folder_" + emailHint, currentActiveStorageFolder);
        }
      } catch (e) {}
      currentPath = "/";
      selectedPaths.clear();
      updateBatchBar();
      expandedSpaces.add(currentActiveStorageFolder);
      await loadFiles("/");
      if (mySeq !== spaceSwitchSeq) return;
      if (forceRefresh) {
        showToast(`✔ 已刷新并同步 [${currentActiveStorageFolder}] 索引！`);
      }
    } else {
      showToast(`❌ 切换失败: ${sData.message || ""}`, true);
    }
  } catch (err) {
    if (mySeq === spaceSwitchSeq) {
      showToast("❌ 切换请求异常: " + err.message, true);
    }
  }
}

async function renameIsolatedSpace(oldName) {
  const newName = await showInputPrompt({
    title: "✏️ 隔离空间改名",
    desc: `请输入隔离空间 [${oldName}] 的新名称:`,
    label: "新空间名称",
    defaultValue: oldName
  });
  if (!newName || !newName.trim() || newName.trim() === oldName) return;
  const trimmed = newName.trim();
  try {
    const rRes = await fetch("/api/folders/rename", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ old_folder: oldName, new_folder: trimmed })
    });
    const rData = await rRes.json();
    if (rData.status === "ok") {
      if (expandedSpaces.has(oldName)) {
        expandedSpaces.delete(oldName);
        expandedSpaces.add(trimmed);
      }
      if (cleanFolderDisplayName(currentActiveStorageFolder) === oldName) {
        currentActiveStorageFolder = trimmed;
      }
      showToast(`✔ 隔离空间已成功改名为 [${trimmed}]！`);
      await loadStatus();
      await loadIsolatedSpaces();
    } else {
      showToast(`❌ 改名失败: ${rData.message || ""}`, true);
    }
  } catch (err) {
    showToast("❌ 改名异常: " + err.message, true);
  }
}

async function deleteIsolatedSpace(folderName) {
  if (isolatedSpaces.length <= 1) {
    showToast("⚠️ 禁止删除：当前仅剩最后一个隔离空间，网盘必须至少保留一个空间！", true);
    return;
  }
  const ok = await showConfirmDialog({
    title: "⚠️ 危险操作确认",
    message: `确定要永久删除隔离空间「${folderName}」吗？\n\n【极其重要】：此操作将不可逆地物理抹除 QQ 邮箱云端该目录下的所有邮件分片与主索引，全部数据将被永久清除！`
  });
  if (!ok) return;
  try {
    const dRes = await fetch("/api/folders/delete", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ folder: folderName })
    });
    const dData = await dRes.json();
    if (dData.status === "ok") {
      expandedSpaces.delete(folderName);
      if (dData.switched) {
        showToast(`✔ 已彻底抹除原活动空间 [${folderName}]，已自动切换至 [${dData.current_folder}]！`);
      } else {
        showToast(`✔ 已成功彻底抹除闲置隔离空间 [${folderName}]！`);
      }
      currentPath = "/";
      selectedPaths.clear();
      await loadStatus();
      await loadFiles();
      await loadIsolatedSpaces();
      updateBreadcrumb();
    } else {
      showToast(`❌ 删除失败: ${dData.message || ""}`, true);
    }
  } catch (err) {
    showToast("❌ 删除请求异常: " + err.message, true);
  }
}

function openSpaceActionMenu(anchorBtn, spaceName, isReceivedFolder) {
  closeSpaceActionMenu();
  const rect = anchorBtn.getBoundingClientRect();
  const menu = document.createElement("div");
  menu.className = "space-context-menu";

  const addItem = (label, onClick, isDanger = false) => {
    const btn = document.createElement("button");
    btn.className = `space-context-item${isDanger ? " danger" : ""}`;
    btn.innerText = label;
    btn.onclick = (e) => {
      e.stopPropagation();
      closeSpaceActionMenu();
      onClick();
    };
    menu.appendChild(btn);
  };

  addItem("🔄 刷新索引", () => switchIsolatedSpace(spaceName, true));
  if (!isReceivedFolder) {
    addItem("✏️ 改名", () => renameIsolatedSpace(spaceName));
  }
  addItem("🔍 存储对账", async () => {
    const activeName = cleanFolderDisplayName(currentActiveStorageFolder);
    if (spaceName !== activeName) {
      await switchIsolatedSpace(spaceName, false);
    }
    triggerHealthCheck();
  });
  if (!isReceivedFolder) {
    addItem("🗑️ 删除空间", () => deleteIsolatedSpace(spaceName), true);
  }

  document.body.appendChild(menu);
  const menuRect = menu.getBoundingClientRect();
  let left = rect.right + 4;
  if (left + menuRect.width > window.innerWidth - 8) {
    left = rect.left - menuRect.width - 4;
  }
  let top = rect.top;
  if (top + menuRect.height > window.innerHeight - 8) {
    top = window.innerHeight - menuRect.height - 8;
  }
  menu.style.left = `${Math.max(8, left)}px`;
  menu.style.top = `${Math.max(8, top)}px`;
  activeSpaceMenuEl = menu;
}

function renderFolderTree() {
  const treeContainer = document.getElementById("folderTreeView");
  if (!treeContainer) return;
  treeContainer.innerHTML = "";

  if (!isolatedSpaces || isolatedSpaces.length === 0) {
    const root = buildFolderTreeData();
    renderTreeNode(root, treeContainer);
    return;
  }

  const activeName = cleanFolderDisplayName(currentActiveStorageFolder);
  let allSpacesFileCount = 0;
  let allSpacesTotalBytes = 0;

  isolatedSpaces.forEach(space => {
    const displayName = cleanFolderDisplayName(space.display_name || space.name);
    const isReceivedFolder = (displayName === "收到的文件");
    const isCurrentSpace = (displayName === activeName);
    const isExpanded = isCurrentSpace && expandedSpaces.has(displayName);

    const block = document.createElement("div");
    block.className = `isolated-space-block ${isCurrentSpace ? "active" : ""}`;

    const header = document.createElement("div");
    header.className = "isolated-space-header";

    const arrow = document.createElement("span");
    arrow.className = "isolated-space-arrow";
    arrow.innerText = isExpanded ? "▼" : "▶";

    const icon = document.createElement("span");
    icon.className = "isolated-space-icon";
    icon.innerText = isReceivedFolder ? "📥" : (isExpanded ? "📂" : "📁");

    const nameEl = document.createElement("span");
    nameEl.className = "isolated-space-name";
    nameEl.innerText = displayName;
    nameEl.title = displayName;

    const moreBtn = document.createElement("button");
    moreBtn.className = "isolated-space-more-btn";
    moreBtn.type = "button";
    moreBtn.innerText = "⋯";
    moreBtn.title = "空间操作 (刷新 / 改名 / 删除)";
    moreBtn.onclick = (e) => {
      e.stopPropagation();
      openSpaceActionMenu(moreBtn, displayName, isReceivedFolder);
    };

    header.appendChild(arrow);
    header.appendChild(icon);
    header.appendChild(nameEl);

    const statsEl = document.createElement("span");
    statsEl.className = "isolated-space-stats";
    if (isCurrentSpace) {
      const actualFiles = (filesData || []).filter(f => !f.is_dir);
      const totalBytes = actualFiles.reduce((acc, f) => acc + (f.size || 0), 0);
      allSpacesFileCount += actualFiles.length;
      allSpacesTotalBytes += totalBytes;
      statsEl.innerText = `${actualFiles.length} 个 · ${formatBytes(totalBytes)}`;
      statsEl.title = `已存文件: ${actualFiles.length} 个，总大小: ${formatBytes(totalBytes)}`;
    } else if (space.stats_loaded) {
      const fCnt = space.file_count || 0;
      const fSz = space.total_size || 0;
      allSpacesFileCount += fCnt;
      allSpacesTotalBytes += fSz;
      statsEl.innerText = `${fCnt} 个 · ${formatBytes(fSz)}`;
      statsEl.title = `已存文件: ${fCnt} 个，总大小: ${formatBytes(fSz)}`;
    } else {
      statsEl.innerText = "统计中...";
    }
    header.appendChild(statsEl);

    header.appendChild(moreBtn);

    arrow.onclick = async (e) => {
      e.stopPropagation();
      closeSpaceActionMenu();
      if (!isCurrentSpace) {
        await switchIsolatedSpace(displayName, false);
      } else {
        if (expandedSpaces.has(displayName)) {
          expandedSpaces.delete(displayName);
        } else {
          expandedSpaces.add(displayName);
        }
        renderFolderTree();
      }
    };

    header.onclick = async () => {
      closeSpaceActionMenu();
      if (!isCurrentSpace) {
        await switchIsolatedSpace(displayName, false);
      } else {
        if (currentPath !== "/") {
          currentPath = "/";
          selectedPaths.clear();
          updateBatchBar();
          expandedSpaces.add(displayName);
          renderFolderTree();
          updateBreadcrumb();
          renderFiles();
        } else {
          if (expandedSpaces.has(displayName)) {
            expandedSpaces.delete(displayName);
          } else {
            expandedSpaces.add(displayName);
          }
          renderFolderTree();
        }
      }
    };

    block.appendChild(header);

    if (isExpanded) {
      const childrenWrap = document.createElement("div");
      childrenWrap.className = "isolated-space-children";
      const root = buildFolderTreeData();
      if (root.children && root.children.length > 0) {
        root.children.forEach(child => renderTreeNode(child, childrenWrap));
      } else {
        const emptyEl = document.createElement("div");
        emptyEl.className = "isolated-space-empty";
        emptyEl.innerText = "无子文件夹";
        childrenWrap.appendChild(emptyEl);
      }
      block.appendChild(childrenWrap);
    }

    treeContainer.appendChild(block);
  });

  const totalStatsEl = document.getElementById("spacesTotalStats");
  if (totalStatsEl) {
    totalStatsEl.innerText = `${allSpacesFileCount} 个 · ${formatBytes(allSpacesTotalBytes)}`;
    totalStatsEl.title = `所有隔离空间合计: ${allSpacesFileCount} 个文件，总大小 ${formatBytes(allSpacesTotalBytes)}`;
  }
}

function renderTreeNode(node, container) {
  const hasChildren = node.children && node.children.length > 0;
  const isExpanded = expandedFolders.has(node.path);
  const isActive = currentPath === node.path;

  const row = document.createElement("div");
  row.className = `tree-item-row ${isActive ? 'active' : ''}`;
  // 严密层级缩进：同级目录（如 1 和 2）拥有完全相同的 level 与 padding，子目录（如 3）规范后移
  row.style.paddingLeft = `${node.level * 6 + 4}px`;

  const arrow = document.createElement("span");
  if (hasChildren) {
    arrow.className = "tree-toggle-arrow";
    arrow.innerHTML = isExpanded ? "▼" : "▶";
    arrow.onclick = (e) => {
      e.stopPropagation();
      if (expandedFolders.has(node.path)) {
        expandedFolders.delete(node.path);
      } else {
        expandedFolders.add(node.path);
      }
      renderFolderTree();
    };
  } else {
    // 严格占位：无子目录节点使用完全等宽的透明占位符，确保同级文件夹图标绝对平齐！
    arrow.className = "tree-toggle-arrow-placeholder";
    arrow.innerHTML = "&nbsp;";
  }

  const icon = document.createElement("span");
  icon.className = "tree-icon";
  icon.innerText = (hasChildren && isExpanded) ? "📂" : "📁";

  const label = document.createElement("span");
  label.className = "tree-label";
  label.innerText = node.name;
  label.title = node.path;

  row.appendChild(arrow);
  row.appendChild(icon);
  row.appendChild(label);

  row.onclick = () => {
    currentPath = node.path;
    selectedPaths.clear();
    updateBatchBar();
    if (hasChildren) {
      if (expandedFolders.has(node.path)) {
        expandedFolders.delete(node.path);
      } else {
        expandedFolders.add(node.path);
      }
    }
    renderFolderTree();
    updateBreadcrumb();
    renderFiles();
  };

  container.appendChild(row);

  if (hasChildren && isExpanded) {
    node.children.forEach(child => renderTreeNode(child, container));
  }
}

// ==================== 多维度排序与搜索过滤 ====================

function getFilteredAndSortedFiles() {
  let list = [];

  if (searchKeyword.trim() !== "") {
    // 检索模式：全盘模糊搜索匹配的文件与文件夹
    const kw = searchKeyword.trim().toLowerCase();
    list = filesData.filter(f => f.name.toLowerCase().includes(kw));
  } else {
    // 常规目录模式
    list = filesData.filter(f => {
      const p = f.virtual_path || "";
      if (currentPath === "/") {
        return p.startsWith("/") && p.slice(1).indexOf("/") === -1;
      }
      return p.startsWith(currentPath + "/") && p.slice(currentPath.length + 1).indexOf("/") === -1;
    });
  }

  return list.sort((a, b) => {
    if (a.is_dir !== b.is_dir) {
      return a.is_dir ? -1 : 1;
    }

    let comp = 0;
    if (sortField === "name") {
      comp = a.name.localeCompare(b.name, "zh-CN", { numeric: true });
    } else if (sortField === "size") {
      const sa = a.is_dir ? 0 : a.size;
      const sb = b.is_dir ? 0 : b.size;
      comp = sa - sb;
    } else if (sortField === "time") {
      const ta = new Date(a.mod_time).getTime() || 0;
      const tb = new Date(b.mod_time).getTime() || 0;
      comp = ta - tb;
    }

    return sortOrder === "asc" ? comp : -comp;
  });
}

function handleSortClick(field) {
  if (sortField === field) {
    sortOrder = sortOrder === "asc" ? "desc" : "asc";
  } else {
    sortField = field;
    sortOrder = field === "name" ? "asc" : "desc";
  }
  renderFiles();
}

// ==================== 主内容区文件渲染 (只保留下载/重命名/删除 3个按钮，绝不换行) ====================

window.handleThumbError = function(img, fallbackIcon) {
  if (!img) return;
  img.onerror = null;
  const parent = img.closest(".list-thumb-box, .grid-thumb-box") || img.parentElement;
  if (parent) {
    const isGrid = parent.classList.contains("grid-thumb-box");
    parent.innerHTML = `<span class="${isGrid ? 'grid-icon-generic' : 'list-icon-generic'}">${fallbackIcon || '🎬'}</span>`;
  } else {
    img.style.display = "none";
  }
};

function renderFiles() {
  const container = document.getElementById("fileContainer");
  const contentList = document.getElementById("fileContentList");
  if (!container || !contentList) return;

  contentList.innerHTML = "";

  if (isListView) {
    container.className = "file-view-wrapper list-mode";
  } else {
    container.className = "file-view-wrapper grid-mode";
  }

  if (!isConfiguredState) {
    contentList.innerHTML = `
      <div style="text-align: center; color: #94a3b8; padding: 60px 0; width: 100%;">
        <p style="font-size: 48px; margin-bottom: 12px;">👋</p>
        <p style="font-size: 15px; font-weight: 600; color: #475569;">欢迎使用 QQMailDrive 个人加密网盘</p>
        <p style="font-size: 13px; margin-top: 6px;">请先在弹出的向导或设置中，输入 QQ 邮箱与 IMAP 授权码以初始化专属空间</p>
      </div>`;
    return;
  }

  const curFiles = getFilteredAndSortedFiles();

  if (curFiles.length === 0) {
    contentList.innerHTML = `
      <div style="text-align: center; color: #94a3b8; padding: 50px 0; width: 100%;">
        <p style="font-size: 36px; margin-bottom: 8px;">📂</p>
        <p>${searchKeyword ? '未找到匹配的文件或文件夹' : '当前目录下暂无文件，直接将文件拖拽至此白色区域即可上传'}</p>
      </div>`;
    updateBatchBar();
    return;
  }

  // 1. 列表模式渲染
  if (isListView) {
    const tableHeader = document.createElement("div");
    tableHeader.className = "table-header-row";

    const allSelected = curFiles.length > 0 && curFiles.every(f => selectedPaths.has(f.virtual_path));
    const arrowSymbol = sortOrder === "asc" ? "▲" : "▼";
    const nameArrow = sortField === "name" ? `<span class="sort-arrow">${arrowSymbol}</span>` : "";
    const sizeArrow = sortField === "size" ? `<span class="sort-arrow">${arrowSymbol}</span>` : "";
    const timeArrow = sortField === "time" ? `<span class="sort-arrow">${arrowSymbol}</span>` : "";

    tableHeader.innerHTML = `
      <div class="col-cb">
        <input type="checkbox" id="chkSelectAll" ${allSelected ? "checked" : ""}>
      </div>
      <div class="col-name sortable-header" id="thSortName">名称 ${nameArrow}</div>
      <div class="col-size sortable-header" id="thSortSize" style="justify-content: flex-end;">大小 ${sizeArrow}</div>
      <div class="col-time sortable-header" id="thSortTime" style="justify-content: center;">修改时间 ${timeArrow}</div>
      <div class="col-actions">操作</div>
    `;

    tableHeader.querySelector("#chkSelectAll").onclick = (e) => {
      const checked = e.target.checked;
      curFiles.forEach(f => {
        if (checked) selectedPaths.add(f.virtual_path);
        else selectedPaths.delete(f.virtual_path);
      });
      updateBatchBar();
      renderFiles();
    };

    tableHeader.querySelector("#thSortName").onclick = () => handleSortClick("name");
    tableHeader.querySelector("#thSortSize").onclick = () => handleSortClick("size");
    tableHeader.querySelector("#thSortTime").onclick = () => handleSortClick("time");

    contentList.appendChild(tableHeader);

    curFiles.forEach(f => {
      const row = document.createElement("div");
      row.className = "table-item-row";
      const isChecked = selectedPaths.has(f.virtual_path);

      let thumbHtml = "";
      if (f.is_dir) {
        thumbHtml = `<div class="list-thumb-box"><span class="list-icon-generic">📁</span></div>`;
      } else if (f.thumb_base64 || f.is_media || f.name.match(/\.(jpg|jpeg|png|webp|gif|bmp|mp4|mkv|mov|avi|webm)$/i)) {
        const tSrc = f.thumb_base64 || `/api/thumb?path=${encodeURIComponent(f.virtual_path)}`;
        const fallbackIcon = f.name.match(/\.(mp4|mkv|mov|avi|webm)$/i) ? '🎬' : '🖼️';
        thumbHtml = `<div class="list-thumb-box"><img class="list-thumb-img" src="${tSrc}" loading="lazy" alt="" onerror="handleThumbError(this,'${fallbackIcon}')"></div>`;
      } else {
        thumbHtml = `<div class="list-thumb-box"><span class="list-icon-generic">📄</span></div>`;
      }

      const timeStr = (f.is_shared_in ? '📬 ' : '') + formatDateTime(f.mod_time);

      row.innerHTML = `
        <div class="col-cb">
          <input type="checkbox" class="item-cb" ${isChecked ? "checked" : ""}>
        </div>
        <div class="col-name">
          ${thumbHtml}
          <span class="list-name-text" title="点击进入/预览">${f.name}</span>${f.is_dir ? `<span class="item-count-badge">${f.direct_count || 0}个文件</span>` : ''}
        </div>
        <div class="col-size">${formatBytes(f.size || 0)}</div>
        <div class="col-time">${timeStr}</div>
        <div class="col-actions">
          <button class="btn btn-primary btn-sm btn-download">下载</button>
          <button class="btn btn-secondary btn-sm btn-rename">重命名</button>
          <button class="btn btn-secondary btn-sm btn-del">删除</button>
        </div>
      `;

      bindItemEvents(row, f);
      contentList.appendChild(row);
    });
    updateBatchBar();
    return;
  }

  // 2. 大图标网格模式渲染 (上大图、中名、下大小与修改时间、底3个按钮)
  curFiles.forEach(f => {
    const card = document.createElement("div");
    card.className = "grid-card";
    const isChecked = selectedPaths.has(f.virtual_path);

    let thumbHtml = "";
    if (f.is_dir) {
      thumbHtml = `<div class="grid-thumb-box" title="点击进入文件夹"><span class="grid-icon-generic">📁</span></div>`;
    } else if (f.thumb_base64 || f.is_media || f.name.match(/\.(jpg|jpeg|png|webp|gif|bmp|mp4|mkv|mov|avi|webm)$/i)) {
      const tSrc = f.thumb_base64 || `/api/thumb?path=${encodeURIComponent(f.virtual_path)}`;
      const fallbackIcon = f.name.match(/\.(mp4|mkv|mov|avi|webm)$/i) ? '🎬' : '🖼️';
      thumbHtml = `<div class="grid-thumb-box" title="点击预览"><img class="grid-thumb-img" src="${tSrc}" loading="lazy" alt="" onerror="handleThumbError(this,'${fallbackIcon}')"></div>`;
    } else {
      thumbHtml = `<div class="grid-thumb-box" title="点击预览文档"><span class="grid-icon-generic">📄</span></div>`;
    }

    const timeStr = (f.is_shared_in ? '📬 ' : '') + formatDateTime(f.mod_time);

    card.innerHTML = `
      <div class="grid-cb-wrap">
        <input type="checkbox" class="item-cb" ${isChecked ? "checked" : ""}>
      </div>
      ${thumbHtml}
      <div class="grid-meta">
        <div class="grid-title" title="点击进入/预览">${f.name}${f.is_dir ? ` <span class="item-count-badge">${f.direct_count || 0}个文件</span>` : ''}</div>
        <div class="grid-info-row">
          <span>${f.is_dir ? ('📁 ' + formatBytes(f.size || 0)) : formatBytes(f.size || 0)}</span>
          <span>${timeStr}</span>
        </div>
        <div class="grid-actions">
          <button class="btn btn-primary btn-sm btn-download">下载</button>
          <button class="btn btn-secondary btn-sm btn-rename">重命名</button>
          <button class="btn btn-secondary btn-sm btn-del">删除</button>
        </div>
      </div>
    `;

    bindItemEvents(card, f);
    contentList.appendChild(card);
  });
  updateBatchBar();
}

// ==================== 右键上下文菜单模块 (支持单选与多选) ====================
let contextMenuTarget = null;

function openContextMenu(clientX, clientY, file) {
  contextMenuTarget = file;
  const menu = document.getElementById("fileContextMenu");
  if (!menu) return;

  const count = selectedPaths.size;
  const ctxDownload = document.getElementById("ctxDownload");
  const ctxMove = document.getElementById("ctxMove");
  const ctxRename = document.getElementById("ctxRename");
  const ctxDelete = document.getElementById("ctxDelete");

  const ctxShare = document.getElementById("ctxShare");
  if (count > 1) {
    if (ctxDownload) ctxDownload.querySelector(".ctx-text").innerText = `下载 (已选 ${count} 项)`;
    if (ctxShare) ctxShare.querySelector(".ctx-text").innerText = `分享文件 (已选 ${count} 项)`;
    if (ctxCopy) ctxCopy.querySelector(".ctx-text").innerText = `复制 (已选 ${count} 项)`;
    if (ctxMove) ctxMove.querySelector(".ctx-text").innerText = `移动 (已选 ${count} 项)`;
    if (ctxRename) ctxRename.classList.add("disabled");
    if (ctxDelete) ctxDelete.querySelector(".ctx-text").innerText = `删除 (已选 ${count} 项)`;
  } else {
    if (ctxDownload) ctxDownload.querySelector(".ctx-text").innerText = file.is_dir ? "下载文件夹 (ZIP)" : "下载";
    if (ctxShare) ctxShare.querySelector(".ctx-text").innerText = file.is_dir ? "分享文件夹" : "分享文件";
    if (ctxCopy) ctxCopy.querySelector(".ctx-text").innerText = "复制";
    if (ctxMove) ctxMove.querySelector(".ctx-text").innerText = "移动";
    if (ctxRename) {
      ctxRename.classList.remove("disabled");
      ctxRename.querySelector(".ctx-text").innerText = "重命名";
    }
    if (ctxDelete) ctxDelete.querySelector(".ctx-text").innerText = "删除";
  }

  menu.classList.remove("hidden");
  menu.style.display = "block";

  // 自适应边界定位，防止菜单超出窗口
  const menuWidth = 165;
  const menuHeight = 180;
  let posX = clientX;
  let posY = clientY;

  if (posX + menuWidth > window.innerWidth) {
    posX = Math.max(10, window.innerWidth - menuWidth - 10);
  }
  if (posY + menuHeight > window.innerHeight) {
    posY = Math.max(10, window.innerHeight - menuHeight - 10);
  }

  menu.style.left = posX + "px";
  menu.style.top = posY + "px";
}

function closeContextMenu() {
  const menu = document.getElementById("fileContextMenu");
  if (menu) {
    menu.classList.add("hidden");
    menu.style.display = "none";
  }
}

function bindItemEvents(el, f) {
  // 右键上下文菜单支持 (单选/多选右键)
  el.oncontextmenu = (e) => {
    e.preventDefault();
    e.stopPropagation();

    // 如果当前右键点击的项目不在多选集合中，清除其他并选中当前项 (无刷新更新勾选状态，绝不破坏 DOM)
    if (!selectedPaths.has(f.virtual_path)) {
      selectedPaths.clear();
      selectedPaths.add(f.virtual_path);
      document.querySelectorAll(".table-item-row, .grid-card").forEach(itemEl => {
        const itemCb = itemEl.querySelector(".item-cb");
        if (itemCb) itemCb.checked = (itemEl === el);
      });
      updateBatchBar();
    }

    openContextMenu(e.clientX, e.clientY, f);
  };

  // 单项勾选事件
  const cb = el.querySelector(".item-cb");
  if (cb) {
    cb.onclick = (e) => {
      e.stopPropagation();
      if (cb.checked) {
        selectedPaths.add(f.virtual_path);
      } else {
        selectedPaths.delete(f.virtual_path);
      }
      updateBatchBar();
    };
  }

  // 点击图标或名称：直接进入文件夹或打开预览
  const triggerOpen = () => {
    if (f.is_dir) {
      currentPath = f.virtual_path;
      selectedPaths.clear();
      updateBatchBar();
      expandedFolders.add(currentPath);
      renderFolderTree();
      updateBreadcrumb();
      renderFiles();
    } else {
      openPreview(f);
    }
  };

  const nameEl = el.querySelector(".list-name-text, .grid-title");
  if (nameEl) nameEl.onclick = triggerOpen;

  const thumbBox = el.querySelector(".grid-thumb-box, .list-thumb-box");
  if (thumbBox) thumbBox.onclick = triggerOpen;

  // 整行或整卡片双击无阻碍进入文件夹或打开预览
  el.ondblclick = (e) => {
    if (e.target.closest(".item-cb, .btn-download, .btn-rename, .btn-del, .col-actions")) return;
    triggerOpen();
  };

  // 下载按钮 (纯网页独立下载，不调用浏览器下载，支持并发)
  const btnDownload = el.querySelector(".btn-download");
  if (btnDownload) {
    btnDownload.onclick = () => {
      requestWebDownload(f);
    };
  }

  // 重命名 (使用网页模态输入框，彻底替换浏览器 prompt)
  const btnRename = el.querySelector(".btn-rename");
  if (btnRename) {
    btnRename.onclick = async () => {
      const newName = await showInputPrompt({
        title: "✏️ 重命名",
        desc: `请输入 "${f.name}" 的新名称:`,
        label: "新文件名",
        defaultValue: f.name
      });
      if (newName && newName.trim() !== "" && newName.trim() !== f.name) {
        let cleanNew = newName.trim().replace(/^[\\\/]+|[\\\/]+$/g, "").replace(/\\/g, "/");
        if (!cleanNew || cleanNew === f.name) return;
        const parts = f.virtual_path.split("/").filter(Boolean);
        parts[parts.length - 1] = cleanNew;
        const newPath = "/" + parts.join("/");

        const fd = new FormData();
        fd.append("old_path", f.virtual_path);
        fd.append("new_path", newPath);
        await fetch("/api/fs/rename", { method: "POST", body: fd });
        showToast("✔ 重命名成功！");
        await loadFiles();
      }
    };
  }

  // 删除 (使用网页模态确认框，彻底替换浏览器 confirm)
  const btnDel = el.querySelector(".btn-del");
  if (btnDel) {
    btnDel.onclick = async () => {
      const msg = f.is_dir
        ? `确定彻底删除文件夹 "${f.name}" 吗？\n将连同其内部的所有子文件及嵌套文件夹一并彻底物理删除！`
        : `确定从云端永久删除 "${f.name}" 吗？`;
      const ok = await showConfirmDialog({
        title: f.is_dir ? "🗑️ 彻底删除文件夹" : "🗑️ 彻底删除文件",
        message: msg,
        isDanger: true,
        confirmText: "彻底删除"
      });
      if (ok) {
        await executeBatchDelete([f.virtual_path]);
      }
    };
  }
}

// ==================== 双通道状态交互与极速提速引导管理器 ====================

function initChannelStatusHandlers() {
  const btnImapStatus = document.getElementById("btnImapStatus");
  const imapStatusText = document.getElementById("imapStatusText");
  const btnHttpStatus = document.getElementById("btnHttpStatus");
  const httpStatusText = document.getElementById("httpStatusText");

  if (btnImapStatus) {
    btnImapStatus.onclick = async () => {
      if (isRefreshingImap) return;
      isRefreshingImap = true;

      // 切换为正在刷新状态，不弹任何通知
      btnImapStatus.className = "status-pill-btn connecting";
      if (imapStatusText) imapStatusText.innerText = "IMAP通道 正在刷新...";
      btnImapStatus.title = "正在后台刷新 IMAP 通道连接状态...";

      let reconnected = false;
      try {
        const res = await fetch("/api/imap/reconnect", { method: "POST" });
        const ret = await res.json();
        reconnected = !!(ret.status === "ok" && ret.connected);
        isImapConnected = reconnected;
      } catch (e) {
        isImapConnected = false;
        reconnected = false;
      } finally {
        isRefreshingImap = false;
        await loadStatus();
      }

      if (!reconnected) {
        // 没登录就弹窗登录
        if (!isConfiguredState) {
          const firstRunModal = document.getElementById("firstRunModal");
          if (firstRunModal) firstRunModal.classList.remove("hidden");
        } else {
          const settingsModal = document.getElementById("settingsModal");
          if (settingsModal) settingsModal.classList.remove("hidden");
        }
      }
    };
  }

  if (btnHttpStatus) {
    btnHttpStatus.onclick = async () => {
      // 1. 如果当前是未连接状态（或正在连接初始化）：用户点击直接弹出扫码登录窗口，不先刷新，不阻拦！
      if (!isHttpConnected) {
        openStandaloneQRModal();
        return;
      }

      // 2. 如果当前是已连接状态：点击触发主动刷新探活
      if (isRefreshingHttp) return;
      isRefreshingHttp = true;

      btnHttpStatus.className = "status-pill-btn connecting";
      if (httpStatusText) httpStatusText.innerText = "HTTP通道 正在刷新...";
      btnHttpStatus.title = "正在后台刷新 HTTP 网页通道状态...";

      let isValid = false;
      try {
        const res = await fetch("/api/webmail/status");
        const ret = await res.json();
        isValid = !!(ret.status === "ok" && ret.is_valid);
        isHttpConnected = isValid;
        if (isValid) {
          sessionSkipHttpPrompt = false;
        }
      } catch (e) {
        isHttpConnected = false;
        isValid = false;
      } finally {
        isRefreshingHttp = false;
        await loadStatus();
      }

      if (!isValid) {
        // 探活发现会话已失效，直接弹出扫码登录窗口
        openStandaloneQRModal();
      }
    };
  }

  // 绑定独立二维码弹窗内的关闭与刷新事件
  const btnCloseQR = document.getElementById("btnCloseWebmailQRModal");
  if (btnCloseQR) btnCloseQR.onclick = closeStandaloneQRModal;

  const btnCancelQR = document.getElementById("btnCancelWebmailQRModal");
  if (btnCancelQR) btnCancelQR.onclick = closeStandaloneQRModal;

  const qrModalEl = document.getElementById("webmailQRModal");
  if (qrModalEl) {
    qrModalEl.onclick = (e) => {
      if (e.target === qrModalEl) closeStandaloneQRModal();
    };
  }

  const btnRefreshQR = document.getElementById("btnStandaloneRefreshQR");
  if (btnRefreshQR) btnRefreshQR.onclick = () => loadStandaloneQRCode();

  const btnMaskRefresh = document.getElementById("btnStandaloneQRRefreshMask");
  if (btnMaskRefresh) btnMaskRefresh.onclick = () => loadStandaloneQRCode();
}

function openStandaloneQRModal(onSuccessCallback) {
  const modal = document.getElementById("webmailQRModal");
  if (!modal) return;

  const tipEmail = document.getElementById("qrTargetEmailTip");
  const navAccount = document.getElementById("navAccountLabel");
  if (tipEmail) {
    tipEmail.innerText = (navAccount && navAccount.innerText) ? navAccount.innerText : "当前账号";
  }

  modal.classList.remove("hidden");
  stopStandaloneQRPoll();
  loadStandaloneQRCode(onSuccessCallback);
}

function closeStandaloneQRModal() {
  const modal = document.getElementById("webmailQRModal");
  if (modal) modal.classList.add("hidden");
  stopStandaloneQRPoll();
  isRefreshingHttp = false;
  loadStatus();
}

function stopStandaloneQRPoll() {
  if (standaloneQRPollTimer) {
    clearInterval(standaloneQRPollTimer);
    standaloneQRPollTimer = null;
  }
}

async function loadStandaloneQRCode(onSuccessCallback) {
  stopStandaloneQRPoll();

  const qrImg = document.getElementById("standaloneQRImg");
  const qrTip = document.getElementById("standaloneQRTip");
  const qrMask = document.getElementById("standaloneQRMask");

  if (qrMask) qrMask.classList.add("hidden");
  if (qrTip) qrTip.innerText = "⏳ 正在生成手机 QQ 官方登录二维码...";

  try {
    const res = await fetch("/api/webmail/qr");
    const data = await res.json();
    if (data.status === "ok" && data.qr_b64) {
      if (qrImg) qrImg.src = data.qr_b64;
      const qrsig = data.qrsig || "";
      if (qrTip) qrTip.innerText = "请使用当前网盘账号对应的【手机 QQ】扫码授权";

      standaloneQRPollTimer = setInterval(async () => {
        const modal = document.getElementById("webmailQRModal");
        if (!modal || modal.classList.contains("hidden")) {
          stopStandaloneQRPoll();
          return;
        }

        try {
          const cRes = await fetch("/api/webmail/qr_check", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ qrsig })
          });
          const cData = await cRes.json();
          if (cData.code === 0) {
            stopStandaloneQRPoll();
            closeStandaloneQRModal();
            // 扫码登录成功，先显示刷新动画并完成状态同步
            const btnHttpStatus = document.getElementById("btnHttpStatus");
            const httpStatusText = document.getElementById("httpStatusText");
            if (btnHttpStatus) btnHttpStatus.className = "status-pill-btn connecting";
            if (httpStatusText) httpStatusText.innerText = "HTTP通道 正在刷新...";
            isHttpConnected = true;
            sessionSkipHttpPrompt = false;
            await loadStatus();
            showToast("✔ 手机 QQ 扫码登录成功！HTTP 极速通道已激活！");
            if (typeof onSuccessCallback === "function") {
              onSuccessCallback();
            }
          } else if (cData.code === 67) {
            if (qrTip) qrTip.innerText = "📱 手机 QQ 已扫码，请在手机上点击确认登录";
          } else if (cData.code === 65) {
            stopStandaloneQRPoll();
            if (qrMask) {
              const maskText = document.getElementById("standaloneQRMaskText");
              if (maskText) maskText.innerText = "二维码已失效";
              qrMask.classList.remove("hidden");
            }
            if (qrTip) qrTip.innerText = "⚠️ 二维码已失效，请点击刷新";
          } else if (cData.code === -1 || cData.status === "error" || (cData.message && (cData.message.includes("安全拦截") || cData.message.includes("不一致")))) {
            stopStandaloneQRPoll();
            const errMsg = cData.message || "❌ 扫码账号与当前网盘账号不一致！";
            if (qrTip) qrTip.innerText = errMsg;
            if (qrMask) {
              const maskText = document.getElementById("standaloneQRMaskText");
              if (maskText) maskText.innerText = "账号不一致，已安全拦截";
              qrMask.classList.remove("hidden");
            }
          }
        } catch (pollErr) {
          // ignore transient poll error
        }
      }, 1800);
    } else {
      if (qrTip) qrTip.innerText = "获取二维码失败: " + (data.message || "");
    }
  } catch (err) {
    if (qrTip) qrTip.innerText = "网络异常: " + err.message;
  }
}

// 提速引导确认器：未连接 HTTP 极速通道时引导扫码提速；若点“暂不登录”，本次程序运行期间不再反复打扰，直接保底走 IMAP
function checkSpeedupChannel(actionCallback) {
  if (isHttpConnected || sessionSkipHttpPrompt) {
    if (typeof actionCallback === "function") actionCallback();
    return;
  }

  const modal = document.getElementById("speedupPromptModal");
  if (!modal) {
    if (typeof actionCallback === "function") actionCallback();
    return;
  }

  modal.classList.remove("hidden");

  const btnSkip = document.getElementById("btnSpeedupSkip");
  if (btnSkip) {
    btnSkip.onclick = () => {
      sessionSkipHttpPrompt = true; // 本次程序运行期间不再打扰，保底走 IMAP
      modal.classList.add("hidden");
      if (typeof actionCallback === "function") actionCallback();
    };
  }

  const btnScan = document.getElementById("btnSpeedupScan");
  if (btnScan) {
    btnScan.onclick = () => {
      modal.classList.add("hidden");
      openStandaloneQRModal(() => {
        if (typeof actionCallback === "function") actionCallback();
      });
    };
  }

  const btnClose = document.getElementById("btnCloseSpeedupPrompt");
  if (btnClose) {
    btnClose.onclick = () => {
      modal.classList.add("hidden");
    };
  }
}

// 发起下载请求（统一提速引导把关）
function requestWebDownload(files) {
  if (!files) return;
  const list = Array.isArray(files) ? files : [files];
  if (list.length === 0) return;
  checkSpeedupChannel(() => {
    list.forEach(f => startWebDownload(f));
  });
}

// ==================== 网页端独立并发下载管理器 ====================

let maxConcurrentTransfers = 3; // 默认最大并发数 3，可随设置调高

let configuredDownloadDir = "";
let configuredChunkSizeBytes = 52428799;

// ==================== 全局严格消息队列调度引擎 (Unified Strict FIFO Task Queue) ====================

// 发起下载任务：无论何时点击、勾选多少，统一放入全局待处理队列排队
function startWebDownload(f) {
  const taskId = "dl_" + Date.now() + "_" + Math.random().toString(36).substring(2, 7);
  const cSz = configuredChunkSizeBytes || 52428799;
  const calcChunks = (f.chunks && f.chunks.length > 0) ? f.chunks.length : Math.max(1, Math.ceil((f.size || 0) / cSz));

  clientTaskQueue.push({
    id: taskId,
    backend_id: null,
    type: "download",
    name: "📥 " + f.name,
    file_name: f.name,
    virtual_path: f.virtual_path,
    size: f.size,
    total: f.size,
    total_chunks: calcChunks,
    loaded: 0,
    progress: 0,
    speed: 0,
    status: "queued",
    start_time: Date.now()
  });

  logWebTransfer(`[队列] 加入下载排队: ${f.name}`);
  showToast(`已加入下载队列: ${f.name}`);
  renderSideTransferList();
  processQueueDispatcher();
}

// 调度器核心：单门流控出队，大文件下载满额关闸，每次出队严格按设置等待延时毫秒
async function processQueueDispatcher() {
  if (isDispatcherRunning) return;
  isDispatcherRunning = true;

  try {
    while (true) {
      // 1. 获取当前活跃的上传与下载任务数
      const activeUploads = clientTaskQueue.filter(t => t.type === "upload" && t.status === "running");
      const activeDownloads = clientTaskQueue.filter(t => t.type === "download" && t.status === "running");

      // 2. 严格受控上限检查
      // 寻找下一个处于 queued 状态的待执行任务 (FIFO 严格正序)
      const nextTask = clientTaskQueue.find(t => t.status === "queued");
      if (!nextTask) {
        break; // 队列中已无排队任务，调度器休眠
      }

      // 如果是上传：若当前已有上传在跑，关闸等待（大文件保持单通道一封一封传）
      if (nextTask.type === "upload" && activeUploads.length >= maxUploadConcurrency) {
        break;
      }

      // 如果是下载：若当前下载已达上限，关闸停止任何新请求，等待某一传输完成关闭才能继续
      if (nextTask.type === "download" && activeDownloads.length >= maxDownloadConcurrency) {
        break;
      }

      // 3. 准许出队启动该任务
      nextTask.status = "running";
      renderSideTransferList();
      executeQueueTask(nextTask);

      // 4. 核心流控铁律：每次派发任务出闸后，严格等待用户自定义设置的延时 (如 250ms/500ms)
      const delay = Math.max(0, configuredDelayMS);
      if (delay > 0) {
        await new Promise(r => setTimeout(r, delay));
      }
    }
  } finally {
    isDispatcherRunning = false;
  }
}

// 任务具体执行器
async function executeQueueTask(task) {
  if (task.type === "upload") {
    task.backend_id = task.id;
    const fd = new FormData();
    fd.append("file", task.file);
    fd.append("virtual_path", task.virtual_path);
    fd.append("task_id", task.id);
    if (task.thumb_base64) fd.append("thumb_base64", task.thumb_base64);

    logWebTransfer(`[上传] 调度出队开始传输: ${task.file_name}`);
    const uploadStart = Date.now();

    await new Promise((resolve) => {
      const xhr = new XMLHttpRequest();
      task.xhr = xhr;
      xhr.open("POST", "/api/upload");

      xhr.upload.onprogress = (e) => {
        if (task.status === "cancelled") return;
        if (e.lengthComputable && e.total > 0) {
          // 在后端切片尚未开始写入云端邮箱时，由本地打包进度真实驱动
          if (!task.backend_transferred || task.backend_transferred === 0) {
            task.loaded = e.loaded;
            task.total = e.total;
            task.progress = Math.min(99, Math.round((e.loaded / e.total) * 100));
            const elapsed = (Date.now() - uploadStart) / 1000;
            if (elapsed > 0) {
              task.speed = (e.loaded / (1024 * 1024)) / elapsed;
            }
            const totalC = task.total_chunks || 1;
            const doneCount = Math.min(totalC, Math.floor((e.loaded / e.total) * totalC));
            task.completed_chunks = [];
            for (let i = 1; i <= doneCount; i++) {
              task.completed_chunks.push(i);
            }
            if (doneCount < totalC) {
              task.current_chunk = doneCount + 1;
              task.active_chunks = [doneCount + 1];
            } else {
              task.active_chunks = [];
            }
            renderSideTransferList();
          }
        }
      };

      xhr.onload = () => {
        if (xhr.status >= 200 && xhr.status < 300) {
          try {
            const rJson = JSON.parse(xhr.responseText || "{}");
            if (rJson.stage_detail && rJson.stage_detail.includes("重复文件")) {
              task.is_duplicate = true;
              task.stage_detail = "目录下重复文件";
            }
          } catch (e) {}
          task.status = "completed";
          task.progress = 100;
          task.loaded = task.total;
          task.speed = 0;
          task.active_chunks = [];
          const totalC = Math.max(1, task.total_chunks || 1);
          task.completed_chunks = [];
          for (let i = 1; i <= totalC; i++) task.completed_chunks.push(i);
          loadFiles(currentPath);
          logWebTransfer(`✔ [上传成功] 文件已成功入库并同步至云端: ${task.file_name}`);
          showToast(`✔ 文件上传入库成功: ${task.file_name}`);
        } else {
          task.status = "failed";
          const errMsg = (xhr.responseText || `HTTP ${xhr.status}`).trim();
          logWebTransfer(`❌ [上传失败] ${task.file_name} 传输失败 (${xhr.status}): ${errMsg}`);
          showToast(`❌ 文件上传失败: ${errMsg}`, true);
        }
        renderSideTransferList();
        processQueueDispatcher();
        resolve();
      };

      xhr.onabort = () => {
        logWebTransfer(`[上传] 传输已由用户主动取消: ${task.file_name}`);
        resolve();
      };

      xhr.onerror = () => {
        if (task.status === "cancelled") {
          resolve();
          return;
        }
        task.status = "failed";
        logWebTransfer(`❌ [上传异常] ${task.file_name} 网络传输异常中断 (连接超时或套接字断开)`);
        showToast(`❌ 上传异常: ${task.file_name}`, true);
        renderSideTransferList();
        processQueueDispatcher();
        resolve();
      };

      xhr.send(fd);
    });
  } else if (task.type === "download") {
    try {
      logWebTransfer(`[下载] 调度出队启动原生流式解密: ${task.file_name}`);
      const res = await fetch(`/api/fs/download_local?path=${encodeURIComponent(task.virtual_path)}`);
      const data = await res.json();
      if (data.status === "ok") {
        task.backend_id = data.task_id;
      } else {
        task.status = "failed";
        const errDetail = data.message || "未知原因";
        logWebTransfer(`❌ [下载启动失败] ${task.file_name}: ${errDetail}`);
        showToast(`❌ 下载启动失败: ${errDetail}`, true);
        renderSideTransferList();
        processQueueDispatcher();
      }
    } catch (e) {
      task.status = "failed";
      logWebTransfer(`❌ [下载异常] ${task.file_name} 网络请求异常: ${e.message}`);
      showToast(`❌ 下载异常: ${e.message}`, true);
      renderSideTransferList();
      processQueueDispatcher();
    }
  } else if (task.type === "repair") {
    try {
      logWebTransfer("[重构] 启动云端数据对齐与主索引重构...");
      const res = await fetch("/api/health/repair", { method: "POST", body: new URLSearchParams({ action: "repair_all" }) });
      const data = await res.json();
      if (data.status === "ok") {
        task.backend_id = data.task_id || "health_repair";
        task.status = "completed";
        task.progress = 100;
        logWebTransfer(`✔ [重构完成] 补录 ${data.repaired_count || 0} 项，清理 ${data.ghost_count || 0} 死条目`);
        showToast(`✔ 云端对齐完成！补录 ${data.repaired_count || 0} 项，清理 ${data.ghost_count || 0} 项`);
        await loadFiles(currentPath);
        await loadStatus();
        renderSideTransferList();
        processQueueDispatcher();
      } else {
        task.status = "failed";
        renderSideTransferList();
        processQueueDispatcher();
      }
    } catch (e) {
      task.status = "failed";
      renderSideTransferList();
      processQueueDispatcher();
    }
  }
}

// 动态更新智能清空按钮 (一键清空所有已完成、失败与异常项)
function updateClearCompletedButton() {
  const btnClearComp = document.getElementById("btnClearCompleted");
  if (!btnClearComp) return;

  const hasCompleted = clientTaskQueue.some(t => t.status === "completed");
  const hasFailed = clientTaskQueue.some(t => t.status === "failed" || t.status === "cancelled");

  if (hasCompleted || hasFailed) {
    btnClearComp.style.display = "inline-flex";
    btnClearComp.innerText = (hasCompleted && hasFailed) ? "🧹 清空结束任务" : (hasCompleted ? "🧹 清空已完成" : "🧹 清理失败项");
    btnClearComp.onclick = async () => {
      try {
        await fetch("/api/tasks/clear_completed?type=all", { method: "POST" });
      } catch (e) {}
      clientTaskQueue = clientTaskQueue.filter(t => t.status !== "completed" && t.status !== "failed" && t.status !== "cancelled");
      renderSideTransferList();
      showToast("已清空所有已结束的任务记录");
      processQueueDispatcher();
    };
  } else {
    btnClearComp.style.display = "none";
  }
}

// 渲染左侧下部传输队列 (严格展示排队中、传输中与已完成，每个任务独立进度与流控控制)
// 状态与网速格式化
function getStatusSpeedText(item) {
  if (item.status === "completed") {
    if (item.type === "upload") return "✔ 上传完成";
    if (item.type === "share") return "✔ 分享已送达";
    if (item.type === "preview") return "✔ 已下载完毕";
    if (item.type === "share_recv") return "✔ 接收已入库";
    return "✔ 下载完成";
  }
  if (item.status === "queued") return "⏳ 排队等待中";
  if (item.status === "paused") return "⏸ 已暂停";
  if (item.status === "failed") return "❌ 传输失败";
  if (item.status === "cancelled") return "✖ 已取消";
  if (item.stage === "落盘与哈希核验" || (item.stage_detail && item.stage_detail.includes("核验"))) {
    return "💿 正在落盘组装与哈希核验";
  }
  if ((item.retrying_chunks && item.retrying_chunks.length > 0) || (item.stage_detail && (item.stage_detail.includes("重试") || item.stage_detail.includes("避让") || item.stage_detail.includes("频控")))) {
    return "⏳ 触发频控/重试避让中";
  }
  if (item.is_system) return "🔧 重构中";

  if (item.type === "upload") {
    if (item.status !== "completed" && (!item.stage || item.stage === "1/5 扫描与预处理" || item.stage === "2/5 本地打包与切片" || (item.stage_detail && (item.stage_detail.includes("扫描") || item.stage_detail.includes("打包") || item.stage_detail.includes("特征"))))) {
      return `正在打包校验: ${(item.speed || 0).toFixed(1)} MB/s`;
    }
    return `正在上传: ${(item.speed || 0).toFixed(1)} MB/s`;
  }
  if (item.type === "share") {
    if (item.stage_detail && (item.stage_detail.includes("清单") || item.stage_detail.includes("提货单"))) {
      return "📬 正在投递提货清单";
    }
    return "⚡ 正在推送切片";
  }
  if (item.type === "preview") {
    return `正在流式缓冲: ${(item.speed || 0).toFixed(1)} MB/s`;
  }
  if (item.type === "share_recv") {
    if (item.stage_detail && item.stage_detail.includes("归档")) return "📬 正在归档入库";
    const doneCnt = (item.completed_chunks || []).length;
    const totCnt = item.total_chunks || 1;
    return `📬 正在接收分享 (${doneCnt}/${totCnt})`;
  }
  return `正在下载: ${(item.speed || 0).toFixed(1)} MB/s`;
}

// 绑定单个传输卡片的交互事件 (暂停、恢复、直接取消、重试、系统提示)
function bindCardActions(card, item) {
  const btnToggle = card.querySelector(".btn-item-toggle");
  if (btnToggle) {
    btnToggle.onclick = async (e) => {
      e.stopPropagation();
      const id = card.dataset.id;
      const t = clientTaskQueue.find(x => x.id === id);
      if (t) {
        if (t.status === "queued") {
          t.status = "paused";
          if (!t.paused_at) {
            t.paused_at = Date.now();
            t.paused_elapsed_sec = 0;
          }
        } else if (t.status === "paused") {
          // 用户从暂停点击继续：立即恢复 running 并解冻计时器
          t.status = "running";
          t.userPausedAt = 0;
          if (t.paused_at) {
            t.total_paused_ms = (t.total_paused_ms || 0) + (Date.now() - t.paused_at);
            t.paused_at = null;
            t.paused_elapsed_sec = null;
          }
          processQueueDispatcher();
          if (t.backend_id) {
            await fetch(`/api/tasks/toggle?task_id=${encodeURIComponent(t.backend_id)}`, { method: "POST" });
          }
        } else {
          // 用户从运行点击暂停：立即挂起并冻结当前计时器
          t.status = "paused";
          t.userPausedAt = Date.now();
          if (!t.paused_at) {
            t.paused_at = Date.now();
            t.paused_elapsed_sec = Math.max(0, (t.paused_at - (t.start_time || t.paused_at) - (t.total_paused_ms || 0)) / 1000);
          }
          if (t.backend_id) {
            await fetch(`/api/tasks/toggle?task_id=${encodeURIComponent(t.backend_id)}`, { method: "POST" });
          }
        }
        renderSideTransferList();
      }
    };
  }

  // 绑定完成按钮 (点击后立即平滑移除此卡片，并通知后端清理)
  const btnDone = card.querySelector(".btn-item-done");
  if (btnDone) {
    btnDone.onclick = async (e) => {
      e.stopPropagation();
      const id = card.dataset.id;
      const t = clientTaskQueue.find(x => x.id === id);
      const backendId = t ? (t.backend_id || t.id) : id;
      try {
        await fetch(`/api/tasks/clear_completed?task_id=${encodeURIComponent(backendId)}`, { method: "POST" });
      } catch (err) {}
      clientTaskQueue = clientTaskQueue.filter(x => x.id !== id);
      card.remove();
      updateClearCompletedButton();
      const countBadge = document.getElementById("sideTransferCount");
      if (countBadge) countBadge.innerText = clientTaskQueue.length;
      if (clientTaskQueue.length === 0) {
        const container = document.getElementById("sideTransferList");
        if (container && !container.querySelector(".side-transfer-empty")) {
          container.innerHTML = `<div class="side-transfer-empty">暂无传输任务</div>`;
        }
      }
    };
  }

  // 绑定取消按钮 (直接取消，不弹任何二次确认框)
  const btnCancel = card.querySelector(".btn-item-cancel");
  if (btnCancel) {
    btnCancel.onclick = async (e) => {
      e.stopPropagation();
      const id = card.dataset.id;
      const t = clientTaskQueue.find(x => x.id === id);
      if (t) {
        t.status = "cancelled";
        if (t.xhr) {
          try { t.xhr.abort(); } catch (err) {}
        }
      }
      const backendId = t ? (t.backend_id || t.id) : id;
      try {
        await fetch(`/api/tasks/cancel?task_id=${encodeURIComponent(backendId)}&reason=user_click`, { method: "POST" });
      } catch (err) {}
      clientTaskQueue = clientTaskQueue.filter(x => x.id !== id);
      renderSideTransferList();
      showToast("✔ 传输任务已取消，关联临时数据已彻底清除");
      processQueueDispatcher();
    };
  }

  // 绑定重试按钮
  const btnRetry = card.querySelector(".btn-item-retry");
  if (btnRetry) {
    btnRetry.onclick = (e) => {
      e.stopPropagation();
      const id = card.dataset.id;
      const t = clientTaskQueue.find(x => x.id === id);
      if (t) {
        const backendId = t.backend_id || t.id;
        fetch(`/api/tasks/cancel?task_id=${encodeURIComponent(backendId)}&reason=user_retry`, { method: "POST" }).catch(() => {});
        t.backend_id = null;
        t.status = "queued";
        t.loaded = 0;
        t.progress = 0;
        t.completed_chunks = [];
        t.active_chunks = [];
        renderSideTransferList();
        showToast("已重新加入队列重试传输");
        processQueueDispatcher();
      }
    };
  }

  // 绑定不可取消系统任务提示
  const btnLocked = card.querySelector(".btn-item-locked");
  if (btnLocked) {
    btnLocked.onclick = (e) => {
      e.stopPropagation();
      showToast("⚠️ 当前任务处于系统关键维护阶段，为保障云端数据完整性，暂不支持中途取消。");
    };
  }
}

// 渲染左侧下部传输队列 (全面改造为就地局部靶向更新，彻底消除暴力 innerHTML 导致的鼠标悬停闪烁与动画重置)
function renderSideTransferList() {
  const container = document.getElementById("sideTransferList");
  const countBadge = document.getElementById("sideTransferCount");
  if (!container) return;

  if (countBadge) countBadge.innerText = clientTaskQueue.length;
  updateClearCompletedButton();

  if (clientTaskQueue.length === 0) {
    if (!container.querySelector(".side-transfer-empty")) {
      container.innerHTML = `<div class="side-transfer-empty">暂无传输任务</div>`;
    }
    return;
  }

  const emptyEl = container.querySelector(".side-transfer-empty");
  if (emptyEl) emptyEl.remove();

  const displayList = [...clientTaskQueue].reverse();
  const validIds = new Set(displayList.map(t => t.id));

  // 1. 移除已出队的过期卡片
  container.querySelectorAll(".transfer-item-card").forEach(card => {
    const id = card.dataset.id;
    if (!validIds.has(id)) {
      card.remove();
    }
  });

  // 2. 就地靶向更新卡片，节点常驻稳定，零闪烁
  displayList.forEach(item => {
    let card = container.querySelector(`.transfer-item-card[data-id="${item.id}"]`);

    if (!card) {
      // 首次添加卡片 (统一三行信息网格：严格等高 22px，等距 5px，左右列绝对对齐)
      const elapsedSec = Math.floor(getTaskElapsedSec(item));
      const timeStr = formatDuration(elapsedSec);
      const timeLabel = getTaskTimeLabel(item);

      const div = document.createElement("div");
      div.className = "transfer-item-card";
      div.dataset.id = item.id;
      div.dataset.status = item.status;
      div.innerHTML = `
        <div class="transfer-item-header">
          <span class="transfer-item-name" title="${escapeHtml(item.name)}">${escapeHtml(item.name)}</span>
        </div>
        ${(() => {
          const totalC = Math.max(1, item.total_chunks || 1);
          const cWidth = getGridAvailableWidth();
          const cols = calculateGridColumns(totalC, cWidth);
          return `<div class="transfer-chunk-grid ${totalC === 1 ? 'grid-single' : ''}" style="--grid-cols: ${cols}; grid-template-columns: repeat(${cols}, 1fr) !important;" title="分片状态点阵图 (共 ${totalC} 个分片)">
            ${renderChunkBlocks(item)}
          </div>`;
        })()}
        <div class="transfer-info-grid">
          <div class="transfer-info-row row-status">
            <span class="info-col-left transfer-item-status-speed" style="${item.status === 'completed' ? 'color: #10b981 !important; font-weight: 600 !important;' : ''}">${escapeHtml(getStatusSpeedText(item))}</span>
            <span class="info-col-right transfer-item-pct" style="font-size: 16px !important; font-weight: 700 !important; color: var(--primary) !important;">${item.progress}%</span>
          </div>
          <div class="transfer-info-row row-progress">
            <span class="info-col-left transfer-chunk-progress-text">${escapeHtml(getChunkProgressText(item))}</span>
            <span class="info-col-right transfer-item-size">${formatTransferSize(item.loaded)} / ${formatTransferSize(item.total)}</span>
          </div>
          <div class="transfer-info-row row-actions">
            <span class="info-col-left transfer-item-time-tag">${timeLabel}: ${timeStr}</span>
            <div class="info-col-right transfer-item-btns-group">${renderItemActionButtons(item)}</div>
          </div>
        </div>
      `;
      container.appendChild(div);
      bindCardActions(div, item);
      return;
    }

    // 就地局部精准微调文本与样式，DOM 节点不动
    const speedEl = card.querySelector(".transfer-item-status-speed");
    if (speedEl) {
      const text = getStatusSpeedText(item);
      if (speedEl.innerText !== text) speedEl.innerText = text;
      if (item.status === "completed") {
        speedEl.style.setProperty("color", "#10b981", "important");
        speedEl.style.setProperty("font-weight", "600", "important");
      } else {
        speedEl.style.removeProperty("color");
        speedEl.style.removeProperty("font-weight");
      }
    }

    const sizeEl = card.querySelector(".transfer-item-size");
    if (sizeEl) {
      const sizeText = `${formatTransferSize(item.loaded)}/${formatTransferSize(item.total)}`;
      if (sizeEl.innerText !== sizeText) sizeEl.innerText = sizeText;
    }

    const pctEl = card.querySelector(".transfer-item-pct");
    if (pctEl) {
      const pctText = `${item.progress}%`;
      if (pctEl.innerText !== pctText) pctEl.innerText = pctText;
      pctEl.style.setProperty("font-size", "16px", "important");
      pctEl.style.setProperty("font-weight", "700", "important");
    }

    const chunkTextEl = card.querySelector(".transfer-chunk-progress-text");
    if (chunkTextEl) {
      const cText = getChunkProgressText(item);
      if (chunkTextEl.innerText !== cText) chunkTextEl.innerText = cText;
    }

    const gridEl = card.querySelector(".transfer-chunk-grid");
    if (gridEl) {
      const totalC = Math.max(1, item.total_chunks || 1);
      const desiredClass = `transfer-chunk-grid ${totalC === 1 ? 'grid-single' : ''}`;
      if (gridEl.className !== desiredClass) gridEl.className = desiredClass;
      const cWidth = getGridAvailableWidth();
      const cols = calculateGridColumns(totalC, cWidth);
      if (totalC > 1) {
        gridEl.style.setProperty("--grid-cols", cols);
        gridEl.style.setProperty("grid-template-columns", `repeat(${cols}, 1fr)`, "important");
      } else {
        gridEl.style.removeProperty("--grid-cols");
        gridEl.style.removeProperty("grid-template-columns");
      }
      const newGridHtml = renderChunkBlocks(item);
      if (gridEl.innerHTML !== newGridHtml) {
        gridEl.innerHTML = newGridHtml;
      }
    }

    const timeTag = card.querySelector(".transfer-item-time-tag");
    if (timeTag) {
      const elapsedSec = Math.floor(getTaskElapsedSec(item));
      const timeStr = formatDuration(elapsedSec);
      const timeLabel = getTaskTimeLabel(item);
      const desiredTime = `${timeLabel}: ${timeStr}`;
      if (timeTag.innerText !== desiredTime) timeTag.innerText = desiredTime;
    }

    const prevStatus = card.dataset.status;
    if (prevStatus !== item.status) {
      card.dataset.status = item.status;
      const btnsGroup = card.querySelector(".transfer-item-btns-group");
      if (btnsGroup) {
        btnsGroup.innerHTML = renderItemActionButtons(item);
        bindCardActions(card, item);
      }
    } else {
      const btnToggle = card.querySelector(".btn-item-toggle");
      if (btnToggle) {
        const isPaused = item.status === "paused";
        const iconSpan = btnToggle.querySelector(".btn-toggle-icon");
        const textSpan = btnToggle.querySelector(".btn-toggle-text");
        const desiredIcon = isPaused ? "▶" : "⏸";
        const desiredText = isPaused ? "继续" : "暂停";
        if (iconSpan && iconSpan.innerText !== desiredIcon) iconSpan.innerText = desiredIcon;
        if (textSpan && textSpan.innerText !== desiredText) textSpan.innerText = desiredText;
        btnToggle.title = isPaused ? "继续传输" : "暂停传输";
      }
    }
  });
}

// ==================== 批量操作逻辑 ====================

function updateBatchBar() {
  const group = document.getElementById("batchActionGroup");
  const countTag = document.getElementById("batchCountTag");
  const globalCb = document.getElementById("globalSelectAll");
  const curFiles = getFilteredAndSortedFiles();

  if (group && countTag) {
    if (selectedPaths.size > 0) {
      group.classList.remove("hidden");
      countTag.innerText = `已选 ${selectedPaths.size} 项`;
    } else {
      group.classList.add("hidden");
    }
  }

  if (globalCb) {
    if (curFiles.length > 0 && curFiles.every(f => selectedPaths.has(f.virtual_path))) {
      globalCb.checked = true;
      globalCb.indeterminate = false;
    } else if (selectedPaths.size > 0) {
      globalCb.checked = false;
      globalCb.indeterminate = true;
    } else {
      globalCb.checked = false;
      globalCb.indeterminate = false;
    }
  }
}

async function executeBatchDelete(paths) {
  showToast(`⏳ 正在向 QQ 邮箱发送物理清除指令 (共 ${paths.length} 项)...`, false);
  showCloudLoading(true, "正在向 QQ 邮箱发送彻底删除指令并抹除云端切片...", "系统正在全盘匹配关联邮件并执行物理清除 (EXPUNGE)，请稍候");
  try {
    const res = await fetch("/api/fs/batch_delete", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ paths }),
    });
    const data = await res.json();
    showCloudLoading(false);
    if (data.status === "ok") {
      paths.forEach(p => selectedPaths.delete(p));
      updateBatchBar();
      logWebTransfer(`✔ [删除完成] 成功物理删除 ${paths.length} 项并同步更新主索引`);
      showToast(`✔ 成功彻底物理删除 ${paths.length} 项！关联切片已抹除，主索引已更新`);
      await loadFiles(currentPath);
      await loadStatus();
    } else {
      const errDetail = data.message || "请求错误";
      logWebTransfer(`❌ [删除失败] 物理删除失败: ${errDetail}`);
      showToast("删除失败: " + errDetail, true);
    }
  } catch (err) {
    showCloudLoading(false);
    logWebTransfer(`❌ [删除异常] 删除请求异常: ${err.message}`);
    showToast("删除请求异常: " + err.message, true);
  }
}

// 移动目标弹窗：现代化层级树状图 (无刷新平滑折叠 + 紧凑10px缩进 + 跨空间深层目录展开)
let selectedMoveDestFolder = ""; // 跨空间目标，空代表当前空间
let currentMoveTargetPaths = [];

let transferActionType = "move"; // "move" or "copy"

function openMoveModal(paths) {
  transferActionType = "move";
  openTransferPickerModal(paths, "move");
}

function openCopyModal(paths) {
  transferActionType = "copy";
  openTransferPickerModal(paths, "copy");
}

function openTransferPickerModal(paths, actionType) {
  if (!paths || !Array.isArray(paths)) {
    paths = currentMoveTargetPaths && currentMoveTargetPaths.length > 0 ? currentMoveTargetPaths : (contextMenuTarget ? [contextMenuTarget.virtual_path] : []);
  }
  if (!paths || paths.length === 0) return;
  currentMoveTargetPaths = paths;
  moveTargetPaths = paths;
  selectedMoveDest = "/";
  selectedMoveDestFolder = "";

  const modal = document.getElementById("moveModal");
  const modalTitle = document.getElementById("moveModalTitle");
  const desc = document.getElementById("moveTargetsDesc");
  const picker = document.getElementById("moveFolderPicker");
  const btnConfirm = document.getElementById("btnConfirmMove");

  if (!modal || !picker) return;

  const actionName = (actionType === "copy") ? "复制" : "移动";
  if (modalTitle) {
    modalTitle.innerText = (actionType === "copy") ? "📋 复制到目标文件夹" : "📦 移动到目标文件夹";
  }
  if (btnConfirm) {
    btnConfirm.innerText = (actionType === "copy") ? "确认复制" : "确认移动";
  }
  desc.innerText = `正在${actionName} ${paths.length} 个项目，请选择目标文件夹：`;
  modal.classList.remove("hidden");

  // 构建常驻 DOM 树
  buildMoveModalPermanentTree(paths);
}

function updateMoveTargetSummaryText() {
  let targetSummary = document.getElementById("moveTargetSummaryText");
  const picker = document.getElementById("moveFolderPicker");
  if (!targetSummary && picker) {
    targetSummary = document.createElement("div");
    targetSummary.id = "moveTargetSummaryText";
    targetSummary.style.marginTop = "10px";
    targetSummary.style.fontSize = "13px";
    targetSummary.style.fontWeight = "600";
    picker.parentNode.insertBefore(targetSummary, picker.nextSibling);
  }
  if (!targetSummary) return;

  const currentSpaceDisplayName = cleanFolderDisplayName(currentActiveStorageFolder) || "当前空间";

  if (selectedMoveDestFolder) {
    const destName = (selectedMoveDest === "/" ? "(根目录)" : selectedMoveDest);
    targetSummary.innerHTML = `📍 目标位置：<span style="color:#0284c7; font-weight:700;">[${escapeHtml(selectedMoveDestFolder)}]</span> -> ${escapeHtml(destName)}`;
  } else {
    const destName = (selectedMoveDest === "/" ? "(根目录)" : selectedMoveDest);
    targetSummary.innerHTML = `📍 目标位置：<span style="color:#0284c7; font-weight:700;">[${escapeHtml(currentSpaceDisplayName)}]</span> -> ${escapeHtml(destName)}`;
  }
}

function buildMoveModalPermanentTree(paths) {
  const picker = document.getElementById("moveFolderPicker");
  if (!picker) return;
  picker.innerHTML = "";

  const currentSpaceDisplayName = cleanFolderDisplayName(currentActiveStorageFolder) || "当前空间";

  // 1. 当前空间目录树容器 (取消外层冗余标题，首行直呈空间根节点)
  const currentSpaceCard = document.createElement("div");
  currentSpaceCard.className = "move-space-group";

  const treeBox = document.createElement("div");
  treeBox.className = "move-tree-box";

  function getDirectSubDirs(parentPath) {
    const isRoot = (parentPath === "/");
    return filesData.filter(f => {
      if (!f.is_dir) return false;
      if (isRoot) {
        const parts = f.virtual_path.split("/").filter(Boolean);
        return parts.length === 1;
      }
      return f.virtual_path.startsWith(parentPath + "/") && f.virtual_path.split("/").filter(Boolean).length === parentPath.split("/").filter(Boolean).length + 1;
    }).sort((a, b) => a.name.localeCompare(b.name));
  }

  // 递归创建紧凑节点 (每层缩进 6px，紧贴侧边)
  function createPermanentNode(path, name, level) {
    if (transferActionType === "move") {
      const isSelfOrSub = paths.some(p => path === p || path.startsWith(p + "/"));
      if (isSelfOrSub) return null;
    } else {
      // 复制模式：仅对文件夹禁止复制进其自身或其子目录，文件允许复制到当前目录
      const isSelfOrSubDir = paths.some(p => {
        const f = filesData.find(x => x.virtual_path === p);
        return f && f.is_dir && (path === p || path.startsWith(p + "/"));
      });
      if (isSelfOrSubDir) return null;
    }

    const nodeWrap = document.createElement("div");
    nodeWrap.className = "move-tree-node-wrap";

    const subDirs = getDirectSubDirs(path);
    const hasChildren = subDirs.length > 0;

    const row = document.createElement("div");
    row.className = "move-tree-row";
    row.dataset.path = path;
    row.dataset.space = "";
    // 紧凑 6px 缩进对齐图 1
    row.style.paddingLeft = `${level * 6 + 2}px`;
    row.style.display = "flex";
    row.style.alignItems = "center";
    row.style.gap = "4px";
    row.style.paddingTop = "4px";
    row.style.paddingBottom = "4px";
    row.style.cursor = "pointer";
    row.style.borderRadius = "4px";

    if (path === selectedMoveDest && selectedMoveDestFolder === "") {
      row.classList.add("selected");
    }

    const arrow = document.createElement("span");
    arrow.style.fontSize = "10px";
    arrow.style.width = "12px";
    arrow.style.textAlign = "center";
    arrow.style.userSelect = "none";
    arrow.style.color = "#64748b";

    let subTreeContainer = null;
    const toggleFolderExpand = () => {
      if (hasChildren && subTreeContainer) {
        const isOpen = (subTreeContainer.style.display === "block");
        subTreeContainer.style.display = isOpen ? "none" : "block";
        arrow.innerText = isOpen ? "▶" : "▼";
      }
    };

    const shouldExpand = (path === "/" || (selectedMoveDest && selectedMoveDest.startsWith(path + "/")));
    if (hasChildren) {
      arrow.innerText = shouldExpand ? "▼" : "▶";
      subTreeContainer = document.createElement("div");
      subTreeContainer.className = "move-sub-tree";
      subTreeContainer.style.display = shouldExpand ? "block" : "none";
    } else {
      arrow.innerHTML = "&nbsp;";
    }

    const textSpan = document.createElement("span");
    textSpan.innerHTML = `📁 ${escapeHtml(name)}`;
    textSpan.style.flex = "1";
    textSpan.style.fontSize = "13px";
    textSpan.style.cursor = "pointer";

    row.appendChild(arrow);
    row.appendChild(textSpan);

    const handlePermanentRowClick = (e) => {
      if (e) e.stopPropagation();
      selectedMoveDestFolder = "";
      selectedMoveDest = path;
      picker.querySelectorAll(".move-tree-row, .move-dest-item").forEach(el => el.classList.remove("selected"));
      row.classList.add("selected");
      updateMoveTargetSummaryText();
      // 点击整行选择框（包含文字、图标、空白区域及三角指示）直接展开/折叠文件夹
      toggleFolderExpand();
    };

    row.onclick = handlePermanentRowClick;
    arrow.onclick = handlePermanentRowClick;
    textSpan.onclick = handlePermanentRowClick;

    nodeWrap.appendChild(row);

    if (hasChildren && subTreeContainer) {
      subDirs.forEach(sub => {
        const childNode = createPermanentNode(sub.virtual_path, sub.name, level + 1);
        if (childNode) subTreeContainer.appendChild(childNode);
      });
      nodeWrap.appendChild(subTreeContainer);
    }

    return nodeWrap;
  }

  const rootNode = createPermanentNode("/", `${currentSpaceDisplayName} (根目录)`, 0);
  if (rootNode) treeBox.appendChild(rootNode);
  currentSpaceCard.appendChild(treeBox);
  picker.appendChild(currentSpaceCard);

  // 2. 跨隔离空间物理迁移区域 (按需动态加载并渲染树状层级)
  fetch("/api/folders").then(r => r.json()).then(data => {
    if (data.status === "ok" && data.folders && data.folders.length > 1) {
      const otherFolders = data.folders.filter(f => !f.is_current);
      if (otherFolders.length > 0) {
        const otherCard = document.createElement("div");
        otherCard.className = "move-space-group";
        otherCard.style.marginTop = "14px";
        const sectionTitle = (transferActionType === "copy") ? "🔀 复制至其他隔离空间 (跨分区复制)：" : "🔀 移动至其他隔离空间 (跨分区迁移)：";
        otherCard.innerHTML = `<div class="move-space-title" style="font-size:13px; font-weight:700; color:var(--text-main); margin-bottom:8px;">${sectionTitle}</div>`;

        const otherList = document.createElement("div");
        otherList.className = "move-tree-box";
        otherList.style.maxHeight = "400px";

        otherFolders.forEach(of => {
          const spaceName = of.display_name || of.name;
          const spaceWrap = document.createElement("div");
          spaceWrap.className = "move-space-wrap";
          spaceWrap.style.marginBottom = "4px";

          const spaceRow = document.createElement("div");
          spaceRow.className = "move-tree-row";
          spaceRow.dataset.space = spaceName;
          spaceRow.dataset.path = "/";
          spaceRow.style.padding = "5px 6px";
          spaceRow.style.borderRadius = "4px";
          spaceRow.style.display = "flex";
          spaceRow.style.alignItems = "center";
          spaceRow.style.gap = "4px";
          spaceRow.style.cursor = "pointer";

          const arrow = document.createElement("span");
          arrow.style.fontSize = "10px";
          arrow.style.width = "12px";
          arrow.style.textAlign = "center";
          arrow.style.color = "#64748b";
          arrow.innerText = "▶";

          const label = document.createElement("span");
          label.style.flex = "1";
          label.style.fontSize = "13px";
          label.innerHTML = `<span>📁</span> <strong>${escapeHtml(spaceName)} (根目录)</strong>`;

          spaceRow.appendChild(arrow);
          spaceRow.appendChild(label);

          const spaceSubTree = document.createElement("div");
          spaceSubTree.className = "space-sub-tree";
          spaceSubTree.style.display = "none";
          spaceSubTree.style.paddingLeft = "8px";

          let isLoadedSubDirs = false;
          const toggleExpandOther = async () => {
            const isOpen = (spaceSubTree.style.display === "block");
            if (isOpen) {
              spaceSubTree.style.display = "none";
              arrow.innerText = "▶";
            } else {
              if (!isLoadedSubDirs) {
                arrow.innerText = "⏳";
                try {
                  const sRes = await fetch(`/api/folders/subdirs?folder=${encodeURIComponent(spaceName)}`);
                  const sData = await sRes.json();
                  spaceSubTree.innerHTML = "";
                  if (sData.status === "ok" && sData.dir_nodes) {
                    // 构建真实的跨分区多层级折叠树 (恢复小三角，默认折叠，禁止全部平铺展开)
                    function getOtherDirectSubDirs(parentPath, allDirs) {
                      const isRoot = (parentPath === "/");
                      const pParts = parentPath.split("/").filter(Boolean);
                      return allDirs.filter(d => {
                        if (d.virtual_path === "/") return false;
                        const dParts = d.virtual_path.split("/").filter(Boolean);
                        if (isRoot) return dParts.length === 1;
                        return d.virtual_path.startsWith(parentPath + "/") && dParts.length === pParts.length + 1;
                      }).sort((a, b) => a.name.localeCompare(b.name));
                    }

                    function createOtherSpaceNode(path, name, level, allDirs) {
                      const nodeWrap = document.createElement("div");
                      nodeWrap.className = "move-tree-node-wrap";

                      const subDirs = getOtherDirectSubDirs(path, allDirs);
                      const hasChildren = subDirs.length > 0;

                      const row = document.createElement("div");
                      row.className = "move-tree-row";
                      row.dataset.space = spaceName;
                      row.dataset.path = path;
                      row.style.paddingLeft = `${level * 6 + 14}px`;
                      row.style.paddingTop = "3px";
                      row.style.paddingBottom = "3px";
                      row.style.fontSize = "12.5px";
                      row.style.cursor = "pointer";
                      row.style.borderRadius = "4px";
                      row.style.display = "flex";
                      row.style.alignItems = "center";
                      row.style.gap = "4px";

                      const subArrow = document.createElement("span");
                      subArrow.style.fontSize = "10px";
                      subArrow.style.width = "12px";
                      subArrow.style.textAlign = "center";
                      subArrow.style.userSelect = "none";
                      subArrow.style.color = "#64748b";

                      let subContainer = null;
                      const toggleSubExpand = () => {
                        if (hasChildren && subContainer) {
                          const isOpen = (subContainer.style.display === "block");
                          subContainer.style.display = isOpen ? "none" : "block";
                          subArrow.innerText = isOpen ? "▶" : "▼";
                        }
                      };

                      if (hasChildren) {
                        subArrow.innerText = "▶"; // 默认折叠，绝不全展开！
                        subContainer = document.createElement("div");
                        subContainer.className = "move-sub-tree";
                        subContainer.style.display = "none"; // 默认闭合！
                      } else {
                        subArrow.innerHTML = "&nbsp;";
                      }

                      const textSpan = document.createElement("span");
                      textSpan.innerHTML = `📁 ${escapeHtml(name)}`;
                      textSpan.style.flex = "1";
                      textSpan.style.cursor = "pointer";

                      row.appendChild(subArrow);
                      row.appendChild(textSpan);

                      const handleOtherRowClick = (se) => {
                        if (se) se.stopPropagation();
                        selectedMoveDestFolder = spaceName;
                        selectedMoveDest = path;
                        picker.querySelectorAll(".move-tree-row, .move-dest-item").forEach(el => el.classList.remove("selected"));
                        row.classList.add("selected");
                        updateMoveTargetSummaryText();
                        // 点击整行选择框直接展开/折叠子文件夹
                        toggleSubExpand();
                      };

                      row.onclick = handleOtherRowClick;
                      subArrow.onclick = handleOtherRowClick;
                      textSpan.onclick = handleOtherRowClick;

                      nodeWrap.appendChild(row);

                      if (hasChildren && subContainer) {
                        subDirs.forEach(sub => {
                          const childEl = createOtherSpaceNode(sub.virtual_path, sub.name, level + 1, allDirs);
                          if (childEl) subContainer.appendChild(childEl);
                        });
                        nodeWrap.appendChild(subContainer);
                      }

                      return nodeWrap;
                    }

                    spaceSubTree.innerHTML = "";
                    const rootSubs = getOtherDirectSubDirs("/", sData.dir_nodes);
                    rootSubs.forEach(sub => {
                      const childEl = createOtherSpaceNode(sub.virtual_path, sub.name, 1, sData.dir_nodes);
                      if (childEl) spaceSubTree.appendChild(childEl);
                    });
                  }
                  isLoadedSubDirs = true;
                } catch (err) {
                  spaceSubTree.innerHTML = `<div style="font-size:11.5px; color:red; padding:4px;">无法读取该空间子目录: ${err.message}</div>`;
                }
              }
              spaceSubTree.style.display = "block";
              arrow.innerText = "▼";
            }
          };

          const handleSpaceClick = (e) => {
            if (e) e.stopPropagation();
            selectedMoveDestFolder = spaceName;
            selectedMoveDest = "/";
            picker.querySelectorAll(".move-tree-row, .move-dest-item").forEach(el => el.classList.remove("selected"));
            spaceRow.classList.add("selected");
            updateMoveTargetSummaryText();
            // 点击整行选择框直接展开/折叠该分区子目录
            toggleExpandOther();
          };

          spaceRow.onclick = handleSpaceClick;
          arrow.onclick = handleSpaceClick;
          label.onclick = handleSpaceClick;

          spaceWrap.appendChild(spaceRow);
          spaceWrap.appendChild(spaceSubTree);
          otherList.appendChild(spaceWrap);
        });

        otherCard.appendChild(otherList);
        picker.appendChild(otherCard);
      }
    }
  }).catch(()=>{});

  updateMoveTargetSummaryText();
}
// ==================== 底部可观测性任务浮层 (状态栏常驻，进度条在顶端且无传输时隐藏) ====================

let lastRenderedLogSignature = "";

function startTaskPolling() {
  if (pollingTimer) clearInterval(pollingTimer);
  pollingTimer = setInterval(async () => {
    try {
      const res = await fetch("/api/tasks");
      if (!res.ok) throw new Error("HTTP " + res.status);
      const list = await res.json() || [];

      backendFailCount = 0;
      if (isBackendOffline) {
        setBackendOfflineUI(false);
        showToast("✔ 已恢复与 QQMailDrive 本地服务端的连接");
        loadStatus();
        loadFiles(currentPath);
      }
      activeTasks = list;

      // 实时同步后端传输进度与状态到 clientTaskQueue
      let hasCompleted = false;
      activeTasks.forEach(bt => {
        // 匹配客户端任务：优先按唯一 task_id 强绑定精确匹配，杜绝同名任务串号劫持与失败死锁
        let ct = clientTaskQueue.find(t => t.id === bt.task_id || (t.backend_id && t.backend_id === bt.task_id));
        if (!ct && bt.status === "running") {
          ct = clientTaskQueue.find(t => t.type === bt.type && t.file_name === bt.file_name && t.status === "running");
        }
        if (!ct && (bt.type === "share" || bt.type === "download" || bt.type === "share_recv")) {
          const isRecv = (bt.type === "share_recv" || (bt.type === "download" && bt.stage_detail && bt.stage_detail.includes("收到的文件")));
          const taskType = isRecv ? "share_recv" : bt.type;
          let displayName = bt.file_name;
          if (isRecv) {
            displayName = "📬 " + bt.file_name;
          } else if (bt.type === "share") {
            displayName = "🔗 " + bt.file_name + " ➔ " + (bt.target_email || "好友");
          } else if (bt.type === "download") {
            displayName = "📥 " + bt.file_name;
          } else if (bt.type === "upload") {
            displayName = "📤 " + bt.file_name;
          }
          clientTaskQueue.push({
            id: bt.task_id,
            backend_id: bt.task_id,
            type: taskType,
            name: displayName,
            file_name: bt.file_name,
            virtual_path: bt.virtual_path,
            size: bt.total_size,
            total: bt.total_size,
            loaded: bt.transferred,
            progress: Math.round(bt.progress_pct || 0),
            speed: bt.speed_mbs || 0,
            status: bt.status,
            total_chunks: bt.total_chunks,
            completed_chunks: bt.completed_chunks || [],
            writing_chunks: bt.writing_chunks || [],
            active_chunks: bt.active_chunks || [],
            start_time: Date.now()
          });
          if (bt.status === "completed") {
            hasCompleted = true;
          }
        }
        if (!ct && bt.type === "preview") {
          const isDone = bt.status === "completed";
          let parsedStartTime = Date.now();
          if (bt.created_at) {
            const t = typeof bt.created_at === "number" ? (bt.created_at > 1e11 ? bt.created_at : bt.created_at * 1000) : new Date(bt.created_at).getTime();
            if (!isNaN(t) && t > 0) parsedStartTime = t;
          }
          clientTaskQueue.push({
            id: bt.task_id,
            backend_id: bt.task_id,
            type: "preview",
            name: "🎬 " + bt.file_name + (isDone ? "" : " (在线缓冲)"),
            file_name: bt.file_name,
            virtual_path: bt.virtual_path,
            size: bt.total_size,
            total: bt.total_size,
            loaded: isDone ? bt.total_size : bt.transferred,
            progress: isDone ? 100 : Math.round(bt.progress_pct || 0),
            speed: isDone ? 0 : (bt.speed_mbs || 0),
            status: bt.status,
            stage: bt.stage,
            stage_detail: bt.stage_detail,
            total_chunks: bt.total_chunks,
            completed_chunks: bt.completed_chunks || [],
            writing_chunks: bt.writing_chunks || [],
            active_chunks: bt.active_chunks || [],
            start_time: parsedStartTime,
            completed_at: isDone ? Date.now() : null
          });
          if (isDone) {
            hasCompleted = true;
          }
        }
        if (ct) {
          ct.backend_id = bt.task_id;
          ct.stage = bt.stage;
          ct.stage_detail = bt.stage_detail;
          if (!ct.start_time && bt.created_at) {
            const t = typeof bt.created_at === "number" ? (bt.created_at > 1e11 ? bt.created_at : bt.created_at * 1000) : new Date(bt.created_at).getTime();
            if (!isNaN(t) && t > 0) ct.start_time = t;
          }
          if (bt.type) {
            const isRecv = (bt.type === "share_recv" || (bt.type === "download" && bt.stage_detail && bt.stage_detail.includes("收到的文件")));
            ct.type = isRecv ? "share_recv" : bt.type;
          }
          if (bt.transferred > 0 || bt.status === "completed" || (bt.completed_chunks && bt.completed_chunks.length > 0)) {
            ct.loaded = (bt.status === "completed") ? (bt.total_size || ct.total) : bt.transferred;
            ct.total = bt.total_size || ct.total;
            if (bt.status === "completed") {
              ct.progress = 100;
              ct.speed = 0;
            } else {
              ct.progress = Math.min(99, Math.round(bt.progress_pct || 0));
            }
          }
          if (bt.status === "completed") {
            ct.status = "completed";
            ct.progress = 100;
            ct.loaded = ct.total;
            if (!ct.completed_chunks || ct.completed_chunks.length < (ct.total_chunks || 1)) {
              const tot = ct.total_chunks || 1;
              const allChunks = [];
              for (let i = 1; i <= tot; i++) allChunks.push(i);
              ct.completed_chunks = allChunks;
            }
            if (ct.elapsed_sec === undefined || ct.elapsed_sec === null) {
              const end = Date.now();
              ct.completed_at = end;
              ct.elapsed_sec = Math.max(0, (end - (ct.start_time || end) - (ct.total_paused_ms || 0)) / 1000);
            }
          } else if (bt.status === "running") {
            ct.status = "running";
            ct.elapsed_sec = null;
            ct.completed_at = null;
          } else {
            ct.status = bt.status;
          }
          ct.speed = bt.speed_mbs || 0;
          ct.total_chunks = bt.total_chunks || ct.total_chunks;
          ct.completed_chunks = bt.completed_chunks || [];
          ct.writing_chunks = bt.writing_chunks || [];
          ct.retrying_chunks = bt.retrying_chunks || [];
          ct.active_chunks = bt.active_chunks || [];
          ct.current_chunk = bt.current_chunk;

          // 若用户刚点击了暂停（3秒内锁定），防止轮询竞态将 paused 冲刷回 running
          const isUserPausedLock = ct.status === "paused" && ct.userPausedAt && (Date.now() - ct.userPausedAt < 3000);
          if (!isUserPausedLock) {
            if (ct.status === "paused" && bt.status === "running") {
              if (ct.paused_at) {
                ct.total_paused_ms = (ct.total_paused_ms || 0) + (Date.now() - ct.paused_at);
                ct.paused_at = null;
                ct.paused_elapsed_sec = null;
              }
            } else if (ct.status !== "paused" && bt.status === "paused") {
              if (!ct.paused_at) {
                ct.paused_at = Date.now();
                ct.paused_elapsed_sec = Math.max(0, (ct.paused_at - (ct.start_time || ct.paused_at) - (ct.total_paused_ms || 0)) / 1000);
              }
            }
            ct.status = bt.status;
            if (bt.status === "running") {
              ct.elapsed_sec = null;
              ct.completed_at = null;
            }
          }

          if (bt.status === "completed" || bt.status === "failed") {
            if (ct.elapsed_sec === undefined || ct.elapsed_sec === null) {
              const end = Date.now();
              ct.completed_at = end;
              ct.elapsed_sec = Math.max(0, (end - (ct.start_time || end) - (ct.total_paused_ms || 0)) / 1000);
            }
            if (ct.status !== bt.status) {
              ct.status = bt.status;
              hasCompleted = true;
              if (bt.status === "completed") {
                // 上传成功已在出队响应中通知，此处仅通知后台下载完成，彻底避免重复提示两次
                if (bt.type === "download") {
                  showToast(`✔ 文件下载解密完成: ${bt.file_name}`);
                }
              }
            }
          } else if (bt.status === "paused") {
            ct.status = "paused";
            if (!ct.paused_at) {
              ct.paused_at = Date.now();
              ct.paused_elapsed_sec = Math.max(0, (ct.paused_at - (ct.start_time || ct.paused_at) - (ct.total_paused_ms || 0)) / 1000);
            }
          }
        }
      });

      if (hasCompleted) {
        await loadFiles(currentPath);
        await loadStatus();
        await updateUnifiedLogs(true);
        // 传输席位释放，立即唤醒调度器处理后续排队任务
        processQueueDispatcher();
      }

      if (clientTaskQueue.some(t => t.status === "queued")) {
        processQueueDispatcher();
      }

      // 后台静默收件索引变更自愈感知：每 2 秒静默对齐 index_ver，若发生变动立即触发 loadFiles 刷新界面
      if (typeof window.__lastIndexVerCheck === "undefined") window.__lastIndexVerCheck = 0;
      if (Date.now() - window.__lastIndexVerCheck > 1500) {
        window.__lastIndexVerCheck = Date.now();
        fetch("/api/status").then(r => {
          if (!r.ok) throw new Error("HTTP " + r.status);
          return r.json();
        }).then(sData => {
          backendFailCount = 0;
          if (isBackendOffline) {
            setBackendOfflineUI(false);
            loadStatus();
          }
          if (sData && typeof sData.index_ver === "number") {
            if (typeof window.__lastKnownIndexVer === "number" && sData.index_ver !== window.__lastKnownIndexVer) {
              window.__lastKnownIndexVer = sData.index_ver;
              loadFiles(currentPath);
              loadStatus();
            } else {
              window.__lastKnownIndexVer = sData.index_ver;
            }
          }
          if (sData && typeof sData.cache_rev === "number") {
            if (typeof window.__lastKnownCacheRev === "number" && sData.cache_rev !== window.__lastKnownCacheRev) {
              window.__lastKnownCacheRev = sData.cache_rev;
              loadIsolatedSpaces();
            } else {
              window.__lastKnownCacheRev = sData.cache_rev;
            }
          }
        }).catch(() => {
          backendFailCount++;
          if (backendFailCount >= 2) {
            setBackendOfflineUI(true);
          }
        });
      }

      updateTaskDock();
      renderSideTransferList();
      await updateUnifiedLogs(false);
    } catch (e) {
      backendFailCount++;
      if (backendFailCount >= 2) {
        setBackendOfflineUI(true);
      }
    }
  }, 400);
}

function updateTaskDock() {
  const pauseBtn = document.getElementById("btnPauseTask");
  if (!pauseBtn) return;

  const runningOrPaused = activeTasks.filter(t => t.status === "running" || t.status === "paused");
  if (runningOrPaused.length === 0) {
    pauseBtn.classList.add("hidden");
  } else {
    pauseBtn.classList.remove("hidden");
    const isPaused = runningOrPaused.every(t => t.status === "paused");
    pauseBtn.innerText = isPaused ? "▶️ 继续" : "⏸ 暂停";
  }
}

let currentLogLevel = "ALL";
let currentLogModule = "ALL";
let logSearchKeyword = "";
let isLogAutoScroll = true;
let cachedStructuredLogs = [];

// 全量任务统一实时滚屏日志 (四级日志过滤 DEBUG/INFO/WARN/ERROR、模块筛选、关键字实时检索)
async function updateUnifiedLogs(force = false) {
  const consoleEl = document.getElementById("logConsole");
  const drawer = document.getElementById("logDrawer");
  if (!consoleEl) return;

  if (!force && drawer && drawer.classList.contains("hidden")) {
    return;
  }

  try {
    const res = await fetch("/api/logs?format=json&limit=20000");
    let serverLogs = [];
    if (res.ok) {
      serverLogs = await res.json() || [];
    }

    // 前端自产日志在发生时已精准记录 timestamp，直接与服务端日志无损合并
    const allEntries = [...serverLogs, ...webClientLogs];
    if (allEntries.length === 0) return;

    // 稳定毫秒级时序排序：基于真实发生时间排序，毫秒或秒相同严格保持原入队顺序，彻底修复时间颠倒错位
    allEntries.sort((a, b) => {
      const tA = a.timestamp ? new Date(a.timestamp).getTime() : 0;
      const tB = b.timestamp ? new Date(b.timestamp).getTime() : 0;
      if (tA && tB && tA !== tB) {
        return tA - tB;
      }
      const sA = a.time_str || "";
      const sB = b.time_str || "";
      if (sA !== sB) {
        return sA.localeCompare(sB);
      }
      return 0;
    });

    cachedStructuredLogs = allEntries;

    let filtered = allEntries;

    // 1. 等级过滤
    if (currentLogLevel !== "ALL") {
      filtered = filtered.filter(e => (e.level || "INFO").toUpperCase() === currentLogLevel);
    }

    // 2. 模块过滤
    if (currentLogModule !== "ALL") {
      filtered = filtered.filter(e => (e.module || "SYSTEM").toUpperCase().includes(currentLogModule));
    }

    // 3. 关键字搜索过滤
    if (logSearchKeyword) {
      const kw = logSearchKeyword.toLowerCase();
      filtered = filtered.filter(e => 
        (e.message || "").toLowerCase().includes(kw) || 
        (e.module || "").toLowerCase().includes(kw) ||
        (e.time_str || "").includes(kw)
      );
    }

    const sig = `${currentLogLevel}_${currentLogModule}_${logSearchKeyword}_${filtered.length}_${filtered[filtered.length - 1]?.message || ""}`;
    if (!force && sig === lastRenderedLogSignature) {
      return;
    }
    lastRenderedLogSignature = sig;

    if (filtered.length === 0) {
      consoleEl.innerHTML = `<div style="color: #64748b; padding: 12px;">[筛选无匹配记录] 当前筛选条件下暂无日志条目</div>`;
      return;
    }

    let renderList = filtered;
    if (renderList.length > 5000) {
      renderList = renderList.slice(renderList.length - 5000);
    }
    consoleEl.innerHTML = renderList.map(item => {
      const lvl = (item.level || "INFO").toUpperCase();
      const mod = item.module || "SYSTEM";
      const time = item.time_str || "";
      const msg = item.message || "";

      let lvlClass = "log-badge-info";
      let textCol = "#cbd5e1";
      if (lvl === "DEBUG") { lvlClass = "log-badge-debug"; textCol = "#38bdf8"; }
      else if (lvl === "WARN") { lvlClass = "log-badge-warn"; textCol = "#fbbf24"; }
      else if (lvl === "ERROR") { lvlClass = "log-badge-error"; textCol = "#f87171"; }
      else if (msg.includes("✔") || msg.includes("成功")) { textCol = "#34d399"; }

      // 频控避让提示专属调色：采用醒目但温润舒缓的暖杏橙色 (#fb923c)，既能一眼聚焦识别又不会过度刺眼
      if (msg.includes("频控避让") || msg.includes("频率控制") || msg.includes("-20003")) {
        lvlClass = "log-badge-throttle";
        textCol = "#fb923c";
      }

      return `
        <div class="log-entry-row" style="color: ${textCol};">
          <span style="color: #64748b;">[${escapeHtml(time)}]</span>
          <span class="log-badge ${lvlClass}">${lvl}</span>
          <span class="log-module-tag">[${escapeHtml(mod)}]</span>
          <span>${escapeHtml(msg)}</span>
        </div>`;
    }).join("");

    if (isLogAutoScroll) {
      consoleEl.scrollTop = consoleEl.scrollHeight;
    }
  } catch (e) {}
}

let currentPreviewNode = null;

function openPreview(node) {
  currentPreviewNode = node;
  const modal = document.getElementById("previewModal");
  const box = document.getElementById("modalPreviewBox");
  const header = document.getElementById("previewHeader");
  const container = document.getElementById("previewContainer");
  const title = document.getElementById("previewTitle");

  title.innerText = node.name;
  title.title = node.name;
  modal.classList.remove("hidden");

  // 初始化居中浮窗并开启鼠标自由拖拽
  if (box && header) {
    box.style.transform = "none";
    box.style.left = `${Math.max(20, (window.innerWidth - 820) / 2)}px`;
    box.style.top = `${Math.max(30, (window.innerHeight - 560) / 2)}px`;
    makeElementDraggable(header, box);
  }

  // 绑定预览浮窗右上角控件
  const btnPrevDl = document.getElementById("btnPreviewDownload");
  const btnHeadPot = document.getElementById("btnHeaderLaunchPotPlayer");
  const btnHeadCopy = document.getElementById("btnHeaderCopyStream");
  const btnMaximize = document.getElementById("btnModalMaximize");

  if (btnPrevDl) {
    btnPrevDl.onclick = (e) => {
      e.stopPropagation();
      requestWebDownload(node);
    };
  }

  if (btnMaximize && box) {
    btnMaximize.onclick = (e) => {
      e.stopPropagation();
      box.classList.toggle("maximized");
      btnMaximize.innerText = box.classList.contains("maximized") ? "🗗" : "⛶";
      btnMaximize.title = box.classList.contains("maximized") ? "还原窗口" : "最大化窗口";
    };
  }

  // 将预览缓冲任务压入左侧传输队列，实时可见缓冲速度与分块进度
  const prevTaskId = "task_prev_" + node.id;
  const rawChunkSize = Math.max(1, (configuredChunkSizeBytes || 52428799) - 36);
  const calcChunks = Math.max(1, Math.ceil(node.size / rawChunkSize));
  const allChunks = [];
  for (let i = 1; i <= calcChunks; i++) allChunks.push(i);

  const existingActive = activeTasks.find(bt => (bt.task_id === prevTaskId || bt.virtual_path === node.virtual_path) && bt.status === "completed");
  let existingClientTask = clientTaskQueue.find(x => x.id === prevTaskId);

  if (existingActive) {
    if (existingClientTask) {
      existingClientTask.status = "completed";
      existingClientTask.stage = "5/5 完成";
      existingClientTask.stage_detail = "✔ 本地已完全预载/已下载完毕，可直接离线秒播";
      existingClientTask.progress = 100;
      existingClientTask.loaded = node.size;
      existingClientTask.total = node.size;
      existingClientTask.total_chunks = calcChunks;
      existingClientTask.completed_chunks = allChunks;
      existingClientTask.speed = 0;
    } else {
      clientTaskQueue.push({
        id: prevTaskId,
        backend_id: prevTaskId,
        type: "preview",
        name: "🎬 " + node.name,
        file_name: node.name,
        virtual_path: node.virtual_path,
        size: node.size,
        total: node.size,
        loaded: node.size,
        progress: 100,
        speed: 0,
        status: "completed",
        stage: "5/5 完成",
        stage_detail: "✔ 本地已完全预载/已下载完毕，可直接离线秒播",
        total_chunks: calcChunks,
        completed_chunks: allChunks,
        active_chunks: [],
        start_time: Date.now(),
        completed_at: Date.now()
      });
    }
    renderSideTransferList();
  } else if (existingClientTask) {
    existingClientTask.status = "running";
    existingClientTask.elapsed_sec = null;
    existingClientTask.completed_at = null;
    existingClientTask.paused_at = null;
    existingClientTask.paused_elapsed_sec = null;
    existingClientTask.total_paused_ms = 0;
    existingClientTask.start_time = Date.now();
    existingClientTask.name = "🎬 " + node.name + " (在线缓冲)";
    existingClientTask.stage = "正在缓冲";
    existingClientTask.stage_detail = "正在连接云端拉取媒体切片...";
    existingClientTask.active_chunks = [1];
    existingClientTask.speed = 0;
    renderSideTransferList();
    startTaskPolling();
  } else {
    clientTaskQueue.push({
      id: prevTaskId,
      backend_id: prevTaskId,
      type: "preview",
      name: "🎬 " + node.name + " (在线缓冲)",
      file_name: node.name,
      virtual_path: node.virtual_path,
      size: node.size,
      total: node.size,
      loaded: 0,
      progress: 0,
      speed: 0,
      status: "running",
      total_chunks: calcChunks,
      completed_chunks: [],
      active_chunks: [1],
      start_time: Date.now()
    });
    renderSideTransferList();
    startTaskPolling();
  }

  const previewUrl = `/api/preview?path=${encodeURIComponent(node.virtual_path)}`;

  // 极速对齐：向后端立即执行一次任务探查，若后端已识别为完整预载文件，立即瞬时切为完成态
  setTimeout(async () => {
    try {
      const res = await fetch("/api/tasks");
      const list = await res.json();
      if (Array.isArray(list)) {
        const bt = list.find(t => t.task_id === prevTaskId || t.virtual_path === node.virtual_path);
        if (bt && bt.status === "completed") {
          const ct = clientTaskQueue.find(x => x.id === prevTaskId);
          if (ct) {
            ct.status = "completed";
            ct.progress = 100;
            ct.loaded = ct.total;
            ct.speed = 0;
            ct.completed_chunks = allChunks;
            ct.stage = bt.stage;
            ct.stage_detail = bt.stage_detail;
            renderSideTransferList();
          }
        }
      }
    } catch (e) {}
  }, 120);

  if (node.name.match(/\.(jpg|jpeg|png|webp|gif|bmp)$/i)) {
    if (btnHeadPot) btnHeadPot.classList.add("hidden");
    if (btnHeadCopy) btnHeadCopy.classList.add("hidden");
    const previewThumbSrc = node.thumb_base64 || `/api/thumb?path=${encodeURIComponent(node.virtual_path)}`;
    if (previewThumbSrc) {
      container.innerHTML = `
        <div style="position:relative; display:flex; flex-direction:column; align-items:center;">
          <img id="previewImgEl" src="${previewThumbSrc}" alt="preview" style="max-width:100%; max-height:65vh; border-radius:6px; object-fit:contain;">
          <div id="previewLoadingHint" style="font-size:12px; color:#94a3b8; margin-top:8px;">⚡ 已展示快速缩略图，后台正在流式解密原图...</div>
        </div>`;
      const fullImg = new Image();
      fullImg.onload = () => {
        const el = document.getElementById("previewImgEl");
        const hint = document.getElementById("previewLoadingHint");
        if (el) el.src = previewUrl;
        if (hint) hint.innerText = "✔ 原图已流式解密加载完毕";
      };
      fullImg.onerror = () => {
        const hint = document.getElementById("previewLoadingHint");
        if (hint) hint.innerText = "✔ 当前显示为内置缩略图预览";
      };
      fullImg.src = previewUrl;
    } else {
      container.innerHTML = `<img src="${previewUrl}" alt="preview" style="max-width:100%; max-height:65vh; border-radius:6px; object-fit:contain;">`;
    }
  } else if (node.name.match(/\.(mp4|webm|mov|mkv|avi)$/i)) {
    const vThumbSrc = node.thumb_base64 || `/api/thumb?path=${encodeURIComponent(node.virtual_path)}`;
    const posterAttr = vThumbSrc ? `poster="${vThumbSrc}"` : "";
    const potplayerProtocolUrl = `potplayer://${location.origin}${previewUrl}`;
    const directHttpUrl = `${location.origin}${previewUrl}`;

    // 激活顶部工具栏的 PotPlayer 唤起与流地址复制
    if (btnHeadPot) {
      btnHeadPot.href = potplayerProtocolUrl;
      btnHeadPot.classList.remove("hidden");
      btnHeadPot.onclick = () => {
        const v = document.getElementById("previewVideoEl");
        if (v) {
          try { v.pause(); v.removeAttribute("src"); v.load(); } catch (e) {}
        }
        showToast("✔ 已调起 PotPlayer 边下边播，网页端已自动挂起释放资源");
      };
    }
    if (btnHeadCopy) {
      btnHeadCopy.classList.remove("hidden");
      btnHeadCopy.onclick = () => {
        if (navigator.clipboard && navigator.clipboard.writeText) {
          navigator.clipboard.writeText(directHttpUrl).then(() => {
            showToast("✔ 播放地址已复制！可在 PotPlayer / VLC 中按 Ctrl+U 粘贴播放");
          }).catch(() => {
            prompt("请手动复制流媒体播放地址：", directHttpUrl);
          });
        } else {
          prompt("请手动复制流媒体播放地址：", directHttpUrl);
        }
      };
    }

    container.innerHTML = `
      <div id="customPlayerContainer" class="custom-video-player">
        <video id="previewVideoEl" autoplay ${posterAttr} src="${previewUrl}"></video>
        
        <!-- 浮动缓冲状态徽章 -->
        <div id="vpBufferingBadge" style="position:absolute; top:14px; left:16px; background:rgba(0,0,0,0.65); backdrop-filter:blur(4px); color:#38bdf8; font-size:11.5px; padding:4px 12px; border-radius:20px; display:none; align-items:center; gap:6px; z-index:20; border:1px solid rgba(56,189,248,0.25); pointer-events:none;">
          <span style="display:inline-block; width:6px; height:6px; border-radius:50%; background:#38bdf8;"></span>
          <span>⚡ 云端切片高速缓冲中...</span>
        </div>

        <!-- 一体化全功能现代播放器控制栏 (完全替代并屏蔽原生控件) -->
        <div id="vpControlsBar" class="vp-controls-bar" style="position:absolute; bottom:0; left:0; right:0; background:linear-gradient(to top, rgba(0,0,0,0.92) 0%, rgba(0,0,0,0.45) 70%, transparent 100%); padding:12px 18px 10px; display:flex; flex-direction:column; gap:6px; transition:opacity 0.25s ease; z-index:10;">
          <!-- 顶层：专业级已缓冲管道进度条组件 (加粗8px~10px + 已加载填充 + 微米拖拽 + 悬停时间微卡) -->
          <div id="vpProgressWrap" class="vp-progress-wrap" title="点击或拖拽跳转时间轴">
            <div class="vp-progress-bar">
              <div id="vpProgressBuffered" class="vp-progress-buffered"></div>
              <div id="vpProgressPlayed" class="vp-progress-played"></div>
            </div>
            <div id="vpProgressThumb" class="vp-progress-thumb"></div>
            <div id="vpHoverTooltip" class="vp-hover-tooltip">00:00</div>
          </div>

          <!-- 底层：播放、时间、音量、倍速 (含3.0x)、全屏 -->
          <div style="display:flex; align-items:center; justify-content:space-between; width:100%; gap:10px;">
            <div style="display:flex; align-items:center; gap:12px;">
              <button id="vpBtnPlayPause" style="background:none; border:none; color:#fff; font-size:16px; cursor:pointer; padding:2px 6px; border-radius:4px;" title="播放/暂停 (空格键)">▶</button>
              <span id="vpTimeDisplay" style="font-size:12px; font-variant-numeric:tabular-nums; color:#e2e8f0; letter-spacing:0.5px;">00:00 / 00:00</span>
            </div>

            <div style="display:flex; align-items:center; gap:14px;">
              <!-- 音量调节 -->
              <div style="display:flex; align-items:center; gap:6px;">
                <button id="vpBtnMute" style="background:none; border:none; color:#e2e8f0; font-size:14px; cursor:pointer; padding:2px;" title="静音/恢复音量">🔊</button>
                <input type="range" id="vpVolumeSlider" min="0" max="1" step="0.05" value="1" style="width:65px; cursor:pointer; accent-color:#38bdf8; height:4px;" title="音量调节">
              </div>

              <!-- 倍速播放下拉菜单 (支持 0.75x, 1.0x, 1.25x, 1.5x, 2.0x, 3.0x 极速) -->
              <div style="display:flex; align-items:center;">
                <select id="vpSpeedSelect" style="background:rgba(255,255,255,0.15); color:#fff; border:1px solid rgba(255,255,255,0.25); font-size:11.5px; border-radius:4px; padding:2px 4px; outline:none; cursor:pointer;" title="倍速播放">
                  <option value="0.75" style="background:#1e293b; color:#fff;">0.75x</option>
                  <option value="1.0" selected style="background:#1e293b; color:#fff;">1.0x 正常</option>
                  <option value="1.25" style="background:#1e293b; color:#fff;">1.25x</option>
                  <option value="1.5" style="background:#1e293b; color:#fff;">1.5x</option>
                  <option value="2.0" style="background:#1e293b; color:#fff;">2.0x</option>
                  <option value="3.0" style="background:#1e293b; color:#fff;">3.0x 极速</option>
                </select>
              </div>

              <!-- 全屏最大化 -->
              <button id="vpBtnFullscreen" style="background:none; border:none; color:#e2e8f0; font-size:15px; cursor:pointer; padding:2px;" title="全屏最大化 (或双击画面)">⛶</button>
            </div>
          </div>
        </div>
      </div>`;

    const vEl = document.getElementById("previewVideoEl");
    const pContainer = document.getElementById("customPlayerContainer");
    const cBar = document.getElementById("vpControlsBar");
    const btnPlayPause = document.getElementById("vpBtnPlayPause");
    const timeDisp = document.getElementById("vpTimeDisplay");
    const progressWrap = document.getElementById("vpProgressWrap");
    const progressBuffered = document.getElementById("vpProgressBuffered");
    const progressPlayed = document.getElementById("vpProgressPlayed");
    const progressThumb = document.getElementById("vpProgressThumb");
    const hoverTooltip = document.getElementById("vpHoverTooltip");
    const btnMute = document.getElementById("vpBtnMute");
    const volSlider = document.getElementById("vpVolumeSlider");
    const speedSelect = document.getElementById("vpSpeedSelect");
    const btnFs = document.getElementById("vpBtnFullscreen");
    const badgeEl = document.getElementById("vpBufferingBadge");

    let knownTotalSec = 0;
    let baseOffsetSec = 0;
    let isScrubbing = false;

    // 播放/暂停控制
    const togglePlay = () => {
      if (!vEl) return;
      if (vEl.paused) {
        vEl.play().catch(()=>{});
        if (btnPlayPause) btnPlayPause.innerText = "⏸";
      } else {
        vEl.pause();
        if (btnPlayPause) btnPlayPause.innerText = "▶";
      }
    };
    if (btnPlayPause) btnPlayPause.onclick = togglePlay;
    if (vEl) {
      vEl.onclick = togglePlay;
      vEl.onplay = () => { if (btnPlayPause) btnPlayPause.innerText = "⏸"; };
      vEl.onpause = () => { if (btnPlayPause) btnPlayPause.innerText = "▶"; };
    }

    // 渲染专业已加载缓冲进度条 (结合云端落盘分片与浏览器内存流)
    const updateBufferedDisplay = () => {
      if (!vEl || !progressBuffered) return;
      const dur = knownTotalSec > 0 ? knownTotalSec : (vEl.duration || 0);
      if (!dur || !isFinite(dur) || dur <= 0) return;

      const pId = "task_prev_" + node.id;
      const task = clientTaskQueue.find(t => t.id === pId || t.virtual_path === node.virtual_path);

      let html = "";
      if (task && task.total_chunks && task.completed_chunks && task.completed_chunks.length > 0) {
        const totalC = task.total_chunks;
        const sorted = [...new Set(task.completed_chunks)].sort((a,b) => a-b);
        let startC = sorted[0];
        let prevC = sorted[0];

        for (let i = 1; i <= sorted.length; i++) {
          if (i < sorted.length && sorted[i] === prevC + 1) {
            prevC = sorted[i];
          } else {
            const leftPct = ((startC - 1) / totalC) * 100;
            const widthPct = ((prevC - startC + 1) / totalC) * 100;
            html += `<div class="vp-buffer-chunk" style="left:${leftPct.toFixed(2)}%; width:${widthPct.toFixed(2)}%;"></div>`;
            if (i < sorted.length) {
              startC = sorted[i];
              prevC = sorted[i];
            }
          }
        }
      }

      if (vEl.buffered && vEl.buffered.length > 0) {
        for (let i = 0; i < vEl.buffered.length; i++) {
          const bStart = vEl.buffered.start(i);
          const bEnd = vEl.buffered.end(i);
          const leftPct = (bStart / dur) * 100;
          const widthPct = ((bEnd - bStart) / dur) * 100;
          html += `<div class="vp-buffer-chunk" style="left:${leftPct.toFixed(2)}%; width:${widthPct.toFixed(2)}%; background:rgba(56,189,248,0.35);"></div>`;
        }
      }

      if (html) {
        progressBuffered.innerHTML = html;
      }
    };

    // 渲染播放进度高亮与手柄
    const updatePlayedDisplay = (curSec) => {
      const dur = knownTotalSec > 0 ? knownTotalSec : (vEl.duration || 0);
      if (!dur || !isFinite(dur) || dur <= 0) return;
      const pct = Math.max(0, Math.min(100, (curSec / dur) * 100));
      if (progressPlayed) progressPlayed.style.width = pct + "%";
      if (progressThumb) progressThumb.style.left = pct + "%";
      if (timeDisp) {
        timeDisp.innerText = `${formatMediaDuration(curSec)} / ${formatMediaDuration(dur)}`;
      }
    };

    // 牢牢锁定原片全量真实总时长 (兼容 loadedmetadata 与 durationchange)
    const onDurationReady = () => {
      if (vEl.duration && isFinite(vEl.duration) && vEl.duration > 0 && !vEl.dataset.transcoding) {
        knownTotalSec = Math.floor(vEl.duration);
        const curT = (vEl.dataset.transcoding === "1") ? (baseOffsetSec + (vEl.currentTime || 0)) : (vEl.currentTime || 0);
        updatePlayedDisplay(curT);
        updateBufferedDisplay();
      }
    };
    vEl.addEventListener("loadedmetadata", onDurationReady);
    vEl.addEventListener("durationchange", onDurationReady);

    // 鼠标悬停时间卡片预览
    if (progressWrap && hoverTooltip) {
      progressWrap.addEventListener("mousemove", (e) => {
        const rect = progressWrap.getBoundingClientRect();
        const dur = knownTotalSec > 0 ? knownTotalSec : (vEl.duration || 0);
        if (!dur || rect.width <= 0) return;
        const clientX = (e.clientX !== undefined) ? e.clientX : 0;
        const clickX = Math.max(0, Math.min(rect.width, clientX - rect.left));
        const pct = clickX / rect.width;
        const hoverSec = Math.floor(pct * dur);
        hoverTooltip.innerText = formatMediaDuration(hoverSec);
        hoverTooltip.style.left = (pct * 100) + "%";
        hoverTooltip.style.display = "block";
      });
      progressWrap.addEventListener("mouseleave", () => {
        if (!isScrubbing) hoverTooltip.style.display = "none";
      });
    }

    // 进度条微米级点击与平滑拖拽跳转
    const calculateSecFromEvent = (e) => {
      const rect = progressWrap.getBoundingClientRect();
      const dur = knownTotalSec > 0 ? knownTotalSec : (vEl.duration || 0);
      if (!dur || rect.width <= 0) return 0;
      const clientX = (e.clientX !== undefined) ? e.clientX : (e.touches && e.touches[0] ? e.touches[0].clientX : 0);
      const clickX = Math.max(0, Math.min(rect.width, clientX - rect.left));
      const pct = clickX / rect.width;
      return Math.floor(pct * dur);
    };

    if (progressWrap) {
      const startScrub = (e) => {
        isScrubbing = true;
        progressWrap.classList.add("scrubbing");
        const targetSec = calculateSecFromEvent(e);
        updatePlayedDisplay(targetSec);

        const moveScrub = (me) => {
          if (!isScrubbing) return;
          const s = calculateSecFromEvent(me);
          updatePlayedDisplay(s);
          if (hoverTooltip) {
            hoverTooltip.innerText = formatMediaDuration(s);
            const rect = progressWrap.getBoundingClientRect();
            const clientX = (me.clientX !== undefined) ? me.clientX : (me.touches && me.touches[0] ? me.touches[0].clientX : 0);
            const clickX = Math.max(0, Math.min(rect.width, clientX - rect.left));
            hoverTooltip.style.left = (clickX / rect.width * 100) + "%";
            hoverTooltip.style.display = "block";
          }
        };

        const endScrub = (ue) => {
          if (!isScrubbing) return;
          isScrubbing = false;
          progressWrap.classList.remove("scrubbing");
          if (hoverTooltip) hoverTooltip.style.display = "none";
          window.removeEventListener("mousemove", moveScrub);
          window.removeEventListener("mouseup", endScrub);
          window.removeEventListener("touchmove", moveScrub);
          window.removeEventListener("touchend", endScrub);

          const finalSec = calculateSecFromEvent(ue);
          if (badgeEl) badgeEl.style.display = "inline-flex";
          logWebTransfer(`[INFO] [🎬 播放器跳转] 时间轴跳转至 ${formatMediaDuration(finalSec)}，后台正在优先预缓冲目标切片...`);
          if (vEl.dataset.transcoding === "1") {
            baseOffsetSec = finalSec;
            vEl.pause();
            vEl.src = `${previewUrl}&transcode=1&t=${finalSec}&dur=${knownTotalSec}`;
            vEl.load();
            vEl.play().catch(()=>{});
          } else {
            vEl.currentTime = finalSec;
          }
          updatePlayedDisplay(finalSec);
        };

        window.addEventListener("mousemove", moveScrub);
        window.addEventListener("mouseup", endScrub);
        window.addEventListener("touchmove", moveScrub);
        window.addEventListener("touchend", endScrub);
      };

      progressWrap.addEventListener("mousedown", startScrub);
      progressWrap.addEventListener("touchstart", startScrub, { passive: true });
    }

    // 音量与静音控制
    if (volSlider && vEl) {
      volSlider.addEventListener("input", () => {
        vEl.volume = parseFloat(volSlider.value);
        vEl.muted = (vEl.volume === 0);
        if (btnMute) btnMute.innerText = vEl.muted ? "🔇" : "🔊";
      });
    }
    if (btnMute && vEl) {
      btnMute.onclick = () => {
        vEl.muted = !vEl.muted;
        btnMute.innerText = vEl.muted ? "🔇" : "🔊";
      };
    }

    // 倍速播放控制 (含3.0x 极速)
    if (speedSelect && vEl) {
      speedSelect.addEventListener("change", () => {
        vEl.playbackRate = parseFloat(speedSelect.value);
        showToast(`已切换至 ${speedSelect.value}x 倍速播放`);
      });
    }

    // 全屏最大化控制 (支持点击按钮或双击画面全屏)
    const toggleFullscreen = () => {
      if (!document.fullscreenElement && !document.webkitFullscreenElement) {
        if (pContainer && pContainer.requestFullscreen) {
          pContainer.requestFullscreen().catch(() => {
            if (vEl && vEl.requestFullscreen) vEl.requestFullscreen().catch(()=>{});
          });
        } else if (pContainer && pContainer.webkitRequestFullscreen) {
          pContainer.webkitRequestFullscreen();
        } else if (vEl && vEl.requestFullscreen) {
          vEl.requestFullscreen().catch(()=>{});
        }
      } else {
        if (document.exitFullscreen) {
          document.exitFullscreen().catch(()=>{});
        } else if (document.webkitExitFullscreen) {
          document.webkitExitFullscreen();
        }
      }
    };
    if (btnFs) btnFs.onclick = toggleFullscreen;
    if (vEl) vEl.ondblclick = toggleFullscreen;

    // 全局快捷键支持 (空格暂停/播放，左右方向键微调5秒，上下方向键微调音量，F键全屏，M键静音)
    const onPreviewKeyDown = (e) => {
      if (!modal || modal.classList.contains("hidden")) return;
      if (e.target && (e.target.tagName === "INPUT" || e.target.tagName === "TEXTAREA")) return;

      const dur = knownTotalSec > 0 ? knownTotalSec : (vEl.duration || 0);
      const curT = (vEl.dataset.transcoding === "1") ? (baseOffsetSec + (vEl.currentTime || 0)) : (vEl.currentTime || 0);

      switch (e.code) {
        case "Escape":
          if (!document.fullscreenElement && !document.webkitFullscreenElement) {
            e.preventDefault();
            closePreviewFn();
          }
          break;
        case "Space":
          e.preventDefault();
          togglePlay();
          break;
        case "ArrowLeft":
          e.preventDefault();
          const backSec = Math.max(0, curT - 5);
          if (vEl.dataset.transcoding === "1") {
            baseOffsetSec = Math.floor(backSec);
            vEl.src = `${previewUrl}&transcode=1&t=${Math.floor(backSec)}&dur=${knownTotalSec}`;
            vEl.load();
            vEl.play().catch(()=>{});
          } else {
            vEl.currentTime = backSec;
          }
          updatePlayedDisplay(backSec);
          break;
        case "ArrowRight":
          e.preventDefault();
          const fwdSec = Math.min(dur, curT + 5);
          if (vEl.dataset.transcoding === "1") {
            baseOffsetSec = Math.floor(fwdSec);
            vEl.src = `${previewUrl}&transcode=1&t=${Math.floor(fwdSec)}&dur=${knownTotalSec}`;
            vEl.load();
            vEl.play().catch(()=>{});
          } else {
            vEl.currentTime = fwdSec;
          }
          updatePlayedDisplay(fwdSec);
          break;
        case "ArrowUp":
          e.preventDefault();
          vEl.volume = Math.min(1, parseFloat((vEl.volume + 0.05).toFixed(2)));
          if (volSlider) volSlider.value = vEl.volume;
          if (btnMute) btnMute.innerText = vEl.volume === 0 ? "🔇" : "🔊";
          showToast(`音量: ${Math.round(vEl.volume * 100)}%`);
          break;
        case "ArrowDown":
          e.preventDefault();
          vEl.volume = Math.max(0, parseFloat((vEl.volume - 0.05).toFixed(2)));
          if (volSlider) volSlider.value = vEl.volume;
          if (btnMute) btnMute.innerText = vEl.volume === 0 ? "🔇" : "🔊";
          showToast(`音量: ${Math.round(vEl.volume * 100)}%`);
          break;
        case "KeyF":
          e.preventDefault();
          toggleFullscreen();
          break;
        case "KeyM":
          e.preventDefault();
          if (btnMute) btnMute.click();
          break;
      }
    };
    window.addEventListener("keydown", onPreviewKeyDown);

    // 控制栏平滑自动隐藏逻辑 (播放中静止 2.5 秒淡出，鼠标移动瞬间浮现)
    if (pContainer && cBar) {
      let hideTimer = null;
      const resetHideTimer = () => {
        cBar.style.opacity = "1";
        clearTimeout(hideTimer);
        if (vEl && !vEl.paused) {
          hideTimer = setTimeout(() => {
            if (!isScrubbing) cBar.style.opacity = "0";
          }, 2500);
        }
      };
      pContainer.addEventListener("mousemove", resetHideTimer);
      pContainer.addEventListener("mouseleave", () => {
        if (vEl && !vEl.paused && !isScrubbing) cBar.style.opacity = "0";
      });
    }

    // 静音自动探测自愈与全片时间轴实时同步
    if (vEl) {
      let audioChecked = false;
      const onTimeUpdate = () => {
        if (!audioChecked && !vEl.paused && !vEl.dataset.transcoding && vEl.currentTime > 0.5) {
          audioChecked = true;
          if (typeof vEl.webkitAudioDecodedByteCount === "number" && vEl.webkitAudioDecodedByteCount === 0) {
            vEl.dataset.transcoding = "1";
            const curT = Math.floor(vEl.currentTime || 0);
            baseOffsetSec = curT;
            vEl.pause();
            vEl.src = `${previewUrl}&transcode=1&t=${curT}&dur=${knownTotalSec}`;
            vEl.load();
            vEl.play().catch(()=>{});
            showToast("🎧 检测到杜比/多声道音轨，已自动无缝切入 FFmpeg 实时发声管道！");
          }
        }
        if (!isScrubbing) {
          const realTime = (vEl.dataset.transcoding === "1") ? (baseOffsetSec + (vEl.currentTime || 0)) : (vEl.currentTime || 0);
          updatePlayedDisplay(realTime);
          updateBufferedDisplay();
        }
      };
      vEl.addEventListener("timeupdate", onTimeUpdate);
      vEl.addEventListener("progress", updateBufferedDisplay);
      vEl.addEventListener("waiting", () => { if (badgeEl) badgeEl.style.display = "inline-flex"; });
      vEl.addEventListener("playing", () => { if (badgeEl) badgeEl.style.display = "none"; });
      vEl.addEventListener("canplay", () => { if (badgeEl) badgeEl.style.display = "none"; });
      vEl.onloadeddata = () => {
        if (badgeEl) badgeEl.style.display = "none";
        logWebTransfer(`[INFO] [🎬 视频预览] ✔ 视频流首帧画面已就绪起播: ${node.name}`);
        updateBufferedDisplay();
      };
      vEl.onerror = () => {
        const err = vEl.error;
        const code = err ? err.code : 0;
        const msg = (err && err.message) ? err.message : "无详细错误描述";
        logWebTransfer(`[WARN] [🎬 视频预览] 浏览器内核无法原生播放 (代码: ${code}) | 原因: ${msg}`);

        if (badgeEl) badgeEl.style.display = "none";

        // 浮现老旧视频编码友好提示与 PotPlayer 一键调起面板
        const customPlayer = document.getElementById("customPlayerContainer");
        if (customPlayer && !document.getElementById("vpCodecUnsupportedBanner")) {
          const banner = document.createElement("div");
          banner.id = "vpCodecUnsupportedBanner";
          banner.style.cssText = "position:absolute; inset:0; background:rgba(15,23,42,0.92); z-index:50; display:flex; flex-direction:column; align-items:center; justify-content:center; padding:24px; text-align:center;";
          banner.innerHTML = `
            <div style="font-size:42px; margin-bottom:12px;">🎬</div>
            <h4 style="color:#f8fafc; margin:0 0 8px 0; font-size:16px;">浏览器内核不支持此视频编码 (DivX/XviD/老旧MKV)</h4>
            <p style="color:#94a3b8; font-size:13px; max-width:440px; margin:0 0 20px 0; line-height:1.6;">
              现代浏览器出于专利原因已移除对该编码的原生支持。建议直接调起本地专业播放器享受全高清边下边播：
            </p>
            <div style="display:flex; gap:12px; flex-wrap:wrap; justify-content:center;">
              <a href="${potplayerProtocolUrl}" class="btn btn-primary" style="display:inline-flex; align-items:center; gap:6px; padding:9px 18px; font-weight:600; text-decoration:none;">
                🚀 一键调起 PotPlayer 播放
              </a>
              <button type="button" class="btn btn-secondary" id="btnCopyDirectStreamLink" style="padding:9px 16px;">
                📋 复制播放直链
              </button>
            </div>
          `;
          customPlayer.appendChild(banner);
          const cpBtn = document.getElementById("btnCopyDirectStreamLink");
          if (cpBtn) {
            cpBtn.onclick = () => {
              if (navigator.clipboard && navigator.clipboard.writeText) {
                navigator.clipboard.writeText(directHttpUrl).then(() => showToast("✔ 播放直链已复制！可在 PotPlayer / VLC 中按 Ctrl+U 粘贴播放"));
              } else {
                prompt("请手动复制流媒体播放地址：", directHttpUrl);
              }
            };
          }
        }
      };
    }
  } else {
    if (btnHeadPot) btnHeadPot.classList.add("hidden");
    if (btnHeadCopy) btnHeadCopy.classList.add("hidden");
    container.innerHTML = `
      <div style="padding: 40px; text-align: center; color: #fff;">
        <p style="font-size: 44px; margin-bottom: 12px;">📄</p>
        <p style="font-weight: 500;">${node.name}</p>
        <p style="font-size: 13px; color: #94a3b8; margin-top: 6px;">文件大小: ${formatBytes(node.size)}</p>
        <button class="btn btn-primary" style="margin-top: 18px;" id="btnModalDownload">📥 立即下载此文件到本地</button>
      </div>`;
    const dlBtn = document.getElementById("btnModalDownload");
    if (dlBtn) {
      dlBtn.onclick = () => {
        modal.classList.add("hidden");
        requestWebDownload(node);
      };
    }
  }
}

function updateBreadcrumb() {
  const bc = document.getElementById("breadcrumb");
  if (!bc) return;
  bc.innerHTML = "";

  const currentSpaceDisplayName = cleanFolderDisplayName(currentActiveStorageFolder) || "全部文件";
  const rootSpan = document.createElement("span");
  rootSpan.className = "crumb";
  rootSpan.innerText = currentSpaceDisplayName;
  rootSpan.title = `点击回到 ${currentSpaceDisplayName} 根目录`;
  rootSpan.onclick = () => {
    currentPath = "/";
    searchKeyword = "";
    const searchInput = document.getElementById("searchInput");
    if (searchInput) searchInput.value = "";
    const searchClear = document.getElementById("searchClear");
    if (searchClear) searchClear.classList.add("hidden");
    selectedPaths.clear();
    updateBatchBar();
    renderFolderTree();
    updateBreadcrumb();
    renderFiles();
  };
  bc.appendChild(rootSpan);

  if (currentPath !== "/") {
    const parts = currentPath.split("/").filter(Boolean);
    let buildPath = "";
    parts.forEach(p => {
      buildPath += "/" + p;
      const span = document.createElement("span");
      span.className = "crumb";
      span.innerText = " / " + p;
      span.dataset.path = buildPath;
      span.onclick = () => {
        currentPath = span.dataset.path;
        selectedPaths.clear();
        updateBatchBar();
        expandedFolders.add(currentPath);
        renderFolderTree();
        updateBreadcrumb();
        renderFiles();
      };
      bc.appendChild(span);
    });
  }
}

// 视频前端原生 Canvas 384px 截帧 (严格防挂起、防不支持编码导致空数据)
async function captureVideoThumbnail(file, size = 384) {
  return new Promise((resolve) => {
    let resolved = false;
    const finish = (res) => {
      if (resolved) return;
      resolved = true;
      clearTimeout(timer);
      try { URL.revokeObjectURL(url); } catch (e) {}
      resolve(res || "");
    };

    // 3 秒严格超时熔断机制，若格式不支持 (如 XviD/DivX/MPEG2) 或解码受阻，立即放行并优雅降级为默认图标
    const timer = setTimeout(() => {
      finish("");
    }, 3000);

    const video = document.createElement("video");
    video.preload = "metadata";
    video.muted = true;
    video.playsInline = true;
    video.onerror = () => finish("");
    video.onabort = () => finish("");
    let url = "";
    try {
      url = URL.createObjectURL(file);
      video.src = url;
    } catch (e) {
      finish("");
      return;
    }

    video.onloadeddata = () => {
      if (!video.videoWidth || !video.videoHeight || video.videoWidth === 0 || video.videoHeight === 0) {
        finish("");
        return;
      }
      video.currentTime = Math.min(2, (video.duration > 0 ? video.duration / 2 : 1));
    };

    video.onseeked = () => {
      const w = video.videoWidth;
      const h = video.videoHeight;
      if (!w || !h || w === 0 || h === 0) {
        finish("");
        return;
      }
      const canvas = document.createElement("canvas");
      let targetW = w;
      let targetH = h;
      if (targetW > targetH) {
        if (targetW > size) {
          targetH = Math.round((targetH * size) / targetW);
          targetW = size;
        }
      } else {
        if (targetH > size) {
          targetW = Math.round((targetW * size) / targetH);
          targetH = size;
        }
      }
      canvas.width = targetW;
      canvas.height = targetH;
      const ctx = canvas.getContext("2d");
      ctx.drawImage(video, 0, 0, targetW, targetH);
      try {
        const dataUrl = canvas.toDataURL("image/webp", 0.65);
        if (dataUrl && dataUrl.length > 60) {
          finish(dataUrl);
        } else {
          finish("");
        }
      } catch (e) {
        finish("");
      }
    };

    video.onerror = () => {
      finish("");
    };
  });
}

// 图片前端 Canvas 384px 缩略图
async function captureImageThumbnail(file, size = 384) {
  return new Promise((resolve) => {
    const reader = new FileReader();
    reader.onload = (e) => {
      const img = new Image();
      img.onload = () => {
        const canvas = document.createElement("canvas");
        let w = img.width;
        let h = img.height;
        if (w > h) {
          if (w > size) {
            h = Math.round((h * size) / w);
            w = size;
          }
        } else {
          if (h > size) {
            w = Math.round((w * size) / h);
            h = size;
          }
        }
        canvas.width = w;
        canvas.height = h;
        const ctx = canvas.getContext("2d");
        ctx.drawImage(img, 0, 0, w, h);
        resolve(canvas.toDataURL("image/webp", 0.65));
      };
      img.onerror = () => resolve("");
      img.src = e.target.result;
    };
    reader.onerror = () => resolve("");
    reader.readAsDataURL(file);
  });
}

// 处理文件上传提交：无论何时拖入、拖入多少，全部统一压入全局待执行队列排队
async function handleFilesUpload(files) {
  if (!isConfiguredState) {
    showToast("请先完成首次配置并连入 QQ 邮箱！", true);
    document.getElementById("firstRunModal").classList.remove("hidden");
    return;
  }

  const fileListSnapshot = Array.from(files || []);
  if (fileListSnapshot.length === 0) return;

  // 极速提速引导：若 HTTP 未连接且用户未在本次运行期间选择“暂不登录”，引导扫码开启 HTTP 协同加速；点“暂不登录”则保底纯走 IMAP
  checkSpeedupChannel(() => {
    doActualFilesUpload(fileListSnapshot);
  });
}

async function doActualFilesUpload(fileListSnapshot) {
  for (let i = 0; i < fileListSnapshot.length; i++) {
    const file = fileListSnapshot[i];
    const relPath = file.webkitRelativePath || file.name;
    const targetVPath = (currentPath === "/" ? "" : currentPath) + "/" + relPath;

    // 核心优化：若队列中已存在同名且处于 failed/cancelled 的旧任务，立即平滑移除并清理后端，杜绝串号污染
    const oldFailedIdx = clientTaskQueue.findIndex(t => t.type === "upload" && t.file_name === file.name && (t.status === "failed" || t.status === "cancelled"));
    if (oldFailedIdx !== -1) {
      const oldTask = clientTaskQueue[oldFailedIdx];
      const oldCard = document.querySelector(`.transfer-item-card[data-id="${oldTask.id}"]`);
      if (oldCard) oldCard.remove();
      const oldBackendId = oldTask.backend_id || oldTask.id;
      fetch(`/api/tasks/cancel?task_id=${encodeURIComponent(oldBackendId)}&reason=reupload`, { method: "POST" }).catch(() => {});
      clientTaskQueue.splice(oldFailedIdx, 1);
    }

    let thumb = "";
    if (file.type.startsWith("video/") || file.name.match(/\.(mp4|mkv|mov|avi|webm|flv|wmv)$/i)) {
      thumb = await captureVideoThumbnail(file, 384);
    } else if (file.type.startsWith("image/") || file.name.match(/\.(jpg|jpeg|png|webp|gif|bmp)$/i)) {
      thumb = await captureImageThumbnail(file, 384);
    }

    const taskId = "up_" + Date.now() + "_" + Math.random().toString(36).substring(2, 7);
    const cSz = configuredChunkSizeBytes || 52428799;
    const calcChunks = Math.max(1, Math.ceil(file.size / cSz));
    clientTaskQueue.push({
      id: taskId,
      backend_id: null,
      type: "upload",
      name: ((file.webkitRelativePath && file.webkitRelativePath.includes("/")) ? "📁 " : "📤 ") + file.name,
      file_name: file.name,
      file: file,
      virtual_path: targetVPath,
      thumb_base64: thumb,
      size: file.size,
      total: file.size,
      total_chunks: calcChunks,
      loaded: 0,
      progress: 0,
      speed: 0,
      status: "queued",
      start_time: Date.now()
    });
  }

  showToast(`已将 ${fileListSnapshot.length} 个文件加入上传队列`);
  renderSideTransferList();
  startTaskPolling();
  processQueueDispatcher();
}

function bindEvents() {
  initChannelStatusHandlers();

  // 侧边栏快捷新建隔离空间
  const btnCreateSpaceQuick = document.getElementById("btnCreateSpaceQuick");
  if (btnCreateSpaceQuick) {
    btnCreateSpaceQuick.onclick = async () => {
      if (!isConfiguredState) {
        showToast("请先完成首次配置并连入 QQ 邮箱！", true);
        return;
      }
      const folderName = await showInputPrompt({
        title: "➕ 新建隔离空间",
        desc: "每个隔离空间在 QQ 邮箱中对应独立目录，拥有独立文件树与索引：",
        label: "空间名称",
        defaultValue: ""
      });
      if (!folderName || !folderName.trim()) return;
      try {
        const res = await fetch("/api/folders/create", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ folder: folderName.trim() })
        });
        const data = await res.json();
        if (data.status === "ok") {
          showToast(`✔ 成功创建新隔离空间 [${folderName.trim()}]！`);
          await loadIsolatedSpaces();
        } else {
          showToast(`❌ 创建空间失败: ${data.message || ""}`, true);
        }
      } catch (err) {
        showToast("❌ 创建请求异常: " + err.message, true);
      }
    };
  }

  document.addEventListener("click", (e) => {
    if (!e.target.closest(".space-context-menu") && !e.target.closest(".isolated-space-more-btn")) {
      closeSpaceActionMenu();
    }
  });

  // 全局点击自动关闭右键菜单
  document.addEventListener("click", (e) => {
    if (!e.target.closest("#fileContextMenu")) {
      closeContextMenu();
    }
  });
  window.addEventListener("scroll", () => closeContextMenu(), true);
  window.addEventListener("resize", () => { closeContextMenu(); renderSideTransferList(); });

  // 绑定右键菜单操作项
  const ctxDownload = document.getElementById("ctxDownload");
  if (ctxDownload) {
    ctxDownload.onclick = (e) => {
      e.stopPropagation();
      closeContextMenu();
      if (selectedPaths.size > 1) {
        document.getElementById("btnBatchDownload").click();
      } else if (contextMenuTarget) {
        requestWebDownload(contextMenuTarget);
      }
    };
  }

  const ctxCopy = document.getElementById("ctxCopy");
  if (ctxCopy) {
    ctxCopy.onclick = (e) => {
      e.stopPropagation();
      closeContextMenu();
      if (selectedPaths.size > 1) {
        const btnBC = document.getElementById("btnBatchCopy");
        if (btnBC) btnBC.click();
        else openCopyModal(Array.from(selectedPaths));
      } else if (contextMenuTarget) {
        openCopyModal([contextMenuTarget.virtual_path]);
      }
    };
  }

  const ctxMove = document.getElementById("ctxMove");
  if (ctxMove) {
    ctxMove.onclick = (e) => {
      e.stopPropagation();
      closeContextMenu();
      if (selectedPaths.size > 1) {
        document.getElementById("btnBatchMove").click();
      } else if (contextMenuTarget) {
        openMoveModal([contextMenuTarget.virtual_path]);
      }
    };
  }

  const ctxRename = document.getElementById("ctxRename");
  if (ctxRename) {
    ctxRename.onclick = async (e) => {
      e.stopPropagation();
      closeContextMenu();
      if (contextMenuTarget && selectedPaths.size <= 1) {
        const f = contextMenuTarget;
        const newName = await showInputPrompt({
          title: "✏️ 重命名",
          desc: `请输入 "${f.name}" 的新名称:`,
          label: "新文件名",
          defaultValue: f.name
        });
        if (newName && newName.trim() !== "" && newName.trim() !== f.name) {
          let cleanNew = newName.trim().replace(/^[\\\/]+|[\\\/]+$/g, "").replace(/\\/g, "/");
          if (!cleanNew || cleanNew === f.name) return;
          const parts = f.virtual_path.split("/").filter(Boolean);
          parts[parts.length - 1] = cleanNew;
          const newPath = "/" + parts.join("/");

          const fd = new FormData();
          fd.append("old_path", f.virtual_path);
          fd.append("new_path", newPath);
          try {
            const res = await fetch("/api/fs/rename", { method: "POST", body: fd });
            const data = await res.json();
            if (data.status === "ok") {
              logWebTransfer(`✔ [重命名成功] 成功将 "${f.name}" 更名为 "${newName.trim()}"`);
              showToast("✔ 重命名成功");
              loadFiles(currentPath);
            } else {
              logWebTransfer(`❌ [重命名失败] "${f.name}" 更名受阻: ${data.message || ""}`);
              showToast("❌ 重命名失败: " + (data.message || ""), true);
            }
          } catch (err) {
            logWebTransfer(`❌ [重命名异常] "${f.name}" 请求异常: ${err.message}`);
            showToast("❌ 重命名请求异常: " + err.message, true);
          }
        }
      }
    };
  }

  const ctxDelete = document.getElementById("ctxDelete");
  if (ctxDelete) {
    ctxDelete.onclick = async (e) => {
      e.stopPropagation();
      closeContextMenu();
      if (selectedPaths.size > 1) {
        document.getElementById("btnBatchDelete").click();
      } else if (contextMenuTarget) {
        const f = contextMenuTarget;
        const confirmed = await showConfirmDialog({
          title: "🗑️ 彻底删除确认",
          desc: `确定要彻底物理删除 "${f.name}" 吗？此操作将立即从 QQ 邮箱云端彻底清除对应的分片邮件，不可恢复！`,
          confirmText: "彻底删除",
          confirmClass: "btn-danger"
        });
        if (confirmed) {
          const fd = new FormData();
          fd.append("path", f.virtual_path);
          try {
            const res = await fetch("/api/fs/delete", { method: "POST", body: fd });
            const data = await res.json();
            if (data.status === "ok") {
              showToast("✔ 文件已彻底从云端删除");
              loadFiles(currentPath);
            } else {
              showToast("❌ 删除失败: " + (data.message || ""), true);
            }
          } catch (err) {
            showToast("❌ 删除异常: " + err.message, true);
          }
        }
      }
    };
  }

  // 底部日志抽屉高度拖拽调整 (支持平滑向上拉伸与本地记忆)
  const logDrawerResizer = document.getElementById("logDrawerResizer");
  const logDrawer = document.getElementById("logDrawer");
  if (logDrawerResizer && logDrawer) {
    const savedH = localStorage.getItem("log_drawer_height");
    if (savedH) {
      logDrawer.style.height = savedH + "px";
    }

    let startY = 0;
    let startH = 0;

    const onLogDrawerMouseMove = (e) => {
      const dy = startY - e.clientY;
      const newH = Math.max(120, Math.min(window.innerHeight * 0.85, startH + dy));
      logDrawer.style.height = newH + "px";
      localStorage.setItem("log_drawer_height", Math.round(newH));
    };

    const onLogDrawerMouseUp = () => {
      logDrawerResizer.classList.remove("dragging");
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
      document.removeEventListener("mousemove", onLogDrawerMouseMove);
      document.removeEventListener("mouseup", onLogDrawerMouseUp);
    };

    logDrawerResizer.onmousedown = (e) => {
      e.preventDefault();
      startY = e.clientY;
      startH = logDrawer.getBoundingClientRect().height;
      logDrawerResizer.classList.add("dragging");
      document.body.style.cursor = "ns-resize";
      document.body.style.userSelect = "none";
      document.addEventListener("mousemove", onLogDrawerMouseMove);
      document.addEventListener("mouseup", onLogDrawerMouseUp);
    };
  }

  // 侧边栏左右拖拽宽度调整 (支持平滑自适应与本地记忆)
  const sidebarResizer = document.getElementById("sidebarResizer");
  const sidebar = document.querySelector(".sidebar");
  if (sidebarResizer && sidebar) {
    const savedW = localStorage.getItem("sidebar_width");
    if (savedW) {
      const parsedW = parseInt(savedW, 10);
      if (parsedW >= 240 && parsedW <= 800) {
        sidebar.style.width = parsedW + "px";
      }
    }

    let startX = 0;
    let startW = 0;

    const onSidebarMouseMove = (e) => {
      const dx = e.clientX - startX;
      const newW = Math.max(250, Math.min(window.innerWidth * 0.6, Math.min(750, startW + dx)));
      sidebar.style.width = newW + "px";
      localStorage.setItem("sidebar_width", Math.round(newW));
      renderSideTransferList();
    };

    const onSidebarMouseUp = () => {
      sidebarResizer.classList.remove("resizing");
      document.body.style.cursor = "";
      document.body.style.userSelect = "";
      document.removeEventListener("mousemove", onSidebarMouseMove);
      document.removeEventListener("mouseup", onSidebarMouseUp);
      renderSideTransferList();
    };

    sidebarResizer.onmousedown = (e) => {
      e.preventDefault();
      startX = e.clientX;
      startW = sidebar.getBoundingClientRect().width;
      sidebarResizer.classList.add("resizing");
      document.body.style.cursor = "col-resize";
      document.body.style.userSelect = "none";
      document.addEventListener("mousemove", onSidebarMouseMove);
      document.addEventListener("mouseup", onSidebarMouseUp);
    };
  }

  // 传输队列面板顶部高度拖拽调整 (按住向上拖动扩展更多可视面积)
  const resizer = document.getElementById("sidePanelResizer");
  const panel = document.getElementById("sideTransferPanel");
  if (resizer && panel) {
    const savedH = localStorage.getItem("side_queue_panel_height");
    if (savedH) {
      panel.style.height = savedH + "px";
    }

    let startY = 0;
    let startH = 0;

    const onMouseMove = (e) => {
      const dy = startY - e.clientY;
      const newH = Math.max(160, Math.min(window.innerHeight * 0.8, startH + dy));
      panel.style.height = newH + "px";
      localStorage.setItem("side_queue_panel_height", Math.round(newH));
    };

    const onMouseUp = () => {
      resizer.classList.remove("dragging");
      document.removeEventListener("mousemove", onMouseMove);
      document.removeEventListener("mouseup", onMouseUp);
    };

    resizer.onmousedown = (e) => {
      e.preventDefault();
      startY = e.clientY;
      startH = panel.getBoundingClientRect().height;
      resizer.classList.add("dragging");
      document.addEventListener("mousemove", onMouseMove);
      document.addEventListener("mouseup", onMouseUp);
    };
  }

  // 全局文件区拖拽上传支持 (中心浅色水印全区域响应拖拽)
  const fileContainer = document.getElementById("fileContainer");
  if (fileContainer) {
    fileContainer.ondragover = (e) => {
      e.preventDefault();
      fileContainer.classList.add("dragover");
    };
    fileContainer.ondragleave = (e) => {
      if (!fileContainer.contains(e.relatedTarget)) {
        fileContainer.classList.remove("dragover");
      }
    };
    fileContainer.ondrop = async (e) => {
      e.preventDefault();
      fileContainer.classList.remove("dragover");
      if (e.dataTransfer) {
        const files = await extractFilesFromDataTransfer(e.dataTransfer);
        if (files && files.length > 0) {
          handleFilesUpload(files);
        }
      }
    };
  }

  // 检索输入框监听
  const globalSelectAllCb = document.getElementById("globalSelectAll");
  if (globalSelectAllCb) {
    globalSelectAllCb.onchange = (e) => {
      const curFiles = getFilteredAndSortedFiles();
      if (e.target.checked) {
        curFiles.forEach(f => selectedPaths.add(f.virtual_path));
      } else {
        curFiles.forEach(f => selectedPaths.delete(f.virtual_path));
      }
      updateBatchBar();
      renderFiles();
    };
  }

  // 清空已完成传输任务状态初始化
  updateClearCompletedButton();

  // 选择本地下载保存目录 (优先唤起原生系统目录选择窗，带完整盘符与绝对路径)
  const btnPickDir = document.getElementById("btnPickDownloadDir");
  if (btnPickDir) {
    btnPickDir.onclick = async () => {
      try {
        showToast("正在打开系统文件夹选择窗口...");
        const res = await fetch("/api/fs/pick_dir", { method: "POST" });
        const data = await res.json();
        if (data.status === "ok" && data.path) {
          const input = document.getElementById("cfgDownloadDir");
          if (input) input.value = data.path;
          showToast(`已选择保存目录: ${data.path}`);
          return;
        } else if (data.status === "cancelled") {
          return;
        }
      } catch (e) {
        console.error("调用系统目录选择器失败:", e);
      }

      // 降级提示输入 (使用网页模态输入窗，彻底替换 prompt)
      const custom = await showInputPrompt({
        title: "📁 设置本地保存目录",
        desc: "请输入您本地期望直接保存文件的物理路径 (如 D:\\Downloads 或 D:\\):",
        label: "本地文件夹路径",
        defaultValue: "D:\\Downloads"
      });
      if (custom) {
        const input = document.getElementById("cfgDownloadDir");
        if (input) input.value = custom.trim();
      }
    };
  }

  const searchInput = document.getElementById("searchInput");
  const searchClear = document.getElementById("searchClear");
  if (searchInput) {
    searchInput.oninput = (e) => {
      searchKeyword = e.target.value;
      if (searchClear) {
        if (searchKeyword) searchClear.classList.remove("hidden");
        else searchClear.classList.add("hidden");
      }
      renderFiles();
    };
  }
  if (searchClear) {
    searchClear.onclick = () => {
      searchKeyword = "";
      if (searchInput) searchInput.value = "";
      searchClear.classList.add("hidden");
      renderFiles();
    };
  }

  document.getElementById("filePicker").onchange = (e) => {
    handleFilesUpload(e.target.files);
    e.target.value = "";
  };
  document.getElementById("folderPicker").onchange = (e) => {
    handleFilesUpload(e.target.files);
    e.target.value = "";
  };

  // 展开收起日志
  document.getElementById("btnToggleLog").onclick = () => {
    const drawer = document.getElementById("logDrawer");
    drawer.classList.toggle("hidden");
    const isHidden = drawer.classList.contains("hidden");
    document.getElementById("btnToggleLog").innerText = isHidden ? "📜 查看日志" : "🔽 收起日志";
    if (!isHidden) {
      updateUnifiedLogs(true);
    }
  };

  // 绑定日志等级切换按钮 (DEBUG/INFO/WARN/ERROR)
  document.querySelectorAll(".log-level-btn").forEach(btn => {
    btn.onclick = () => {
      document.querySelectorAll(".log-level-btn").forEach(b => b.classList.remove("active"));
      btn.classList.add("active");
      currentLogLevel = (btn.dataset.level || "ALL").toUpperCase();
      updateUnifiedLogs(true);
    };
  });

  // 绑定日志模块切换按钮
  document.querySelectorAll(".log-module-btn").forEach(btn => {
    btn.onclick = () => {
      document.querySelectorAll(".log-module-btn").forEach(b => b.classList.remove("active"));
      btn.classList.add("active");
      currentLogModule = (btn.dataset.module || "ALL").toUpperCase();
      updateUnifiedLogs(true);
    };
  });

  // 绑定日志搜索框
  const logSearchInput = document.getElementById("logSearchInput");
  if (logSearchInput) {
    logSearchInput.oninput = (e) => {
      logSearchKeyword = (e.target.value || "").trim();
      updateUnifiedLogs(true);
    };
  }

  // 绑定日志导出按钮
  const btnExportLog = document.getElementById("btnExportLog");
  if (btnExportLog) {
    btnExportLog.onclick = () => {
      if (cachedStructuredLogs.length === 0) {
        showToast("暂无可导出的日志数据");
        return;
      }
      const textLines = cachedStructuredLogs.map(item => 
        `[${item.time_str || ""}] [${(item.level || "INFO").toUpperCase()}] [${item.module || "SYSTEM"}] ${item.message || ""}`
      );
      const blob = new Blob([textLines.join("\r\n")], { type: "text/plain;charset=utf-8" });
      const url = URL.createObjectURL(blob);
      const a = document.createElement("a");
      a.href = url;
      a.download = `QQMailDrive_Log_${new Date().toISOString().replace(/[:.]/g, "-")}.txt`;
      document.body.appendChild(a);
      a.click();
      document.body.removeChild(a);
      URL.revokeObjectURL(url);
      showToast("✔ 已成功导出系统日志文本");
    };
  }

  // 绑定清屏按钮
  const btnClearLog = document.getElementById("btnClearLogConsole");
  if (btnClearLog) {
    btnClearLog.onclick = async () => {
      await fetch("/api/logs/clear", { method: "POST" }).catch(()=>{});
      webClientLogs = [];
      cachedStructuredLogs = [];
      const consoleEl = document.getElementById("logConsole");
      if (consoleEl) consoleEl.innerHTML = `<div style="color: #64748b; padding: 12px;">[已清屏] 屏幕日志已清空，等待新日志输出...</div>`;
      lastRenderedLogSignature = "";
      showToast("已清空系统日志缓存");
    };
  }

  // 绑定自动滚屏切换按钮
  const btnAutoScroll = document.getElementById("btnToggleAutoScroll");
  if (btnAutoScroll) {
    btnAutoScroll.onclick = () => {
      isLogAutoScroll = !isLogAutoScroll;
      btnAutoScroll.classList.toggle("active", isLogAutoScroll);
      btnAutoScroll.innerText = isLogAutoScroll ? "⬇️ 自动滚屏" : "⏸ 暂停滚屏";
    };
  }

  const btnResumeAll = document.getElementById("btnResumeAll");
  if (btnResumeAll) {
    btnResumeAll.onclick = async () => {
      await fetch("/api/tasks/resume_all", { method: "POST" });
      activeTasks.forEach(t => { if (t.status === "paused") t.status = "running"; });
      clientTaskQueue.forEach(t => {
        if (t.status === "paused") {
          t.status = "running";
          if (t.paused_at) {
            t.total_paused_ms = (t.total_paused_ms || 0) + (Date.now() - t.paused_at);
            t.paused_at = null;
            t.paused_elapsed_sec = null;
          }
        }
      });
      renderSideTransferList();
      showToast("已启动全部传输任务");
    };
  }

  const btnPauseAll = document.getElementById("btnPauseAll");
  if (btnPauseAll) {
    btnPauseAll.onclick = async () => {
      await fetch("/api/tasks/pause_all", { method: "POST" });
      activeTasks.forEach(t => { if (t.status === "running") t.status = "paused"; });
      clientTaskQueue.forEach(t => {
        if (t.status === "running") {
          t.status = "paused";
          if (!t.paused_at) {
            t.paused_at = Date.now();
            t.paused_elapsed_sec = Math.max(0, (t.paused_at - (t.start_time || t.paused_at) - (t.total_paused_ms || 0)) / 1000);
          }
        }
      });
      renderSideTransferList();
      showToast("已暂停全部传输任务");
    };
  }

  const closePreviewFn = () => {
    const vEl = document.getElementById("previewVideoEl");
    if (vEl) {
      try {
        vEl.pause();
        vEl.removeAttribute("src");
        vEl.load();
      } catch (e) {}
    }
    document.getElementById("previewModal").classList.add("hidden");
    document.getElementById("previewContainer").innerHTML = "";
    if (currentPreviewNode) {
      const pid = "task_prev_" + currentPreviewNode.id;
      const existingTask = clientTaskQueue.find(x => x.id === pid);
      if (existingTask && existingTask.status !== "completed") {
        fetch(`/api/tasks/cancel?task_id=${encodeURIComponent(pid)}&reason=close_preview`, { method: "POST" }).catch(()=>{});
        clientTaskQueue = clientTaskQueue.filter(x => x.id !== pid);
      }
      renderSideTransferList();
      currentPreviewNode = null;
      const box = document.getElementById("modalPreviewBox");
      if (box) box.classList.remove("maximized");
      const btnMax = document.getElementById("btnModalMaximize");
      if (btnMax) { btnMax.innerText = "⛶"; btnMax.title = "最大化窗口"; }
    }
  };
  const btnClosePrev = document.getElementById("btnClosePreview");
  if (btnClosePrev) {
    btnClosePrev.onclick = closePreviewFn;
  }

  // 批量复制与批量移动按钮
  const btnBatchCopy = document.getElementById("btnBatchCopy");
  if (btnBatchCopy) {
    btnBatchCopy.onclick = () => {
      if (selectedPaths.size === 0) return;
      openCopyModal(Array.from(selectedPaths));
    };
  }

  document.getElementById("btnBatchMove").onclick = () => {
    if (selectedPaths.size === 0) return;
    openMoveModal(Array.from(selectedPaths));
  };

  // 移动弹窗按钮
    // 移动弹窗右上角关闭叉叉
  const btnCloseMove = document.getElementById("btnCloseMoveModal");
  if (btnCloseMove) {
    btnCloseMove.onclick = () => document.getElementById("moveModal").classList.add("hidden");
  }

  const btnMoveNewFolder = document.getElementById("btnMoveNewFolder");
  if (btnMoveNewFolder) {
    btnMoveNewFolder.onclick = async () => {
      const destName = (selectedMoveDest === "/" ? "根目录" : selectedMoveDest);
      const spaceName = selectedMoveDestFolder || cleanFolderDisplayName(currentActiveStorageFolder) || "当前空间";
      const folderName = await showInputPrompt({
        title: "📁 新建目标文件夹",
        desc: `在 [${spaceName}] 的 [${destName}] 下创建新子文件夹：`,
        label: "文件夹名称",
        placeholder: "请输入文件夹名称"
      });

      if (!folderName || !folderName.trim()) return;
      let cleanName = folderName.trim().replace(/^[\\\/]+|[\\\/]+$/g, "");
      cleanName = cleanName.replace(/\\/g, "/");
      if (!cleanName) return;
      const parentDir = (selectedMoveDest === "/" ? "" : selectedMoveDest);
      const newVPath = parentDir + "/" + cleanName;

      btnMoveNewFolder.disabled = true;
      btnMoveNewFolder.innerText = "⏳ 创建中...";
      try {
        const fd = new FormData();
        fd.append("path", newVPath);
        if (selectedMoveDestFolder) {
          fd.append("target_folder", selectedMoveDestFolder);
        }
        const mkRes = await fetch("/api/fs/mkdir", { method: "POST", body: fd });
        if (mkRes.ok) {
          showToast(`✔ 成功在目标位置创建新文件夹 "${cleanName}"`);
          logWebTransfer(`✔ [目标新建文件夹] 成功创建: ${newVPath}`);
          await loadFiles();
          selectedMoveDest = newVPath;
          buildMoveModalPermanentTree(moveTargetPaths);
          updateMoveTargetSummaryText();
        } else {
          const txt = await mkRes.text();
          showToast(`❌ 创建文件夹失败: ${txt || mkRes.status}`, true);
        }
      } catch (err) {
        showToast(`❌ 创建请求异常: ${err.message}`, true);
      } finally {
        btnMoveNewFolder.disabled = false;
        btnMoveNewFolder.innerHTML = "<span>📁</span><span>新建文件夹</span>";
      }
    };
  }

  document.getElementById("btnCancelMove").onclick = () => {
    document.getElementById("moveModal").classList.add("hidden");
  };

  document.getElementById("btnConfirmMove").onclick = async () => {
    if (moveTargetPaths.length === 0) return;
    const btn = document.getElementById("btnConfirmMove");
    const actionName = (transferActionType === "copy") ? "复制" : "移动";
    const apiUrl = (transferActionType === "copy") ? "/api/fs/copy" : "/api/fs/move";

    btn.disabled = true;
    btn.innerText = `⏳ ${actionName}中...`;

    try {
      const payload = { paths: moveTargetPaths, target_dir: selectedMoveDest };
      if (selectedMoveDestFolder) {
        payload.target_folder = selectedMoveDestFolder;
      }
      const res = await fetch(apiUrl, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
      });
      const data = await res.json();
      btn.disabled = false;
      btn.innerText = `确认${actionName}`;
      document.getElementById("moveModal").classList.add("hidden");

      if (data.status === "ok") {
        // 移动模式清理已移走项目勾选；复制模式保持原有多选勾选不动 (保持高亮与多选栏就绪)
        if (transferActionType === "move") {
          selectedPaths.clear();
          updateBatchBar();
        }
        const targetDesc = selectedMoveDestFolder ? `[${selectedMoveDestFolder}] -> ${selectedMoveDest}` : selectedMoveDest;
        logWebTransfer(`✔ [${actionName}成功] 成功${actionName} ${moveTargetPaths.length} 个项目至: ${targetDesc}`);
        showToast(`✔ 项目${actionName}成功！`);
        await loadFiles();
        await loadStatus();
        if (transferActionType === "copy") {
          updateBatchBar();
          renderFiles();
        }
      } else {
        const errDetail = data.message || "操作受阻";
        logWebTransfer(`❌ [${actionName}失败] ${actionName}项目失败: ${errDetail}`);
        showToast(`${actionName}失败: ` + errDetail, true);
      }
    } catch (err) {
      btn.disabled = false;
      btn.innerText = `确认${actionName}`;
      logWebTransfer(`❌ [${actionName}异常] ${actionName}请求异常: ${err.message}`);
      showToast(`${actionName}请求异常: ` + err.message, true);
    }
  };

  // 批量操作栏按钮
  document.getElementById("btnCancelBatch").onclick = () => {
    selectedPaths.clear();
    updateBatchBar();
    renderFiles();
  };

  document.getElementById("btnBatchDelete").onclick = async () => {
    if (selectedPaths.size === 0) return;
    const count = selectedPaths.size;
    const ok = await showConfirmDialog({
      title: "🗑️ 批量彻底删除",
      message: `确定要彻底删除已勾选的 ${count} 个项目吗？\n若包含文件夹，将递归销毁其内部所有的子文件及多层嵌套文件夹！`,
      isDanger: true,
      confirmText: `彻底删除已选 ${count} 项`
    });
    if (ok) {
      await executeBatchDelete(Array.from(selectedPaths));
    }
  };

  document.getElementById("btnBatchDownload").onclick = () => {
    if (selectedPaths.size === 0) return;
    const toDownload = filesData.filter(f => selectedPaths.has(f.virtual_path));
    requestWebDownload(toDownload);
  };

  // 视图切换按钮
  document.getElementById("btnToggleView").onclick = () => {
    isListView = !isListView;
    document.getElementById("btnToggleView").innerText = isListView ? "🔲 切换网格" : "📋 切换列表";
    renderFiles();
  };

  // 文件夹树：一键全部展开/折叠
  document.getElementById("btnExpandAll").onclick = () => {
    const root = buildFolderTreeData();
    const allPaths = [];
    function collectPaths(n) {
      allPaths.push(n.path);
      if (n.children) n.children.forEach(collectPaths);
    }
    collectPaths(root);

    if (expandedFolders.size >= allPaths.length) {
      expandedFolders.clear();
      expandedFolders.add("/");
      document.getElementById("btnExpandAll").innerText = "⛶ 展开全部";
    } else {
      allPaths.forEach(p => expandedFolders.add(p));
      document.getElementById("btnExpandAll").innerText = "⊟ 折叠全部";
    }
    renderFolderTree();
  };

  // 深度整理
  document.getElementById("btnCompact").onclick = async () => {
    if (!isConfiguredState) {
      showToast("请先完成设置并连入 QQ 邮箱！", true);
      return;
    }
    try {
      const res = await fetch("/api/config");
      const cfg = await res.json();
      const mb = cfg.chunk_size_bytes ? Math.round(cfg.chunk_size_bytes / (1024 * 1024)) : 50;
      const sizeSpan = document.getElementById("dispCompactChunkSize");
      if (sizeSpan) sizeSpan.innerText = `${mb}MB`;
    } catch (e) {}
    document.getElementById("compactModal").classList.remove("hidden");
  };
  document.getElementById("btnCancelCompact").onclick = () => {
    document.getElementById("compactModal").classList.add("hidden");
  };
  document.getElementById("btnConfirmCompact").onclick = async () => {
    document.getElementById("compactModal").classList.add("hidden");
    const inputVal = await showInputPrompt({
      title: "⚠️ 深度整理功能确认",
      desc: "本功能目前尚在完善中，非必要请勿执行！如确实需要执行，请输入字母 y 确认：",
      label: "输入 y 确认执行",
      defaultValue: ""
    });
    if (!inputVal || inputVal.trim().toLowerCase() !== "y") {
      showToast("已取消深度整理");
      return;
    }

    const btn = document.getElementById("btnConfirmCompact");
    btn.disabled = true;
    btn.innerText = "⏳ 正在进行组合装箱重组中...";
    try {
      const res = await fetch("/api/maintenance/compact", { method: "POST" });
      const data = await res.json();
      btn.disabled = false;
      btn.innerText = "🚀 开始执行整理";
      if (data.status === "ok") {
        showToast(`✔ 深度整理完成！释放了 ${data.saved_mails || 0} 封邮件配额！`);
        await loadFiles();
        await loadStatus();
      } else {
        showToast("整理异常: " + (data.message || "执行失败"), true);
      }
    } catch (e) {
      btn.disabled = false;
      btn.innerText = "🚀 开始执行整理";
      showToast("请求失败: " + e.message, true);
    }
  };

  // 统一空间探测与进入调度器 (首次登录与设置切账号对齐)
  let frVerifiedCredentials = null;
  let frDetectedFolders = [];
  let frFromSettings = false;
  const btnFrStep1Next = document.getElementById("btnFrStep1Next");
  const btnFrBackToStep1 = document.getElementById("btnFrBackToStep1");
  const btnFrFinish = document.getElementById("btnFrFinish");

  // 统一三态调度：0个创建，1个默认进入，多个选择进入 (严格排除收到的文件)
  async function resolveAccountAndMount(email, authCode, extraCfg = {}, fromSettings = false) {
    frFromSettings = fromSettings;
    showCloudLoading(true, "正在连接 QQ 邮箱并探测云端隔离存储空间...", "系统正在通过 IMAP TLS 建立加密长连接并扫描 QMD 规范分区");
    try {
      const res = await fetch("/api/init/check", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ email, imap_auth_code: authCode })
      });
      const data = await res.json();
      showCloudLoading(false);

      if (data.status !== "ok") {
        showToast("QQ 邮箱验证失败: " + (data.message || "请检查账号与授权码"), true);
        return false;
      }

      // 提取探测到的隔离网盘空间 (包含「收到的文件」专属目录)
      const detected = (data.existing_folders || []);
      frVerifiedCredentials = { email, authCode, ...extraCfg };
      frDetectedFolders = detected;

      // 规则 1: 0 个有效隔离空间 -> 弹窗让用户创建
      if (detected.length === 0) {
        if (fromSettings) {
          document.getElementById("settingsModal").classList.add("hidden");
        }
        document.getElementById("firstRunModal").classList.remove("hidden");
        document.getElementById("frStep1").classList.add("hidden");
        document.getElementById("frStep2").classList.remove("hidden");
        document.getElementById("frOldUserSection").classList.add("hidden");
        document.getElementById("frNewUserSection").classList.remove("hidden");
        const defaultInput = document.getElementById("frNewUserFolder");
        if (defaultInput) defaultInput.value = data.default_folder || "MailDrive";
        btnFrFinish.innerText = "🚀 进入选中的空间";
        return true;
      }

      // 规则 2: 检测到 1 个及以上隔离空间 -> 跳过选择弹窗，有记录则进上次空间，否则默认进第一个
      if (detected.length >= 1) {
        detected.sort((a, b) => {
          const aName = cleanFolderDisplayName(a.display_name || a.name);
          const bName = cleanFolderDisplayName(b.display_name || b.name);
          const aRec = (aName === "收到的文件");
          const bRec = (bName === "收到的文件");
          if (aRec && !bRec) return 1;
          if (!aRec && bRec) return -1;
          return aName.localeCompare(bName, "zh-CN", { numeric: true, sensitivity: "base" });
        });

        let savedLast = "";
        try {
          savedLast = cleanFolderDisplayName(extraCfg.storage_folder || localStorage.getItem("qmd_last_folder_" + email) || "");
        } catch (e) {}

        const matchedSaved = savedLast
          ? detected.find(f => cleanFolderDisplayName(f.display_name || f.name) === savedLast)
          : null;

        const chosenItem = matchedSaved || detected[0];
        const targetFolder = cleanFolderDisplayName(chosenItem.display_name || chosenItem.name) || "MailDrive";

        showCloudLoading(true, `正在为您连入专属隔离空间 [${targetFolder}]...`, "系统正在建立 IMAP 加密选区并构建安全主索引");
        const savePayload = {
          email,
          imap_auth_code: authCode,
          storage_folder: targetFolder,
          ...extraCfg
        };
        const cRes = await fetch("/api/config", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(savePayload)
        });
        const cData = await cRes.json();
        showCloudLoading(false);

        if (cData.status === "ok") {
          document.getElementById("firstRunModal").classList.add("hidden");
          if (fromSettings) {
            document.getElementById("settingsModal").classList.add("hidden");
          }
          currentActiveStorageFolder = cleanFolderDisplayName(cData.folder || targetFolder);
          try {
            localStorage.setItem("qmd_last_folder_" + email, currentActiveStorageFolder);
          } catch (e) {}
          currentPath = "/";
          selectedPaths.clear();
          showToast(`✔ 登录成功！已自动进入专属隔离空间 [${currentActiveStorageFolder}]！`);
          await loadStatus();
          showCloudLoading(true);
          await loadFiles("/");
          await loadIsolatedSpaces();
          showCloudLoading(false);
          return true;
        } else {
          showToast("进入空间失败: " + (cData.message || ""), true);
          return false;
        }
      }
      return true;
    } catch (err) {
      showCloudLoading(false);
      showToast("检测异常: " + err.message, true);
      return false;
    }
  }

  const frEmail = document.getElementById("frEmail");
  const frAuthCode = document.getElementById("frAuthCode");
  const frNewUserFolder = document.getElementById("frNewUserFolder");

  if (frEmail && btnFrStep1Next) {
    frEmail.addEventListener("keydown", (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        if (frAuthCode && !frAuthCode.value.trim()) {
          frAuthCode.focus();
        } else {
          btnFrStep1Next.click();
        }
      }
    });
  }
  if (frAuthCode && btnFrStep1Next) {
    frAuthCode.addEventListener("keydown", (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        btnFrStep1Next.click();
      }
    });
  }
  if (frNewUserFolder && btnFrFinish) {
    frNewUserFolder.addEventListener("keydown", (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        btnFrFinish.click();
      }
    });
  }

  if (btnFrStep1Next) {
    btnFrStep1Next.onclick = async () => {
      let emailRaw = document.getElementById("frEmail").value.trim();
      const email = emailRaw.includes("@") ? emailRaw : (emailRaw + "@qq.com");
      const authCode = document.getElementById("frAuthCode").value.trim();

      if (!emailRaw || !authCode) {
        showToast("请输入完整的 QQ 账号与 16 位 IMAP 授权码！", true);
        return;
      }

      btnFrStep1Next.disabled = true;
      btnFrStep1Next.innerText = "⏳ 正在连接 QQ 邮箱并探测空间...";
      await resolveAccountAndMount(email, authCode, {}, false);
      btnFrStep1Next.disabled = false;
      btnFrStep1Next.innerText = "下一步：验证并检测云端空间 ➔";
    };
  }

  if (btnFrBackToStep1) {
    btnFrBackToStep1.onclick = () => {
      document.getElementById("frStep2").classList.add("hidden");
      if (frFromSettings) {
        document.getElementById("firstRunModal").classList.add("hidden");
        document.getElementById("settingsModal").classList.remove("hidden");
      } else {
        document.getElementById("frStep1").classList.remove("hidden");
      }
    };
  }

  if (btnFrFinish) {
    btnFrFinish.onclick = async () => {
      if (!frVerifiedCredentials) {
        showToast("凭据已失效，请重新验证", true);
        document.getElementById("frStep2").classList.add("hidden");
        if (frFromSettings) {
          document.getElementById("firstRunModal").classList.add("hidden");
          document.getElementById("settingsModal").classList.remove("hidden");
        } else {
          document.getElementById("frStep1").classList.remove("hidden");
        }
        return;
      }

      let chosenFolder = "MailDrive";
      const oldUserSec = document.getElementById("frOldUserSection");

      if (oldUserSec && !oldUserSec.classList.contains("hidden")) {
        const checked = document.querySelector('input[name="frFolderSelect"]:checked');
        chosenFolder = checked ? checked.value : (frDetectedFolders[0]?.display_name || frDetectedFolders[0]?.name || "MailDrive");
      } else {
        const inputVal = document.getElementById("frNewUserFolder").value.trim();
        chosenFolder = inputVal || "MailDrive";
      }

      btnFrFinish.disabled = true;
      btnFrFinish.innerText = "🚀 正在初始化空间并进入...";
      showCloudLoading(true, `正在为您初始化云端隔离空间 [${chosenFolder}]...`, "系统正在建立 IMAP 加密选区并构建安全主索引");

      try {
        const payload = {
          email: frVerifiedCredentials.email,
          imap_auth_code: frVerifiedCredentials.authCode,
          storage_folder: chosenFolder,
        };
        if (typeof frVerifiedCredentials.chunk_size_bytes === "number") payload.chunk_size_bytes = frVerifiedCredentials.chunk_size_bytes;
        if (typeof frVerifiedCredentials.chunk_size_mb === "number") payload.chunk_size_mb = frVerifiedCredentials.chunk_size_mb;
        if (frVerifiedCredentials.download_dir) payload.download_dir = frVerifiedCredentials.download_dir;
        if (typeof frVerifiedCredentials.max_concurrency === "number") payload.max_concurrency = frVerifiedCredentials.max_concurrency;
        if (typeof frVerifiedCredentials.chunk_download_concurrency === "number") payload.chunk_download_concurrency = frVerifiedCredentials.chunk_download_concurrency;
        if (typeof frVerifiedCredentials.request_delay_ms === "number") payload.request_delay_ms = frVerifiedCredentials.request_delay_ms;

        const res = await fetch("/api/config", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(payload),
        });
        const data = await res.json();
        btnFrFinish.disabled = false;
        btnFrFinish.innerText = "🚀 进入选中的空间";
        showCloudLoading(false);

        if (data.status === "ok") {
          document.getElementById("firstRunModal").classList.add("hidden");
          currentActiveStorageFolder = cleanFolderDisplayName(data.folder || chosenFolder);
          currentPath = "/";
          selectedPaths.clear();
          showToast(`✔ 欢迎使用！已成功连入专属网盘空间 [${currentActiveStorageFolder}]！`);
          await loadStatus();
          showCloudLoading(true);
          await loadFiles("/");
          renderFolderTree();
          showCloudLoading(false);
        } else {
          showToast("初始化失败: " + (data.message || "请检查邮箱配置"), true);
        }
      } catch (err) {
        btnFrFinish.disabled = false;
        btnFrFinish.innerText = "🚀 进入选中的空间";
        showCloudLoading(false);
        showToast("请求异常: " + err.message, true);
      }
    };
  }

  // 异常对账确认
  const btnConfirm = document.getElementById("btnConfirmRepair");
  if (btnConfirm) {
    btnConfirm.onclick = async () => {
      crashPromptDismissed = true;
      await fetch("/api/status/ack_clean_exit", { method: "POST" });
      triggerHealthCheck();
    };
  }
  const btnSkip = document.getElementById("btnSkipRepair");
  if (btnSkip) {
    btnSkip.onclick = async () => {
      crashPromptDismissed = true;
      await fetch("/api/status/ack_clean_exit", { method: "POST" });
    };
  }

  document.getElementById("btnHealthCheck").onclick = () => triggerHealthCheck();

  // 设置弹窗标签切换函数 (账号 / 功能 / 上传 / 下载)
  function switchSettingsTab(tabName) {
    const modal = document.getElementById("settingsModal");
    if (!modal) return;
    const tabBtns = modal.querySelectorAll(".settings-tab-btn");
    const panels = modal.querySelectorAll(".settings-panel");
    tabBtns.forEach(btn => {
      btn.classList.toggle("active", btn.dataset.tab === tabName);
    });
    panels.forEach(p => {
      p.classList.toggle("active", p.id === `settingsPanel-${tabName}`);
    });
  }

  // 设置面板
  document.getElementById("btnSettings").onclick = async () => {
    if (!isConfiguredState) {
      showToast("请先完成首次配置并连入 QQ 邮箱！", true);
      document.getElementById("firstRunModal").classList.remove("hidden");
      return;
    }
    const res = await fetch("/api/config");
    const cfg = await res.json();
    currentActiveStorageFolder = cleanFolderDisplayName(cfg.display_folder || cfg.storage_folder || currentActiveStorageFolder || "");
    loadedSettingsEmail = (cfg.email || "").toLowerCase().trim();
    loadedSettingsAuthCode = (cfg.imap_auth_code || "").trim();
    document.getElementById("cfgEmail").value = (cfg.email || "").replace("@qq.com", "");
    document.getElementById("cfgAuthCode").value = cfg.imap_auth_code || "";
    const nickInput = document.getElementById("cfgUserNick");
    if (nickInput) nickInput.value = cfg.user_nick || "";
    const byteInput = document.getElementById("cfgChunkBytes");
    if (byteInput) {
      const bytes = cfg.chunk_size_bytes || 52428799;
      byteInput.value = bytes;
      updateChunkSizeLabel(bytes);
      byteInput.oninput = (e) => updateChunkSizeLabel(parseInt(e.target.value, 10) || 0);
    }
    configuredDownloadDir = cfg.download_dir || "";
    const dlDirInput = document.getElementById("cfgDownloadDir");
    if (dlDirInput) dlDirInput.value = configuredDownloadDir;
    const cidEl = document.getElementById("cfgComputerId");
    if (cidEl) cidEl.innerText = cfg.computer_id || "--";

    // 严格以配置为准
    configuredDelayMS = (typeof cfg.request_delay_ms === "number" && cfg.request_delay_ms >= 0) ? cfg.request_delay_ms : 250;
    maxUploadConcurrency = (typeof cfg.max_concurrency === "number" && cfg.max_concurrency >= 1) ? Math.min(3, cfg.max_concurrency) : 1;
    maxDownloadConcurrency = (typeof cfg.chunk_download_concurrency === "number" && cfg.chunk_download_concurrency >= 1) ? Math.min(5, cfg.chunk_download_concurrency) : 3;

    document.getElementById("cfgDelay").value = configuredDelayMS;
    document.getElementById("cfgConcurrency").value = maxUploadConcurrency;
    if (document.getElementById("cfgChunkConcurrency")) {
      document.getElementById("cfgChunkConcurrency").value = maxDownloadConcurrency;
    }
    if (document.getElementById("cfgHTTPDownloadConcurrency")) {
      document.getElementById("cfgHTTPDownloadConcurrency").value = (typeof cfg.http_download_concurrency === "number" && cfg.http_download_concurrency >= 0) ? Math.min(5, cfg.http_download_concurrency) : 3;
    }
    if (document.getElementById("cfgHTTPUploadConcurrency")) {
      document.getElementById("cfgHTTPUploadConcurrency").value = (typeof cfg.http_upload_concurrency === "number" && cfg.http_upload_concurrency >= 0) ? Math.min(3, cfg.http_upload_concurrency) : 1;
    }
    const thumbCacheEl = document.getElementById("cfgThumbLocalCache");
    if (thumbCacheEl) {
      thumbCacheEl.checked = (cfg.thumb_local_cache !== false);
    }

    switchSettingsTab("account");
    document.getElementById("settingsModal").classList.remove("hidden");
  };

  // 设置弹窗标签切换 (账号 / 功能 / 上传 / 下载)
  document.querySelectorAll("#settingsModal .settings-tab-btn").forEach(btn => {
    btn.onclick = (e) => {
      e.preventDefault();
      switchSettingsTab(btn.dataset.tab);
    };
  });

    // 全局模态窗右上角圆形关闭按钮统一绑定
  const btnCloseNF = document.getElementById("btnCloseNewFolderModal");
  if (btnCloseNF) {
    btnCloseNF.onclick = () => document.getElementById("btnCancelNewFolder").click();
  }
  const btnCloseCompact = document.getElementById("btnCloseCompactModal");
  if (btnCloseCompact) {
    btnCloseCompact.onclick = () => document.getElementById("btnCancelCompact").click();
  }
  const btnCloseHealth = document.getElementById("btnCloseHealthModal");
  if (btnCloseHealth) {
    btnCloseHealth.onclick = () => document.getElementById("btnCloseHealth").click();
  }
  const btnCloseSettings = document.getElementById("btnCloseSettingsModal");
  if (btnCloseSettings) {
    btnCloseSettings.onclick = () => document.getElementById("btnCloseSettings").click();
  }
  const btnCloseShare = document.getElementById("btnCloseShareModal");
  if (btnCloseShare) {
    btnCloseShare.onclick = () => document.getElementById("btnCancelShare").click();
  }

  document.getElementById("btnCloseSettings").onclick = () => {
    document.getElementById("settingsModal").classList.add("hidden");
  };

  // 安全退出客户端
  const btnExitAppEl = document.getElementById("btnExitApp");
  if (btnExitAppEl) {
    btnExitAppEl.onclick = async () => {
      if (!confirm("确定要安全退出 QQMailDrive 吗？\n\n退出后将：\n1. 自动同步并保存云端主索引\n2. 释放 IMAP 及 HTTP 长连接通道\n3. 关闭本地核心服务进程\n\n退出后您可以随时重新打开 QQMailDrive.exe 再次启动。")) {
        return;
      }
      showToast("正在安全保存索引并退出服务...", 3000);
      setBackendOfflineUI(true);
      try {
        await fetch("/api/exit", { method: "POST" });
      } catch (e) {}
      showToast("✔ QQMailDrive 已安全退出，您可以关闭此网页", 8000);
    };
  }

  let loadedSettingsEmail = "";
  let loadedSettingsAuthCode = "";

  const settingsFormEl = document.getElementById("settingsForm");
  if (settingsFormEl) {
    settingsFormEl.querySelectorAll("input").forEach(input => {
      input.addEventListener("keydown", (e) => {
        if (e.key === "Enter") {
          e.preventDefault();
          const submitBtn = settingsFormEl.querySelector("button[type='submit']");
          if (submitBtn) submitBtn.click();
        }
      });
    });
  }

  document.getElementById("settingsForm").onsubmit = async (e) => {
    e.preventDefault();
    const submitBtn = document.getElementById("settingsForm").querySelector("button[type='submit']");
    if (submitBtn) {
      submitBtn.disabled = true;
      submitBtn.innerText = "⏳ 正在保存设置...";
    }
    const rawEmail = document.getElementById("cfgEmail").value.trim();
    if (!rawEmail) {
      showToast("请输入 QQ 邮箱账号！", true);
      switchSettingsTab("account");
      const el = document.getElementById("cfgEmail");
      if (el) el.focus();
      if (submitBtn) { submitBtn.disabled = false; submitBtn.innerText = "保存设置"; }
      return;
    }
    const authCode = document.getElementById("cfgAuthCode").value.trim();
    if (!authCode) {
      showToast("请输入 IMAP 授权码！", true);
      switchSettingsTab("account");
      const el = document.getElementById("cfgAuthCode");
      if (el) el.focus();
      if (submitBtn) { submitBtn.disabled = false; submitBtn.innerText = "保存设置"; }
      return;
    }
    const chunkBytes = parseInt(document.getElementById("cfgChunkBytes").value, 10) || 52428799;
    if (chunkBytes < 1048576) {
      showToast("单分片大小不能小于 1MB (1048576 字节)！", true);
      switchSettingsTab("features");
      const el = document.getElementById("cfgChunkBytes");
      if (el) el.focus();
      if (submitBtn) { submitBtn.disabled = false; submitBtn.innerText = "保存设置"; }
      return;
    }
    const finalEmail = rawEmail.includes("@") ? rawEmail : (rawEmail + "@qq.com");
    const userNickVal = (document.getElementById("cfgUserNick") ? document.getElementById("cfgUserNick").value.trim() : "");
    const dlDirVal = (document.getElementById("cfgDownloadDir") ? document.getElementById("cfgDownloadDir").value.trim() : "");
    configuredDownloadDir = dlDirVal;

    // 读取用户真实输入的延时与并发数，用户设 0 就是 0，设 250 就是 250，绝不强行锁死 500！
    const userDelay = parseInt(document.getElementById("cfgDelay").value, 10);
    configuredDelayMS = isNaN(userDelay) ? 250 : Math.max(0, userDelay);

    const userUploadConc = parseInt(document.getElementById("cfgConcurrency").value, 10);
    maxUploadConcurrency = isNaN(userUploadConc) ? 1 : Math.min(3, Math.max(1, userUploadConc));

    const userDlConc = document.getElementById("cfgChunkConcurrency") ? parseInt(document.getElementById("cfgChunkConcurrency").value, 10) : 3;
    maxDownloadConcurrency = (isNaN(userDlConc) || userDlConc < 1) ? 3 : Math.min(5, Math.max(1, userDlConc));
    const userHttpDlConc = document.getElementById("cfgHTTPDownloadConcurrency") ? parseInt(document.getElementById("cfgHTTPDownloadConcurrency").value, 10) : 3;
    const finalHttpDlConc = isNaN(userHttpDlConc) ? 3 : Math.min(5, Math.max(0, userHttpDlConc));
    const userHttpUpConc = document.getElementById("cfgHTTPUploadConcurrency") ? parseInt(document.getElementById("cfgHTTPUploadConcurrency").value, 10) : 1;
    const finalHttpUpConc = isNaN(userHttpUpConc) ? 1 : Math.min(3, Math.max(0, userHttpUpConc));
    const thumbLocalCacheVal = document.getElementById("cfgThumbLocalCache") ? document.getElementById("cfgThumbLocalCache").checked : true;

    // 判定是否切换了账号凭据 (邮箱或授权码变更)
    const isAccountChanged = (loadedSettingsEmail !== "" && finalEmail.toLowerCase().trim() !== loadedSettingsEmail) ||
                            (loadedSettingsAuthCode !== "" && authCode !== loadedSettingsAuthCode);

    if (isAccountChanged) {
      // 切换账号走统一空间探测调度器 (0个创建、1个直入、多个选择，且排除收到的文件)
      if (submitBtn) {
        submitBtn.disabled = true;
        submitBtn.innerText = "⏳ 正在探测新账号空间...";
      }
      const ok = await resolveAccountAndMount(finalEmail, authCode, {
        user_nick: userNickVal,
        chunk_size_bytes: chunkBytes,
        chunk_size_mb: Math.round(chunkBytes / (1024 * 1024)),
        download_dir: dlDirVal,
        max_concurrency: maxUploadConcurrency,
        chunk_download_concurrency: maxDownloadConcurrency,
        http_download_concurrency: finalHttpDlConc,
        http_upload_concurrency: finalHttpUpConc,
        request_delay_ms: configuredDelayMS,
        thumb_local_cache: thumbLocalCacheVal,
      }, true);

      if (submitBtn) {
        submitBtn.disabled = false;
        submitBtn.innerText = "保存设置";
      }
      return;
    }

    // 账号未变：仅更新并发、延时等常规参数，秒级生效
    const cfg = {
      email: finalEmail,
      user_nick: userNickVal,
      imap_auth_code: authCode,
      storage_folder: currentActiveStorageFolder, // 保持用户当前已打开的文件夹不变
      chunk_size_bytes: chunkBytes,
      chunk_size_mb: Math.round(chunkBytes / (1024 * 1024)),
      download_dir: dlDirVal,
      max_concurrency: maxUploadConcurrency,
      chunk_download_concurrency: maxDownloadConcurrency,
      http_download_concurrency: finalHttpDlConc,
      http_upload_concurrency: finalHttpUpConc,
      request_delay_ms: configuredDelayMS,
      thumb_local_cache: thumbLocalCacheVal,
    };

    const res = await fetch("/api/config", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(cfg),
    });
    const data = await res.json();
    document.getElementById("settingsModal").classList.add("hidden");
    if (data.status === "ok") {
      logWebTransfer(`✔ [系统设置] 配置已更新并同步至云端: 下载并发 ${maxDownloadConcurrency} 链路, 出队延时 ${configuredDelayMS}ms`);
      showToast(`✔ 设置已保存并同步至云端！(当前电脑下载目录已绑定)`);
    } else {
      logWebTransfer(`⚠️ [系统设置] 设置保存提示: ${data.message || "请核对授权码"}`);
      showToast("设置已保存，但连接提示: " + (data.message || "请核对授权码"), true);
    }
    if (submitBtn) {
      submitBtn.disabled = false;
      submitBtn.innerText = "保存设置";
    }
    await loadStatus();
    await loadFiles();
    processQueueDispatcher();
  };

  
  // 绑定分享弹窗与极速免流云转发 (支持微信/QQ扫码免流云转 + SMTP保底)
  const shareModal = document.getElementById("shareModal");
  const shareTargetEmail = document.getElementById("shareTargetEmail");
  const shareFilesDesc = document.getElementById("shareFilesDesc");
  const btnConfirmShare = document.getElementById("btnConfirmShare");
  const btnCancelShare = document.getElementById("btnCancelShare");
  const webmailStatusBox = document.getElementById("webmailStatusBox");
  const webmailQRBox = document.getElementById("webmailQRBox");
  const webmailQRImg = document.getElementById("webmailQRImg");
  const webmailQRTip = document.getElementById("webmailQRTip");
  const btnRefreshQR = document.getElementById("btnRefreshQR");
  const btnManualInputSid = document.getElementById("btnManualInputSid");

  let pendingSharePaths = [];
  let webmailQRPollTimer = null;
  let currentWebmailQRSig = "";
  let currentWebmailUUID = "";
  let isWebmailValidSession = false;

  async function checkWebmailStatus() {
    try {
      const res = await fetch("/api/webmail/status");
      const data = await res.json();
      isWebmailValidSession = !!(data.status === "ok" && data.is_valid);
      if (isWebmailValidSession) {
        if (webmailStatusBox) webmailStatusBox.classList.remove("hidden");
        if (webmailQRBox) webmailQRBox.classList.add("hidden");
        stopWebmailQRPoll();
      } else {
        if (webmailStatusBox) webmailStatusBox.classList.add("hidden");
        if (webmailQRBox) webmailQRBox.classList.remove("hidden");
      }
    } catch (e) {
      isWebmailValidSession = false;
    }
  }

  function stopWebmailQRPoll() {
    if (webmailQRPollTimer) {
      clearInterval(webmailQRPollTimer);
      webmailQRPollTimer = null;
    }
  }

  async function loadWebmailQRCode(customTipHtml = "") {
    if (customTipHtml === "qq" || customTipHtml === "wx") customTipHtml = "";
    stopWebmailQRPoll();

    if (customTipHtml) {
      if (webmailQRTip) webmailQRTip.innerHTML = customTipHtml;
    } else {
      if (webmailQRTip) webmailQRTip.innerText = "⏳ 正在生成手机 QQ 官方登录二维码...";
    }

    try {
      const res = await fetch("/api/webmail/qr");
      const data = await res.json();
      if (data.status === "ok" && data.qr_b64) {
        currentWebmailQRSig = data.qrsig || "";
        if (webmailQRImg) webmailQRImg.src = data.qr_b64;
        if (!customTipHtml && webmailQRTip) {
          webmailQRTip.innerText = "请使用当前账号对应的【手机 QQ】扫码授权";
        }

        // 启动高频心跳轮询状态
        webmailQRPollTimer = setInterval(async () => {
          if (shareModal && shareModal.classList.contains("hidden")) {
            stopWebmailQRPoll();
            return;
          }
          try {
            const cRes = await fetch("/api/webmail/qr_check", {
              method: "POST",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify({ qrsig: currentWebmailQRSig })
            });
            const cData = await cRes.json();
            if (cData.code === 0) {
              stopWebmailQRPoll();
              isWebmailValidSession = true;
              isHttpConnected = true;
              sessionSkipHttpPrompt = false;
              loadStatus();
              if (webmailStatusBox) webmailStatusBox.classList.remove("hidden");
              if (webmailQRBox) webmailQRBox.classList.add("hidden");
              showToast("✔ 手机 QQ 扫码授权成功，账号校验一致！云端秒转已就绪！");
            } else if (cData.code === -1 || cData.status === "error" || (cData.message && (cData.message.includes("安全拦截") || cData.message.includes("不一致")))) {
              stopWebmailQRPoll();
              const errMsg = cData.message || "❌ [安全拦截] 扫码账号与当前网盘配置账号不一致！已自动终止授权，防止串号！请换用当前账号对应的手机 QQ 扫码。";
              const tipHtml = `<div style="color: #dc2626; font-weight: 600; line-height: 1.4; padding: 6px 10px; background: #fef2f2; border-radius: 6px; border: 1px solid #fecaca; margin-top: 6px; text-align: left; font-size: 12px;">${escapeHtml(errMsg)}<div style="font-size: 11px; color: #b91c1c; margin-top: 3px;">⚡ 二维码已自动刷新，请换用当前账号对应的手机 QQ 重新扫码</div></div>`;
              // 用户体验优化：无需弹窗和气泡打扰，直接在卡片内展示拦截提醒，并全自动无感换取新二维码
              loadWebmailQRCode(tipHtml);
            } else if (cData.code === 67) {
              if (webmailQRTip) webmailQRTip.innerText = "📱 手机 QQ 已扫码，请在手机上点击确认登录";
            } else if (cData.code === 65) {
              if (webmailQRTip) webmailQRTip.innerText = "⚠️ 二维码已失效，请点击下方刷新";
              stopWebmailQRPoll();
            } else if (cData.status === "error") {
              stopWebmailQRPoll();
              if (webmailQRTip) webmailQRTip.innerText = "⚠️ 扫码授权失败: " + (cData.message || "");
            }
          } catch (err) {}
        }, 1800);
      } else {
        if (webmailQRTip) webmailQRTip.innerText = "获取二维码失败: " + (data.message || "");
      }
    } catch (err) {
      if (webmailQRTip) webmailQRTip.innerText = "网络异常: " + err.message;
    }
  }



  if (btnRefreshQR) {
    btnRefreshQR.onclick = () => loadWebmailQRCode();
  }

  // 手动录入 sid 功能已按需移除，完全依赖官方扫码安全绑定与自动云端漫游

  async function openShareModal(paths) {
    if (!paths || paths.length === 0) return;

    // 严密检查：所选条目中是否包含至少 1 个有效文件 (空文件夹无法被分享)
    let totalContainedFiles = 0;
    for (const p of paths) {
      const node = filesData.find(f => f.virtual_path === p);
      if (node) {
        if (!node.is_dir) {
          totalContainedFiles++;
        } else {
          const prefix = node.virtual_path + "/";
          const subCount = filesData.filter(f => !f.is_dir && f.virtual_path.startsWith(prefix)).length;
          totalContainedFiles += subCount;
        }
      }
    }

    if (totalContainedFiles === 0) {
      showToast("⚠️ 所选文件夹为空 (包含 0 个文件)，暂不支持分享纯空目录！", true);
      return;
    }

    pendingSharePaths = paths;
    if (!shareModal) return;
    if (shareFilesDesc) {
      shareFilesDesc.innerText = `已选择 ${paths.length} 项 (共计 ${totalContainedFiles} 个文件)，将通过独立信封加密推送至对方邮箱：\n${paths.map(p => p.split("/").pop()).join(", ")}`;
    }
    if (shareTargetEmail) shareTargetEmail.value = "";
    shareModal.classList.remove("hidden");

    // 检查并自动唤起扫码状态
    await checkWebmailStatus();
    if (!isWebmailValidSession) {
      loadWebmailQRCode();
    }
  }

  if (btnCancelShare && shareModal) {
    btnCancelShare.onclick = () => {
      stopWebmailQRPoll();
      shareModal.classList.add("hidden");
    };
  }

  if (shareTargetEmail && btnConfirmShare) {
    shareTargetEmail.addEventListener("keydown", (e) => {
      if (e.key === "Enter") {
        e.preventDefault();
        btnConfirmShare.click();
      }
    });
  }

  if (btnConfirmShare && shareModal) {
    btnConfirmShare.onclick = async () => {
      let email = shareTargetEmail ? shareTargetEmail.value.trim() : "";
      if (!email) {
        showToast("请输入对方 QQ 邮箱账号！", true);
        return;
      }
      if (!email.includes("@")) {
        email += "@qq.com";
      }
      if (!isWebmailValidSession) {
        showToast("⚠️ 【云端免流秒转】尚未授权！请先在弹窗上方通过手机 QQ 扫码授权就绪后再发送！", true);
        return;
      }

      btnConfirmShare.disabled = true;
      btnConfirmShare.innerText = "⏳ 正在排队发信...";

      const sendMode = "cloud_fast";

      try {
        const res = await fetch("/api/share/create", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ paths: pendingSharePaths, to_email: email, mode: sendMode })
        });
        const data = await res.json();
        if (data.status === "ok") {
          stopWebmailQRPoll();
          if (data.mode === "cloud_fast") {
            showToast(`⚡ 已启动向 ${email} 的【云端免流秒转发】任务，本地 0 流量消耗！可在左侧队列查看进度！`);
          } else {
            showToast(`🚀 已启动向 ${email} 的标准分享任务，可在左侧队列查看实时进度！`);
          }
          shareModal.classList.add("hidden");
          renderSideTransferList();
        } else {
          showToast("❌ 发起分享失败: " + (data.message || ""), true);
        }
      } catch (err) {
        showToast("❌ 发送请求异常: " + err.message, true);
      } finally {
        btnConfirmShare.disabled = false;
        btnConfirmShare.innerText = "🚀 立即发送分享";
      }
    };
  }
  // 绑定右键菜单 ctxShare
  const ctxShare = document.getElementById("ctxShare");
  if (ctxShare) {
    ctxShare.onclick = (e) => {
      e.stopPropagation();
      closeContextMenu();
      if (selectedPaths.size > 1) {
        openShareModal(Array.from(selectedPaths));
      } else if (contextMenuTarget) {
        openShareModal([contextMenuTarget.virtual_path]);
      }
    };
  }

  // 绑定批量操作栏 btnBatchShare
  const btnBatchShare = document.getElementById("btnBatchShare");
  if (btnBatchShare) {
    btnBatchShare.onclick = () => {
      if (selectedPaths.size > 0) {
        openShareModal(Array.from(selectedPaths));
      }
    };
  }


  document.getElementById("btnRefresh").onclick = async () => {
    const btn = document.getElementById("btnRefresh");
    const originText = btn.innerText;
    btn.innerText = "⏳ 刷新中...";
    btn.disabled = true;

    showCloudLoading(true);
    await loadStatus();
    await loadFiles();
    showCloudLoading(false);

    setTimeout(() => {
      btn.innerText = originText;
      btn.disabled = false;
      showToast("✔ 已同步最新云端文件列表");
    }, 300);
  };

  // 新建文件夹网页原生弹窗
  document.getElementById("btnNewFolder").onclick = () => {
    if (!isConfiguredState) {
      showToast("请先完成设置并连入 QQ 邮箱！", true);
      return;
    }
    const modal = document.getElementById("newFolderModal");
    const input = document.getElementById("newFolderNameInput");
    input.value = "";
    modal.classList.remove("hidden");
    setTimeout(() => input.focus(), 100);
  };

  document.getElementById("btnCancelNewFolder").onclick = () => {
    document.getElementById("newFolderModal").classList.add("hidden");
  };

  let isCreatingFolder = false;
  const submitNewFolder = async () => {
    if (isCreatingFolder) return;
    const input = document.getElementById("newFolderNameInput");
    let name = input.value.trim().replace(/^[\\\/]+|[\\\/]+$/g, "");
    name = name.replace(/\\/g, "/");
    if (!name) return;
    isCreatingFolder = true;
    const btnConfirm = document.getElementById("btnConfirmNewFolder");
    if (btnConfirm) { btnConfirm.disabled = true; btnConfirm.innerText = "⏳ 创建中..."; }

    const vPath = (currentPath === "/" ? "" : currentPath) + "/" + name;
    const fd = new FormData();
    fd.append("path", vPath);
    const mkRes = await fetch("/api/fs/mkdir", { method: "POST", body: fd });

    document.getElementById("newFolderModal").classList.add("hidden");
    expandedFolders.add(currentPath);
    if (mkRes.ok) {
      logWebTransfer(`✔ [新建文件夹] 成功创建文件夹: ${vPath}`);
      showToast(`✔ 文件夹 "${name}" 创建成功`);
    } else {
      const txt = await mkRes.text();
      logWebTransfer(`❌ [新建文件夹失败] 创建文件夹失败: ${txt || mkRes.status}`);
      showToast(`❌ 创建文件夹失败: ${txt || mkRes.status}`, true);
    }
    await loadFiles();
    isCreatingFolder = false;
    if (btnConfirm) { btnConfirm.disabled = false; btnConfirm.innerText = "立即创建"; }
  };

  document.getElementById("btnConfirmNewFolder").onclick = submitNewFolder;
  document.getElementById("newFolderNameInput").onkeydown = (e) => {
    if (e.key === "Enter") submitNewFolder();
  };
}

async function triggerHealthCheck() {
  const modal = document.getElementById("healthModal");
  const resBox = document.getElementById("healthResults");
  const btnApply = document.getElementById("btnApplyRepair");
  btnApply.classList.add("hidden");
  resBox.innerHTML = `
    <div class="health-loading-wrap">
      <div class="health-spinner"></div>
      <div class="health-loading-text">🔍 正在以只读模式扫描云端邮件报头...</div>
      <div class="health-loading-sub">正在核对当前隔离空间数据一致性，请稍候</div>
    </div>
  `;
  modal.classList.remove("hidden");

  try {
    const res = await fetch("/api/health/check");
    const data = await res.json();

    if (data.error) {
      resBox.innerHTML = `<p style="color: #ef4444;">${data.error}</p>`;
      btnApply.classList.add("hidden");
      return;
    }

    let html = `<p><strong>已审查邮件总数:</strong> ${data.total_mails_scanned} 封</p>`;
    html += `<p><strong>主索引更新次数:</strong>  ${data.master_index_ver}</p><hr style="margin: 10px 0; border: 0; border-top: 1px solid #e2e8f0;">`;

    let hasIssue = false;
    if (data.orphan_files && data.orphan_files.length > 0) {
      hasIssue = true;
      html += `<p style="color: #2563eb;"><strong>发现未入库的完好文件 (${data.orphan_files.length} 个):</strong></p><ul>`;
      data.orphan_files.forEach(o => html += `<li>${o.name} (分片齐备，可一键补录)</li>`);
      html += `</ul>`;
    }

    if (data.ghost_entries && data.ghost_entries.length > 0) {
      hasIssue = true;
      html += `<p style="color: #ef4444; margin-top: 8px;"><strong>发现失效死条目 (${data.ghost_entries.length} 个):</strong></p><ul>`;
      data.ghost_entries.forEach(g => html += `<li>${g} (云端邮件已丢失，需清理)</li>`);
      html += `</ul>`;
    }

    if (data.orphan_chunks_count && data.orphan_chunks_count > 0) {
      hasIssue = true;
      html += `<p style="color: #ef4444; margin-top: 8px;"><strong>发现云端无主残留切片邮件 (${data.orphan_chunks_count} 封):</strong></p><p style="font-size: 12px; color: #64748b;">这些邮件对应的文件已被删除，但切片邮件仍滞留占用邮箱空间，点击下方按钮可一键彻底物理抹除 (EXPUNGE)！</p>`;
    }

    if (!hasIssue) {
      html += `<p style="color: #10b981; font-weight: 600; margin-top: 10px;">✔ 数据极其健康！主索引与云端所有邮件 100% 吻合，无需任何修复。</p>`;
      btnApply.classList.add("hidden");
    } else {
      btnApply.classList.remove("hidden");
    }

    resBox.innerHTML = html;

    btnApply.disabled = false;
    btnApply.innerText = "🛠️ 立即执行云端修复与重构";

    btnApply.onclick = async () => {
      btnApply.disabled = true;
      btnApply.innerText = "⏳ 正在重构修复并同步云端...";
      showToast("开始执行云端数据重构与修复...");
      try {
        const res = await fetch("/api/health/repair", { method: "POST", body: new URLSearchParams({ action: "repair_all" }) });
        const data = await res.json();
        if (data.status === "ok") {
          const purged = data.orphan_chunks_purged || 0;
          showToast(`✔ 云端数据重构成功！补录 ${data.repaired_count || 0} 文件，清理 ${data.ghost_count || 0} 死条目，彻底抹除 ${purged} 封残留邮件！`);
          modal.classList.add("hidden");
          await loadFiles(currentPath);
          await loadStatus();
        } else {
          showToast("❌ 重构失败: " + (data.message || ""), true);
        }
      } catch (e) {
        showToast("❌ 请求异常: " + e.message, true);
      } finally {
        btnApply.disabled = false;
        btnApply.innerText = "🛠️ 立即执行云端修复与重构";
      }
    };
  } catch (err) {
    resBox.innerHTML = `<p style="color: red;">对账扫描失败: ${err.message}</p>`;
  }

  document.getElementById("btnCloseHealth").onclick = () => modal.classList.add("hidden");
}

function formatBytes(bytes) {
  if (bytes === 0) return '0 B';
  const k = 1024;
  const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
}

function formatDateTime(dateStr) {
  if (!dateStr) return "-";
  try {
    const d = new Date(dateStr);
    if (isNaN(d.getTime())) return "-";
    const pad = (n) => (n < 10 ? "0" + n : n);
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  } catch (e) {
    return "-";
  }
}
