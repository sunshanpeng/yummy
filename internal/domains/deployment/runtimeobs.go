package deployment

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/store"
	"github.com/sunshanpeng/yummy/internal/lib/security"
	"github.com/sunshanpeng/yummy/internal/lib/workerprotocol"
	"gorm.io/gorm"
)

const (
	// runtimeLogQuotaBytes 每应用运行日志保留字节预算；写入时收敛删除最旧数据，不建后台清理服务。
	runtimeLogQuotaBytes int64 = 4 << 20
	// runtimeLogQuotaChunks 每应用运行日志块数上限；两个配额同时生效，超出的最旧行被删除。
	runtimeLogQuotaChunks = 2000
	// runtimeMetricRetention 基础监控采样保留窗口；超过窗口的历史采样在写入事务内删除。
	runtimeMetricRetention = 24 * time.Hour
	// runtimeMetricMaxFutureSkew 允许采样时间相对数据库时钟的超前偏差，吸收 worker 与数据库的时钟漂移。
	runtimeMetricMaxFutureSkew = 30 * time.Second
	// runtimeMetricQueryLimit 单次监控查询的采样点安全上限；保留窗口本身有界，防止异常数据撑爆响应。
	runtimeMetricQueryLimit = 10000
)

// runtimeLogCursor 把 (created_at, id) 键集游标编码为一个有界字符串。
func runtimeLogCursor(value runtimeLogChunkRecord) string {
	return strconv.FormatInt(value.CreatedAt.UnixNano(), 10) + "|" + value.ID
}

func splitRuntimeLogCursor(value string) (time.Time, string, bool) {
	index := strings.IndexByte(value, '|')
	if index <= 0 || index == len(value)-1 {
		return time.Time{}, "", false
	}
	nanos, err := strconv.ParseInt(value[:index], 10, 64)
	if err != nil {
		return time.Time{}, "", false
	}
	return time.Unix(0, nanos).UTC(), value[index+1:], true
}

// alignRuntimeObservation 校验 worker 标签与当前期望代际、发布及冻结运行位置一致。
// 运行日志/采样沿用的授权模型与 ApplyObservation 相同：只有被放置到该应用的已认证
// worker 可以为它上报；标签必须对齐当前期望代际，旧 Pod 的数据也按当前操作标签上报。
func alignRuntimeObservation(tx *gorm.DB, worker WorkerIdentity, ws, app, operationID, releaseID string, generation int64) error {
	state, err := readState(tx, ws, app)
	if err != nil {
		return err
	}
	if state.Generation != generation || text(state.DesiredReleaseID) != releaseID {
		return ErrStaleObservation
	}
	release, err := readRelease(tx, ws, app, releaseID)
	if err != nil {
		return err
	}
	placement := release.PlacementSnapshot
	if placement.NodeID != worker.NodeID || placement.WorkerID != worker.WorkerID {
		return authorization.ErrDenied
	}
	var node nodeRecord
	if err = tx.First(&node, "id = ?", worker.NodeID).Error; err != nil {
		return err
	}
	if node.ClusterNodeUID != placement.ClusterNodeUID {
		return authorization.ErrDenied
	}
	if operationID == "" {
		return nil
	}
	var op operationRecord
	if err = tx.First(&op, "workspace_id = ? AND application_id = ? AND id = ?", ws, app, operationID).Error; err != nil {
		return missing(err)
	}
	if op.ReleaseID != releaseID || op.Generation != generation {
		return ErrStaleObservation
	}
	return nil
}

// convergeRuntimeLogs 在写入事务内按 (created_at DESC, id DESC) 只保留最新的
// runtimeLogQuotaChunks 块、且自新到旧累计字节不超过 runtimeLogQuotaBytes，其余旧行删除。
func convergeRuntimeLogs(tx *gorm.DB, ws, app string) error {
	// 【数据库专有】数据修改 CTE（WITH...DELETE）与 octet_length 为 PostgreSQL 专有；MySQL 8
	// 窗口函数可用但需拆成先 SELECT 再 DELETE 两步。
	return tx.Exec(`WITH ranked AS (
    SELECT id,
           ROW_NUMBER() OVER (ORDER BY created_at DESC, id DESC) AS rn,
           SUM(octet_length(content)) OVER (ORDER BY created_at DESC, id DESC ROWS BETWEEN UNBOUNDED PRECEDING AND CURRENT ROW) AS total
    FROM runtime_log_chunks
    WHERE workspace_id = ? AND application_id = ?
)
DELETE FROM runtime_log_chunks WHERE id IN (SELECT id FROM ranked WHERE rn > ? OR total > ?)`, ws, app, runtimeLogQuotaChunks, runtimeLogQuotaBytes).Error
}

