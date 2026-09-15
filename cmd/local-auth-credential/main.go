// local-auth-credential 生成游客绑定的本地测试凭据。
// 输出只适用于 GO_LOCAL_BINDING_VERIFIER_ENABLED=1 的隔离环境。
package main

import (
	"flag"
	"fmt"
	"os"

	"ai-business-service/internal/transport/authentry"
)

func main() {
	provider := flag.String("provider", "email", "验证方式，例如 email/google/apple")
	subject := flag.String("subject", "local-user@example.test", "本地测试 subject")
	secret := flag.String("secret", os.Getenv("GO_LOCAL_BINDING_SECRET"), "本地绑定密钥")
	flag.Parse()
	credential, err := authentry.IssueLocalBindingCredential(*provider, *subject, *secret)
	if err != nil {
		fmt.Fprintln(os.Stderr, "生成本地凭据失败")
		os.Exit(1)
	}
	fmt.Println(credential)
}
