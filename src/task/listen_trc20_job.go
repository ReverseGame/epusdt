package task

import (
    "github.com/assimon/luuu/model/data"
    "github.com/assimon/luuu/model/service"
    "github.com/assimon/luuu/util/log"
    "sync"
)

type ListenTrc20Job struct {
}

var gListenTrc20JobLock sync.Mutex

// Run 无入参，启动独立收入核对并执行本轮支付监听，无返回值。
func (r ListenTrc20Job) Run() {
    // 核对任务内部异步并发上限为1；启动在支付锁外，钱包查询失败仍会执行。
    service.StartIncomeAudit()
    gListenTrc20JobLock.Lock()
    defer gListenTrc20JobLock.Unlock()
    walletAddress, err := data.GetAvailableWalletAddress()
    if err != nil {
        log.Sugar.Error(err)
        return
    }
    if len(walletAddress) <= 0 {
        return
    }
    var wg sync.WaitGroup
    for _, address := range walletAddress {
        wg.Add(1)
        go service.Trc20CallBack(address.Token, &wg)
    }
    wg.Wait()
}
