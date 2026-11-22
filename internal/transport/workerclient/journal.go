package workerclient

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	platformv1 "github.com/sunshanpeng/yummy/gen/go/platform/v1"
	"github.com/sunshanpeng/yummy/internal/lib/workerprotocol"
	"google.golang.org/protobuf/proto"
)

const maxAttemptLogBytes = 20 << 20

// maxRuntimeLogStreamBytes 是单个容器实例日志流在 journal 中的字节预算；
// 超出即截断保护本地发送队列。
const maxRuntimeLogStreamBytes = 4 << 20

type journalEntry struct {
	Channel, ExecutionID string
	Seq                  int64
	Payload              []byte
}
type journalExecution struct {
	Fingerprint string
	Done        bool
}
type journalState struct {
	Version     int
	Entries     []journalEntry
	Sequences   map[string]int64
	LogBytes    map[string]int64
	Truncated   map[string]bool
	Executions  map[string]journalExecution
	Generations map[string]int64
}

// Journal 用0600快照、同目录原子替换及文件/目录fsync保存待确认事件；不持久化调度秘密。
// 占满时失败关闭，不删除未ACK的成功/失败状态；运维需恢复连接或明确处理磁盘容量。
type Journal struct {
	mu        sync.Mutex
	directory string
	root      *os.Root
	maxBytes  int64
	state     journalState
}

