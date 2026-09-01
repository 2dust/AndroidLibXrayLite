//go:build android

package libv2ray

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// /proc/net/* UID resolver. Used when getConnectionOwnerUid() (API < 29) is
// unavailable.
// follows c impl
//	https://github.com/emanuele-f/PCAPdroid/blob/master/app/src/main/jni/common/uid_resolver.c
//
// /proc/net/{tcp,udp} field layout (after strings.Fields):
//
//	[0] sl  [1] local_address  [2] rem_address  [3] st
//	[4] tx_queue:rx_queue  [5] tr:tm->when  [6] retrnsmt
//	[7] uid  [8] timeout  [9] inode
// ResolveUidFromProc resolves the UID of the process that owns a given connection
// by reading /proc/net/{tcp,udp} files. This is used when getConnectionOwnerUid()
// (API < 29) is unavailable.
//
// network: "tcp" or "udp"
// srcIP: Source IP address
// srcPort: Source port
// destIP: Destination IP address
// destPort: Destination port
// Returns the UID of the owning process, or -1 if not found.
func ResolveUidFromProc(network, srcIP string, srcPort int, destIP string, destPort int) (int, error) {
	ip := net.ParseIP(srcIP)
	if ip == nil {
		return -1, fmt.Errorf("bad src IP %s", srcIP)
	}

	if ip4 := ip.To4(); ip4 != nil {
		uid, err := resolve4(network, ip4, srcPort, destIP, destPort)
		if err == nil {
			return uid, nil
		}
		// PCAPdroid: sprintf("0000000000000000FFFF0000%08X", ip4)
		// Catches IPv4 conns shown in tcp6/udp6.
		mapped := make([]byte, 16)
		copy(mapped[10:], []byte{0xFF, 0xFF})
		copy(mapped[12:], ip4)
		return resolve6(network, mapped, srcPort, destIP, destPort)
	}

	ip16 := ip.To16()
	if ip16 != nil {
		return resolve6(network, ip16, srcPort, destIP, destPort)
	}

	return -1, fmt.Errorf("bad IP family for %s", srcIP)
}

func resolve4(network string, ip4 []byte, srcPort int, destIP string, destPort int) (int, error) {
	procFile := procFilePath(network, false)
	if procFile == "" {
		return -1, fmt.Errorf("unsupported network %s", network)
	}
	targetHex, _ := formatHexAddr4(ip4, srcPort)
	destHex, _ := formatDestHex(destIP, destPort, false)
	return findUidInProcFile(procFile, targetHex, destHex)
}

func resolve6(network string, ip16 []byte, srcPort int, destIP string, destPort int) (int, error) {
	procFile := procFilePath(network, true)
	if procFile == "" {
		return -1, fmt.Errorf("unsupported network %s", network)
	}
	targetHex, _ := formatHexAddr6(ip16, srcPort)
	destHex, _ := formatDestHex(destIP, destPort, true)
	return findUidInProcFile(procFile, targetHex, destHex)
}

func procFilePath(network string, v6 bool) string {
	switch network {
	case "tcp":
		if v6 {
			return "/proc/net/tcp6"
		}
		return "/proc/net/tcp"
	case "udp":
		if v6 {
			return "/proc/net/udp6"
		}
		return "/proc/net/udp"
	default:
		return ""
	}
}

// formatDestHex -> format dest IP:port for matching. Returns "" if dest
// info is missing (match any remote).
func formatDestHex(destIP string, destPort int, v6 bool) (string, error) {
	ip := net.ParseIP(destIP)
	if ip == nil || destPort <= 0 {
		return "", nil
	}
	if v6 {
		if ip16 := ip.To16(); ip16 != nil {
			return formatHexAddr6(ip16, destPort)
		}
		if ip4 := ip.To4(); ip4 != nil {
			mapped := make([]byte, 16)
			copy(mapped[10:], []byte{0xFF, 0xFF})
			copy(mapped[12:], ip4)
			return formatHexAddr6(mapped, destPort)
		}
		return "", nil
	}
	if ip4 := ip.To4(); ip4 != nil {
		return formatHexAddr4(ip4, destPort)
	}
	return "", nil
}

// /proc/net/* stores IPs per 32-bit word in little-endian byte order.
// PCAPdroid casts in6_addr to uint32_t[4] on LE hardware; we replicate
// that here.
func formatHexAddr4(ip4 []byte, port int) (string, error) {
	b := make([]byte, 4)
	copy(b, ip4)
	for i, j := 0, 3; i < j; i, j = i+1, j-1 {
		b[i], b[j] = b[j], b[i]
	}
	return fmt.Sprintf("%s:%04X", strings.ToUpper(hex.EncodeToString(b)), port), nil
}

func formatHexAddr6(ip16 []byte, port int) (string, error) {
	words := make([]string, 4)
	for w := 0; w < 4; w++ {
		off := w * 4
		b := []byte{ip16[off+3], ip16[off+2], ip16[off+1], ip16[off+0]}
		words[w] = strings.ToUpper(hex.EncodeToString(b))
	}
	return fmt.Sprintf("%s:%04X", strings.Join(words, ""), port), nil
}

func allZeros(s string) bool {
	for _, c := range s {
		if c != '0' {
			return false
		}
	}
	return len(s) > 0
}

//
//   sport == src_port
//   (dport == dst_port || dport == 0)
//   (dhex == conn_dhex || dhex == zero)
//   (shex == conn_shex || shex == zero)
//
// where shex/dhex/sport/dport are from /proc and
// conn_shex/conn_dhex/src_port/dst_port are our connection args.
func findUidInProcFile(filePath, targetHex, destHex string) (int, error) {
	f, err := os.Open(filePath)
	if err != nil {
		return -1, err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	first := true
	for s.Scan() {
		if first {
			first = false
			continue
		}

		fields := strings.Fields(s.Text())
		if len(fields) < 8 {
			continue
		}

		// Split hex:port into parts for independent matching.
		loc := strings.SplitN(fields[1], ":", 2)
		tgt := strings.SplitN(targetHex, ":", 2)
		if len(loc) != 2 || len(tgt) != 2 {
			continue
		}

		// sport == src_port
		if loc[1] != tgt[1] {
			continue
		}
		// shex == conn_shex || shex == zero
		if loc[0] != tgt[0] && !allZeros(loc[0]) {
			continue
		}

		if destHex != "" {
			rem := strings.SplitN(fields[2], ":", 2)
			dst := strings.SplitN(destHex, ":", 2)
			if len(rem) != 2 || len(dst) != 2 {
				continue
			}

			// dport == dst_port || dport == 0
			if rem[1] != dst[1] && rem[1] != "0000" {
				continue
			}
			// dhex == conn_dhex || dhex == zero
			if rem[0] != dst[0] && !allZeros(rem[0]) {
				continue
			}
		}

		uid, err := strconv.Atoi(fields[7])
		if err != nil {
			continue
		}
		return uid, nil
	}

	return -1, fmt.Errorf("not found in %s", filePath)
}
