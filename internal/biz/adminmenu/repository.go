// Package adminmenu 定义管理后台的全局配置投影 —— 目前只有菜单节点可见性。
//
// 这里**只存覆盖项**（`{节点路径: 是否可见}`），不存节点清单。
// 节点清单的唯一真相源是 ai-admin 的 `routes/routeManifest.tsx`：
// 后端再存一份清单，routeManifest 一改就会在后端留下幽灵节点，
// 而「节点设置」页要展示的恰恰是清单本身。
//
// 于是接口语义是「覆盖项读写」，不是「菜单配置管理」——
// 后端不知道有哪些节点，也不需要知道。
package adminmenu

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrInvalid 表示覆盖项本身不合法（键不是绝对路径、数量或长度越界）。
// 与「读不到配置」区分开：前者是调用方的输入问题，后者是环境问题。
var ErrInvalid = errors.New("admin menu: invalid input")

const (
	// MaxOverrides 只是防灌垃圾的上限。节点数由前端清单决定，
	// 真实规模在 50 上下，这里给足余量。
	MaxOverrides = 500
	// MaxPathLen 单条路径长度上限。菜单路径是 `/a/b/c` 这种，
	// 200 已经远超实际需要，超过基本可以判定是脏数据。
	MaxPathLen = 200
	// SettingsDocumentID 单文档存储的固定 _id。
	// 全局配置只有一份，用固定 _id 比「查第一条」可靠 ——
	// 后者在并发写入产生多条文档时会静默取到其中任意一条。
	SettingsDocumentID = "menu-visibility"
)

// Visibility 是菜单可见性配置的读取结果。
type Visibility struct {
	Overrides map[string]bool
	UpdatedAt *time.Time
	UpdatedBy string
}

// Actor 是写操作的审计主体。
type Actor struct{ ID, Role string }

type Repository interface {
	Get(context.Context) (Visibility, error)
	Save(context.Context, Actor, map[string]bool) (Visibility, error)
}

// NormalizeOverrides 校验并归一化覆盖项。
//
// 只接受以 "/" 开头的路径：菜单节点路径全部是绝对路径，
// 放行相对路径只会在前端产生永远匹配不上的键 ——
// 那类键不会报错，只会安静地不生效。
//
// 空 map 是合法输入，含义是「全部按默认清单」。
func NormalizeOverrides(overrides map[string]bool) (map[string]bool, error) {
	if len(overrides) > MaxOverrides {
		return nil, ErrInvalid
	}
	out := make(map[string]bool, len(overrides))
	for path, visible := range overrides {
		trimmed := strings.TrimSpace(path)
		if trimmed == "" || !strings.HasPrefix(trimmed, "/") || len(trimmed) > MaxPathLen {
			return nil, ErrInvalid
		}
		out[trimmed] = visible
	}
	return out, nil
}
