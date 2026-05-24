package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"time"
)

const (
	connReadIdleTimeout = 5 * time.Minute
	connWriteTimeout    = 10 * time.Second
	readBufferSize      = 1024
	maxCommandBytes     = 64 * 1024
	maxPendingBytes     = maxCommandBytes
)

// setPortStatus 更新端口状态及重试信息。
func setPortStatus(p *PortInfo, status int, failCount int, nextRetry time.Time) {
	p.statusMu.Lock()
	p.Status = status
	p.failCount = failCount
	p.nextRetry = nextRetry
	p.statusMu.Unlock()
}

func portStatusSnapshot(p *PortInfo) (int, int, time.Time) {
	p.statusMu.RLock()
	defer p.statusMu.RUnlock()
	return p.Status, p.failCount, p.nextRetry
}

func setPortRetry(p *PortInfo) {
	_, failCount, _ := portStatusSnapshot(p)
	backoff := retryBackoff(failCount)
	setPortStatus(p, 2, failCount+1, time.Now().Add(backoff))
}

func retryBackoff(failCount int) time.Duration {
	if failCount >= 6 {
		return 60 * time.Second
	}
	return time.Duration(1<<failCount) * time.Second
}

// Start 主循环：首次立即并发启动所有端口，后续定时重试失败端口（指数退避）
func Start(ctx context.Context, pm map[int64]*PortInfo, globalDelay int, maxConn int) {
	var serverWG sync.WaitGroup
	startServer := func(p *PortInfo) {
		select {
		case <-ctx.Done():
			return
		default:
		}
		serverWG.Add(1)
		go func() {
			defer serverWG.Done()
			BuildServer(ctx, p, globalDelay, maxConn)
		}()
	}

	for _, p := range pm {
		startServer(p)
	}

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	rejectionTicker := time.NewTicker(time.Minute)
	defer rejectionTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			logRejectedConnections(pm)
			serverWG.Wait()
			return
		case <-ticker.C:
			now := time.Now()
			var retryPorts []*PortInfo
			for _, p := range pm {
				status, _, nextRetry := portStatusSnapshot(p)
				if status == 2 && now.After(nextRetry) {
					retryPorts = append(retryPorts, p)
				}
			}
			for _, p := range retryPorts {
				_, failCount, nextRetry := portStatusSnapshot(p)
				setPortStatus(p, 0, failCount, nextRetry)
				startServer(p)
			}
		case <-rejectionTicker.C:
			logRejectedConnections(pm)
		}
	}
}

// BuildServer 在指定端口上开启 TCP 监听
func BuildServer(ctx context.Context, port *PortInfo, globalDelay int, maxConn int) {
	listener, err := net.Listen("tcp", port.Addr)
	if err != nil {
		slog.Error("端口监听失败", "addr", port.Addr, "error", err)
		setPortRetry(port)
		return
	}
	defer listener.Close()
	setPortStatus(port, 1, 0, time.Time{})

	slog.Info("端口监听已启动", "addr", port.Addr)

	stopListenerClose := context.AfterFunc(ctx, func() {
		listener.Close()
	})
	defer stopListenerClose()

	var connWG sync.WaitGroup
	defer func() {
		connWG.Wait()
	}()

	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			slog.Error("端口监听异常，将等待重试", "addr", port.Addr, "error", err)
			setPortRetry(port)
			return
		}
		if !tryAcquireConn(port, maxConn) {
			port.rejectedConns.Add(1)
			conn.Close()
			continue
		}
		connWG.Add(1)
		go func() {
			defer connWG.Done()
			defer releaseConn(port)
			handleConn(ctx, conn, port, globalDelay)
		}()
	}
}

func tryAcquireConn(port *PortInfo, maxConn int) bool {
	if maxConn == 0 {
		port.activeConns.Add(1)
		return true
	}
	for {
		active := port.activeConns.Load()
		if active >= int64(maxConn) {
			return false
		}
		if port.activeConns.CompareAndSwap(active, active+1) {
			return true
		}
	}
}

func releaseConn(port *PortInfo) {
	port.activeConns.Add(-1)
}

func logRejectedConnections(pm map[int64]*PortInfo) {
	for _, p := range pm {
		count := p.rejectedConns.Swap(0)
		if count > 0 {
			slog.Warn("端口连接数达到上限，已拒绝新连接", "addr", p.Addr, "rejected", count, "active", p.activeConns.Load())
		}
	}
}