func OpenJournal(directory string, maxBytes int64) (*Journal, error) {
	if directory == "" {
		return nil, workerprotocol.ErrProtocol
	}
	if maxBytes == 0 {
		maxBytes = 64 << 20
	}
	if maxBytes < 1<<20 || maxBytes > 256<<20 {
		return nil, workerprotocol.ErrProtocol
	}
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, workerprotocol.ErrProtocol
	}
	if info.Mode().Perm()&0077 != 0 {
		return nil, workerprotocol.ErrProtocol
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, err
	}
	journal := &Journal{directory: absolute, root: root, maxBytes: maxBytes, state: journalState{Version: 1, Sequences: map[string]int64{}, LogBytes: map[string]int64{}, Truncated: map[string]bool{}, Executions: map[string]journalExecution{}, Generations: map[string]int64{}}}
	file, err := root.Open("journal.json")
	if errors.Is(err, os.ErrNotExist) {
		return journal, nil
	}
	if err != nil {
		root.Close()
		return nil, err
	}
	defer file.Close()
	fileInfo, err := file.Stat()
	if err != nil || !fileInfo.Mode().IsRegular() || fileInfo.Mode().Perm() != 0600 || fileInfo.Size() > maxBytes {
		root.Close()
		return nil, workerprotocol.ErrProtocol
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil || int64(len(data)) > maxBytes {
		root.Close()
		return nil, workerprotocol.ErrSpoolFull
	}
	if err := json.Unmarshal(data, &journal.state); err != nil {
		root.Close()
		return nil, workerprotocol.ErrProtocol
	}
	// 版本不匹配 = 指纹算法或状态结构升级：旧 spool 的执行指纹与新代码必然
	// 失配（workerprotocol.ErrProtocol 死循环），重置为新 spool 重新开始——观察与幂等事实在
	// 控制面持久化，重放只会重执行未完成操作，安全。
	if journal.state.Version != 1 {
		journal.state = journalState{Version: 1, Sequences: map[string]int64{}, LogBytes: map[string]int64{}, Truncated: map[string]bool{}, Entries: []journalEntry{}, Executions: map[string]journalExecution{}, Generations: map[string]int64{}}
		if err := journal.save(); err != nil {
			root.Close()
			return nil, err
		}
	}
	if journal.state.Sequences == nil || journal.state.LogBytes == nil || journal.state.Truncated == nil || journal.state.Executions == nil || journal.state.Generations == nil {
		root.Close()
		return nil, workerprotocol.ErrProtocol
	}
	for _, entry := range journal.state.Entries {
		var message platformv1.WorkerEnvelope
		if err := proto.Unmarshal(entry.Payload, &message); err != nil {
			root.Close()
			return nil, workerprotocol.ErrProtocol
		}
		channel, id, seq, err := eventIdentity(&message)
		if err != nil || channel != entry.Channel || id != entry.ExecutionID || seq != entry.Seq {
			root.Close()
			return nil, workerprotocol.ErrProtocol
		}
	}
	return journal, nil
}
func (j *Journal) Close() error { return j.root.Close() }
func (j *Journal) save() error {
	data, err := json.Marshal(j.state)
	if err != nil {
		return err
	}
	if int64(len(data)) > j.maxBytes {
		return workerprotocol.ErrSpoolFull
	}
	file, err := j.root.OpenFile("journal.tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if syncErr != nil {
		return syncErr
	}
	if closeErr != nil {
		return closeErr
	}
	if err := j.root.Rename("journal.tmp", "journal.json"); err != nil {
		return err
	}
	directory, err := j.root.Open(".")
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
func eventIdentity(message *platformv1.WorkerEnvelope) (string, string, int64, error) {
	switch value := message.Payload.(type) {
	case *platformv1.WorkerEnvelope_BuildEvent:
		if value.BuildEvent != nil {
			return "build-event", value.BuildEvent.AttemptId, value.BuildEvent.Seq, nil
		}
	case *platformv1.WorkerEnvelope_BuildLog:
		if value.BuildLog != nil {
			return "build-log", value.BuildLog.AttemptId, value.BuildLog.Seq, nil
		}
	case *platformv1.WorkerEnvelope_DeploymentObservation:
		if value.DeploymentObservation != nil {
			return "deployment", value.DeploymentObservation.OperationId, value.DeploymentObservation.Seq, nil
		}
	case *platformv1.WorkerEnvelope_RuntimeLog:
		if value.RuntimeLog != nil {
			id, err := workerprotocol.RuntimeLogStreamID(value.RuntimeLog)
			if err != nil {
				return "", "", 0, err
			}
			return "runtime-log", id, value.RuntimeLog.Seq, nil
		}
	case *platformv1.WorkerEnvelope_RuntimeMetric:
		if value.RuntimeMetric != nil {
			// 采样没有独立序号，采集毫秒时间同时充当 journal 序号与控制面幂等键；
			// 与 WorkerAck.Seq 对账即可清理对应待确认条目。
			return "runtime-metric", value.RuntimeMetric.PodUid, value.RuntimeMetric.CollectedUnixMilli, nil
		}
	}
	return "", "", 0, workerprotocol.ErrProtocol
}
func setEventSequence(message *platformv1.WorkerEnvelope, seq int64) {
	switch value := message.Payload.(type) {
	case *platformv1.WorkerEnvelope_BuildEvent:
		value.BuildEvent.Seq = seq
	case *platformv1.WorkerEnvelope_BuildLog:
		value.BuildLog.Seq = seq
	case *platformv1.WorkerEnvelope_DeploymentObservation:
		value.DeploymentObservation.Seq = seq
	case *platformv1.WorkerEnvelope_RuntimeLog:
		value.RuntimeLog.Seq = seq
	}
}

func (j *Journal) append(message *platformv1.WorkerEnvelope) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	message = proto.Clone(message).(*platformv1.WorkerEnvelope)
	channel, id, seq, err := eventIdentity(message)
	if err != nil || id == "" || len(id) > 128 || seq < 0 {
		return workerprotocol.ErrProtocol
	}
	key := channel + ":" + id
	oldLog, oldTruncated := j.state.LogBytes[id], j.state.Truncated[id]
	if log := message.GetBuildLog(); log != nil {
		if len(log.Content) > 16<<10 {
			return workerprotocol.ErrProtocol
		}
		if j.state.Truncated[id] {
			return nil
		}
		if oldLog+int64(len(log.Content)) > maxAttemptLogBytes {
			log.Content = []byte("\n[平台提示：构建日志已截断，关键状态继续上报]\n")
			log.Truncated = true
			j.state.Truncated[id] = true
		} else {
			j.state.LogBytes[id] += int64(len(log.Content))
		}
	}
	// 运行日志与构建日志同构：单块上限一致，流预算耗尽后截断并继续上报，不阻塞关键状态。
	if log := message.GetRuntimeLog(); log != nil {
		if len(log.Content) > workerprotocol.RuntimeLogMaxChunkBytes {
			return workerprotocol.ErrProtocol
		}
		if j.state.Truncated[id] {
			return nil
		}
		if oldLog+int64(len(log.Content)) > maxRuntimeLogStreamBytes {
			log.Content = []byte("\n[平台提示：运行日志已截断，恢复连接后按控制面配额续传]\n")
			log.Truncated = true
			j.state.Truncated[id] = true
		} else {
			j.state.LogBytes[id] += int64(len(log.Content))
		}
	}
	// 采样没有独立序号字段，采集毫秒时间充当 journal 序号；0 不是合法采集时间。
	if channel == "runtime-metric" && seq <= 0 {
		return workerprotocol.ErrProtocol
	}
	oldSeq := j.state.Sequences[key]
	if seq == 0 {
		seq = oldSeq + 1
		setEventSequence(message, seq)
	}
	payload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(message)
	if err != nil {
		return err
	}
	if seq <= oldSeq {
		for _, entry := range j.state.Entries {
			if entry.Channel == channel && entry.ExecutionID == id && entry.Seq == seq && bytes.Equal(entry.Payload, payload) {
				j.state.LogBytes[id], j.state.Truncated[id] = oldLog, oldTruncated
				return nil
			}
		}
		j.state.LogBytes[id], j.state.Truncated[id] = oldLog, oldTruncated
		return workerprotocol.ErrProtocol
	}
	// 采样间隔不固定，允许跳号；只拒绝时间回退与同键异值（上面的 seq<=oldSeq 分支）。
	if channel != "runtime-metric" && seq != oldSeq+1 {
		j.state.LogBytes[id], j.state.Truncated[id] = oldLog, oldTruncated
		return workerprotocol.ErrProtocol
	}
	entry := journalEntry{Channel: channel, ExecutionID: id, Seq: seq, Payload: payload}
	j.state.Entries = append(j.state.Entries, entry)
	j.state.Sequences[key] = seq
	// 给关键状态保留1MiB空间，日志满时不能阻塞成功/停止事实的持久发送。
	if channel == "build-log" || channel == "runtime-log" {
		data, _ := json.Marshal(j.state)
		if int64(len(data)) > j.maxBytes-(1<<20) {
			j.state.Entries = j.state.Entries[:len(j.state.Entries)-1]
			j.state.Sequences[key] = oldSeq
			j.state.LogBytes[id], j.state.Truncated[id] = oldLog, oldTruncated
			return workerprotocol.ErrSpoolFull
		}
	}
	if err := j.save(); err != nil {
		j.state.Entries = j.state.Entries[:len(j.state.Entries)-1]
		j.state.Sequences[key] = oldSeq
		j.state.LogBytes[id], j.state.Truncated[id] = oldLog, oldTruncated
		return err
	}
	return nil
}
func (j *Journal) ack(value *platformv1.WorkerAck) error {
	if value == nil || value.Seq < 1 {
		return workerprotocol.ErrProtocol
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	index := -1
	for i, entry := range j.state.Entries {
		if entry.Channel == value.Channel && entry.ExecutionID == value.ExecutionId && entry.Seq == value.Seq {
			index = i
			break
		}
	}
	if index < 0 {
		return nil
	}
	old := j.state.Entries
	j.state.Entries = append(append([]journalEntry(nil), old[:index]...), old[index+1:]...)
	// 运行日志流长期存活：条目全部确认后清零流预算记账，避免一次溢出把该流永久截断。
	// 记账与删除同一次落盘，崩溃后重载不会残留过期的截断标记。
	oldBytes, oldTruncated := j.state.LogBytes[value.ExecutionId], j.state.Truncated[value.ExecutionId]
	drained := value.Channel == "runtime-log"
	if drained {
		for _, entry := range j.state.Entries {
			if entry.Channel == value.Channel && entry.ExecutionID == value.ExecutionId {
				drained = false
				break
			}
		}
	}
	if drained {
		delete(j.state.LogBytes, value.ExecutionId)
		delete(j.state.Truncated, value.ExecutionId)
	}
	if err := j.save(); err != nil {
		j.state.Entries = old
		if drained {
			j.state.LogBytes[value.ExecutionId], j.state.Truncated[value.ExecutionId] = oldBytes, oldTruncated
		}
		return err
	}
	return nil
}
func (j *Journal) pending() ([]*platformv1.WorkerEnvelope, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	result := make([]*platformv1.WorkerEnvelope, 0, len(j.state.Entries))
	for _, entry := range j.state.Entries {
		var message platformv1.WorkerEnvelope
		if err := proto.Unmarshal(entry.Payload, &message); err != nil {
			return nil, err
		}
		result = append(result, &message)
	}
	return result, nil
}
func (j *Journal) begin(key, fingerprint string) (bool, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if value, ok := j.state.Executions[key]; ok {
		if value.Fingerprint != fingerprint {
			return false, false, workerprotocol.ErrProtocol
		}
		return true, value.Done, nil
	}
	if len(j.state.Executions) >= 1024 {
		return false, false, workerprotocol.ErrSpoolFull
	}
	j.state.Executions[key] = journalExecution{Fingerprint: fingerprint}
	if err := j.save(); err != nil {
		delete(j.state.Executions, key)
		return false, false, err
	}
	return false, false, nil
}
func (j *Journal) done(key string) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	value, ok := j.state.Executions[key]
	if !ok {
		return workerprotocol.ErrProtocol
	}
	old := value
	value.Done = true
	j.state.Executions[key] = value
	if err := j.save(); err != nil {
		j.state.Executions[key] = old
		return err
	}
	return nil
}
func (j *Journal) generation(app string, generation int64) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	old := j.state.Generations[app]
	if generation < old {
		return workerprotocol.ErrIdentity
	}
	if generation == old {
		return nil
	}
	j.state.Generations[app] = generation
	if err := j.save(); err != nil {
		j.state.Generations[app] = old
		return err
	}
	return nil
}

