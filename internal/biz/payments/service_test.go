package payments

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// 同一渠道交易的重放必须只增加一次本地余额。该测试只传入已经完成渠道校验的回执，
// 不把原始签名、购买凭证或任何 Node 钱包事实带入支付领域。
func TestApplyReceipt同一渠道交易只入账一次(t *testing.T) {
	repository := newMemoryReceiptRepository()
	service := NewService(repository, func() time.Time {
		return time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	})
	receipt := Receipt{
		Provider:              ProviderPayCores,
		ExternalTransactionID: "paycores-transaction-1",
		PaymentOrderID:        "payment-order-1",
		UserID:                "user-1",
		DiamondAmount:         100,
	}

	first, err := service.applyReceipt(context.Background(), receipt)
	if err != nil {
		t.Fatalf("首次 ApplyReceipt() error = %v", err)
	}
	if !first.Applied || first.DiamondBalance != 100 {
		t.Fatalf("首次入账结果 = %#v，期望已入账且余额为 100", first)
	}

	replay, err := service.applyReceipt(context.Background(), receipt)
	if err != nil {
		t.Fatalf("重放 ApplyReceipt() error = %v", err)
	}
	if replay.Applied || replay.DiamondBalance != 100 {
		t.Fatalf("重放结果 = %#v，期望不重复入账且余额仍为 100", replay)
	}
	if got := repository.balance("user-1"); got != 100 {
		t.Fatalf("余额 = %d，期望 100", got)
	}
	if got := repository.creditCount(receipt); got != 1 {
		t.Fatalf("支付入账账本数 = %d，期望 1", got)
	}
}

// 同一渠道交易号只能对应一份不可变的结算事实，不能让重放回调覆盖用户或钻石数。
func TestApplyReceipt同交易不同事实拒绝且不改余额(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*Receipt)
	}{
		{name: "钻石数", mutate: func(receipt *Receipt) { receipt.DiamondAmount = 200 }},
		{name: "支付订单", mutate: func(receipt *Receipt) { receipt.PaymentOrderID = "payment-order-rewritten" }},
		{name: "用户", mutate: func(receipt *Receipt) { receipt.UserID = "user-rewritten" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repository := newMemoryReceiptRepository()
			service := NewService(repository, func() time.Time {
				return time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
			})
			original := Receipt{
				Provider:              ProviderPayCores,
				ExternalTransactionID: "paycores-transaction-conflict",
				PaymentOrderID:        "payment-order-original",
				UserID:                "user-original",
				DiamondAmount:         100,
			}
			if _, err := service.applyReceipt(context.Background(), original); err != nil {
				t.Fatalf("首次 ApplyReceipt() error = %v", err)
			}

			conflicting := original
			testCase.mutate(&conflicting)
			if _, err := service.applyReceipt(context.Background(), conflicting); !errors.Is(err, ErrReceiptConflict) {
				t.Fatalf("冲突回执 error = %v，期望 ErrReceiptConflict", err)
			}
			if got := repository.balance(original.UserID); got != 100 {
				t.Fatalf("冲突后余额 = %d，期望保持 100", got)
			}
			if got := repository.creditCount(original); got != 1 {
				t.Fatalf("冲突后支付入账账本数 = %d，期望保持 1", got)
			}
		})
	}
}

// 本地支付入账是一个原子事务：账本写入失败时不能留下已创建回执或已增加余额。
func TestApplyReceipt账本写入失败时回滚回执与余额(t *testing.T) {
	repository := newMemoryReceiptRepository()
	repository.appendCreditError = errors.New("injected payment credit write failure")
	service := NewService(repository, func() time.Time {
		return time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	})
	receipt := Receipt{
		Provider:              ProviderAppStore,
		ExternalTransactionID: "app-store-transaction-rollback",
		PaymentOrderID:        "payment-order-rollback",
		UserID:                "user-rollback",
		DiamondAmount:         100,
	}

	if _, err := service.applyReceipt(context.Background(), receipt); !errors.Is(err, repository.appendCreditError) {
		t.Fatalf("ApplyReceipt() error = %v，期望注入的账本错误", err)
	}
	if got := repository.balance(receipt.UserID); got != 0 {
		t.Fatalf("失败后的余额 = %d，期望回滚至 0", got)
	}
	if stored, err := repository.FindReceipt(context.Background(), receipt.Provider, receipt.ExternalTransactionID); err != nil || stored != nil {
		t.Fatalf("失败后回执 = %#v, error = %v，期望不残留回执", stored, err)
	}
	if got := repository.creditCount(receipt); got != 0 {
		t.Fatalf("失败后支付入账账本数 = %d，期望 0", got)
	}
}

