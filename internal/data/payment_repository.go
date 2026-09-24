package data

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"ai-business-service/internal/biz/payments"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoPaymentRepository 只管理 Go 自有支付商品、订单、回执、账户余额和账本分录。
// 它不持有 Node 钱包、支付渠道凭证或任何 HTTP 回调协议。
type mongoPaymentRepository struct {
	client        *mongo.Client
	accounts      *mongo.Collection
	products      *mongo.Collection
	orders        *mongo.Collection
	receipts      *mongo.Collection
	ledgerEntries *mongo.Collection
}

// NewPaymentRepository 创建本地支付领域的 MongoDB 适配器。
func NewPaymentRepository(data *Data) payments.PaymentRepository {
	if data == nil || data.client == nil || data.database == nil {
		return nil
	}
	return &mongoPaymentRepository{
		client:        data.client,
		accounts:      data.database.Collection(schema.CollectionAccounts),
		products:      data.database.Collection(schema.CollectionPaymentProducts),
		orders:        data.database.Collection(schema.CollectionPaymentOrders),
		receipts:      data.database.Collection(schema.CollectionPaymentReceipts),
		ledgerEntries: data.database.Collection(schema.CollectionLedgerEntries),
	}
}

// WithinTx 为支付领域提供回执、余额和账本共用的本地 MongoDB 事务边界。
func (repository *mongoPaymentRepository) WithinTx(ctx context.Context, operation func(context.Context) error) error {
	if err := repository.ready(); err != nil {
		return err
	}
	return NewMongoTxRunner(repository.client).WithinTx(ctx, operation)
}

// CreateProduct 保存不可覆盖的本地支付商品版本。
func (repository *mongoPaymentRepository) CreateProduct(ctx context.Context, product payments.PaymentProduct) error {
	if err := repository.ready(); err != nil {
		return err
	}
	if err := product.Validate(); err != nil {
		return err
	}
	document := model.PaymentProductDocument{
		ProductID:     product.ID,
		Version:       product.Version,
		DiamondAmount: product.DiamondAmount,
		AmountCents:   product.AmountCents,
		Currency:      product.Currency,
		Label:         product.Label,
		PublishStatus: string(product.PublishStatus),
	}
	if _, err := repository.products.InsertOne(ctx, document); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("create payment product %q version %d: %w; %w", product.ID, product.Version, payments.ErrPaymentProductAlreadyExists, err)
		}
		return fmt.Errorf("create payment product %q version %d: %w", product.ID, product.Version, err)
	}
	return nil
}

// FindProduct 按商品标识和版本读取不可变的本地商品事实。
func (repository *mongoPaymentRepository) FindProduct(ctx context.Context, productID string, version int64) (*payments.PaymentProduct, error) {
	if err := repository.ready(); err != nil {
		return nil, err
	}
	var document model.PaymentProductDocument
	err := repository.products.FindOne(ctx, bson.D{{Key: "product_id", Value: productID}, {Key: "version", Value: version}}).Decode(&document)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find payment product %q version %d: %w", productID, version, err)
	}
	return &payments.PaymentProduct{
		ID:            document.ProductID,
		Version:       document.Version,
		DiamondAmount: document.DiamondAmount,
		AmountCents:   document.AmountCents,
		Currency:      document.Currency,
		Label:         document.Label,
		PublishStatus: payments.ProductPublishStatus(document.PublishStatus),
	}, nil
}

// FindLatestPublishedProduct 读取当前最高版本的已发布本地商品。
// HTTP 层只允许提交商品标识，具体钻石数与版本必须由此服务端查询冻结。
func (repository *mongoPaymentRepository) FindLatestPublishedProduct(ctx context.Context, productID string) (*payments.PaymentProduct, error) {
	if err := repository.ready(); err != nil {
		return nil, err
	}
	var document model.PaymentProductDocument
	err := repository.products.FindOne(
		ctx,
		bson.D{{Key: "product_id", Value: productID}, {Key: "publish_status", Value: string(payments.ProductPublishStatusPublished)}},
		options.FindOne().SetSort(bson.D{{Key: "version", Value: -1}}),
	).Decode(&document)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find latest published payment product %q: %w", productID, err)
	}
	return &payments.PaymentProduct{
		ID:            document.ProductID,
		Version:       document.Version,
		DiamondAmount: document.DiamondAmount,
		AmountCents:   document.AmountCents,
		Currency:      document.Currency,
		Label:         document.Label,
		PublishStatus: payments.ProductPublishStatus(document.PublishStatus),
	}, nil
}

