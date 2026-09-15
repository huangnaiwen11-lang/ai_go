// local-store-receipt 生成 Apple/Google 商店回执的本地测试格式。
// 仅用于 LocalVerifier 联调，不连接商店，也不生成真实签名。
package main

import (
	"flag"
	"fmt"
	"os"

	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/integrations/appstore"
)

func main() {
	provider := flag.String("provider", "apple", "商店：apple 或 google")
	transaction := flag.String("transaction", "local-transaction-1", "本地交易号")
	storeProduct := flag.String("store-product", "com.example.coins100", "商店商品标识")
	flag.Parse()
	receipt, err := appstore.IssueLocalReceipt(payments.StoreProvider(*provider), *transaction, *storeProduct)
	if err != nil {
		fmt.Fprintln(os.Stderr, "生成本地商店回执失败")
		os.Exit(1)
	}
	fmt.Println(receipt)
}
