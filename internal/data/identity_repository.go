package data

import (
	"context"
	"errors"
	"fmt"
	"time"

	"ai-business-service/internal/biz/authcredential"
	"ai-business-service/internal/biz/identity"
	"ai-business-service/internal/data/model"
	"ai-business-service/internal/data/schema"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

type mongoUserRepository struct {
	collection *mongo.Collection
}

type mongoIdentityRepository struct {
	collection *mongo.Collection
}

type mongoSessionRepository struct {
	collection *mongo.Collection
}

type mongoCredentialRepository struct{ collection *mongo.Collection }
type mongoAccountRepository struct{ collection *mongo.Collection }

// NewUserRepository 返回用户状态仓储的领域接口实现。
func NewUserRepository(data *Data) identity.UserRepository {
	return &mongoUserRepository{collection: data.database.Collection(schema.CollectionUsers)}
}

// NewAuthUserRepository 返回支持注册和设备游客查询的用户仓储。
func NewAuthUserRepository(data *Data) *mongoUserRepository {
	return &mongoUserRepository{collection: data.database.Collection(schema.CollectionUsers)}
}

// UpdateDisplayName 只更新用户公开昵称；资料接口不会写入时区、账户状态或结算字段。
func (repository *mongoUserRepository) UpdateDisplayName(ctx context.Context, userID, displayName string, updatedAt time.Time) (*identity.User, error) {
	var document model.UserDocument
	err := repository.collection.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: userID}, {Key: "account_status", Value: string(identity.AccountStatusNormal)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "display_name", Value: displayName}, {Key: "updated_at", Value: updatedAt}}}}, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&document)
	if err != nil {
		return nil, mapUserReadError(err)
	}
	return toBizUser(document), nil
}

// UpdateProfile 仅持久化资料领域允许的昵称和简介，不能越权修改账户或时区事实。
func (repository *mongoUserRepository) UpdateProfile(ctx context.Context, userID, displayName, bio string, avatarImageID *string, updatedAt time.Time) (*identity.User, error) {
	var document model.UserDocument
	fields := bson.D{{Key: "display_name", Value: displayName}, {Key: "bio", Value: bio}, {Key: "updated_at", Value: updatedAt}}
	if avatarImageID != nil {
		fields = append(fields, bson.E{Key: "avatar_image_id", Value: *avatarImageID})
	}
	err := repository.collection.FindOneAndUpdate(ctx, bson.D{{Key: "_id", Value: userID}, {Key: "account_status", Value: string(identity.AccountStatusNormal)}}, bson.D{{Key: "$set", Value: fields}}, options.FindOneAndUpdate().SetReturnDocument(options.After)).Decode(&document)
	if err != nil {
		return nil, mapUserReadError(err)
	}
	return toBizUser(document), nil
}

// NewIdentityRepository 返回外部身份仓储的领域接口实现。
func NewIdentityRepository(data *Data) identity.IdentityRepository {
	return &mongoIdentityRepository{collection: data.database.Collection(schema.CollectionIdentities)}
}

// NewSessionRepository 返回会话仓储的领域接口实现。
func NewSessionRepository(data *Data) identity.SessionRepository {
	return &mongoSessionRepository{collection: data.database.Collection(schema.CollectionSessions)}
}

// NewAuthSessionRepository 返回支持会话签发的仓储。
func NewAuthSessionRepository(data *Data) identity.AuthSessionRepository {
	return &mongoSessionRepository{collection: data.database.Collection(schema.CollectionSessions)}
}

// NewCredentialRepository 返回 Go 自有邮箱密码凭据仓储。
func NewCredentialRepository(data *Data) authcredential.Repository {
	return &mongoCredentialRepository{collection: data.database.Collection(schema.CollectionCredentials)}
}

// NewAccountRepository 返回新用户零余额账户仓储。
func NewAccountRepository(data *Data) identity.AccountRepository {
	return &mongoAccountRepository{collection: data.database.Collection(schema.CollectionAccounts)}
}

