package proxy

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"weave-os/router/internal/auth"
	"weave-os/router/internal/dispatch"
	"weave-os/router/internal/inference"
	"weave-os/router/internal/observability"
	"weave-os/router/internal/providers"
	"weave-os/router/internal/proxy/usage"
	"weave-os/router/internal/requestcontext"
	"weave-os/router/internal/router/catalog"
	"weave-os/router/internal/subscriptions"
)

type managedSubscriptionCandidatesKey struct{}

type managedSubscriptionCandidate struct {
	accountID string
	provider  auth.SubscriptionProvider
	state     auth.SubscriptionAccountState
	resetAt   time.Time
	enabled   bool
}

// WithManagedSubscriptionCandidates retains detached, credential-free admission
// facts. Routing suppression and client/model denials cannot erase pool capacity.
func WithManagedSubscriptionCandidates(ctx context.Context, accounts []*auth.SubscriptionAccount) context.Context {
	candidates := make([]managedSubscriptionCandidate, 0, len(accounts))
	enrolled := make(map[auth.SubscriptionProvider]struct{})
	for _, account := range accounts {
		candidate := managedSubscriptionCandidate{accountID: account.ID, provider: account.Provider, state: account.State, enabled: account.Enabled}
		if account.CooldownUntil != nil {
			candidate.resetAt = *account.CooldownUntil
		}
		candidates = append(candidates, candidate)
		enrolled[account.Provider] = struct{}{}
	}
	ctx = context.WithValue(ctx, managedSubscriptionCandidatesKey{}, candidates)
	return context.WithValue(ctx, ManagedSubscriptionProvidersContextKey{}, enrolled)
}

func subscriptionPoolForModel(target inference.Target) (subscriptions.Provider, bool) {
	if entry, known := catalog.ByID(target.CatalogID); known {
		for _, binding := range entry.Providers {
			if pool, compatible := managedSubscriptionProviderFromUpstream(binding.Provider, target.CatalogID); compatible {
				return pool, true
			}
		}
	}
	return managedSubscriptionProviderFromUpstream(target.Provider, target.CatalogID)
}

func (s *Service) withSubscriptionFundingAuthorization(ctx context.Context, headers http.Header) context.Context {
	ctx = WithManagedSubscriptionUsage(ctx)
	ctx, _ = withSubscriptionAdmissionBudget(ctx)
	if dispatch.AuthorizationFromContext(ctx) != nil {
		return ctx
	}
	codexToken, claudeToken := presentSubscriptionTokens(ctx, headers)
	if creds := CredentialsFromContext(ctx); creds != nil && creds.OAuth {
		if creds.Source == requestcontext.SourceCodexSubscription {
			codexToken = string(creds.APIKey)
		} else {
			claudeToken = string(creds.APIKey)
		}
	}
	admissionCtx := ctx
	return dispatch.WithAuthorization(ctx, func(callCtx context.Context, target inference.Target) error {
		if creds := CredentialsFromContext(callCtx); creds != nil && creds.OAuth {
			return nil
		}
		pool, compatible := subscriptionPoolForModel(target)
		if !compatible {
			return nil
		}
		refusal := clientAppRefusalFunding{counts: clientAppRefusalPaidFallback(callCtx), clientApp: ClientIdentityFrom(callCtx).ClientApp}
		if err := s.authorizeSubscriptionAPI(admissionCtx, pool, codexToken, claudeToken, refusal); err != nil {
			observability.FromContext(callCtx).Warn("Blocked API inference without subscription quota-exhaustion evidence", "model", target.CatalogID, "provider", target.Provider, "subscription_provider", pool, "err", err)
			return err
		}
		return nil
	})
}

func (s *Service) subscriptionAPIAuthorization(ctx context.Context, provider, model string) error {
	ctx = s.withSubscriptionFundingAuthorization(ctx, nil)
	return dispatch.AuthorizationFromContext(ctx)(clearCredentials(ctx), inference.Target{Provider: provider, CatalogID: model})
}

