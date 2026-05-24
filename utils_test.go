package main

import (
	"net"
	"testing"
)

func TestSelectLocalIPv4SkipsLocalAndVirtualCandidates(t *testing.T) {
	hardwareAddr := net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}
	flags := net.FlagUp | net.FlagBroadcast | net.FlagMulticast
	candidates := []localIPCandidate{
		{
			name:         "Loopback Pseudo-Interface 1",
			flags:        net.FlagUp | net.FlagLoopback,
			hardwareAddr: hardwareAddr,
			ip:           net.ParseIP("127.0.0.1"),
		},
		{
			name:         "以太网 2",
			flags:        flags,
			hardwareAddr: hardwareAddr,
			ip:           net.ParseIP("169.254.113.251"),
		},
		{
			name:         "vEthernet (Default Switch)",
			flags:        flags,
			hardwareAddr: hardwareAddr,
			ip:           net.ParseIP("172.20.64.1"),
		},
		{
			name:         "以太网",
			flags:        flags,
			hardwareAddr: hardwareAddr,
			ip:           net.ParseIP("10.0.0.20"),
		},
	}

	got, err := selectLocalIPv4(candidates)
	if err != nil {
		t.Fatalf("selectLocalIPv4 returned error: %v", err)
	}
	if got.String() != "10.0.0.20" {
		t.Fatalf("selectLocalIPv4 = %s, want 10.0.0.20", got)
	}
}

func TestSelectLocalIPv4ReturnsErrorWhenNoUsableAddressExists(t *testing.T) {
	hardwareAddr := net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55}
	candidates := []localIPCandidate{
		{
			name:         "以太网",
			flags:        0,
			hardwareAddr: hardwareAddr,
			ip:           net.ParseIP("10.0.0.20"),
		},
		{
			name:         "VMware Network Adapter VMnet8",
			flags:        net.FlagUp | net.FlagBroadcast | net.FlagMulticast,
			hardwareAddr: hardwareAddr,
			ip:           net.ParseIP("192.168.100.1"),
		},
		{
			name:         "以太网",
			flags:        net.FlagUp | net.FlagBroadcast | net.FlagMulticast,
			hardwareAddr: nil,
			ip:           net.ParseIP("10.0.0.20"),
		},
		{
			name:         "以太网",
			flags:        net.FlagUp | net.FlagBroadcast | net.FlagMulticast,
			hardwareAddr: hardwareAddr,
			ip:           net.ParseIP("169.254.113.251"),
		},
	}

	if _, err := selectLocalIPv4(candidates); err == nil {
		t.Fatal("selectLocalIPv4 returned nil error, want failure")
	}
}

func TestIsLikelyVirtualInterface(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{name: "以太网", want: false},
		{name: "WLAN", want: false},
		{name: "vEthernet (Default Switch)", want: true},
		{name: "VMware Network Adapter VMnet8", want: true},
		{name: "Tailscale", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLikelyVirtualInterface(tt.name); got != tt.want {
				t.Fatalf("isLikelyVirtualInterface(%q) = %v, want %v", tt.name, got, tt.want)
			}
		})
	}
}

func TestValidateLocalBindIPWithCandidates(t *testing.T) {
	candidates := []localIPCandidate{
		{
			name:         "Loopback Pseudo-Interface 1",
			flags:        net.FlagUp | net.FlagLoopback,
			hardwareAddr: nil,
			ip:           net.ParseIP("127.0.0.1"),
		},
		{
			name:         "以太网",
			flags:        net.FlagUp | net.FlagBroadcast | net.FlagMulticast,
			hardwareAddr: net.HardwareAddr{0x00, 0x11, 0x22, 0x33, 0x44, 0x55},
			ip:           net.ParseIP("10.0.0.20"),
		},
	}

	tests := []struct {
		name    string
		rawIP   string
		want    string
		wantErr bool
	}{
		{name: "physical ip", rawIP: "10.0.0.20", want: "10.0.0.20"},
		{name: "loopback ip", rawIP: "127.0.0.1", want: "127.0.0.1"},
		{name: "trim spaces", rawIP: " 10.0.0.20 ", want: "10.0.0.20"},
		{name: "invalid syntax", rawIP: "not-an-ip", wantErr: true},
		{name: "unspecified", rawIP: "0.0.0.0", wantErr: true},
		{name: "non local", rawIP: "10.0.0.21", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateLocalBindIPWithCandidates(tt.rawIP, candidates)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("validateLocalBindIPWithCandidates(%q) returned nil error", tt.rawIP)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateLocalBindIPWithCandidates(%q) returned error: %v", tt.rawIP, err)
			}
			if got != tt.want {
				t.Fatalf("validateLocalBindIPWithCandidates(%q) = %q, want %q", tt.rawIP, got, tt.want)
			}
		})
	}
}