// 余额写入属于同一入账事务：它失败时已创建的回执也必须回滚。
func TestApplyReceipt余额写入失败时回滚回执与账本(t *testing.T) {
	repository := newMemoryReceiptRepository()
	repository.creditDiamondsError = errors.New("injected diamond credit failure")
	service := NewService(repository, func() time.Time {
		return time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	})
	receipt := Receipt{
		Provider:              ProviderAppStore,
		ExternalTransactionID: "app-store-transaction-credit-failure",
		PaymentOrderID:        "payment-order-credit-failure",
		UserID:                "user-credit-failure",
		DiamondAmount:         100,
	}

	if _, err := service.applyReceipt(context.Background(), receipt); !errors.Is(err, repository.creditDiamondsError) {
		t.Fatalf("ApplyReceipt() error = %v，期望注入的余额写入错误", err)
	}
	if got := repository.balance(receipt.UserID); got != 0 {
		t.Fatalf("失败后的余额 = %d，期望 0", got)
	}
	if stored, err := repository.FindReceipt(context.Background(), receipt.Provider, receipt.ExternalTransactionID); err != nil || stored != nil {
		t.Fatalf("失败后回执 = %#v, error = %v，期望不残留回执", stored, err)
	}
	if got := repository.creditCount(receipt); got != 0 {
		t.Fatalf("失败后支付入账账本数 = %d，期望 0", got)
	}
}

// 两个事务同时首次处理同一交易时，后到事务会遇到唯一键冲突。
// 它必须读取先提交的事实并收敛为幂等重放，而不是把正常重试误报为业务冲突。
func TestApplyReceipt并发唯一键冲突收敛为幂等重放(t *testing.T) {
	repository := newMemoryReceiptRepository()
	service := NewService(repository, func() time.Time {
		return time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	})
	receipt := Receipt{
		Provider:              ProviderPayCores,
		ExternalTransactionID: "paycores-transaction-race",
		PaymentOrderID:        "payment-order-race",
		UserID:                "user-race",
		DiamondAmount:         100,
	}
	// 模拟另一个事务已经完成入账，但当前事务第一次读取时还看不到该提交。
	repository.raceWinnerReceipt = &receipt
	repository.raceWinnerBalance = 100

	result, err := service.applyReceipt(context.Background(), receipt)
	if err != nil {
		t.Fatalf("并发重放 ApplyReceipt() error = %v", err)
	}
	if result.Applied || result.DiamondBalance != 100 {
		t.Fatalf("并发重放结果 = %#v，期望不重复入账且余额为 100", result)
	}
	if got := repository.creditCount(receipt); got != 0 {
		t.Fatalf("当前失败事务产生的支付入账账本数 = %d，期望 0", got)
	}
}

// 多个协程同时重放同一可信回执时，必须恰好有一次入账。
func TestApplyReceipt并发重放只入账一次(t *testing.T) {
	repository := newMemoryReceiptRepository()
	service := NewService(repository, func() time.Time {
		return time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	})
	receipt := Receipt{
		Provider:              ProviderPayCores,
		ExternalTransactionID: "paycores-transaction-concurrent",
		PaymentOrderID:        "payment-order-concurrent",
		UserID:                "user-concurrent",
		DiamondAmount:         100,
	}

	const callers = 8
	results := make(chan ApplyResult, callers)
	errors := make(chan error, callers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			result, err := service.applyReceipt(context.Background(), receipt)
			results <- result
			errors <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errors)

	appliedCount := 0
	for err := range errors {
		if err != nil {
			t.Fatalf("并发 ApplyReceipt() error = %v", err)
		}
	}
	for result := range results {
		if result.Applied {
			appliedCount++
		}
		if result.DiamondBalance != 100 {
			t.Fatalf("并发结果 = %#v，期望余额为 100", result)
		}
	}
	if appliedCount != 1 {
		t.Fatalf("已入账调用数 = %d，期望 1", appliedCount)
	}
	if got := repository.balance(receipt.UserID); got != 100 {
		t.Fatalf("并发后余额 = %d，期望 100", got)
	}
	if got := repository.creditCount(receipt); got != 1 {
		t.Fatalf("并发后支付入账账本数 = %d，期望 1", got)
	}
}

