package deployment

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/platform/authorization"
)

func createOn(t *testing.T, e environment, app, key string) Result {
	t.Helper()
	input := createInput(key)
	input.ArtifactID = "artifact_" + app
	r, err := e.service.CreateDeployment(context.Background(), e.root, "space", app, input)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func runtimeLogInput(r Result, app, pod string, seq int64, content string) RuntimeLogInput {
	return RuntimeLogInput{WorkspaceID: "space", ApplicationID: app, OperationID: r.Operation.ID, ReleaseID: r.Release.ID, Generation: r.Operation.Generation, PodUID: pod, Container: "app", RestartCount: 0, Seq: seq, Content: []byte(content)}
}

func runtimeMetricInput(r Result, app, pod string, collected time.Time) RuntimeMetricInput {
	return RuntimeMetricInput{WorkspaceID: "space", ApplicationID: app, ReleaseID: r.Release.ID, Generation: r.Operation.Generation, PodUID: pod, CPUNanoCores: 1000, CPUWindowMillis: 1000, MemoryWorkingSetBytes: 2048, CollectedAt: collected}
}

func dbNow(t *testing.T, e environment) time.Time {
	t.Helper()
	var now time.Time
	if err := e.db.Raw("SELECT NOW()").Scan(&now).Error; err != nil {
		t.Fatal(err)
	}
	return now
}

func countAppRows(t *testing.T, e environment, table, app string) int64 {
	t.Helper()
	var count int64
	if err := e.db.Table(table).Where("workspace_id = ? AND application_id = ?", "space", app).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count
}

func rowExists(t *testing.T, e environment, table, id string) bool {
	t.Helper()
	var count int64
	if err := e.db.Table(table).Where("id = ?", id).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	return count == 1
}

// seedRuntimeLogs 用数据库时钟错开 created_at，保证列表分页顺序确定；prefix 使 id 不跨应用冲突。
func seedRuntimeLogs(t *testing.T, e environment, app, pod, prefix string, count, size, ageBaseSeconds int) {
	t.Helper()
	mustExec(t, e.db, `INSERT INTO runtime_log_chunks (id, workspace_id, application_id, pod_uid, container, restart_count, seq, payload_hash, content, truncated, created_at)
SELECT 'seed_'||?||'_'||g, 'space', ?, ?, ?, 0, g, ?, repeat('x', ?)::bytea, FALSE, NOW() - make_interval(secs => ? - g)
FROM generate_series(1, ?) g`, prefix, app, pod, "app", strings.Repeat("a", 64), size, ageBaseSeconds, count)
}

func seedRuntimeMetric(t *testing.T, e environment, app, release, id, pod string, ageSeconds int) {
	t.Helper()
	mustExec(t, e.db, `INSERT INTO runtime_metric_samples (id, workspace_id, application_id, release_id, generation, pod_uid, cpu_nano_cores, cpu_window_millis, memory_working_set_bytes, payload_hash, collected_at)
VALUES (?, 'space', ?, ?, 1, ?, 1, 1000, 1, ?, NOW() - make_interval(secs => ?))`, id, app, release, pod, strings.Repeat("b", 64), ageSeconds)
}

func TestApplyRuntimeLogIdempotencySequenceAndAccess(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	r := create(t, e, "runtime-log")
	if _, err := e.service.ApplyRuntimeLog(ctx, e.worker, runtimeLogInput(r, "app_one", "pod-1", 1, "first")); err != nil {
		t.Fatal(err)
	}
	ack, err := e.service.ApplyRuntimeLog(ctx, e.worker, runtimeLogInput(r, "app_one", "pod-1", 1, "first"))
	if err != nil || !ack.Duplicate || ack.Seq != 1 {
		t.Fatalf("相同内容重放应幂等: %+v %v", ack, err)
	}
	if _, err := e.service.ApplyRuntimeLog(ctx, e.worker, runtimeLogInput(r, "app_one", "pod-1", 1, "changed")); !errors.Is(err, ErrSequence) {
		t.Fatalf("同键不同内容应冲突: %v", err)
	}
	if _, err := e.service.ApplyRuntimeLog(ctx, e.worker, runtimeLogInput(r, "app_one", "pod-1", 3, "gap")); !errors.Is(err, ErrSequence) {
		t.Fatalf("断号应拒绝: %v", err)
	}
	if _, err := e.service.ApplyRuntimeLog(ctx, e.worker, runtimeLogInput(r, "app_one", "pod-1", 2, "second")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.ApplyRuntimeLog(ctx, e.worker, runtimeLogInput(r, "app_one", "pod-2", 1, "other-pod")); err != nil {
		t.Fatal(err)
	}
	if count := countAppRows(t, e, "runtime_log_chunks", "app_one"); count != 3 {
		t.Fatalf("块数=%d", count)
	}
	// 大小上限与代际标签校验。
	tooBig := runtimeLogInput(r, "app_one", "pod-1", 3, "")
	tooBig.Content = make([]byte, RuntimeLogMaxChunkBytes+1)
	if _, err := e.service.ApplyRuntimeLog(ctx, e.worker, tooBig); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("超大块应无效: %v", err)
	}
	stale := runtimeLogInput(r, "app_one", "pod-1", 3, "stale")
	stale.Generation++
	if _, err := e.service.ApplyRuntimeLog(ctx, e.worker, stale); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("旧代际应拒绝: %v", err)
	}
	// 未部署的 app_two 没有运行位置：worker 标签对不上 placement 应拒绝。
	if _, err := e.service.ApplyRuntimeLog(ctx, e.worker, runtimeLogInput(r, "app_two", "pod-9", 1, "no-placement")); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("无发布应用应拒绝: %v", err)
	}
	if _, err := e.service.ApplyRuntimeLog(ctx, WorkerIdentity{NodeID: e.node.ID, WorkerID: e.node.WorkerID, CertificateSerial: "ghost", CertificateSHA256: strings.Repeat("1", 64)}, runtimeLogInput(r, "app_one", "pod-1", 3, "ghost")); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("未知证书应拒绝: %v", err)
	}
}

