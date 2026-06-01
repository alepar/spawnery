package firewall

import (
	"strings"
	"testing"
)

func TestRulesHostFloor(t *testing.T) {
	rules := Rules("172.17.0.5", []string{"10.9.9.0/24"})
	var lines []string
	for _, r := range rules {
		lines = append(lines, strings.Join(r.Args, " "))
	}
	all := strings.Join(lines, "\n")
	for _, l := range lines {
		if !strings.HasPrefix(l, "-s 172.17.0.5 ") {
			t.Fatalf("rule not scoped to -s ip: %q", l)
		}
	}
	for _, cidr := range []string{"169.254.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		if !strings.Contains(all, "-d "+cidr+" -j DROP") {
			t.Fatalf("missing DROP for %s", cidr)
		}
	}
	if !strings.Contains(all, "-d 10.9.9.0/24 -j ACCEPT") {
		t.Fatal("missing allow-CIDR ACCEPT")
	}
	udp, firstDrop := -1, -1
	for i, l := range lines {
		if strings.Contains(l, "--dport 53 -j ACCEPT") && udp == -1 {
			udp = i
		}
		if strings.Contains(l, "-j DROP") && firstDrop == -1 {
			firstDrop = i
		}
	}
	if udp == -1 || firstDrop == -1 || udp > firstDrop {
		t.Fatalf("DNS ACCEPT (%d) must precede first DROP (%d)", udp, firstDrop)
	}
}
