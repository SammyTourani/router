package proxy

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/translate"

	"github.com/tidwall/gjson"
)

// subscriptionThirdPartyDenialTTL keeps a linked Claude account out of one
// client app's leases after Anthropic refused that app on the account's plan
// limits. The refusal lasts until the account holder buys extra usage, so the
// TTL only bounds how often the router re-probes.
const subscriptionThirdPartyDenialTTL = subscriptionModelDenialTTL

// ErrSubscriptionClientAppRefused ends a forced-Claude turn whose linked Claude
// subscriptions refused the third-party client, instead of billing paid API.
var ErrSubscriptionClientAppRefused = errors.New("claude subscription refused third-party client")

func markSubscriptionClientAppRefused(ctx context.Context) {
	if usage, _ := ctx.Value(ManagedSubscriptionUsageContextKey{}).(*ManagedSubscriptionUsage); usage != nil {
		usage.ClientAppRefused = true
	}
}

// forcedClaudeClientAppRefused reports whether a paid attempt would only stand
// in for subscriptions that refused this client app on a forced model. A
// routed turn keeps its paid rescue; a forced one is the user's explicit pick.
func forcedClaudeClientAppRefused(ctx context.Context, decision router.Decision, creds *Credentials) bool {
	if creds != nil && (creds.OAuth || creds.Source == credSourceBYOK) {
		return false
	}
	usage, _ := ctx.Value(ManagedSubscriptionUsageContextKey{}).(*ManagedSubscriptionUsage)
	return usage != nil && usage.ClientAppRefused && strings.HasPrefix(decision.Reason, translate.ReasonUserForceModel)
}

// anthropicSubscriptionThirdPartyRefused reports whether err is Anthropic's
// refusal to serve a non-Claude-Code client on a subscription's plan limits
// ("Third-party apps now draw from your extra usage, not your plan limits").
// It arrives as a 400 invalid_request_error, so neither IsRetryable nor the
// OAuth-rejection gate catches it; the account is healthy for Claude Code
// traffic, so it is a per-client-app capacity failure, not a bad credential.
func anthropicSubscriptionThirdPartyRefused(err error) bool {
	var upstream *providers.UpstreamErrorResponse
	if !errors.As(err, &upstream) || upstream.Status != http.StatusBadRequest {
		return false
	}
	if gjson.GetBytes(upstream.Body, "error.type").String() != "invalid_request_error" {
		return false
	}
	message := strings.ToLower(gjson.GetBytes(upstream.Body, "error.message").String())
	return strings.Contains(message, "third-party apps") && strings.Contains(message, "extra usage")
}

// thirdPartyDenialModel is the model slot a client-app denial occupies in the
// managed denial cache, disjoint from every catalog model ID.
func thirdPartyDenialModel(clientApp string) string {
	return "\x00client-app:" + clientApp
}

func (a *subscriptionModelAccess) denyManagedClientApp(account, provider, clientApp string, until time.Time) {
	a.denyManaged("account:"+account, account, provider, thirdPartyDenialModel(clientApp), until)
}

func (a *subscriptionModelAccess) managedClientAppDenied(account, provider, clientApp string, now time.Time) bool {
	return a.managedDenied("account:"+account, account, provider, thirdPartyDenialModel(clientApp), now)
}
