package biz

// ModuleRegistry 记录当前可独立演进的业务边界。
// 它只用于应用装配与可观测元数据，不承载跨模块调用或业务状态。
type ModuleRegistry struct {
	Names []string
}

// NewModuleRegistry 返回稳定顺序的业务模块清单。
// 顺序反映依赖方向：身份与模板提供上下文，权益和账本提供结算能力，生成与支付
// 产生日志，创作模块最终向用户投影任务状态。
func NewModuleRegistry() *ModuleRegistry {
	return &ModuleRegistry{
		Names: []string{
			"identity",
			"catalog",
			"entitlement",
			"ledger",
			"generation",
			"payments",
			"creations",
			// works 是面向用户的只读作品投影边界，不参与创作命令或生成编排。
			"works",
			// notification 只负责用户通知读写，不包含推送供应商或后台投递。
			"notification",
		},
	}
}
