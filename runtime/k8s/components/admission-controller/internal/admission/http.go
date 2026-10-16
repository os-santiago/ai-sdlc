package admission

import (
	"net/http"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/contract"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
)

type evaluateRequest struct {
	Issue  Issue  `json:"issue"`
	Policy Policy `json:"policy"`
}

// Routes registers the admission-controller's HTTP API. POST
// /v1/admissions/evaluate accepts an explicit policy (useful for dry runs
// and tests) or, when omitted, loads it from the .ai-sdlc.yaml path named by
// AI_SDLC_CONTRACT_PATH (falling back to the schema-documented defaults when
// unset or unreadable).
func Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/admissions/evaluate", func(w http.ResponseWriter, r *http.Request) {
		var request evaluateRequest
		if !service.DecodeJSON(w, r, &request) {
			return
		}
		if request.Policy.Version == "" {
			request.Policy = PolicyFromContract(contract.LoadFromEnv())
		}
		service.WriteJSON(w, http.StatusOK, Evaluate(request.Issue, request.Policy))
	})
}