// convergeRuntimeMetrics 在写入事务内删除超过 runtimeMetricRetention 的历史采样。
func convergeRuntimeMetrics(tx *gorm.DB, ws, app string) error {
	// 【数据库专有】make_interval(secs => ?) 为 PostgreSQL 专有；MySQL 用 collected_at < DATE_SUB(NOW(), INTERVAL ? SECOND)。
	return tx.Exec(`DELETE FROM runtime_metric_samples WHERE workspace_id = ? AND application_id = ? AND collected_at < NOW() - make_interval(secs => ?)`, ws, app, int64(runtimeMetricRetention/time.Second)).Error
}

// runtimeLogStream 是一个容器实例的日志流身份：seq 只在该流内要求连续。
type runtimeLogStream struct {
	ws, app, pod, container string
	restart                 int32
}

func (s runtimeLogStream) key() string {
	return "workspace_id = ? AND application_id = ? AND pod_uid = ? AND container = ? AND restart_count = ?"
}

func (s runtimeLogStream) args() []any {
	return []any{s.ws, s.app, s.pod, s.container, s.restart}
}

// ApplyRuntimeLog 持久化一个容器运行日志块。seq 在 (pod, container, restart) 流内连续：
// 同键同 hash 幂等 ack，同键不同 hash 或断号/回退按 ErrSequence 拒绝；插入与配额收敛同事务。
func (s *Service) ApplyRuntimeLog(ctx context.Context, worker WorkerIdentity, input RuntimeLogInput) (Ack, error) {
	if input.WorkspaceID == "" || input.ApplicationID == "" || input.Generation < 1 || input.Seq < 1 || input.RestartCount < 0 || input.PodUID == "" || len(input.PodUID) > 64 || input.Container == "" || len(input.Container) > 128 || len(input.OperationID) > 128 || len(input.ReleaseID) > 128 || len(input.Content) > workerprotocol.RuntimeLogMaxChunkBytes {
		return Ack{}, authorization.ErrInvalid
	}
	hash := fingerprint([]any{input.Content, input.Truncated})
	var ack Ack
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := authenticateWorker(tx, worker); err != nil {
			return err
		}
		if err := lockApplication(tx, input.WorkspaceID, input.ApplicationID); err != nil {
			return err
		}
		if err := alignRuntimeObservation(tx, worker, input.WorkspaceID, input.ApplicationID, input.OperationID, input.ReleaseID, input.Generation); err != nil {
			return err
		}
		stream := runtimeLogStream{input.WorkspaceID, input.ApplicationID, input.PodUID, input.Container, input.RestartCount}
		var old runtimeLogChunkRecord
		// 本仓库工具链不接受“两个固定实参后跟切片展开”，统一改为 Where(...).Take。
		err := tx.Where(stream.key()+" AND seq = ?", append(stream.args(), input.Seq)...).Take(&old).Error
		if err == nil {
			if old.PayloadHash != hash {
				return ErrSequence
			}
			ack = Ack{Seq: input.Seq, Duplicate: true}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		var latest struct{ Seq int64 }
		if err = tx.Table("runtime_log_chunks").Select("COALESCE(MAX(seq), 0) AS seq").Where(stream.key(), stream.args()...).Take(&latest).Error; err != nil {
			return err
		}
		if input.Seq != latest.Seq+1 {
			return ErrSequence
		}
		id, err := security.NewID("rlg")
		if err != nil {
			return err
		}
		// created_at 由数据库时钟默认值写入，不使用宿主机 time.Now()。
		chunk := runtimeLogChunkRecord{ID: id, WorkspaceID: input.WorkspaceID, ApplicationID: input.ApplicationID, PodUID: input.PodUID, Container: input.Container, RestartCount: input.RestartCount, Seq: input.Seq, PayloadHash: hash, Content: append([]byte(nil), input.Content...), Truncated: input.Truncated}
		if err = tx.Create(&chunk).Error; err != nil {
			return err
		}
		return convergeRuntimeLogs(tx, input.WorkspaceID, input.ApplicationID)
	})
	return ack, err
}

