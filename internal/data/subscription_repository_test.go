package data

import (
	"context"
	"testing"
)

func TestNewSubscriptionRepository空数据返回可安全调用的读取器(t *testing.T) {
	reader := NewSubscriptionRepository(nil)
	if reader == nil {
		t.Fatal("NewSubscriptionRepository(nil) = nil，要求返回可安全调用的读取器")
	}

	snapshot, err := reader.FindByUserID(context.Background(), "user-1")
	if err == nil {
		t.Fatal("FindByUserID() error = nil，要求返回未配置错误")
	}
	if snapshot != nil {
		t.Fatalf("FindByUserID() snapshot = %#v, want nil", snapshot)
	}
}

func TestNewCreationRepository空数据返回可安全调用的读取器(t *testing.T) {
	for _, testCase := range []struct {
		name string
		data *Data
	}{
		{name: "nil Data", data: nil},
		{name: "nil database", data: &Data{}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			repository := NewCreationRepository(testCase.data)
			if repository == nil {
				t.Fatal("NewCreationRepository() = nil，要求返回可安全调用的读取器")
			}
			creation, err := repository.FindByIdempotencyKey(context.Background(), "key-1")
			if err == nil {
				t.Fatal("FindByIdempotencyKey() error = nil，要求返回未配置错误")
			}
			if creation != nil {
				t.Fatalf("FindByIdempotencyKey() creation = %#v, want nil", creation)
			}
		})
	}
}