// clientAppRefusalFunding lets a Claude seat that refused clientApp count as
// spent. Only a key's paid-fallback opt-in on a forced turn sets counts; an
// unrecognized client legitimately has an empty clientApp.
type clientAppRefusalFunding struct {
	counts    bool
	clientApp string
}

func (s *Service) authorizeSubscriptionAPI(ctx context.Context, pool subscriptions.Provider, codexToken, claudeToken string, refusal clientAppRefusalFunding) error {
	if managedSubscriptionEnrollmentUnavailable(ctx) {
		return fmt.Errorf("%w: subscription enrollment could not be verified", ErrSubscriptionPoolUnavailable)
	}
	now := s.clockNow()
	candidates, snapshotPresent := ctx.Value(managedSubscriptionCandidatesKey{}).([]managedSubscriptionCandidate)
	for _, candidate := range candidates {
		// A seat that can never be leased can never prove exhaustion either.
		if candidate.provider != auth.SubscriptionProvider(pool) || !candidate.enabled || candidate.state == auth.SubscriptionAccountStateDisabled || candidate.state == auth.SubscriptionAccountStateReconnectRequired {
			continue
		}
		if s.managedAccountQuotaExhausted(ctx, candidate, now) {
			continue
		}
		if refusal.counts && pool == subscriptions.ProviderClaude && s.subscriptionModels.managedClientAppDenied(candidate.accountID, providers.ProviderAnthropic, refusal.clientApp, now) {
			continue
		}
		return fmt.Errorf("%w: included quota is available or unknown", ErrSubscriptionPoolUnavailable)
	}
	if !snapshotPresent {
		enrolled, _ := ctx.Value(ManagedSubscriptionProvidersContextKey{}).(map[auth.SubscriptionProvider]struct{})
		if _, present := enrolled[auth.SubscriptionProvider(pool)]; present {
			return fmt.Errorf("%w: enrolled pool has no quota snapshot", ErrSubscriptionPoolUnavailable)
		}
	}
	token := claudeToken
	if pool == subscriptions.ProviderCodex {
		token = codexToken
	}
	if token != "" {
		if state, _ := ctx.Value(ManagedSubscriptionUsageContextKey{}).(*ManagedSubscriptionUsage); state != nil && state.QuotaExhaustedProviders[pool].After(now) {
			return nil
		}
		if s.usageObserver != nil {
			if snapshot, observed := s.usageObserver.Snapshot(s.usageObserver.Key([]byte(token))); observed && includedQuotaSpent(snapshot, now) {
				return nil
			}
		}
		return fmt.Errorf("%w: caller subscription quota is available or unknown", ErrSubscriptionPoolUnavailable)
	}
	return nil
}

func (s *Service) managedAccountQuotaExhausted(ctx context.Context, candidate managedSubscriptionCandidate, now time.Time) bool {
	if candidate.state == auth.SubscriptionAccountStateExhausted && candidate.resetAt.After(now) {
		return true
	}
	if state, _ := ctx.Value(ManagedSubscriptionUsageContextKey{}).(*ManagedSubscriptionUsage); state != nil {
		if state.QuotaExhaustedAccounts[candidate.accountID].After(now) {
			return true
		}
	}
	if s.usageObserver != nil {
		snapshot, observed := s.usageObserver.Snapshot(s.usageObserver.Key([]byte("subscription-account:" + candidate.accountID)))
		return observed && includedQuotaSpent(snapshot, now)
	}
	return false
}

// includedQuotaSpent treats observed paid overage as spent included quota until
// the reported plan reset. Undated overage never authorizes paid inference.
func includedQuotaSpent(snapshot usage.Snapshot, now time.Time) bool {
	return (snapshot.OverageInUse && snapshot.UnifiedResetAt.After(now)) || snapshot.ExhaustedAsOf(now)
}