func (repository *mongoUserRepository) Find(ctx context.Context, userID string) (*identity.User, error) {
	var document model.UserDocument
	err := repository.collection.FindOne(ctx, bson.D{{Key: "_id", Value: userID}}).Decode(&document)
	if err != nil {
		return nil, mapUserReadError(err)
	}
	return toBizUser(document), nil
}

func (repository *mongoUserRepository) Create(ctx context.Context, user identity.User) error {
	_, err := repository.collection.InsertOne(ctx, newUserDocument(user))
	if err == nil {
		return nil
	}
	if mongo.IsDuplicateKeyError(err) {
		return fmt.Errorf("create user duplicate: %w", err)
	}
	return fmt.Errorf("create user: %w", err)
}

func (repository *mongoUserRepository) FindGuestByDevice(ctx context.Context, platform, deviceID string) (*identity.User, error) {
	var document model.UserDocument
	err := repository.collection.FindOne(ctx, bson.D{{Key: "guest_platform", Value: platform}, {Key: "guest_device_id", Value: deviceID}}).Decode(&document)
	if err != nil {
		return nil, mapUserReadError(err)
	}
	return toBizUser(document), nil
}

func (repository *mongoUserRepository) MarkBound(ctx context.Context, userID string, updatedAt time.Time) (*identity.User, error) {
	var document model.UserDocument
	err := repository.collection.FindOneAndUpdate(
		ctx,
		bson.D{
			{Key: "_id", Value: userID},
			{Key: "account_status", Value: string(identity.AccountStatusNormal)},
			{Key: "binding_state", Value: string(identity.BindingStateGuest)},
		},
		bson.D{{Key: "$set", Value: bson.D{
			{Key: "binding_state", Value: string(identity.BindingStateBound)},
			{Key: "updated_at", Value: updatedAt},
		}}},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&document)
	if err == nil {
		return toBizUser(document), nil
	}
	if !errors.Is(err, mongo.ErrNoDocuments) {
		return nil, fmt.Errorf("mark user bound: %w", err)
	}

	// 条件更新未命中时区分用户不存在与状态不满足，避免 data 层泄露 MongoDB 细节。
	if _, findErr := repository.Find(ctx, userID); findErr != nil {
		return nil, findErr
	}
	return nil, identity.ErrGuestBindingNotAllowed
}

func (repository *mongoUserRepository) ChangeAccountStatus(ctx context.Context, userID string, targetStatus identity.AccountStatus, updatedAt time.Time) (*identity.User, error) {
	var document model.UserDocument
	err := repository.collection.FindOneAndUpdate(
		ctx,
		bson.D{{Key: "_id", Value: userID}},
		bson.D{
			{Key: "$set", Value: bson.D{
				{Key: "account_status", Value: string(targetStatus)},
				{Key: "updated_at", Value: updatedAt},
			}},
			{Key: "$inc", Value: bson.D{{Key: "session_version", Value: int64(1)}}},
		},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(&document)
	if err != nil {
		return nil, mapUserReadError(err)
	}
	return toBizUser(document), nil
}

func (repository *mongoIdentityRepository) FindByProviderSubject(ctx context.Context, provider, subject string) (*identity.ExternalIdentity, error) {
	var document model.IdentityDocument
	err := repository.collection.FindOne(ctx, bson.D{
		{Key: "provider", Value: provider},
		{Key: "subject", Value: subject},
	}).Decode(&document)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, identity.ErrIdentityNotFound
		}
		return nil, fmt.Errorf("find external identity: %w", err)
	}
	return toBizIdentity(document), nil
}

func (repository *mongoIdentityRepository) Create(ctx context.Context, externalIdentity identity.ExternalIdentity) error {
	_, err := repository.collection.InsertOne(ctx, newIdentityDocument(externalIdentity))
	if err == nil {
		return nil
	}
	if mongo.IsDuplicateKeyError(err) {
		return identity.ErrIdentityAlreadyBound
	}
	return fmt.Errorf("create external identity: %w", err)
}