func TestApplyRuntimeLogQuotaConvergenceRemovesOldest(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	// 块数配额：2000 块之外最旧的被删除。
	rCount := create(t, e, "quota-count")
	seedRuntimeLogs(t, e, "app_one", "pod-seed", "count", runtimeLogQuotaChunks, 100, 100000)
	if _, err := e.service.ApplyRuntimeLog(ctx, e.worker, runtimeLogInput(rCount, "app_one", "pod-new", 1, "newest")); err != nil {
		t.Fatal(err)
	}
	if count := countAppRows(t, e, "runtime_log_chunks", "app_one"); count != int64(runtimeLogQuotaChunks) {
		t.Fatalf("块数配额未收敛=%d", count)
	}
	if rowExists(t, e, "runtime_log_chunks", "seed_count_1") {
		t.Fatal("最旧的块未被删除")
	}
	if !rowExists(t, e, "runtime_log_chunks", "seed_count_2") || !rowExists(t, e, "runtime_log_chunks", "seed_count_2000") {
		t.Fatal("不应删除配额内的块")
	}
	// 字节配额：4MiB/16KiB+1 = 257 块超出预算，收敛到 256 块（含最新写入的一块）。
	rBytes := createOn(t, e, "app_two", "quota-bytes")
	seedRuntimeLogs(t, e, "app_two", "pod-big", "bytes", int(runtimeLogQuotaBytes/RuntimeLogMaxChunkBytes)+1, RuntimeLogMaxChunkBytes, 100000)
	if _, err := e.service.ApplyRuntimeLog(ctx, e.worker, runtimeLogInput(rBytes, "app_two", "pod-new", 1, "newest")); err != nil {
		t.Fatal(err)
	}
	if count := countAppRows(t, e, "runtime_log_chunks", "app_two"); count != int64(runtimeLogQuotaBytes/RuntimeLogMaxChunkBytes) {
		t.Fatalf("字节配额未收敛=%d", count)
	}
	if rowExists(t, e, "runtime_log_chunks", "seed_bytes_1") || rowExists(t, e, "runtime_log_chunks", "seed_bytes_2") {
		t.Fatal("字节配额应删除最旧的块")
	}
	if !rowExists(t, e, "runtime_log_chunks", "seed_bytes_3") {
		t.Fatal("配额内的块被误删")
	}
}