// CreateOrder 插入已经冻结商品快照的本地待支付订单。
func (repository *mongoPaymentRepository) CreateOrder(ctx context.Context, order payments.PaymentOrder) error {
	if err := repository.ready(); err != nil {
		return err
	}
	if err := order.ValidatePending(); err != nil {
		return err
	}
	if !paymentTransactionActive(ctx) {
		return errors.New("payment order write requires an active MongoDB transaction")
	}
	var product model.PaymentProductDocument
	err := repository.products.FindOne(ctx, bson.D{{Key: "product_id", Value: order.ProductID}, {Key: "version", Value: order.ProductVersion}}).Decode(&product)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return fmt.Errorf("payment product %q version %d is unavailable: %w", order.ProductID, order.ProductVersion, payments.ErrInvalidPaymentOrder)
	}
	if err != nil {
		return fmt.Errorf("find payment product %q version %d while creating order: %w", order.ProductID, order.ProductVersion, err)
	}
	if payments.ProductPublishStatus(product.PublishStatus) != payments.ProductPublishStatusPublished {
		return payments.ErrProductNotPublished
	}
	if product.DiamondAmount != order.DiamondAmount {
		return fmt.Errorf("payment product %q version %d diamond amount does not match order snapshot: %w", order.ProductID, order.ProductVersion, payments.ErrInvalidPaymentOrder)
	}
	if product.AmountCents != order.AmountCents || product.Currency != order.Currency {
		return fmt.Errorf("payment product %q version %d price does not match order snapshot: %w", order.ProductID, order.ProductVersion, payments.ErrInvalidPaymentOrder)
	}
	document := model.PaymentOrderDocument{
		ID:                    order.ID,
		UserID:                order.UserID,
		Provider:              string(order.Provider),
		ProviderOrderID:       order.ProviderOrderID,
		ChannelProvider:       order.ChannelProvider,
		ChannelAccount:        order.ChannelAccount,
		ChannelDevicePlatform: order.ChannelDevicePlatform,
		ProductID:             order.ProductID,
		ProductVersion:        order.ProductVersion,
		DiamondAmount:         order.DiamondAmount,
		AmountCents:           order.AmountCents,
		Currency:              order.Currency,
		Status:                string(order.Status),
		CreatedAt:             order.CreatedAt.UTC(),
		UpdatedAt:             order.UpdatedAt.UTC(),
	}
	if _, err := repository.orders.InsertOne(ctx, document); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("create payment order for provider %q order %q: %w; %w", order.Provider, order.ProviderOrderID, payments.ErrPaymentOrderAlreadyExists, err)
		}
		return fmt.Errorf("create payment order %q: %w", order.ID, err)
	}
	return nil
}

// FindOrder 读取订单冻结快照；查询允许在事务外执行，以便结算前验证可信事实。
func (repository *mongoPaymentRepository) FindOrder(ctx context.Context, orderID string) (*payments.PaymentOrder, error) {
	if err := repository.ready(); err != nil {
		return nil, err
	}
	var document model.PaymentOrderDocument
	err := repository.orders.FindOne(ctx, bson.D{{Key: "_id", Value: orderID}}).Decode(&document)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find payment order %q: %w", orderID, err)
	}
	return &payments.PaymentOrder{
		ID:                    document.ID,
		UserID:                document.UserID,
		Provider:              payments.Provider(document.Provider),
		ProviderOrderID:       document.ProviderOrderID,
		ChannelProvider:       document.ChannelProvider,
		ChannelAccount:        document.ChannelAccount,
		ChannelDevicePlatform: document.ChannelDevicePlatform,
		ProductID:             document.ProductID,
		ProductVersion:        document.ProductVersion,
		DiamondAmount:         document.DiamondAmount,
		AmountCents:           document.AmountCents,
		Currency:              document.Currency,
		Status:                payments.PaymentOrderStatus(document.Status),
		CreatedAt:             document.CreatedAt,
		UpdatedAt:             document.UpdatedAt,
	}, nil
}

