package service

import (
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "net/http"
    "os"
    "strings"
    "testing"
    "time"

    "github.com/assimon/luuu/model/dao"
    "github.com/spf13/viper"
    "gorm.io/driver/mysql"
    "gorm.io/gorm"
    "gorm.io/gorm/logger"
)

// incomeTestTransport 将全部外部请求替换为测试响应，不访问真实供应商或通知服务。
type incomeTestTransport func(*http.Request) (*http.Response, error)

// RoundTrip 输入HTTP请求，返回测试定义的响应或错误。
func (transport incomeTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
    return transport(request)
}

// incomeTestDB 仅在显式指定测试DSN时运行，使用连接级临时表隔离所有真实业务数据。
func incomeTestDB(t *testing.T) *gorm.DB {
    t.Helper()
    dsn := os.Getenv("INCOME_TEST_MYSQL_DSN")
    if dsn == "" {
        t.Skip("未配置测试数据库DSN")
    }
    db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
    if err != nil {
        t.Fatal("无法连接显式指定的测试数据库")
    }
    connection, err := db.DB()
    if err != nil {
        t.Fatal(err)
    }
    connection.SetMaxOpenConns(1)
    connection.SetMaxIdleConns(1)
    previous := dao.Mdb
    dao.Mdb = db
    t.Cleanup(func() {
        dao.Mdb = previous
        connection.Close()
    })
    migration, err := os.ReadFile("../../../sql/20261109-incoming-transfer-event.sql")
    if err != nil {
        t.Fatal(err)
    }
    for _, sql := range strings.Split(string(migration), ";") {
        if strings.TrimSpace(sql) == "" {
            continue
        }
        sql = strings.ReplaceAll(sql, "CREATE TABLE IF NOT EXISTS", "CREATE TEMPORARY TABLE")
        if err := db.Exec(sql).Error; err != nil {
            t.Fatal(err)
        }
    }
    // 临时orders只用于核对查询，屏蔽真实支付订单表，连接关闭自动清理。
    if err := db.Exec("CREATE TEMPORARY TABLE orders (token VARCHAR(34),actual_amount DECIMAL(38,6),block_transaction_id CHAR(64),status INT,deleted_at DATETIME NULL)").Error; err != nil {
        t.Fatal(err)
    }
    return db
}

// TestIncomeMysqlDecimalOrder 验证查询结果绕过订单float字段，使用数据库十进制值唯一匹配。
func TestIncomeMysqlDecimalOrder(t *testing.T) {
    db := incomeTestDB(t)
    hash := incomeTestEvents(t, db, 1)
    if err := db.Exec("INSERT INTO orders(token,actual_amount,block_transaction_id,status) VALUES(?,?,?,2)", usdtContract, "1.5", hash).Error; err != nil {
        t.Fatal(err)
    }
    incomeTestHTTP(t, func(_ *http.Request) (*http.Response, error) {
        t.Error("已付款唯一匹配不得发送异常通知")
        return nil, errors.New("禁止发送")
    })
    if err := reconcileIncome(); err != nil {
        t.Fatal(err)
    }
    var state string
    if err := db.Raw("SELECT state FROM incoming_transfer_event").Scan(&state).Error; err != nil {
        t.Fatal(err)
    }
    if state != "MATCHED" {
        t.Fatal("数据库十进制订单未匹配")
    }
}

// TestIncomeMysqlAsyncStart 验证分页阻塞时启动立即返回，重复启动不会并发创建核对任务。
func TestIncomeMysqlAsyncStart(t *testing.T) {
    incomeTestDB(t)
    viper.Set("income_audit_enabled", true)
    viper.Set("income_audit_start_ms", time.Now().Add(-24*time.Hour).UnixNano()/1e6)
    viper.Set("income_audit_wallets", usdtContract)
    t.Cleanup(func() { viper.Reset() })
    started, release := make(chan struct{}, 2), make(chan struct{})
    incomeTestHTTP(t, func(_ *http.Request) (*http.Response, error) {
        started <- struct{}{}
        <-release
        return incomeTestResponse(UsdtTrc20Resp{Code: 200}), nil
    })
    returned := make(chan struct{})
    go func() { StartIncomeAudit(); close(returned) }()
    select {
    case <-returned:
    case <-time.After(time.Second):
        close(release)
        t.Fatal("启动被分页阻塞")
    }
    select {
    case <-started:
    case <-time.After(time.Second):
        close(release)
        t.Fatal("异步任务未启动")
    }
    StartIncomeAudit()
    close(release)
    deadline := time.Now().Add(3 * time.Second)
    for len(incomeAuditRunning) > 0 && time.Now().Before(deadline) {
        time.Sleep(5 * time.Millisecond)
    }
    if len(incomeAuditRunning) != 0 || len(started) != 0 {
        t.Fatal("异步核对未正常退出或并发重复启动")
    }
}

// incomeTestHTTP 安装仅限当前测试的网络桩，结束时恢复默认传输。
func incomeTestHTTP(t *testing.T, transport incomeTestTransport) {
    t.Helper()
    previous := http.DefaultTransport
    http.DefaultTransport = transport
    t.Cleanup(func() { http.DefaultTransport = previous })
}

// incomeTestResponse 将响应对象编码为HTTP200 JSON，供外部API桩返回。
func incomeTestResponse(body interface{}) *http.Response {
    content, _ := json.Marshal(body)
    return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(content)))}
}