// 支付正向分录必须带稳定的 payment_credit 原因，避免和生成预扣或冲正分录混淆。
func TestApplyReceipt写入固定支付入账原因(t *testing.T) {
	repository := newMemoryReceiptRepository()
	service := NewService(repository, func() time.Time {
		return time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	})
	receipt := Receipt{
		Provider:              ProviderAppStore,
		ExternalTransactionID: "app-store-transaction-reason",
		PaymentOrderID:        "payment-order-reason",
		UserID:                "user-reason",
		DiamondAmount:         100,
	}

	if _, err := service.applyReceipt(context.Background(), receipt); err != nil {
		t.Fatalf("ApplyReceipt() error = %v", err)
	}
	if credit := repository.credit(receipt); credit.reason != paymentCreditReason {
		t.Fatalf("支付账本原因 = %q，期望 %q", credit.reason, paymentCreditReason)
	}
}

// 回执标识全空白时没有可追溯性，不能进入唯一键或账本。
func TestApplyReceipt拒绝全空白标识(t *testing.T) {
	service := NewService(newMemoryReceiptRepository(), func() time.Time {
		return time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
	})
	base := Receipt{
		Provider:              ProviderPayCores,
		ExternalTransactionID: "transaction-whitespace",
		PaymentOrderID:        "order-whitespace",
		UserID:                "user-whitespace",
		DiamondAmount:         100,
	}

	for _, testCase := range []struct {
		name   string
		mutate func(*Receipt)
	}{
		{name: "外部交易号", mutate: func(receipt *Receipt) { receipt.ExternalTransactionID = " \t" }},
		{name: "支付订单号", mutate: func(receipt *Receipt) { receipt.PaymentOrderID = "\n" }},
		{name: "用户标识", mutate: func(receipt *Receipt) { receipt.UserID = "  " }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			receipt := base
			testCase.mutate(&receipt)
			if _, err := service.applyReceipt(context.Background(), receipt); !errors.Is(err, ErrInvalidReceipt) {
				t.Fatalf("ApplyReceipt() error = %v，期望 ErrInvalidReceipt", err)
			}
		})
	}
}

// 已有匹配回执却仍是 pending 表示事务事实不完整，结算不能擅自补标订单状态。
func TestSettleVerifiedOrder拒绝已有回执但订单仍Pending(t *testing.T) {
	repository := newMemoryReceiptRepository()
	order := paymentOrderFixture("order-pending-with-receipt", "user-pending-with-receipt", ProviderPayCores, "provider-order-pending-with-receipt", 100)
	repository.seedOrder(order)
	settlement := verifiedOrderSettlementFixture(order, "transaction-pending-with-receipt")
	receipt := Receipt{
		Provider:              order.Provider,
		ExternalTransactionID: settlement.ExternalTransactionID,
		PaymentOrderID:        order.ID,
		UserID:                order.UserID,
		DiamondAmount:         order.DiamondAmount,
		BusinessAt:            settlement.BusinessAt,
	}
	repository.receipts[receiptKey{provider: receipt.Provider, externalTransactionID: receipt.ExternalTransactionID}] = receipt
	repository.balances[order.UserID] = order.DiamondAmount
	repository.credits[receiptKey{provider: receipt.Provider, externalTransactionID: receipt.ExternalTransactionID}] = memoryPaymentCredit{receipt: receipt, reason: paymentCreditReason}
	service := NewService(repository, fixedPaymentTime)

	if _, err := service.SettleVerifiedOrder(context.Background(), settlement); !errors.Is(err, ErrPaymentSettlementInconsistent) {
		t.Fatalf("SettleVerifiedOrder() error = %v，期望 ErrPaymentSettlementInconsistent", err)
	}
	assertMemoryOrderStatus(t, repository, order.ID, PaymentOrderStatusPending)
	if got := repository.balance(order.UserID); got != order.DiamondAmount {
		t.Fatalf("异常结算后的余额 = %d，期望保持 %d", got, order.DiamondAmount)
	}
}

// paid 订单找不到匹配回执同样表示事务事实不完整，不能被当成普通重复结算。
func TestSettleVerifiedOrder拒绝已Paid却没有匹配回执(t *testing.T) {
	repository := newMemoryReceiptRepository()
	order := paymentOrderFixture("order-paid-without-receipt", "user-paid-without-receipt", ProviderPayCores, "provider-order-paid-without-receipt", 100)
	order.Status = PaymentOrderStatusPaid
	repository.seedOrder(order)
	settlement := verifiedOrderSettlementFixture(order, "transaction-paid-without-receipt")
	service := NewService(repository, fixedPaymentTime)

	if _, err := service.SettleVerifiedOrder(context.Background(), settlement); !errors.Is(err, ErrPaymentSettlementInconsistent) {
		t.Fatalf("SettleVerifiedOrder() error = %v，期望 ErrPaymentSettlementInconsistent", err)
	}
	assertMemoryOrderStatus(t, repository, order.ID, PaymentOrderStatusPaid)
	if receipt, err := repository.FindReceipt(context.Background(), order.Provider, settlement.ExternalTransactionID); err != nil || receipt != nil {
		t.Fatalf("异常结算后的回执 = %#v，error = %v，期望保持不存在", receipt, err)
	}
}

