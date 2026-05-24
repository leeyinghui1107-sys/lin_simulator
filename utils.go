package main

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

type localIPCandidate struct {
	name         string
	flags        net.Flags
	hardwareAddr net.HardwareAddr
	ip           net.IP
}

var virtualInterfaceNameParts = [...]string{
	"loopback",
	"virtual",
	"virtualbox",
	"vbox",
	"vmware",
	"hyper-v",
	"vethernet",
	"docker",
	"wsl",
	"npcap",
	"bluetooth",
	"teredo",
	"isatap",
	"tunnel",
	"tap",
	"tun",
	"tailscale",
	"zerotier",
	"wireguard",
	"openvpn",
	"hamachi",
	"utun",
	"bridge",
	"br-",
}

// getLocalIP 自动选择可用于监听的真实网卡 IPv4 地址。
func getLocalIP() (string, error) {
	candidates, err := getLocalIPCandidates()
	if err != nil {
		return "", err
	}

	ip, err := selectLocalIPv4(candidates)
	if err != nil {
		return "", err
	}
	return ip.String(), nil
}

func getLocalIPCandidates() ([]localIPCandidate, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	var candidates []localIPCandidate
	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ip := ipv4FromAddr(addr)
			if ip == nil {
				continue
			}
			candidates = append(candidates, localIPCandidate{
				name:         iface.Name,
				flags:        iface.Flags,
				hardwareAddr: iface.HardwareAddr,
				ip:           ip,
			})
		}
	}
	return candidates, nil
}

func validateLocalBindIP(rawIP string) (string, error) {
	candidates, err := getLocalIPCandidates()
	if err != nil {
		return "", fmt.Errorf("读取本机网卡失败: %w", err)
	}
	return validateLocalBindIPWithCandidates(rawIP, candidates)
}

func validateLocalBindIPWithCandidates(rawIP string, candidates []localIPCandidate) (string, error) {
	rawIP = strings.TrimSpace(rawIP)
	if rawIP == "" {
		return "", errors.New("IP 不能为空")
	}

	ip := net.ParseIP(rawIP).To4()
	if ip == nil {
		return "", fmt.Errorf("不是合法的 IPv4 地址: %s", rawIP)
	}
	if ip.IsUnspecified() || ip.IsMulticast() {
		return "", fmt.Errorf("不是可绑定的本机单播 IPv4 地址: %s", rawIP)
	}
	if !isAssignedLocalIPv4(ip, candidates) {
		return "", fmt.Errorf("不是本机已启用网卡上的 IPv4 地址: %s", ip.String())
	}
	return ip.String(), nil
}

func isAssignedLocalIPv4(ip net.IP, candidates []localIPCandidate) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	for _, candidate := range candidates {
		if candidate.flags&net.FlagUp == 0 {
			continue
		}
		candidateIP := candidate.ip.To4()
		if candidateIP != nil && candidateIP.Equal(ip4) {
			return true
		}
	}
	return false
}

func selectLocalIPv4(candidates []localIPCandidate) (net.IP, error) {
	for _, candidate := range candidates {
		if !isUsableInterface(candidate) || !isUsableIPv4(candidate.ip) {
			continue
		}
		return candidate.ip.To4(), nil
	}
	return nil, errors.New("未找到有效的非本地/非虚拟本机 IPv4 地址")
}

func ipv4FromAddr(addr net.Addr) net.IP {
	switch v := addr.(type) {
	case *net.IPNet:
		return v.IP.To4()
	case *net.IPAddr:
		return v.IP.To4()
	default:
		return nil
	}
}

func isUsableInterface(candidate localIPCandidate) bool {
	if candidate.flags&net.FlagUp == 0 {
		return false
	}
	if candidate.flags&net.FlagLoopback != 0 {
		return false
	}
	if candidate.flags&net.FlagPointToPoint != 0 {
		return false
	}
	if len(candidate.hardwareAddr) == 0 {
		return false
	}
	return !isLikelyVirtualInterface(candidate.name)
}

func isUsableIPv4(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	return !ip4.IsUnspecified() &&
		!ip4.IsLoopback() &&
		!ip4.IsMulticast() &&
		!ip4.IsLinkLocalUnicast() &&
		!ip4.IsLinkLocalMulticast()
}

func isLikelyVirtualInterface(name string) bool {
	normalized := strings.ToLower(strings.ReplaceAll(name, " ", ""))
	for _, part := range virtualInterfaceNameParts {
		if strings.Contains(normalized, part) {
			return true
		}
	}
	return false
}

// BytesToHex 字节转大写 hex 字符串
func BytesToHex(b []byte) string {
	const table = "0123456789ABCDEF"
	dst := make([]byte, len(b)*2)
	for i, v := range b {
		dst[i*2] = table[v>>4]
		dst[i*2+1] = table[v&0x0F]
	}
	return string(dst)
}
