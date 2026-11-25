// Package metrics is a small, dependency-free registry of counters, gauges and
// histograms exposed in the Prometheus text format, version 0.0.4.
//
// Every label value is bounded. A Label names its own allowlist; a value it
// does not allow, or one outside a short lowercase charset, is recorded as
// "other". Each metric also holds at most MaxSeries label sets, and folds any
// further set into one whose values are all "other". Callers must still pass
// only fixed constants or the output of a bounded diagnostic function: never
// issue text, model output, repository names or free-form errors.
package metrics

import (
	"math"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ContentType is the media type of the 0.0.4 text exposition format.
const ContentType = "text/plain; version=0.0.4; charset=utf-8"

// Other replaces any label value that is not allowed.
const Other = "other"

// MaxSeries bounds the label sets of one metric.
const MaxSeries = 256

var (
	namePattern  = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	valuePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,79}$`)
)

// Label is one label name and the predicate that bounds its values.
type Label struct {
	Name  string
	Allow func(string) bool
}

// OneOf allows exactly the given values.
func OneOf(name string, values ...string) Label {
	set := make(map[string]bool, len(values))
	for _, v := range values {
		set[v] = true
	}
	return Label{Name: name, Allow: func(v string) bool { return set[v] }}
}

// Bounded allows values produced by a bounded diagnostic function. The
// function is the allowlist; the registry still checks the charset, the
// length and MaxSeries.
func Bounded(name string) Label {
	return Label{Name: name, Allow: func(string) bool { return true }}
}

// Value returns v when the label allows it, otherwise Other.
func (l Label) Value(v string) string {
	if !valuePattern.MatchString(v) || l.Allow == nil || !l.Allow(v) {
		return Other
	}
	return v
}

type kind int

const (
	counterKind kind = iota
	gaugeKind
	histogramKind
)

func (k kind) String() string {
	switch k {
	case counterKind:
		return "counter"
	case gaugeKind:
		return "gauge"
	}
	return "histogram"
}

type series struct {
	values  []string
	value   float64
	buckets []uint64
	count   uint64
	sum     float64
}

type family struct {
	name, help string
	kind       kind
	labels     []Label
	buckets    []float64
	mu         sync.Mutex
	series     map[string]*series
}

// Registry holds metric families in name order.
type Registry struct {
	mu       sync.Mutex
	families map[string]*family
	samples  map[string]bool
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{families: map[string]*family{}, samples: map[string]bool{}}
}

// Default is the process registry that /metrics serves.
var Default = NewRegistry()

func (r *Registry) register(name, help string, k kind, buckets []float64, labels []Label) *family {
	if !namePattern.MatchString(name) || strings.ContainsAny(help, "\n\\") {
		panic("metrics: invalid metric " + name)
	}
	seen := map[string]bool{}
	for _, l := range labels {
		if !namePattern.MatchString(l.Name) || strings.HasPrefix(l.Name, "__") || l.Name == "le" || seen[l.Name] {
			panic("metrics: invalid label " + l.Name)
		}
		seen[l.Name] = true
	}
	for i, b := range buckets {
		if math.IsNaN(b) || math.IsInf(b, 0) || (i > 0 && b <= buckets[i-1]) {
			panic("metrics: invalid buckets for " + name)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// A histogram also writes name_bucket, name_sum and name_count: reserve
	// them, so no other metric can emit a sample with the same identity.
	names := []string{name}
	if k == histogramKind {
		names = append(names, name+"_bucket", name+"_sum", name+"_count")
	}
	for _, n := range names {
		if r.samples[n] {
			panic("metrics: duplicate metric " + n)
		}
	}
	for _, n := range names {
		r.samples[n] = true
	}
	// Own the validated labels and buckets: a caller reusing its slices
	// cannot change the registered schema.
	f := &family{name: name, help: help, kind: k, labels: append([]Label(nil), labels...), buckets: append([]float64(nil), buckets...), series: map[string]*series{}}
	r.families[name] = f
	return f
}

// get returns the series for values, creating it within MaxSeries. The caller
// holds f.mu. A wrong number of values is a programming error and is folded
// into the all-Other series rather than panicking in a reconciler. Beyond
// MaxSeries a counter or histogram folds a new set into the all-Other series;
// a gauge refuses it (nil), because a folded gauge value could not be deleted
// and would go stale.
func (f *family) get(values []string) *series {
	bounded := make([]string, len(f.labels))
	for i, l := range f.labels {
		if len(values) == len(f.labels) {
			bounded[i] = l.Value(values[i])
		} else {
			bounded[i] = Other
		}
	}
	key := strings.Join(bounded, "\x00")
	if s, ok := f.series[key]; ok {
		return s
	}
	if f.kind == gaugeKind && len(f.series) >= MaxSeries {
		return nil
	}
	if f.kind != gaugeKind && len(f.series) >= MaxSeries-1 {
		for i := range bounded {
			bounded[i] = Other
		}
		key = strings.Join(bounded, "\x00")
		if s, ok := f.series[key]; ok {
			return s
		}
	}
	s := &series{values: bounded}
	if f.kind == histogramKind {
		s.buckets = make([]uint64, len(f.buckets))
	}
	f.series[key] = s
	return s
}

// Counter is a monotonically increasing value per label set.
type Counter struct{ f *family }

// NewCounter registers a counter in r.
func (r *Registry) NewCounter(name, help string, labels ...Label) *Counter {
	return &Counter{r.register(name, help, counterKind, nil, labels)}
}

// Inc adds one.
func (c *Counter) Inc(values ...string) { c.Add(1, values...) }

// Add adds a non-negative delta; any other delta is ignored.
func (c *Counter) Add(delta float64, values ...string) {
	if c == nil || !(delta >= 0) || math.IsInf(delta, 0) {
		return
	}
	c.f.mu.Lock()
	c.f.get(values).value += delta
	c.f.mu.Unlock()
}

// Gauge is a value that can go up and down per label set.
type Gauge struct{ f *family }

// NewGauge registers a gauge in r.
func (r *Registry) NewGauge(name, help string, labels ...Label) *Gauge {
	return &Gauge{r.register(name, help, gaugeKind, nil, labels)}
}

// Set records v; NaN and infinities are ignored.
func (g *Gauge) Set(v float64, values ...string) {
	if g == nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	g.f.mu.Lock()
	if s := g.f.get(values); s != nil {
		s.value = v
	}
	g.f.mu.Unlock()
}

// Delete removes a label set, so a value that could not be read is absent
// rather than stale.
func (g *Gauge) Delete(values ...string) {
	if g == nil || len(values) != len(g.f.labels) {
		return
	}
	bounded := make([]string, len(values))
	for i, l := range g.f.labels {
		bounded[i] = l.Value(values[i])
	}
	g.f.mu.Lock()
	delete(g.f.series, strings.Join(bounded, "\x00"))
	g.f.mu.Unlock()
}

// Histogram counts observations into fixed cumulative buckets per label set.
type Histogram struct{ f *family }

// NewHistogram registers a histogram with increasing upper bounds.
func (r *Registry) NewHistogram(name, help string, buckets []float64, labels ...Label) *Histogram {
	return &Histogram{r.register(name, help, histogramKind, buckets, labels)}
}

// Observe records v; negative, NaN and infinite values are ignored.
func (h *Histogram) Observe(v float64, values ...string) {
	if h == nil || !(v >= 0) || math.IsInf(v, 0) {
		return
	}
	h.f.mu.Lock()
	s := h.f.get(values)
	for i, upper := range h.f.buckets {
		if v <= upper {
			s.buckets[i]++
		}
	}
	s.count++
	s.sum += v
	h.f.mu.Unlock()
}

// Write renders every family in name order, and each series in label order.
func (r *Registry) Write(b *strings.Builder) {
	r.mu.Lock()
	names := make([]string, 0, len(r.families))
	for name := range r.families {
		names = append(names, name)
	}
	families := make([]*family, 0, len(names))
	sort.Strings(names)
	for _, name := range names {
		families = append(families, r.families[name])
	}
	r.mu.Unlock()
	for _, f := range families {
		f.write(b)
	}
}

func (f *family) write(b *strings.Builder) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b.WriteString("# HELP " + f.name + " " + f.help + "\n")
	b.WriteString("# TYPE " + f.name + " " + f.kind.String() + "\n")
	keys := make([]string, 0, len(f.series))
	for key := range f.series {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		s := f.series[key]
		if f.kind != histogramKind {
			b.WriteString(f.name + f.labelText(s.values, "") + " " + formatFloat(s.value) + "\n")
			continue
		}
		for i, upper := range f.buckets {
			b.WriteString(f.name + "_bucket" + f.labelText(s.values, formatFloat(upper)) + " " + strconv.FormatUint(s.buckets[i], 10) + "\n")
		}
		b.WriteString(f.name + "_bucket" + f.labelText(s.values, "+Inf") + " " + strconv.FormatUint(s.count, 10) + "\n")
		b.WriteString(f.name + "_sum" + f.labelText(s.values, "") + " " + formatFloat(s.sum) + "\n")
		b.WriteString(f.name + "_count" + f.labelText(s.values, "") + " " + strconv.FormatUint(s.count, 10) + "\n")
	}
}

func (f *family) labelText(values []string, le string) string {
	if len(values) == 0 && le == "" {
		return ""
	}
	parts := make([]string, 0, len(values)+1)
	for i, v := range values {
		parts = append(parts, f.labels[i].Name+`="`+escape(v)+`"`)
	}
	if le != "" {
		parts = append(parts, `le="`+le+`"`)
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// escape is defense in depth: allowed values never contain these characters.
func escape(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	}
	return strconv.FormatFloat(v, 'g', -1, 64)
}

// Handler serves the registry read-only, to GET and HEAD only.
func (r *Registry) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var b strings.Builder
		r.Write(&b)
		w.Header().Set("Content-Type", ContentType)
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		if req.Method == http.MethodGet {
			_, _ = w.Write([]byte(b.String()))
		}
	})
}

// Sample reads one series: a counter or gauge value, or a histogram's
// observation count. Values are bounded as on write. It is for tests and
// diagnostics; ok is false when the series does not exist.
func (r *Registry) Sample(name string, values ...string) (value float64, ok bool) {
	r.mu.Lock()
	f := r.families[name]
	r.mu.Unlock()
	if f == nil || len(values) != len(f.labels) {
		return 0, false
	}
	bounded := make([]string, len(values))
	for i, l := range f.labels {
		bounded[i] = l.Value(values[i])
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.series[strings.Join(bounded, "\x00")]
	if !ok {
		return 0, false
	}
	if f.kind == histogramKind {
		return float64(s.count), true
	}
	return s.value, true
}
