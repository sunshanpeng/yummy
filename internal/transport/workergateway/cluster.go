package workergateway

import (
	"time"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/lib/workerprotocol"
	"google.golang.org/protobuf/proto"
)

func (g *Gateway) storeClusterMachines(s *session, uid string, report *platformv1.ClusterMachinesReport) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.sessions[s.identity.NodeID] != s {
		return
	}
	s.clusterMachines = nil
	if uid == "" || report == nil || len(report.Machines) > workerprotocol.ClusterMachineLimit {
		return
	}
	if report.ErrorCode != "" {
		if report.ErrorCode != "FORBIDDEN" && report.ErrorCode != "UNAVAILABLE" {
			return
		}
		s.clusterMachines = &platformv1.ClusterMachinesReport{ErrorCode: report.ErrorCode}
	} else {
		hasWorker := false
		seen := make(map[string]bool, len(report.Machines))
		for _, node := range report.Machines {
			if node.GetUid() == "" || len(node.Uid) > 256 || seen[node.Uid] || node.Name == "" || len(node.Name) > 253 || len(node.Os) > 32 || len(node.Arch) > 32 || len(node.KubeletVersion) > 100 {
				return
			}
			seen[node.Uid] = true
			hasWorker = hasWorker || node.Uid == uid
		}
		if !report.Truncated && !hasWorker {
			return
		}
		s.clusterMachines = proto.CloneOf(report)
	}
	s.machinesObservedAt = time.Now().UTC()
}

// ClusterMachines 只读取当前有效连接的近期报告，断开或过期后不显示旧清单。
func (g *Gateway) ClusterMachines(nodeID string) (*platformv1.ClusterMachinesReport, time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := g.sessions[nodeID]
	if s == nil || s.ctx.Err() != nil || s.clusterMachines == nil || time.Since(s.machinesObservedAt) > 90*time.Second {
		return nil, time.Time{}
	}
	return proto.CloneOf(s.clusterMachines), s.machinesObservedAt
}
