package libv2ray

import (
	"fmt"
	corenet "github.com/xtls/xray-core/common/net"
)

// ProcessFinder is an interface for Android process finding functionality.
// Apps using AndroidLibXrayLite should implement FindProcessByConnection()
// and pass the implementation to RegisterProcessFinder() before starting the core.
type ProcessFinder interface {
	// FindProcessByConnection finds the UID of the process that owns the given connection.
	//
	// network: Protocol type: "tcp" or "udp"
	// srcIP: Source IP address
	// srcPort: Source port
	// destIP: Destination IP address
	// destPort: Destination port
	// Returns the UID of the owning process, or -1 if not found.
	FindProcessByConnection(network, srcIP string, srcPort int, destIP string, destPort int) int
}

// RegisterProcessFinder registers an Android process finder with Xray-core,
// enabling per-app routing based on UID. Must be called before starting the
// core for process-based routing rules to work.
// Pass nil to unregister a previously registered finder.
func (x *CoreController) RegisterProcessFinder(finder ProcessFinder) {
	if finder == nil {
		corenet.RegisterAndroidProcessFinder(nil)
		return
	}

	corenet.RegisterAndroidProcessFinder(func(network, srcIP string, srcPort uint16, destIP string, destPort uint16) (uid int, name string, path string, err error) {
		defer func() {
			if r := recover(); r != nil {
				uid, name, path, err = 0, "", "", fmt.Errorf("process finder panic: %v", r)
			}
		}()
		uid = finder.FindProcessByConnection(network, srcIP, int(srcPort), destIP, int(destPort))
		if uid < 0 {
			return 0, "", "", fmt.Errorf("process not found for %s %s:%d -> %s:%d", network, srcIP, srcPort, destIP, destPort)
		}
		return uid, fmt.Sprintf("%d", uid), "", nil
	})
}
