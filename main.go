package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	_ "modernc.org/sqlite"
)

const defaultListenHost = "0.0.0.0"

func setupLogging(debug bool) {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: level,
	})))
}

func main() {
	// 1. 命令行参数
	dbPath := flag.String("db", "simulator.db", "SQLite 数据库文件路径")
	debug := flag.Bool("debug", false, "启用调试日志")
	delay := flag.Int("delay", 50, "全局发送延迟(ms)")
	maxConn := flag.Int("max-conn", 5, "每个端口最大并发连接数（0为不限制）")
	ip := flag.String("ip", "", "手动指定本机IP并仅监听该IP（为空则监听0.0.0.0）")
	help := flag.Bool("h", false, "显示帮助信息")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "林霖动环数据模拟器 - TCP 设备数据模拟服务\n\n")
		fmt.Fprintf(os.Stderr, "用法:\n  lin_simulator [选项]\n\n")
		fmt.Fprintf(os.Stderr, "选项:\n")
		flag.PrintDefaults()
		fmt.Fprintf(os.Stderr, "\n示例:\n")
		fmt.Fprintf(os.Stderr, "  lin_simulator\n")
		fmt.Fprintf(os.Stderr, "  lin_simulator -delay 100 -debug\n")
		fmt.Fprintf(os.Stderr, "  lin_simulator -max-conn 20\n")
		fmt.Fprintf(os.Stderr, "  lin_simulator -ip 192.168.1.100 -db /data/simulator.db\n")
	}
	flag.Parse()

	if *help {
		flag.Usage()
		os.Exit(0)
	}
	setupLogging(*debug)

	if *delay < 0 {
		slog.Error("delay 不能为负数", "delay", *delay)
		os.Exit(1)
	}
	if *maxConn < 0 {
		slog.Error("max-conn 不能为负数", "max_conn", *maxConn)
		os.Exit(1)
	}

	// 2. 确定监听地址和日志展示 IP
	bindHost := defaultListenHost
	localIP := *ip
	if localIP == "" {
		var err error
		localIP, err = getLocalIP()
		if err != nil {
			localIP = "unknown"
			slog.Warn("自动获取本机IP失败，继续启动", "error", err, "bind", bindHost)
		}
	} else {
		var err error
		bindHost, err = validateLocalBindIP(localIP)
		if err != nil {
			slog.Error("指定的本机IP无效", "ip", localIP, "error", err)
			os.Exit(1)
		}
		localIP = bindHost
	}

	// 3. 初始化完成，记录启动参数
	slog.Info("本机IP", "ip", localIP, "bind", bindHost)

	// 4. 校验 SQLite 文件存在
	if _, err := os.Stat(*dbPath); os.IsNotExist(err) {
		slog.Error("数据库文件不存在", "path", *dbPath)
		os.Exit(1)
	}

	// 5. 打开 SQLite
	db, err := sql.Open("sqlite", *dbPath)
	if err != nil {
		slog.Error("数据库打开失败", "error", err)
		os.Exit(1)
	}

	// 6. 加载 PortMap
	pm, err := LoadPortMap(db, bindHost)
	if err != nil {
		closeDatabase(db)
		slog.Error("数据加载失败", "error", err)
		os.Exit(1)
	}
	if len(pm) == 0 {
		summaries, summaryErr := LoadPortSummaries(db)
		closeDatabase(db)
		if summaryErr != nil {
			slog.Error("没有可模拟的端口命令", "ip", localIP, "available_port_error", summaryErr)
		} else {
			slog.Error("没有可模拟的端口命令", "ip", localIP, "available_ports", summaries)
		}
		os.Exit(1)
	}
	closeDatabase(db)
	logMergedPortRecords(pm)

	// 7. 优雅关闭
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	slog.Info("设备模拟器启动", "ports", len(pm), "ip", localIP, "bind", bindHost, "max_conn_per_port", *maxConn)
	Start(ctx, pm, *delay, *maxConn)
	slog.Info("设备模拟器已退出")
}

func closeDatabase(db *sql.DB) {
	if err := db.Close(); err != nil {
		slog.Warn("数据库关闭失败", "error", err)
	}
}

func logMergedPortRecords(pm map[int]*PortInfo) {
	for _, p := range pm {
		if len(p.MergedRecords) == 0 {
			continue
		}
		slog.Warn(
			"检测到重复端口配置，已合并到首条配置",
			"addr", p.Addr,
			"selected_port_id", p.PortID,
			"selected_delay", p.Delay,
			"merged_records", p.MergedRecords,
		)
	}
}
