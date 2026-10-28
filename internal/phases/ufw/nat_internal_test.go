package ufw

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/Aitor42/CMS-HA-Infrastructure/internal/testutil"
)

func testNATSpec() NATSpec {
	return NATSpec{
		WANIface:     "enp3s0",
		InternalCIDR: "192.168.10.0/24",
		MainCIDR:     "192.168.20.0/24",
		VIP:          "192.168.20.100",
	}
}

func TestRenderNATRulesUsesConfiguredNetworks(t *testing.T) {
	spec := NATSpec{
		WANIface:     "ens3",
		InternalCIDR: "10.10.0.42/16", // non-network address: must be canonicalised
		MainCIDR:     "10.20.0.0/16",
		VIP:          "10.20.0.100",
	}
	rules, err := RenderNATRules(spec)
	if err != nil {
		t.Fatalf("RenderNATRules() = %v", err)
	}
	got := string(rules)
	if strings.Contains(got, "${") {
		t.Errorf("rendered rules contain an unresolved placeholder:\n%s", got)
	}
	for _, want := range []string{
		"-A PREROUTING -i ens3 -p tcp --dport 80 -j DNAT --to-destination 10.20.0.100:80",
		"-A PREROUTING -i ens3 -p tcp --dport 443 -j DNAT --to-destination 10.20.0.100:443",
		"-A POSTROUTING -s 10.10.0.0/16 -o ens3 -j MASQUERADE",
		"-A POSTROUTING -s 10.20.0.0/16 -o ens3 -j MASQUERADE",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered rules are missing %q:\n%s", want, got)
		}
	}
}

func TestRenderNATRulesRejectsInvalidSpec(t *testing.T) {
	tests := []struct {
		name string
		edit func(*NATSpec)
		want string
	}{
		{"empty WAN", func(s *NATSpec) { s.WANIface = "" }, "WAN interface"},
		{"shell in WAN", func(s *NATSpec) { s.WANIface = "enp3s0;rm" }, "WAN interface"},
		{"invalid internal CIDR", func(s *NATSpec) { s.InternalCIDR = "not-a-cidr" }, "internal network CIDR"},
		{"IPv6 internal CIDR", func(s *NATSpec) { s.InternalCIDR = "fd00::/64" }, "not IPv4"},
		{"invalid main CIDR", func(s *NATSpec) { s.MainCIDR = "" }, "main network CIDR"},
		{"invalid VIP", func(s *NATSpec) { s.VIP = "192.168.20.999" }, "VIP"},
		{"IPv6 VIP", func(s *NATSpec) { s.VIP = "fd00::100" }, "VIP"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := testNATSpec()
			tc.edit(&spec)
			if _, err := RenderNATRules(spec); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("RenderNATRules(%+v) = %v, want an error containing %q", spec, err, tc.want)
			}
		})
	}
}

func TestWANIfaceByMAC(t *testing.T) {
	fake := &testutil.FakeExecutor{RunFunc: func(_ context.Context, _, cmd string) (string, string, int, error) {
		if strings.Contains(cmd, "ip -o link") {
			return "enp3s0\n", "", 0, nil
		}
		return "", "", 1, nil
	}}
	got, err := WANIface(t.Context(), fake, testRouterIP, "52:54:00:10:00:02")
	if err != nil || got != "enp3s0" {
		t.Fatalf("WANIface() = %q, %v; want enp3s0", got, err)
	}
}

func TestWANIfaceFallsBackToDefaultRoute(t *testing.T) {
	fake := &testutil.FakeExecutor{RunFunc: func(_ context.Context, _, cmd string) (string, string, int, error) {
		switch {
		case strings.Contains(cmd, "ip -o link"):
			return "", "", 0, nil
		case strings.Contains(cmd, "ip route show default"):
			return "enp5s0\n", "", 0, nil
		default:
			return "", "", 1, nil
		}
	}}
	got, err := WANIface(t.Context(), fake, testRouterIP, "52:54:00:10:00:02")
	if err != nil || got != "enp5s0" {
		t.Fatalf("WANIface() = %q, %v; want enp5s0", got, err)
	}
}

func TestWANIfaceLegacyFallbackRequiresTheInterface(t *testing.T) {
	t.Run("exists", func(t *testing.T) {
		fake := &testutil.FakeExecutor{RunFunc: func(_ context.Context, _, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, "ip link show "+DefaultWANIface) {
				return "1: enp3s0: <BROADCAST>", "", 0, nil
			}
			return "", "", 0, nil
		}}
		got, err := WANIface(t.Context(), fake, testRouterIP, "")
		if err != nil || got != DefaultWANIface {
			t.Fatalf("WANIface() = %q, %v; want %s", got, err, DefaultWANIface)
		}
	})
	t.Run("missing", func(t *testing.T) {
		fake := &testutil.FakeExecutor{RunFunc: func(_ context.Context, _, _ string) (string, string, int, error) {
			return "", "", 1, nil
		}}
		if _, err := WANIface(t.Context(), fake, testRouterIP, ""); err == nil {
			t.Fatal("WANIface() succeeded without a resolvable WAN interface")
		}
	})
}

