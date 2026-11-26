package metrics

import (
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func render(r *Registry) string {
	var b strings.Builder
	r.Write(&b)
	return b.String()
}

func TestExpositionFormat(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("demo_total", "Demo counter.", OneOf("role", "a", "b"))
	g := r.NewGauge("demo_depth", "Demo gauge.")
	h := r.NewHistogram("demo_seconds", "Demo histogram.", []float64{1, 2.5}, OneOf("role", "a"))
	c.Inc("b")
	c.Add(2, "a")
	g.Set(7)
	h.Observe(0.5, "a")
	h.Observe(2, "a")
	h.Observe(9, "a")
	want := `# HELP demo_depth Demo gauge.
# TYPE demo_depth gauge
demo_depth 7
# HELP demo_seconds Demo histogram.
# TYPE demo_seconds histogram
demo_seconds_bucket{role="a",le="1"} 1
demo_seconds_bucket{role="a",le="2.5"} 2
demo_seconds_bucket{role="a",le="+Inf"} 3
demo_seconds_sum{role="a"} 11.5
demo_seconds_count{role="a"} 3
# HELP demo_total Demo counter.
# TYPE demo_total counter
demo_total{role="a"} 2
demo_total{role="b"} 1
`
	if got := render(r); got != want {
		t.Fatalf("exposition:\n%s\nwant:\n%s", got, want)
	}
}

func TestRegisteredFamilyWithoutSeriesStillDescribesItself(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("idle_total", "Never incremented.", OneOf("x", "y"))
	if got := render(r); got != "# HELP idle_total Never incremented.\n# TYPE idle_total counter\n" {
		t.Fatal(got)
	}
}

func TestLabelValuesOutsideTheAllowlistBecomeOther(t *testing.T) {
	r := NewRegistry()
	fixed := r.NewCounter("fixed_total", "h", OneOf("outcome", "success"))
	bounded := r.NewCounter("bounded_total", "h", Bounded("stage"))
	hostile := []string{
		"", "Bearer abcdefghijklmnopqrstuvwxyz0123456789", "os-santiago/saferus", "Fix the login bug",
		"stage\n# TYPE injected counter", `quote"brace}`, "UPPER", strings.Repeat("a", 81), "-leading", "ünïcode",
	}
	for _, v := range hostile {
		fixed.Inc(v)
		bounded.Inc(v)
	}
	fixed.Inc("success")
	bounded.Inc("model-provider")
	if v, _ := r.Sample("fixed_total", Other); v != float64(len(hostile)) {
		t.Fatal("hostile values not folded", v)
	}
	if v, _ := r.Sample("bounded_total", Other); v != float64(len(hostile)) {
		t.Fatal("hostile values not folded", v)
	}
	text := render(r)
	for _, v := range hostile[1:] {
		if strings.Contains(text, v) {
			t.Fatalf("label value leaked: %q", v)
		}
	}
	if !strings.Contains(text, `fixed_total{outcome="success"} 1`) || !strings.Contains(text, `bounded_total{stage="model-provider"} 1`) {
		t.Fatal(text)
	}
}

func TestSeriesAreCappedPerMetric(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("many_total", "h", Bounded("stage"))
	for i := 0; i < 3*MaxSeries; i++ {
		c.Inc("stage-" + strconv.Itoa(i))
	}
	lines := 0
	for _, line := range strings.Split(render(r), "\n") {
		if strings.HasPrefix(line, "many_total{") {
			lines++
		}
	}
	if lines != MaxSeries {
		t.Fatal("series", lines)
	}
	if v, _ := r.Sample("many_total", Other); v != float64(2*MaxSeries+1) {
		t.Fatal("overflow not folded into other", v)
	}
	// A known series keeps counting after the cap.
	c.Inc("stage-0")
	if v, _ := r.Sample("many_total", "stage-0"); v != 2 {
		t.Fatal(v)
	}
}

func TestWrongArityAndInvalidNumbersAreIgnoredOrFolded(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("arity_total", "h", OneOf("a", "x"), OneOf("b", "y"))
	g := r.NewGauge("g", "h")
	h := r.NewHistogram("h_seconds", "h", []float64{1})
	c.Inc("x")
	c.Add(-1, "x", "y")
	c.Add(math.NaN(), "x", "y")
	g.Set(math.Inf(1))
	h.Observe(-1)
	h.Observe(math.NaN())
	if v, _ := r.Sample("arity_total", Other, Other); v != 1 {
		t.Fatal("wrong arity not folded", v)
	}
	if _, ok := r.Sample("arity_total", "x", "y"); ok {
		t.Fatal("negative or NaN delta recorded")
	}
	if _, ok := r.Sample("g"); ok {
		t.Fatal("infinite gauge recorded")
	}
	if _, ok := r.Sample("h_seconds"); ok {
		t.Fatal("invalid observation recorded")
	}
}

func TestGaugeDeleteDropsStaleValue(t *testing.T) {
	r := NewRegistry()
	g := r.NewGauge("depth", "h", OneOf("stage", "execution"))
	g.Set(3, "execution")
	g.Delete("execution")
	if strings.Contains(render(r), "depth{") {
		t.Fatal("deleted series rendered")
	}
}

func TestInvalidRegistrationPanics(t *testing.T) {
	for name, register := range map[string]func(*Registry){
		"name":      func(r *Registry) { r.NewCounter("Bad-Name", "h") },
		"help":      func(r *Registry) { r.NewCounter("x", "line\nbreak") },
		"label":     func(r *Registry) { r.NewCounter("x", "h", OneOf("le", "a")) },
		"reserved":  func(r *Registry) { r.NewCounter("x", "h", OneOf("__name", "a")) },
		"duplicate": func(r *Registry) { r.NewCounter("x", "h"); r.NewGauge("x", "h") },
		"labels":    func(r *Registry) { r.NewCounter("x", "h", OneOf("a", "b"), OneOf("a", "c")) },
		"buckets":   func(r *Registry) { r.NewHistogram("x", "h", []float64{2, 1}) },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("accepted")
				}
			}()
			register(NewRegistry())
		})
	}
}

