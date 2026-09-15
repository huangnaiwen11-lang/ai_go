package media

import (
	"context"
	"errors"
	"testing"
)

func TestUsecase保存合法图片并返回当前用户的素材引用(t *testing.T) {
	repository := &recordingRepository{saved: &Image{ID: "media-1", OwnerID: "user-1", Reference: "https://local-media.invalid/assets/media-1"}}
	usecase := NewUsecase(repository)

	image, err := usecase.SaveImage(context.Background(), SaveImageCommand{
		OwnerID: "user-1", Filename: "portrait.png", ContentType: "image/png", Content: []byte{0x89, 0x50, 0x4e, 0x47},
	})
	if err != nil {
		t.Fatalf("SaveImage() error = %v", err)
	}
	if image.Reference == "" || repository.command.OwnerID != "user-1" || repository.command.ContentType != "image/png" {
		t.Fatalf("保存素材 = image:%#v command:%#v", image, repository.command)
	}
}

func TestUsecase拒绝非图片和超出限制的素材(t *testing.T) {
	usecase := NewUsecase(&recordingRepository{})
	for _, command := range []SaveImageCommand{
		{OwnerID: "user-1", Filename: "video.mp4", ContentType: "video/mp4", Content: []byte("not-image")},
		{OwnerID: "user-1", Filename: "large.jpg", ContentType: "image/jpeg", Content: make([]byte, MaxImageBytes+1)},
	} {
		if _, err := usecase.SaveImage(context.Background(), command); !errors.Is(err, ErrInvalidImage) {
			t.Fatalf("非法上传错误 = %v，期望 ErrInvalidImage", err)
		}
	}
}

func TestUsecase拒绝其他用户引用素材(t *testing.T) {
	usecase := NewUsecase(&recordingRepository{resolveErr: errors.New("not owner")})
	if _, err := usecase.ResolveOwnedImage(context.Background(), "user-2", "https://local-media.invalid/assets/media-1"); !errors.Is(err, ErrImageAccessDenied) {
		t.Fatalf("越权读取错误 = %v，期望 ErrImageAccessDenied", err)
	}
}

type recordingRepository struct {
	command    SaveImageCommand
	saved      *Image
	resolveErr error
}

func (repository *recordingRepository) SaveImage(_ context.Context, command SaveImageCommand) (*Image, error) {
	repository.command = command
	return repository.saved, nil
}

func (repository *recordingRepository) ResolveOwnedImage(_ context.Context, _, _ string) (string, error) {
	if repository.resolveErr != nil {
		return "", repository.resolveErr
	}
	return "https://local-media.invalid/assets/media-1", nil
}

func (repository *recordingRepository) OpenOwnedImage(_ context.Context, _, _ string) (*Image, []byte, error) {
	return repository.saved, []byte("image"), nil
}