// ApplyRuntimeMetric 持久化一次 CPU/内存采样。幂等键是 (pod_uid, collected_at)：
// 同键同 hash 幂等 ack（ack.Seq 携带采集毫秒时间，供 worker journal 对账），
// 同键不同 hash 冲突；新鲜度窗口用数据库时钟校验，保留窗口收敛与插入同事务。
func (s *Service) ApplyRuntimeMetric(ctx context.Context, worker WorkerIdentity, input RuntimeMetricInput) (Ack, error) {
	if input.WorkspaceID == "" || input.ApplicationID == "" || input.Generation < 1 || input.ReleaseID == "" || len(input.ReleaseID) > 128 || input.PodUID == "" || len(input.PodUID) > 128 || input.CPUNanoCores < 0 || input.CPUWindowMillis <= 0 || input.MemoryWorkingSetBytes < 0 || input.CollectedAt.IsZero() {
		return Ack{}, authorization.ErrInvalid
	}
	hash := fingerprint([]any{input.CPUNanoCores, input.CPUWindowMillis, input.MemoryWorkingSetBytes})
	var ack Ack
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := authenticateWorker(tx, worker); err != nil {
			return err
		}
		if err := lockApplication(tx, input.WorkspaceID, input.ApplicationID); err != nil {
			return err
		}
		// 采样新鲜度只与数据库时钟比较；过期采样直接拒绝，不落库后再清理。
		var window struct {
			Future  bool
			Expired bool
		}
		// 【数据库专有】?::timestamptz 强转与 make_interval 为 PostgreSQL 专有；MySQL 用 CAST(? AS DATETIME(6)) 与 DATE_SUB/DATE_ADD。
		if err := tx.Raw(`SELECT ?::timestamptz > NOW() + make_interval(secs => ?) AS future, ?::timestamptz < NOW() - make_interval(secs => ?) AS expired`, input.CollectedAt, int64(runtimeMetricMaxFutureSkew/time.Second), input.CollectedAt, int64(runtimeMetricRetention/time.Second)).Scan(&window).Error; err != nil {
			return err
		}
		if window.Future {
			return invalid("采样时间超前于数据库时钟")
		}
		if window.Expired {
			return ErrStaleObservation
		}
		if err := alignRuntimeObservation(tx, worker, input.WorkspaceID, input.ApplicationID, "", input.ReleaseID, input.Generation); err != nil {
			return err
		}
		var old runtimeMetricSampleRecord
		err := tx.First(&old, "pod_uid = ? AND collected_at = ?", input.PodUID, input.CollectedAt).Error
		if err == nil {
			if old.WorkspaceID != input.WorkspaceID || old.ApplicationID != input.ApplicationID || old.PayloadHash != hash {
				return ErrSequence
			}
			ack = Ack{Seq: input.CollectedAt.UnixMilli(), Duplicate: true}
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		id, err := security.NewID("rms")
		if err != nil {
			return err
		}
		sample := runtimeMetricSampleRecord{ID: id, WorkspaceID: input.WorkspaceID, ApplicationID: input.ApplicationID, ReleaseID: input.ReleaseID, Generation: input.Generation, PodUID: input.PodUID, CPUNanoCores: input.CPUNanoCores, CPUWindowMillis: input.CPUWindowMillis, MemoryWorkingSetBytes: input.MemoryWorkingSetBytes, PayloadHash: hash, CollectedAt: input.CollectedAt}
		if err = tx.Create(&sample).Error; err != nil {
			return err
		}
		// ack.Seq 携带采集毫秒时间，与 worker journal 的采样序号对账。
		ack = Ack{Seq: input.CollectedAt.UnixMilli()}
		return convergeRuntimeMetrics(tx, input.WorkspaceID, input.ApplicationID)
	})
	return ack, err
}