func TestHandlerServesTextFormatReadOnly(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("served_total", "h").Inc()
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if w.Code != 200 || w.Header().Get("Content-Type") != ContentType || !strings.Contains(w.Body.String(), "served_total 1\n") {
		t.Fatal(w.Code, w.Header(), w.Body.String())
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		w := httptest.NewRecorder()
		r.Handler().ServeHTTP(w, httptest.NewRequest(method, "/metrics", strings.NewReader("served_total 9")))
		if w.Code != http.StatusMethodNotAllowed {
			t.Fatal(method, w.Code)
		}
	}
	if v, _ := r.Sample("served_total"); v != 1 {
		t.Fatal("a request changed a metric")
	}
}

// The runtime's own metrics: every series the instrumentation can produce
// keeps its labels inside the documented allowlists.
func TestRuntimeMetricsKeepLabelsBounded(t *testing.T) {
	Stop("worker", "candidate", "model-provider")
	Stop("verifier", "verification", "refused-regression")
	Stop("verifier", "review", "error: repository os-santiago/saferus not found")
	Stop("release-manager", "publication", Unclassified)
	Stop("unknown-component", "free text loop", "x")
	ModelCall("generator", ModelSuccess, 1500*time.Millisecond)
	ModelCall("reviewer", ModelTimeout, 600*time.Second)
	ModelCall("planner", "provider said: rate limited", time.Second)
	SetQueueDepth("worker", "execution", 3)
	SetQueueDepth("verifier", "verification", 1<<40)
	SetQueueDepth("worker", "issue-42", 1)
	Delivery("published")
	Delivery("pull-request https://github.com/x/y/pull/1")
	Reconciliation("branch", "not-sent")
	Reconciliation("refs/heads/main", "forge said 502")
	allowed := map[string]map[string]bool{
		"component": set(append(Components, Other)...),
		"loop":      set(append(Loops, Other)...),
		"role":      set(append(ModelRoles, Other)...),
		"outcome":   set(append(append(append(ModelOutcomes, DeliveryOutcomes...), ReconciliationOutcomes...), Other)...),
		"effect":    set(append(ReconciliationEffects, Other)...),
		"stage":     nil, // bounded by the diagnostic functions and the charset
	}
	queue := set(append(QueueStages, Other)...)
	w := httptest.NewRecorder()
	Default.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	label := regexp.MustCompile(`([a-z_]+)="([^"]*)"`)
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, "ai_sdlc_") {
			t.Fatalf("unexpected metric %q", line)
		}
		for _, m := range label.FindAllStringSubmatch(line, -1) {
			name, value := m[1], m[2]
			switch {
			case name == "le":
			case name == "stage" && strings.HasPrefix(line, "ai_sdlc_queue_depth"):
				if !queue[value] {
					t.Fatalf("queue stage %q", value)
				}
			case name == "stage":
				if !valuePattern.MatchString(value) {
					t.Fatalf("stage %q", value)
				}
			default:
				values, known := allowed[name]
				if !known || !values[value] {
					t.Fatalf("unbounded label %s=%q in %q", name, value, line)
				}
			}
		}
	}
	if v, _ := Default.Sample("ai_sdlc_queue_depth", "verifier", "verification"); v != MaxQueueDepth {
		t.Fatal("queue depth not capped", v)
	}
	if v, _ := Default.Sample("ai_sdlc_stops_total", "verifier", "review", Other); v != 1 {
		t.Fatal("free-form stage not folded", v)
	}
	if v, _ := Default.Sample("ai_sdlc_model_call_duration_seconds", "reviewer", ModelTimeout); v != 1 {
		t.Fatal("duration not observed", v)
	}
}

