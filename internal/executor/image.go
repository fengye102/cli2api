package executor

import (
	"context"
	"net/http"
	"time"

	"github.com/caigee-cmd/cli2api/internal/accounts"
	"github.com/caigee-cmd/cli2api/internal/providers"
	"github.com/caigee-cmd/cli2api/internal/translate"
)

// ImageResult is one served image request.
type ImageResult struct {
	Body         []byte
	Credits      *float64
	AccountID    string
	Provider     string
	Routing      string
	AttemptCount int
}

// errNoImagePlane reports a provider that has accounts in the pool but no image
// capability at all — a wiring mistake, not a busy upstream.
func errNoImagePlane(provider string) error {
	return &PrepareError{
		Status:  http.StatusNotImplemented,
		Code:    "image_generation_unsupported",
		Message: "provider " + provider + " 不支持图像生成",
	}
}

// errNoImageAccount reports that no eligible account could serve the model.
func errNoImageAccount(model string) error {
	return &PrepareError{
		Status:  http.StatusServiceUnavailable,
		Code:    "no_image_account",
		Message: "没有可用账号能生成 " + model + "（可能都在冷却或额度不足）",
	}
}

// GenerateImage routes one /v1/images/generations request to an account whose
// provider implements the image capability, and fails over on a classified
// error exactly the way chat does.
//
// It reuses the chat route loop on purpose: account selection, key grants,
// pins, cooldowns, in-flight slots and per-account attempt records all live
// there, and an image request must respect the same ones.
func (e ChatExecutor) GenerateImage(ctx context.Context, req providers.ImageGenerationRequest, prefer, providerFilter string) (result ImageResult, returnErr error) {
	loop := e.newRouteLoop(ctx, prefer, providerFilter, translate.ChatRequest{Model: req.PublicModel})
	defer func() { result.Routing = loop.routing.Source }()
	for loop.index < loop.attempts {
		item, i, err := loop.pickNext(e, req.PublicModel)
		if err != nil {
			attempts, accountID, provider, pickErr := loop.pickFailure(err)
			if loop.lastErr != nil {
				return ImageResult{AttemptCount: attempts, AccountID: accountID, Provider: provider}, pickErr
			}
			return ImageResult{}, pickErr
		}
		poolID := item.ID
		if !e.acquireInFlight(poolID) {
			loop.exclude(item)
			continue
		}
		adapter, ok := e.Providers.Get(itemProvider(item))
		if !ok || adapter.ImageGen == nil {
			e.releaseInFlight(poolID)
			loop.exclude(item)
			loop.lastErr = NewExecutionError(Classified{Kind: accounts.KindModelNotAvailable, Failover: true},
				errNoImagePlane(itemProvider(item)))
			continue
		}
		started := time.Now()
		outcome, callErr := adapter.ImageGen.GenerateImage(ctx, item.ID, req)
		finished := time.Now().UTC()
		latency := int(finished.Sub(started).Milliseconds())
		if callErr == nil {
			e.markOK(item.ID, req.PublicModel)
			e.recordAttempt(ctx, accounts.RequestAttempt{
				AttemptIndex: i, AccountID: item.ID, StartedAt: started, FinishedAt: &finished,
				Status: accounts.AttemptStatusOK, LatencyMs: &latency,
			})
			e.releaseInFlight(poolID)
			return ImageResult{
				Body:         outcome.Body,
				Credits:      outcome.Credits,
				AccountID:    item.ID,
				Provider:     item.Provider,
				AttemptCount: i + 1,
			}, nil
		}
		loop.lastErr = callErr
		if requestContextDone(ctx, callErr) {
			e.releaseInFlight(poolID)
			return ImageResult{AttemptCount: i + 1, AccountID: item.ID, Provider: item.Provider}, callErr
		}
		classified := e.classifyInProcessError(callErr)
		e.markClassified(item.ID, classified, req.PublicModel)
		status := accounts.AttemptStatusError
		if classified.Failover {
			status = accounts.AttemptStatusFailover
		}
		e.recordAttempt(ctx, accounts.RequestAttempt{
			AttemptIndex: i, AccountID: item.ID, StartedAt: started, FinishedAt: &finished,
			Status: status, ErrorKind: classified.Kind, ErrorMessage: truncateErr(callErr.Error()), LatencyMs: &latency,
		})
		e.releaseInFlight(poolID)
		if loop.canFailover(classified) {
			loop.exclude(item)
			continue
		}
		return ImageResult{AttemptCount: i + 1, AccountID: item.ID, Provider: item.Provider}, NewExecutionError(classified, callErr)
	}
	if loop.lastErr != nil {
		return ImageResult{AttemptCount: loop.index, Provider: providerFilter}, loop.lastErr
	}
	return ImageResult{}, errNoImageAccount(req.PublicModel)
}