func TestApplyRuntimeMetricIdempotencyFreshnessAndRetention(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	r := create(t, e, "runtime-metric")
	// 时间一律锚定数据库时钟，避免宿主机偏差影响窗口断言。
	now := dbNow(t, e)
	input := runtimeMetricInput(r, "app_one", "pod-1", now.Add(-time.Second))
	ack, err := e.service.ApplyRuntimeMetric(ctx, e.worker, input)
	if err != nil || ack.Duplicate || ack.Seq != input.CollectedAt.UnixMilli() {
		t.Fatalf("采样应受理: %+v %v", ack, err)
	}
	dup, err := e.service.ApplyRuntimeMetric(ctx, e.worker, input)
	if err != nil || !dup.Duplicate {
		t.Fatalf("同键同值重放应幂等: %+v %v", dup, err)
	}
	conflict := input
	conflict.CPUNanoCores = 2000
	if _, err := e.service.ApplyRuntimeMetric(ctx, e.worker, conflict); !errors.Is(err, ErrSequence) {
		t.Fatalf("同键不同值应冲突: %v", err)
	}
	future := input
	future.PodUID = "pod-future"
	future.CollectedAt = now.Add(time.Hour)
	if _, err := e.service.ApplyRuntimeMetric(ctx, e.worker, future); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("超前采样应无效: %v", err)
	}
	stale := input
	stale.PodUID = "pod-stale"
	stale.CollectedAt = now.Add(-runtimeMetricRetention - time.Hour)
	if _, err := e.service.ApplyRuntimeMetric(ctx, e.worker, stale); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("过期采样应拒绝: %v", err)
	}
	staleLabel := input
	staleLabel.PodUID = "pod-label"
	staleLabel.Generation++
	if _, err := e.service.ApplyRuntimeMetric(ctx, e.worker, staleLabel); !errors.Is(err, ErrStaleObservation) {
		t.Fatalf("旧代际采样应拒绝: %v", err)
	}
	// 保留窗口收敛：超过 24h 的历史采样在写入事务内删除。
	seedRuntimeMetric(t, e, "app_one", r.Release.ID, "seed_old", "pod-old", int(runtimeMetricRetention/time.Second)+3600)
	keep := input
	keep.PodUID = "pod-keep"
	keep.CollectedAt = now.Add(-2 * time.Second)
	if _, err := e.service.ApplyRuntimeMetric(ctx, e.worker, keep); err != nil {
		t.Fatal(err)
	}
	if rowExists(t, e, "runtime_metric_samples", "seed_old") {
		t.Fatal("超过保留窗口的采样未被删除")
	}
	var kept int64
	if err := e.db.Table("runtime_metric_samples").Where("pod_uid = ?", keep.PodUID).Count(&kept).Error; err != nil || kept != 1 {
		t.Fatalf("窗口内采样被误删: %d %v", kept, err)
	}
}

func TestListRuntimeLogsPaginationFilterAndAccess(t *testing.T) {
	e := setup(t)
	seedRuntimeLogs(t, e, "app_one", "pod-list", "list", 5, 64, 100000)
	ctx := context.Background()
	viewer := authorization.Principal{UserID: "viewer", AuthVersion: 1}
	page, err := e.service.ListRuntimeLogs(ctx, e.root, "space", "app_one", 2, "", "", "")
	if err != nil || len(page.Items) != 2 || page.Items[0].Seq != 1 || page.Items[1].Seq != 2 || page.NextCursor == "" {
		t.Fatalf("第一页错误: %+v %v", page, err)
	}
	second, err := e.service.ListRuntimeLogs(ctx, e.root, "space", "app_one", 2, page.NextCursor, "", "")
	if err != nil || len(second.Items) != 2 || second.Items[0].Seq != 3 || second.Items[1].Seq != 4 || second.NextCursor == "" {
		t.Fatalf("第二页错误: %+v %v", second, err)
	}
	third, err := e.service.ListRuntimeLogs(ctx, e.root, "space", "app_one", 2, second.NextCursor, "", "")
	if err != nil || len(third.Items) != 1 || third.Items[0].Seq != 5 || third.NextCursor != "" {
		t.Fatalf("尾页错误: %+v %v", third, err)
	}
	filtered, err := e.service.ListRuntimeLogs(ctx, e.root, "space", "app_one", 10, "", "pod-list", "app")
	if err != nil || len(filtered.Items) != 5 {
		t.Fatalf("pod/container 过滤错误: %+v %v", filtered, err)
	}
	empty, err := e.service.ListRuntimeLogs(ctx, e.root, "space", "app_one", 10, "", "pod-none", "")
	if err != nil || len(empty.Items) != 0 {
		t.Fatalf("空过滤应返回空: %+v %v", empty, err)
	}
	// 游标绑定过滤作用域：换过滤条件的游标无效。
	if _, err := e.service.ListRuntimeLogs(ctx, e.root, "space", "app_one", 2, page.NextCursor, "pod-list", ""); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("跨作用域游标应无效: %v", err)
	}
	// viewer 是 app_one 成员（viewer 角色，含 application:read）可读；非成员应用拒绝。
	if _, err := e.service.ListRuntimeLogs(ctx, viewer, "space", "app_one", 10, "", "", ""); err != nil {
		t.Fatalf("应用成员应可读: %v", err)
	}
	if _, err := e.service.ListRuntimeLogs(ctx, viewer, "space", "app_two", 10, "", "", ""); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("跨应用应拒绝: %v", err)
	}
	if _, err := e.service.ListRuntimeLogs(ctx, authorization.Principal{UserID: "ghost", AuthVersion: 1}, "space", "app_one", 10, "", "", ""); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("未知身份应拒绝: %v", err)
	}
}