func set(values ...string) map[string]bool {
	m := map[string]bool{}
	for _, v := range values {
		m[v] = true
	}
	return m
}

func TestHistogramSampleNamesAreReserved(t *testing.T) {
	for name, register := range map[string]func(*Registry){
		"histogram first": func(r *Registry) { r.NewHistogram("latency", "h", []float64{1}); r.NewGauge("latency_count", "h") },
		"gauge first":     func(r *Registry) { r.NewCounter("latency_sum", "h"); r.NewHistogram("latency", "h", []float64{1}) },
		"bucket":          func(r *Registry) { r.NewHistogram("latency", "h", []float64{1}); r.NewCounter("latency_bucket", "h") },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("colliding sample name accepted")
				}
			}()
			register(NewRegistry())
		})
	}
	r := NewRegistry()
	r.NewHistogram("latency", "h", []float64{1})
	r.NewCounter("latency_total", "h") // no collision
}

func TestRegisteredLabelsAreOwned(t *testing.T) {
	r := NewRegistry()
	labels := []Label{OneOf("a", "x"), OneOf("b", "y")}
	c := r.NewCounter("owned_total", "h", labels...)
	labels[1] = OneOf("a", "y")
	c.Inc("x", "y")
	if !strings.Contains(render(r), `owned_total{a="x",b="y"} 1`) {
		t.Fatal(render(r))
	}
}

// A gauge never folds into "other": beyond MaxSeries it refuses new sets,
// and admitted sets still update and delete.
func TestGaugeRefusesSetsBeyondCapacity(t *testing.T) {
	r := NewRegistry()
	g := r.NewGauge("wide", "h", Bounded("k"))
	for i := 0; i < MaxSeries+10; i++ {
		g.Set(1, "k"+strconv.Itoa(i))
	}
	if _, ok := r.Sample("wide", Other); ok {
		t.Fatal("gauge folded into other")
	}
	if _, ok := r.Sample("wide", "k"+strconv.Itoa(MaxSeries)); ok {
		t.Fatal("gauge exceeded capacity")
	}
	g.Set(5, "k0")
	if v, _ := r.Sample("wide", "k0"); v != 5 {
		t.Fatal("admitted gauge not updated")
	}
	g.Delete("k0")
	g.Set(2, "late")
	if v, ok := r.Sample("wide", "late"); !ok || v != 2 {
		t.Fatal("freed capacity not reused")
	}
}
