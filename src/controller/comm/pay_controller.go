package comm

import (
	"fmt"
	"github.com/assimon/luuu/config"
	"github.com/assimon/luuu/model/dao"
	"github.com/assimon/luuu/model/data"
	"github.com/assimon/luuu/model/mdb"
	"github.com/assimon/luuu/model/request"
	"github.com/assimon/luuu/model/response"
	"github.com/assimon/luuu/model/service"
	"github.com/assimon/luuu/mq"
	"github.com/assimon/luuu/mq/handle"
	"github.com/hibiken/asynq"
	"github.com/labstack/echo/v4"
	"html/template"
	"net/http"
)

// CheckoutCounter 收银台
func (c *BaseCommController) CheckoutCounter(ctx echo.Context) (err error) {
	tradeId := ctx.Param("trade_id")
	resp, err := service.GetCheckoutCounterByTradeId(tradeId)
	if err != nil {
		return ctx.String(http.StatusOK, err.Error())
	}
	tmpl, err := template.ParseFiles(fmt.Sprintf(".%s/%s", config.StaticPath, "index.html"))
	if err != nil {
		return ctx.String(http.StatusOK, err.Error())
	}
	return tmpl.Execute(ctx.Response(), resp)
}

// CheckStatus 支付状态检测
func (c *BaseCommController) CheckStatus(ctx echo.Context) (err error) {
	tradeId := ctx.Param("trade_id")
	order, err := service.GetOrderInfoByTradeId(tradeId)
	if err != nil {
		return c.FailJson(ctx, err)
	}
	resp := response.CheckStatusResponse{
		TradeId: order.TradeId,
		Status:  order.Status,
	}
	return c.SucJson(ctx, resp)
}

// CompletePayment 将支付结果设置为完成
func (c *BaseCommController) CompletePayment(ctx echo.Context) (err error) {
	// 检查当前环境是否为beta
	if config.GetProfile() != "beta" {
		return c.FailJson(ctx, fmt.Errorf("this API is only available in beta environment"))
	}

	tradeId := ctx.Param("trade_id")
	order, err := service.GetOrderInfoByTradeId(tradeId)
	if err != nil {
		return c.FailJson(ctx, err)
	}

	// 检查订单是否已过期
	if order.Status == mdb.StatusExpired {
		return c.FailJson(ctx, fmt.Errorf("Order has expired"))
	}

	// 更新订单状态为支付成功
	tx := dao.Mdb.Begin()
	err = data.OrderSuccessWithTransaction(tx, &request.OrderProcessingRequest{
		TradeId:            tradeId,
		BlockTransactionId: "manual_completion",
		Token:              order.Token,
		Amount:             order.ActualAmount,
	})

	if err != nil {
		tx.Rollback()
		return c.FailJson(ctx, err)
	}

	// 解锁交易
	err = data.UnLockTransaction(order.Token, order.ActualAmount)
	if err != nil {
		tx.Rollback()
		return c.FailJson(ctx, err)
	}

	tx.Commit()

	// 回调队列
	orderCallbackQueue, _ := handle.NewOrderCallbackQueue(order)
	mq.MClient.Enqueue(orderCallbackQueue, asynq.MaxRetry(5))

	resp := response.CheckStatusResponse{
		TradeId: order.TradeId,
		Status:  mdb.StatusPaySuccess,
	}
	return c.SucJson(ctx, resp)
}
