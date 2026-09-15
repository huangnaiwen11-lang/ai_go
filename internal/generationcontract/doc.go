// Package generationcontract 校验三种公开基础创作能力的离线迁移证据。
//
// 它只处理调用方提供的脱敏证据清单，不创建 HTTP 路由、不访问数据库，也不提交
// 生成任务。通过校验不代表可以切流；上线仍须满足迁移矩阵和受控运行验证门禁。
package generationcontract
