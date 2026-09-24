package main

import (
	"context"
	"net/http"
	"strings"

	bizfeedback "ai-business-service/internal/biz/feedback"
	bizmedia "ai-business-service/internal/biz/media"
	"ai-business-service/internal/conf"
	"ai-business-service/internal/data"
	transportfeedback "ai-business-service/internal/transport/feedback"
	"ai-business-service/internal/transport/sessionauth"
)

func newOptionalFeedbackHandler(enabled bool, configPath string, authenticator *sessionauth.Authenticator) (http.Handler, func(), error) {
	if !enabled || authenticator == nil {
		return nil, nil, nil
	}
	bootstrap, err := loadGatewayBootstrap(configPath)
	if err != nil {
		return nil, nil, err
	}
	if err := conf.ValidateConfiguredMongo(bootstrap.GetData()); err != nil {
		return nil, nil, err
	}
	storage, cleanup, err := data.NewData(bootstrap.GetData())
	if err != nil {
		return nil, nil, err
	}
	// 反馈只接收前端传来的素材 ID。此适配器在组合根核验素材归属，
	// 反馈领域本身不依赖本地文件、Mongo 字段或未来的对象存储实现。
	mediaUsecase := bizmedia.NewUsecase(data.NewLocalUserMediaRepository(storage, localMediaDirectory()))
	usecase := bizfeedback.NewUsecase(data.NewFeedbackRepository(storage), mediaFeedbackAttachmentVerifier{media: mediaUsecase})
	return transportfeedback.NewHandler(authenticator, usecase), cleanup, nil
}

type feedbackOwnedImageReader interface {
	OpenOwnedImage(context.Context, string, string) (*bizmedia.Image, []byte, error)
}

type mediaFeedbackAttachmentVerifier struct{ media feedbackOwnedImageReader }

func (verifier mediaFeedbackAttachmentVerifier) VerifyOwnedAttachment(ctx context.Context, userID, attachmentID string) (bizfeedback.Attachment, error) {
	if verifier.media == nil {
		return bizfeedback.Attachment{}, bizfeedback.ErrInvalidInput
	}
	image, _, err := verifier.media.OpenOwnedImage(ctx, userID, attachmentID)
	if err != nil || image == nil || image.ID != attachmentID {
		return bizfeedback.Attachment{}, bizfeedback.ErrInvalidInput
	}
	return bizfeedback.Attachment{ID: image.ID, DownloadURL: "/api/media/images/" + strings.TrimSpace(image.ID)}, nil
}
