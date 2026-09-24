// app-instance-preflight checks that a publish target has an explicit,
// dedicated application identity. It is strictly local and read-only: it does
// not connect to MongoDB, R2, or PolarStar, and it never prints credentials.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"ai-business-service/internal/biz/appinstance"
	"ai-business-service/internal/conf"
)

type report struct {
	Status string `json:"status"`
	AppID  string `json:"app_id"`
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}

func run(args []string, getenv func(string) string, stdout, stderr io.Writer) int {
	if stdout == nil || stderr == nil {
		return 2
	}
	fromEnv := conf.LoadAppInstanceEnvironment(getenv)
	flags := flag.NewFlagSet("app-instance-preflight", flag.ContinueOnError)
	flags.SetOutput(stderr)
	bindFlags(flags, &fromEnv)
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "app instance preflight arguments are invalid")
		return 2
	}
	if !fromEnv.AppScopeEnabled {
		fmt.Fprintln(stderr, "app instance preflight failed: GATEWAY_APP_SCOPE_ENABLED must be true")
		return 1
	}
	if fromEnv.MongoProfile != conf.MongoProfileProduction {
		fmt.Fprintln(stderr, "app instance preflight failed: CLING_MONGO_PROFILE must be production")
		return 1
	}

	if err := appinstance.Validate(appinstance.Config{
		AppID:               fromEnv.AppID,
		PublicOrigin:        fromEnv.PublicOrigin,
		MongoURI:            fromEnv.MongoURI,
		MongoDatabase:       fromEnv.MongoDatabase,
		R2PublicBucket:      fromEnv.R2PublicBucket,
		R2PrivateBucket:     fromEnv.R2PrivateBucket,
		PolarStarAccountRef: fromEnv.PolarStarAccountRef,
		PolarStarTenantID:   fromEnv.PolarStarTenantID,
		CallbackOrigin:      fromEnv.CallbackOrigin,
	}); err != nil {
		fmt.Fprintf(stderr, "app instance preflight failed: %v\n", err)
		return 1
	}
	if err := json.NewEncoder(stdout).Encode(report{Status: "ready", AppID: strings.TrimSpace(fromEnv.AppID)}); err != nil {
		fmt.Fprintln(stderr, "app instance preflight report write failed")
		return 2
	}
	return 0
}

func bindFlags(flags *flag.FlagSet, input *conf.AppInstanceEnvironment) {
	flags.Var(trimmedString{target: &input.AppID}, "app-id", "应用实例 ID（默认读取 CLING_APP_ID）")
	flags.Var(trimmedString{target: &input.PublicOrigin}, "public-origin", "公网 HTTPS origin（默认读取 CLING_PUBLIC_ORIGIN）")
	flags.Var(trimmedString{target: &input.MongoURI}, "mongo-uri", "Mongo URI（默认读取 CLING_MONGO_URI）")
	flags.Var(trimmedString{target: &input.MongoDatabase}, "mongo-database", "Mongo 数据库名（默认读取 CLING_MONGO_DATABASE）")
	flags.Var(trimmedString{target: &input.R2PublicBucket}, "r2-public-bucket", "R2 公开桶（默认读取 R2_BUCKET_NAME）")
	flags.Var(trimmedString{target: &input.R2PrivateBucket}, "r2-private-bucket", "R2 私有桶（默认读取 R2_PRIVATE_BUCKET_NAME）")
	flags.Var(trimmedString{target: &input.PolarStarAccountRef}, "polarstar-account-ref", "PolarStar account 标识（默认读取 POLARSTAR_B2B_ACCOUNT_REF）")
	flags.Var(trimmedString{target: &input.PolarStarTenantID}, "polarstar-tenant-id", "PolarStar tenant 标识（默认读取 POLARSTAR_B2B_TENANT_ID）")
	flags.Var(trimmedString{target: &input.CallbackOrigin}, "callback-origin", "回调 HTTPS origin（默认读取 POLARSTAR_B2B_CALLBACK_ORIGIN）")
}

type trimmedString struct {
	target *string
}

func (value trimmedString) String() string { return "" }

func (value trimmedString) Set(raw string) error {
	*value.target = strings.TrimSpace(raw)
	return nil
}