// TestIncomeMysqlPagination 验证总量口径差异、空结果、满页与上游错误下真实游标变化。
func TestIncomeMysqlPagination(t *testing.T) {
    for _, fixture := range []struct {
        name string
        full bool
        code int
    }{
        {"null_data", false, 200}, {"full_then_empty_total_metadata", true, 200}, {"api_error", false, 500},
    } {
        t.Run(fixture.name, func(t *testing.T) {
            db := incomeTestDB(t)
            initial := time.Now().Add(-24*time.Hour).UnixNano() / 1e6
            calls := 0
            incomeTestHTTP(t, func(request *http.Request) (*http.Response, error) {
                calls++
                if request.URL.Host != "apilist.tronscanapi.com" || request.URL.Query().Get("start") != fmt.Sprint((calls-1)*50) {
                    return nil, errors.New("意外请求或分页偏移")
                }
                page := UsdtTrc20Resp{Code: fixture.code, PageSize: 5000}
                if fixture.full && calls == 1 {
                    // 非目标钱包记录无需回执，用于验证分页继续而非page_size断言。
                    page.Data = make([]Data, 50)
                }
                return incomeTestResponse(page), nil
            })
            err := scanIncomeWallet(usdtContract, initial)
            var cursor int64
            if queryErr := db.Raw("SELECT cursor_ms FROM incoming_transfer_cursor").Scan(&cursor).Error; queryErr != nil {
                t.Fatal(queryErr)
            }
            if fixture.code != 200 {
                if err == nil || cursor != initial {
                    t.Fatal("错误响应不能推进游标")
                }
            } else if err != nil || cursor <= initial || (fixture.full && calls != 2) {
                t.Fatalf("正常分页未推进: cursor=%d calls=%d err=%v", cursor, calls, err)
            }
        })
    }
}

// incomeTestEvents 在临时台账插入完整交易，返回稳定交易号，不产生真实付款数据。
func incomeTestEvents(t *testing.T, db *gorm.DB, count int) string {
    t.Helper()
    hash := strings.Repeat("a", 64)
    for index := 0; index < count; index++ {
        if err := db.Exec("INSERT INTO incoming_transfer_event(event_key,tx_hash,event_index,wallet,sender,amount,block_time,next_attempt_at) VALUES(?,?,?,?,?,?,?,NOW(3))",
            fmt.Sprintf("tron:%s:%d", hash, index), hash, index, usdtContract, usdtContract, "1.500000", 1700000000000).Error; err != nil {
            t.Fatal(err)
        }
    }
    return hash
}

// TestIncomeMysqlGroupRetry 验证部分受理整组回退、8轮死信、错误落库和FAILED不再发送。
func TestIncomeMysqlGroupRetry(t *testing.T) {
    db := incomeTestDB(t)
    incomeTestEvents(t, db, 2)
    for attempt := 1; attempt <= INCOME_MAX_ATTEMPTS; attempt++ {
        var events []incomeEvent
        if err := db.Raw("SELECT * FROM incoming_transfer_event ORDER BY id").Scan(&events).Error; err != nil {
            t.Fatal(err)
        }
        pending, err := classifyIncome(events, nil)
        if err != nil {
            t.Fatal(err)
        }
        sends := 0
        err = deliverIncomeGroup(pending, func(event incomeEvent) error {
            sends++
            if sends == 2 {
                return errors.New("测试接收失败")
            }
            return nil
        })
        if err != nil {
            t.Fatal(err)
        }
        var result []incomeEvent
        if err = db.Raw("SELECT * FROM incoming_transfer_event ORDER BY id").Scan(&result).Error; err != nil {
            t.Fatal(err)
        }
        expected := "PENDING"
        if attempt == INCOME_MAX_ATTEMPTS {
            expected = "FAILED"
        }
        for _, event := range result {
            if event.State != expected || event.Attempts != attempt || !strings.Contains(event.Reason, "测试接收失败") {
                t.Fatalf("整组失败状态错误: %+v", event)
            }
        }
    }
    incomeTestHTTP(t, func(_ *http.Request) (*http.Response, error) {
        t.Error("FAILED不应再次发送")
        return nil, errors.New("禁止发送")
    })
    if err := reconcileIncome(); err != nil {
        t.Fatal(err)
    }
}

// TestIncomeMysqlWholeGroup 验证超过50事件仍完整处理，以及接收端接受后的整组事务提交。
func TestIncomeMysqlWholeGroup(t *testing.T) {
    db := incomeTestDB(t)
    incomeTestEvents(t, db, 51)
    viper.Set("income_audit_notify_url", "https://income-test.invalid/internal/payment/unmatched-income")
    viper.Set("income_audit_secret", strings.Repeat("x", 32))
    t.Cleanup(func() { viper.Reset() })
    sends := 0
    incomeTestHTTP(t, func(request *http.Request) (*http.Response, error) {
        if request.URL.Host != "income-test.invalid" || request.Header.Get("X-Income-Signature") == "" {
            return nil, errors.New("通知地址或签名错误")
        }
        sends++
        var count int64
        if err := db.Raw("SELECT COUNT(*) FROM incoming_transfer_event WHERE state='NOTIFIED'").Scan(&count).Error; err != nil || count != 0 {
            t.Fatal("完成整组受理前不得出现半组NOTIFIED")
        }
        return incomeTestResponse(map[string]bool{"accepted": true}), nil
    })
    if err := reconcileIncome(); err != nil {
        t.Fatal(err)
    }
    var count int64
    if err := db.Raw("SELECT COUNT(*) FROM incoming_transfer_event WHERE state='NOTIFIED'").Scan(&count).Error; err != nil {
        t.Fatal(err)
    }
    if sends != 51 || count != 51 {
        t.Fatalf("交易被截断: sends=%d notified=%d", sends, count)
    }
}