func (repository *mongoSessionRepository) Find(ctx context.Context, sessionID string) (*identity.Session, error) {
	var document model.SessionDocument
	err := repository.collection.FindOne(ctx, bson.D{{Key: "_id", Value: sessionID}}).Decode(&document)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, identity.ErrSessionNotFound
		}
		return nil, fmt.Errorf("find session: %w", err)
	}
	return toBizSession(document), nil
}

func (repository *mongoSessionRepository) Create(ctx context.Context, session identity.Session) error {
	_, err := repository.collection.InsertOne(ctx, newSessionDocument(session))
	if err != nil {
		return fmt.Errorf("create session: %w", err)
	}
	return nil
}

func (repository *mongoSessionRepository) RevokeActiveByUser(ctx context.Context, userID string, revokedAt time.Time) error {
	_, err := repository.collection.UpdateMany(
		ctx,
		bson.D{
			{Key: "user_id", Value: userID},
			{Key: "revoked_at", Value: nil},
		},
		bson.D{{Key: "$set", Value: bson.D{{Key: "revoked_at", Value: revokedAt}}}},
	)
	if err != nil {
		return fmt.Errorf("revoke user sessions: %w", err)
	}
	return nil
}

func (repository *mongoCredentialRepository) FindActiveByEmail(ctx context.Context, email string) (*authcredential.Credential, error) {
	var document model.CredentialDocument
	err := repository.collection.FindOne(ctx, bson.D{{Key: "email_normalized", Value: email}, {Key: "active", Value: true}}).Decode(&document)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, authcredential.ErrCredentialNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find active credential: %w", err)
	}
	return toBizCredential(document), nil
}

// FindActiveByUser 返回用户当前有效凭据，供密码修改流程按会话身份校验旧密码。
func (repository *mongoCredentialRepository) FindActiveByUser(ctx context.Context, userID string) (*authcredential.Credential, error) {
	var document model.CredentialDocument
	err := repository.collection.FindOne(ctx, bson.D{{Key: "user_id", Value: userID}, {Key: "active", Value: true}}).Decode(&document)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, authcredential.ErrCredentialNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find active credential by user: %w", err)
	}
	return toBizCredential(document), nil
}

func (repository *mongoCredentialRepository) Create(ctx context.Context, credential authcredential.Credential) error {
	_, err := repository.collection.InsertOne(ctx, newCredentialDocument(credential))
	if mongo.IsDuplicateKeyError(err) {
		return authcredential.ErrEmailAlreadyRegistered
	}
	if err != nil {
		return fmt.Errorf("create credential: %w", err)
	}
	return nil
}

func (repository *mongoCredentialRepository) UpdatePasswordHash(ctx context.Context, userID, passwordHash string, updatedAt time.Time) error {
	result, err := repository.collection.UpdateOne(ctx, bson.D{{Key: "user_id", Value: userID}, {Key: "active", Value: true}}, bson.D{{Key: "$set", Value: bson.D{{Key: "password_hash", Value: passwordHash}, {Key: "updated_at", Value: updatedAt}}}})
	if err != nil {
		return fmt.Errorf("update password hash: %w", err)
	}
	if result.MatchedCount == 0 {
		return authcredential.ErrCredentialNotFound
	}
	return nil
}

func (repository *mongoCredentialRepository) DeactivateByUser(ctx context.Context, userID string, updatedAt time.Time) error {
	_, err := repository.collection.UpdateMany(ctx, bson.D{{Key: "user_id", Value: userID}, {Key: "active", Value: true}}, bson.D{{Key: "$set", Value: bson.D{{Key: "active", Value: false}, {Key: "updated_at", Value: updatedAt}}}})
	if err != nil {
		return fmt.Errorf("deactivate user credentials: %w", err)
	}
	return nil
}

func (repository *mongoAccountRepository) CreateZero(ctx context.Context, userID string, now time.Time) error {
	_, err := repository.collection.InsertOne(ctx, model.AccountDocument{ID: userID, DiamondBalance: 0, CreatedAt: now, UpdatedAt: now})
	if err != nil {
		return fmt.Errorf("create zero balance account: %w", err)
	}
	return nil
}