// paid 订单的外部交易若关联到其他订单，也属于本地结算事实损坏。
func TestSettleVerifiedOrder拒绝已Paid却有不匹配回执(t *testing.T) {
	repository := newMemoryReceiptRepository()
	order := paymentOrderFixture("order-paid-with-conflicting-receipt", "user-paid-with-conflicting-receipt", ProviderPayCores, "provider-order-paid-with-conflicting-receipt", 100)
	order.Status = PaymentOrderStatusPaid
	repository.seedOrder(order)
	settlement := verifiedOrderSettlementFixture(order, "transaction-paid-with-conflicting-receipt")
	repository.receipts[receiptKey{provider: order.Provider, externalTransactionID: settlement.ExternalTransactionID}] = Receipt{
		Provider:              order.Provider,
		ExternalTransactionID: settlement.ExternalTransactionID,
		PaymentOrderID:        "other-order",
		UserID:                "other-user",
		DiamondAmount:         order.DiamondAmount,
		BusinessAt:            settlement.BusinessAt,
	}
	service := NewService(repository, fixedPaymentTime)

	if _, err := service.SettleVerifiedOrder(context.Background(), settlement); !errors.Is(err, ErrPaymentSettlementInconsistent) {
		t.Fatalf("SettleVerifiedOrder() error = %v，期望 ErrPaymentSettlementInconsistent", err)
	}
	assertMemoryOrderStatus(t, repository, order.ID, PaymentOrderStatusPaid)
}

// 已冻结订单必须按创建时的钻石数结算，商品后续发布的新版本不能改变这笔订单。
func TestSettleVerifiedOrder按订单冻结钻石数结算(t *testing.T) {
	repository := newMemoryReceiptRepository()
	productV1 := PaymentProduct{ID: "coins-frozen", Version: 1, DiamondAmount: 100, PublishStatus: ProductPublishStatusPublished}
	if err := repository.CreateProduct(context.Background(), productV1); err != nil {
		t.Fatalf("CreateProduct() v1 error = %v", err)
	}
	order, err := FreezeOrder(CreateOrderInput{
		OrderID:         "order-frozen",
		UserID:          "user-frozen",
		Provider:        ProviderPayCores,
		ProviderOrderID: "provider-order-frozen",
	}, productV1, fixedPaymentTime())
	if err != nil {
		t.Fatalf("FreezeOrder() error = %v", err)
	}
	if err := repository.WithinTx(context.Background(), func(txCtx context.Context) error {
		return repository.CreateOrder(txCtx, *order)
	}); err != nil {
		t.Fatalf("CreateOrder() error = %v", err)
	}
	// 商品后来更新到更高版本和金额；结算输入中没有也不允许携带该数值。
	laterProduct := productV1
	laterProduct.Version = 2
	laterProduct.DiamondAmount = 200
	if err := repository.CreateProduct(context.Background(), laterProduct); err != nil {
		t.Fatalf("CreateProduct() v2 error = %v", err)
	}
	service := NewService(repository, fixedPaymentTime)

	result, err := service.SettleVerifiedOrder(context.Background(), VerifiedOrderSettlement{
		PaymentOrderID:        order.ID,
		UserID:                order.UserID,
		Provider:              order.Provider,
		ProviderOrderID:       order.ProviderOrderID,
		ExternalTransactionID: "transaction-frozen",
		BusinessAt:            fixedPaymentTime(),
	})
	if err != nil {
		t.Fatalf("SettleVerifiedOrder() error = %v", err)
	}
	if !result.Applied || result.DiamondBalance != order.DiamondAmount {
		t.Fatalf("结算结果 = %#v，期望按冻结钻石数 %d 入账", result, order.DiamondAmount)
	}
	assertMemoryOrderStatus(t, repository, order.ID, PaymentOrderStatusPaid)
}

