package entitlement

import "errors"

var (
	// ErrUnsupportedProductOutput 表示请求了当前产品边界以外的输出类型。
	ErrUnsupportedProductOutput = errors.New("entitlement: unsupported product output")
	// ErrUnsupportedVideoDuration 表示视频时长不在固定价格表中。
	ErrUnsupportedVideoDuration = errors.New("entitlement: unsupported video duration")
	// ErrInvalidTimezone 表示用户固化的 IANA 时区无法用于计算本地日界。
	ErrInvalidTimezone = errors.New("entitlement: invalid timezone")
)