func newIdentityDocument(externalIdentity identity.ExternalIdentity) model.IdentityDocument {
	return model.IdentityDocument{
		ID:        externalIdentity.ID,
		Provider:  externalIdentity.Provider,
		Subject:   externalIdentity.Subject,
		UserID:    externalIdentity.UserID,
		CreatedAt: externalIdentity.CreatedAt,
	}
}

func newUserDocument(user identity.User) model.UserDocument {
	return model.UserDocument{ID: user.ID, DisplayName: user.DisplayName, Bio: user.Bio, AvatarImageID: user.AvatarImageID, AccountStatus: string(user.AccountStatus), BindingState: string(user.BindingState), Role: user.Role, Timezone: user.Timezone, GuestPlatform: user.GuestPlatform, GuestDeviceID: user.GuestDeviceID, SessionVersion: user.SessionVersion, ContentAccess: user.ContentAccess, CreatedAt: user.CreatedAt, UpdatedAt: user.UpdatedAt}
}

func newSessionDocument(session identity.Session) model.SessionDocument {
	return model.SessionDocument{ID: session.ID, UserID: session.UserID, SessionVersion: session.SessionVersion, RevokedAt: session.RevokedAt, ExpiresAt: session.ExpiresAt}
}

func newCredentialDocument(credential authcredential.Credential) model.CredentialDocument {
	return model.CredentialDocument{ID: credential.ID, UserID: credential.UserID, EmailNormalized: credential.EmailNormalized, PasswordHash: credential.PasswordHash, Active: credential.Active, CreatedAt: credential.CreatedAt, UpdatedAt: credential.UpdatedAt}
}

func toBizUser(document model.UserDocument) *identity.User {
	return &identity.User{
		ID:             document.ID,
		DisplayName:    document.DisplayName,
		Bio:            document.Bio,
		AvatarImageID:  document.AvatarImageID,
		AccountStatus:  identity.AccountStatus(document.AccountStatus),
		BindingState:   identity.BindingState(document.BindingState),
		Role:           document.Role,
		Timezone:       document.Timezone,
		GuestPlatform:  document.GuestPlatform,
		GuestDeviceID:  document.GuestDeviceID,
		SessionVersion: document.SessionVersion,
		ContentAccess:  document.ContentAccess,
		CreatedAt:      document.CreatedAt,
		UpdatedAt:      document.UpdatedAt,
	}
}

func toBizCredential(document model.CredentialDocument) *authcredential.Credential {
	return &authcredential.Credential{ID: document.ID, UserID: document.UserID, EmailNormalized: document.EmailNormalized, PasswordHash: document.PasswordHash, Active: document.Active, CreatedAt: document.CreatedAt, UpdatedAt: document.UpdatedAt}
}

func toBizIdentity(document model.IdentityDocument) *identity.ExternalIdentity {
	return &identity.ExternalIdentity{
		ID:        document.ID,
		Provider:  document.Provider,
		Subject:   document.Subject,
		UserID:    document.UserID,
		CreatedAt: document.CreatedAt,
	}
}

func toBizSession(document model.SessionDocument) *identity.Session {
	return &identity.Session{
		ID:             document.ID,
		UserID:         document.UserID,
		SessionVersion: document.SessionVersion,
		RevokedAt:      document.RevokedAt,
		ExpiresAt:      document.ExpiresAt,
	}
}

func mapUserReadError(err error) error {
	if errors.Is(err, mongo.ErrNoDocuments) {
		return identity.ErrUserNotFound
	}
	return fmt.Errorf("find user: %w", err)
}

var (
	_ identity.UserRepository        = (*mongoUserRepository)(nil)
	_ identity.IdentityRepository    = (*mongoIdentityRepository)(nil)
	_ identity.SessionRepository     = (*mongoSessionRepository)(nil)
	_ identity.AuthUserRepository    = (*mongoUserRepository)(nil)
	_ identity.AuthSessionRepository = (*mongoSessionRepository)(nil)
	_ authcredential.Repository      = (*mongoCredentialRepository)(nil)
	_ identity.AccountRepository     = (*mongoAccountRepository)(nil)
)