// 回调关联必须逐项与本地冻结订单一致，任何不一致都不能改变订单或余额。
func TestSettleVerifiedOrder拒绝不匹配的订单关联(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		mutate func(*VerifiedOrderSettlement)
	}{
		{name: "用户", mutate: func(settlement *VerifiedOrderSettlement) { settlement.UserID = "other-user" }},
		{name: "渠道", mutate: func(settlement *VerifiedOrderSettlement) { settlement.Provider = ProviderAppStore }},
		{name: "渠道订单号", mutate: func(settlement *VerifiedOrderSettlement) { settlement.ProviderOrderID = "other-provider-order" }},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repository := newMemoryReceiptRepository()
			order := paymentOrderFixture("order-mismatch-"+testCase.name, "user-mismatch", ProviderPayCores, "provider-order-mismatch", 100)
			repository.seedOrder(order)
			service := NewService(repository, fixedPaymentTime)
			settlement := VerifiedOrderSettlement{
				PaymentOrderID:        order.ID,
				UserID:                order.UserID,
				Provider:              order.Provider,
				ProviderOrderID:       order.ProviderOrderID,
				ExternalTransactionID: "transaction-mismatch-" + testCase.name,
				BusinessAt:            fixedPaymentTime(),
			}
			testCase.mutate(&settlement)

			if _, err := service.SettleVerifiedOrder(context.Background(), settlement); !errors.Is(err, ErrPaymentOrderMismatch) {
				t.Fatalf("SettleVerifiedOrder() error = %v，期望 ErrPaymentOrderMismatch", err)
			}
			if got := repository.balance(order.UserID); got != 0 {
				t.Fatalf("拒绝后的余额 = %d，期望 0", got)
			}
			if got := repository.creditCount(Receipt{Provider: settlement.Provider, ExternalTransactionID: settlement.ExternalTransactionID}); got != 0 {
				t.Fatalf("拒绝后的支付账本数 = %d，期望 0", got)
			}
			assertMemoryOrderStatus(t, repository, order.ID, PaymentOrderStatusPending)
		})
	}
}

// 同一外部交易的并发回调只允许一次入账；其余调用收敛为幂等重放，订单最终为 paid。
func TestSettleVerifiedOrder并发重放只入账一次(t *testing.T) {
	repository := newMemoryReceiptRepository()
	order := paymentOrderFixture("order-concurrent", "user-concurrent-order", ProviderPayCores, "provider-order-concurrent", 100)
	repository.seedOrder(order)
	service := NewService(repository, fixedPaymentTime)
	settlement := VerifiedOrderSettlement{
		PaymentOrderID:        order.ID,
		UserID:                order.UserID,
		Provider:              order.Provider,
		ProviderOrderID:       order.ProviderOrderID,
		ExternalTransactionID: "transaction-concurrent-order",
		BusinessAt:            fixedPaymentTime(),
	}

	const callers = 8
	results := make(chan ApplyResult, callers)
	errors := make(chan error, callers)
	start := make(chan struct{})
	var group sync.WaitGroup
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			result, err := service.SettleVerifiedOrder(context.Background(), settlement)
			results <- result
			errors <- err
		}()
	}
	close(start)
	group.Wait()
	close(results)
	close(errors)

	appliedCount := 0
	for err := range errors {
		if err != nil {
			t.Fatalf("并发 SettleVerifiedOrder() error = %v", err)
		}
	}
	for result := range results {
		if result.Applied {
			appliedCount++
		}
		if result.DiamondBalance != order.DiamondAmount {
			t.Fatalf("并发结算结果 = %#v，期望余额为 %d", result, order.DiamondAmount)
		}
	}
	if appliedCount != 1 {
		t.Fatalf("已入账调用数 = %d，期望 1", appliedCount)
	}
	if got := repository.balance(order.UserID); got != order.DiamondAmount {
		t.Fatalf("并发后的余额 = %d，期望 %d", got, order.DiamondAmount)
	}
	if got := repository.creditCount(Receipt{Provider: order.Provider, ExternalTransactionID: settlement.ExternalTransactionID}); got != 1 {
		t.Fatalf("并发后的支付账本数 = %d，期望 1", got)
	}
	assertMemoryOrderStatus(t, repository, order.ID, PaymentOrderStatusPaid)
}

