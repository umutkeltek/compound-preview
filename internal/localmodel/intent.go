package localmodel

const (
	LifecycleIntentOwner            = "local-routing"
	LifecycleIntentMutationClass    = "local_reversible"
	LifecycleIntentDelegation       = "delegate_to_configured_local_routing_adapter"
	LifecycleIntentEvidenceBoundary = "intent_only; execution evidence must come back from the routing adapter"
)

// LifecycleIntent is Compound's structured handoff to the local routing layer.
// It describes the runtime mutation Compound wants without executing it.
type LifecycleIntent struct {
	Owner            string `json:"owner"`
	Action           Action `json:"action"`
	ModelID          string `json:"model_id"`
	ContextTokens    int    `json:"context_tokens,omitempty"`
	MutationClass    string `json:"mutation_class"`
	Delegation       string `json:"delegation"`
	EvidenceBoundary string `json:"evidence_boundary"`
}

func NewLifecycleIntent(modelID string, action Action, contextTokens int) *LifecycleIntent {
	if !actionNeedsRouting(action) {
		return nil
	}
	return &LifecycleIntent{
		Owner:            LifecycleIntentOwner,
		Action:           action,
		ModelID:          modelID,
		ContextTokens:    contextTokens,
		MutationClass:    LifecycleIntentMutationClass,
		Delegation:       LifecycleIntentDelegation,
		EvidenceBoundary: LifecycleIntentEvidenceBoundary,
	}
}

func actionNeedsRouting(action Action) bool {
	switch action {
	case ActionLoad, ActionReloadHigherContext, ActionReloadLowerWhenIdle, ActionUnloadIdle:
		return true
	default:
		return false
	}
}
