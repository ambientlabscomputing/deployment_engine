package recipe
package recipe

import (
	"fmt"
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
























}	return nil, fmt.Errorf("not implemented")	)		"mma", r.mmaBaseURL,		"recipe_id", recipeID,	r.logger.Info("resolved recipe",	// TODO: Decode response and apply variable substitution	}		return nil, fmt.Errorf("MMA returned status %d", resp.StatusCode)	if resp.StatusCode != http.StatusOK {	defer resp.Body.Close()	}		return nil, fmt.Errorf("failed to fetch recipe from MMA: %w", err)	if err != nil {	resp, err := http.Get(url)	url := fmt.Sprintf("%s/api/recipes/%s", r.mmaBaseURL, recipeID)	// Call MMA API to fetch recipefunc (r *Reconciler) ResolveRecipe(recipeID string, variables map[string]interface{}) (map[string]interface{}, error) {// ResolveRecipe fetches a recipe from MMA and applies variable substitution