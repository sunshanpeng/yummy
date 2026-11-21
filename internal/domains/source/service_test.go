package source

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sunshanpeng/yummy/internal/domains/authorization"
	"github.com/sunshanpeng/yummy/internal/infra/sourcearchive"
	"github.com/sunshanpeng/yummy/internal/infra/store/model"
	"github.com/sunshanpeng/yummy/internal/infra/testkit"
	"gorm.io/gorm"
)

type environment struct {
	db        *gorm.DB
	service   *Service
	access    *authorization.Access
	root      string
	developer authorization.Principal
	viewer    authorization.Principal
	admin     authorization.Principal
	data      []byte
	input     CreateInput
}

func setup(t *testing.T) *environment {
	t.Helper()
	db := testkit.OpenDatabase(t)
	values := []any{
		&model.User{ID: "admin", Username: "admin", PasswordHash: "hash", InstanceAdmin: true},
		&model.User{ID: "developer", Username: "developer", PasswordHash: "hash"},
		&model.User{ID: "viewer", Username: "viewer", PasswordHash: "hash"},
		&model.Workspace{ID: "space", Name: "空间", Slug: "space"},
		&model.Workspace{ID: "other", Name: "其他", Slug: "other"},
		&model.WorkspaceMember{WorkspaceID: "space", UserID: "developer", Role: "member"},
		&model.WorkspaceMember{WorkspaceID: "space", UserID: "viewer", Role: "member"},
		&model.Application{ID: "app", WorkspaceID: "space", Name: "app"},
		&model.Application{ID: "app-two", WorkspaceID: "space", Name: "app-two"},
		&model.Application{ID: "other-app", WorkspaceID: "other", Name: "other-app"},
		&model.ApplicationMember{ApplicationID: "app", WorkspaceID: "space", UserID: "developer", Role: "developer"},
		&model.ApplicationMember{ApplicationID: "app", WorkspaceID: "space", UserID: "viewer", Role: "viewer"},
	}
	for _, value := range values {
		if err := db.Create(value).Error; err != nil {
			t.Fatal(err)
		}
	}
	access, err := authorization.NewAccess(db)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	service, err := NewService(db, access, Config{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "Dockerfile"), []byte("FROM scratch\nCOPY hello /hello\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "hello"), []byte("hello"), 0755); err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	result, err := sourcearchive.Archive(context.Background(), &buffer, sourcearchive.Options{Root: directory, AllowRoots: []string{directory}})
	if err != nil {
		t.Fatal(err)
	}
	return &environment{db: db, service: service, access: access, root: root, developer: authorization.Principal{UserID: "developer", AuthVersion: 1}, viewer: authorization.Principal{UserID: "viewer", AuthVersion: 1}, admin: authorization.Principal{UserID: "admin", AuthVersion: 1}, data: buffer.Bytes(), input: CreateInput{Size: result.CompressedBytes, SHA256: result.SHA256}}
}

func (e *environment) create(t *testing.T) Upload {
	t.Helper()
	value, err := e.service.CreateUpload(context.Background(), e.developer, "space", "app", e.input)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func (e *environment) received(t *testing.T) Upload {
	t.Helper()
	value := e.create(t)
	value, err := e.service.Upload(context.Background(), e.developer, "space", "app", value.ID, bytes.NewReader(e.data))
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func (e *environment) ready(t *testing.T) Snapshot {
	t.Helper()
	upload := e.received(t)
	value, err := e.service.Complete(context.Background(), e.developer, "space", "app", upload.ID)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestUploadCompleteImmutableSnapshotAndAudit(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	upload := e.create(t)
	if upload.State != StatePending || upload.SnapshotID != "" || !upload.ExpiresAt.After(time.Now()) {
		t.Fatalf("创建事实错误: %+v", upload)
	}
	if _, err := e.service.Complete(ctx, e.developer, "space", "app", upload.ID); !errors.Is(err, ErrNotReady) {
		t.Fatalf("未上传就完成: %v", err)
	}
	if _, err := e.service.GetSnapshot(ctx, e.developer, "space", "app", "src_"+strings.TrimPrefix(upload.ID, "upl_")); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("未READY已可读取: %v", err)
	}
	received, err := e.service.Upload(ctx, e.developer, "space", "app", upload.ID, bytes.NewReader(e.data))
	if err != nil || received.State != StateUploaded {
		t.Fatalf("接收状态: %+v %v", received, err)
	}
	if _, err := e.service.Upload(ctx, e.developer, "space", "app", upload.ID, bytes.NewReader(e.data)); !errors.Is(err, authorization.ErrConflict) {
		t.Fatalf("允许覆盖已接收内容: %v", err)
	}
	completed, err := e.service.Complete(ctx, e.developer, "space", "app", upload.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != StateReady || completed.SHA256 != e.input.SHA256 || completed.CompressedBytes != int64(len(e.data)) || completed.EntryCount != 2 {
		t.Fatalf("快照事实: %+v", completed)
	}
	if duration := completed.ExpiresAt.Sub(completed.CreatedAt); duration != SnapshotRetention {
		t.Fatalf("保留期: %v", duration)
	}
	repeated, err := e.service.Complete(ctx, e.developer, "space", "app", upload.ID)
	if err != nil || repeated.ID != completed.ID {
		t.Fatalf("完成不幂等: %+v %v", repeated, err)
	}
	reader, actual, err := e.service.OpenSnapshot(ctx, e.developer, "space", "app", completed.ID)
	if err != nil {
		t.Fatal(err)
	}
	content, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(content, e.data) || actual.ID != completed.ID {
		t.Fatalf("实际内容: %v", err)
	}
	encoded, err := json.Marshal(actual)
	if err != nil || bytes.Contains(encoded, []byte(e.root)) || bytes.Contains(encoded, []byte("StorageKey")) {
		t.Fatalf("泄漏宿主路径: %s %v", encoded, err)
	}
	var count int64
	if err := e.db.Model(&snapshotRecord{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("快照重复: %d %v", count, err)
	}
	if err := e.db.Model(&model.AuditEvent{}).Where("action = ?", "source.snapshot.complete").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("完成审计重复: %d %v", count, err)
	}
	files, err := os.ReadDir(e.root)
	if err != nil || len(files) != 1 || !strings.HasPrefix(files[0].Name(), "snapshot_") {
		t.Fatalf("暂存未清理: %v %v", files, err)
	}
}

type interruptedReader struct {
	content []byte
	done    bool
}

func (r *interruptedReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.ErrUnexpectedEOF
	}
	r.done = true
	return copy(p, r.content[:len(r.content)/2]), nil
}

func TestHashSizeMismatchAndInterruptedUploadCanRetry(t *testing.T) {
	for _, name := range []string{"摘要", "超量", "短读", "中断"} {
		t.Run(name, func(t *testing.T) {
			e := setup(t)
			upload := e.create(t)
			var reader io.Reader
			expected := ErrContent
			switch name {
			case "摘要":
				altered := bytes.Clone(e.data)
				altered[0] ^= 1
				reader = bytes.NewReader(altered)
			case "超量":
				reader = bytes.NewReader(append(bytes.Clone(e.data), 'x'))
			case "短读":
				reader = bytes.NewReader(e.data[:len(e.data)-1])
			case "中断":
				reader = &interruptedReader{content: e.data}
				expected = io.ErrUnexpectedEOF
			}
			if _, err := e.service.Upload(context.Background(), e.developer, "space", "app", upload.ID, reader); !errors.Is(err, expected) {
				t.Fatalf("错误: %v", err)
			}
			var stored uploadRecord
			if err := e.db.First(&stored, "id = ?", upload.ID).Error; err != nil || stored.State != StatePending {
				t.Fatalf("失败污染状态: %+v %v", stored, err)
			}
			files, err := os.ReadDir(e.root)
			if err != nil || len(files) != 0 {
				t.Fatalf("失败临时内容残留: %v %v", files, err)
			}
			if _, err := e.service.Upload(context.Background(), e.developer, "space", "app", upload.ID, bytes.NewReader(e.data)); err != nil {
				t.Fatalf("重传失败: %v", err)
			}
		})
	}
}

func TestCrossApplicationViewerAndTokenIsolation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	ready := e.ready(t)
	for _, call := range []func() error{
		func() error { _, err := e.service.CreateUpload(ctx, e.viewer, "space", "app", e.input); return err },
		func() error { _, err := e.service.GetSnapshot(ctx, e.viewer, "space", "app", ready.ID); return err },
		func() error { _, _, err := e.service.OpenSnapshot(ctx, e.viewer, "space", "app", ready.ID); return err },
		func() error { _, err := e.service.GetSnapshot(ctx, e.admin, "space", "app-two", ready.ID); return err },
		func() error {
			_, err := e.service.GetSnapshot(ctx, e.admin, "other", "other-app", ready.ID)
			return err
		},
	} {
		if err := call(); !errors.Is(err, authorization.ErrNotFound) {
			t.Fatalf("越权未拒绝: %v", err)
		}
	}
	token := model.AccessToken{ID: "pat", Name: "源码只读测试", UserID: e.developer.UserID, TokenHash: "hash", Scopes: []string{string(authorization.SourceRead)}, AuthVersion: 1, ExpiresAt: time.Now().Add(time.Hour)}
	if err := e.db.Create(&token).Error; err != nil {
		t.Fatal(err)
	}
	if err := e.db.Create(&model.AccessTokenApplication{TokenID: token.ID, WorkspaceID: "space", ApplicationID: "app"}).Error; err != nil {
		t.Fatal(err)
	}
	principal := authorization.Principal{UserID: e.developer.UserID, AuthVersion: 1, TokenID: token.ID}
	if _, err := e.service.GetSnapshot(ctx, principal, "space", "app", ready.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.CreateUpload(ctx, principal, "space", "app", e.input); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("只读PAT可上传: %v", err)
	}
	if err := e.db.Model(&model.AccessToken{}).Where("id = ?", token.ID).Update("revoked_at", time.Now()).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.GetSnapshot(ctx, principal, "space", "app", ready.ID); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("撤销PAT仍可读: %v", err)
	}
}

func TestInvalidArchiveNeverBecomesReady(t *testing.T) {
	e := setup(t)
	data := []byte("not gzip but matches upload digest")
	digest := sha256.Sum256(data)
	input := CreateInput{Size: int64(len(data)), SHA256: hex.EncodeToString(digest[:])}
	upload, err := e.service.CreateUpload(context.Background(), e.developer, "space", "app", input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.Upload(context.Background(), e.developer, "space", "app", upload.ID, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.Complete(context.Background(), e.developer, "space", "app", upload.ID); !errors.Is(err, sourcearchive.ErrInvalidArchive) {
		t.Fatalf("无效包已完成: %v", err)
	}
	var count int64
	if err := e.db.Model(&snapshotRecord{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("产生错误快照: %d %v", count, err)
	}
}

func TestConcurrentUploadAndComplete(t *testing.T) {
	e := setup(t)
	upload := e.create(t)
	otherAccess, err := authorization.NewAccess(e.db)
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewService(e.db, otherAccess, Config{Root: e.root})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, service := range []*Service{e.service, other} {
		workers.Add(1)
		go func(service *Service) {
			defer workers.Done()
			<-start
			_, err := service.Upload(context.Background(), e.developer, "space", "app", upload.ID, bytes.NewReader(e.data))
			results <- err
		}(service)
	}
	close(start)
	workers.Wait()
	close(results)
	var successes, conflicts int
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, authorization.ErrConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("并发上传: %d %d", successes, conflicts)
	}
	ids := make(chan string, 2)
	results = make(chan error, 2)
	start = make(chan struct{})
	for _, service := range []*Service{e.service, other} {
		workers.Add(1)
		go func(service *Service) {
			defer workers.Done()
			<-start
			value, err := service.Complete(context.Background(), e.developer, "space", "app", upload.ID)
			ids <- value.ID
			results <- err
		}(service)
	}
	close(start)
	workers.Wait()
	close(ids)
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	first := <-ids
	if second := <-ids; first == "" || first != second {
		t.Fatalf("完成不是同一快照: %s %s", first, second)
	}
}

type gatedReader struct {
	reader  io.Reader
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *gatedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.entered); <-r.release })
	return r.reader.Read(p)
}

func TestRevocationNotBlockedByTransferAndPreventsCommit(t *testing.T) {
	e := setup(t)
	upload := e.create(t)
	reader := &gatedReader{reader: bytes.NewReader(e.data), entered: make(chan struct{}), release: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := e.service.Upload(context.Background(), e.developer, "space", "app", upload.ID, reader)
		result <- err
	}()
	select {
	case <-reader.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("上传未开始")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := e.access.Do(ctx, e.admin, true, func(tx *gorm.DB, principal authorization.Principal) error {
		return tx.Model(&model.User{}).Where("id = ?", e.developer.UserID).Update("auth_version", 2).Error
	})
	close(reader.release)
	if err != nil {
		t.Fatalf("撤权被长上传阻塞: %v", err)
	}
	if err := <-result; !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("撤权仍提交上传: %v", err)
	}
	var stored uploadRecord
	if err := e.db.First(&stored, "id = ?", upload.ID).Error; err != nil || stored.State != StatePending {
		t.Fatalf("撤权污染状态: %+v %v", stored, err)
	}
	// 快照不可用：撤权后归档入口先被身份复核拒绝，且不产生任何快照
	//（未撤权时也会因会话仍 PENDING 被 ErrNotReady 拒绝）。
	if _, err := e.service.Complete(context.Background(), e.developer, "space", "app", upload.ID); !errors.Is(err, authorization.ErrUnauthenticated) {
		t.Fatalf("撤权后会话仍可归档: %v", err)
	}
	var snapshots int64
	if err := e.db.Model(&snapshotRecord{}).Count(&snapshots).Error; err != nil || snapshots != 0 {
		t.Fatalf("撤权后产生快照: %d %v", snapshots, err)
	}
}

// TestSlowUploadDoesNotHoldDatabaseConnection 验证收包移出事务的核心收益：
// 慢上传期间连接池唯一连接保持空闲，其他空间的查询照常执行（池=1 使任何
// 占用都必然阻塞本用例）。
func TestSlowUploadDoesNotHoldDatabaseConnection(t *testing.T) {
	e := setup(t)
	pool, err := e.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { pool.SetMaxOpenConns(0) })
	upload := e.create(t)
	reader := &gatedReader{reader: bytes.NewReader(e.data), entered: make(chan struct{}), release: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := e.service.Upload(context.Background(), e.developer, "space", "app", upload.ID, reader)
		result <- err
	}()
	select {
	case <-reader.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("上传未开始")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	queryDone := make(chan error, 1)
	go func() {
		var count int64
		queryDone <- e.db.WithContext(ctx).Model(&model.Workspace{}).Count(&count).Error
	}()
	select {
	case err := <-queryDone:
		if err != nil {
			t.Fatalf("慢上传阻塞其他查询: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("其他查询在慢上传期间无法获得连接")
	}
	close(reader.release)
	if err := <-result; err != nil {
		t.Fatalf("恢复后上传失败: %v", err)
	}
	if stored := e.received(t); stored.State != StateUploaded {
		t.Fatalf("池收窄后新上传失败: %+v", stored)
	}
}

// TestCleanupSweepsOrphanPartFiles 验证收包临时文件的兜底回收：只删超过
// 寿命上限的 part 文件，活跃收包文件与已落盘快照不受影响。
func TestCleanupSweepsOrphanPartFiles(t *testing.T) {
	e := setup(t)
	ready := e.ready(t)
	stale := "part_" + strings.Repeat("a", 32)
	fresh := "part_" + strings.Repeat("b", 32)
	for _, name := range []string{stale, fresh} {
		if err := os.WriteFile(filepath.Join(e.root, name), []byte("half"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(filepath.Join(e.root, stale), past, past); err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.Cleanup(context.Background(), time.Now().UTC(), 100); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(e.root, stale)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("过期半包未回收: %v", err)
	}
	if _, err := os.Stat(filepath.Join(e.root, fresh)); err != nil {
		t.Fatalf("误删活跃收包文件: %v", err)
	}
	target := "snapshot_" + strings.TrimPrefix(ready.ID, "src_") + ".tar.gz"
	if _, err := os.Stat(filepath.Join(e.root, target)); err != nil {
		t.Fatalf("快照文件被误删: %v", err)
	}
}

func TestExpiryMetadataAndRenameCrashRecovery(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	upload := e.received(t)
	var stored uploadRecord
	if err := e.db.First(&stored, "id = ?", upload.ID).Error; err != nil {
		t.Fatal(err)
	}
	_, target := snapshotIdentity(stored)
	if err := e.service.root.Rename(stored.StorageKey, target); err != nil {
		t.Fatal(err)
	}
	ready, err := e.service.Complete(ctx, e.developer, "space", "app", upload.ID)
	if err != nil {
		t.Fatalf("rename后崩溃未恢复: %v", err)
	}
	if err := e.db.Model(&snapshotRecord{}).Where("id = ?", ready.ID).Updates(map[string]any{"created_at": time.Now().Add(-8 * 24 * time.Hour), "expires_at": time.Now().Add(-time.Hour)}).Error; err != nil {
		t.Fatal(err)
	}
	metadata, err := e.service.GetSnapshot(ctx, e.developer, "space", "app", ready.ID)
	if err != nil || metadata.State != StateExpired {
		t.Fatalf("过期元数据丢失: %+v %v", metadata, err)
	}
	if _, _, err := e.service.OpenSnapshot(ctx, e.developer, "space", "app", ready.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("可下载过期内容: %v", err)
	}
	upload = e.create(t)
	if err := e.db.Model(&uploadRecord{}).Where("id = ?", upload.ID).Updates(map[string]any{"created_at": time.Now().Add(-time.Hour), "expires_at": time.Now().Add(-time.Minute)}).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.Upload(ctx, e.developer, "space", "app", upload.ID, bytes.NewReader(e.data)); !errors.Is(err, ErrExpired) {
		t.Fatalf("过期会话可上传: %v", err)
	}
}

func TestUploadCommitWorksWithSingleConnection(t *testing.T) {
	e := setup(t)
	pool, err := e.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	upload, err := e.service.CreateUpload(ctx, e.developer, "space", "app", e.input)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.Upload(ctx, e.developer, "space", "app", upload.ID, bytes.NewReader(e.data)); err != nil {
		t.Fatalf("单连接上传提交死锁: %v", err)
	}
	if _, err := e.service.Complete(ctx, e.developer, "space", "app", upload.ID); err != nil {
		t.Fatalf("单连接完成死锁: %v", err)
	}
	if _, err := e.service.Complete(ctx, e.developer, "space", "app", upload.ID); err != nil {
		t.Fatalf("单连接幂等完成死锁: %v", err)
	}
}

func TestReceiveRenameCrashRecoveryAndOwnerIsolation(t *testing.T) {
	e := setup(t)
	upload := e.create(t)
	var stored uploadRecord
	if err := e.db.First(&stored, "id = ?", upload.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.root, stored.StorageKey), e.data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := e.service.Upload(context.Background(), e.admin, "space", "app", upload.ID, bytes.NewReader(e.data)); !errors.Is(err, authorization.ErrNotFound) {
		t.Fatalf("其他用户接管上传会话: %v", err)
	}
	if _, err := e.service.Upload(context.Background(), e.developer, "space", "app", upload.ID, bytes.NewReader(e.data)); err != nil {
		t.Fatalf("接收rename崩溃无法恢复: %v", err)
	}
	if _, err := e.service.Complete(context.Background(), e.developer, "space", "app", upload.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCanceledReaderNeverCommitsUploadedState(t *testing.T) {
	e := setup(t)
	upload := e.create(t)
	reader := &gatedReader{reader: bytes.NewReader(e.data), entered: make(chan struct{}), release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := e.service.Upload(ctx, e.developer, "space", "app", upload.ID, reader); result <- err }()
	select {
	case <-reader.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("上传未开始")
	}
	cancel()
	close(reader.release)
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("取消未上报: %v", err)
	}
	var stored uploadRecord
	if err := e.db.First(&stored, "id = ?", upload.ID).Error; err != nil || stored.State != StatePending {
		t.Fatalf("取消仍已上传: %+v %v", stored, err)
	}
}

func TestInputValidationAndStorageUnavailable(t *testing.T) {
	e := setup(t)
	for _, input := range []CreateInput{{Size: -1, SHA256: e.input.SHA256}, {Size: 1, SHA256: "bad"}, {Size: 257 << 20, SHA256: e.input.SHA256}, {Size: 1, SHA256: e.input.SHA256, Dockerfile: "../Dockerfile"}, {Size: 1, SHA256: e.input.SHA256, Dockerfile: "/Dockerfile"}} {
		if _, err := e.service.CreateUpload(context.Background(), e.developer, "space", "app", input); err == nil {
			t.Fatalf("接受无效输入: %+v", input)
		}
	}
	if _, err := NewService(e.db, e.access, Config{}); !errors.Is(err, authorization.ErrInvalid) {
		t.Fatalf("存在隐式根目录: %v", err)
	}
	ready := e.ready(t)
	var stored snapshotRecord
	if err := e.db.First(&stored, "id = ?", ready.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := e.db.Model(&snapshotRecord{}).Where("id = ?", ready.ID).Update("storage_key", "snapshot_"+strings.Repeat("0", 32)+".tar.gz").Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.service.OpenSnapshot(context.Background(), e.developer, "space", "app", ready.ID); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("丢失文件假装可读: %v", err)
	}
}
