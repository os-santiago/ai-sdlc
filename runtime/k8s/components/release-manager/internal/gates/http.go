package gates

import (
	"net/http"

	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/contract"
	"github.com/os-santiago/ai-sdlc/runtime/k8s/internal/platform/service"
)

func Routes(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/releases/evaluate", func(w http.ResponseWriter, r *http.Request) {
		var snapshot Snapshot
		if !service.DecodeJSON(w, r, &snapshot) {
			return
		}
		snapshot = ApplyContractDefaults(snapshot, contract.LoadFromEnv())
		service.WriteJSON(w, http.StatusOK, Evaluate(snapshot))
	})
}
