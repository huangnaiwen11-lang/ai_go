// catalog-contract-replay 比较两个 HTTP 上游的公开 Catalog 响应。
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"ai-business-service/internal/catalogcontract"
)

const defaultBaselineUpstream = "http://127.0.0.1:4000"

type replayConfig struct {
	baselineUpstream  string
	candidateUpstream string
}

type replayCase struct {
	requestCase catalogcontract.RequestCase
	conditional bool
}

type replayFunction func(context.Context, *http.Client, string, string, catalogcontract.RequestCase) error

func main() {
	os.Exit(run(os.Getenv, os.Stdout, os.Stderr))
}

func run(getenv func(string) string, stdout, stderr io.Writer) int {
	config, err := replayConfigFromEnv(getenv)
	if err != nil {
		fmt.Fprintf(stderr, "catalog contract replay configuration error: %v\n", err)
		return 2
	}

	client := &http.Client{Timeout: 10 * time.Second}
	for _, caseSpec := range fixedReplayCases() {
		requestCase := caseSpec.requestCase
		replayErr := replayFixedCase(
			context.Background(),
			client,
			config.baselineUpstream,
			config.candidateUpstream,
			caseSpec,
			catalogcontract.Replay,
			catalogcontract.ReplayConditional,
		)
		if replayErr != nil {
			fmt.Fprintf(stderr, "%s: %v\n", requestCase.Name, replayErr)
			return 1
		}
		fmt.Fprintf(stdout, "ok %s\n", requestCase.Name)
	}

	return 0
}

func replayFixedCase(
	ctx context.Context,
	client *http.Client,
	baselineUpstream, candidateUpstream string,
	caseSpec replayCase,
	ordinaryReplay, conditionalReplay replayFunction,
) error {
	if caseSpec.conditional {
		return conditionalReplay(ctx, client, baselineUpstream, candidateUpstream, caseSpec.requestCase)
	}

	return ordinaryReplay(ctx, client, baselineUpstream, candidateUpstream, caseSpec.requestCase)
}

func replayConfigFromEnv(getenv func(string) string) (replayConfig, error) {
	baseline := strings.TrimSpace(getenv("CATALOG_BASELINE_UPSTREAM"))
	if baseline == "" {
		baseline = defaultBaselineUpstream
	}
	if !isHTTPUpstreamURL(baseline) {
		return replayConfig{}, errors.New("baseline upstream must be an http(s) URL")
	}

	candidate := strings.TrimSpace(getenv("CATALOG_CANDIDATE_UPSTREAM"))
	if candidate == "" || !isHTTPUpstreamURL(candidate) {
		return replayConfig{}, errors.New("CATALOG_CANDIDATE_UPSTREAM must be explicitly set to an http(s) URL")
	}

	return replayConfig{
		baselineUpstream:  baseline,
		candidateUpstream: candidate,
	}, nil
}

func isHTTPUpstreamURL(raw string) bool {
	upstream, err := url.Parse(raw)
	if err != nil || upstream.Host == "" || upstream.User != nil {
		return false
	}
	return upstream.Scheme == "http" || upstream.Scheme == "https"
}

func fixedReplayCases() []replayCase {
	// 所有固定用例都保持匿名，只验证公开协议与缓存传播；绝不伪造已认证用户上下文。
	return []replayCase{
		{requestCase: newReplayCase("api-query", "/api/homepage/video-templates", "category=all&limit=24", "web", catalogcontract.CacheContract{
			RequireCacheStatus: true,
			ETagPolicy:         catalogcontract.ETagForbidden,
		})},
		{requestCase: newReplayCase("api-v1-query", "/api/v1/homepage/video-templates", "category=all&limit=24", "web", catalogcontract.CacheContract{
			RequireCacheStatus: true,
			ETagPolicy:         catalogcontract.ETagForbidden,
		})},
		{requestCase: newReplayCase("api-no-query", "/api/homepage/video-templates", "", "web", catalogcontract.CacheContract{
			RequireCacheStatus: true,
			ETagPolicy:         catalogcontract.ETagForbidden,
		})},
		{requestCase: newReplayCase("api-limit-min", "/api/homepage/video-templates", "category=all&limit=1&offset=0", "web", catalogcontract.CacheContract{
			RequireCacheStatus: true,
			ETagPolicy:         catalogcontract.ETagForbidden,
		})},
		{requestCase: newReplayCase("api-limit-max", "/api/homepage/video-templates", "category=all&limit=100&offset=1", "web", catalogcontract.CacheContract{
			RequireCacheStatus: true,
			ETagPolicy:         catalogcontract.ETagForbidden,
		})},
		{requestCase: newReplayCase("api-query-normalization", "/api/homepage/video-templates", "category=unrecognized&limit=not-a-number&offset=-1", "web", catalogcontract.CacheContract{
			RequireCacheStatus: true,
			ETagPolicy:         catalogcontract.ETagForbidden,
		})},
		{requestCase: newReplayCase("api-max-rating-sfw", "/api/homepage/video-templates", "category=all&limit=24&maxRating=sfw", "web", catalogcontract.CacheContract{
			RequireCacheStatus: true,
			ETagPolicy:         catalogcontract.ETagForbidden,
		})},
		{requestCase: newReplayCase("api-ios-platform", "/api/homepage/video-templates", "category=all&limit=24", "ios", catalogcontract.CacheContract{
			RequireCacheStatus: true,
			ETagPolicy:         catalogcontract.ETagForbidden,
		})},
		{requestCase: newReplayCase("api-android-platform", "/api/homepage/video-templates", "category=all&limit=24", "android", catalogcontract.CacheContract{
			RequireCacheStatus: true,
			ETagPolicy:         catalogcontract.ETagForbidden,
		})},
		{
			requestCase: newReplayCase("homepage-content-etag", "/api/homepage/content", "catalog=contract-replay", "web", catalogcontract.CacheContract{
				RequireCacheStatus: true,
				ETagPolicy:         catalogcontract.ETagRequired,
			}),
			conditional: true,
		},
	}
}

func newReplayCase(name, path, rawQuery, platform string, cacheContract catalogcontract.CacheContract) catalogcontract.RequestCase {
	return catalogcontract.RequestCase{
		Name:          name,
		Path:          path,
		RawQuery:      rawQuery,
		CacheContract: cacheContract,
		Header: http.Header{
			"X-Request-Id":      {"catalog-contract-replay-" + name},
			"X-Client-Platform": {platform},
		},
	}
}