// FindOrderByProviderOrder 按渠道及其唯一渠道订单号读取已冻结的本地订单。
func (repository *mongoPaymentRepository) FindOrderByProviderOrder(ctx context.Context, provider payments.Provider, providerOrderID string) (*payments.PaymentOrder, error) {
	if err := repository.ready(); err != nil {
		return nil, err
	}
	var document model.PaymentOrderDocument
	err := repository.orders.FindOne(ctx, bson.D{{Key: "provider", Value: string(provider)}, {Key: "provider_order_id", Value: providerOrderID}}).Decode(&document)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find payment order for provider %q order %q: %w", provider, providerOrderID, err)
	}
	return &payments.PaymentOrder{
		ID:                    document.ID,
		UserID:                document.UserID,
		Provider:              payments.Provider(document.Provider),
		ProviderOrderID:       document.ProviderOrderID,
		ChannelProvider:       document.ChannelProvider,
		ChannelAccount:        document.ChannelAccount,
		ChannelDevicePlatform: document.ChannelDevicePlatform,
		ProductID:             document.ProductID,
		ProductVersion:        document.ProductVersion,
		DiamondAmount:         document.DiamondAmount,
		AmountCents:           document.AmountCents,
		Currency:              document.Currency,
		Status:                payments.PaymentOrderStatus(document.Status),
		CreatedAt:             document.CreatedAt,
		UpdatedAt:             document.UpdatedAt,
	}, nil
}

// BindPayCoresProviderOrder 将本地占位关联原子替换为 PayCores 返回的真实订单号。
// 条件中同时锁定本地订单、占位值和 pending 状态，防止重试或回调竞态覆盖关联关系。
func (repository *mongoPaymentRepository) BindPayCoresProviderOrder(ctx context.Context, localOrderID, provisionalProviderOrderID, providerOrderID string, updatedAt time.Time) (bool, error) {
	if err := repository.ready(); err != nil {
		return false, err
	}
	if localOrderID == "" || provisionalProviderOrderID == "" || providerOrderID == "" || updatedAt.IsZero() {
		return false, payments.ErrInvalidPaymentOrder
	}
	result, err := repository.orders.UpdateOne(ctx,
		bson.D{
			{Key: "_id", Value: localOrderID},
			{Key: "provider", Value: string(payments.ProviderPayCores)},
			{Key: "provider_order_id", Value: provisionalProviderOrderID},
			{Key: "status", Value: string(payments.PaymentOrderStatusPending)},
		},
		bson.D{{Key: "$set", Value: bson.D{{Key: "provider_order_id", Value: providerOrderID}, {Key: "updated_at", Value: updatedAt.UTC()}}}},
	)
	if err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return false, payments.ErrPaymentOrderAlreadyExists
		}
		return false, fmt.Errorf("bind paycores provider order %q to local order %q: %w", providerOrderID, localOrderID, err)
	}
	return result.MatchedCount == 1, nil
}

// MarkOrderPaid 在同一事务内以条件更新完成 pending 到 paid 的一次性迁移。
func (repository *mongoPaymentRepository) MarkOrderPaid(ctx context.Context, orderID string, updatedAt time.Time) (bool, error) {
	if err := repository.ready(); err != nil {
		return false, err
	}
	if !paymentTransactionActive(ctx) {
		return false, errors.New("payment order status write requires an active MongoDB transaction")
	}
	result, err := repository.orders.UpdateOne(
		ctx,
		bson.D{{Key: "_id", Value: orderID}, {Key: "status", Value: string(payments.PaymentOrderStatusPending)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "status", Value: string(payments.PaymentOrderStatusPaid)}, {Key: "updated_at", Value: updatedAt.UTC()}}}},
	)
	if err != nil {
		return false, fmt.Errorf("mark payment order %q paid: %w", orderID, err)
	}
	return result.MatchedCount == 1, nil
}

