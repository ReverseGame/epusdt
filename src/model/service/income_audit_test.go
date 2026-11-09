package service

import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "strings"
    "testing"

    "github.com/spf13/viper"
)

// TestTronAddress 验证官方USDT合约的Base58Check地址及非法输入拒绝。
func TestTronAddress(t *testing.T) {
    address, err := tronAddress("a614f803b6fd780986a42c78ec9c7f77e6ded13c")
    if err != nil || address != usdtContract {
        t.Fatalf("合约转换错误: %s %v", address, err)
    }
    if _, err = tronAddress("bad"); err == nil {
        t.Fatal("非法地址不能通过")
    }
    if _, err = topicAddress(strings.Repeat("1", 64)); err == nil {
        t.Fatal("非零ABI填充不能通过")
    }
}

// TestIncomeNotifyAcknowledgement 验证真实发送函数的签名正文、接受标记及失败响应处理。
func TestIncomeNotifyAcknowledgement(t *testing.T) {
    secret := strings.Repeat("x", 32)
    viper.Set("income_audit_notify_url", "https://income-test.invalid/internal/payment/unmatched-income")
    viper.Set("income_audit_secret", secret)
    t.Cleanup(func() { viper.Reset() })
    for _, fixture := range []struct {
        name     string
        status   int
        accepted bool
        fail     bool
    }{
        {"accepted", 200, true, false}, {"rejected", 200, false, true}, {"bad_signature", 403, false, true},
    } {
        t.Run(fixture.name, func(t *testing.T) {
            incomeTestHTTP(t, func(request *http.Request) (*http.Response, error) {
                body, err := io.ReadAll(request.Body)
                if err != nil {
                    t.Fatal(err)
                }
                parts := strings.Split(request.Header.Get("X-Income-Signature"), ":")
                if len(parts) != 2 {
                    t.Fatal("签名头格式错误")
                }
                mac := hmac.New(sha256.New, []byte(secret))
                mac.Write([]byte(parts[0] + "."))
                mac.Write(body)
                if hex.EncodeToString(mac.Sum(nil)) != parts[1] {
                    t.Fatal("签名未覆盖原始正文")
                }
                response := incomeTestResponse(map[string]bool{"accepted": fixture.accepted})
                response.StatusCode = fixture.status
                return response, nil
            })
            err := notifyIncome(incomeEvent{EventKey: "test-event", Amount: "1.5"})
            if (err != nil) != fixture.fail {
                t.Fatalf("受理判定错误: %v", err)
            }
        })
    }
}

// TestIncomeDecimalMatching 验证金额等值及双向唯一匹配，不将重复金额误认为独立付款。
func TestIncomeDecimalMatching(t *testing.T) {
    events := []incomeEvent{{State: "PENDING", Wallet: "wallet", Amount: "1.5"},
        {State: "PENDING", Wallet: "wallet", Amount: "1.500000"}}
    orders := []incomePaidOrder{{Token: "wallet", ActualAmount: "1.5000"}}
    result, err := classifyIncome(events, orders)
    if err != nil || result[0].State != "NOTIFIED" || result[1].State != "NOTIFIED" {
        t.Fatalf("重复事件金额不能唯一匹配: %+v %v", result, err)
    }
    result, err = classifyIncome(events[:1], orders)
    if err != nil || result[0].State != "MATCHED" {
        t.Fatalf("十进制等值唯一匹配失败: %+v %v", result, err)
    }
    result, err = classifyIncome(events[:1], append(orders, orders[0]))
    if err != nil || result[0].State != "NOTIFIED" {
        t.Fatal("一个事件对应多个付款订单不能算唯一匹配")
    }
    events[0].Amount = "9007199254740993.123456"
    result, err = classifyIncome(events[:1], []incomePaidOrder{{Token: "wallet", ActualAmount: events[0].Amount}})
    if err != nil || result[0].State != "MATCHED" {
        t.Fatal("大额十进制精度不能经过float64")
    }
}

// TestIncomeEnabledEnv 验证当前Viper版本从真实env格式解析true和false。
func TestIncomeEnabledEnv(t *testing.T) {
    for _, value := range []string{"true", "TRUE", "1", "false", "0"} {
        config := viper.New()
        config.SetConfigType("env")
        if err := config.ReadConfig(strings.NewReader("income_audit_enabled=" + value)); err != nil {
            t.Fatal(err)
        }
        expected := value == "true" || value == "TRUE" || value == "1"
        if config.GetBool("income_audit_enabled") != expected {
            t.Fatalf("配置解析错误: %s", value)
        }
    }
}

// TestReceiptEventIdentity 验证同交易同金额的两个事件使用日志序号区分且保留六位金额。
func TestReceiptEventIdentity(t *testing.T) {
    hash := strings.Repeat("a", 64)
    address := "a614f803b6fd780986a42c78ec9c7f77e6ded13c"
    topic := strings.Repeat("0", 24) + address
    entry := fmt.Sprintf(`{"address":"%s","topics":["%s","%s","%s"],"data":"%064x"}`, address, transferTopic, topic, topic, 1000001)
    var receipt solidReceipt
    fixture := fmt.Sprintf(`{"id":"%s","blockTimeStamp":1700000000000,"receipt":{"result":"SUCCESS"},"log":[%s,%s]}`, hash, entry, entry)
    if err := json.Unmarshal([]byte(fixture), &receipt); err != nil {
        t.Fatal(err)
    }
    events, err := receiptIncome(receipt, hash, usdtContract)
    if err != nil || len(events) != 2 {
        t.Fatalf("事件读取失败: %v %v", events, err)
    }
    if events[0].EventKey == events[1].EventKey || events[0].Amount != "1.000001" {
        t.Fatal("事件去重或金额精度错误")
    }
    again, _ := receiptIncome(receipt, hash, usdtContract)
    if again[0].EventKey != events[0].EventKey {
        t.Fatal("重复查询必须保持稳定事件身份")
    }
    receipt.Receipt.Result = "REVERT"
    if _, err := receiptIncome(receipt, hash, usdtContract); err == nil {
        t.Fatal("失败回执不能算到账")
    }
    receipt.Receipt.Result = "SUCCESS"
    if _, err := receiptIncome(receipt, strings.Repeat("b", 64), usdtContract); err == nil {
        t.Fatal("错误交易身份必须拒绝")
    }
}

// TestWrongContractIgnored 验证伪造代币即使发出相同Transfer事件也不计为USDT到账。
func TestWrongContractIgnored(t *testing.T) {
    hash := strings.Repeat("a", 64)
    var receipt solidReceipt
    fixture := fmt.Sprintf(`{"id":"%s","blockTimeStamp":1700000000000,"receipt":{"result":"SUCCESS"},"log":[{"address":"0000000000000000000000000000000000000001","topics":["%s","%064x","%064x"],"data":"%064x"}]}`, hash, transferTopic, 1, 1, 1)
    if err := json.Unmarshal([]byte(fixture), &receipt); err != nil {
        t.Fatal(err)
    }
    events, err := receiptIncome(receipt, hash, usdtContract)
    if err != nil || len(events) != 0 {
        t.Fatal("其它代币不能触发USDT告警")
    }
}
