package metrics

import "time"

// The runtime's metrics and their bounded label values. See
// docs/operations/metrics.md for the list and its cardinality.

// Components are the runtime processes that serve /metrics.
var Components = []string{"intake", "admission-controller", "orchestrator", "worker", "release-manager", "relay", "verifier", "provenance-verifier", "build-controller", "deployer"}

// QueueStages are the queues whose waiting work is sampled.
var QueueStages = []string{"execution", "verification", "review", "publication", "outbox"}

// Loops are the reconcilers that log a "stopped safely" line.
var Loops = []string{
	// worker
	"candidate",
	// verifier
	"review", "verification", "publication-handoff", "repair-handoff",
	// provenance verifier
	"provenance", "promotion-dispatch",
	// release manager
	"publication", "merged-source-dispatch", "merge", "terminal", "production-dispatch", "promotion", "planned-release-dispatch",
	// build controller
	"build",
	// deployer
	"deployment", "deployment-completion-dispatch",
}

// Unclassified is the stop stage of a loop that has no bounded diagnostic.
const Unclassified = "unclassified"

// ModelRoles are the broker paths that call the model gateway.
var ModelRoles = []string{"generator", "reviewer"}

// Model call outcome classes, from the broker's view of one gateway call.
const (
	ModelSuccess         = "success"          // a valid assistant completion
	ModelTimeout         = "timeout"          // the call deadline ended first
	ModelCanceled        = "canceled"         // the caller canceled the call (for example on shutdown)
	ModelTransport       = "transport"        // connection or transport failure
	ModelStatus4xx       = "status-4xx"       // the gateway answered 4xx
	ModelStatus5xx       = "status-5xx"       // the gateway answered 5xx
	ModelStatusOther     = "status-other"     // any other non-200 status
	ModelInvalidResponse = "invalid-response" // 200 with a refused body
	ModelRejected        = "rejected"         // refused before any request
)

// ModelOutcomes lists every model call outcome class.
var ModelOutcomes = []string{ModelSuccess, ModelTimeout, ModelCanceled, ModelTransport, ModelStatus4xx, ModelStatus5xx, ModelStatusOther, ModelInvalidResponse, ModelRejected}

// DeliveryOutcomes are the results of one claimed publication or merge
// attempt of the release manager. Quarantine is a standing state, not an
// attempt, and is not counted.
var DeliveryOutcomes = []string{"published", "publication-stopped", "merged", "merge-stopped"}

// ModelCallBuckets span a fast local answer up to the 600 s ceiling.
var ModelCallBuckets = []float64{1, 2.5, 5, 10, 20, 30, 60, 90, 120, 180, 300, 450, 600}

var (
	queueDepth = Default.NewGauge("ai_sdlc_queue_depth",
		"Work items waiting per queue, sampled from the store (capped at 10000).",
		OneOf("component", Components...), OneOf("stage", QueueStages...))
	stops = Default.NewCounter("ai_sdlc_stops_total",
		"Reconciler cycles that stopped safely, by loop and bounded diagnostic stage.",
		OneOf("component", Components...), OneOf("loop", Loops...), Bounded("stage"))
	modelCalls = Default.NewCounter("ai_sdlc_model_calls_total",
		"Model gateway calls made by the broker, by role and outcome class.",
		OneOf("role", ModelRoles...), OneOf("outcome", ModelOutcomes...))
	modelCallSeconds = Default.NewHistogram("ai_sdlc_model_call_duration_seconds",
		"Duration of model gateway calls made by the broker, by role and outcome class.",
		ModelCallBuckets, OneOf("role", ModelRoles...), OneOf("outcome", ModelOutcomes...))
	deliveries = Default.NewCounter("ai_sdlc_deliveries_total",
		"Publication and merge attempts of the release manager, by outcome.",
		OneOf("outcome", DeliveryOutcomes...))
)

// MaxQueueDepth caps the sampled count, so the query stays bounded.
const MaxQueueDepth = 10000

// SetQueueDepth records the waiting work of one queue.
func SetQueueDepth(component, stage string, depth int64) {
	if depth > MaxQueueDepth {
		depth = MaxQueueDepth
	}
	queueDepth.Set(float64(depth), component, stage)
}

// ForgetQueueDepth drops a queue whose depth could not be read.
func ForgetQueueDepth(component, stage string) { queueDepth.Delete(component, stage) }

// Stop counts one "stopped safely" line. stage must be a fixed constant or
// the output of a bounded diagnostic function (for example
// execution.DiagnosticStage), never error text.
func Stop(component, loop, stage string) { stops.Inc(component, loop, stage) }

// ModelCall counts one broker call to the model gateway and its duration.
func ModelCall(role, outcome string, elapsed time.Duration) {
	modelCalls.Inc(role, outcome)
	modelCallSeconds.Observe(elapsed.Seconds(), role, outcome)
}

// Delivery counts one terminal publication or merge outcome.
func Delivery(outcome string) { deliveries.Inc(outcome) }