// 账本失败时，回执、余额和订单 paid 状态必须都由同一事务回滚。
func TestSettleVerifiedOrder账本失败时回滚订单回执与余额(t *testing.T) {
	repository := newMemoryReceiptRepository()
	repository.appendCreditError = errors.New("injected payment credit write failure")
	order := paymentOrderFixture("order-rollback", "user-rollback-order", ProviderAppStore, "provider-order-rollback", 100)
	repository.seedOrder(order)
	service := NewService(repository, fixedPaymentTime)
	settlement := VerifiedOrderSettlement{
		PaymentOrderID:        order.ID,
		UserID:                order.UserID,
		Provider:              order.Provider,
		ProviderOrderID:       order.ProviderOrderID,
		ExternalTransactionID: "transaction-rollback-order",
		BusinessAt:            fixedPaymentTime(),
	}

	if _, err := service.SettleVerifiedOrder(context.Background(), settlement); !errors.Is(err, repository.appendCreditError) {
		t.Fatalf("SettleVerifiedOrder() error = %v，期望注入的账本错误", err)
	}
	if got := repository.balance(order.UserID); got != 0 {
		t.Fatalf("失败后的余额 = %d，期望 0", got)
	}
	if receipt, err := repository.FindReceipt(context.Background(), order.Provider, settlement.ExternalTransactionID); err != nil || receipt != nil {
		t.Fatalf("失败后的回执 = %#v，error = %v，期望不残留回执", receipt, err)
	}
	if got := repository.creditCount(Receipt{Provider: order.Provider, ExternalTransactionID: settlement.ExternalTransactionID}); got != 0 {
		t.Fatalf("失败后的支付账本数 = %d，期望 0", got)
	}
	assertMemoryOrderStatus(t, repository, order.ID, PaymentOrderStatusPending)
}

// 同一外部交易不能改绑到另一张订单或另一位用户。
func TestSettleVerifiedOrder拒绝外部交易改绑订单或用户(t *testing.T) {
	repository := newMemoryReceiptRepository()
	firstOrder := paymentOrderFixture("order-original", "user-original", ProviderPayCores, "provider-order-original", 100)
	secondOrder := paymentOrderFixture("order-rewritten", "user-rewritten", ProviderPayCores, "provider-order-rewritten", 200)
	repository.seedOrder(firstOrder)
	repository.seedOrder(secondOrder)
	service := NewService(repository, fixedPaymentTime)
	transactionID := "transaction-conflicting-order"
	if _, err := service.SettleVerifiedOrder(context.Background(), VerifiedOrderSettlement{
		PaymentOrderID: firstOrder.ID, UserID: firstOrder.UserID, Provider: firstOrder.Provider, ProviderOrderID: firstOrder.ProviderOrderID,
		ExternalTransactionID: transactionID, BusinessAt: fixedPaymentTime(),
	}); err != nil {
		t.Fatalf("首次 SettleVerifiedOrder() error = %v", err)
	}
	if _, err := service.SettleVerifiedOrder(context.Background(), VerifiedOrderSettlement{
		PaymentOrderID: secondOrder.ID, UserID: secondOrder.UserID, Provider: secondOrder.Provider, ProviderOrderID: secondOrder.ProviderOrderID,
		ExternalTransactionID: transactionID, BusinessAt: fixedPaymentTime(),
	}); !errors.Is(err, ErrReceiptConflict) {
		t.Fatalf("改绑交易 error = %v，期望 ErrReceiptConflict", err)
	}
	if got := repository.balance(secondOrder.UserID); got != 0 {
		t.Fatalf("改绑后的第二用户余额 = %d，期望 0", got)
	}
	assertMemoryOrderStatus(t, repository, secondOrder.ID, PaymentOrderStatusPending)
}

// memoryReceiptRepository 仅用于领域单元测试，模拟支付入账必须保持原子性的三类事实。
type memoryReceiptRepository struct {
	// transactionMu 保护完整内存事务，模拟数据库的原子提交边界。
	transactionMu       sync.Mutex
	products            map[productKey]PaymentProduct
	orders              map[string]PaymentOrder
	receipts            map[receiptKey]Receipt
	balances            map[string]int64
	credits             map[receiptKey]memoryPaymentCredit
	appendCreditError   error
	creditDiamondsError error
	// raceWinnerReceipt 模拟外部并发事务的已提交事实，不属于当前内存事务回滚范围。
	raceWinnerReceipt *Receipt
	raceWinnerBalance int64
	raceWinnerVisible bool
}

type receiptKey struct {
	provider              Provider
	externalTransactionID string
}

type productKey struct {
	id      string
	version int64
}

type memoryPaymentCredit struct {
	receipt Receipt
	reason  string
}

type receiptRepositorySnapshot struct {
	products map[productKey]PaymentProduct
	orders   map[string]PaymentOrder
	receipts map[receiptKey]Receipt
	balances map[string]int64
	credits  map[receiptKey]memoryPaymentCredit
}