func TestListRuntimeMetricsWindowAndAccess(t *testing.T) {
	e := setup(t)
	r := createOn(t, e, "app_two", "list-metrics")
	seedRuntimeMetric(t, e, "app_two", r.Release.ID, "m_1", "pod-m", 90000)
	seedRuntimeMetric(t, e, "app_two", r.Release.ID, "m_2", "pod-m", 80000)
	seedRuntimeMetric(t, e, "app_two", r.Release.ID, "m_3", "pod-m", 70000)
	ctx := context.Background()
	all, err := e.service.ListRuntimeMetrics(ctx, e.root, "space", "app_two", time.Time{}, time.Time{})
	if err != nil || len(all) != 3 || all[0].ID != "m_1" || all[2].ID != "m_3" {
		t.Fatalf("全量窗口应按采集时间升序: %+v %v", all, err)
	}
	// 窗口 [81k..69k 秒前] 应恰好包含 m_2 (80k) 与 m_3 (70k)，排除 m_1 (90k)。
	dbNowValue := dbNow(t, e)
	since, until := dbNowValue.Add(-81000*time.Second), dbNowValue.Add(-69000*time.Second)
	window, err := e.service.ListRuntimeMetrics(ctx, e.root, "space", "app_two", since, until)
	if err != nil || len(window) != 2 || window[0].ID != "m_2" || window[1].ID != "m_3" {
		t.Fatalf("since/until 窗口错误: %+v %v", window, err)
	}
	viewer := authorization.Principal{UserID: "viewer", AuthVersion: 1}
	if _, err := e.service.ListRuntimeMetrics(ctx, viewer, "space", "app_two", time.Time{}, time.Time{}); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("跨应用应拒绝: %v", err)
	}
	if _, err := e.service.ListRuntimeMetrics(ctx, e.root, "space", "app_two", until, since); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("窗口倒置应无效: %v", err)
	}
}

func TestClaimPendingSynthesizesObservationResume(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	r := create(t, e, "resume-1")
	finishHTTP(t, e, r)
	// 观察新鲜（刚完成发布）时不合成恢复命令。
	if _, err := e.service.ClaimPending(ctx, e.worker); !errors.Is(err, ErrNoPending) {
		t.Fatalf("新鲜观察不应合成恢复=%v", err)
	}
	// 观察停更超过阈值（worker 重启丢采集器的实机形态）：合成 RECONCILE，
	// 指向 serving 操作本身，观察从已持久化序号之后续传。
	mustExec(t, e.db, "UPDATE application_runtime_states SET last_observed_at = NOW() - INTERVAL '3 minutes' WHERE workspace_id='space' AND application_id='app_one'")
	dispatch, err := e.service.ClaimPending(ctx, e.worker)
	if err != nil {
		t.Fatal(err)
	}
	if dispatch.Action != Reconcile || dispatch.OperationID != r.Operation.ID || dispatch.ReleaseID != r.Release.ID || dispatch.Generation != r.Operation.Generation || dispatch.NextEventSeq != 2 {
		t.Fatalf("恢复命令字段不符: %+v", dispatch)
	}
	if dispatch.Config.View.Revision < 1 || dispatch.SpaceSlug != "space" || dispatch.AppName != "one" {
		t.Fatalf("恢复命令配置不完整: %+v", dispatch)
	}
	// 退避窗口内不重复合成，恢复完成前（LastObservedAt 刷新）的周期轮询安静。
	if _, err := e.service.ClaimPending(ctx, e.worker); !errors.Is(err, ErrNoPending) {
		t.Fatalf("退避窗口不应重复合成=%v", err)
	}
	// 停止意图（DesiredState=STOP）的应用绝不为它合成恢复。
	mustExec(t, e.db, "UPDATE application_runtime_states SET desired_state='STOP' WHERE workspace_id='space' AND application_id='app_one'")
	if _, err := e.service.ClaimPending(ctx, e.worker); !errors.Is(err, ErrNoPending) {
		t.Fatalf("停止意图不应合成恢复=%v", err)
	}
}
