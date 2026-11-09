package service

import (
    "bytes"
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "math/big"
    "net/http"
    "net/url"
    "strconv"
    "strings"
    "time"

    "github.com/assimon/luuu/model/dao"
    "github.com/assimon/luuu/model/mdb"
    "github.com/assimon/luuu/util/log"
    "github.com/shopspring/decimal"
    "github.com/spf13/viper"
    "gorm.io/gorm"
)

const usdtContract = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"
const transferTopic = "ddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

var incomeAuditRunning = make(chan struct{}, 1)

// StartIncomeAudit 非阻塞启动独立核对，耗时分页不延迟原五秒支付监听。
func StartIncomeAudit() {
    if !viper.GetBool("income_audit_enabled") {
        return
    }
    select {
    case incomeAuditRunning <- struct{}{}:
        go func() {
            defer func() {
                <-incomeAuditRunning
                if recover() != nil {
                    log.Sugar.Error("收入核对异常退出，下一轮继续原游标")
                }
            }()
            AuditTrc20Income()
        }()
    default:
    }
}

// incomeEvent 保存已确认到账的事件身份和通知状态，不参与订单付款匹配。
type incomeEvent struct {
    ID         uint64
    EventKey   string
    TxHash     string
    EventIndex int
    Wallet     string
    Sender     string
    Amount     string
    BlockTime  int64
    State      string
    Reason     string
    Attempts   int
}

// solidReceipt 是已固化交易回执；日志数组下标作为同交易内稳定的事件序号。
type solidReceipt struct {
    ID        string `json:"id"`
    BlockTime int64  `json:"blockTimeStamp"`
    Receipt   struct {
        Result string `json:"result"`
    } `json:"receipt"`
    Logs []struct {
        Address string   `json:"address"`
        Topics  []string `json:"topics"`
        Data    string   `json:"data"`
    } `json:"log"`
}

// AuditTrc20Income 独立核对业务白名单钱包；分页失败不前移游标，支付原流程不受影响。
func AuditTrc20Income() {
    if !viper.GetBool("income_audit_enabled") {
        return
    }
    start := viper.GetInt64("income_audit_start_ms")
    wallets := strings.Split(viper.GetString("income_audit_wallets"), ",")
    if start <= 0 {
        log.Sugar.Error("收入核对起始时间未配置")
        return
    }
    for _, value := range wallets {
        wallet := strings.TrimSpace(value)
        if len(wallet) != 34 || !strings.HasPrefix(wallet, "T") {
            continue
        }
        if err := scanIncomeWallet(wallet, start); err != nil {
            log.Sugar.Errorf("收入核对扫描失败，wallet=%s，保留游标: %v", wallet, err)
        }
    }
    if err := reconcileIncome(); err != nil {
        log.Sugar.Errorf("收入核对通知重试失败: %v", err)
    }
}