func newMemoryReceiptRepository() *memoryReceiptRepository {
	return &memoryReceiptRepository{
		products: make(map[productKey]PaymentProduct),
		orders:   make(map[string]PaymentOrder),
		receipts: make(map[receiptKey]Receipt),
		balances: make(map[string]int64),
		credits:  make(map[receiptKey]memoryPaymentCredit),
	}
}

func (repository *memoryReceiptRepository) CreateProduct(_ context.Context, product PaymentProduct) error {
	if err := product.Validate(); err != nil {
		return err
	}
	key := productKey{id: product.ID, version: product.Version}
	if _, exists := repository.products[key]; exists {
		return ErrPaymentProductAlreadyExists
	}
	repository.products[key] = product
	return nil
}

func (repository *memoryReceiptRepository) FindProduct(_ context.Context, productID string, version int64) (*PaymentProduct, error) {
	product, exists := repository.products[productKey{id: productID, version: version}]
	if !exists {
		return nil, nil
	}
	return &product, nil
}

func (repository *memoryReceiptRepository) CreateOrder(_ context.Context, order PaymentOrder) error {
	if err := order.ValidatePending(); err != nil {
		return err
	}
	if _, exists := repository.orders[order.ID]; exists {
		return ErrPaymentOrderAlreadyExists
	}
	repository.orders[order.ID] = order
	return nil
}

func (repository *memoryReceiptRepository) FindOrder(_ context.Context, orderID string) (*PaymentOrder, error) {
	order, exists := repository.orders[orderID]
	if !exists {
		return nil, nil
	}
	return &order, nil
}

func (repository *memoryReceiptRepository) FindOrderByProviderOrder(_ context.Context, provider Provider, providerOrderID string) (*PaymentOrder, error) {
	for _, order := range repository.orders {
		if order.Provider == provider && order.ProviderOrderID == providerOrderID {
			return &order, nil
		}
	}
	return nil, nil
}

func (repository *memoryReceiptRepository) MarkOrderPaid(_ context.Context, orderID string, updatedAt time.Time) (bool, error) {
	order, exists := repository.orders[orderID]
	if !exists || order.Status != PaymentOrderStatusPending {
		return false, nil
	}
	order.Status = PaymentOrderStatusPaid
	order.UpdatedAt = updatedAt.UTC()
	repository.orders[orderID] = order
	return true, nil
}

func (repository *memoryReceiptRepository) WithinTx(ctx context.Context, operation func(context.Context) error) error {
	repository.transactionMu.Lock()
	defer repository.transactionMu.Unlock()

	snapshot := repository.snapshot()
	if err := operation(ctx); err != nil {
		repository.restore(snapshot)
		return err
	}
	return nil
}

func (repository *memoryReceiptRepository) FindReceipt(_ context.Context, provider Provider, externalTransactionID string) (*Receipt, error) {
	if repository.raceWinnerReceipt != nil && repository.raceWinnerReceipt.Provider == provider && repository.raceWinnerReceipt.ExternalTransactionID == externalTransactionID && repository.raceWinnerVisible {
		receipt := *repository.raceWinnerReceipt
		return &receipt, nil
	}
	receipt, ok := repository.receipts[receiptKey{provider: provider, externalTransactionID: externalTransactionID}]
	if !ok {
		return nil, nil
	}
	return &receipt, nil
}

func (repository *memoryReceiptRepository) FindDiamondBalance(_ context.Context, userID string) (int64, error) {
	if repository.raceWinnerReceipt != nil && repository.raceWinnerVisible && repository.raceWinnerReceipt.UserID == userID {
		return repository.raceWinnerBalance, nil
	}
	return repository.balances[userID], nil
}

func (repository *memoryReceiptRepository) CreateReceipt(_ context.Context, receipt Receipt) error {
	if repository.raceWinnerReceipt != nil && repository.raceWinnerReceipt.Provider == receipt.Provider && repository.raceWinnerReceipt.ExternalTransactionID == receipt.ExternalTransactionID {
		repository.raceWinnerVisible = true
		return ErrReceiptAlreadyExists
	}
	key := receiptKey{provider: receipt.Provider, externalTransactionID: receipt.ExternalTransactionID}
	if _, exists := repository.receipts[key]; exists {
		return fmt.Errorf("duplicate receipt: %w", ErrReceiptConflict)
	}
	repository.receipts[key] = receipt
	return nil
}

