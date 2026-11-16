// Package ws 提供 rkttl 的 WebSocket 服务层：实时串口收发 + xterm.js 网页控制台。
//
// 与旧的 SSE + HTTP POST 组合不同，WebSocket 是全双工单连接：
//   - 服务器通过二进制帧推送串口原始字节（无需 base64）
//   - 客户端通过文本命令或二进制帧发送输入
//   - 断线后自动重连，客户端通过 seq 游标从环形缓冲补齐历史
package ws

import (
	"fmt"
	"net"
	"sort"
	"strings"

	"go.bug.st/serial"
	"go.bug.st/serial/enumerator"
)

// PortInfo 描述一个本机串口。
type PortInfo struct {
	Name         string `json:"name"`
	IsUSB        bool   `json:"is_usb"`
	VID          string `json:"vid,omitempty"`
	PID          string `json:"pid,omitempty"`
	SerialNumber string `json:"serial_number,omitempty"`
	Product      string `json:"product,omitempty"`
}

// ListPorts 枚举本机串口。
func ListPorts() ([]PortInfo, error) {
	details, err := enumerator.GetDetailedPortsList()
	if err != nil {
		names, err2 := serial.GetPortsList()
		if err2 != nil {
			return nil, fmt.Errorf("枚举串口失败: %v / %v", err, err2)
		}
		out := make([]PortInfo, 0, len(names))
		for _, n := range names {
			out = append(out, PortInfo{Name: n})
		}
		return out, nil
	}
	out := make([]PortInfo, 0, len(details))
	for _, d := range details {
		out = append(out, PortInfo{
			Name:         d.Name,
			IsUSB:        d.IsUSB,
			VID:          d.VID,
			PID:          d.PID,
			SerialNumber: d.SerialNumber,
			Product:      d.Product,
		})
	}
	return out, nil
}

// NetIf 描述一个带 IPv4 地址的网卡。
type NetIf struct {
	Name      string `json:"name"`
	IP        string `json:"ip"`
	IsVirtual bool   `json:"is_virtual"`
}

// ConsoleURL 是一个可供远端访问的网页控制台地址。
type ConsoleURL struct {
	Name      string `json:"name,omitempty"`
	IP        string `json:"ip"`
	URL       string `json:"url"`
	IsVirtual bool   `json:"is_virtual,omitempty"`
}

var virtualKeywords = []string{
	"vmware", "vmnet", "virtualbox", "vbox",
	"vethernet", "hyper-v", "msloop", "npcap", "wsl",
	"docker", "veth", "br-", "bridge", "virbr", "libvirt",
	"tailscale", "zerotier", "openvpn", "wireguard", "hamachi",
	"tap", "tun", "vpn", "virtual", "pseudo", "loopback", "lo0",
	"easytier",
}

func isVirtualNIC(name string, flags net.Flags) bool {
	n := strings.ToLower(name)
	for _, kw := range virtualKeywords {
		if strings.Contains(n, kw) {
			return true
		}
	}
	if flags&net.FlagPointToPoint != 0 && flags&net.FlagBroadcast == 0 {
		return true
	}
	return false
}

// ListNetIfs 枚举所有处于 UP 状态、带 IPv4 地址的非 loopback 网卡。
func ListNetIfs() []NetIf {
	ifis, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []NetIf
	for _, ifi := range ifis {
		if ifi.Flags&net.FlagLoopback != 0 || ifi.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipn.IP.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() {
				continue
			}
			out = append(out, NetIf{
				Name:      ifi.Name,
				IP:        ip4.String(),
				IsVirtual: isVirtualNIC(ifi.Name, ifi.Flags),
			})
			break
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return !out[i].IsVirtual && out[j].IsVirtual })
	return out
}

// PrimaryNetIf 返回"最可能对远端有效"的网卡。
func PrimaryNetIf() (NetIf, bool) {
	ifs := ListNetIfs()
	if len(ifs) == 0 {
		return NetIf{}, false
	}
	for _, n := range ifs {
		if !n.IsVirtual {
			return n, true
		}
	}
	return ifs[0], true
}

// ConsoleURLs 根据监听地址枚举可供远端访问的网页控制台地址。
func ConsoleURLs(listenAddr, token string) []ConsoleURL {
	host, port, err := net.SplitHostPort(listenAddr)
	if err != nil {
		host, port = listenAddr, ""
	}
	suffix := ""
	if token != "" {
		suffix = "/?token=" + token
	}
	makeURL := func(n NetIf) ConsoleURL {
		return ConsoleURL{
			Name:      n.Name,
			IP:        n.IP,
			URL:       "http://" + net.JoinHostPort(n.IP, port) + suffix,
			IsVirtual: n.IsVirtual,
		}
	}
	if host != "" && host != "0.0.0.0" && host != "::" && host != "[::]" {
		return []ConsoleURL{{IP: host, URL: "http://" + net.JoinHostPort(host, port) + suffix}}
	}
	ifs := ListNetIfs()
	out := make([]ConsoleURL, 0, len(ifs))
	for _, n := range ifs {
		out = append(out, makeURL(n))
	}
	return out
}