// handleConn 处理单个 TCP 连接
func handleConn(ctx context.Context, conn net.Conn, port *PortInfo, globalDelay int) {
	defer conn.Close()

	stopConnClose := context.AfterFunc(ctx, func() {
		conn.Close()
	})
	defer stopConnClose()

	// 读超时防止空闲连接永久占用 goroutine。
	if err := conn.SetReadDeadline(time.Now().Add(connReadIdleTimeout)); err != nil {
		slog.Debug("设置读取超时失败", "addr", port.Addr, "error", err)
		return
	}

	// 每连接独立的轮转索引：值为"下次使用的索引"。
	cmdIdx := make(map[string]int, len(port.Commands))

	slog.Info("客户端已连接", "addr", port.Addr, "remote", conn.RemoteAddr().String())

	buf := make([]byte, readBufferSize)
	pending := make([]byte, 0, port.MaxCommandLen)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				slog.Debug("客户端正常断开", "remote", conn.RemoteAddr().String())
			} else if errors.Is(err, net.ErrClosed) {
				// 连接已关闭
			} else {
				slog.Debug("读取错误", "remote", conn.RemoteAddr().String(), "error", err)
			}
			return
		}

		// 每次成功读取后重置超时
		if err := conn.SetReadDeadline(time.Now().Add(connReadIdleTimeout)); err != nil {
			slog.Debug("重置读取超时失败", "addr", port.Addr, "remote", conn.RemoteAddr().String(), "error", err)
			return
		}

		pending = append(pending, buf[:n]...)
		var ok bool
		pending, ok = processBufferedCommands(ctx, conn, port, globalDelay, pending, cmdIdx)
		if !ok {
			return
		}
		if len(pending) > maxPendingBytes {
			slog.Warn("连接缓冲区超过上限，关闭连接", "addr", port.Addr, "remote", conn.RemoteAddr().String(), "pending", len(pending), "limit", maxPendingBytes)
			return
		}
		pending = compactPending(pending)
	}
}

func processBufferedCommands(ctx context.Context, conn net.Conn, port *PortInfo, globalDelay int, pending []byte, cmdIdx map[string]int) ([]byte, bool) {
	for len(pending) > 0 {
		cmd, cmdLen, cmdKey := longestCommandMatch(port, pending)
		if cmd != nil {
			idx := cmdIdx[cmdKey]
			response := cmd.Responses[idx]
			cmdIdx[cmdKey] = (idx + 1) % len(cmd.Responses)

			if !waitResponseDelay(ctx, port, globalDelay) {
				return pending, false
			}
			if err := writeAll(conn, response); err != nil {
				slog.Debug("写入失败", "addr", port.Addr, "cmd", cmd.HexKey, "error", err)
				return pending, false
			}
			pending = pending[cmdLen:]
			continue
		}

		if isCommandPrefix(port, pending) {
			return pending, true
		}

		slog.Debug("未匹配命令字节，丢弃并继续同步", "addr", port.Addr, "byte", BytesToHex(pending[:1]))
		pending = pending[1:]
	}
	return pending, true
}

func longestCommandMatch(port *PortInfo, pending []byte) (*CommandInfo, int, string) {
	maxLen := len(pending)
	if port.MaxCommandLen > 0 && maxLen > port.MaxCommandLen {
		maxLen = port.MaxCommandLen
	}

	var (
		match    *CommandInfo
		matchLen int
		matchKey string
	)
	for _, length := range port.CommandLengths {
		if length > maxLen {
			continue
		}
		key := string(pending[:length])
		cmd, ok := port.Commands[key]
		if !ok || len(cmd.Responses) == 0 {
			continue
		}
		if length < matchLen {
			continue
		}
		match = cmd
		matchLen = length
		matchKey = key
	}
	return match, matchLen, matchKey
}

func isCommandPrefix(port *PortInfo, pending []byte) bool {
	if len(pending) == 0 {
		return true
	}
	if port.MaxCommandLen > 0 && len(pending) > port.MaxCommandLen {
		return false
	}
	key := string(pending)
	if _, ok := port.Commands[key]; ok {
		return true
	}
	_, ok := port.CommandPrefix[key]
	return ok
}

func waitResponseDelay(ctx context.Context, port *PortInfo, globalDelay int) bool {
	d := globalDelay
	if port.Delay > 0 {
		d = port.Delay
	}
	if d <= 0 {
		return true
	}

	timer := time.NewTimer(time.Duration(d) * time.Millisecond)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func writeAll(conn net.Conn, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if err := conn.SetWriteDeadline(time.Now().Add(connWriteTimeout)); err != nil {
		return err
	}
	for len(data) > 0 {
		n, err := conn.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func compactPending(pending []byte) []byte {
	if len(pending) == 0 {
		return nil
	}
	if cap(pending) <= maxPendingBytes {
		return pending
	}
	newCap := max(len(pending)*2, len(pending)+readBufferSize)
	newCap = min(newCap, maxPendingBytes)
	compact := make([]byte, len(pending), newCap)
	copy(compact, pending)
	return compact
}
