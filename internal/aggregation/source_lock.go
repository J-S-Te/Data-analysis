package aggregation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"gorm.io/gorm"
)

// scheduledSyncLockPrefix 是 MySQL 命名锁的前缀；锁名只含哈希，不含租户或来源原文。
const scheduledSyncLockPrefix = "data-analysis:sync:"

// scheduledSyncLocker 为定时全量同步提供“同一租户/来源同时只有一个执行者”的互斥。
// TryAcquire 成功时返回释放函数；acquired=false 表示另一个执行者持有锁。
type scheduledSyncLocker interface {
	TryAcquire(ctx context.Context, tenantID, source string) (release func() error, acquired bool, err error)
}

// scheduledSyncLockName 生成稳定、定长且有界的互斥锁名。
// 参数：tenantID 为租户，source 为子系统来源编码。
// 返回值：前缀 + 租户与来源的 SHA-256 截断（160 bit），总长 59，小于 MySQL
// GET_LOCK 的 64 字符上限。使用哈希而非原文是为了避免 tenant/source 中的分隔符
// 或超长值改变锁粒度。
func scheduledSyncLockName(tenantID, source string) string {
	sum := sha256.Sum256([]byte(tenantID + "\x00" + source))
	return scheduledSyncLockPrefix + hex.EncodeToString(sum[:])[:40]
}

// scheduledSyncLockerFor 按聚合库方言选择互斥实现。
// 生产使用 MySQL 会话级 GET_LOCK；SQLite 等单进程测试库使用进程内互斥。
func scheduledSyncLockerFor(db *gorm.DB) scheduledSyncLocker {
	if db != nil && db.Dialector != nil && db.Dialector.Name() == "mysql" {
		return mysqlScheduledSyncLocker{db: db}
	}
	return processScheduledSyncLocker
}

// mysqlScheduledSyncLocker 使用 MySQL GET_LOCK 在同一会话连接上持有命名锁。
// GET_LOCK 随会话结束自动释放；正常路径显式 RELEASE_LOCK 后归还连接到连接池。
type mysqlScheduledSyncLocker struct{ db *gorm.DB }

// TryAcquire 非阻塞获取租户/来源级命名锁。
// 参数：ctx 为取消上下文，tenantID 为租户，source 为来源编码。
// 返回值：获取成功返回释放函数；锁被占用返回 acquired=false；连接或锁错误返回 err。
func (locker mysqlScheduledSyncLocker) TryAcquire(ctx context.Context, tenantID, source string) (func() error, bool, error) {
	sqlDB, err := locker.db.DB()
	if err != nil {
		return nil, false, err
	}
	// GET_LOCK 是会话级状态，必须固定同一连接直到释放，不能走 GORM 连接池。
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	name := scheduledSyncLockName(tenantID, source)
	var acquired sql.NullInt64
	if err := conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", name).Scan(&acquired); err != nil {
		_ = conn.Close()
		return nil, false, err
	}
	if !acquired.Valid {
		// MySQL 文档：GET_LOCK 返回 NULL 表示发生了错误，不能当成“未获得锁”继续同步。
		_ = conn.Close()
		return nil, false, errors.New("scheduled sync lock returned NULL")
	}
	if acquired.Int64 != 1 {
		_ = conn.Close()
		return nil, false, nil
	}
	release := func() error {
		defer conn.Close()
		// 同步取消后仍需释放会话锁，否则连接归还池中会继续占锁。
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		var released sql.NullInt64
		if err := conn.QueryRowContext(releaseCtx, "SELECT RELEASE_LOCK(?)", name).Scan(&released); err != nil {
			return err
		}
		if !released.Valid || released.Int64 != 1 {
			return errors.New("scheduled sync lock was not held at release")
		}
		return nil
	}
	return release, true, nil
}

// processScheduledSyncLocker 是单进程测试库（SQLite）的退路：
// 只能防止同进程重叠轮次，不提供跨进程保证；生产 MySQL 使用 GET_LOCK。
type processScheduledSyncLockerImpl struct{ locks sync.Map }

var processScheduledSyncLocker = &processScheduledSyncLockerImpl{}

func (locker *processScheduledSyncLockerImpl) TryAcquire(_ context.Context, tenantID, source string) (func() error, bool, error) {
	value, _ := locker.locks.LoadOrStore(scheduledSyncLockName(tenantID, source), &sync.Mutex{})
	mutex := value.(*sync.Mutex)
	if !mutex.TryLock() {
		return nil, false, nil
	}
	return func() error {
		mutex.Unlock()
		return nil
	}, true, nil
}