// ListRuntimeLogs 按 (created_at, id) 时间正序分页返回运行日志块，可按 pod/container 过滤。
// 授权走 access.Do + application:read；游标绑定查询作用域，跨应用或改参的游标无效。
func (s *Service) ListRuntimeLogs(ctx context.Context, p authorization.Principal, ws, app string, limit int, cursor, podUID, container string) (RuntimeLogPage, error) {
	result := RuntimeLogPage{Items: []RuntimeLog{}}
	if len(podUID) > 64 || len(container) > 128 {
		return result, authorization.ErrInvalid
	}
	scope := "runtime-logs:" + ws + ":" + app + ":" + p.UserID + ":" + p.TokenID + ":" + podUID + ":" + container
	limit, after, err := store.ParsePage(limit, cursor, scope)
	if err != nil {
		return result, authorization.ErrInvalid
	}
	err = s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, authorization.ApplicationRead); err != nil {
			return err
		}
		query := tx.Where("workspace_id = ? AND application_id = ?", ws, app)
		if podUID != "" {
			query = query.Where("pod_uid = ?", podUID)
		}
		if container != "" {
			query = query.Where("container = ?", container)
		}
		if after != "" {
			stamp, id, ok := splitRuntimeLogCursor(after)
			if !ok {
				return authorization.ErrInvalid
			}
			query = query.Where("created_at > ? OR (created_at = ? AND id > ?)", stamp, stamp, id)
		}
		var rows []runtimeLogChunkRecord
		if err := query.Order("created_at ASC, id ASC").Limit(limit + 1).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) > limit {
			result.NextCursor = store.NextCursor(scope, runtimeLogCursor(rows[limit-1]))
			rows = rows[:limit]
		}
		for _, row := range rows {
			result.Items = append(result.Items, RuntimeLog{ID: row.ID, WorkspaceID: row.WorkspaceID, ApplicationID: row.ApplicationID, PodUID: row.PodUID, Container: row.Container, RestartCount: row.RestartCount, Seq: row.Seq, Content: row.Content, Truncated: row.Truncated, CreatedAt: row.CreatedAt})
		}
		return nil
	})
	return result, err
}

// ListRuntimeMetrics 返回保留窗口内按采集时间升序的采样点；since/until 为零值时表示不设界。
func (s *Service) ListRuntimeMetrics(ctx context.Context, p authorization.Principal, ws, app string, since, until time.Time) ([]RuntimeMetricSample, error) {
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		return nil, authorization.ErrInvalid
	}
	var rows []runtimeMetricSampleRecord
	err := s.access.Do(ctx, p, false, func(tx *gorm.DB, current authorization.Principal) error {
		if _, _, err := s.access.Application(tx, current, ws, app, authorization.ApplicationRead); err != nil {
			return err
		}
		query := tx.Where("workspace_id = ? AND application_id = ?", ws, app)
		if !since.IsZero() {
			query = query.Where("collected_at >= ?", since)
		}
		if !until.IsZero() {
			query = query.Where("collected_at <= ?", until)
		}
		// 只取最新一段再反转为升序：窗口截断时优先保留最新采样，符合监控图表语义。
		if err := query.Order("collected_at DESC, id DESC").Limit(runtimeMetricQueryLimit).Find(&rows).Error; err != nil {
			return err
		}
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result := make([]RuntimeMetricSample, 0, len(rows))
	for _, row := range rows {
		result = append(result, RuntimeMetricSample{ID: row.ID, WorkspaceID: row.WorkspaceID, ApplicationID: row.ApplicationID, ReleaseID: row.ReleaseID, Generation: row.Generation, PodUID: row.PodUID, CPUNanoCores: row.CPUNanoCores, CPUWindowMillis: row.CPUWindowMillis, MemoryWorkingSetBytes: row.MemoryWorkingSetBytes, CollectedAt: row.CollectedAt})
	}
	return result, nil
}