// scanIncomeWallet 使用固定时间窗完整分页，并以已固化回执校验每笔入账的合约、收款人及金额。
func scanIncomeWallet(wallet string, initial int64) error {
    if err := dao.Mdb.Exec("INSERT IGNORE INTO incoming_transfer_cursor(wallet,cursor_ms) VALUES(?,?)", wallet, initial).Error; err != nil {
        return err
    }
    var cursor struct{ CursorMs int64 }
    if err := dao.Mdb.Raw("SELECT cursor_ms FROM incoming_transfer_cursor WHERE wallet=?", wallet).Scan(&cursor).Error; err != nil {
        return err
    }
    window := viper.GetInt64("income_audit_window_ms")
    if window <= 0 || window > 3600000 {
        window = 3600000
    }
    overlap := int64((5 * time.Minute) / time.Millisecond)
    if overlap > window/2 {
        overlap = window / 2
    }
    begin := cursor.CursorMs - overlap
    if begin < initial {
        begin = initial
    }
    end := time.Now().Add(-2*time.Minute).UnixNano() / 1e6
    // 大积压分小时补齐；同一轮冻结右边界避免分页时新记录挤动偏移量。
    if end > cursor.CursorMs+window {
        end = cursor.CursorMs + window
    }
    if end <= begin {
        return nil
    }
    seen := make(map[string]bool)
    for offset := 0; offset < 10000; offset += 50 {
        query := url.Values{"address": {wallet}, "trc20Id": {usdtContract}, "start": {strconv.Itoa(offset)},
            "limit": {"50"}, "direction": {"2"}, "db_version": {"1"}, "reverse": {"false"},
            "start_timestamp": {strconv.FormatInt(begin, 10)}, "end_timestamp": {strconv.FormatInt(end, 10)}}
        var page UsdtTrc20Resp
        if err := incomeJSON(http.MethodGet, UsdtTrc20ApiUri+"?"+query.Encode(), nil, &page, "TRON-PRO-API-KEY", viper.GetString("income_audit_tronscan_key")); err != nil {
            return err
        }
        if page.Code != 200 || page.Data == nil || page.PageSize != len(page.Data) {
            return errors.New("转账分页响应无效")
        }
        for _, transfer := range page.Data {
            if transfer.To != wallet || transfer.ID != usdtContract || transfer.Revert != 0 || transfer.ContractRet != "SUCCESS" {
                continue
            }
            if transfer.Confirmed != 1 {
                return errors.New("时间窗存在未确认转账，暂不推进游标")
            }
            if transfer.BlockTimestamp < begin || transfer.BlockTimestamp > end {
                return errors.New("转账时间越界")
            }
            if !validHash(transfer.Hash) {
                return errors.New("交易号无效")
            }
            if seen[transfer.Hash] {
                continue
            }
            seen[transfer.Hash] = true
            var receipt solidReceipt
            body, _ := json.Marshal(map[string]string{"value": transfer.Hash})
            if err := incomeJSON(http.MethodPost, "https://api.trongrid.io/walletsolidity/gettransactioninfobyid", body, &receipt,
                "TRON-PRO-API-KEY", viper.GetString("income_audit_trongrid_key")); err != nil {
                return err
            }
            events, err := receiptIncome(receipt, transfer.Hash, wallet)
            if err != nil {
                return err
            }
            if len(events) == 0 {
                return errors.New("转账索引与固化回执不一致")
            }
            err = dao.Mdb.Transaction(func(tx *gorm.DB) error {
                for _, event := range events {
                    if event.BlockTime < initial {
                        continue
                    }
                    err = tx.Exec("INSERT INTO incoming_transfer_event(event_key,tx_hash,event_index,wallet,sender,amount,block_time,next_attempt_at) VALUES(?,?,?,?,?,?,?,DATE_ADD(NOW(3),INTERVAL 60 SECOND)) ON DUPLICATE KEY UPDATE event_key=VALUES(event_key)",
                        event.EventKey, event.TxHash, event.EventIndex, event.Wallet, event.Sender, event.Amount, event.BlockTime).Error
                    if err != nil {
                        return err
                    }
                }
                return nil
            })
            if err != nil {
                return err
            }
        }
        if len(page.Data) < 50 {
            return dao.Mdb.Exec("UPDATE incoming_transfer_cursor SET cursor_ms=GREATEST(cursor_ms,?) WHERE wallet=?", end, wallet).Error
        }
    }
    return errors.New("时间窗达到供应商一万条上限，需缩小扫描窗口后重跑；游标未前移")
}

// receiptIncome 从成功固化回执提取USDT转账；输入交易及目标钱包，返回带稳定序号的事件。
func receiptIncome(receipt solidReceipt, hash, wallet string) ([]incomeEvent, error) {
    if receipt.ID != hash || receipt.Receipt.Result != "SUCCESS" || receipt.BlockTime <= 0 {
        return nil, errors.New("固化回执未确认成功")
    }
    events := make([]incomeEvent, 0)
    for index, entry := range receipt.Logs {
        contract, err := tronAddress(entry.Address)
        if err != nil || contract != usdtContract {
            continue
        }
        if len(entry.Topics) != 3 || strings.ToLower(entry.Topics[0]) != transferTopic {
            continue
        }
        recipient, err := topicAddress(entry.Topics[2])
        if err != nil {
            return nil, err
        }
        if recipient != wallet {
            continue
        }
        sender, err := topicAddress(entry.Topics[1])
        if err != nil {
            return nil, err
        }
        if len(entry.Data) != 64 {
            return nil, errors.New("转账金额编码无效")
        }
        raw, ok := new(big.Int).SetString(entry.Data, 16)
        if !ok || raw.Sign() <= 0 {
            continue
        }
        amount := decimal.NewFromBigInt(raw, -6).StringFixed(6)
        events = append(events, incomeEvent{EventKey: "tron:" + hash + ":" + strconv.Itoa(index), TxHash: hash,
            EventIndex: index, Wallet: wallet, Sender: sender, Amount: amount, BlockTime: receipt.BlockTime})
    }
    return events, nil
}

// topicAddress 将32字节ABI地址参数转为TRON地址，非零填充视为异常。
func topicAddress(topic string) (string, error) {
    if len(topic) != 64 || strings.Trim(topic[:24], "0") != "" {
        return "", errors.New("事件地址编码无效")
    }
    return tronAddress(topic[24:])
}

// tronAddress 为20字节日志地址添加TRON前缀和Base58Check校验码。
func tronAddress(value string) (string, error) {
    raw, err := hex.DecodeString(value)
    if err != nil {
        return "", err
    }
    if len(raw) == 20 {
        raw = append([]byte{0x41}, raw...)
    }
    if len(raw) != 21 || raw[0] != 0x41 {
        return "", errors.New("TRON地址无效")
    }
    first := sha256.Sum256(raw)
    second := sha256.Sum256(first[:])
    n := new(big.Int).SetBytes(append(raw, second[:4]...))
    alphabet := "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
    output := make([]byte, 0, 34)
    for n.Sign() > 0 {
        remainder := new(big.Int)
        n.QuoRem(n, big.NewInt(58), remainder)
        output = append(output, alphabet[remainder.Int64()])
    }
    for left, right := 0, len(output)-1; left < right; left, right = left+1, right-1 {
        output[left], output[right] = output[right], output[left]
    }
    return string(output), nil
}