func TestWANIfaceRejectsInvalidMACAndAmbiguousOutput(t *testing.T) {
	t.Run("invalid MAC", func(t *testing.T) {
		if _, err := WANIface(t.Context(), &testutil.FakeExecutor{}, testRouterIP, "not-a-mac"); err == nil {
			t.Fatal("WANIface() accepted an invalid MAC")
		}
	})
	t.Run("multiple MAC matches", func(t *testing.T) {
		fake := &testutil.FakeExecutor{RunFunc: func(_ context.Context, _, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, "ip -o link") {
				return "enp3s0\nenp4s0\n", "", 0, nil
			}
			return "", "", 0, nil
		}}
		if _, err := WANIface(t.Context(), fake, testRouterIP, "52:54:00:10:00:02"); err == nil || !strings.Contains(err.Error(), "multiple") {
			t.Fatalf("WANIface() = %v, want a multiple-interface error", err)
		}
	})
	t.Run("invalid interface name", func(t *testing.T) {
		fake := &testutil.FakeExecutor{RunFunc: func(_ context.Context, _, cmd string) (string, string, int, error) {
			if strings.Contains(cmd, "ip -o link") {
				return "bad/iface\n", "", 0, nil
			}
			return "", "", 0, nil
		}}
		if _, err := WANIface(t.Context(), fake, testRouterIP, "52:54:00:10:00:02"); err == nil || !strings.Contains(err.Error(), "invalid interface") {
			t.Fatalf("WANIface() = %v, want an invalid-interface error", err)
		}
	})
}

func TestConfigureNATUploadsAndInjectsIdempotently(t *testing.T) {
	fake := &testutil.FakeExecutor{}
	if _, err := ConfigureNAT(t.Context(), fake, testRouterIP, testNATSpec(), false); err != nil {
		t.Fatalf("ConfigureNAT() = %v", err)
	}
	content := string(fake.Files[testRouterIP][natRulesPath])
	if !strings.Contains(content, "192.168.20.100:80") || !strings.Contains(content, "-s 192.168.10.0/24") {
		t.Errorf("uploaded NAT rules are wrong:\n%s", content)
	}
	for _, want := range []string{
		"cp /etc/ufw/before.rules /tmp/before.rules.bak",
		"trap restore ERR",
		"sed -i '/^# NAT Rules/,/^COMMIT/",
		"awk '/^\\*filter/",
		"grep -c '^\\*nat'",
		"rm -f /tmp/nat.rules /tmp/before.rules.bak",
	} {
		if !hasCommand(fake.Commands, want) {
			t.Errorf("ConfigureNAT() did not run %q", want)
		}
	}
	if hasCommand(fake.Commands, "iptables-restore") {
		t.Error("ConfigureNAT(apply=false) loaded the live table")
	}
}

func TestConfigureNATDoesNotDeleteForeignNATTable(t *testing.T) {
	fake := &testutil.FakeExecutor{}
	if _, err := ConfigureNAT(t.Context(), fake, testRouterIP, testNATSpec(), false); err != nil {
		t.Fatalf("ConfigureNAT() = %v", err)
	}
	var inject string
	for _, command := range fake.Commands {
		if strings.Contains(command.Cmd, "cp /etc/ufw/before.rules") {
			inject = command.Cmd
			break
		}
	}
	if inject == "" {
		t.Fatal("ConfigureNAT() did not run the injection script")
	}
	if strings.Contains(inject, "sed -i '/^\\*nat/") {
		t.Error("ConfigureNAT() still deletes any *nat table instead of only its managed block")
	}
	if !strings.Contains(inject, "refusing to delete an unmanaged *nat table") {
		t.Error("ConfigureNAT() does not refuse to delete a foreign *nat table")
	}
}

func TestConfigureNATLoadsLiveTableWhenRequested(t *testing.T) {
	fake := &testutil.FakeExecutor{}
	if _, err := ConfigureNAT(t.Context(), fake, testRouterIP, testNATSpec(), true); err != nil {
		t.Fatalf("ConfigureNAT() = %v", err)
	}
	if !hasCommand(fake.Commands, "iptables-restore < "+natRulesPath) {
		t.Error("ConfigureNAT(apply=true) did not load the live nat table")
	}
}

func TestConfigureNATFailsWhenInjectionFails(t *testing.T) {
	fake := &testutil.FakeExecutor{RunFunc: func(_ context.Context, _, cmd string) (string, string, int, error) {
		if strings.Contains(cmd, "cp /etc/ufw/before.rules") {
			return "", "bad rules", 1, fmt.Errorf("exit status 1")
		}
		return "", "", 0, nil
	}}
	if _, err := ConfigureNAT(t.Context(), fake, testRouterIP, testNATSpec(), false); err == nil || !strings.Contains(err.Error(), "inject NAT rules") {
		t.Fatalf("ConfigureNAT() = %v, want an injection error", err)
	}
}

func TestConfigureNATFailsWhenUploadFails(t *testing.T) {
	fake := &testutil.FakeExecutor{CopyErr: map[string]error{testRouterIP: fmt.Errorf("sftp unavailable")}}
	if _, err := ConfigureNAT(t.Context(), fake, testRouterIP, testNATSpec(), false); err == nil || !strings.Contains(err.Error(), "upload NAT rules") {
		t.Fatalf("ConfigureNAT() = %v, want an upload error", err)
	}
}
