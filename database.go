package main

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
)

type PortSummary struct {
	Port  int
	Items int
}

// LoadPortMap 从 SQLite 加载端口配置及命令映射到内存
func LoadPortMap(db *sql.DB, bindHost string) (map[int64]*PortInfo, error) {
	// 查询1：加载全部有效端口，监听地址统一绑定到启动参数决定的地址。
	portRows, err := db.Query(
		"SELECT port_id, port, delay FROM ports WHERE port BETWEEN 1 AND 65535 ORDER BY port, port_id",
	)
	if err != nil {
		return nil, fmt.Errorf("查询端口配置失败: %w", err)
	}
	defer portRows.Close()

	pm := make(map[int64]*PortInfo)
	for portRows.Next() {
		var portID int64
		var port int
		var delay int
		if err := portRows.Scan(&portID, &port, &delay); err != nil {
			return nil, fmt.Errorf("扫描端口行失败: %w", err)
		}
		if delay < 0 {
			return nil, fmt.Errorf("端口 %d(port_id=%d) 的 delay 不能为负数: %d", port, portID, delay)
		}
		key := int64(port)
		if existing, ok := pm[key]; ok {
			existing.MergedRecords = append(existing.MergedRecords, MergedPortRecord{PortID: portID, Delay: delay})
			continue
		}
		pm[key] = &PortInfo{
			PortID:        portID,
			Addr:          fmt.Sprintf("%s:%d", bindHost, port),
			Delay:         delay,
			Commands:      make(map[string]*CommandInfo),
			CommandPrefix: make(map[string]struct{}),
		}
	}
	if err := portRows.Err(); err != nil {
		return nil, fmt.Errorf("遍历端口行失败: %w", err)
	}

	if len(pm) == 0 {
		return pm, nil
	}

	// 查询2：按端口号加载命令；同一端口号的多条旧 IP 记录合并到一个本地监听端口。
	cmdRows, err := db.Query(
		`SELECT p.port, c.command_key, c.return_value, c.seq
		 FROM commands c
		 JOIN ports p ON c.port_id = p.port_id
		 WHERE p.port BETWEEN 1 AND 65535
		 ORDER BY p.port, c.command_key, c.port_id, c.seq`,
	)
	if err != nil {
		return nil, fmt.Errorf("查询命令映射失败: %w", err)
	}
	defer cmdRows.Close()

	for cmdRows.Next() {
		var port int
		var cmdKey, retVal string
		var seq int
		if err := cmdRows.Scan(&port, &cmdKey, &retVal, &seq); err != nil {
			return nil, fmt.Errorf("扫描命令行失败: %w", err)
		}
		if p, ok := pm[int64(port)]; ok {
			command, normalizedCmdKey, err := decodeHexField("command_key", cmdKey)
			if err != nil {
				return nil, fmt.Errorf("端口 %d 的命令编码无效: %w", port, err)
			}
			if len(command) == 0 {
				return nil, fmt.Errorf("端口 %d 的命令编码为空", port)
			}
			if len(command) > maxCommandBytes {
				return nil, fmt.Errorf("端口 %d 命令 %s 过长: %d 字节，最大 %d 字节", port, normalizedCmdKey, len(command), maxCommandBytes)
			}
			response, _, err := decodeHexField("return_value", retVal)
			if err != nil {
				return nil, fmt.Errorf("端口 %d 命令 %s 的返回值编码无效: %w", port, normalizedCmdKey, err)
			}
			p.addCommandResponse(command, normalizedCmdKey, response)
		}
	}
	if err := cmdRows.Err(); err != nil {
		return nil, fmt.Errorf("遍历命令行失败: %w", err)
	}

	return pm, nil
}

func decodeHexField(field string, value string) ([]byte, string, error) {
	normalized := strings.ToUpper(strings.TrimSpace(value))
	if normalized == "" {
		return nil, normalized, fmt.Errorf("%s 不能为空", field)
	}
	if len(normalized)%2 != 0 {
		return nil, normalized, fmt.Errorf("%s 长度必须为偶数", field)
	}
	data, err := hex.DecodeString(normalized)
	if err != nil {
		return nil, normalized, err
	}
	return data, normalized, nil
}

func LoadPortSummaries(db *sql.DB) ([]PortSummary, error) {
	rows, err := db.Query("SELECT port, COUNT(*) FROM ports WHERE port BETWEEN 1 AND 65535 GROUP BY port ORDER BY port")
	if err != nil {
		return nil, fmt.Errorf("查询端口列表失败: %w", err)
	}
	defer rows.Close()

	var summaries []PortSummary
	for rows.Next() {
		var summary PortSummary
		if err := rows.Scan(&summary.Port, &summary.Items); err != nil {
			return nil, fmt.Errorf("扫描端口行失败: %w", err)
		}
		summaries = append(summaries, summary)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历端口行失败: %w", err)
	}
	return summaries, nil
}
