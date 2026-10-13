package proxy

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"weave-os/router/internal/providers"
	"weave-os/router/internal/router"
	"weave-os/router/internal/router/sessionpin"
	"weave-os/router/internal/translate"
)

type turnStrikePinStore struct {
	rolePinStore
	mu      sync.Mutex
	strikes map[string][]string
	cleared []string
}

func (s *turnStrikePinStore) StrikeTurnModel(_ context.Context, _ [sessionpin.SessionKeyLen]byte, role, model string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.strikes == nil {
		s.strikes = map[string][]string{}
	}
	s.strikes[role] = append(s.strikes[role], model)
	return nil
}

func (s *turnStrikePinStore) ClearTurnStrikes(_ context.Context, _ [sessionpin.SessionKeyLen]byte, role string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleared = append(s.cleared, role)
	return nil
}

var _ sessionpin.TurnStrikeStore = (*turnStrikePinStore)(nil)

const toolResultTurnBody = `{"model":"claude-opus-4-8","messages":[` +
	`{"role":"user","content":"fix the build"},` +
	`{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"make"}}]},` +
	`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}]}`

func runTurnLoopWithBody(t *testing.T, svc *Service, body string) turnLoopResult {
	t.Helper()
	env, err := translate.ParseAnthropic([]byte(body))
	require.NoError(t, err)
	features := env.RoutingFeatures(false)
	res, err := svc.runTurnLoop(context.Background(), env, features, "api-key", uuid.New(), "", http.Header{}, router.Request{
		RequestedModel:       features.Model,
		EstimatedInputTokens: features.Tokens,
		HasTools:             features.HasTools,
		ConversationMessages: conversationMessagesForRouting(env),
	})
	require.NoError(t, err)
	return res
}

func turnStrikePin() sessionpin.Pin {
	pin := demotedPin(freshTurnModel)
	pin.TurnStrikes = []string{demotedPinModel}
	return pin
}

// A model that failed and was rescued stays out for every tool-result request
// of the same agent turn.
func TestTurnLoopKeepsTurnStrikeOutUntilUserPrompt(t *testing.T) {
	store := &turnStrikePinStore{rolePinStore: rolePinStore{byRole: map[string]sessionpin.Pin{
		sessionpin.DefaultRole: turnStrikePin(),
	}}}
	svc, scorer := cooldownTurnLoopService(store, false)

	res := runTurnLoopWithBody(t, svc, toolResultTurnBody)

	require.NotEmpty(t, scorer.requests)
	for _, req := range scorer.requests {
		assert.Contains(t, req.AutomaticExcludedModels, demotedPinModel)
	}
	assert.Contains(t, res.SessionDemotedModels, demotedPinModel)
	assert.Empty(t, store.cleared)
}

// The next real user prompt starts a new agent turn: the struck model is
// eligible again and the stored strikes are cleared.
func TestTurnLoopUserPromptClearsTurnStrikes(t *testing.T) {
	store := &turnStrikePinStore{rolePinStore: rolePinStore{byRole: map[string]sessionpin.Pin{
		sessionpin.DefaultRole: turnStrikePin(),
	}}}
	svc, scorer := cooldownTurnLoopService(store, false)

	res := runDemotionTurnLoop(t, svc, context.Background())

	for _, req := range scorer.requests {
		assert.NotContains(t, req.AutomaticExcludedModels, demotedPinModel)
	}
	assert.NotContains(t, res.SessionDemotedModels, demotedPinModel)
	assert.ElementsMatch(t, []string{sessionpin.DefaultRole, hmmHistoryRole(sessionpin.DefaultRole)}, store.cleared)
}

// With session-lifetime demotion off, a rescued primary failure is still kept
// out for the agent turn on every row the next request merges.
func TestRescuedFailureStrikesModelForAgentTurn(t *testing.T) {
	store := &turnStrikePinStore{}
	svc := newRescuedDemotionTestService(store, false)
	ctx := withTurnFailures(context.Background())

	demoted, _ := strikeRescuedPrimary(svc, ctx, &providers.UpstreamErrorResponse{Status: http.StatusServiceUnavailable, Headers: http.Header{}})
	assert.Empty(t, demoted, "no session-lifetime demotion while the flag is off")

	served := router.Decision{Model: freshTurnModel, Reason: "cluster:v0.2"}
	svc.strikeRescuedModelsForTurn(ctx, nil, served, false, nonZeroSessionKey(), sessionpin.DefaultRole, sessionpin.DefaultRole)
	assert.Equal(t, map[string][]string{
		sessionpin.DefaultRole:                 {demotedArm},
		hmmHistoryRole(sessionpin.DefaultRole): {demotedArm},
	}, store.strikes)
}

func TestTurnStrikeSkippedWhenTurnFailedOrPinned(t *testing.T) {
	served := router.Decision{Model: freshTurnModel, Reason: "cluster:v0.2"}
	for name, strike := range map[string]func(*Service, context.Context){
		"turn failed": func(svc *Service, ctx context.Context) {
			svc.strikeRescuedModelsForTurn(ctx, assert.AnError, served, false, nonZeroSessionKey(), sessionpin.DefaultRole, sessionpin.DefaultRole)
		},
		"hard pinned": func(svc *Service, ctx context.Context) {
			svc.strikeRescuedModelsForTurn(ctx, nil, served, true, nonZeroSessionKey(), sessionpin.DefaultRole, sessionpin.DefaultRole)
		},
		"forced model": func(svc *Service, ctx context.Context) {
			forced := router.Decision{Model: freshTurnModel, Reason: translate.ReasonUserForceModel}
			svc.strikeRescuedModelsForTurn(ctx, nil, forced, false, nonZeroSessionKey(), sessionpin.DefaultRole, sessionpin.DefaultRole)
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := &turnStrikePinStore{}
			svc := newRescuedDemotionTestService(store, false)
			ctx := withTurnFailures(context.Background())
			recordTurnFailure(ctx, demotedArm)
			strike(svc, ctx)
			assert.Empty(t, store.strikes)
		})
	}
}
