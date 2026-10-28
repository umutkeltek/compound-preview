package idea

import (
	"context"
	"net/http"

	"compound/internal/distiller"
)

// chatCompletionJSONFn is the variable production code overrides with
// distiller.ChatCompletionJSON. Tests inject a stub via package-level
// rebinding in their setup phase.
var chatCompletionJSONFn = func(ctx context.Context, client *http.Client, baseURL, apiKey, model, system, user string) ([]byte, error) {
	content, _, _, err := distiller.ChatCompletionJSON(ctx, client, baseURL, apiKey, model, system, user)
	return content, err
}