// 指纹只覆盖跨重投不可变字段；源码短期令牌、Job UID 可能变化。部署的
// action 不参与指纹：同一操作的原命令与观察恢复（RECONCILE）命令在此语义
// 上是同一执行，集群侧已写入的注解指纹按此口径核对。
// 凭据与明文秘密不写 journal，也不把低熵秘密的摘要持久化。
func buildFingerprint(value *platformv1.WorkerBuildCommand) (string, error) {
	frozen := &platformv1.WorkerBuildCommand{BuildId: value.BuildId, AttemptId: value.AttemptId, WorkspaceId: value.WorkspaceId, ApplicationId: value.ApplicationId, NodeId: value.NodeId, WorkerId: value.WorkerId, StableJobName: value.StableJobName, SourceSnapshotId: value.SourceSnapshotId, SourceSha256: value.SourceSha256, Dockerfile: value.Dockerfile, ContextPath: value.ContextPath, Target: value.Target, BuildArgs: value.BuildArgs, Repository: value.Repository, Tag: value.Tag, Platform: value.Platform, DeadlineUnix: value.DeadlineUnix}
	return fingerprintMessage(frozen)
}

func deploymentFingerprint(value *platformv1.WorkerDeploymentCommand) (string, error) {
	frozen := &platformv1.WorkerDeploymentCommand{WorkspaceId: value.WorkspaceId, ApplicationId: value.ApplicationId, OperationId: value.OperationId, ReleaseId: value.ReleaseId, Generation: value.Generation, NodeId: value.NodeId, WorkerId: value.WorkerId, ClusterNodeUid: value.ClusterNodeUid, Repository: value.Repository, Digest: value.Digest, Platform: value.Platform, RestartMarker: value.RestartMarker, Config: value.Config}
	return fingerprintMessage(frozen)
}

func fingerprintMessage(value proto.Message) (string, error) {
	data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(value)
	if err != nil {
		return "", workerprotocol.ErrProtocol
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}
