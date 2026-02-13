package recipe

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
)

// Reconciler resolves recipe references to deployment specs
type Reconciler struct {
	mmaBaseURL string
	logger     *slog.Logger
}

// NewReconciler creates a new recipe reconciler
func NewReconciler(mmaBaseURL string, logger *slog.Logger) *Reconciler {
	return &Reconciler{
		mmaBaseURL: mmaBaseURL,
		logger:     logger,
	}
}

// ResolveRecipe fetches and resolves a recipe from the MMA
func (r *Reconciler) ResolveRecipe(recipeID string, variables map[string]string) (map[string]interface{}, error) {
	r.logger.Info("resolving recipe", "id", recipeID)

	url := fmt.Sprintf("%s/api/v1/recipes/%s", r.mmaBaseURL, recipeID)
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch recipe %s: %w", recipeID, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("recipe %s returned status %d", recipeID, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read recipe response: %w", err)
	}

	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("failed to parse recipe %s: %w", recipeID, err)
	}

	r.logger.Info("recipe resolved", "id", recipeID)
	return result, nil
}
