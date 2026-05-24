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

const (
	portStatusStopped = iota
	portStatusListening
	portStatusRetry
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
	setPortStatus(p, portStatusRetry, failCount+1, time.Now().Add(backoff))
}

func retryBackoff(failCount int) time.Duration {
	if failCount >= 6 {
		return 60 * time.Second
	}
	return time.Duration(1<<failCount) * time.Second
}

// Start 主循环：首次立即并发启动所有端口，后续定时重试失败端口（指数退避）
func Start(ctx context.Context, pm map[int]*PortInfo, globalDelay int, maxConn int) {
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
				if status == portStatusRetry && now.After(nextRetry) {
					retryPorts = append(retryPorts, p)
				}
			}
			for _, p := range retryPorts {
				_, failCount, nextRetry := portStatusSnapshot(p)
				setPortStatus(p, portStatusStopped, failCount, nextRetry)
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
	setPortStatus(port, portStatusListening, 0, time.Time{})

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

func logRejectedConnections(pm map[int]*PortInfo) {
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

	state := connState{}
	responseDelay := responseDelayFor(port, globalDelay)
	debugLog := slog.Default().Enabled(ctx, slog.LevelDebug)

	if debugLog {
		slog.Debug("客户端已连接", "addr", port.Addr, "remote", conn.RemoteAddr().String())
	}

	buf := make([]byte, readBufferSize)
	pending := make([]byte, 0, initialPendingCapacity(port.MaxCommandLen))
	for {
		n, err := conn.Read(buf)
		if err != nil {
			if debugLog {
				if errors.Is(err, io.EOF) {
					slog.Debug("客户端正常断开", "remote", conn.RemoteAddr().String())
				} else if errors.Is(err, net.ErrClosed) {
					// 连接已关闭
				} else {
					slog.Debug("读取错误", "remote", conn.RemoteAddr().String(), "error", err)
				}
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
		pending, ok = processBufferedCommands(ctx, conn, port, responseDelay, pending, &state, debugLog)
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

type connState struct {
	responseIndex map[*CommandInfo]int
}

func (s *connState) nextResponse(cmd *CommandInfo) []byte {
	if len(cmd.Responses) == 1 {
		return cmd.Responses[0]
	}
	if s.responseIndex == nil {
		s.responseIndex = make(map[*CommandInfo]int)
	}
	idx := s.responseIndex[cmd]
	s.responseIndex[cmd] = (idx + 1) % len(cmd.Responses)
	return cmd.Responses[idx]
}

func processBufferedCommands(ctx context.Context, conn net.Conn, port *PortInfo, responseDelay time.Duration, pending []byte, state *connState, debugLog bool) ([]byte, bool) {
	for consumed := 0; consumed < len(pending); {
		input := pending[consumed:]
		cmd, cmdLen, prefix := matchCommand(port, input)
		if cmd != nil {
			response := state.nextResponse(cmd)

			if !waitResponseDelay(ctx, responseDelay) {
				return input, false
			}
			if err := writeAll(conn, response); err != nil {
				if debugLog {
					slog.Debug("写入失败", "addr", port.Addr, "cmd", cmd.HexKey, "error", err)
				}
				return input, false
			}
			consumed += cmdLen
			continue
		}

		if prefix {
			if consumed == 0 {
				return pending, true
			}
			copy(pending, input)
			return pending[:len(input)], true
		}

		if debugLog {
			slog.Debug("未匹配命令字节，丢弃并继续同步", "addr", port.Addr, "byte", BytesToHex(input[:1]))
		}
		consumed++
	}
	return pending[:0], true
}

func matchCommand(port *PortInfo, pending []byte) (*CommandInfo, int, bool) {
	if len(pending) == 0 {
		return nil, 0, true
	}

	node := port.commandRoot
	if node == nil {
		return nil, 0, false
	}

	maxLen := len(pending)
	if port.MaxCommandLen > 0 && maxLen > port.MaxCommandLen {
		maxLen = port.MaxCommandLen
	}

	var (
		match    *CommandInfo
		matchLen int
	)
	for i := 0; i < maxLen; i++ {
		if len(node.children) == 0 {
			return match, matchLen, false
		}
		next := node.children[pending[i]]
		if next == nil {
			return match, matchLen, false
		}
		node = next
		if node.command != nil && len(node.command.Responses) > 0 {
			match = node.command
			matchLen = i + 1
		}
	}
	return match, matchLen, len(pending) <= port.MaxCommandLen
}

func responseDelayFor(port *PortInfo, globalDelay int) time.Duration {
	d := globalDelay
	if port.Delay > 0 {
		d = port.Delay
	}
	if d <= 0 {
		return 0
	}
	return time.Duration(d) * time.Millisecond
}

func initialPendingCapacity(maxCommandLen int) int {
	if maxCommandLen <= 0 || maxCommandLen > readBufferSize {
		return readBufferSize
	}
	return maxCommandLen
}

func waitResponseDelay(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}

	timer := time.NewTimer(d)
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
	if cap(pending) <= maxPendingBytes {
		return pending
	}
	if len(pending) == 0 {
		return make([]byte, 0, readBufferSize)
	}
	newCap := max(len(pending)*2, len(pending)+readBufferSize)
	newCap = min(newCap, maxPendingBytes)
	compact := make([]byte, len(pending), newCap)
	copy(compact, pending)
	return compact
}
