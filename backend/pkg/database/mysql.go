// Package database 提供数据库连接与连接池初始化，不执行业务表迁移。
package database

import (
	"context"
	"database/sql"
	"errors"
	driver "github.com/go-sql-driver/mysql"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"time"
)

// MySQLConfig 定义连接和连接池设置；密码不应写入日志。
type MySQLConfig struct {
	Address, Username, Password, Database string
	MaxOpenConns, MaxIdleConns            int
	ConnMaxLifetime, ConnectTimeout       time.Duration
}

// OpenMySQL 初始化连接池并用调用方 Context 检查连通性；成功后由调用方关闭 sql.DB。
func OpenMySQL(ctx context.Context, config MySQLConfig) (*gorm.DB, *sql.DB, error) {
	if config.Address == "" || config.Username == "" || config.Database == "" {
		return nil, nil, errors.New("数据库地址、用户名和库名不能为空")
	}
	if config.MaxOpenConns < 0 || config.MaxIdleConns < 0 || config.ConnMaxLifetime < 0 || config.ConnectTimeout < 0 {
		return nil, nil, errors.New("数据库连接池参数不能为负数")
	}
	if config.MaxOpenConns == 0 {
		config.MaxOpenConns = 10
	}
	if config.MaxIdleConns == 0 {
		config.MaxIdleConns = 5
	}
	if config.MaxIdleConns > config.MaxOpenConns {
		config.MaxIdleConns = config.MaxOpenConns
	}
	if config.ConnMaxLifetime == 0 {
		config.ConnMaxLifetime = 3 * time.Minute
	}
	if config.ConnectTimeout == 0 {
		config.ConnectTimeout = 5 * time.Second
	}
	// 使用驱动生成 DSN，正确编码密码和 UTC 时区，避免手工拼接特殊字符。
	dsn := driver.NewConfig()
	dsn.Net = "tcp"
	dsn.Addr = config.Address
	dsn.User = config.Username
	dsn.Passwd = config.Password
	dsn.DBName = config.Database
	dsn.ParseTime = true
	dsn.Loc = time.UTC
	dsn.Timeout = config.ConnectTimeout
	dsn.ReadTimeout = 5 * time.Second
	dsn.WriteTimeout = 5 * time.Second
	dsn.Params = map[string]string{"charset": "utf8mb4", "time_zone": "'+00:00'"}
	pool, err := sql.Open("mysql", dsn.FormatDSN())
	if err != nil {
		return nil, nil, err
	}
	pool.SetMaxOpenConns(config.MaxOpenConns)
	pool.SetMaxIdleConns(config.MaxIdleConns)
	pool.SetConnMaxLifetime(config.ConnMaxLifetime)
	// 初始化失败时关闭连接池，避免启动重试泄漏资源。
	if err = pool.PingContext(ctx); err != nil {
		pool.Close()
		return nil, nil, err
	}
	db, err := gorm.Open(mysql.New(mysql.Config{Conn: pool, SkipInitializeWithVersion: true}), &gorm.Config{
		DisableAutomaticPing: true, TranslateError: true, Logger: logger.Default.LogMode(logger.Silent), NowFunc: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	return db, pool, nil
}
