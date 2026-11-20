package autonomy

import (
	"fmt"
	"strings"

	"9router/proxy/internal/providers"
)

// RecoveryStrategy identifies the automated healing action for an upstream failure.
type RecoveryStrategy string

const (
	StrategyRefreshOAuth       RecoveryStrategy = "refresh_oauth"
	StrategyDiscoverProject    RecoveryStrategy = "discover_project"
	StrategyDeprecateFallback  RecoveryStrategy = "deprecate_and_fallback"
	StrategyAbortNoRetry       RecoveryStrategy = "abort_no_retry"
	StrategyRateLimitBackoff   RecoveryStrategy = "exponential_backoff_and_rotate"
	StrategyTransientRetry     RecoveryStrategy = "transient_backoff_and_retry"
	StrategyAdminRequired      RecoveryStrategy = "needs_admin"
)

// RecoveryPlan describes the classified error, chosen action, and backoff timings.
type RecoveryPlan struct {
	Strategy            RecoveryStrategy
	ShouldFallback      bool
	ShouldRotateAccount bool
	MarkModelDeprecated bool
	CooldownSec         int
	NewBackoffLevel     int
	NeedsAdmin          bool
	Detected            string
	Reason              string
}

// PlanRecovery classifies an upstream or transport failure into a concrete self-healing strategy.
// Distinguishes transient recoverable faults from revoked credentials, missing onboarding, and
// invalid client tool schemas so the gateway does not waste quota in pointless retry loops.
func PlanRecovery(provider, model string, statusCode int, errorText string, backoffLevel int) RecoveryPlan {
	lower := strings.ToLower(errorText)

	// 1. Revoked or unconfigured OAuth refresh requires administrator re-authorization.
	if strings.Contains(lower, "invalid_grant") ||
		strings.Contains(lower, "token has been expired or revoked") ||
		strings.Contains(lower, "oauth client credentials not configured") {
		return RecoveryPlan{
			Strategy:            StrategyAdminRequired,
			ShouldFallback:      true,
			ShouldRotateAccount: true,
			CooldownSec:         300,
			NewBackoffLevel:     backoffLevel,
			NeedsAdmin:          true,
			Detected:            "OAuth refresh credential rejected or unconfigured",
			Reason:              truncateSafe(errorText, 140),
		}
	}

	// 2. Missing Antigravity Cloud Code project needs onboarding / administrator action.
	if strings.Contains(lower, "no project id") || strings.Contains(lower, "onboard") {
		return RecoveryPlan{
			Strategy:            StrategyAdminRequired,
			ShouldFallback:      true,
			ShouldRotateAccount: true,
			CooldownSec:         300,
			NewBackoffLevel:     backoffLevel,
			NeedsAdmin:          true,
			Detected:            "Antigravity Cloud Code project missing",
			Reason:              "Account requires Antigravity onboarding or explicit projectId",
		}
	}

	// 3. Model deprecation or missing upstream model (404 / version banner / model not found).
	if statusCode == 404 ||
		strings.Contains(lower, "no longer supported") ||
		strings.Contains(lower, "model is deprecated") ||
		(strings.Contains(lower, "not_found") && strings.Contains(lower, "model")) {
		return RecoveryPlan{
			Strategy:            StrategyDeprecateFallback,
			ShouldFallback:      true,
			ShouldRotateAccount: false,
			MarkModelDeprecated: true,
			CooldownSec:         120,
			NewBackoffLevel:     backoffLevel,
			NeedsAdmin:          false,
			Detected:            fmt.Sprintf("model %s rejected as deprecated/missing", model),
			Reason:              truncateSafe(errorText, 140),
		}
	}

	// 4. Client schema/tool declaration errors (400 INVALID_ARGUMENT) must NOT spin through accounts.
	if statusCode == 400 && (strings.Contains(lower, "invalid_argument") ||
		strings.Contains(lower, "function_declarations") ||
		strings.Contains(lower, "improperly formed") ||
		strings.Contains(lower, "schema") ||
		strings.Contains(lower, "tool")) {
		return RecoveryPlan{
			Strategy:   StrategyAbortNoRetry,
			Detected:   "upstream rejected request or tool schema (400)",
			Reason:     "skipping cross-account retries because client payload schema is invalid",
			NeedsAdmin: false,
		}
	}

	// 5. Auth expiry (401/403) — transient after refresh attempt, rotate to another healthy account.
	if statusCode == 401 || statusCode == 403 {
		return RecoveryPlan{
			Strategy:            StrategyRefreshOAuth,
			ShouldFallback:      true,
			ShouldRotateAccount: true,
			CooldownSec:         60,
			NewBackoffLevel:     backoffLevel,
			Detected:            fmt.Sprintf("upstream authentication rejected (%d)", statusCode),
			Reason:              "attempt OAuth refresh and rotate away from affected connection",
		}
	}

	// 6. Rate limiting / capacity exhaustion — exponential backoff and rotate connection.
	if statusCode == 429 ||
		strings.Contains(lower, "rate limit") ||
		strings.Contains(lower, "too many requests") ||
		strings.Contains(lower, "quota") ||
		strings.Contains(lower, "capacity") ||
		strings.Contains(lower, "overloaded") {
		cls := providers.ClassifyError(statusCode, errorText, backoffLevel)
		cooldownSec := int((cls.CooldownMs + 999) / 1000)
		return RecoveryPlan{
			Strategy:            StrategyRateLimitBackoff,
			ShouldFallback:      true,
			ShouldRotateAccount: true,
			CooldownSec:         cooldownSec,
			NewBackoffLevel:     cls.NewBackoffLevel,
			Detected:            fmt.Sprintf("upstream quota/rate-limit on %s/%s", provider, model),
			Reason:              fmt.Sprintf("exponential backoff %ds (level %d) and rotate account", cooldownSec, cls.NewBackoffLevel),
		}
	}

	// 7. SSE stall, EOF, or transient upstream/network fault.
	cls := providers.ClassifyError(statusCode, errorText, backoffLevel)
	cooldownSec := int((cls.CooldownMs + 999) / 1000)
	if cooldownSec <= 0 {
		cooldownSec = 15
	}
	return RecoveryPlan{
		Strategy:            StrategyTransientRetry,
		ShouldFallback:      true,
		ShouldRotateAccount: true,
		CooldownSec:         cooldownSec,
		NewBackoffLevel:     cls.NewBackoffLevel,
		Detected:            fmt.Sprintf("transient upstream/network error (%d)", statusCode),
		Reason:              fmt.Sprintf("short %ds cooldown and failover if backup connection exists", cooldownSec),
	}
}

func truncateSafe(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
