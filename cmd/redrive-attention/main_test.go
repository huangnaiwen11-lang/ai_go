package main

import (
	"strings"
	"testing"

	"ai-business-service/internal/biz/outbox"
)

// missingConfig 指向一个不存在的配置：任何「先连库/先读配置再校验参数」的实现
// 都会在这里报配置错误，而正确实现必须在读配置之前就拒绝请求。
const missingConfig = "does-not-exist.yaml"

func TestRun必填参数缺失时在触库之前被拒(t *testing.T) {
	valid := outbox.RedriveCommand{
		EventID: "event-1", ExpectedRedriveCount: 0,
		ActorID: "admin-1", Reason: "provider 已恢复", Key: "ticket-42",
	}
	cases := map[string]func(outbox.RedriveCommand) outbox.RedriveCommand{
		"缺事件":  func(command outbox.RedriveCommand) outbox.RedriveCommand { command.EventID = ""; return command },
		"缺操作者": func(command outbox.RedriveCommand) outbox.RedriveCommand { command.ActorID = ""; return command },
		"缺幂等键": func(command outbox.RedriveCommand) outbox.RedriveCommand { command.Key = ""; return command },
		"缺原因":  func(command outbox.RedriveCommand) outbox.RedriveCommand { command.Reason = ""; return command },
		"次数令牌为负": func(command outbox.RedriveCommand) outbox.RedriveCommand {
			command.ExpectedRedriveCount = -1
			return command
		},
		"次数超出上限": func(command outbox.RedriveCommand) outbox.RedriveCommand {
			command.ExpectedRedriveCount = outbox.MaxRedriveCount + 1
			return command
		},
		"原因首尾含空白": func(command outbox.RedriveCommand) outbox.RedriveCommand { command.Reason = " 偷懒 "; return command },
	}
	for name, mutate := range cases {
		err := run(missingConfig, mutate(valid))
		if err == nil {
			t.Fatalf("%s: run() error = nil, want 拒绝", name)
		}
		// 关键断言：拒绝必须发生在读配置之前，而不是连库失败之后的顺带结论。
		if strings.Contains(err.Error(), "本地配置") {
			t.Fatalf("%s: 参数校验发生在读配置之后: %v", name, err)
		}
	}
}

func TestRun合法参数才会走到读配置(t *testing.T) {
	err := run(missingConfig, outbox.RedriveCommand{
		EventID: "event-1", ExpectedRedriveCount: 0,
		ActorID: "admin-1", Reason: "provider 已恢复", Key: "ticket-42",
	})
	if err == nil {
		t.Fatal("配置缺失时 run() 必须失败")
	}
	// 合法参数应当越过参数校验，因此这里必须看到配置错误：这是「参数校验在前」
	// 这条顺序断言的反向证据。
	if !strings.Contains(err.Error(), "本地配置") {
		t.Fatalf("合法参数应当进入到读配置阶段: %v", err)
	}
}