// FindReceipt 按支付渠道和外部交易号读取已经提交的可信回执。
func (repository *mongoPaymentRepository) FindReceipt(ctx context.Context, provider payments.Provider, externalTransactionID string) (*payments.Receipt, error) {
	if err := repository.ready(); err != nil {
		return nil, err
	}
	var document model.PaymentReceiptDocument
	err := repository.receipts.FindOne(ctx, bson.D{
		{Key: "provider", Value: string(provider)},
		{Key: "external_transaction_id", Value: externalTransactionID},
	}).Decode(&document)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("find payment receipt for provider %q transaction %q: %w", provider, externalTransactionID, err)
	}
	return &payments.Receipt{
		Provider:              payments.Provider(document.Provider),
		ExternalTransactionID: document.ExternalTransactionID,
		PaymentOrderID:        document.PaymentOrderID,
		UserID:                document.UserID,
		DiamondAmount:         document.DiamondAmount,
		BusinessAt:            document.ReceivedAt,
	}, nil
}

// FindDiamondBalance 读取当前事务可见的 Go 自有账户钻石余额。
func (repository *mongoPaymentRepository) FindDiamondBalance(ctx context.Context, userID string) (int64, error) {
	if err := repository.ready(); err != nil {
		return 0, err
	}
	var account model.AccountDocument
	err := repository.accounts.FindOne(ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&account)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("find payment account balance for user %q: %w", userID, err)
	}
	return account.DiamondBalance, nil
}

// CreateReceipt 插入支付渠道交易的唯一事实。
// 重复键仅表示另一事务已经处理同一交易，交由领域层重新读取并收敛为幂等重放。
func (repository *mongoPaymentRepository) CreateReceipt(ctx context.Context, receipt payments.Receipt) error {
	if err := repository.ready(); err != nil {
		return err
	}
	if !paymentTransactionActive(ctx) {
		return errors.New("payment receipt write requires an active MongoDB transaction")
	}
	document := model.PaymentReceiptDocument{
		ID:                    paymentReceiptID(receipt.Provider, receipt.ExternalTransactionID),
		Provider:              string(receipt.Provider),
		ExternalTransactionID: receipt.ExternalTransactionID,
		PaymentOrderID:        receipt.PaymentOrderID,
		UserID:                receipt.UserID,
		DiamondAmount:         receipt.DiamondAmount,
		ReceivedAt:            receipt.BusinessAt,
	}
	if _, err := repository.receipts.InsertOne(ctx, document); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("create payment receipt for provider %q transaction %q: %w; %w", receipt.Provider, receipt.ExternalTransactionID, payments.ErrReceiptAlreadyExists, err)
		}
		return fmt.Errorf("create payment receipt for provider %q transaction %q: %w", receipt.Provider, receipt.ExternalTransactionID, err)
	}
	return nil
}

// CreditDiamonds 通过 upsert 原子增加 Go 自有账户钻石，并返回写入后的余额。
// 支付入账是余额文档的合法物化来源，因此账户尚未存在时在本地账本创建，而不读取旧钱包。
func (repository *mongoPaymentRepository) CreditDiamonds(ctx context.Context, userID string, diamonds int64, businessAt time.Time) (int64, error) {
	if err := repository.ready(); err != nil {
		return 0, err
	}
	if !paymentTransactionActive(ctx) {
		return 0, errors.New("payment diamond credit requires an active MongoDB transaction")
	}
	if diamonds <= 0 || businessAt.IsZero() {
		return 0, errors.New("payment credit requires positive diamonds and business time")
	}
	var account model.AccountDocument
	err := repository.accounts.FindOneAndUpdate(
		ctx,
		bson.D{{Key: "_id", Value: userID}},
		bson.D{
			{Key: "$inc", Value: bson.D{{Key: "diamond_balance", Value: diamonds}}},
			{Key: "$set", Value: bson.D{{Key: "updated_at", Value: businessAt.UTC()}}},
			{Key: "$setOnInsert", Value: bson.D{{Key: "created_at", Value: businessAt.UTC()}}},
		},
		options.FindOneAndUpdate().SetUpsert(true).SetReturnDocument(options.After),
	).Decode(&account)
	if err != nil {
		return 0, fmt.Errorf("credit payment diamonds for user %q: %w", userID, err)
	}
	return account.DiamondBalance, nil
}

