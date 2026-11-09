package service

import (
    "encoding/json"
    "fmt"
    "strings"
    "testing"
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
