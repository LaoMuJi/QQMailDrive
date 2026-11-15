package main

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

func main() {
	printBanner()

	// 1. 自检并清理本地 temp 临时目录
	cleanTempDir()

	// 2. 加载本地配置文件（若丢失则自动静默重建）
	if err := LoadConfig(); err != nil {
		fmt.Printf("[提示] 配置文件初始化: %v\n", err)
	}

	// 3. 初始化 IMAP 客户端
	imapClient := NewIMAPClient(GlobalConfig.IMAPServer, GlobalConfig.Email, GlobalConfig.IMAPAuthCode)
	GlobalEngine = NewStorageEngine(imapClient)

	// 判断是否已完成配置
	if !GlobalConfig.IsConfigured() {
		fmt.Println("----------------------------------------------------------")
		fmt.Println("[提示] 首次启动 QQMailDrive，尚未配置邮箱与授权码。")
		fmt.Println("请在稍后自动弹出的网页中完成首次初始化设置。")
		fmt.Println("----------------------------------------------------------")
		// 首次使用未配置时，绝不标记为异常退出
		SetCleanExitStatus(true)
	} else {
		// 标记开始运行
		fmt.Printf("[1/3] 正在连接 QQ 邮箱 IMAP 服务器 (%s)...\n", GlobalConfig.IMAPServer)
		if err := imapClient.Connect(); err != nil {
			fmt.Printf("! 连接邮箱失败: %v\n", err)
			fmt.Println("[提示] 可在稍后弹出的 Web 页面中检查授权码是否正确。")
		} else {
			fmt.Printf("✔ 成功连入 QQ 邮箱！账号: %s\n", GlobalConfig.Email)

			// 优先从系统专属「收到的文件」邮件检索并合并云端漫游配置
			fmt.Println("[云端配置] 正在检索云端设置备份 [QMD:CONFIG]...")
			if cloudCfg, err := GlobalEngine.LoadCloudConfig(); err == nil && cloudCfg != nil {
				ApplyCloudConfig(cloudCfg)
				fmt.Printf("✔ [云端配置同步] 成功同步云端设置！(分片: %d 字节, 下载并发: %d, 防风控延时: %dms, 识别码: %s)\n",
					GlobalConfig.ChunkSizeBytes, GlobalConfig.ChunkDownloadConcurrency, GlobalConfig.RequestDelayMS, GetComputerID())
				if GlobalConfig.DownloadDir != "" {
					fmt.Printf("  └─ 本机专属下载目录已就绪: %s\n", GlobalConfig.DownloadDir)
				}
			} else {
				fmt.Printf("ℹ [云端配置同步] 未在云端检测到备份或首次使用，将在系统就绪后自动建立首份云端配置备份...\n")
				go func() {
					time.Sleep(1500 * time.Millisecond)
					configLock.RLock()
					cfgCopy := GlobalConfig
					configLock.RUnlock()
					_ = GlobalEngine.SaveCloudConfig(cfgCopy)
				}()
			}

			fmt.Printf("[2/3] 检查并选择隔离存储文件夹: [%s]...\n", GlobalConfig.StorageFolder)
			if err := imapClient.EnsureAndSelectFolder(GlobalConfig.StorageFolder); err != nil {
				fmt.Printf("! 选择存储文件夹警告: %v\n", err)
			} else {
				fmt.Println("✔ 存储文件夹已就绪。")
				// 自动初始化或同步主索引 (内部已同步立即拉起云端会话恢复与预热)
				_, _ = GlobalEngine.InitOrLoadCloudIndex()

				// 首次启动强行对账与自动修复：以云端为唯一权威，补录所有完好分片
				fmt.Println("[首次强行对账] 正在核验云端存储一致性...")
				checker := NewConsistencyChecker(GlobalEngine, imapClient)
				if report, err := checker.RunHealthCheck(); err == nil && report != nil {
					if len(report.OrphanFiles) > 0 {
						fmt.Printf("[首次强行对账] 发现 %d 项需补录或修复的文件，自动入库...\n", len(report.OrphanFiles))
						_ = checker.RepairOrphans(report.OrphanFiles)
					}
					fmt.Printf("✔ 对账完成，当前网盘共纳管 %d 个文件。\n", len(GlobalEngine.masterIndex.Files))
				}
				SetCleanExitStatus(true)
			}
		}
	}

	// 4. 启动本地 Web 服务器
	server := NewWebServer(DefaultPort, GlobalEngine)
	go func() {
		if err := server.Start(); err != nil {
			fmt.Printf("Web 服务启动异常: %v\n", err)
		}
	}()

	// 5. 自动唤醒默认浏览器打开控制面板
	url := fmt.Sprintf("http://127.0.0.1:%d", DefaultPort)
	fmt.Printf("[3/3] 正在启动控制面板并在浏览器中打开: %s\n", url)
	go func() {
		time.Sleep(800 * time.Millisecond)
		openBrowser(url)
	}()

	// 6. 优雅退出信号捕获
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	fmt.Println("\n正在安全退出 QQMailDrive...")
	if GlobalEngine != nil {
		_ = GlobalEngine.FlushMasterIndexSync()
	}
	SetCleanExitStatus(true)
	cleanTempDir()
	imapClient.Close()
	fmt.Println("✔ 已安全断开连接，临时缓存已清除，感谢使用！")
}

func printBanner() {
	fmt.Println("==========================================================")
	fmt.Printf("   %s %s\n", AppName, AppVersion)
	fmt.Printf("   作者: %s  |  发布时间: %s\n", AppAuthor, AppBuildDate)
	fmt.Println("   基于 QQ 邮箱老用户空间的个人强加密云存储系统")
	fmt.Println("==========================================================")
}

func cleanTempDir() {
	tempDir := "temp"
	_ = os.MkdirAll(tempDir, 0755)
	entries, err := os.ReadDir(tempDir)
	if err == nil {
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(tempDir, e.Name()))
		}
	}
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}