// validHash 校验交易号，避免无效输入进入外部请求及通知身份键。
func validHash(value string) bool {
    decoded, err := hex.DecodeString(value)
    return err == nil && len(decoded) == 32
}

// incomeJSON 执行有界HTTPS查询并解析响应；外部错误不包含凭据或完整响应正文。
func incomeJSON(method, endpoint string, body []byte, target interface{}, header, credential string) error {
    req, err := http.NewRequest(method, endpoint, bytes.NewReader(body))
    if err != nil {
        return errors.New("收入核对请求地址无效")
    }
    req.Header.Set("Content-Type", "application/json")
    if credential != "" {
        req.Header.Set(header, credential)
    }
    client := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
    response, err := client.Do(req)
    if err != nil {
        return errors.New("收入核对网络请求失败")
    }
    defer response.Body.Close()
    if response.StatusCode != http.StatusOK {
        return fmt.Errorf("收入核对HTTP状态%d", response.StatusCode)
    }
    return json.NewDecoder(io.LimitReader(response.Body, 4*1024*1024)).Decode(target)
}

// reconcileIncome 先排除历史已付款交易再通知；数据库查询失败保持待核对，不冒充未匹配。
func reconcileIncome() error {
    var events []incomeEvent
    if err := dao.Mdb.Raw("SELECT * FROM incoming_transfer_event WHERE state='PENDING' AND next_attempt_at<=NOW(3) ORDER BY id LIMIT 50").Scan(&events).Error; err != nil {
        return err
    }
    for _, event := range events {
        var orders []mdb.Orders
        if err := dao.Mdb.Where("block_transaction_id=? AND status=?", event.TxHash, mdb.StatusPaySuccess).Find(&orders).Error; err != nil {
            return err
        }
        var sameAmount int64
        if err := dao.Mdb.Raw("SELECT COUNT(*) FROM incoming_transfer_event WHERE tx_hash=? AND wallet=? AND amount=?",
            event.TxHash, event.Wallet, event.Amount).Scan(&sameAmount).Error; err != nil {
            return err
        }
        matched := false
        for _, order := range orders {
            amount, err := decimal.NewFromString(event.Amount)
            if err != nil {
                return err
            }
            if order.Token == event.Wallet && amount.Equal(decimal.NewFromFloat(order.ActualAmount)) && sameAmount == 1 {
                matched = true
            }
        }
        if matched {
            if err := dao.Mdb.Exec("UPDATE incoming_transfer_event SET state='MATCHED' WHERE id=?", event.ID).Error; err != nil {
                return err
            }
            continue
        }
        event.Reason = "链上已到账，未找到对应已付款订单，请业务核对；系统未自动入账"
        if len(orders) > 0 {
            event.Reason = "同交易已有付款记录，但到账事件与订单无法唯一对应，请业务核对，禁止重复入账"
        }
        if err := notifyIncome(event); err != nil {
            delay := 30 * (1 << uint(minIncome(event.Attempts, 6)))
            if err = dao.Mdb.Exec("UPDATE incoming_transfer_event SET attempts=attempts+1,next_attempt_at=DATE_ADD(NOW(3),INTERVAL ? SECOND) WHERE id=? AND state='PENDING'", delay, event.ID).Error; err != nil {
                return err
            }
            continue
        }
        if err := dao.Mdb.Exec("UPDATE incoming_transfer_event SET state='NOTIFIED',reason=? WHERE id=?", event.Reason, event.ID).Error; err != nil {
            return err
        }
    }
    return nil
}

// notifyIncome 以HMAC签名投递核心服务，只有持久化成功响应才标记已通知。
func notifyIncome(event incomeEvent) error {
    endpoint, secret := viper.GetString("income_audit_notify_url"), viper.GetString("income_audit_secret")
    if !strings.HasPrefix(endpoint, "https://") || len(secret) < 32 {
        return errors.New("收入通知配置缺失")
    }
    body, err := json.Marshal(map[string]interface{}{"eventKey": event.EventKey, "txHash": event.TxHash,
        "eventIndex": event.EventIndex, "wallet": event.Wallet, "sender": event.Sender, "amount": event.Amount,
        "blockTime": event.BlockTime, "reason": event.Reason})
    if err != nil {
        return err
    }
    timestamp := strconv.FormatInt(time.Now().Unix(), 10)
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write([]byte(timestamp + "."))
    mac.Write(body)
    var result struct {
        Accepted bool `json:"accepted"`
    }
    if err = incomeJSON(http.MethodPost, endpoint, body, &result, "X-Income-Signature", timestamp+":"+hex.EncodeToString(mac.Sum(nil))); err != nil {
        return err
    }
    if !result.Accepted {
        return errors.New("核心服务未确认入库")
    }
    return nil
}

// minIncome 对重试退避指数封顶，输入两整数，返回较小值。
func minIncome(a, b int) int {
    if a < b {
        return a
    }
    return b
}
