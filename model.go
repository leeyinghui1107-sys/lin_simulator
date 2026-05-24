package main

import (
	"sync"
	"sync/atomic"
	"time"
)

// PortInfo 端口配置及命令-返回值映射
type PortInfo struct {
	PortID         int64
	Addr           string // "ip:port"
	Delay          int    // 端口级延迟(ms)；0 时使用全局延迟
	statusMu       sync.RWMutex
	Status         int // 0=未开启, 1=正常, 2=错误
	failCount      int
	nextRetry      time.Time
	activeConns    atomic.Int64
	rejectedConns  atomic.Uint64
	Commands       map[string]*CommandInfo // 原始命令字节字符串 → 返回值列表
	CommandPrefix  map[string]struct{}     // 原始命令字节前缀，用于 TCP 流重同步
	CommandLengths []int
	MaxCommandLen  int
	MergedRecords  []MergedPortRecord
}

type CommandInfo struct {
	HexKey    string
	Responses [][]byte
}

type MergedPortRecord struct {
	PortID int64
	Delay  int
}

func (p *PortInfo) addCommandResponse(command []byte, hexKey string, response []byte) {
	if p.Commands == nil {
		p.Commands = make(map[string]*CommandInfo)
	}
	if p.CommandPrefix == nil {
		p.CommandPrefix = make(map[string]struct{})
	}

	key := string(command)
	cmd, ok := p.Commands[key]
	if !ok {
		cmd = &CommandInfo{HexKey: hexKey}
		p.Commands[key] = cmd
		p.addCommandLength(len(command))
		if len(command) > p.MaxCommandLen {
			p.MaxCommandLen = len(command)
		}
		for i := 1; i < len(command); i++ {
			p.CommandPrefix[string(command[:i])] = struct{}{}
		}
	}
	cmd.Responses = append(cmd.Responses, response)
}

func (p *PortInfo) addCommandLength(length int) {
	for _, existing := range p.CommandLengths {
		if existing == length {
			return
		}
	}
	p.CommandLengths = append(p.CommandLengths, length)
}