// AppendPaymentCredit 记录不可变的本地正向账本分录，并将其关联回支付回执。
func (repository *mongoPaymentRepository) AppendPaymentCredit(ctx context.Context, credit payments.PaymentCredit) error {
	if err := repository.ready(); err != nil {
		return err
	}
	if !paymentTransactionActive(ctx) {
		return errors.New("payment ledger write requires an active MongoDB transaction")
	}
	if credit.Reason == "" || credit.CreatedAt.IsZero() {
		return errors.New("payment credit requires reason and creation time")
	}
	receipt := credit.Receipt
	ledgerEntryID := paymentCreditLedgerEntryID(receipt.Provider, receipt.ExternalTransactionID)
	entry := model.LedgerEntryDocument{
		ID:             ledgerEntryID,
		IdempotencyKey: ledgerEntryID,
		AccountID:      receipt.UserID,
		DeltaDiamonds:  receipt.DiamondAmount,
		Reason:         credit.Reason,
		CreatedAt:      credit.CreatedAt.UTC(),
	}
	if _, err := repository.ledgerEntries.InsertOne(ctx, entry); err != nil {
		return fmt.Errorf("append payment credit ledger entry %q: %w", ledgerEntryID, err)
	}
	result, err := repository.receipts.UpdateOne(
		ctx,
		bson.D{{Key: "_id", Value: paymentReceiptID(receipt.Provider, receipt.ExternalTransactionID)}},
		bson.D{{Key: "$set", Value: bson.D{{Key: "ledger_entry_id", Value: ledgerEntryID}}}},
	)
	if err != nil {
		return fmt.Errorf("link payment receipt to ledger entry %q: %w", ledgerEntryID, err)
	}
	if result.MatchedCount != 1 {
		return fmt.Errorf("payment receipt missing while linking ledger entry %q", ledgerEntryID)
	}
	return nil
}

func (repository *mongoPaymentRepository) ready() error {
	if repository == nil || repository.client == nil || repository.accounts == nil || repository.products == nil || repository.orders == nil || repository.receipts == nil || repository.ledgerEntries == nil {
		return errors.New("payment repository is not configured")
	}
	return nil
}

// paymentTransactionActive 保护三类支付事实不被仓储调用方拆开提交。
// 查询仍允许在事务外运行，供幂等重放在唯一键竞争后读取已提交的回执。
func paymentTransactionActive(ctx context.Context) bool {
	session := mongo.SessionFromContext(ctx)
	return session != nil && session.TransactionRunning()
}

func paymentReceiptID(provider payments.Provider, externalTransactionID string) string {
	return "payment-receipt:" + paymentTransactionDigest(provider, externalTransactionID)
}

func paymentCreditLedgerEntryID(provider payments.Provider, externalTransactionID string) string {
	return "payment-credit:" + paymentTransactionDigest(provider, externalTransactionID)
}

// paymentTransactionDigest 避免把渠道原始交易号直接作为 MongoDB 主键或日志主键。
func paymentTransactionDigest(provider payments.Provider, externalTransactionID string) string {
	sum := sha256.Sum256([]byte(string(provider) + "\x00" + externalTransactionID))
	return hex.EncodeToString(sum[:])
}

var _ payments.PaymentRepository = (*mongoPaymentRepository)(nil)
