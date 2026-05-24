package main

import (
	"sync"
	"sync/atomic"
	"time"
)

// PortInfo 端口配置及命令-返回值映射
type PortInfo struct {
	PortID        int64
	Addr          string // "ip:port"
	Delay         int    // 端口级延迟(ms)；0 时使用全局延迟
	statusMu      sync.RWMutex
	Status        int
	failCount     int
	nextRetry     time.Time
	activeConns   atomic.Int64
	rejectedConns atomic.Uint64
	commandRoot   *commandNode
	CommandCount  int
	MaxCommandLen int
	MergedRecords []MergedPortRecord
}

type CommandInfo struct {
	HexKey    string
	Responses [][]byte
}

type commandNode struct {
	children map[byte]*commandNode
	command  *CommandInfo
}

type MergedPortRecord struct {
	PortID int64
	Delay  int
}

func (p *PortInfo) addCommandResponse(command []byte, hexKey string, response []byte) {
	if p.commandRoot == nil {
		p.commandRoot = &commandNode{}
	}

	node := p.commandRoot
	for _, b := range command {
		if node.children == nil {
			node.children = make(map[byte]*commandNode)
		}
		next := node.children[b]
		if next == nil {
			next = &commandNode{}
			node.children[b] = next
		}
		node = next
	}

	cmd := node.command
	if cmd == nil {
		cmd = &CommandInfo{HexKey: hexKey}
		node.command = cmd
		p.CommandCount++
		if len(command) > p.MaxCommandLen {
			p.MaxCommandLen = len(command)
		}
	}
	cmd.Responses = append(cmd.Responses, response)
}
