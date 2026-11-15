package api

import (
	"net"
	"strings"
	"testing"
)

func TestIsVirtualNIC(t *testing.T) {
	cases := []struct {
		name  string
		flags net.Flags
		want  bool
	}{
		{"以太网", net.FlagUp | net.FlagBroadcast, false},
		{"Ethernet", net.FlagUp | net.FlagBroadcast, false},
		{"WLAN", net.FlagUp | net.FlagBroadcast, false},
		{"en0", net.FlagUp | net.FlagBroadcast, false},
		{"eth0", net.FlagUp | net.FlagBroadcast, false},

		{"VMware Network Adapter VMnet1", net.FlagUp | net.FlagBroadcast, true},
		{"VirtualBox Host-Only Network", net.FlagUp | net.FlagBroadcast, true},
		{"vEthernet (Hyper-V)", net.FlagUp | net.FlagBroadcast, true},
		{"vEthernet (WSL)", net.FlagUp | net.FlagBroadcast, true},
		{"Loopback Pseudo-Interface 1", net.FlagUp, true},
		{"docker0", net.FlagUp | net.FlagBroadcast, true},
		{"br-1a2b3c4d5e6f", net.FlagUp | net.FlagBroadcast, true},
		{"tailscale0", net.FlagUp | net.FlagPointToPoint, true},
		{"tun0", net.FlagUp | net.FlagPointToPoint, true},
		{"ZeroTier One [af.in]", net.FlagUp | net.FlagBroadcast, true},

		// 点对点且无广播 → 隧道类
		{"some-ppp-link", net.FlagUp | net.FlagPointToPoint, true},
	}
	for _, c := range cases {
		if got := isVirtualNIC(c.name, c.flags); got != c.want {
			t.Errorf("isVirtualNIC(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestListNetIfsSanity(t *testing.T) {
	ifs := ListNetIfs()
	for _, n := range ifs {
		ip := net.ParseIP(n.IP)
		if ip == nil || ip.To4() == nil {
			t.Errorf("网卡 %s 的 IP %q 不是有效 IPv4", n.Name, n.IP)
		}
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
			t.Errorf("网卡 %s 的 IP %q 不应为 loopback/链路本地地址", n.Name, n.IP)
		}
		if strings.ToLower(n.Name) == "lo" {
			t.Errorf("loopback 接口不应出现在列表中: %s", n.Name)
		}
	}
	// 排序不变量：物理网卡必须在虚拟网卡之前
	virtualSeen := false
	for _, n := range ifs {
		if n.IsVirtual {
			virtualSeen = true
		} else if virtualSeen {
			t.Errorf("物理网卡 %s 排在了虚拟网卡之后", n.Name)
		}
	}
}

func TestConsoleURLsSpecificHost(t *testing.T) {
	urls := ConsoleURLs("192.168.10.20:7373", "tok")
	if len(urls) != 1 {
		t.Fatalf("监听具体 IP 应只返回 1 个地址，得到 %d", len(urls))
	}
	if urls[0].URL != "http://192.168.10.20:7373/?token=tok" {
		t.Fatalf("URL = %q", urls[0].URL)
	}
	if urls[0].Name != "" {
		t.Fatalf("具体 IP 监听不应带网卡名: %q", urls[0].Name)
	}
	// 无令牌
	urls = ConsoleURLs("10.0.0.5:8080", "")
	if urls[0].URL != "http://10.0.0.5:8080" {
		t.Fatalf("URL = %q", urls[0].URL)
	}
}

func TestConsoleURLsWildcard(t *testing.T) {
	urls := ConsoleURLs("0.0.0.0:7373", "t")
	if len(urls) == 0 {
		t.Skip("本机无 IPv4 网卡，跳过")
	}
	ifs := ListNetIfs()
	if len(urls) != len(ifs) {
		t.Fatalf("0.0.0.0 监听应逐网卡列出: urls=%d ifs=%d", len(urls), len(ifs))
	}
	// 物理优先 + 名称与虚拟标记对齐 + URL 均以 /?token=t 结尾
	for i, u := range urls {
		if u.Name != ifs[i].Name || u.IsVirtual != ifs[i].IsVirtual {
			t.Fatalf("urls[%d] 与 ifs[%d] 不对齐", i, i)
		}
		if !strings.HasSuffix(u.URL, ":7373/?token=t") {
			t.Fatalf("URL = %q", u.URL)
		}
	}
	// IPv6 通配也按网卡列出
	urls6 := ConsoleURLs("[::]:7373", "")
	if len(urls6) != len(ifs) {
		t.Fatalf("[::] 监听应同样逐网卡列出")
	}
}

func TestPrimaryNetIfPrefersPhysical(t *testing.T) {
	n, ok := PrimaryNetIf()
	ifs := ListNetIfs()
	if len(ifs) == 0 {
		if ok {
			t.Fatal("无网卡时 ok 应为 false")
		}
		return
	}
	if !ok {
		t.Fatal("有网卡时 ok 应为 true")
	}
	// 若存在物理网卡，主选必须是物理的
	hasPhysical := false
	for _, f := range ifs {
		if !f.IsVirtual {
			hasPhysical = true
			break
		}
	}
	if hasPhysical && n.IsVirtual {
		t.Fatalf("PrimaryNetIf 选中了虚拟网卡 %s，存在物理网卡时应优先物理", n.Name)
	}
}