func (repository *memoryReceiptRepository) CreditDiamonds(_ context.Context, userID string, diamonds int64, _ time.Time) (int64, error) {
	if repository.creditDiamondsError != nil {
		return 0, repository.creditDiamondsError
	}
	if diamonds < 0 {
		return 0, errors.New("negative credit is not allowed")
	}
	repository.balances[userID] += diamonds
	return repository.balances[userID], nil
}

func (repository *memoryReceiptRepository) AppendPaymentCredit(_ context.Context, credit PaymentCredit) error {
	if repository.appendCreditError != nil {
		return repository.appendCreditError
	}
	receipt := credit.Receipt
	key := receiptKey{provider: receipt.Provider, externalTransactionID: receipt.ExternalTransactionID}
	if _, exists := repository.credits[key]; exists {
		return fmt.Errorf("duplicate payment credit: %w", ErrReceiptConflict)
	}
	repository.credits[key] = memoryPaymentCredit{receipt: receipt, reason: credit.Reason}
	return nil
}

func (repository *memoryReceiptRepository) balance(userID string) int64 {
	return repository.balances[userID]
}

func (repository *memoryReceiptRepository) creditCount(receipt Receipt) int {
	_, exists := repository.credits[receiptKey{provider: receipt.Provider, externalTransactionID: receipt.ExternalTransactionID}]
	if !exists {
		return 0
	}
	return 1
}

func (repository *memoryReceiptRepository) credit(receipt Receipt) memoryPaymentCredit {
	return repository.credits[receiptKey{provider: receipt.Provider, externalTransactionID: receipt.ExternalTransactionID}]
}

func (repository *memoryReceiptRepository) snapshot() receiptRepositorySnapshot {
	snapshot := receiptRepositorySnapshot{
		products: make(map[productKey]PaymentProduct, len(repository.products)),
		orders:   make(map[string]PaymentOrder, len(repository.orders)),
		receipts: make(map[receiptKey]Receipt, len(repository.receipts)),
		balances: make(map[string]int64, len(repository.balances)),
		credits:  make(map[receiptKey]memoryPaymentCredit, len(repository.credits)),
	}
	for key, product := range repository.products {
		snapshot.products[key] = product
	}
	for orderID, order := range repository.orders {
		snapshot.orders[orderID] = order
	}
	for key, receipt := range repository.receipts {
		snapshot.receipts[key] = receipt
	}
	for userID, balance := range repository.balances {
		snapshot.balances[userID] = balance
	}
	for key, credit := range repository.credits {
		snapshot.credits[key] = credit
	}
	return snapshot
}

func (repository *memoryReceiptRepository) restore(snapshot receiptRepositorySnapshot) {
	repository.products = snapshot.products
	repository.orders = snapshot.orders
	repository.receipts = snapshot.receipts
	repository.balances = snapshot.balances
	repository.credits = snapshot.credits
}

func (repository *memoryReceiptRepository) seedOrder(order PaymentOrder) {
	repository.orders[order.ID] = order
}

func paymentOrderFixture(id, userID string, provider Provider, providerOrderID string, diamondAmount int64) PaymentOrder {
	at := fixedPaymentTime()
	return PaymentOrder{
		ID:              id,
		UserID:          userID,
		Provider:        provider,
		ProviderOrderID: providerOrderID,
		ProductID:       "coins-100",
		ProductVersion:  1,
		DiamondAmount:   diamondAmount,
		Status:          PaymentOrderStatusPending,
		CreatedAt:       at,
		UpdatedAt:       at,
	}
}

func verifiedOrderSettlementFixture(order PaymentOrder, externalTransactionID string) VerifiedOrderSettlement {
	return VerifiedOrderSettlement{
		PaymentOrderID:        order.ID,
		UserID:                order.UserID,
		Provider:              order.Provider,
		ProviderOrderID:       order.ProviderOrderID,
		ExternalTransactionID: externalTransactionID,
		BusinessAt:            fixedPaymentTime(),
	}
}

func fixedPaymentTime() time.Time {
	return time.Date(2026, time.September, 9, 12, 0, 0, 0, time.UTC)
}

func assertMemoryOrderStatus(t *testing.T, repository *memoryReceiptRepository, orderID string, want PaymentOrderStatus) {
	t.Helper()
	order, err := repository.FindOrder(context.Background(), orderID)
	if err != nil || order == nil || order.Status != want {
		t.Fatalf("订单状态 = %#v，error = %v，期望 %q", order, err, want)
	}
}

var _ Repository = (*memoryReceiptRepository)(nil)
var _ PaymentRepository = (*memoryReceiptRepository)(nil)
