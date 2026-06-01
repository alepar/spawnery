package firewall

import (
	"strings"
	"testing"
)

func TestRulesBlockFloor(t *testing.T) {
	rules := Rules(nil)
	joined := make([]string, len(rules))
	for i, r := range rules {
		joined[i] = strings.Join(r.Args, " ")
	}
	all := strings.Join(joined, "\n")
	for _, cidr := range []string{"169.254.0.0/16", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"} {
		if !strings.Contains(all, "-d "+cidr+" -j DROP") {
			t.Fatalf("missing DROP for %s in:\n%s", cidr, all)
		}
	}
	loIdx, firstDrop := -1, -1
	for i, j := range joined {
		if strings.Contains(j, "-o lo -j ACCEPT") && loIdx == -1 {
			loIdx = i
		}
		if strings.Contains(j, "-j DROP") && firstDrop == -1 {
			firstDrop = i
		}
	}
	if loIdx == -1 || firstDrop == -1 || loIdx > firstDrop {
		t.Fatalf("lo ACCEPT (%d) must precede first DROP (%d)", loIdx, firstDrop)
	}
}

func TestRulesAllowDNSBeforeDrops(t *testing.T) {
	rules := Rules(nil)
	joined := make([]string, len(rules))
	for i, r := range rules {
		joined[i] = strings.Join(r.Args, " ")
	}
	udp, tcp, firstDrop := -1, -1, -1
	for i, j := range joined {
		if strings.Contains(j, "-p udp --dport 53 -j ACCEPT") && udp == -1 {
			udp = i
		}
		if strings.Contains(j, "-p tcp --dport 53 -j ACCEPT") && tcp == -1 {
			tcp = i
		}
		if strings.Contains(j, "-j DROP") && firstDrop == -1 {
			firstDrop = i
		}
	}
	if udp == -1 || tcp == -1 {
		t.Fatalf("missing DNS ACCEPT rules (udp=%d tcp=%d):\n%s", udp, tcp, strings.Join(joined, "\n"))
	}
	if udp > firstDrop || tcp > firstDrop {
		t.Fatalf("DNS ACCEPT (udp=%d tcp=%d) must precede first DROP (%d)", udp, tcp, firstDrop)
	}
}

func TestRulesAllowCIDRsBeforeDrops(t *testing.T) {
	rules := Rules([]string{"192.168.50.0/24"})
	acceptIdx, dropIdx := -1, -1
	for i, r := range rules {
		j := strings.Join(r.Args, " ")
		if strings.Contains(j, "-d 192.168.50.0/24 -j ACCEPT") {
			acceptIdx = i
		}
		if strings.Contains(j, "-j DROP") && dropIdx == -1 {
			dropIdx = i
		}
	}
	if acceptIdx == -1 {
		t.Fatal("allow-CIDR ACCEPT rule missing")
	}
	if acceptIdx > dropIdx {
		t.Fatalf("allow-CIDR ACCEPT (%d) must precede DROPs (%d)", acceptIdx, dropIdx)
	}
}
